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
// proxyURL 为空时使用直连（Proxy 为 nil）；no_proxy 中的条目遵循标准格式，
// 例如 "localhost"、"127.0.0.1"、"192.168.*"、"*.example.com" 等。
func ConfigureGlobalProxy(proxyURL string, noProxy []string) {
	// 未配置时保持环境变量代理（http.ProxyFromEnvironment，对齐 Python 的
	// aiohttp trust_env=True：市场拉取等出站请求跟随 HTTP_PROXY/HTTPS_PROXY
	// 环境变量），不强制直连。
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
	} else {
		proxyFunc = http.ProxyFromEnvironment
	}

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
