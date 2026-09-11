// Package main is the AstrBot Go entry point.
// Ported from main.py
package main

import (
	"context"
	"flag"
	"fmt"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/WaterGodFurina/Astrbot-golang/internal/lifecycle"
	"github.com/WaterGodFurina/Astrbot-golang/internal/log"
	"github.com/WaterGodFurina/Astrbot-golang/internal/version"
)

const logo = `
    ___                    __        ____        _       _
   /   |  __  ______  ____/ /____ _ / __ \____ _(_)___  (_)___  ____
  / /| | / / / / __ \/ __  / ___ // /_/ / __  / / __ \/ / __ \/ __ \
 / ___ |/ /_/ / / / / /_/ (__  )/ _, _/ /_/ / / / / / / /_/ / / / /
/_/  |_|\__,_/_/ /_/\__,_/____/_/ |_|\__,_/_/_/ /_/_/ /_/_/ /_/_/ /
`

func init() {
	applyRuntimeMemoryTuning()
	// Android 没有 /etc/resolv.conf（/etc 是只读 system 分区），Go 纯 resolver
	// （CGO_ENABLED=0）读不到配置会回退到 [::1]:53；而 Android 的 DNS 由 netd 通过
	// /dev/socket/dnsproxyd（Bionic getaddrinfo）提供，netd 并不监听 UDP 53，导致
	// 所有域名解析报 "connection refused"。Termux 的 resolv-conf 包把真实 DNS 写在
	// $PREFIX/etc/resolv.conf，这里改读该文件并直连其中的 nameserver；读不到时
	// 回退公共 DNS。
	if runtime.GOOS == "android" {
		servers, fallback := androidDNSServers()
		if fallback {
			log.GetDefault().Warn("[DNS] Android 未找到 resolv.conf（Termux 需安装 resolv-conf 包），已回退公共 DNS 8.8.8.8/1.1.1.1；若仍无法解析请安装：pkg install resolv-conf")
		}
		var idx atomic.Uint32
		net.DefaultResolver = &net.Resolver{
			PreferGo: true,
			Dial: func(ctx context.Context, network, address string) (net.Conn, error) {
				i := idx.Add(1)
				srv := servers[(i-1)%uint32(len(servers))]
				d := net.Dialer{Timeout: 5 * time.Second}
				return d.DialContext(ctx, network, srv)
			},
		}
	}
}

// applyRuntimeMemoryTuning 调低 Go 运行时的内存水位（内存占用优化）：
//   - GC 目标从默认 100% 降到 50%：堆增长峰值减半，RSS 显著下降，
//     代价是 GC 频率升高（小幅 CPU 开销）。
//   - 软内存上限默认 128MiB（GOMEMLIMIT 语义）：接近上限时 GC 加压回收，
//     防止低内存设备（Android/Termux）OOM；平时远未触及时无感知。
//
// 环境变量覆盖：ASTRBOT_GOGC（GC 百分比）、ASTRBOT_GOMEMLIMIT
// （字节数或带 KiB/MiB/GiB 后缀；设 0 关闭软限）。
func applyRuntimeMemoryTuning() {
	if v := strings.TrimSpace(os.Getenv("ASTRBOT_GOGC")); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			debug.SetGCPercent(n)
		}
	} else {
		debug.SetGCPercent(50)
	}
	limitEnv := strings.TrimSpace(os.Getenv("ASTRBOT_GOMEMLIMIT"))
	var limit int64 = 128 << 20 // 默认 128MiB
	if limitEnv != "" {
		if n, err := parseMemorySize(limitEnv); err == nil {
			limit = n
		}
	}
	if limit > 0 {
		debug.SetMemoryLimit(limit)
	}
}

// parseMemorySize 解析内存大小：纯字节数（"268435456"）或带后缀
// （"128MiB"/"512KB"/"1GiB"，大小写不敏感，支持 B/KB/MB/GB/KiB/MiB/GiB）。
func parseMemorySize(s string) (int64, error) {
	s = strings.TrimSpace(s)
	if n, err := strconv.ParseInt(s, 10, 64); err == nil {
		return n, nil
	}
	upper := strings.ToUpper(s)
	units := []struct {
		suffix string
		mult   int64
	}{
		{"KIB", 1 << 10}, {"MIB", 1 << 20}, {"GIB", 1 << 30},
		{"KB", 1 << 10}, {"MB", 1 << 20}, {"GB", 1 << 30}, {"B", 1},
	}
	for _, u := range units {
		if strings.HasSuffix(upper, u.suffix) {
			num := strings.TrimSpace(strings.TrimSuffix(upper, u.suffix))
			if n, err := strconv.ParseInt(num, 10, 64); err == nil && n > 0 {
				return n * u.mult, nil
			}
			return 0, fmt.Errorf("无效内存大小 %q", s)
		}
	}
	return 0, fmt.Errorf("无效内存大小 %q", s)
}

// androidDNSServers 返回 Android（Termux）下可用的 DNS 服务器。
// 依次尝试 $PREFIX/etc/resolv.conf（Termux resolv-conf 包）、常见 Termux 路径、
// /etc/resolv.conf（proot/termux-chroot 绑定后存在），最后回退公共 DNS。
// 第二返回值标记是否发生了回退（未读到任何 resolv.conf）。
func androidDNSServers() ([]string, bool) {
	var candidates []string
	if p := os.Getenv("PREFIX"); p != "" {
		candidates = append(candidates, filepath.Join(p, "etc", "resolv.conf"))
	}
	candidates = append(candidates,
		"/data/data/com.termux/files/usr/etc/resolv.conf",
		"/etc/resolv.conf",
	)
	var servers []string
	for _, path := range candidates {
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		for _, line := range strings.Split(string(data), "\n") {
			line = strings.TrimSpace(line)
			if !strings.HasPrefix(line, "nameserver") {
				continue
			}
			fields := strings.Fields(line)
			if len(fields) >= 2 && net.ParseIP(fields[1]) != nil {
				servers = append(servers, net.JoinHostPort(fields[1], "53"))
			}
		}
		if len(servers) > 0 {
			return servers, false
		}
	}
	return []string{"8.8.8.8:53", "1.1.1.1:53"}, true
}

func main() {
	webuiDir := flag.String("webui-dir", "", "Directory path for external WebUI static files (optional; empty = use the embedded dist)")
	resetPassword := flag.Bool("reset-password", false, "Reset the dashboard initial password on startup")
	flag.Parse()

	// Initialize logging
	logLevel := os.Getenv("ASTRBOT_LOG_LEVEL")
	if logLevel == "" {
		logLevel = "INFO"
	}
	log.GetDefault().SetLevel(log.ParseLevel(logLevel))

	fmt.Print(logo)
	fmt.Printf("AstrBot Go - %s (port from Python v%s)\n", version.Version, version.PythonVersion)
	fmt.Println()

	if *resetPassword {
		os.Setenv("ASTRBOT_RESET_DASHBOARD_PASSWORD", "1")
	}

	// Ensure data directory exists
	if err := os.MkdirAll("data", 0755); err != nil {
		fmt.Fprintf(os.Stderr, "Failed to create data directory: %v\n", err)
		os.Exit(1)
	}

	// 清理 Windows 自升级残留：两步改名后留下的 <exe>.old 文件。
	if runtime.GOOS == "windows" {
		if exe, err := os.Executable(); err == nil {
			_ = os.Remove(exe + ".old")
		}
	}

	// Create lifecycle
	lc := lifecycle.New()
	if *webuiDir != "" {
		lc.SetWebUIDir(*webuiDir)
	}

	// Start
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if err := lc.Start(ctx); err != nil {
		fmt.Fprintf(os.Stderr, "Failed to start: %v\n", err)
		os.Exit(1)
	}

	// Wait for shutdown signal
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	sig := <-sigCh
	fmt.Printf("\nReceived signal %v, shutting down...\n", sig)
	lc.Stop()
}
