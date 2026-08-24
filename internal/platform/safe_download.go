// 发送链路受控下载器：供各平台适配器把"URL → 内容"的裸下载替换为
// 带 SSRF 校验（仅 http(s)、拒绝内网/环回/链路本地/保留地址段）与
// 大小上限的受控下载，防止出站消息中的任意 URL 旁路 mediaHostAllowed 防线。
package platform

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"time"
)

// safeDownloadTimeout 限制发送链路对出站媒体 URL 的单次下载时长。
const safeDownloadTimeout = 30 * time.Second

// maxDownloadRedirects 限制下载重定向次数，防止 SSRF 通过跳转逃逸主机校验。
const maxDownloadRedirects = 10

// blockedDownloadPrefixes 是禁止下载的内网/环回/链路本地/保留地址段
// （含云元数据 169.254.169.254），防止服务端请求伪造（SSRF）。
var blockedDownloadPrefixes = []netip.Prefix{
	netip.MustParsePrefix("127.0.0.0/8"),
	netip.MustParsePrefix("::1/128"),
	netip.MustParsePrefix("10.0.0.0/8"),
	netip.MustParsePrefix("172.16.0.0/12"),
	netip.MustParsePrefix("192.168.0.0/16"),
	netip.MustParsePrefix("169.254.0.0/16"),
	netip.MustParsePrefix("fe80::/10"),
	netip.MustParsePrefix("100.64.0.0/10"),
}

// safeDownloadClient 构造带超时与逐跳重定向主机校验的下载客户端。
func safeDownloadClient() *http.Client {
	return &http.Client{
		Timeout: safeDownloadTimeout,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= maxDownloadRedirects {
				return fmt.Errorf("下载重定向次数过多")
			}
			if req.URL == nil {
				return fmt.Errorf("下载重定向目标缺少 URL")
			}
			if req.URL.Scheme != "http" && req.URL.Scheme != "https" {
				return fmt.Errorf("下载重定向目标必须为 http(s)")
			}
			return validateDownloadHost(req.URL.Hostname())
		},
	}
}

// validateDownloadURL 校验下载目标：仅允许 http(s)，且主机不得为内网/保留地址。
func validateDownloadURL(rawURL string) error {
	u, err := url.Parse(rawURL)
	if err != nil {
		return fmt.Errorf("无法解析下载 URL: %v", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("下载 URL 必须为 http(s): %s", rawURL)
	}
	return validateDownloadHost(u.Hostname())
}

// validateDownloadHost 解析主机并拒绝环回/私网/链路本地（含云元数据）等非公网地址。
func validateDownloadHost(host string) error {
	if host == "" {
		return fmt.Errorf("下载 URL 缺少主机名")
	}
	ips, err := net.LookupIP(host)
	if err != nil {
		return fmt.Errorf("下载主机解析失败 %q: %v", host, err)
	}
	if len(ips) == 0 {
		return fmt.Errorf("下载主机 %q 无解析结果", host)
	}
	for _, ip := range ips {
		addr, ok := netip.AddrFromSlice(ip)
		if !ok {
			continue
		}
		addr = addr.Unmap()
		for _, p := range blockedDownloadPrefixes {
			if p.Contains(addr) {
				return fmt.Errorf("拒绝下载内网/保留地址 %s", addr)
			}
		}
	}
	return nil
}

// SafeDownloadBytes 下载 URL 内容：仅允许 http(s)、主机不得为内网/保留地址、
// 逐跳复验重定向，读取大小限制为 maxBytes（超过即报错）。
func SafeDownloadBytes(ctx context.Context, rawURL string, maxBytes int64) ([]byte, error) {
	if err := validateDownloadURL(rawURL); err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, err
	}
	resp, err := safeDownloadClient().Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("下载失败: HTTP %d", resp.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxBytes+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > maxBytes {
		return nil, fmt.Errorf("下载内容超过大小上限 %d 字节", maxBytes)
	}
	return data, nil
}
