package dashboard

import (
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
)

// Generic chunked-upload session management, ported from
// astrbot/dashboard/services/chunked_upload_service.py (4.29.0). Business
// services keep ownership of validation/naming/post-merge handling; this file
// owns the session lifecycle, chunk storage, streaming assembly and expiry.
//
// The WebUI file flow uses it to upload large files in chunks:
//
//	POST /api/v1/files/upload/init     {filename,total_size,total_chunks}
//	POST /api/v1/files/upload/chunk    multipart: upload_id,index,file
//	POST /api/v1/files/upload/complete {upload_id}
//	POST /api/v1/files/upload/abort    {upload_id}
//	POST /api/v1/files/upload/status   ?upload_id=
const (
	chunkUploadChunkSize   = 1 << 20
	chunkUploadExpireSecs  = 3600
	chunkUploadCleanupTick = 5 * time.Minute
)

type chunkUploadSession struct {
	ID           string
	Filename     string
	ContentType  string
	TotalSize    int64
	TotalChunks  int
	ChunkSize    int64
	Dir          string
	Received     map[int]bool
	CreatedAt    time.Time
	LastActivity time.Time
	mu           sync.Mutex
}

// chunksRoot returns the temporary chunk root (data/webui_files/.chunks).
func (s *Server) uploadChunksRoot() string {
	return filepath.Join(s.webuiFilesDir(), ".chunks")
}

func (s *Server) chunkUploadSessions() map[string]*chunkUploadSession {
	s.chunkUploadsMu.Lock()
	defer s.chunkUploadsMu.Unlock()
	if s.chunkUploads == nil {
		s.chunkUploads = make(map[string]*chunkUploadSession)
	}
	return s.chunkUploads
}

// handleChunkedUpload serves the /files/upload/* lifecycle.
func (s *Server) handleChunkedUpload(w http.ResponseWriter, r *http.Request, sub string) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, apiError("仅支持 POST"))
		return
	}
	switch sub {
	case "init":
		s.chunkUploadInit(w, r)
	case "chunk":
		s.chunkUploadPut(w, r)
	case "complete":
		s.chunkUploadComplete(w, r)
	case "abort":
		s.chunkUploadAbort(w, r)
	case "status":
		s.chunkUploadStatus(w, r)
	default:
		writeJSON(w, http.StatusNotFound, apiError("未知的上传操作"))
	}
}

func (s *Server) chunkUploadInit(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Filename    string `json:"filename"`
		ContentType string `json:"content_type"`
		TotalSize   int64  `json:"total_size"`
		TotalChunks int    `json:"total_chunks"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, apiError("无效的 JSON: "+err.Error()))
		return
	}
	if body.TotalSize <= 0 || body.TotalSize > maxWebUIFileSize {
		writeJSON(w, http.StatusBadRequest, apiError("文件大小非法或超限（上限 64MB）"))
		return
	}
	totalChunks := body.TotalChunks
	if totalChunks <= 0 {
		totalChunks = int((body.TotalSize + chunkUploadChunkSize - 1) / chunkUploadChunkSize)
	}
	if totalChunks <= 0 || totalChunks > 100000 {
		writeJSON(w, http.StatusBadRequest, apiError("分片数量非法"))
		return
	}
	id := uuid.NewString()
	dir := filepath.Join(s.uploadChunksRoot(), id)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		writeJSON(w, http.StatusInternalServerError, apiError("创建分片目录失败: "+err.Error()))
		return
	}
	sess := &chunkUploadSession{
		ID:           id,
		Filename:     sanitizePath(filepath.Base(strings.ReplaceAll(body.Filename, "\\", "/"))),
		ContentType:  body.ContentType,
		TotalSize:    body.TotalSize,
		TotalChunks:  totalChunks,
		ChunkSize:    chunkUploadChunkSize,
		Dir:          dir,
		Received:     map[int]bool{},
		CreatedAt:    time.Now(),
		LastActivity: time.Now(),
	}
	if sess.Filename == "" || sess.Filename == "_" {
		sess.Filename = "file"
	}
	s.chunkUploadSessions()[id] = sess
	writeJSON(w, http.StatusOK, apiOK(map[string]interface{}{
		"upload_id":    id,
		"chunk_size":   sess.ChunkSize,
		"total_chunks": sess.TotalChunks,
	}))
}

func (s *Server) chunkUploadPut(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseMultipartForm(8 << 20); err != nil {
		writeJSON(w, http.StatusBadRequest, apiError("解析分片失败: "+err.Error()))
		return
	}
	uploadID := r.FormValue("upload_id")
	indexStr := r.FormValue("index")
	index, err := strconv.Atoi(indexStr)
	if uploadID == "" || err != nil || index < 0 {
		writeJSON(w, http.StatusBadRequest, apiError("upload_id / index 非法"))
		return
	}
	sess := s.chunkUploadSessions()[uploadID]
	if sess == nil {
		writeJSON(w, http.StatusNotFound, apiError("上传会话不存在或已过期"))
		return
	}
	if index >= sess.TotalChunks {
		writeJSON(w, http.StatusBadRequest, apiError("分片索引越界"))
		return
	}
	file, _, err := r.FormFile("file")
	if err != nil {
		file, _, err = r.FormFile("chunk")
	}
	if err != nil {
		writeJSON(w, http.StatusBadRequest, apiError("未收到分片: "+err.Error()))
		return
	}
	defer file.Close()
	partPath := filepath.Join(sess.Dir, strconv.Itoa(index)+".part")
	out, err := os.Create(partPath)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, apiError("写入分片失败: "+err.Error()))
		return
	}
	n, err := io.Copy(out, io.LimitReader(file, chunkUploadChunkSize+1))
	_ = out.Close()
	if err != nil || n > chunkUploadChunkSize {
		_ = os.Remove(partPath)
		writeJSON(w, http.StatusBadRequest, apiError("分片过大或写入失败"))
		return
	}
	sess.mu.Lock()
	sess.Received[index] = true
	sess.LastActivity = time.Now()
	received := len(sess.Received)
	sess.mu.Unlock()
	writeJSON(w, http.StatusOK, apiOK(map[string]interface{}{"received": received}))
}

func (s *Server) chunkUploadComplete(w http.ResponseWriter, r *http.Request) {
	uploadID := s.readUploadID(r)
	sess := s.chunkUploadSessions()[uploadID]
	if sess == nil {
		writeJSON(w, http.StatusNotFound, apiError("上传会话不存在或已过期"))
		return
	}
	sess.mu.Lock()
	received := len(sess.Received)
	sess.mu.Unlock()
	if received != sess.TotalChunks {
		writeJSON(w, http.StatusBadRequest, apiError("分片不完整"))
		return
	}
	attachmentID := uuid.NewString()
	dir := s.webuiFilesDir()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		writeJSON(w, http.StatusInternalServerError, apiError("创建目录失败: "+err.Error()))
		return
	}
	dst := filepath.Join(dir, attachmentID)
	out, err := os.Create(dst)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, apiError("写入文件失败: "+err.Error()))
		return
	}
	var total int64
	for i := 0; i < sess.TotalChunks; i++ {
		part, err := os.Open(filepath.Join(sess.Dir, strconv.Itoa(i)+".part"))
		if err != nil {
			_ = out.Close()
			_ = os.Remove(dst)
			writeJSON(w, http.StatusBadRequest, apiError("缺少分片 "+strconv.Itoa(i)))
			return
		}
		n, err := io.Copy(out, io.LimitReader(part, chunkUploadChunkSize))
		_ = part.Close()
		if err != nil {
			_ = out.Close()
			_ = os.Remove(dst)
			writeJSON(w, http.StatusInternalServerError, apiError("合并分片失败: "+err.Error()))
			return
		}
		total += n
	}
	_ = out.Close()
	if total > maxWebUIFileSize {
		_ = os.Remove(dst)
		writeJSON(w, http.StatusBadRequest, apiError("上传文件过大（单文件上限 64MB）"))
		return
	}
	meta := webuiFileMeta{
		AttachmentID: attachmentID,
		Filename:     sess.Filename,
		Type:         webUIFileType(sess.ContentType),
		Size:         total,
		CreatedAt:    time.Now().Format(time.RFC3339Nano),
		FileToken:    uuid.NewString(),
	}
	data, _ := json.Marshal(meta)
	if err := writeFileAtomic(filepath.Join(dir, attachmentID+".json"), data, 0o644); err != nil {
		_ = os.Remove(dst)
		writeJSON(w, http.StatusInternalServerError, apiError("写入元数据失败: "+err.Error()))
		return
	}
	s.chunkUploadDiscard(uploadID)
	writeJSON(w, http.StatusOK, apiOK(map[string]interface{}{
		"attachment_id": meta.AttachmentID,
		"filename":      meta.Filename,
		"type":          meta.Type,
	}))
}

func (s *Server) chunkUploadAbort(w http.ResponseWriter, r *http.Request) {
	uploadID := s.readUploadID(r)
	s.chunkUploadDiscard(uploadID)
	writeJSON(w, http.StatusOK, apiOK(map[string]interface{}{"ok": true}))
}

func (s *Server) chunkUploadStatus(w http.ResponseWriter, r *http.Request) {
	uploadID := r.URL.Query().Get("upload_id")
	if uploadID == "" {
		var body struct {
			UploadID string `json:"upload_id"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		uploadID = body.UploadID
	}
	sess := s.chunkUploadSessions()[uploadID]
	if sess == nil {
		writeJSON(w, http.StatusNotFound, apiError("上传会话不存在或已过期"))
		return
	}
	sess.mu.Lock()
	received := make([]int, 0, len(sess.Received))
	for idx := range sess.Received {
		received = append(received, idx)
	}
	sess.mu.Unlock()
	writeJSON(w, http.StatusOK, apiOK(map[string]interface{}{
		"upload_id":    sess.ID,
		"received":     received,
		"total_chunks": sess.TotalChunks,
		"complete":     len(received) == sess.TotalChunks,
	}))
}

func (s *Server) readUploadID(r *http.Request) string {
	uploadID := r.URL.Query().Get("upload_id")
	if uploadID != "" {
		return uploadID
	}
	var body struct {
		UploadID string `json:"upload_id"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body)
	return body.UploadID
}

func (s *Server) chunkUploadDiscard(uploadID string) {
	if uploadID == "" {
		return
	}
	sess := s.chunkUploadSessions()[uploadID]
	if sess == nil {
		return
	}
	_ = os.RemoveAll(sess.Dir)
	s.chunkUploadsMu.Lock()
	delete(s.chunkUploads, uploadID)
	s.chunkUploadsMu.Unlock()
}

// cleanupChunkUploads removes expired sessions (best-effort).
func (s *Server) cleanupChunkUploads() {
	cutoff := time.Now().Add(-time.Duration(chunkUploadExpireSecs) * time.Second)
	s.chunkUploadsMu.Lock()
	defer s.chunkUploadsMu.Unlock()
	for id, sess := range s.chunkUploads {
		if sess.LastActivity.Before(cutoff) {
			_ = os.RemoveAll(sess.Dir)
			delete(s.chunkUploads, id)
		}
	}
}
