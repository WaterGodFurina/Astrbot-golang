// Package utils - media utilities.
// Ported from astrbot/core/utils/media_utils.py
package utils

import (
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// IsFileURI returns true if the string is a file:// URI.
func IsFileURI(s string) bool {
	return strings.HasPrefix(s, "file://")
}

// FileURIToPath converts a file:// URI to a filesystem path.
func FileURIToPath(uri string) string {
	if !IsFileURI(uri) {
		return uri
	}
	u, err := url.Parse(uri)
	if err != nil {
		return strings.TrimPrefix(uri, "file://")
	}
	return u.Path
}

// maxDownloadBytes bounds the response body of remote downloads, protecting
// against unbounded memory/disk usage (mirrors the t2i 64MB limit).
const maxDownloadBytes = 64 << 20

// downloadClient carries an overall timeout and uses http.DefaultTransport so
// the globally-configured proxy (ConfigureGlobalProxy) is honored.
var downloadClient = &http.Client{Timeout: 60 * time.Second}

// DownloadFile downloads a file from a URL to a local path.
func DownloadFile(ctx context.Context, urlStr, destPath string) error {
	req, err := http.NewRequestWithContext(ctx, "GET", urlStr, nil)
	if err != nil {
		return err
	}
	resp, err := downloadClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return fmt.Errorf("download failed: HTTP %d", resp.StatusCode)
	}
	if err := os.MkdirAll(filepath.Dir(destPath), 0750); err != nil {
		return err
	}
	// #nosec G304 -- destPath is the caller-supplied download destination.
	f, err := os.Create(destPath)
	if err != nil {
		return err
	}
	defer f.Close()
	written, err := io.Copy(f, io.LimitReader(resp.Body, maxDownloadBytes+1))
	if err != nil {
		_ = os.Remove(destPath)
		return err
	}
	if written > maxDownloadBytes {
		_ = os.Remove(destPath)
		return fmt.Errorf("download exceeds size limit of %d bytes", maxDownloadBytes)
	}
	return nil
}

// DownloadToBase64 downloads a URL and returns base64-encoded data.
func DownloadToBase64(ctx context.Context, urlStr string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", urlStr, nil)
	if err != nil {
		return "", err
	}
	resp, err := downloadClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return "", fmt.Errorf("download failed: HTTP %d", resp.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxDownloadBytes+1))
	if err != nil {
		return "", err
	}
	if len(data) > maxDownloadBytes {
		return "", fmt.Errorf("download exceeds size limit of %d bytes", maxDownloadBytes)
	}
	return base64.StdEncoding.EncodeToString(data), nil
}

// ReadFileToBase64 reads a local file and returns base64-encoded data.
func ReadFileToBase64(path string) (string, error) {
	// #nosec G304 -- generic utility; the path is provided by the caller.
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(data), nil
}

// EnsureJPEG 返回原始路径，不做实际 JPEG 转码：本实现未引入图像处理依赖
// （Python 原版使用 PIL 转码），调用方不应依赖格式转换结果；只校验文件存在。
func EnsureJPEG(path string) (string, error) {
	// In Go, we would use imaging library to convert.
	// For now, just verify the file exists.
	if _, err := os.Stat(path); err != nil {
		return "", err
	}
	return path, nil
}

// DetectAudioFormat 通过文件头 magic bytes 识别音频格式（对应
// astrbot/core/utils/media_utils.py 的 _get_audio_magic_type）。
// 返回 wav/amr/opus/ogg/flac/mp3/m4a/silk 之一；无法识别时返回空字符串。
func DetectAudioFormat(path string) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	header := make([]byte, 64)
	n, _ := io.ReadFull(f, header)
	header = header[:n]
	if len(header) < 12 {
		return ""
	}

	if bytes.Equal(header[:4], []byte("RIFF")) && bytes.Equal(header[8:12], []byte("WAVE")) {
		return "wav"
	}
	if bytes.Equal(header[:4], []byte("#!AM")) {
		return "amr"
	}
	if bytes.Equal(header[:4], []byte("OggS")) {
		if bytes.Contains(header, []byte("OpusHead")) {
			return "opus"
		}
		return "ogg"
	}
	if bytes.Equal(header[:4], []byte("fLaC")) {
		return "flac"
	}
	if bytes.Equal(header[:3], []byte("ID3")) || (header[0] == 0xff && header[1] == 0xfb) {
		return "mp3"
	}
	if len(header) >= 8 && bytes.Equal(header[4:8], []byte("ftyp")) {
		return "m4a"
	}
	if bytes.HasPrefix(header, []byte("#!SILK_V3")) || bytes.HasPrefix(header, []byte{0x02, '#', '!', 'S', 'I', 'L', 'K', '_', 'V', '3'}) {
		return "silk"
	}
	return ""
}

// EnsureWAV 返回原始路径，不做实际 WAV 转码：本实现未引入 ffmpeg 等依赖
// （Python 原版调用 ffmpeg/tencent_silk_to_wav 转码），调用方不应依赖格式
// 转换结果；只校验文件存在。
func EnsureWAV(path string) (string, error) {
	if _, err := os.Stat(path); err != nil {
		return "", err
	}
	return path, nil
}

// DescribeMediaRef returns a text description of a media reference.
func DescribeMediaRef(ref string) string {
	if IsFileURI(ref) {
		return FileURIToPath(ref)
	}
	if _, err := os.Stat(ref); err == nil {
		return ref // local file
	}
	return ref // URL or other
}

// TempFilePath returns a temporary file path with the given suffix.
func TempFilePath(suffix string) string {
	return filepath.Join(os.TempDir(), fmt.Sprintf("astrbot_%d_%s", time.Now().UnixNano(), suffix))
}

// SaveBase64ToFile saves base64-encoded data to a file.
func SaveBase64ToFile(b64, path string) error {
	data, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0750); err != nil {
		return err
	}
	return os.WriteFile(path, data, 0600)
}

// BytesToBase64 converts bytes to base64 string.
func BytesToBase64(data []byte) string {
	return base64.StdEncoding.EncodeToString(data)
}

// Base64ToBytes converts base64 string to bytes.
func Base64ToBytes(b64 string) ([]byte, error) {
	return base64.StdEncoding.DecodeString(b64)
}

// ResolveMediaPath resolves a media path (file URI, local path, or URL).
func ResolveMediaPath(ref string) string {
	if IsFileURI(ref) {
		return FileURIToPath(ref)
	}
	return ref
}

// ReadAll reads all bytes from a reader and closes it.
func ReadAll(r io.ReadCloser) ([]byte, error) {
	defer r.Close()
	return io.ReadAll(r)
}

// NewBufferReader creates a reader from bytes.
func NewBufferReader(data []byte) *bytes.Reader {
	return bytes.NewReader(data)
}
