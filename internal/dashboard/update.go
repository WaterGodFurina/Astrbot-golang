package dashboard

// AstrBot Go 版本发布 / 切换升级实现。
// 版本列表从 GitHub Release API 拉取；"切换版本"根据当前 GOOS/GOARCH
// 选择对应平台的 zip 资产下载，解压后原子替换本进程二进制并触发重启。

import (
	"archive/zip"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"time"

	"github.com/WaterGodFurina/Astrbot-golang/internal/log"
	"github.com/WaterGodFurina/Astrbot-golang/internal/version"
)

const (
	updateRepoOwner = "WaterGodFurina"
	updateRepoName  = "Astrbot-golang"
	updateRepoAPI   = "https://api.github.com/repos/WaterGodFurina/Astrbot-golang/releases"
	// maxUpdateDownload 升级包下载字节上限（512 MiB），防恶意/损坏内容撑爆磁盘。
	maxUpdateDownload = 512 << 20
)

// updateTagRe 白名单校验升级目标版本号：仅允许可选 v 前缀 + 字母数字点横线，
// 防止恶意 tag 注入下载 URL（路径穿越/协议混淆）。
var updateTagRe = regexp.MustCompile(`^v?[0-9A-Za-z.\-]+$`)

var updateLogger = log.GetDefault().WithComponent("Updater")

// githubUpdateProxy 解析 update 请求传入的 GitHub 代理（前端可选），为空则
// 回退到配置的 github_proxy 加速前缀，再为空则直连。
func (s *Server) githubUpdateProxy(reqProxy string) string {
	if p := strings.TrimSpace(reqProxy); p != "" {
		return p
	}
	return s.githubProxyForMarket()
}

// updateProgressSet 记录切换版本进度（供前端轮询）。
func (s *Server) updateProgressSet(id string, st *installStatus) {
	if id == "" {
		return
	}
	s.updateProgressMu.Lock()
	s.updateProgress[id] = st
	s.updateProgressMu.Unlock()
}

// updateProgressGet 读取切换版本进度。
func (s *Server) updateProgressGet(id string) *installStatus {
	s.updateProgressMu.Lock()
	defer s.updateProgressMu.Unlock()
	st := s.updateProgress[id]
	return st
}

// fetchGithubReleases 从 GitHub Release API 拉取本项目的发行版列表。
// 返回每个 release 的简化视图：tag_name/name/published_at/body/prerelease。
func (s *Server) fetchGithubReleases(proxy string) ([]map[string]interface{}, error) {
	url := updateRepoAPI
	if proxy != "" {
		url = strings.TrimRight(proxy, "/") + "/" + url
	}
	// 出站 URL 校验防 SSRF（代理前缀为管理员输入）。
	if err := validateOutboundURL(url); err != nil {
		return nil, err
	}
	client := newOutboundClient(30 * time.Second)
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("GitHub release API 返回 HTTP %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return nil, err
	}
	var raw []struct {
		TagName     string `json:"tag_name"`
		Name        string `json:"name"`
		PublishedAt string `json:"published_at"`
		Body        string `json:"body"`
		Prerelease  bool   `json:"prerelease"`
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, err
	}
	out := make([]map[string]interface{}, 0, len(raw))
	for _, r := range raw {
		out = append(out, map[string]interface{}{
			"tag_name":     r.TagName,
			"name":         r.Name,
			"published_at": r.PublishedAt,
			"body":         r.Body,
			"prerelease":   r.Prerelease,
		})
	}
	return out, nil
}

// resourceForPlatform 把 GOOS/GOARCH 映射到 release 资产的后缀（对齐
// .github/workflows/release.yml 的 matrix resource 命名）。
func resourceForPlatform(goos, goarch string) string {
	switch goos {
	case "windows":
		if goarch == "arm64" {
			return "windows-aarch64"
		}
		return "windows-x86_64"
	case "darwin":
		if goarch == "arm64" {
			return "macos-aarch64"
		}
		return "macos-x86_64"
	case "android":
		return "android-aarch64"
	case "linux":
		switch goarch {
		case "arm64":
			return "linux-aarch64-gnu"
		case "loong64":
			return "linux-loongarch64-gnu"
		default:
			return "linux-x86_64-gnu"
		}
	}
	return ""
}

// buildDownloadURL 构造指定版本 + 平台 zip 的下载地址。
func buildDownloadURL(tag, resource string) string {
	return fmt.Sprintf("https://github.com/WaterGodFurina/Astrbot-golang/releases/download/%s/astrbot-golang-%s-%s.zip", tag, tag, resource)
}

// handleUpdateReleases 处理 GET /api/update/releases：返回发行版列表。
func (s *Server) handleUpdateReleases(proxy string) ([]map[string]interface{}, error) {
	return s.fetchGithubReleases(proxy)
}

// handleUpdateCheck 处理 GET /api/update/check：比较当前版本与最新发行版。
func (s *Server) handleUpdateCheck(proxy string) map[string]interface{} {
	releases, err := s.fetchGithubReleases(proxy)
	if err != nil {
		updateLogger.Warn("检查更新失败: %v", err)
		return map[string]interface{}{
			"version":        version.Version,
			"latest_version": version.Version,
			"has_update":     false,
		}
	}
	latest := version.Version
	for _, r := range releases {
		if pr, _ := r["prerelease"].(bool); pr {
			continue
		}
		tag, _ := r["tag_name"].(string)
		if tag != "" {
			latest = tag
			break
		}
	}
	cur := strings.TrimPrefix(version.Version, "v")
	lat := strings.TrimPrefix(latest, "v")
	has := lat != "" && lat != cur
	return map[string]interface{}{
		"version":        version.Version,
		"latest_version": latest,
		"has_update":     has,
	}
}

// doUpdateCore 执行"切换版本"：根据 version + 当前平台选择 zip 下载，
// 解压替换本进程二进制并触发重启。
func (s *Server) doUpdateCore(w http.ResponseWriter, progressID, tag, proxy string) {
	resource := resourceForPlatform(runtime.GOOS, runtime.GOARCH)
	if resource == "" {
		s.updateProgressSet(progressID, &installStatus{Status: "error", Text: "当前平台暂不支持自动升级"})
		writeJSON(w, http.StatusOK, apiOK(map[string]interface{}{
			"status": "error", "message": "当前平台暂不支持自动升级",
		}))
		return
	}

	tag = strings.TrimSpace(tag)
	if tag == "" {
		s.updateProgressSet(progressID, &installStatus{Status: "error", Text: "缺少目标版本号"})
		writeJSON(w, http.StatusOK, apiOK(map[string]interface{}{
			"status": "error", "message": "缺少目标版本号",
		}))
		return
	}
	if !updateTagRe.MatchString(tag) {
		s.updateProgressSet(progressID, &installStatus{Status: "error", Text: "非法版本号格式"})
		writeJSON(w, http.StatusOK, apiOK(map[string]interface{}{
			"status": "error", "message": "非法版本号格式",
		}))
		return
	}

	// 异步执行下载 + 替换 + 重启，先返回让前端开始轮询进度。
	go s.performUpdate(progressID, tag, resource, proxy)

	writeJSON(w, http.StatusOK, apiOK(map[string]interface{}{
		"status": "ok", "message": "开始下载 " + tag,
	}))
}

// performUpdate 在后台执行下载→解压→替换→重启。
func (s *Server) performUpdate(progressID, tag, resource, proxy string) {
	setp := func(pct int, text string) {
		s.updateProgressSet(progressID, &installStatus{Status: "downloading", Percent: pct, Text: text})
	}
	setp(2, "准备下载 "+tag)

	exe, err := os.Executable()
	if err != nil {
		s.updateProgressSet(progressID, &installStatus{Status: "error", Text: "定位可执行文件失败: " + err.Error()})
		return
	}

	downloadURL := buildDownloadURL(tag, resource)
	if proxy != "" {
		downloadURL = strings.TrimRight(proxy, "/") + "/" + downloadURL
	}
	// 出站 URL 校验防 SSRF（proxy 为管理员输入，tag 已过白名单）。
	if err := validateOutboundURL(downloadURL); err != nil {
		s.updateProgressSet(progressID, &installStatus{Status: "error", Text: "下载地址校验失败: " + err.Error()})
		return
	}
	setp(5, "下载 "+downloadURL)

	tmpDir, err := os.MkdirTemp("", "astrbot-update-*")
	if err != nil {
		s.updateProgressSet(progressID, &installStatus{Status: "error", Text: "创建临时目录失败: " + err.Error()})
		return
	}
	defer os.RemoveAll(tmpDir)

	zipPath := filepath.Join(tmpDir, "release.zip")
	if err := s.downloadFile(downloadURL, zipPath, progressID); err != nil {
		s.updateProgressSet(progressID, &installStatus{Status: "error", Text: "下载失败: " + err.Error()})
		return
	}
	setp(75, "解压并替换")

	newBin, err := extractSingleBinary(zipPath, tmpDir)
	if err != nil {
		s.updateProgressSet(progressID, &installStatus{Status: "error", Text: "解压失败: " + err.Error()})
		return
	}

	// 原子替换当前二进制：先写临时文件再 rename 覆盖。
	exeDir := filepath.Dir(exe)
	tmpExe := filepath.Join(exeDir, ".astrbot-update-bin"+filepath.Ext(exe))
	if err := copyFile(newBin, tmpExe); err != nil {
		s.updateProgressSet(progressID, &installStatus{Status: "error", Text: "写入新二进制失败: " + err.Error()})
		return
	}
	if err := os.Chmod(tmpExe, 0o755); err != nil {
		_ = os.Remove(tmpExe)
		s.updateProgressSet(progressID, &installStatus{Status: "error", Text: "设置权限失败: " + err.Error()})
		return
	}
	if runtime.GOOS == "windows" {
		// Windows 不允许覆盖/删除正在运行的 exe，但允许改名（MoveFileEx 无
		// REPLACE）。两步改名：先把当前 exe 移成 .old，再把新 exe 移入原位；
		// 新实例启动时删除 .old（见 main.go 启动清理）。
		oldExe := exe + ".old"
		_ = os.Remove(oldExe) // 清理上次升级残留
		if err := os.Rename(exe, oldExe); err != nil {
			_ = os.Remove(tmpExe)
			s.updateProgressSet(progressID, &installStatus{Status: "error", Text: "替换二进制失败（无法改名运行中的 exe）: " + err.Error()})
			return
		}
		if err := os.Rename(tmpExe, exe); err != nil {
			// 回滚：把旧 exe 移回原位，避免留下"无主 exe"。
			_ = os.Rename(oldExe, exe)
			_ = os.Remove(tmpExe)
			s.updateProgressSet(progressID, &installStatus{Status: "error", Text: "替换二进制失败: " + err.Error()})
			return
		}
	} else {
		if err := os.Rename(tmpExe, exe); err != nil {
			_ = os.Remove(tmpExe)
			s.updateProgressSet(progressID, &installStatus{Status: "error", Text: "替换二进制失败: " + err.Error()})
			return
		}
	}

	s.updateProgressSet(progressID, &installStatus{Status: "done", Percent: 100, Text: "升级完成，正在重启…"})

	// 触发核心自重启（spawn 新实例 → 优雅停机 → 退出）。
	if s.restartFunc != nil {
		go s.restartFunc()
	}
}

// downloadFile 流式下载 URL 到 dst，并更新进度。
func (s *Server) downloadFile(url, dst, progressID string) error {
	// 下载前再次校验出站 URL 防 SSRF。
	if err := validateOutboundURL(url); err != nil {
		return err
	}
	client := newOutboundClient(2 * time.Minute)
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	total := resp.ContentLength
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer out.Close()
	buf := make([]byte, 64*1024)
	var written int64
	for {
		n, rerr := resp.Body.Read(buf)
		if n > 0 {
			written += int64(n)
			if written > maxUpdateDownload {
				return fmt.Errorf("下载内容超过 %d MiB 上限", maxUpdateDownload>>20)
			}
			if _, werr := out.Write(buf[:n]); werr != nil {
				return werr
			}
			pct := 5
			if total > 0 {
				pct = 5 + int(70*float64(written)/float64(total))
			}
			s.updateProgressSet(progressID, &installStatus{Status: "downloading", Percent: pct, Text: fmt.Sprintf("下载中 %d%%", pct)})
		}
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			return rerr
		}
	}
	return nil
}

// extractSingleBinary 从 zip 中提取唯一的可执行文件（astrbot / astrbot.exe）。
func extractSingleBinary(zipPath, destDir string) (string, error) {
	zr, err := zip.OpenReader(zipPath)
	if err != nil {
		return "", err
	}
	defer zr.Close()
	for _, f := range zr.File {
		if f.FileInfo().IsDir() {
			continue
		}
		name := filepath.Base(f.Name)
		if name == "astrbot" || name == "astrbot.exe" {
			rc, err := f.Open()
			if err != nil {
				return "", err
			}
			dst := filepath.Join(destDir, name)
			out, err := os.Create(dst)
			if err != nil {
				rc.Close()
				return "", err
			}
			_, err = io.Copy(out, rc)
			rc.Close()
			out.Close()
			if err != nil {
				return "", err
			}
			return dst, nil
		}
	}
	return "", fmt.Errorf("压缩包中未找到 astrbot 可执行文件")
}

// copyFile 复制文件内容与权限。
func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer out.Close()
	if _, err := io.Copy(out, in); err != nil {
		return err
	}
	return out.Sync()
}

// changelogCachePath / changelogBodyPath 是 changelog 的本地缓存位置：
// releases 列表整体缓存在 data/changelogs.json，单版本 body 缓存为
// data/changelogs/v<version>.md（对齐 Python stat_service 的 v<version>.md
// 命名）。
func (s *Server) changelogCachePath() string {
	return filepath.Join(s.kbDataDir(), "changelogs.json")
}

func (s *Server) changelogBodyPath(version string) string {
	return filepath.Join(s.kbDataDir(), "changelogs", "v"+version+".md")
}

// changelogCacheTTL 控制 changelog 列表缓存的刷新间隔。
const changelogCacheTTL = 10 * time.Minute

// changelogVersionRe 白名单校验 changelog 版本号（防路径穿越）。
var changelogVersionRe = regexp.MustCompile(`^[a-zA-Z0-9._-]+$`)

// fileExistsLocal reports whether a file exists.
func fileExistsLocal(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// fetchCachedChangelogReleases 从 GitHub 拉取 releases 并缓存到 data/
// （TTL 内直接读缓存，离线时回退缓存）。
func (s *Server) fetchCachedChangelogReleases() []map[string]interface{} {
	path := s.changelogCachePath()
	if data, err := os.ReadFile(path); err == nil {
		var cached struct {
			FetchedAt time.Time                `json:"fetched_at"`
			Releases  []map[string]interface{} `json:"releases"`
		}
		if json.Unmarshal(data, &cached) == nil && cached.Releases != nil {
			if time.Since(cached.FetchedAt) < changelogCacheTTL || len(cached.Releases) == 0 {
				return cached.Releases
			}
		}
	}
	releases, err := s.fetchGithubReleases("")
	if err != nil {
		// 拉取失败时回退到已有缓存（若有）。
		if data, rerr := os.ReadFile(path); rerr == nil {
			var cached struct {
				Releases []map[string]interface{} `json:"releases"`
			}
			if json.Unmarshal(data, &cached) == nil && cached.Releases != nil {
				return cached.Releases
			}
		}
		return nil
	}
	if payload, err := json.MarshalIndent(map[string]interface{}{
		"fetched_at": time.Now(),
		"releases":   releases,
	}, "", "  "); err == nil {
		_ = writeFileAtomic(path, payload, 0o644)
	}
	return releases
}

// handleChangelogs implements GET /changelogs 与 GET /changelogs/{version}：
// 版本列表来自 GitHub releases（v 前缀剥离），单版本内容为 release body。
func (s *Server) handleChangelogs(w http.ResponseWriter, r *http.Request, parts []string) {
	if len(parts) > 0 && parts[0] != "" {
		version := strings.TrimPrefix(parts[0], "v")
		if !changelogVersionRe.MatchString(version) || strings.Contains(version, "..") {
			writeJSON(w, http.StatusOK, apiError("Invalid version format"))
			return
		}
		// 优先读本地缓存文件，其次从 releases 缓存取 body 落盘。
		if bodyPath := s.changelogBodyPath(version); fileExistsLocal(bodyPath) {
			if content, err := os.ReadFile(bodyPath); err == nil {
				writeJSON(w, http.StatusOK, apiOK(map[string]interface{}{
					"content": string(content),
					"version": version,
				}))
				return
			}
		}
		releases := s.fetchCachedChangelogReleases()
		for _, rel := range releases {
			tag, _ := rel["tag_name"].(string)
			if strings.TrimPrefix(tag, "v") == version {
				body, _ := rel["body"].(string)
				_ = os.MkdirAll(filepath.Dir(s.changelogBodyPath(version)), 0o755)
				_ = writeFileAtomic(s.changelogBodyPath(version), []byte(body), 0o644)
				writeJSON(w, http.StatusOK, apiOK(map[string]interface{}{
					"content": body,
					"version": version,
				}))
				return
			}
		}
		writeJSON(w, http.StatusNotFound, apiError("Changelog for version "+version+" not found"))
		return
	}
	releases := s.fetchCachedChangelogReleases()
	versions := make([]string, 0, len(releases))
	for _, rel := range releases {
		tag, _ := rel["tag_name"].(string)
		if tag != "" {
			versions = append(versions, strings.TrimPrefix(tag, "v"))
		}
	}
	writeJSON(w, http.StatusOK, apiOK(map[string]interface{}{"versions": versions}))
}
