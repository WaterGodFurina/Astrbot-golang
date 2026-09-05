// QQ 官方分片上传协议实现（>10MB 文件）。
// 1:1 移植自 qqofficial_chunked_upload.py：
//   - upload_prepare：提交哈希（md5/sha1/md5_10m）获取分片计划
//   - 分片 PUT 到预签名 COS URL + upload_part_finish 确认（40093001 重试）
//   - /files 合并（40093001 重试；40093002 每日配额报错）
package qqofficial

import (
	"bytes"
	"context"
	"crypto/md5"
	"crypto/sha1"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"time"
)

// 分片上传常量（对齐 Python qqofficial_chunked_upload.py）
const (
	// ChunkedUploadThreshold 本地文件超过该大小时走分片上传（10MB）
	ChunkedUploadThreshold = 10 * 1024 * 1024
	// chunkMD5Prefix10M QQ 要求的前 10,002,432 字节 MD5
	chunkMD5Prefix10M = 10002432
	// chunkAPIAttempts prepare / merge 的最大尝试次数
	chunkAPIAttempts = 3
	// chunkMaxConcurrency 单文件分片上传最大并发数
	chunkMaxConcurrency = 4
	// chunkPUTAttempts 单个分片 PUT 的最大尝试次数
	chunkPUTAttempts = 3
	// chunkPUTTimeout 单次分片 PUT 超时（对齐 Python _API_TIMEOUT_SECONDS=300）
	chunkPUTTimeout = 300 * time.Second
	// chunkDefaultRetryTimeout upload_part_finish 的默认重试窗口
	chunkDefaultRetryTimeout = 300 * time.Second
	// chunkMaxRetryTimeout upload_part_finish 重试窗口上限
	chunkMaxRetryTimeout = 600 * time.Second
	// chunkDefaultRetryDelay 默认重试间隔
	chunkDefaultRetryDelay = 1 * time.Second
	// chunkRetryableUploadCode QQ 分片瞬时错误码（BDH 内部错误，可安全重试）
	chunkRetryableUploadCode = 40093001
	// chunkDailyQuotaCode QQ 每日文件上传配额耗尽错误码
	chunkDailyQuotaCode = 40093002
)

// ChunkedUploader 通过 QQ 官方 multipart 协议上传一个本地文件。
type ChunkedUploader struct {
	poster APIPoster
	client *http.Client // 直连预签名 COS URL 的 PUT 客户端（独立 300s 超时）
}

// NewChunkedUploader 基于带鉴权 API 能力构造分片上传器。
func NewChunkedUploader(poster APIPoster) *ChunkedUploader {
	return &ChunkedUploader{
		poster: poster,
		client: &http.Client{Timeout: chunkPUTTimeout},
	}
}

// chunkPart 单个分片的 prepare 元数据。
type chunkPart struct {
	Index        int
	BlockSize    int
	PresignedURL string
}

// chunkSession 一次分片上传内所有分片共享的状态（对齐 Python _UploadSession）。
type chunkSession struct {
	basePath      string
	uploadID      string
	blockSize     int
	partIndexBase int
	filePath      string
	fileSize      int64
	retryTimeout  time.Duration
	retryDelay    time.Duration
	totalParts    int
}

// intField 宽松读取整数字段（JSON 数字可能为 float64/int）。
func intField(v interface{}) int {
	switch n := v.(type) {
	case float64:
		return int(n)
	case int:
		return n
	case int64:
		return int(n)
	}
	return 0
}

// partIndexOf 读取分片索引（兼容 index / part_index 两种字段名）。
func partIndexOf(part map[string]interface{}) int {
	if v, ok := part["index"]; ok {
		return intField(v)
	}
	return intField(part["part_index"])
}

// Upload 执行完整分片上传流程：
// prepare → 并发 PUT 分片 + finish → merge，返回 media 元数据
// （file_uuid/file_info/ttl，结构与普通上传接口一致）。
func (u *ChunkedUploader) Upload(scene, targetID, filePath string, fileType int, fileName string, srvSendMsg bool) (map[string]interface{}, error) {
	if targetID == "" {
		return nil, fmt.Errorf("QQ 分片上传缺少接收者 id")
	}
	fi, err := os.Stat(filePath)
	if err != nil || !fi.Mode().IsRegular() {
		return nil, fmt.Errorf("QQ 分片上传文件不存在: %s", filePath)
	}
	fileSize := fi.Size()
	hashes, err := computeFileHashes(filePath)
	if err != nil {
		return nil, fmt.Errorf("QQ 分片上传计算哈希失败: %v", err)
	}
	basePath := mediaBasePath(scene, targetID)

	logger.I18nInfo("[QQOfficial] 启动分片上传: file=%s size=%d type=%d", fileName, fileSize, fileType)

	// ---- upload_prepare（40093001 重试，40093002 配额报错）----
	prepareBody := map[string]interface{}{
		"file_type": fileType,
		"file_size": strconv.FormatInt(fileSize, 10), // 对齐 Python str(file_size)
		"file_name": fileName,
		"md5":       hashes["md5"],
		"sha1":      hashes["sha1"],
		"md5_10m":   hashes["md5_10m"],
	}
	var prepareRes map[string]interface{}
	var lastErr error
	for attempt := 0; attempt < chunkAPIAttempts; attempt++ {
		res, err := u.poster.PostJSON(basePath+"/upload_prepare", prepareBody)
		if apiErr := apiErrOf(res, err); apiErr == nil {
			prepareRes = res
			lastErr = nil
			break
		} else {
			lastErr = apiErr
			if apiErr.Code == chunkDailyQuotaCode {
				return nil, fmt.Errorf("QQ 每日文件上传配额已用尽 (40093002)")
			}
			if apiErr.Code == chunkRetryableUploadCode && attempt < chunkAPIAttempts-1 {
				time.Sleep(chunkDefaultRetryDelay)
				continue
			}
			break
		}
	}
	if lastErr != nil {
		return nil, fmt.Errorf("QQ upload_prepare 失败: %v", lastErr)
	}

	// ---- 解析分片计划 ----
	prepare := prepareRes
	if d, ok := prepareRes["data"].(map[string]interface{}); ok {
		prepare = d
	}
	uploadID, _ := prepare["upload_id"].(string)
	blockSize := intField(prepare["block_size"])
	partsRaw, _ := prepare["parts"].([]interface{})
	if uploadID == "" || blockSize <= 0 || len(partsRaw) == 0 {
		return nil, fmt.Errorf("QQ upload_prepare 响应不完整: %s", toJSONString(prepareRes))
	}
	parts := make([]chunkPart, 0, len(partsRaw))
	lowest := -1
	for _, p := range partsRaw {
		pm, ok := p.(map[string]interface{})
		if !ok {
			return nil, fmt.Errorf("QQ 分片元数据无效: %s", toJSONString(p))
		}
		idx := partIndexOf(pm)
		size := intField(pm["block_size"])
		if size <= 0 {
			size = blockSize
		}
		presigned, _ := pm["presigned_url"].(string)
		if idx < 0 || size <= 0 || presigned == "" {
			return nil, fmt.Errorf("QQ 分片元数据无效: %s", toJSONString(pm))
		}
		parts = append(parts, chunkPart{Index: idx, BlockSize: size, PresignedURL: presigned})
		if lowest < 0 || idx < lowest {
			lowest = idx
		}
	}
	if lowest != 0 && lowest != 1 {
		return nil, fmt.Errorf("不支持的分片起始索引: %d", lowest)
	}

	// ---- upload_config（并发/重试窗口/重试间隔，含边界钳制）----
	concurrency := 1
	retryTimeout := chunkDefaultRetryTimeout
	retryDelay := chunkDefaultRetryDelay
	if uc, ok := prepare["upload_config"].(map[string]interface{}); ok {
		if n := intField(uc["concurrency"]); n > 0 {
			concurrency = n
		}
		if n := intField(uc["retry_timeout"]); n > 0 {
			retryTimeout = time.Duration(n) * time.Second
		}
		if v, ok := uc["retry_delay"].(float64); ok && v > 0 {
			retryDelay = time.Duration(v * float64(time.Second))
		}
	}
	if concurrency > chunkMaxConcurrency {
		concurrency = chunkMaxConcurrency
	}
	if retryTimeout > chunkMaxRetryTimeout {
		retryTimeout = chunkMaxRetryTimeout
	}

	session := &chunkSession{
		basePath:      basePath,
		uploadID:      uploadID,
		blockSize:     blockSize,
		partIndexBase: lowest,
		filePath:      filePath,
		fileSize:      fileSize,
		retryTimeout:  retryTimeout,
		retryDelay:    retryDelay,
		totalParts:    len(parts),
	}
	logger.I18nInfo("[QQOfficial] 分片上传已就绪: 共 %d 个分片", len(parts))

	// ---- 并发上传分片（信号量限流，取第一个错误）----
	sem := make(chan struct{}, concurrency)
	var wg sync.WaitGroup
	var mu sync.Mutex
	var firstErr error
	for _, part := range parts {
		wg.Add(1)
		go func(p chunkPart) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			if err := u.uploadPart(session, p); err != nil {
				mu.Lock()
				if firstErr == nil {
					firstErr = err
				}
				mu.Unlock()
			}
		}(part)
	}
	wg.Wait()
	if firstErr != nil {
		return nil, firstErr
	}

	// ---- merge（/files 合并：40093001 重试，40093002 配额报错）----
	mergeBody := map[string]interface{}{
		"file_type":    fileType,
		"srv_send_msg": srvSendMsg,
		"file_name":    fileName,
		"upload_id":    uploadID,
	}
	var mergeRes map[string]interface{}
	for attempt := 0; attempt < chunkAPIAttempts; attempt++ {
		res, err := u.poster.PostJSON(basePath+"/files", mergeBody)
		if apiErr := apiErrOf(res, err); apiErr == nil {
			mergeRes = res
			lastErr = nil
			break
		} else {
			lastErr = apiErr
			if apiErr.Code == chunkDailyQuotaCode {
				return nil, fmt.Errorf("QQ 每日文件上传配额已用尽 (40093002)")
			}
			if apiErr.Code == chunkRetryableUploadCode && attempt < chunkAPIAttempts-1 {
				time.Sleep(session.retryDelay)
				continue
			}
			break
		}
	}
	if lastErr != nil {
		return nil, fmt.Errorf("QQ 文件合并失败: %v", lastErr)
	}

	merge := mergeRes
	if d, ok := mergeRes["data"].(map[string]interface{}); ok {
		merge = d
	}
	fileUUID, _ := merge["file_uuid"].(string)
	fileInfo, _ := merge["file_info"].(string)
	if fileUUID == "" || fileInfo == "" {
		return nil, fmt.Errorf("QQ 文件合并响应不完整: %s", toJSONString(mergeRes))
	}

	logger.I18nInfo("[QQOfficial] 分片上传完成: %s", fileName)
	return map[string]interface{}{
		"file_uuid": fileUUID,
		"file_info": fileInfo,
		"ttl":       intField(merge["ttl"]),
	}, nil
}

// uploadPart 读取、PUT 并确认一个分片（对齐 Python _upload_part）。
func (u *ChunkedUploader) uploadPart(s *chunkSession, p chunkPart) error {
	offset := int64(p.Index-s.partIndexBase) * int64(s.blockSize)
	length := int64(p.BlockSize)
	if rem := s.fileSize - offset; length > rem {
		length = rem
	}
	if length <= 0 {
		return fmt.Errorf("QQ 分片 %d 超出文件范围", p.Index)
	}
	data, err := readPart(s.filePath, offset, length)
	if err != nil {
		return fmt.Errorf("QQ 分片 %d 读取失败: %v", p.Index, err)
	}
	sum := md5.Sum(data)
	if err := u.putPart(s, p, data); err != nil {
		return err
	}
	return u.finishPart(s, p, int(length), hex.EncodeToString(sum[:]))
}

// putPart 将分片 PUT 到预签名 COS URL（带次数限制重试，对齐 Python _put_part）。
func (u *ChunkedUploader) putPart(s *chunkSession, p chunkPart, data []byte) error {
	var lastErr error
	for attempt := 0; attempt < chunkPUTAttempts; attempt++ {
		if attempt > 0 {
			time.Sleep(s.retryDelay)
		}
		req, err := http.NewRequest(http.MethodPut, p.PresignedURL, bytes.NewReader(data))
		if err != nil {
			lastErr = err
			continue
		}
		req.ContentLength = int64(len(data))
		ctx, cancel := context.WithTimeout(context.Background(), chunkPUTTimeout)
		resp, err := u.client.Do(req.WithContext(ctx))
		if err != nil {
			cancel()
			lastErr = err
			continue
		}
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4<<10))
		_ = resp.Body.Close()
		cancel()
		if resp.StatusCode >= 200 && resp.StatusCode < 300 {
			return nil
		}
		lastErr = fmt.Errorf("COS 返回 HTTP %d", resp.StatusCode)
	}
	return fmt.Errorf("QQ 分片 PUT %d/%d 失败: %v", p.Index+1, s.totalParts, lastErr)
}

// finishPart 确认一个分片：40093001 在重试窗口内循环重试，40093002 报配额错误
// （对齐 Python _finish_part）。
func (u *ChunkedUploader) finishPart(s *chunkSession, p chunkPart, partSize int, partMD5 string) error {
	body := map[string]interface{}{
		"upload_id":  s.uploadID,
		"part_index": p.Index,
		"block_size": strconv.Itoa(partSize), // 对齐 Python str(part_size)
		"md5":        partMD5,
	}
	deadline := time.Now().Add(s.retryTimeout)
	for {
		res, err := u.poster.PostJSON(s.basePath+"/upload_part_finish", body)
		apiErr := apiErrOf(res, err)
		if apiErr == nil {
			return nil
		}
		if apiErr.Code == chunkDailyQuotaCode {
			return fmt.Errorf("QQ 每日文件上传配额已用尽 (40093002)")
		}
		if apiErr.Code != chunkRetryableUploadCode || !time.Now().Before(deadline) {
			return fmt.Errorf("QQ upload_part_finish 失败（分片 %d）: %v", p.Index, apiErr)
		}
		time.Sleep(s.retryDelay)
	}
}

// computeFileHashes 单次遍历文件计算 md5 / sha1 / 前 10002432 字节 md5
// （对齐 Python _compute_file_hashes）。
func computeFileHashes(filePath string) (map[string]string, error) {
	f, err := os.Open(filePath)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	fullMD5 := md5.New()
	fullSHA1 := sha1.New()
	prefixMD5 := md5.New()
	prefixRemaining := chunkMD5Prefix10M
	buf := make([]byte, 64*1024)
	for {
		n, err := f.Read(buf)
		if n > 0 {
			chunk := buf[:n]
			fullMD5.Write(chunk)
			fullSHA1.Write(chunk)
			if prefixRemaining > 0 {
				take := n
				if take > prefixRemaining {
					take = prefixRemaining
				}
				prefixMD5.Write(chunk[:take])
				prefixRemaining -= take
			}
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
	}
	return map[string]string{
		"md5":     hex.EncodeToString(fullMD5.Sum(nil)),
		"sha1":    hex.EncodeToString(fullSHA1.Sum(nil)),
		"md5_10m": hex.EncodeToString(prefixMD5.Sum(nil)),
	}, nil
}

// readPart 读取文件指定偏移处恰好 length 字节（对齐 Python _read_file_part）。
func readPart(filePath string, offset, length int64) ([]byte, error) {
	f, err := os.Open(filePath)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	if _, err := f.Seek(offset, io.SeekStart); err != nil {
		return nil, err
	}
	data := make([]byte, length)
	if _, err := io.ReadFull(f, data); err != nil {
		return nil, err
	}
	return data, nil
}

// statRegular 判断路径是常规文件（仅常规文件参与分片判定）。
func statRegular(path string) (os.FileInfo, error) {
	fi, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	if !fi.Mode().IsRegular() {
		return nil, fmt.Errorf("非常规文件: %s", path)
	}
	return fi, nil
}

// fileToBase64 读取本地文件并 base64 编码。
func fileToBase64(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(data), nil
}

// baseName 返回路径的文件名部分。
func baseName(path string) string { return filepath.Base(path) }
