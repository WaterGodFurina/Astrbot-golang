package dashboard

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	neturl "net/url"
	"regexp"
	"strings"
	"time"
)

// pipUserAgent mimics pip (some mirrors reject non-pip user agents), aligned
// with py stat_service _PIP_USER_AGENT.
const pipUserAgent = "pip/25.0"

// handlePypiIndexTest checks whether a PyPI index (mirror) answers requests.
// Only response headers are awaited (the index page can be large).
// Ported from py stat_service.test_pypi_index_connection (4.29.4).
func (s *Server) handlePypiIndexTest(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, apiError("仅支持 POST"))
		return
	}
	indexURL, err := readIndexURL(r)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, apiError(err.Error()))
		return
	}
	testURL := strings.TrimRight(indexURL, "/") + "/"
	if err := validateOutboundURLStrict(testURL); err != nil {
		writeJSON(w, http.StatusBadRequest, apiError("index_url 校验失败: "+err.Error()))
		return
	}
	req, err := http.NewRequest(http.MethodGet, testURL, nil)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, apiError("请求构造失败: "+err.Error()))
		return
	}
	req.Header.Set("User-Agent", pipUserAgent)
	client := newOutboundClientStrict(10 * time.Second)
	start := time.Now()
	resp, err := client.Do(req)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, apiError("请求失败: "+err.Error()))
		return
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	if resp.StatusCode != http.StatusOK {
		writeJSON(w, http.StatusBadGateway, apiError(fmt.Sprintf("Failed. Status code: %d", resp.StatusCode)))
		return
	}
	latency := float64(time.Since(start).Microseconds()) / 1000.0
	writeJSON(w, http.StatusOK, apiOK(map[string]interface{}{
		"latency": math.Round(latency*100) / 100,
	}))
}

var wheelHrefRe = regexp.MustCompile(`href="([^"]+\.whl)"`)

// handlePypiIndexSpeed downloads a real wheel from the index and reports the
// throughput (aligned with py stat_service.measure_pypi_index_speed). Sampling
// is bounded to 8 MiB so slow mirrors still answer promptly.
func (s *Server) handlePypiIndexSpeed(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, apiError("仅支持 POST"))
		return
	}
	indexURL, err := readIndexURL(r)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, apiError(err.Error()))
		return
	}
	base := strings.TrimRight(indexURL, "/")
	pageURL := base + "/pip/"
	if err := validateOutboundURLStrict(pageURL); err != nil {
		writeJSON(w, http.StatusBadRequest, apiError("index_url 校验失败: "+err.Error()))
		return
	}
	client := newOutboundClientStrict(15 * time.Second)

	pageReq, err := http.NewRequest(http.MethodGet, pageURL, nil)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, apiError("请求构造失败: "+err.Error()))
		return
	}
	pageReq.Header.Set("User-Agent", pipUserAgent)
	pageResp, err := client.Do(pageReq)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, apiError("请求失败: "+err.Error()))
		return
	}
	pageBytes, _ := io.ReadAll(io.LimitReader(pageResp.Body, 4<<20))
	pageResp.Body.Close()
	if pageResp.StatusCode != http.StatusOK {
		writeJSON(w, http.StatusBadGateway, apiError(fmt.Sprintf("Failed. Status code: %d", pageResp.StatusCode)))
		return
	}
	match := wheelHrefRe.FindSubmatch(pageBytes)
	if match == nil {
		writeJSON(w, http.StatusBadGateway, apiError("未在索引页找到可下载的 wheel"))
		return
	}
	wheelURL := resolveURL(pageURL, string(match[1]))
	if err := validateOutboundURLStrict(wheelURL); err != nil {
		writeJSON(w, http.StatusBadRequest, apiError("wheel URL 校验失败: "+err.Error()))
		return
	}
	wheelReq, err := http.NewRequest(http.MethodGet, wheelURL, nil)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, apiError("请求构造失败: "+err.Error()))
		return
	}
	wheelReq.Header.Set("User-Agent", pipUserAgent)
	start := time.Now()
	wheelResp, err := client.Do(wheelReq)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, apiError("下载失败: "+err.Error()))
		return
	}
	defer wheelResp.Body.Close()
	sampled, _ := io.Copy(io.Discard, io.LimitReader(wheelResp.Body, 8<<20))
	elapsed := time.Since(start).Seconds()
	if elapsed <= 0 {
		elapsed = 0.001
	}
	mbps := (float64(sampled) / 1e6) / elapsed
	writeJSON(w, http.StatusOK, apiOK(map[string]interface{}{
		"speed":           math.Round(mbps*100) / 100,
		"sampled_bytes":   sampled,
		"elapsed_seconds": math.Round(elapsed*100) / 100,
	}))
}

// githubSpeedTarget is the repository asset used for ghproxy throughput
// measurement (aligned with py stat_service _GITHUB_SPEED_TARGET_URL).
const githubSpeedTarget = "https://github.com/AstrBotDevs/AstrBot/raw/refs/heads/master/astrbot/core/utils/t2i/template/shiki_runtime.iife.js"

const (
	speedSampleSeconds  = 6
	speedSampleMaxBytes = 4 << 20
)

// handleGhproxySpeed downloads a repository file through a ghproxy and reports
// throughput (aligned with py stat_service.measure_ghproxy_speed). Sampling is
// bounded to 4 MiB or 6 seconds.
func (s *Server) handleGhproxySpeed(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, apiError("仅支持 POST"))
		return
	}
	proxyURL := strings.TrimSpace(r.URL.Query().Get("proxy_url"))
	if proxyURL == "" {
		var body struct {
			ProxyURL string `json:"proxy_url"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			writeJSON(w, http.StatusBadRequest, apiError("无效的 JSON: "+err.Error()))
			return
		}
		proxyURL = strings.TrimSpace(body.ProxyURL)
	}
	testURL := githubSpeedTarget
	if proxyURL != "" {
		testURL = strings.TrimRight(proxyURL, "/") + "/" + githubSpeedTarget
	}
	if err := validateOutboundURLStrict(testURL); err != nil {
		writeJSON(w, http.StatusBadRequest, apiError("proxy_url 校验失败: "+err.Error()))
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), speedSampleSeconds*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, testURL, nil)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, apiError("请求构造失败: "+err.Error()))
		return
	}
	client := newOutboundClientStrict(time.Duration(speedSampleSeconds+5) * time.Second)
	start := time.Now()
	resp, err := client.Do(req)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, apiError("ghproxy 测速失败: "+err.Error()))
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		writeJSON(w, http.StatusBadGateway, apiError(fmt.Sprintf("ghproxy 测速失败: HTTP %d", resp.StatusCode)))
		return
	}
	buf := make([]byte, 64*1024)
	var received int64
	for received < speedSampleMaxBytes {
		n, readErr := resp.Body.Read(buf)
		received += int64(n)
		if readErr != nil {
			break
		}
		if time.Since(start) > speedSampleSeconds*time.Second {
			break
		}
	}
	duration := time.Since(start).Seconds()
	if received == 0 || duration <= 0 {
		writeJSON(w, http.StatusBadGateway, apiError("No data received"))
		return
	}
	writeJSON(w, http.StatusOK, apiOK(map[string]interface{}{
		"speed":    math.Round((float64(received)/1048576/duration)*100) / 100,
		"bytes":    received,
		"duration": math.Round(duration*100) / 100,
	}))
}

func readIndexURL(r *http.Request) (string, error) {
	indexURL := strings.TrimSpace(r.URL.Query().Get("index_url"))
	if indexURL == "" {
		var body struct {
			IndexURL string `json:"index_url"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		indexURL = strings.TrimSpace(body.IndexURL)
	}
	if indexURL == "" {
		return "", fmt.Errorf("index_url is required")
	}
	u, err := neturl.Parse(indexURL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return "", fmt.Errorf("index_url must be a valid http(s) URL")
	}
	return indexURL, nil
}

func resolveURL(base, ref string) string {
	b, err := neturl.Parse(base)
	if err != nil {
		return ref
	}
	refU, err := neturl.Parse(ref)
	if err != nil {
		return ref
	}
	return b.ResolveReference(refU).String()
}
