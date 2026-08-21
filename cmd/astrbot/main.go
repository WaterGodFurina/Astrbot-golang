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
	"runtime"
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
	// Android 没有 /etc/resolv.conf，Go 纯 resolver（CGO_ENABLED=0）会回退到
	// [::1]:53，而 Android 的 DNS 由 netd 提供、只监听 127.0.0.1:53（IPv4），
	// 导致所有域名解析报 "connection refused"。统一把解析指向 netd。
	if runtime.GOOS == "android" {
		net.DefaultResolver = &net.Resolver{
			PreferGo: true,
			Dial: func(ctx context.Context, network, address string) (net.Conn, error) {
				d := net.Dialer{Timeout: 5 * time.Second}
				return d.DialContext(ctx, "udp", "127.0.0.1:53")
			},
		}
	}
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
