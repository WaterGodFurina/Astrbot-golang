package dashboard

import (
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/WaterGodFurina/Astrbot-golang/internal/log"
)

// marketCacheTTL controls how long a fetched registry snapshot is served from
// memory before it is refetched.
const marketCacheTTL = 5 * time.Minute

// maxMarketBodySize caps the registry response body to avoid unbounded reads.
const maxMarketBodySize = 4 << 20 // 4 MiB

// validateOutboundURL 校验出站 URL，防止 SSRF：仅允许 http/https，拒绝解析到
// 回环/私有/链路本地（含云元数据 169.254.169.254）地址及 localhost/.local 主机名。
// 供 custom_registry 与 provider 模型探测等由用户输入 URL 的请求共用。
func validateOutboundURL(rawURL string) error {
	u, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil {
		return fmt.Errorf("无效的 URL: %w", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("仅允许 http/https URL，当前 scheme 为 %q", u.Scheme)
	}
	host := strings.ToLower(strings.TrimSpace(u.Hostname()))
	if host == "" {
		return fmt.Errorf("URL 缺少主机名")
	}
	if host == "localhost" || strings.HasSuffix(host, ".localhost") || strings.HasSuffix(host, ".local") {
		return fmt.Errorf("禁止访问本地主机 %q", host)
	}
	if ip := net.ParseIP(host); ip != nil {
		if isBlockedIP(ip) {
			return fmt.Errorf("禁止访问内网/回环地址 %s", ip)
		}
		return nil
	}
	ips, err := net.LookupIP(host)
	if err != nil {
		return fmt.Errorf("域名解析失败 %q: %w", host, err)
	}
	for _, ip := range ips {
		if isBlockedIP(ip) {
			return fmt.Errorf("域名 %s 解析到内网/回环地址 %s", host, ip)
		}
	}
	return nil
}

// isBlockedIP 判断 IP 是否属于禁止出站访问的地址段（私有 10/8、172.16/12、
// 192.168/16、fc00::/7，链路本地 169.254/16、fe80::/10——含云元数据
// 169.254.169.254，未指定地址）。回环（127.0.0.0/8、::1）放行：bot 本就运行在
// 该主机上，允许显式回环 IP 支持本地插件市场/registry 的合法场景；localhost
// 这类主机名仍在上层被拒绝，防止 /etc/hosts 与 DNS rebinding 混淆。
func isBlockedIP(ip net.IP) bool {
	if ip == nil {
		return true
	}
	return ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsUnspecified()
}

// newOutboundClient 构建一个出站 http.Client：除初始 URL 需要调用方先过
// validateOutboundURL 外，重定向的每一跳都会在此处再次校验，防止恶意服务器把
// 请求重定向到内网/云元数据端点绕过 SSRF 防护。
func newOutboundClient(timeout time.Duration) *http.Client {
	return &http.Client{
		Timeout: timeout,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if err := validateOutboundURL(req.URL.String()); err != nil {
				return fmt.Errorf("重定向目标校验失败 %s: %v", req.URL.Redacted(), err)
			}
			return nil
		},
	}
}

// fetchPluginMarket returns the plugin market registry data for the given
// registry URL (defaulting to defaultPluginMarketURL), honoring an in-memory
// cache unless forceRefresh is set. On fetch failure it falls back to the
// cached snapshot; with no cache it returns an explicit error (never a nil
// error, so the WebUI cannot mistake a failed fetch for an empty market).
// The cache is only touched under marketMu; network IO happens outside the
// lock so concurrent market requests are not serialized.
func (s *Server) fetchPluginMarket(registryURL string, forceRefresh bool) (interface{}, error) {
	url := strings.TrimSpace(registryURL)
	if url == "" {
		url = defaultPluginMarketURL
	}
	if err := validateOutboundURL(url); err != nil {
		log.GetDefault().Warn("plugin market URL 校验失败: %v", err)
		return nil, err
	}

	if !forceRefresh {
		if data, ok := s.cachedMarket(url, true); ok {
			return data, nil
		}
	}

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Get(url)
	if err != nil {
		log.GetDefault().Info("plugin market fetch failed: %v", err)
		if data, ok := s.cachedMarket(url, false); ok {
			return data, nil
		}
		return nil, fmt.Errorf("获取插件市场失败: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		log.GetDefault().Info("plugin market fetch failed with status %d", resp.StatusCode)
		if data, ok := s.cachedMarket(url, false); ok {
			return data, nil
		}
		return nil, fmt.Errorf("市场请求失败: HTTP %d", resp.StatusCode)
	}
	data, decErr := decodeMarketBody(resp.Body)
	if decErr != nil {
		log.GetDefault().Info("plugin market decode failed: %v", decErr)
		if cached, ok := s.cachedMarket(url, false); ok {
			return cached, nil
		}
		return nil, fmt.Errorf("市场数据解析失败: %w", decErr)
	}
	s.marketCacheSet(url, data)
	return data, nil
}

// cachedMarket returns a cached registry snapshot for url. freshOnly=true
// requires the snapshot to be within marketCacheTTL; false returns any cached
// entry (stale fallback). Safe for concurrent use.
func (s *Server) cachedMarket(url string, freshOnly bool) (interface{}, bool) {
	s.marketMu.Lock()
	defer s.marketMu.Unlock()
	entry, ok := s.marketCache[url]
	if !ok {
		return nil, false
	}
	if freshOnly && time.Since(entry.fetchedAt) >= marketCacheTTL {
		return nil, false
	}
	return entry.data, true
}

// marketCacheSet stores a fresh registry snapshot under marketMu.
func (s *Server) marketCacheSet(url string, data interface{}) {
	s.marketMu.Lock()
	defer s.marketMu.Unlock()
	s.marketCache[url] = &marketCacheEntry{data: data, fetchedAt: time.Now()}
}

// decodeMarketBody reads and parses a registry JSON payload. It tolerates both
// a plain JSON object and a {data, timestamp} cache wrapper. 响应体大小受限，
// 防止恶意 registry 返回超大 payload 耗尽内存。
func decodeMarketBody(r io.Reader) (interface{}, error) {
	data, err := io.ReadAll(io.LimitReader(r, maxMarketBodySize+1))
	if err != nil {
		return nil, err
	}
	if len(data) > maxMarketBodySize {
		return nil, fmt.Errorf("市场数据过大（超过 %d 字节）", maxMarketBodySize)
	}
	var raw map[string]interface{}
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, err
	}
	if wrapped, ok := raw["data"]; ok {
		return wrapped, nil
	}
	return raw, nil
}
