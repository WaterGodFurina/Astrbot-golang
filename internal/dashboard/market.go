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

// fetchPluginMarket returns the plugin market registry data for the given
// registry URL (defaulting to defaultPluginMarketURL), honoring an in-memory
// cache unless forceRefresh is set. On fetch failure it falls back to the
// cached snapshot; with no cache it returns an error.
func (s *Server) fetchPluginMarket(registryURL string, forceRefresh bool) (interface{}, error) {
	url := strings.TrimSpace(registryURL)
	if url == "" {
		url = defaultPluginMarketURL
	}
	if err := validateOutboundURL(url); err != nil {
		log.GetDefault().Warn("plugin market URL 校验失败: %v", err)
		return nil, err
	}

	s.marketMu.Lock()
	defer s.marketMu.Unlock()

	if !forceRefresh {
		if entry, ok := s.marketCache[url]; ok && time.Since(entry.fetchedAt) < marketCacheTTL {
			return entry.data, nil
		}
	}

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Get(url)
	if err == nil {
		defer resp.Body.Close()
		if resp.StatusCode == http.StatusOK {
			data, decErr := decodeMarketBody(resp.Body)
			if decErr == nil {
				s.marketCache[url] = &marketCacheEntry{data: data, fetchedAt: time.Now()}
				return data, nil
			}
			log.GetDefault().Info("plugin market decode failed: %v", decErr)
		} else {
			log.GetDefault().Info("plugin market fetch failed with status %d", resp.StatusCode)
		}
	} else {
		log.GetDefault().Info("plugin market fetch failed: %v", err)
	}

	if entry, ok := s.marketCache[url]; ok {
		return entry.data, nil
	}
	return nil, err
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
