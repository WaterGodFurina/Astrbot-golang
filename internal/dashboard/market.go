package dashboard

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/WaterGodFurina/Astrbot-golang/internal/config"
	"github.com/WaterGodFurina/Astrbot-golang/internal/log"
)

// marketCacheTTL controls how long a fetched registry snapshot is served from
// memory before it is refetched.
const marketCacheTTL = 5 * time.Minute

// defaultPythonMarketURL 是 AstrBot 官方（Python）插件市场主源：
// api.soulter.top（国内直连 CDN，Python 原版默认源，2-3s 可拉取 1700+ 插件）。
// GitHub raw 作为兜底源（defaultPythonMarketFallbackURL）。与默认 golang
// 插件源（defaultPluginMarketURL）并列，用户可在 WebUI 插件源管理器中切换。
// 结构为 {"<插件名>": {display_name, desc, author, repo, tags, version, ...}}，
// 前端市场解析已兼容。
const (
	defaultPythonMarketURL         = "https://api.soulter.top/astrbot/plugins"
	defaultPythonMarketFallbackURL = "https://github.com/AstrBotDevs/AstrBot_Plugins_Collection/raw/refs/heads/main/plugin_cache_original.json"
	defaultPluginMarketURL         = "https://astrbotgomarket.350430.xyz/package.json"
	marketDiskCacheDir             = "plugins_market_cache"
	marketDiskCacheTTL             = 10 * time.Minute
)

// defaultPluginSources returns the built-in plugin market sources shown in the
// WebUI source manager, alongside user-defined custom sources. The golang
// source has an empty url = the host's default market (custom_registry empty
// falls back to defaultPluginMarketURL).
func defaultPluginSources() []map[string]interface{} {
	return []map[string]interface{}{
		{"name": "默认golang插件源", "url": "", "builtin": true, "type": "golang"},
		{"name": "默认Python插件源", "url": defaultPythonMarketURL, "builtin": true, "type": "python"},
	}
}

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
// registry URL (defaulting to defaultPluginMarketURL; the Python source falls
// back to its CDN primary then GitHub raw). Cache layers: in-memory
// (marketCacheTTL) → disk (marketDiskCacheTTL, survives restarts, aligned with
// Python's data/plugins.json) → network. forceRefresh bypasses the fresh caches
// but still falls back to stale disk cache on network failure, so switching
// sources never blocks the WebUI for the full timeout.
func (s *Server) fetchPluginMarket(registryURL string, forceRefresh bool) (interface{}, error) {
	raw := strings.TrimSpace(registryURL)
	urls := []string{raw}
	if raw == "" {
		urls = []string{defaultPluginMarketURL}
	} else if raw == defaultPythonMarketURL {
		urls = []string{defaultPythonMarketURL, defaultPythonMarketFallbackURL}
	}
	// 应用 GitHub 加速前缀（config github_proxy）：raw.githubusercontent.com
	// 兜底源直连慢/超时时走加速。
	proxy := s.githubProxyForMarket()
	prepared := make([]string, 0, len(urls))
	for _, u := range urls {
		if proxy != "" && strings.HasPrefix(u, "https://raw.githubusercontent.com/") {
			u = strings.TrimRight(proxy, "/") + "/" + u
		}
		if err := validateOutboundURL(u); err != nil {
			log.GetDefault().Warn("plugin market URL 校验失败: %v", err)
			continue
		}
		prepared = append(prepared, u)
	}
	if len(prepared) == 0 {
		return nil, fmt.Errorf("插件市场 URL 校验失败")
	}

	key := prepared[0]
	if !forceRefresh {
		if data, ok := s.cachedMarket(key, true); ok {
			return data, nil
		}
		if data, ok := s.cachedMarketDisk(key, true); ok {
			s.marketCacheSet(key, data)
			return data, nil
		}
	}

	client := newOutboundClient(30 * time.Second)
	var lastErr error
	for _, u := range prepared {
		resp, err := client.Get(u)
		if err != nil {
			log.GetDefault().Info("plugin market fetch failed (%s): %v", u, err)
			lastErr = err
			continue
		}
		if resp.StatusCode != http.StatusOK {
			log.GetDefault().Info("plugin market fetch failed (%s): HTTP %d", u, resp.StatusCode)
			lastErr = fmt.Errorf("HTTP %d", resp.StatusCode)
			_ = resp.Body.Close()
			continue
		}
		data, decErr := decodeMarketBody(resp.Body)
		_ = resp.Body.Close()
		if decErr != nil {
			log.GetDefault().Info("plugin market decode failed (%s): %v", u, decErr)
			lastErr = decErr
			continue
		}
		s.marketCacheSet(key, data)
		s.marketCacheSetDisk(key, data)
		return data, nil
	}

	// 网络全失败：回退磁盘缓存（即使过期），避免切换源时阻塞/空市场。
	if data, ok := s.cachedMarketDisk(key, false); ok {
		log.GetDefault().Warn("市场拉取失败，回退磁盘缓存: %v", lastErr)
		s.marketCacheSet(key, data)
		return data, nil
	}
	return nil, fmt.Errorf("获取插件市场失败: %v", lastErr)
}

// githubProxyForMarket reads the configured GitHub accelerator
// (config github_proxy, e.g. https://ghfast.top/) used to prefix
// raw.githubusercontent.com market URLs. Empty when unset.
func (s *Server) githubProxyForMarket() string {
	if s.configMgr == nil {
		return ""
	}
	cm, ok := s.configMgr.(*config.ConfigManager)
	if !ok {
		return ""
	}
	cfg := cm.Get("default")
	if cfg == nil {
		return ""
	}
	return strings.TrimSpace(cfg.GetString("github_proxy"))
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

// marketDiskCachePath 返回市场数据的本地持久化缓存路径（对齐 Python
// data/plugins.json / plugins_custom_<hash>.json 语义，跨重启有效）。
func (s *Server) marketDiskCachePath(url string) string {
	sum := sha256.Sum256([]byte(url))
	return filepath.Join(s.dataDir, marketDiskCacheDir, fmt.Sprintf("%x.json", sum[:16]))
}

// marketCacheSetDisk 把市场数据写入磁盘缓存（{fetched_at, data} 包装）。
func (s *Server) marketCacheSetDisk(url string, data interface{}) {
	path := s.marketDiskCachePath(url)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return
	}
	payload := map[string]interface{}{"fetched_at": time.Now().Unix(), "data": data}
	b, err := json.Marshal(payload)
	if err != nil {
		return
	}
	_ = os.WriteFile(path, b, 0o644)
}

// cachedMarketDisk 读取磁盘缓存。freshOnly=true 要求新鲜（marketDiskCacheTTL
// 内），false 返回任意（含过期，供网络失败回退）。dataDir 为空（测试构造）
// 时直接 miss。
func (s *Server) cachedMarketDisk(url string, freshOnly bool) (interface{}, bool) {
	if s.dataDir == "" {
		return nil, false
	}
	b, err := os.ReadFile(s.marketDiskCachePath(url))
	if err != nil {
		return nil, false
	}
	var payload struct {
		FetchedAt int64           `json:"fetched_at"`
		Data      json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(b, &payload); err != nil {
		return nil, false
	}
	if freshOnly && time.Since(time.Unix(payload.FetchedAt, 0)) >= marketDiskCacheTTL {
		return nil, false
	}
	// #nosec unsafe-deserialization-interface -- 市场缓存数据：由宿主自己的缓存文件写入
	//（fetchMarketBody 已限流 maxMarketBodySize），结构动态故用 interface{}，非攻击面。
	var data interface{} // nosemgrep: go.lang.security.deserialization.unsafe-deserialization-interface.go-unsafe-deserialization-interface
	if err := json.Unmarshal(payload.Data, &data); err != nil {
		return nil, false
	}
	return data, true
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

// repoIdentifierFromURL 从仓库 URL 提取 owner/name 标识（对齐 Python
// repo_identifier_from_url：github.com/owner/name[.git] → owner/name）。
func repoIdentifierFromURL(repoURL string) string {
	text := strings.TrimSpace(strings.TrimRight(strings.TrimSuffix(repoURL, ".git"), "/"))
	if text == "" {
		return ""
	}
	u, err := url.Parse(text)
	if err != nil || u.Host == "" {
		return ""
	}
	parts := strings.Split(strings.Trim(u.Path, "/"), "/")
	if len(parts) < 2 {
		return ""
	}
	return strings.ToLower(parts[len(parts)-2] + "/" + parts[len(parts)-1])
}

// marketEntryIdentifier 返回市场条目的标识：market_plugin_id 字段优先，
// 否则 author/name 组合；对齐 Python get_market_plugin_id。
func marketEntryIdentifier(entry map[string]interface{}, fallbackKey string) string {
	if v, _ := entry["market_plugin_id"].(string); strings.TrimSpace(v) != "" {
		return strings.TrimSpace(v)
	}
	author, _ := entry["author"].(string)
	name, _ := entry["name"].(string)
	author = strings.TrimSpace(author)
	name = strings.TrimSpace(name)
	fallbackKey = strings.TrimSpace(fallbackKey)
	if name == "" && fallbackKey != "" && !strings.Contains(fallbackKey, "/") {
		name = fallbackKey
	}
	if author != "" && name != "" {
		return author + "/" + name
	}
	return ""
}

// resolveMarketPluginEntry 在市场数据（dict 或数组）中按标识匹配插件条目，
// 匹配顺序对齐 Python resolve_market_plugin_entry：market 标识 → repo 标识
// → repo URL → name/marketplace_name。
func resolveMarketPluginEntry(marketData interface{}, recordID, recordRepo, recordName string) (map[string]interface{}, bool) {
	recordID = strings.TrimSpace(recordID)
	recordRepoIdent := repoIdentifierFromURL(recordRepo)
	recordRepoNorm := strings.ToLower(strings.TrimRight(strings.TrimSpace(recordRepo), "/"))
	recordNames := map[string]bool{}
	for _, n := range []string{strings.TrimSpace(recordName), strings.ReplaceAll(strings.TrimSpace(recordName), "_", "-")} {
		if n != "" {
			recordNames[n] = true
		}
	}
	matches := func(ident string, pluginEntry map[string]interface{}) bool {
		ident = strings.TrimSpace(ident)
		if recordID != "" && ident == recordID {
			return true
		}
		if recordRepoIdent != "" && ident == recordRepoIdent {
			return true
		}
		pluginRepo, _ := pluginEntry["repo"].(string)
		pluginRepo = strings.TrimSpace(pluginRepo)
		if recordRepoIdent != "" && repoIdentifierFromURL(pluginRepo) == recordRepoIdent {
			return true
		}
		if recordRepoNorm != "" && strings.ToLower(strings.TrimRight(pluginRepo, "/")) == recordRepoNorm {
			return true
		}
		if pluginName, _ := pluginEntry["name"].(string); strings.TrimSpace(pluginName) != "" {
			return recordNames[strings.TrimSpace(pluginName)]
		}
		return false
	}
	switch data := marketData.(type) {
	case map[string]interface{}:
		for key, raw := range data {
			if key == "$meta" {
				continue
			}
			pluginEntry, ok := raw.(map[string]interface{})
			if !ok {
				continue
			}
			if matches(marketEntryIdentifier(pluginEntry, key), pluginEntry) {
				return pluginEntry, true
			}
		}
	case []interface{}:
		for _, raw := range data {
			pluginEntry, ok := raw.(map[string]interface{})
			if !ok {
				continue
			}
			if matches(marketEntryIdentifier(pluginEntry, ""), pluginEntry) {
				return pluginEntry, true
			}
		}
	}
	return nil, false
}

// resolveLatestPluginSource 更新插件前从原 registry 重新解析该插件的最新
// 下载源（对齐 Python resolve_market_update_info）：market 类型且 registry
// 拉取/匹配成功时返回最新 download_url/repo；否则返回空（调用方回退
// manifest 记录的旧源，不阻断更新）。
func (s *Server) resolveLatestPluginSource(pid string) (dlURL, repo string) {
	if s.subPluginMgr == nil {
		return "", ""
	}
	entry := s.subPluginMgr.ManifestEntry(pid)
	if entry == nil || !strings.EqualFold(strings.TrimSpace(entry.InstallMethod), "market") {
		return "", ""
	}
	registryURL := strings.TrimSpace(entry.RegistryURL)
	if registryURL == "" {
		return "", ""
	}
	marketData, err := s.fetchPluginMarket(registryURL, false)
	if err != nil {
		log.GetDefault().Warn("更新插件 %s 前拉取市场数据失败，回退记录源: %v", pid, err)
		return "", ""
	}
	pluginEntry, ok := resolveMarketPluginEntry(marketData, entry.MarketPluginID, entry.Repo, entry.Name)
	if !ok {
		return "", ""
	}
	dl, _ := pluginEntry["download_url"].(string)
	r, _ := pluginEntry["repo"].(string)
	return strings.TrimSpace(dl), strings.TrimSpace(r)
}
