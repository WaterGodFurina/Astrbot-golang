// Package dashboard - file token public file service.
// 对齐 Python 本体 /api/file/{token}：插件经 HostService RegisterFileToken
// 反调用把宿主侧文件登记为随机 uuid4 令牌，下游（sandbox runtime / 消息链
// 里的本地媒体引用等）凭 token 经本公开路由流式读取文件，避免暴露真实路径。
package dashboard

import (
	"errors"
	"net/http"
	"os"

	"github.com/WaterGodFurina/Astrbot-golang/internal/plugin"
)

// handleFileToken serves GET /api/file/{token}.
//
// 鉴权模型：token 本身即凭据（随机 uuid4 不可枚举，且带 TTL）——路由已在
// apiAuthAllowed 放行带 token 段的请求，这里只做注册表校验：
//   - 未登记/已清理 → 404（不区分"从未存在"，避免探测注册表内容）；
//   - 已过期 → 410 Gone（消费方据此知道令牌曾有效但已失效）；
//   - 路径来自注册表（登记时已 EvalSymlinks 解析并校验为普通文件），不存在
//     用户可控路径，天然防目录遍历；发送前仍复核文件存在且为普通文件。
//
// 发送用 http.ServeFile：按扩展名嗅探 Content-Type、支持 Range/If-Modified
// 流式发送，不整读进内存。
func (s *Server) handleFileToken(w http.ResponseWriter, r *http.Request, token string) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		writeJSON(w, http.StatusMethodNotAllowed, apiError("GET required"))
		return
	}
	if token == "" || s.fileTokens == nil {
		writeJSON(w, http.StatusNotFound, apiError("file token not found"))
		return
	}
	path, err := s.fileTokens.Lookup(token)
	if err != nil {
		if errors.Is(err, plugin.ErrFileTokenExpired) {
			writeJSON(w, http.StatusGone, apiError("file token expired"))
			return
		}
		writeJSON(w, http.StatusNotFound, apiError("file token not found"))
		return
	}
	info, err := os.Stat(path)
	if err != nil || !info.Mode().IsRegular() {
		// 登记后文件被删除/替换为目录：按未找到处理。
		writeJSON(w, http.StatusNotFound, apiError("file token not found"))
		return
	}
	// #nosec G304 -- path 来自文件令牌注册表（RegisterFileToken 反调用登记，
	// 登记时已 EvalSymlinks+普通文件校验），非请求方可控输入；token 不可枚举。
	http.ServeFile(w, r, path)
}
