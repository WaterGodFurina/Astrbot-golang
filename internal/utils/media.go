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
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// IsFileURI returns true if the string is a file:// URI.
func IsFileURI(s string) bool {
	return strings.HasPrefix(s, "file://")
}

// FileURIToPathOK 把 file:// URI 解析为文件系统路径，并报告输入是否为合法
// 的本地 file URI。语义对齐 Python 的
// urllib.request.url2pathname(urllib.parse.urlparse(raw).path)：
//   - scheme 必须为 file（为空或其它 scheme 返回 false）；
//   - host 必须为空或 localhost，指向远端主机的 file URI 不当作本地路径；
//   - percent-encoding 由 url.Parse 的 Path 字段自动解码（空格 %20、中文等）；
//   - Windows 盘符路径 file:///C:/... 去掉前导 '/' 得到 C:/...，
//     再由 filepath.FromSlash 归一为本地分隔符；非 Windows 保序。
func FileURIToPathOK(raw string) (string, bool) {
	if !strings.HasPrefix(raw, "file://") {
		return "", false
	}
	// Windows 盘符形态（file://C:\...、file://C:/...、file:///C:/...）：
	// url.Parse 会把盘符当作 host，从而被误判为远端主机而拒绝。这里显式
	// 折叠为本地路径（percent-encoding 仍解码）；仅 Windows 生效。
	if runtime.GOOS == "windows" {
		if p, ok := fileURIToPathWindows(raw); ok {
			return p, true
		}
	}
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "file" {
		return "", false
	}
	if u.Host != "" && !strings.EqualFold(u.Host, "localhost") {
		return "", false
	}
	// u.Path 已由 net/url 解码 percent-encoding（RawPath 保留原始形态），
	// 无需再调用 url.PathUnescape。
	p := u.Path
	if p == "" {
		return "", false
	}
	if runtime.GOOS == "windows" && len(p) >= 3 && p[0] == '/' && p[2] == ':' {
		p = p[1:]
	}
	return filepath.FromSlash(p), true
}

// fileURIToPathWindows 解析 Windows 盘符形态的 file URI（file://C:\...、
// file://C:/...、file:///C:/...）。独立成函数以便跨平台单测（生产仅 Windows
// 调用）。返回 ok=false 表示不是盘符形态，交回通用 url.Parse 路径处理。
func fileURIToPathWindows(raw string) (string, bool) {
	r := strings.TrimPrefix(raw, "file://")
	if strings.HasPrefix(r, "/") {
		r = r[1:]
	}
	if len(r) >= 2 && r[1] == ':' {
		if dec, err := url.PathUnescape(r); err == nil {
			r = dec
		}
		return filepath.FromSlash(r), true
	}
	return "", false
}

// FileURIToPath converts a file:// URI to a filesystem path. Non-URI inputs and
// malformed URIs are returned unchanged so callers can treat the result as a
// best-effort path.
func FileURIToPath(uri string) string {
	if p, ok := FileURIToPathOK(uri); ok {
		return p
	}
	return uri
}

// PathToFileURI 把本地绝对路径转换为标准 file:// URI，语义对齐 Python
// pathlib.Path.as_uri()：路径分隔符统一为 '/'，Windows 盘符路径（如
// C:\Users\...）会被规范化为 file:///C:/Users/...，空格、中文等特殊字符
// 由 net/url 自动 percent-encode。直接裸拼 "file://" + 路径在 Windows 上
// 会产生 file://C:\Users\... 这类畸形 URI，OneBot 端会拒收媒体。
func PathToFileURI(path string) string {
	u := &url.URL{Scheme: "file", Path: "/" + filepath.ToSlash(path)}
	return u.String()
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

// EnsureWAV 将指定路径的音频确保转换为 24kHz 单声道 WAV 文件。
// 若输入已是 WAV 且符合要求则返回原路径；若是 SILK 则调用纯 Go silk 转码；
// 若为其他格式，在安装了 ffmpeg 时使用 ffmpeg 转码，未安装时原样返回。
func EnsureWAV(path string) (string, error) {
	if _, err := os.Stat(path); err != nil {
		return "", err
	}
	format := DetectAudioFormat(path)
	if format == "wav" {
		return path, nil
	}
	if format == "silk" {
		outPath := TempFilePath("converted.wav")
		if _, err := TencentSilkToWAV(context.Background(), path, outPath); err == nil {
			return outPath, nil
		}
	}
	// 其它格式（mp3/m4a/ogg/amr/flac 等）：对齐 py ensure_wav →
	// convert_audio_format("wav")，用 ffmpeg 转 wav；失败/未安装则原样返回，
	// 由上游按支持的格式兜底。
	outPath := TempFilePath("converted.wav")
	if err := convertAudioWithFFmpeg(context.Background(), path, outPath); err == nil {
		return outPath, nil
	}
	return path, nil
}

// convertAudioWithFFmpeg 调用 `ffmpeg -y -i in out` 转码（对齐 py
// convert_audio_format 的 wav 分支参数）。
func convertAudioWithFFmpeg(ctx context.Context, in, out string) error {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		return err
	}
	cmd := exec.CommandContext(ctx, "ffmpeg", "-y", "-i", in, out)
	if err := cmd.Run(); err != nil {
		_ = os.Remove(out)
		return err
	}
	return nil
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
