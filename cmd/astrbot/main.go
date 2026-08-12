// Package main is the AstrBot Go entry point.
// Ported from main.py
package main

import (
        "context"
        "flag"
        "fmt"
        "os"
        "os/signal"
        "syscall"

        "github.com/WaterGodFurina/Astrbot-golang/internal/lifecycle"
        "github.com/WaterGodFurina/Astrbot-golang/internal/log"
)

const logo = `
    ___                    __        ____        _       _
   /   |  __  ______  ____/ /____ _ / __ \____ _(_)___  (_)___  ____
  / /| | / / / / __ \/ __  / ___ // /_/ / __  / / __ \/ / __ \/ __ \
 / ___ |/ /_/ / / / / /_/ (__  )/ _, _/ /_/ / / / / / / /_/ / / / /
/_/  |_|\__,_/_/ /_/\__,_/____/_/ |_|\__,_/_/_/ /_/_/ /_/_/ /_/_/ /
`

func main() {
	webuiDir := flag.String("webui-dir", "webui", "Directory path for WebUI static files (default: ./webui, falls back to embedded dist)")
        resetPassword := flag.Bool("reset-password", false, "Reset the dashboard initial password on startup")
        flag.Parse()

        // Initialize logging
        logLevel := os.Getenv("ASTRBOT_LOG_LEVEL")
        if logLevel == "" {
                logLevel = "INFO"
        }
        log.GetDefault().SetLevel(log.ParseLevel(logLevel))

        fmt.Print(logo)
        fmt.Println("AstrBot Go - v0.1.0 (port from Python v4.27.2)")
        fmt.Println()

        if *resetPassword {
                os.Setenv("ASTRBOT_RESET_DASHBOARD_PASSWORD", "1")
        }

        // Ensure data directory exists
        if err := os.MkdirAll("data", 0755); err != nil {
                fmt.Fprintf(os.Stderr, "Failed to create data directory: %v\n", err)
                os.Exit(1)
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
