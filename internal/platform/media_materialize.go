// Media URL materialization: 在平台发送链路的统一入口，把消息链中携带的
// http(s) 媒体 URL 下载为本地临时文件，使"把 URL 当本地路径"的适配器
// （如钉钉 voice/video/file）不再把 URL 直接喂给 os.ReadFile / ffmpeg。
//
// 该逻辑挂在 PlatformManager.Send 这个所有平台共用的发送咽喉上，因此对
// pipeline、dashboard 兜底路径、插件 host_service 的 SendMessage 一律生效。
package platform

import (
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/WaterGodFurina/Astrbot-golang/pkg/message"
)

// mediaDownloadTimeout 限制单个媒体 URL 的下载时长。
// 用 var 而非 const，便于单测缩短超时验证失败路径。
var mediaDownloadTimeout = 30 * time.Second

// maxMediaDownloadSize 限制单个媒体 URL 的下载大小。
// 用 var 而非 const，便于单测调小上限验证超限路径。
var maxMediaDownloadSize = 50 << 20 // 50 MiB

// materializeRemoteMedia downloads http(s) media URLs found in a chain to local
// temp files. Returns a cleanup func that removes every temp file created; the
// caller must invoke it after the send completes.
//
// Components that already carry a local path, carry a non-http(s) URL, or whose
// download fails / is rejected by the host guard are left untouched (their URL
// stays intact so URL-aware adapters keep working). Nested chains (Reply /
// forward Nodes) are walked recursively.
func materializeRemoteMedia(chain *message.MessageChain) (cleanup func()) {
	if chain == nil {
		return func() {}
	}
	var temps []string
	var walk func(comps []message.Component)
	walk = func(comps []message.Component) {
		for _, comp := range comps {
			switch c := comp.(type) {
			case *message.Image:
				if p := materializeMediaURL(c.URL, c.Path, c.File, &temps); p != "" {
					c.Path, c.File = p, p
				}
			case *message.Record:
				if p := materializeMediaURL(c.URL, c.Path, c.File, &temps); p != "" {
					c.Path, c.File = p, p
				}
			case *message.Video:
				if p := materializeMediaURL(c.URL, c.Path, "", &temps); p != "" {
					c.Path = p
				}
			case *message.File:
				if p := materializeMediaURL(c.URL, c.Path, "", &temps); p != "" {
					c.Path = p
				}
			case *message.Reply:
				walk(c.Chain)
			case *message.Nodes:
				for _, n := range c.Nodes {
					if n != nil {
						walk(n.Content)
					}
				}
			}
		}
	}
	walk(chain.Chain)
	return func() {
		for _, p := range temps {
			_ = os.Remove(p)
		}
	}
}

// materializeMediaURL downloads remoteURL to a temp file when the component has
// no usable local path. Returns the temp path, or "" when nothing was
// downloaded (no URL / already local / download failed / host rejected).
func materializeMediaURL(remoteURL, path, file string, temps *[]string) string {
	if isLocalMediaPath(path) || isLocalMediaPath(file) {
		return "" // 已有本地路径，无需下载
	}
	if !isHTTPURL(remoteURL) {
		return ""
	}
	if !mediaHostAllowed(remoteURL) {
		logger.Warn("媒体 URL 因地址受限被拒绝（拒绝链路本地/组播/未指定地址）: %s", remoteURL)
		return ""
	}
	tmp, err := downloadMediaToTemp(remoteURL)
	if err != nil {
		logger.Warn("媒体 URL 下载失败 (%s): %v", remoteURL, err)
		return ""
	}
	*temps = append(*temps, tmp)
	return tmp
}

// isLocalMediaPath reports whether p is a non-empty value that looks like a
// local filesystem path rather than a URL.
func isLocalMediaPath(p string) bool {
	return p != "" && !strings.HasPrefix(p, "http://") && !strings.HasPrefix(p, "https://")
}

func isHTTPURL(s string) bool {
	return strings.HasPrefix(s, "http://") || strings.HasPrefix(s, "https://")
}

// mediaHostAllowed rejects hosts whose IP is link-local (covers the cloud
// metadata endpoints 169.254.169.254), multicast or unspecified. Loopback and
// RFC1918 private ranges are allowed on purpose: media URLs routinely point to
// the bot's own loopback services (aiocqhttp / line media servers) or LAN
// storage, and unlike web_fetch the fetched bytes are only forwarded to a chat
// platform. DNS failure fails closed (reject).
func mediaHostAllowed(rawURL string) bool {
	u, err := url.Parse(rawURL)
	if err != nil || u.Hostname() == "" {
		return false
	}
	host := u.Hostname()
	if ip := net.ParseIP(host); ip != nil {
		return ipAllowed(ip)
	}
	addrs, err := net.LookupIP(host)
	if err != nil {
		return false
	}
	for _, a := range addrs {
		if !ipAllowed(a) {
			return false
		}
	}
	return true
}

func ipAllowed(ip net.IP) bool {
	if ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() ||
		ip.IsMulticast() || ip.IsUnspecified() {
		return false
	}
	return true
}

// downloadMediaToTemp downloads rawURL (2xx only) to a temp file with a size
// cap and preserves a sanitized file extension so MIME-sniffing adapters keep
// working. Returns the temp path.
func downloadMediaToTemp(rawURL string) (string, error) {
	client := &http.Client{Timeout: mediaDownloadTimeout}
	resp, err := client.Get(rawURL)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("http %d", resp.StatusCode)
	}

	pattern := "astrbot-media-*"
	if ext := sanitizedURLExt(rawURL); ext != "" {
		pattern = "astrbot-media-*" + ext
	}
	f, err := os.CreateTemp("", pattern)
	if err != nil {
		return "", err
	}
	name := f.Name()
	written, err := io.Copy(f, io.LimitReader(resp.Body, int64(maxMediaDownloadSize)+1))
	if err != nil {
		_ = f.Close()
		_ = os.Remove(name)
		return "", err
	}
	if written > int64(maxMediaDownloadSize) {
		_ = f.Close()
		_ = os.Remove(name)
		return "", fmt.Errorf("媒体超过 %d 字节", maxMediaDownloadSize)
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(name)
		return "", err
	}
	return name, nil
}

// sanitizedURLExt extracts a safe extension (e.g. ".mp3") from a URL path,
// ignoring any query/fragment and non-alphanumeric suffix garbage.
func sanitizedURLExt(rawURL string) string {
	u, err := url.Parse(rawURL)
	if err != nil {
		return ""
	}
	ext := filepath.Ext(u.Path)
	if ext == "" {
		return ""
	}
	// Keep only letters/digits, allow at most one leading dot.
	ext = strings.ToLower(ext)
	var b strings.Builder
	for _, r := range ext {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
		}
	}
	if b.Len() == 0 {
		return ""
	}
	return "." + b.String()
}
