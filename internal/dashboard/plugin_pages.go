// Package dashboard - 插件自带 Web 页面（Plugin Pages）。
//
// 对齐 Python astrbot/dashboard/services/plugin_page_service.py：插件目录下
// pages/<page_name>/index.html 即为一个页面。入口端点（GET /plugins/page）
// 校验插件存在且已启用后，签发短期 asset_token 并返回 content_path；内容端点
// （GET /plugins/page-content/...）凭 query 上的 asset_token 或入口时种下的
// 专用 Cookie 放行——iframe 与其子资源（css/js/img）无法携带 Authorization 头，
// Cookie 方案免去 Python 版对 HTML/CSS/JS 的 URL 重写。
package dashboard

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"mime"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	pluginPageAssetCookieName = "plugin_page_asset"
	pluginPageAssetTTL        = 10 * time.Minute
	pluginPageEntryFile       = "index.html"
	pluginPageRootName        = "pages"
	pluginPageMaxAssetSize    = 8 << 20
)

var (
	pluginPageSecretOnce     sync.Once
	pluginPageSecretFallback string
)

// pluginPageSecret 返回 asset_token 的签名密钥：优先复用 dashboard 的 JWT
// secret（随密码文件持久化），无认证实例时退化为进程级随机值。
func (s *Server) pluginPageSecret() string {
	if s.auth != nil {
		if secret := s.auth.JWTSecret(); secret != "" {
			return secret
		}
	}
	pluginPageSecretOnce.Do(func() {
		pluginPageSecretFallback = generateRandomToken(32)
	})
	return pluginPageSecretFallback
}

// issuePluginPageAssetToken 签发 plugin|page|exp 的 HMAC-SHA256 短期 token。
func (s *Server) issuePluginPageAssetToken(pluginID, page string) string {
	payload := pluginID + "\x00" + page + "\x00" +
		strconv.FormatInt(time.Now().Add(pluginPageAssetTTL).Unix(), 10)
	mac := hmac.New(sha256.New, []byte(s.pluginPageSecret()))
	mac.Write([]byte(payload))
	return base64.RawURLEncoding.EncodeToString([]byte(payload)) + "." +
		base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

// validPluginPageAssetToken 常时比较签名并校验有效期与 plugin/page 归属。
func (s *Server) validPluginPageAssetToken(token, pluginID, page string) bool {
	head, sig, ok := strings.Cut(token, ".")
	if !ok {
		return false
	}
	raw, err := base64.RawURLEncoding.DecodeString(head)
	if err != nil {
		return false
	}
	sigBytes, err := base64.RawURLEncoding.DecodeString(sig)
	if err != nil {
		return false
	}
	mac := hmac.New(sha256.New, []byte(s.pluginPageSecret()))
	mac.Write(raw)
	if !hmac.Equal(sigBytes, mac.Sum(nil)) {
		return false
	}
	seg := strings.Split(string(raw), "\x00")
	if len(seg) != 3 || seg[0] != pluginID || seg[1] != page {
		return false
	}
	exp, err := strconv.ParseInt(seg[2], 10, 64)
	return err == nil && time.Now().Unix() < exp
}

// pluginPageAssetAllowed 供 router 鉴权白名单调用：asset_token（query）或
// 专用 Cookie 任一有效即放行 page-content 资产请求。
func (s *Server) pluginPageAssetAllowed(r *http.Request, parts []string) bool {
	if len(parts) < 2 || parts[0] == "" || parts[1] == "" {
		return false
	}
	pluginID, page := parts[0], parts[1]
	if token := r.URL.Query().Get("asset_token"); token != "" &&
		s.validPluginPageAssetToken(token, pluginID, page) {
		return true
	}
	if cookie, err := r.Cookie(pluginPageAssetCookieName); err == nil {
		return s.validPluginPageAssetToken(cookie.Value, pluginID, page)
	}
	return false
}

// normalizePluginPageName 对齐 Python normalize_plugin_page_name：拒绝空名、
// 点前缀与任何路径分隔符，杜绝把 page 名当路径段穿越。
func normalizePluginPageName(raw string) (string, error) {
	name := strings.TrimSpace(raw)
	if name == "" {
		return "", errors.New("无效的插件 Page 名称")
	}
	if strings.ContainsAny(name, "/\\") || strings.HasPrefix(name, ".") || name == ".." {
		return "", errors.New("无效的插件 Page 名称")
	}
	return name, nil
}

// safePluginDirID 与 internal/plugin 的 sanitizeID 同规则（含全点段拒绝），
// 防止插件 id 拼路径时逃逸 plugins 目录。
func safePluginDirID(id string) string {
	var b strings.Builder
	for _, r := range id {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9',
			r == '-', r == '_', r == '.', r >= 0x4e00 && r <= 0x9fff:
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	s := b.String()
	if strings.Trim(s, ".") == "" {
		return "_"
	}
	return s
}

// pluginActivated 用规范 id 查插件行，返回 (canonicalID, activated)。
func (s *Server) pluginActivated(idOrName string) (string, bool) {
	pid, _, ok := s.resolveSubprocessPlugin(idOrName)
	if !ok {
		return "", false
	}
	if s.subPluginMgr != nil {
		for _, m := range s.subPluginMgr.ListInfo() {
			if rowID, _ := m["id"].(string); rowID == pid {
				activated, _ := m["activated"].(bool)
				return pid, activated
			}
		}
	}
	return pid, false
}

// pluginPageRoot 返回 <dataDir>/plugins/<id>/pages/<page> 的绝对路径，并做
// plugins 目录包含性校验。
func (s *Server) pluginPageRoot(pluginID, page string) (string, error) {
	pluginsRoot, err := filepath.Abs(filepath.Join(s.kbDataDir(), "plugins"))
	if err != nil {
		return "", err
	}
	root, err := filepath.Abs(filepath.Join(pluginsRoot, safePluginDirID(pluginID), pluginPageRootName, page))
	if err != nil {
		return "", err
	}
	if !strings.HasPrefix(root, pluginsRoot+string(os.PathSeparator)) {
		return "", errors.New("非法的插件目录")
	}
	return root, nil
}

// handlePluginPage implements GET /plugins/page?plugin_id=&page_name=（对齐
// Python get_plugin_page_entry_config）：返回页面入口与带短期 asset_token
// 的 content_path，同时种下专用 Cookie 供 iframe 子资源免头鉴权。
func (s *Server) handlePluginPage(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSON(w, http.StatusMethodNotAllowed, apiError("仅支持 GET"))
		return
	}
	pluginID := r.URL.Query().Get("plugin_id")
	page, err := normalizePluginPageName(r.URL.Query().Get("page_name"))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, apiError(err.Error()))
		return
	}
	pid, activated := s.pluginActivated(pluginID)
	if pid == "" {
		writeJSON(w, http.StatusNotFound, apiError("插件不存在"))
		return
	}
	if !activated {
		writeJSON(w, http.StatusForbidden, apiError("插件未启用"))
		return
	}
	root, err := s.pluginPageRoot(pid, page)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, apiError(err.Error()))
		return
	}
	if info, err := os.Stat(filepath.Join(root, pluginPageEntryFile)); err != nil || info.IsDir() {
		writeJSON(w, http.StatusNotFound, apiError("插件 Page 不存在"))
		return
	}
	token := s.issuePluginPageAssetToken(pid, page)
	http.SetCookie(w, &http.Cookie{
		Name:     pluginPageAssetCookieName,
		Value:    token,
		Path:     "/api/",
		MaxAge:   int(pluginPageAssetTTL.Seconds()),
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
	})
	writeJSON(w, http.StatusOK, apiOK(map[string]interface{}{
		"name":     page,
		"title":    page,
		"i18n_key": "pages." + page,
		"content_path": "/api/v1/plugins/page-content/" +
			url.PathEscape(pid) + "/" + url.PathEscape(page) + "/?asset_token=" + url.QueryEscape(token),
	}))
}

// servePluginPageContent implements GET /plugins/page-content/{plugin}/{page}[/{asset}]
// （对齐 Python serve_page_content，简化点：不重写 HTML，子资源凭 Cookie 鉴权）。
func (s *Server) servePluginPageContent(w http.ResponseWriter, r *http.Request, parts []string) {
	if len(parts) < 2 {
		writeJSON(w, http.StatusNotFound, apiError("插件 Page 不存在"))
		return
	}
	pluginID, page := parts[0], parts[1]
	pageNorm, err := normalizePluginPageName(page)
	if err != nil || pageNorm != page {
		writeJSON(w, http.StatusNotFound, apiError("插件 Page 不存在"))
		return
	}
	pid, activated := s.pluginActivated(pluginID)
	if pid == "" || pid != pluginID {
		writeJSON(w, http.StatusNotFound, apiError("插件不存在"))
		return
	}
	if !activated {
		writeJSON(w, http.StatusForbidden, apiError("插件未启用"))
		return
	}
	root, err := s.pluginPageRoot(pid, page)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, apiError(err.Error()))
		return
	}
	asset := strings.Trim(strings.Join(parts[2:], "/"), "/")
	if asset == "" {
		asset = pluginPageEntryFile
	}
	// Clean("/"+asset) 把任意 ../ 序列钉死在 root 内；再解析符号链接后复核
	// 包含性，防 symlink 逃逸。
	full := filepath.Join(root, filepath.Clean("/"+asset))
	if resolvedRoot, err := filepath.EvalSymlinks(root); err == nil {
		root = resolvedRoot
	}
	resolved, err := filepath.EvalSymlinks(full)
	if err != nil || (resolved != root && !strings.HasPrefix(resolved, root+string(os.PathSeparator))) {
		writeJSON(w, http.StatusNotFound, apiError("插件 Page 资源不存在"))
		return
	}
	info, err := os.Stat(resolved)
	if err != nil || info.IsDir() {
		writeJSON(w, http.StatusNotFound, apiError("插件 Page 资源不存在"))
		return
	}
	if info.Size() > pluginPageMaxAssetSize {
		writeJSON(w, http.StatusRequestEntityTooLarge, apiError("插件 Page 资源过大"))
		return
	}
	data, err := os.ReadFile(resolved)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, apiError("读取插件 Page 资源失败"))
		return
	}
	ct := mime.TypeByExtension(strings.ToLower(filepath.Ext(resolved)))
	if ct == "" {
		ct = "application/octet-stream"
	}
	w.Header().Set("Content-Type", ct)
	w.Header().Set("X-Content-Type-Options", "nosniff")
	if strings.EqualFold(filepath.Ext(resolved), ".html") {
		w.Header().Set("Cache-Control", "no-store")
	}
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(data)
}
