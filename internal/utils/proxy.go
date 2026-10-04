// 全局 HTTP 代理配置。
// 通过修改 http.DefaultTransport，使所有未显式指定 Transport 的 &http.Client{}
// 自动走配置的 http_proxy，同时支持 no_proxy 白名单（直连）。
package utils

import (
	"net/http"
	"net/url"
	"strings"
	"time"

	"golang.org/x/net/http/httpproxy"
)

// ConfigureGlobalProxy 根据配置的 http_proxy 与 no_proxy 配置 http.DefaultTransport，
// 使所有 `&http.Client{}`（Transport=nil 时使用 DefaultTransport）自动走代理。
//
// proxyURL 为空时使用直连（Proxy 为 nil），不跟随系统 HTTP_PROXY/HTTPS_PROXY
// 环境变量——"其它请求遵循前端配置的代理"，配置为空即直连（容器/宿主的环境
// 代理可能是坏代理，如对部分目标返回 EOF）。pip/venv 安装另有独立的代理规则
// （见 pysdk.PipEnv：配置代理为空时才回退系统 https_proxy）。
// no_proxy 中的条目遵循标准格式，例如 "localhost"、"127.0.0.1"、"192.168.*"、
// "*.example.com" 等。
// globalProxyFunc 保存最近一次 ConfigureGlobalProxy 构建的代理函数，供自行构造
// http.Transport 的调用方（如插件 zig cc / 源码下载）复用，保持与全局请求一致的
// 代理策略：仅认 config http_proxy，未配置即直连（不跟随环境变量）。
var globalProxyFunc func(req *http.Request) (*url.URL, error)

// ProxyFunc 返回全局代理函数（源自 config http_proxy；未配置时为 nil=直连）。
// 供未使用 http.DefaultTransport 的自建 Transport 复用同一策略。
func ProxyFunc() func(req *http.Request) (*url.URL, error) {
	return globalProxyFunc
}

func ConfigureGlobalProxy(proxyURL string, noProxy []string) {
	var proxyFunc func(req *http.Request) (*url.URL, error)
	if strings.TrimSpace(proxyURL) != "" {
		// 仅基于传入的配置构建，不调用 httpproxy.FromEnvironment，
		// 因为环境变量的取值/优先级与 AstrBot 的 config 配置并不一致。
		// HTTPProxy 只作用于 http:// 请求；https:// 请求必须显式设
		// HTTPSProxy，否则（如 GitHub raw 市场）仍直连。
		cfg := &httpproxy.Config{
			HTTPProxy:  proxyURL,
			HTTPSProxy: proxyURL,
			NoProxy:    strings.Join(noProxy, ","),
		}
		proxyFunc = func(req *http.Request) (*url.URL, error) {
			return cfg.ProxyFunc()(req.URL)
		}
	}
	// proxyFunc 为 nil 时 Transport.Proxy == nil → 直连。
	globalProxyFunc = proxyFunc

	// 保留与 http.DefaultTransport 相近的合理默认值（连接池、超时等）。
	http.DefaultTransport = &http.Transport{
		Proxy:                 proxyFunc,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          100,
		MaxIdleConnsPerHost:   10,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
	}
}
