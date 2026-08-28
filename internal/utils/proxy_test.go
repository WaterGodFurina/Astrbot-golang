package utils

import (
	"net/http"
	"testing"
)

// TestConfigureGlobalProxyEmptyIsDirect: config http_proxy 为空时 DefaultTransport
// 必须直连（Proxy == nil），不跟随系统 HTTP_PROXY/HTTPS_PROXY 环境变量。
func TestConfigureGlobalProxyEmptyIsDirect(t *testing.T) {
	t.Setenv("HTTP_PROXY", "http://127.0.0.1:9")
	t.Setenv("HTTPS_PROXY", "http://127.0.0.1:9")
	ConfigureGlobalProxy("", nil)
	tr, ok := http.DefaultTransport.(*http.Transport)
	if !ok {
		t.Fatalf("DefaultTransport = %T, want *http.Transport", http.DefaultTransport)
	}
	if tr.Proxy != nil {
		t.Fatal("config proxy empty: Proxy must be nil (direct)")
	}
}

// TestConfigureGlobalProxySet: config http_proxy 非空时 DefaultTransport 使用
// 该代理。
func TestConfigureGlobalProxySet(t *testing.T) {
	ConfigureGlobalProxy("http://192.168.3.9:7900", []string{"localhost"})
	tr, ok := http.DefaultTransport.(*http.Transport)
	if !ok {
		t.Fatalf("DefaultTransport = %T, want *http.Transport", http.DefaultTransport)
	}
	if tr.Proxy == nil {
		t.Fatal("config proxy set: Proxy must not be nil")
	}
}
