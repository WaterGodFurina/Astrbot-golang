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
	if err := os.MkdirAll(filepath.Dir(destPath), 0755); err != nil {
		return err
	}
	f, err := os.Create(destPath)
	if err != nil {
		return err
	}
	defer f.Close()
	written, err := io.Copy(f, io.LimitReader(resp.Body, maxDownloadBytes+1))
	if err != nil {
		return err
	}
	if written > maxDownloadBytes {
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
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(data), nil
}

// EnsureJPEG converts an image to JPEG format (placeholder — returns original path).
func EnsureJPEG(path string) (string, error) {
	// In Go, we would use imaging library to convert.
	// For now, just verify the file exists.
	if _, err := os.Stat(path); err != nil {
		return "", err
	}
	return path, nil
}

// EnsureWAV converts audio to WAV format (placeholder — returns original path).
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
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return err
	}
	return os.WriteFile(path, data, 0644)
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
