// Package dashboard —— 知识库文档端点。
// 本文件由 handlers.go 的 handleKBDocuments 按子资源拆分而来，行为与拆分前
// 完全一致：URL 导入、单文档详情/删除、multipart 上传（含异步索引任务）、
// 目录列表。路由分发壳 handleKBDocuments 仍留在 handlers.go。
package dashboard

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// recordKBTask stores a knowledge-base upload task state.
func (s *Server) recordKBTask(t *kbUploadTask) {
	if t == nil || t.TaskID == "" {
		return
	}
	s.mu.Lock()
	s.kbTasks[t.TaskID] = t
	s.mu.Unlock()
	if t.Status == "completed" || t.Status == "failed" {
		// 终态在 TTL 后自动清除（仅当仍是本条记录时），避免 map 只增不删。
		time.AfterFunc(kbTaskCleanupTTL, func() {
			s.mu.Lock()
			if s.kbTasks[t.TaskID] == t {
				delete(s.kbTasks, t.TaskID)
			}
			s.mu.Unlock()
		})
	}
}

// kbDocImportURL handles POST /knowledge-bases/{kb_id}/documents/import-url
// （URL 导入）。kbID 来自 RESTful 路径 /knowledge-bases/{kb_id}/documents，
// 旧版子路径形式由调用方经 query 参数解析后传入。
func (s *Server) kbDocImportURL(w http.ResponseWriter, r *http.Request, kbID string) {
	var body map[string]interface{}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		// 导入不限方法（GET/POST 均命中）：GET 无请求体时按空 body 处理，
		// 由后续 url 参数校验兜底；只有非空但非法的 JSON 才报错。
		if !errors.Is(err, io.EOF) {
			writeJSON(w, http.StatusBadRequest, apiError("无效的 JSON: "+err.Error()))
			return
		}
	}
	url, _ := body["url"].(string)
	if url == "" || (!strings.HasPrefix(url, "http://") && !strings.HasPrefix(url, "https://")) {
		writeJSON(w, http.StatusOK, apiError("缺少或无效的参数 url"))
		return
	}
	if err := validateOutboundURL(url); err != nil {
		writeJSON(w, http.StatusOK, apiError("下载文档失败: "+err.Error()))
		return
	}
	chunkSize := 512
	chunkOverlap := 50
	if v, ok := body["chunk_size"].(float64); ok && v > 0 {
		chunkSize = int(v)
	}
	if v, ok := body["chunk_overlap"].(float64); ok && v >= 0 {
		chunkOverlap = int(v)
	}

	// Download the remote content with a bounded timeout and size limit.
	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		writeJSON(w, http.StatusOK, apiError("创建下载请求失败: "+err.Error()))
		return
	}
	client := newOutboundClient(60 * time.Second)
	resp, err := client.Do(req)
	if err != nil {
		writeJSON(w, http.StatusOK, apiError("下载文档失败: "+err.Error()))
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		writeJSON(w, http.StatusOK, apiError(fmt.Sprintf("下载文档失败: HTTP %d", resp.StatusCode)))
		return
	}
	content, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if err != nil {
		writeJSON(w, http.StatusOK, apiError("读取文档内容失败: "+err.Error()))
		return
	}
	if len(content) == 0 {
		writeJSON(w, http.StatusOK, apiError("下载的文档内容为空"))
		return
	}

	// Save under the KB documents directory like the multipart upload path.
	dir := filepath.Join(s.kbDataDir(), "knowledge_bases", sanitizePath(kbID), "documents")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		writeJSON(w, http.StatusOK, apiError("创建知识库数据目录失败: "+err.Error()))
		return
	}
	name := filepath.Base(strings.TrimRight(url, "/"))
	if name == "" || name == "." || name == "/" {
		name = "imported"
	}
	dst := filepath.Join(dir, sanitizePath(name))
	if err := os.WriteFile(dst, content, 0o644); err != nil {
		writeJSON(w, http.StatusOK, apiError("保存文档失败: "+err.Error()))
		return
	}
	mod := time.Now().UnixNano()
	docID := fmt.Sprintf("doc_%d_%s", mod, name)
	taskID := fmt.Sprintf("task_%d", time.Now().UnixNano())
	s.recordKBTask(&kbUploadTask{
		TaskID: taskID,
		KBID:   kbID,
		Status: "processing",
		Stage:  "chunking",
		Total:  100,
	})

	// Index asynchronously (chunk → embed → dual write), same as uploads.
	go func() {
		if _, err := s.indexKBFile(kbID, docID, name, content, chunkSize, chunkOverlap); err != nil {
			s.recordKBTask(&kbUploadTask{
				TaskID:       taskID,
				KBID:         kbID,
				Status:       "completed",
				Stage:        "embedding_failed",
				Current:      100,
				Total:        100,
				SuccessCount: 0,
				FailedCount:  1,
				Error:        err.Error(),
			})
			return
		}
		s.recordKBTask(&kbUploadTask{
			TaskID:       taskID,
			KBID:         kbID,
			Status:       "completed",
			Stage:        "completed",
			Current:      100,
			Total:        100,
			SuccessCount: 1,
			FailedCount:  0,
		})
	}()

	writeJSON(w, http.StatusOK, apiOKMsg("文档导入任务已创建", map[string]interface{}{
		"task_id":    taskID,
		"file_count": 1,
		"documents": []map[string]interface{}{{
			"doc_id":    docID,
			"doc_name":  name,
			"file_size": len(content),
		}},
	}))
}

// kbDocDetail handles GET /knowledge-bases/{kb_id}/documents/{document_id} —
// single doc detail（DELETE 同路径为删除文档）。
func (s *Server) kbDocDetail(w http.ResponseWriter, r *http.Request, kbID, docID string) {
	if r.Method == http.MethodDelete {
		// Delete nanovec vectors first, then SQLite chunk rows, then the
		// on-disk file.
		if err := s.kbDeleteDoc(kbID, docID); err != nil {
			logger.I18nWarn("删除文档 %s 失败: %v", docID, err)
			writeJSON(w, http.StatusInternalServerError, apiError("删除文档失败: "+err.Error()))
			return
		}
		dir := filepath.Join(s.kbDataDir(), "knowledge_bases", sanitizePath(kbID), "documents")
		if doc := s.kbDocumentByID(kbID, docID); doc != nil {
			if err := os.Remove(filepath.Join(dir, sanitizePath(anyStr(doc["doc_name"])))); err != nil {
				logger.I18nWarn("删除文档文件 %s 失败: %v", docID, err)
			}
		}
		writeJSON(w, http.StatusOK, apiOKMsg("文档已删除", map[string]interface{}{}))
		return
	}
	if doc := s.kbDocumentByID(kbID, docID); doc != nil {
		writeJSON(w, http.StatusOK, apiOK(doc))
		return
	}
	writeJSON(w, http.StatusOK, apiError("文档不存在"))
}

// kbDocUpload handles POST /knowledge-bases/{kb_id}/documents（multipart 上传）:
// Multipart upload: save the files under the KB's data directory, then
// index them asynchronously (chunk → embed → SQLite + nanovec dual
// write). The WebUI polls the returned task for progress.
func (s *Server) kbDocUpload(w http.ResponseWriter, r *http.Request, kbID string) {
	r.Body = http.MaxBytesReader(w, r.Body, maxMultipartBodySize)
	if err := r.ParseMultipartForm(64 << 20); err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			writeJSON(w, http.StatusRequestEntityTooLarge, apiError("上传文件过大（上限 256MB）"))
			return
		}
		writeJSON(w, http.StatusOK, apiError("解析上传文件失败: "+err.Error()))
		return
	}
	chunkSize := 512
	chunkOverlap := 50
	if v := r.FormValue("chunk_size"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			chunkSize = n
		}
	}
	if v := r.FormValue("chunk_overlap"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 {
			chunkOverlap = n
		}
	}

	dir := filepath.Join(s.kbDataDir(), "knowledge_bases", sanitizePath(kbID), "documents")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		writeJSON(w, http.StatusOK, apiError("创建知识库数据目录失败: "+err.Error()))
		return
	}
	var docs []struct {
		DocID string
		Name  string
		Path  string
	}
	for key, files := range r.MultipartForm.File {
		if key != "file" && !strings.HasPrefix(key, "file") && key != "files[]" {
			continue
		}
		for _, fh := range files {
			src, err := fh.Open()
			if err != nil {
				continue
			}
			name := filepath.Base(fh.Filename)
			if name == "" || name == "." {
				name = "document"
			}
			dst := filepath.Join(dir, sanitizePath(name))
			out, err := os.Create(dst)
			if err != nil {
				_ = src.Close()
				continue
			}
			n, err := io.Copy(out, io.LimitReader(src, maxKBDocFileSize+1))
			if err != nil {
				_ = out.Close()
				_ = src.Close()
				_ = os.Remove(dst)
				continue
			}
			_ = out.Close()
			_ = src.Close()
			if n > maxKBDocFileSize {
				_ = os.Remove(dst)
				continue
			}
			info, _ := os.Stat(dst)
			mod := int64(0)
			if info != nil {
				mod = info.ModTime().UnixNano()
			}
			docID := fmt.Sprintf("doc_%d_%s", mod, name)
			docs = append(docs, struct {
				DocID string
				Name  string
				Path  string
			}{DocID: docID, Name: name, Path: dst})
		}
	}
	if len(docs) == 0 {
		writeJSON(w, http.StatusOK, apiError("缺少文件"))
		return
	}
	taskID := fmt.Sprintf("task_%d", time.Now().UnixNano())
	s.recordKBTask(&kbUploadTask{
		TaskID: taskID,
		KBID:   kbID,
		Status: "processing",
		Stage:  "chunking",
		Total:  len(docs) * 100,
	})

	saved := make([]map[string]interface{}, 0, len(docs))
	for _, d := range docs {
		saved = append(saved, map[string]interface{}{
			"doc_id":   d.DocID,
			"doc_name": d.Name,
			"file_size": func() int64 {
				if fi, err := os.Stat(d.Path); err == nil {
					return fi.Size()
				}
				return 0
			}(),
		})
	}

	// Index asynchronously: chunk each file, embed, dual-write.
	go func() {
		success, failed := 0, 0
		total := len(docs)
		for i, d := range docs {
			s.recordKBTask(&kbUploadTask{
				TaskID:       taskID,
				KBID:         kbID,
				Status:       "processing",
				Stage:        "chunking",
				FileIndex:    i,
				Current:      i * 100,
				Total:        total * 100,
				SuccessCount: success,
				FailedCount:  failed,
			})
			content, err := os.ReadFile(d.Path)
			if err != nil {
				failed++
				continue
			}
			if _, err := s.indexKBFile(kbID, d.DocID, d.Name, content, chunkSize, chunkOverlap); err != nil {
				// The file is saved; SQLite records may exist. Report failure
				// for the vector index but keep the doc listed.
				failed++
				s.recordKBTask(&kbUploadTask{
					TaskID:       taskID,
					KBID:         kbID,
					Status:       "processing",
					Stage:        "embedding_failed",
					FileIndex:    i,
					Current:      (i + 1) * 100,
					Total:        total * 100,
					SuccessCount: success,
					FailedCount:  failed,
					Error:        err.Error(),
				})
				continue
			}
			success++
		}
		status := "completed"
		if failed > 0 {
			status = "failed"
		}
		s.recordKBTask(&kbUploadTask{
			TaskID:       taskID,
			KBID:         kbID,
			Status:       status,
			Stage:        "completed",
			Current:      total * 100,
			Total:        total * 100,
			SuccessCount: success,
			FailedCount:  failed,
		})
	}()

	writeJSON(w, http.StatusOK, apiOKMsg("文档上传成功，正在后台分块", map[string]interface{}{
		"task_id":    taskID,
		"file_count": len(docs),
		"documents":  saved,
	}))
}

// kbDocList handles GET /knowledge-bases/{kb_id}/documents — list documents
// stored under the KB data directory, with real pagination honoring the
// page/page_size query params (default page_size 10, aligned with the WebUI's
// document table).
func (s *Server) kbDocList(w http.ResponseWriter, r *http.Request, kbID string) {
	dir := filepath.Join(s.kbDataDir(), "knowledge_bases", sanitizePath(kbID), "documents")
	// 一次聚合查询取回全部 doc_id → chunk 数（GROUP BY），替代循环内逐文档
	// CountKBChunks 的 N+1 模式。
	chunkCounts := map[string]int{}
	if s.database != nil {
		if counts, err := s.database.CountKBChunksByDoc(kbID); err == nil {
			chunkCounts = counts
		}
	}
	items := []interface{}{}
	search := strings.TrimSpace(r.URL.Query().Get("search"))
	if entries, err := os.ReadDir(dir); err == nil {
		for _, e := range entries {
			if e.IsDir() {
				continue
			}
			name := e.Name()
			// search 过滤：按文档文件名做不区分大小写的子串匹配（doc_name）。
			if search != "" && !strings.Contains(strings.ToLower(name), strings.ToLower(search)) {
				continue
			}
			info, _ := e.Info()
			size := int64(0)
			mod := int64(0)
			modTime := time.Time{}
			if info != nil {
				size = info.Size()
				mod = info.ModTime().UnixNano()
				modTime = info.ModTime()
			}
			docID := fmt.Sprintf("doc_%d_%s", mod, e.Name())
			chunkCount := chunkCounts[docID]
			items = append(items, map[string]interface{}{
				"doc_id":      docID,
				"doc_name":    name,
				"file_type":   strings.TrimPrefix(filepath.Ext(name), "."),
				"file_size":   size,
				"created_at":  modTime.Format(time.RFC3339),
				"chunk_count": chunkCount,
			})
		}
	}
	page := parseIntDefault(r.URL.Query().Get("page"), 1)
	pageSize := parseIntDefault(r.URL.Query().Get("page_size"), 10)
	if page < 1 {
		page = 1
	}
	if pageSize < 1 {
		pageSize = 10
	}
	total := len(items)
	start := (page - 1) * pageSize
	if start > total {
		start = total
	}
	end := start + pageSize
	if end > total {
		end = total
	}
	totalPages := 0
	if total > 0 {
		totalPages = (total + pageSize - 1) / pageSize
	}
	writeJSON(w, http.StatusOK, apiOK(map[string]interface{}{
		"items":       items[start:end],
		"page":        page,
		"page_size":   pageSize,
		"total":       total,
		"total_pages": totalPages,
	}))
}

// kbDocumentByID finds a document file in a KB's data directory by its doc_id.
// The doc_id encodes "<modtime>_<filename>"; the file name is the part after
// the first underscore so re-listing and detail both resolve to the same doc.
func (s *Server) kbDocumentByID(kbID, docID string) map[string]interface{} {
	dir := filepath.Join(s.kbDataDir(), "knowledge_bases", sanitizePath(kbID), "documents")
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		info, _ := e.Info()
		mod := int64(0)
		if info != nil {
			mod = info.ModTime().UnixNano()
		}
		candidate := fmt.Sprintf("doc_%d_%s", mod, e.Name())
		if candidate != docID {
			continue
		}
		size := int64(0)
		if info != nil {
			size = info.Size()
		}
		// 详情响应限制带回的正文体积（上传上限 64MB 的文件整读进 JSON 会
		// 打爆内存与响应体）；超大文件提示走分块接口查看内容。
		content := []byte("")
		if size <= 512*1024 {
			content, _ = os.ReadFile(filepath.Join(dir, e.Name()))
		} else {
			content = []byte(fmt.Sprintf("[文件 %d 字节超过 512KB 详情上限，内容已省略]", size))
		}
		chunkCount := 0
		if s.database != nil {
			if n, err := s.database.CountKBChunks(kbID, candidate); err == nil {
				chunkCount = n
			}
		}
		created := ""
		if info != nil {
			created = info.ModTime().Format(time.RFC3339)
		}
		return map[string]interface{}{
			"doc_id":      candidate,
			"doc_name":    e.Name(),
			"file_type":   strings.TrimPrefix(filepath.Ext(e.Name()), "."),
			"file_size":   size,
			"content":     string(content),
			"chunk_count": chunkCount,
			"created_at":  created,
		}
	}
	return nil
}

// anyStr extracts a string from an interface value.
func anyStr(v interface{}) string {
	if s, ok := v.(string); ok {
		return s
	}
	return ""
}
