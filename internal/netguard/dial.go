// Package netguard 提供出站连接的防 SSRF 拨号器：每个主机只解析一次、对解析
// 结果做策略校验、并把连接钉扎到同一个通过校验的 IP。这样"校验时解析到的
// 地址"与"实际建连的地址"一致，关闭 DNS-rebinding TOCTOU 窗口。plugin 的
// 插件下载与 dashboard 的出站请求客户端共用这一实现。
package netguard

import (
	"context"
	"fmt"
	"net"
	"sync"
	"time"
)

// dialTimeout 单次 TCP 建连超时。
const dialTimeout = 30 * time.Second

// PinnedDialContext 返回一个 http.Transport.DialContext：每个主机在首次拨号
// 时解析一次，对每个候选 IP 调用 validate，跳过被拒候选，并把该主机的后续
// 连接钉扎到第一个通过校验的 IP（优先 IPv4——钉扎后失去拨号器的多地址回退，
// 固定 IPv4 更稳）。validate 为 nil 时只做钉扎、不做阻断。所有候选都被拒绝
// （或解析失败）时报错，请求不会以未校验的地址建连。
//
// URL 与 Host header / TLS SNI 保持原主机名不变，只有真正拨号的 IP 被替换。
func PinnedDialContext(validate func(net.IP) error) func(ctx context.Context, network, addr string) (net.Conn, error) {
	var mu sync.Mutex
	pinned := map[string]net.IP{}
	dialer := &net.Dialer{Timeout: dialTimeout}
	return func(ctx context.Context, network, addr string) (net.Conn, error) {
		host, port, err := net.SplitHostPort(addr)
		if err != nil {
			return nil, err
		}
		mu.Lock()
		ip, ok := pinned[host]
		mu.Unlock()
		if !ok {
			resolved, err := resolveAllowed(host, validate)
			if err != nil {
				return nil, err
			}
			mu.Lock()
			if prev, ok := pinned[host]; ok {
				resolved = prev // 并发拨号竞争：使用先写入的钉扎结果
			} else {
				pinned[host] = resolved
			}
			mu.Unlock()
			ip = resolved
		}
		return dialer.DialContext(ctx, network, net.JoinHostPort(ip.String(), port))
	}
}

// resolveAllowed 解析主机并返回第一个通过 validate 的 IP（优先 IPv4）。
func resolveAllowed(host string, validate func(net.IP) error) (net.IP, error) {
	ips, err := net.LookupIP(host)
	if err != nil {
		return nil, fmt.Errorf("resolve %s: %w", host, err)
	}
	var firstAllowed net.IP
	for _, cand := range ips {
		if validate != nil && validate(cand) != nil {
			continue
		}
		if cand.To4() != nil {
			return cand, nil
		}
		if firstAllowed == nil {
			firstAllowed = cand
		}
	}
	if firstAllowed == nil {
		return nil, fmt.Errorf("目标地址 %s 未通过出站校验", host)
	}
	return firstAllowed, nil
}
