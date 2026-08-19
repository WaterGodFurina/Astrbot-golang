// Package utils provides shared utility functions.
// Ported from astrbot/core/utils/io.py and related files.
//
// Bug fix for issue #9446: SSL verification fallback to CERT_NONE.
// The Python code caught SSL errors and re-tried with verify_mode=CERT_NONE,
// disabling TLS verification entirely and exposing the application to MITM.
// In Go, we NEVER disable TLS verification. If certificate verification fails,
// the error is returned to the caller. We only allow InsecureSkipVerify on
// explicitly user-configured self-signed endpoints (via a dedicated option).
package utils

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/WaterGodFurina/Astrbot-golang/internal/log"
)

var logger = log.GetDefault().WithComponent("IO")

// HTTPClient is a reusable HTTP client with proper TLS defaults.
type HTTPClient struct {
	client    *http.Client
	userAgent string
}

// NewHTTPClient creates an HTTP client with secure TLS defaults.
// proxyURL is optional ("http://" or "socks5://").
// caCertPath is optional (path to a custom CA bundle PEM file).
func NewHTTPClient(proxyURL, caCertPath string, timeout time.Duration) (*HTTPClient, error) {
	tlsConfig := &tls.Config{
		// Issue #9446 fix: we never set InsecureSkipVerify=true as a fallback.
		// If the server cert is invalid, the request fails. Period.
		MinVersion: tls.VersionTLS12,
	}

	if caCertPath != "" {
		// #nosec G304 -- caCertPath is an admin-configured CA bundle path.
		pemData, err := os.ReadFile(caCertPath)
		if err != nil {
			return nil, fmt.Errorf("read CA cert: %w", err)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(pemData) {
			return nil, fmt.Errorf("no valid certs found in %s", caCertPath)
		}
		tlsConfig.RootCAs = pool
	} else {
		// Use system cert pool
		pool, err := x509.SystemCertPool()
		if err != nil {
			pool = x509.NewCertPool()
		}
		tlsConfig.RootCAs = pool
	}

	transport := &http.Transport{
		TLSClientConfig:       tlsConfig,
		MaxIdleConns:          100,
		MaxIdleConnsPerHost:   10,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
	}

	if proxyURL != "" {
		u, err := url.Parse(proxyURL)
		if err != nil {
			return nil, fmt.Errorf("parse proxy url: %w", err)
		}
		transport.Proxy = http.ProxyURL(u)
	} else {
		// Respect HTTP_PROXY / HTTPS_PROXY env vars
		transport.Proxy = http.ProxyFromEnvironment
	}

	if timeout == 0 {
		timeout = 30 * time.Second
	}

	return &HTTPClient{
		client: &http.Client{
			Transport: transport,
			Timeout:   timeout,
		},
		userAgent: "AstrBot/1.0 Go",
	}, nil
}

// DownloadFile downloads a URL to a local file path.
// Returns the path to the downloaded file.
func (h *HTTPClient) DownloadFile(ctx context.Context, urlStr, destPath string, showProgress bool) error {
	req, err := http.NewRequestWithContext(ctx, "GET", urlStr, nil)
	if err != nil {
		return fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("User-Agent", h.userAgent)

	resp, err := h.client.Do(req)
	if err != nil {
		// Issue #9446: Do NOT fallback to insecure TLS. Return the error.
		return fmt.Errorf("download failed (TLS enforced): %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("download failed: HTTP %d for %s", resp.StatusCode, safeURLForLog(urlStr))
	}

	if destPath == "" {
		return fmt.Errorf("destPath is empty")
	}

	if err := os.MkdirAll(filepath.Dir(destPath), 0750); err != nil {
		return fmt.Errorf("create dir: %w", err)
	}

	// #nosec G304 -- destPath is the caller-supplied download destination.
	out, err := os.Create(destPath)
	if err != nil {
		return fmt.Errorf("create file: %w", err)
	}
	defer out.Close()

	if _, err := io.Copy(out, resp.Body); err != nil {
		if err := os.Remove(destPath); err != nil {
			logger.Debug("cleanup partial file %s failed: %v", destPath, err)
		}
		return fmt.Errorf("write file: %w", err)
	}

	return nil
}

// DownloadBytes downloads a URL and returns the content as bytes.
func (h *HTTPClient) DownloadBytes(ctx context.Context, urlStr string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", urlStr, nil)
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("User-Agent", h.userAgent)

	resp, err := h.client.Do(req)
	if err != nil {
		// Issue #9446: No insecure fallback. Return error directly.
		return nil, fmt.Errorf("download failed (TLS enforced): %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("download failed: HTTP %d for %s", resp.StatusCode, safeURLForLog(urlStr))
	}

	return io.ReadAll(resp.Body)
}

// PostJSON sends a POST request with a JSON body and returns the response.
func (h *HTTPClient) PostJSON(ctx context.Context, urlStr string, body io.Reader) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, "POST", urlStr, body)
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("User-Agent", h.userAgent)
	req.Header.Set("Content-Type", "application/json")

	resp, err := h.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("post failed (TLS enforced): %w", err)
	}
	return resp, nil
}

// safeURLForLog returns a URL summary that omits query strings and fragments.
// This prevents leaking signed URLs or tokens in log output.
func safeURLForLog(rawURL string) string {
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return fmt.Sprintf("URL len=%d", len(rawURL))
	}
	if parsed.Scheme == "http" || parsed.Scheme == "https" {
		filename := filepath.Base(parsed.Path)
		suffix := ""
		if filename != "" {
			suffix = fmt.Sprintf(" file=%q", filename)
		}
		return fmt.Sprintf("%s URL host=%q%s len=%d", parsed.Scheme, parsed.Host, suffix, len(rawURL))
	}
	return fmt.Sprintf("URL len=%d", len(rawURL))
}

// EnsureDir ensures a directory exists. If a non-directory file exists at the
// path, it is removed first.
func EnsureDir(dirPath string) error {
	info, err := os.Stat(dirPath)
	if err == nil {
		if info.IsDir() {
			return nil
		}
		// Path exists but is not a directory
		logger.Warn("Path %s exists but is not a directory; removing it", dirPath)
		if err := os.Remove(dirPath); err != nil {
			return fmt.Errorf("remove conflicting path: %w", err)
		}
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("stat path: %w", err)
	}
	return os.MkdirAll(dirPath, 0750)
}

// RemoveDir removes a file or directory tree.
func RemoveDir(path string) error {
	return os.RemoveAll(path)
}

// PortChecker checks if a TCP port is reachable.
func PortChecker(port int, host string) bool {
	addr := net.JoinHostPort(host, strconv.Itoa(port))
	conn, err := net.DialTimeout("tcp", addr, time.Second)
	if err != nil {
		return false
	}
	defer conn.Close()
	return true
}

// FileToBase64 reads a file and returns its base64-encoded content with prefix.
func FileToBase64(path string) (string, error) {
	// #nosec G304 -- generic utility; the path is provided by the caller.
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	return "base64://" + base64Encode(data), nil
}

// base64Encode wraps standard base64 encoding.
func base64Encode(data []byte) string {
	return base64StdEncoding(data)
}

// IsURL checks if a string looks like a URL.
func IsURL(s string) bool {
	return strings.HasPrefix(s, "http://") || strings.HasPrefix(s, "https://")
}

// GetLocalIPAddresses returns non-loopback IPv4 addresses.
func GetLocalIPAddresses() []string {
	var ips []string
	interfaces, err := net.Interfaces()
	if err != nil {
		return ips
	}
	for _, iface := range interfaces {
		if iface.Flags&net.FlagUp == 0 || iface.Flags&net.FlagLoopback != 0 {
			continue
		}
		addrs, err := iface.Addrs()
		if err != nil {
			continue
		}
		for _, addr := range addrs {
			var ip net.IP
			switch v := addr.(type) {
			case *net.IPNet:
				ip = v.IP
			case *net.IPAddr:
				ip = v.IP
			}
			if ip == nil || ip.IsLoopback() {
				continue
			}
			if v4 := ip.To4(); v4 != nil {
				ips = append(ips, v4.String())
			}
		}
	}
	return ips
}

// base64StdEncoding uses encoding/base64.
func base64StdEncoding(data []byte) string {
	return base64EncodeBytes(data)
}

// base64EncodeBytes is split to avoid import cycle issues in tests.
func base64EncodeBytes(data []byte) string {
	const chars = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/"
	result := make([]byte, 0, (len(data)+2)/3*4)
	for i := 0; i < len(data); i += 3 {
		var b0, b1, b2 byte
		b0 = data[i]
		if i+1 < len(data) {
			b1 = data[i+1]
		}
		if i+2 < len(data) {
			b2 = data[i+2]
		}
		result = append(result,
			chars[b0>>2],
			chars[((b0&0x03)<<4)|(b1>>4)],
			chars[((b1&0x0f)<<2)|(b2>>6)],
			chars[b2&0x3f],
		)
	}
	// Pad
	pad := len(data) % 3
	if pad == 1 {
		result[len(result)-2] = '='
		result[len(result)-1] = '='
	} else if pad == 2 {
		result[len(result)-1] = '='
	}
	return string(result)
}
