package platform

import (
	"mime"
	"path/filepath"
	"strings"
)

// mimeByExt 内置的固定扩展名 → MIME 映射。优先使用它而不是
// mime.TypeByExtension：后者依赖宿主机（Unix 会读取 /etc/mime.types，
// Windows 读取注册表），同一文件在不同 OS 上可能得到不同结果（甚至为空），
// 导致消息里的 media content-type 跨平台不一致。未覆盖的扩展名再回退到
// 标准库，兼顾完整性与稳定性。
var mimeByExt = map[string]string{
	// 图片
	".jpg":  "image/jpeg",
	".jpeg": "image/jpeg",
	".png":  "image/png",
	".gif":  "image/gif",
	".webp": "image/webp",
	".bmp":  "image/bmp",
	".svg":  "image/svg+xml",
	".ico":  "image/x-icon",
	".tif":  "image/tiff",
	".tiff": "image/tiff",
	// 音频
	".mp3":  "audio/mpeg",
	".wav":  "audio/wav",
	".ogg":  "audio/ogg",
	".oga":  "audio/ogg",
	".opus": "audio/opus",
	".m4a":  "audio/mp4",
	".aac":  "audio/aac",
	".flac": "audio/flac",
	".amr":  "audio/amr",
	".silk": "audio/silk",
	// 视频
	".mp4":  "video/mp4",
	".m4v":  "video/mp4",
	".mov":  "video/quicktime",
	".webm": "video/webm",
	".avi":  "video/x-msvideo",
	".mkv":  "video/x-matroska",
	// 文档
	".pdf":  "application/pdf",
	".txt":  "text/plain",
	".json": "application/json",
	".zip":  "application/zip",
	".gzip": "application/gzip",
	".apk":  "application/vnd.android.package-archive",
}

// MIMETypeByExt 返回扩展名对应的 MIME 类型；内置映射优先，未命中时回退
// 标准库 mime.TypeByExtension。ext 可以是 ".png" 或 "png"。
func MIMETypeByExt(ext string) string {
	if ext == "" {
		return ""
	}
	if !strings.HasPrefix(ext, ".") {
		ext = "." + ext
	}
	if m, ok := mimeByExt[strings.ToLower(ext)]; ok {
		return m
	}
	return mime.TypeByExtension(strings.ToLower(ext))
}

// MIMETypeByFilename 从文件名推断 MIME 类型。
func MIMETypeByFilename(name string) string {
	return MIMETypeByExt(filepath.Ext(name))
}

// preferredExtByMIME 是 MIME → 首选扩展名的固定反向映射，用于按
// Content-Type 猜测文件后缀（Python mimetypes.guess_extension 的对应实现）。
// 不使用 mime.ExtensionsByType：后者同样依赖宿主机的类型表，跨 OS 结果不同。
var preferredExtByMIME = map[string]string{
	"image/jpeg":               ".jpg",
	"image/png":                ".png",
	"image/gif":                ".gif",
	"image/webp":               ".webp",
	"image/bmp":                ".bmp",
	"image/svg+xml":            ".svg",
	"image/tiff":               ".tiff",
	"audio/mpeg":               ".mp3",
	"audio/wav":                ".wav",
	"audio/x-wav":              ".wav",
	"audio/ogg":                ".ogg",
	"audio/opus":               ".opus",
	"audio/mp4":                ".m4a",
	"audio/aac":                ".aac",
	"audio/flac":               ".flac",
	"audio/amr":                ".amr",
	"video/mp4":                ".mp4",
	"video/quicktime":          ".mov",
	"video/webm":               ".webm",
	"video/x-msvideo":          ".avi",
	"video/x-matroska":         ".mkv",
	"application/pdf":          ".pdf",
	"application/json":         ".json",
	"application/zip":          ".zip",
	"application/octet-stream": ".bin",
	"text/plain":               ".txt",
}

// ExtensionByMIMEType 返回 MIME 类型对应的首选扩展名（含前导点），未知返回空串。
func ExtensionByMIMEType(mimeType string) string {
	base := strings.ToLower(strings.TrimSpace(strings.SplitN(mimeType, ";", 2)[0]))
	return preferredExtByMIME[base]
}
