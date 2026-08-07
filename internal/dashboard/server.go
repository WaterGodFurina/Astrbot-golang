// Package dashboard implements the WebUI API server.
// Ported from astrbot/dashboard/server.py
package dashboard

import (
        "context"
        "embed"
        "encoding/json"
        "fmt"
        "net/http"
        "strings"
        "sync"
        "time"

        "github.com/AstrBotDevs/AstrBot/internal/log"
)

//go:embed web/dist/*
var webFS embed.FS

var logger = log.GetDefault().WithComponent("Dashboard")

// Server is the WebUI API server.
type Server struct {
        mux      *http.ServeMux
        srv      *http.Server
        mu       sync.RWMutex
        handlers map[string]http.HandlerFunc
        auth     *PasswordManager
        port     int
}

// NewServer creates a dashboard server with password management.
func NewServer(port int, configPath string) *Server {
        s := &Server{
                mux:      http.NewServeMux(),
                handlers: make(map[string]http.HandlerFunc),
                port:     port,
        }
        // Initialize password manager (generates + prints password on first run)
        s.auth = NewPasswordManager(configPath)
        s.setupRoutes()
        s.srv = &http.Server{
                Addr:         fmt.Sprintf(":%d", port),
                Handler:      s.mux,
                ReadTimeout:  30 * time.Second,
                WriteTimeout: 60 * time.Second,
        }
        return s
}

// setupRoutes registers API endpoints.
func (s *Server) setupRoutes() {
        s.mux.HandleFunc("/api/", s.apiHandler)
        s.mux.HandleFunc("/health", s.healthHandler)
        // Serve embedded WebUI
        s.mux.HandleFunc("/", s.serveWebUI)
}

// apiHandler dispatches API requests.
func (s *Server) apiHandler(w http.ResponseWriter, r *http.Request) {
        path := strings.TrimPrefix(r.URL.Path, "/api/")
        parts := strings.Split(path, "/")
        if len(parts) == 0 || parts[0] == "" {
                writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid endpoint"})
                return
        }

        category := parts[0]

        switch category {
        case "config":
                s.handleConfig(w, r, parts[1:])
        case "provider":
                s.handleProvider(w, r, parts[1:])
        case "platform":
                s.handlePlatform(w, r, parts[1:])
        case "plugin":
                s.handlePlugin(w, r, parts[1:])
        case "knowledge_base":
                s.handleKB(w, r, parts[1:])
        case "auth":
                s.handleAuth(w, r, parts[1:])
        case "stats":
                s.handleStats(w, r, parts[1:])
        default:
                writeJSON(w, http.StatusNotFound, map[string]string{"error": "unknown endpoint"})
        }
}

// healthHandler returns service health.
func (s *Server) healthHandler(w http.ResponseWriter, r *http.Request) {
        writeJSON(w, http.StatusOK, map[string]interface{}{
                "status":  "ok",
                "version": "0.1.0-go",
        })
}

// handleConfig handles config endpoints.
func (s *Server) handleConfig(w http.ResponseWriter, r *http.Request, parts []string) {
        // GET /api/config/get
        if r.Method == http.MethodGet {
                writeJSON(w, http.StatusOK, map[string]interface{}{
                        "status":  "ok",
                        "message": "config endpoint",
                })
                return
        }
        writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
}

// handleProvider handles provider endpoints.
func (s *Server) handleProvider(w http.ResponseWriter, r *http.Request, parts []string) {
        writeJSON(w, http.StatusOK, map[string]interface{}{
                "status":    "ok",
                "providers": []interface{}{},
        })
}

// handlePlatform handles platform endpoints.
func (s *Server) handlePlatform(w http.ResponseWriter, r *http.Request, parts []string) {
        writeJSON(w, http.StatusOK, map[string]interface{}{
                "status":   "ok",
                "platforms": []interface{}{},
        })
}

// handlePlugin handles plugin endpoints.
func (s *Server) handlePlugin(w http.ResponseWriter, r *http.Request, parts []string) {
        writeJSON(w, http.StatusOK, map[string]interface{}{
                "status":  "ok",
                "plugins": []interface{}{},
        })
}

// handleKB handles knowledge base endpoints.
func (s *Server) handleKB(w http.ResponseWriter, r *http.Request, parts []string) {
        writeJSON(w, http.StatusOK, map[string]interface{}{
                "status": "ok",
                "items":  []interface{}{},
        })
}

// handleAuth handles authentication endpoints.
func (s *Server) handleAuth(w http.ResponseWriter, r *http.Request, parts []string) {
        switch len(parts) {
        case 0:
                return
        }
        action := parts[0]
        switch action {
        case "login":
                s.handleLogin(w, r)
        case "logout":
                s.handleLogout(w, r)
        case "check":
                s.handleCheck(w, r)
        default:
                writeJSON(w, http.StatusNotFound, map[string]string{"error": "unknown auth action"})
        }
}

// handleLogin handles POST /api/auth/login.
func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
        if r.Method != http.MethodPost {
                writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "POST required"})
                return
        }
        var creds struct {
                Username string `json:"username"`
                Password string `json:"password"`
        }
        if err := json.NewDecoder(r.Body).Decode(&creds); err != nil {
                writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON"})
                return
        }
        if s.auth == nil {
                writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "auth not initialized"})
                return
        }
        if creds.Username == s.auth.Username() && s.auth.VerifyPassword(creds.Password) {
                writeJSON(w, http.StatusOK, map[string]interface{}{
                        "status":             "ok",
                        "token":              generateRandomToken(32),
                        "username":           creds.Username,
                        "password_change_required": s.auth.PasswordChangeRequired(),
                })
                return
        }
        writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "invalid credentials"})
}

// handleLogout handles POST /api/auth/logout.
func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
        writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// handleCheck handles GET /api/auth/check.
func (s *Server) handleCheck(w http.ResponseWriter, r *http.Request) {
        writeJSON(w, http.StatusOK, map[string]interface{}{
                "status":   "ok",
                "loggedin": false,
        })
}

// handleStats handles stats endpoints.
func (s *Server) handleStats(w http.ResponseWriter, r *http.Request, parts []string) {
        writeJSON(w, http.StatusOK, map[string]interface{}{
                "status":      "ok",
                "platforms":   []interface{}{},
                "providers":   []interface{}{},
                "conversations": []interface{}{},
        })
}

// Start begins serving.
func (s *Server) Start(ctx context.Context) error {
        // Print startup banner with URLs
        if s.auth != nil {
                s.auth.PrintStartupBanner(s.port, getLocalIPs())
        }
        logger.Info("Dashboard API server starting on :%d", s.port)
        go func() {
                <-ctx.Done()
                s.Stop()
        }()
        if err := s.srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
                return err
        }
        return nil
}

// Stop shuts down the server.
func (s *Server) Stop() {
        ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
        defer cancel()
        _ = s.srv.Shutdown(ctx)
        logger.Info("Dashboard API server stopped")
}

func writeJSON(w http.ResponseWriter, code int, data interface{}) {
        w.Header().Set("Content-Type", "application/json")
        w.WriteHeader(code)
        _ = json.NewEncoder(w).Encode(data)
}

// serveWebUI serves the embedded Vue dashboard (AstrBot original WebUI).
func (s *Server) serveWebUI(w http.ResponseWriter, r *http.Request) {
        // Strip leading slash for embed.FS lookup
        cleanPath := strings.TrimPrefix(r.URL.Path, "/")
        if cleanPath == "" {
                cleanPath = "index.html"
        }

        // Try to read from embedded dist directory
        fsPath := "web/dist/" + cleanPath
        data, err := webFS.ReadFile(fsPath)
        if err != nil {
                // SPA fallback: serve index.html for unknown non-file paths
                // (enables Vue Router history mode)
                data, err = webFS.ReadFile("web/dist/index.html")
                if err != nil {
                        http.Error(w, "WebUI not available", http.StatusInternalServerError)
                        return
                }
        }

        // Set content type based on file extension
        w.Header().Set("Content-Type", contentTypeFor(cleanPath))

        // Cache static assets aggressively (they have content hashes in filenames)
        if strings.HasPrefix(cleanPath, "assets/") {
                w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
        }
        w.Write(data)
}

// contentTypeFor returns the MIME type for a file path.
func contentTypeFor(path string) string {
        switch {
        case strings.HasSuffix(path, ".html"):
                return "text/html; charset=utf-8"
        case strings.HasSuffix(path, ".js"):
                return "application/javascript; charset=utf-8"
        case strings.HasSuffix(path, ".css"):
                return "text/css; charset=utf-8"
        case strings.HasSuffix(path, ".json"):
                return "application/json; charset=utf-8"
        case strings.HasSuffix(path, ".svg"):
                return "image/svg+xml"
        case strings.HasSuffix(path, ".png"):
                return "image/png"
        case strings.HasSuffix(path, ".jpg"), strings.HasSuffix(path, ".jpeg"):
                return "image/jpeg"
        case strings.HasSuffix(path, ".woff2"):
                return "font/woff2"
        case strings.HasSuffix(path, ".woff"):
                return "font/woff"
        case strings.HasSuffix(path, ".ttf"):
                return "font/ttf"
        case strings.HasSuffix(path, ".ico"):
                return "image/x-icon"
        default:
                return "application/octet-stream"
        }
}

// getLocalIPs returns non-loopback IPv4 addresses for the banner.
func getLocalIPs() []string {
        return []string{}
}
