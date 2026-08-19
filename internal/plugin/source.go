package plugin

import (
	"archive/tar"
	"archive/zip"
	"compress/gzip"
	"context"
	"fmt"
	"go/parser"
	"go/token"
	"io"
	"io/fs"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

// forbiddenImports are packages plugin source may not import. This is a
// 风险提示 (risk hint), NOT a security boundary: it only flags exact-match
// import paths in the plugin's own .go files and never blocks anything by
// itself.
//
// What it does NOT cover:
//   - indirect imports (a dependency pulling in os/exec, syscall, ...);
//   - cgo (import "C" / #cgo flags are reported as risks, not hard-rejected);
//   - //go:linkname and //go:generate (reported as risks, not hard-rejected);
//   - equivalent unlisted packages (os, net, net/http, io/fs, ...) that can
//     spawn processes or touch the host filesystem/network just as easily.
//
// The real isolation boundary is the child process: plugins run as separate
// subprocesses under the same OS user, so a misbehaving plugin can reach the
// host's files and network no matter what is listed here. This list only
// surfaces obvious risk to the user before install.
var forbiddenImports = []string{"os/exec", "syscall", "unsafe", "reflect"}

// riskyDirectives are comment directives in the plugin's own source that can
// bypass import-level scanning or execute arbitrary code at build time.
var riskyDirectives = []struct {
	marker string
	name   string
	desc   string
}{
	{"//go:linkname", "go:linkname", "链接到内部符号，可绕过 import 限制执行底层操作"},
	{"//go:generate", "go:generate", "在编译期执行任意命令（可生成危险代码）"},
	{"#cgo CFLAGS", "cgo-CFLAGS", "cgo 编译标志，可注入任意编译期命令"},
	{"#cgo LDFLAGS", "cgo-LDFLAGS", "cgo 链接标志，可注入任意编译期命令"},
}

// maxDownloadSize caps a single source download (zip/tar.gz archives).
const maxDownloadSize int64 = 200 << 20 // 200MB

// maxExtractSize / maxExtractFiles cap the total uncompressed archive size and
// entry count so a malicious archive cannot exhaust disk/inodes during install.
const (
	maxExtractSize  int64 = 300 << 20 // 300MB 解压总量上限
	maxExtractFiles int64 = 10000     // 解压条目数上限
)

// ScanFinding describes one risky import in the plugin source.
type ScanFinding struct {
	File    string `json:"file"`    // path relative to the source root
	Line    int    `json:"line"`    // 1-based line of the import statement
	Import  string `json:"import"`  // the forbidden package
	Snippet string `json:"snippet"` // trimmed source line
}

// StaticScan inspects plugin source before compilation: it parses every .go
// file and collects any blacklisted import as a ScanFinding. It does NOT
// reject the plugin; the caller decides (install is blocked unless the user
// explicitly ignores the risk). Does not require network or a Go toolchain.
//
// Like forbiddenImports, this scan is a 风险提示 (risk hint), not a security
// boundary. It only inspects the plugin's own source for exact-match imports
// and comment directives: indirect dependencies, cgo, //go:linkname and
// //go:generate (flagged, never hard-blocked), and unlisted equivalents
// (os, net, net/http, io/fs, ...) are all outside its coverage. Real
// isolation comes from running the plugin as a child subprocess of AstrBot
// (same OS user), never from this scan.
func StaticScan(srcDir string) ([]ScanFinding, error) {
	var findings []ScanFinding
	err := filepath.WalkDir(srcDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if d.Name() == ".git" || d.Name() == "vendor" || d.Name() == "testdata" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") {
			return nil
		}
		finds, err := scanFile(srcDir, path)
		if err != nil {
			return err
		}
		findings = append(findings, finds...)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return findings, nil
}

// scanFile parses one Go file and returns findings for blacklisted imports and
// risky directives (//go:linkname, //go:generate).
func scanFile(srcDir, path string) ([]ScanFinding, error) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, path, nil, parser.ImportsOnly)
	if err != nil {
		return nil, err
	}
	rel := path
	if r, err := filepath.Rel(srcDir, path); err == nil {
		rel = r
	}

	var findings []ScanFinding
	for _, imp := range f.Imports {
		p, err := strconv.Unquote(imp.Path.Value)
		if err != nil {
			continue
		}
		// import "C"（cgo）：#cgo CFLAGS/LDFLAGS 与 import "C" 意味着编译期
		// 会调用 C 编译器，可借此执行任意命令，作为风险项上报（不硬拒）。
		if p == "C" {
			pos := fset.Position(imp.Pos())
			findings = append(findings, ScanFinding{
				File:    rel,
				Line:    pos.Line,
				Import:  "cgo-import-C",
				Snippet: lineAt(path, pos.Line),
			})
			continue
		}
		if !containsStr(forbiddenImports, p) {
			continue
		}
		pos := fset.Position(imp.Pos())
		findings = append(findings, ScanFinding{
			File:    rel,
			Line:    pos.Line,
			Import:  p,
			Snippet: lineAt(path, pos.Line),
		})
	}

	// Risky comment directives: //go:linkname and //go:generate can bypass
	// import-level detection (compile-time injection / symbol linking).
	if data, err := os.ReadFile(path); err == nil {
		lines := strings.Split(string(data), "\n")
		for _, d := range riskyDirectives {
			for i, ln := range lines {
				if !strings.Contains(ln, d.marker) {
					continue
				}
				findings = append(findings, ScanFinding{
					File:    rel,
					Line:    i + 1,
					Import:  d.name,
					Snippet: strings.TrimSpace(ln),
				})
				break // one finding per directive per file is enough
			}
		}
	}

	return findings, nil
}

func containsStr(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

// lineAt returns the trimmed text of the given 1-based line, or "".
func lineAt(path string, line int) string {
	data, err := os.ReadFile(path) // #nosec G304 -- path 为插件源码本地路径（安装/报错定位用）
	if err != nil {
		return ""
	}
	lines := strings.Split(string(data), "\n")
	if line >= 1 && line <= len(lines) {
		return strings.TrimSpace(lines[line-1])
	}
	return ""
}

// fetchSource obtains plugin source from a URL or local directory into a
// temporary directory and returns its path. Caller must os.RemoveAll it.
//
// Supported sources:
//   - local directory (copied)
//   - archive URL ending in .zip / .tar.gz / .tgz (downloaded + extracted)
//   - git URL (cloned shallow with `git`)
func (m *SubprocessManager) fetchSource(ctx context.Context, id, source string) (string, error) {
	tmp, err := os.MkdirTemp("", "astrbot-plugin-src-"+sanitizeID(id)+"-*")
	if err != nil {
		return "", err
	}
	cleanup := func(err error) (string, error) {
		_ = os.RemoveAll(tmp)
		return "", err
	}
	dst := filepath.Join(tmp, "src")

	switch {
	case isLocalDir(source):
		if err := copyDir(source, dst); err != nil {
			return cleanup(fmt.Errorf("copy source: %w", err))
		}
		return dst, nil

	case isLocalArchiveFile(source):
		if err := extractArchive(source, dst); err != nil {
			return cleanup(fmt.Errorf("extract %s: %w", source, err))
		}
		return dst, nil

	case isArchiveURL(source):
		if err := validateSourceURL(source); err != nil {
			return cleanup(fmt.Errorf("invalid archive source %q: %w", source, err))
		}
		archive := filepath.Join(tmp, "src-archive"+archiveExt(source))
		if err := downloadFile(ctx, source, archive); err != nil {
			return cleanup(fmt.Errorf("download %s: %w", source, err))
		}
		if err := extractArchive(archive, dst); err != nil {
			return cleanup(fmt.Errorf("extract %s: %w", source, err))
		}
		return dst, nil

	case isRemoteURL(source):
		if err := validateSourceURL(source); err != nil {
			return cleanup(fmt.Errorf("invalid source %q: %w", source, err))
		}
		// GitHub 加速：配置了 github_proxy 时重写 github.com 仓库 URL
		gitSource := m.applyGitHubProxy(source)
		if err := gitClone(ctx, gitSource, dst); err != nil {
			return cleanup(fmt.Errorf("git clone %s: %w", gitSource, err))
		}
		return dst, nil

	default:
		return cleanup(fmt.Errorf("unsupported plugin source %q (use a git URL, archive URL, or local directory)", source))
	}
}

// isRemoteURL reports whether s is an http/https/git URL (as opposed to a
// local directory or file).
func isRemoteURL(s string) bool {
	u, err := url.Parse(s)
	if err != nil {
		return false
	}
	switch u.Scheme {
	case "http", "https", "git":
		return true
	}
	return false
}

// validateSourceURL enforces safe source URLs: only http/https/git schemes, no
// userinfo, and no loopback/private/link-local hosts (SSRF guard for archive
// downloads and git clones).
func validateSourceURL(raw string) error {
	u := strings.TrimSpace(raw)
	if u == "" {
		return fmt.Errorf("source URL 为空")
	}
	parsed, err := url.Parse(u)
	if err != nil {
		return fmt.Errorf("source URL 解析失败: %w", err)
	}
	switch parsed.Scheme {
	case "http", "https", "git":
	default:
		return fmt.Errorf("source URL 协议不支持（仅允许 http/https/git）: %q", raw)
	}
	if parsed.User != nil {
		return fmt.Errorf("source URL 不允许包含用户信息(userinfo): %q", raw)
	}
	host := parsed.Hostname()
	if host == "" {
		return fmt.Errorf("source URL 缺少主机名: %q", raw)
	}
	if err := rejectLocalHost(host); err != nil {
		return fmt.Errorf("source URL %q: %w", raw, err)
	}
	return nil
}

// safeRedirect is an http.Client CheckRedirect that re-applies the SSRF guard
// (rejectLocalHost) to every redirect hop: the default client follows redirects
// without revalidating the target, so an attacker-chosen 302 could otherwise
// redirect an archive download to a private/metadata endpoint.
func safeRedirect(req *http.Request, via []*http.Request) error {
	if err := rejectLocalHost(req.URL.Hostname()); err != nil {
		return fmt.Errorf("重定向目标 %s: %w", req.URL, err)
	}
	return nil
}

// rejectLocalHost rejects hosts that are or resolve to the local machine,
// private or link-local networks (SSRF guard).
//
// The check FAILS CLOSED: a host whose name cannot be resolved is rejected
// instead of skipped, so a resolution error or attacker-controlled DNS can
// never silently bypass the guard.
//
// Residual DNS-rebinding window: this guard resolves the hostname, then the
// actual connection happens separately. On the HTTP download path
// (downloadFileWithProgress) the connection is pinned to a single resolution
// (see pinnedDialContext), so the address dialed cannot be swapped between
// the check and the connect. Git clones go through the external `git` CLI,
// which re-resolves the hostname on its own; for that path this check remains
// a best-effort hint and a rebinding attacker could still point git at an
// internal address.
func rejectLocalHost(host string) error {
	h := strings.ToLower(strings.TrimSuffix(host, "."))
	if h == "localhost" || h == "ip6-localhost" || strings.HasSuffix(h, ".localhost") {
		return fmt.Errorf("不允许指向本机 host: %s", host)
	}
	if strings.HasSuffix(h, ".local") {
		return fmt.Errorf("不允许指向 .local 主机: %s", host)
	}
	if ip := net.ParseIP(h); ip != nil {
		if ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsUnspecified() {
			return fmt.Errorf("不允许指向本机/内网/链路本地地址: %s", host)
		}
		return nil
	}
	ips, err := net.LookupIP(h)
	if err != nil {
		// 解析失败必须拒绝（fail closed）：无法验证目标不是内网地址时
		// 不允许继续，防止 DNS 故障/恶意 DNS 跳过 SSRF 检查。
		return fmt.Errorf("域名解析失败: %s (%w)", host, err)
	}
	for _, ip := range ips {
		if ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsUnspecified() {
			return fmt.Errorf("域名解析到本机/内网/链路本地地址: %s (%s)", host, ip)
		}
	}
	return nil
}

// pinnedDialContext returns an http.Transport DialContext that resolves each
// hostname exactly once (on first use) and pins every subsequent dial of that
// host to the first resolution result. Combined with rejectLocalHost, this
// closes the DNS-rebinding TOCTOU window on the HTTP download path: the
// address actually dialed comes from a single resolution, so an attacker
// cannot answer "public IP" to the guard and "internal IP" to the connection.
// The request URL is left untouched, so the Host header and TLS SNI still
// carry the original hostname. Redirect targets are resolved independently
// (and each is re-checked by safeRedirect). IPv4 is preferred when a host
// returns both families, since the pin removes the dialer's multi-address
// fallback.
func pinnedDialContext() func(ctx context.Context, network, addr string) (net.Conn, error) {
	var mu sync.Mutex
	pinned := map[string]net.IP{}
	dialer := &net.Dialer{Timeout: 30 * time.Second}
	return func(ctx context.Context, network, addr string) (net.Conn, error) {
		host, port, err := net.SplitHostPort(addr)
		if err != nil {
			return nil, err
		}
		mu.Lock()
		ip, ok := pinned[host]
		mu.Unlock()
		if !ok {
			ips, err := net.LookupIP(host)
			if err != nil {
				return nil, fmt.Errorf("resolve %s: %w", host, err)
			}
			if len(ips) == 0 {
				return nil, fmt.Errorf("resolve %s: no addresses", host)
			}
			ip = ips[0]
			for _, cand := range ips {
				if cand.To4() != nil {
					ip = cand
					break
				}
			}
			mu.Lock()
			if prev, ok := pinned[host]; ok {
				ip = prev // a concurrent dial won the pin race; use its result
			} else {
				pinned[host] = ip
			}
			mu.Unlock()
		}
		return dialer.DialContext(ctx, network, net.JoinHostPort(ip.String(), port))
	}
}

func isHTTP(s string) bool {
	return strings.HasPrefix(s, "http://") || strings.HasPrefix(s, "https://")
}

func isLocalDir(s string) bool {
	if isHTTP(s) {
		return false
	}
	info, err := os.Stat(s)
	return err == nil && info.IsDir()
}

// isLocalArchiveFile reports whether s is a local .zip/.tar.gz/.tgz file.
func isLocalArchiveFile(s string) bool {
	if isHTTP(s) {
		return false
	}
	info, err := os.Stat(s)
	if err != nil || info.IsDir() {
		return false
	}
	return isArchiveURL(s)
}

// archivePath returns the path portion of s used for archive-suffix matching.
// For http/https URLs the query string and fragment are stripped (a source like
// "https://x/y.zip?token=..." must still be recognized as a .zip archive and
// not fall through to a git clone). Local paths are returned unchanged.
func archivePath(s string) string {
	if isHTTP(s) {
		if u, err := url.Parse(s); err == nil && u.Path != "" {
			return u.Path
		}
	}
	return s
}

func isArchiveURL(s string) bool {
	p := archivePath(s)
	return strings.HasSuffix(p, ".zip") ||
		strings.HasSuffix(p, ".tar.gz") ||
		strings.HasSuffix(p, ".tgz")
}

// archiveExt maps an archive source URL to a fixed cache-file extension that
// extractArchive recognizes. filepath.Ext("x.tar.gz") returns only ".gz",
// which matches none of the switch cases, so multi-dot suffixes are preserved.
func archiveExt(s string) string {
	p := strings.ToLower(archivePath(s))
	switch {
	case strings.HasSuffix(p, ".tar.gz"):
		return ".tar.gz"
	case strings.HasSuffix(p, ".tgz"):
		return ".tgz"
	case strings.HasSuffix(p, ".zip"):
		return ".zip"
	}
	return filepath.Ext(p)
}

func gitClone(ctx context.Context, url, dest string) error {
	// URL 以 "-" 开头会被 git 当作选项解析（如 --upload-pack=...），
	// 直接拒绝，防止命令行参数注入。
	if strings.HasPrefix(url, "-") {
		return fmt.Errorf("git URL 不允许以 '-' 开头（拒绝选项注入）: %q", url)
	}
	gitBin, err := exec.LookPath("git")
	if err != nil {
		return fmt.Errorf("git not found on PATH (required to clone %s): %w", url, err)
	}
	cmd := exec.CommandContext(ctx, gitBin, "clone", "--depth", "1", "--quiet", url, dest) // #nosec G204 -- git 参数固定，URL 已拒绝 "-" 前缀防注入
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("%w\n%s", err, out)
	}
	return nil
}

func downloadFile(ctx context.Context, url, dest string) error {
	return downloadFileWithProgress(ctx, url, dest, nil)
}

// downloadFileWithProgress streams a URL to dest, invoking progress(downloaded,
// total) as bytes are written (when non-nil) so callers can show a live bar.
//
// The connection is pinned to a single DNS resolution per host
// (pinnedDialContext), so after rejectLocalHost's check the actual dial
// cannot be re-resolved to a different (e.g. internal) address. Each redirect
// hop is re-validated by safeRedirect and resolves on its own.
func downloadFileWithProgress(ctx context.Context, url, dest string, progress func(downloaded, total int64)) error {
	// Pinning: 每个 host 只解析一次，连接地址 = 检查时验证过的地址
	// （DNS-rebinding TOCTOU 防护，见 pinnedDialContext）。URL 保持不变，
	// Host header 与 TLS SNI 仍然携带原始主机名。
	transport := &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		DialContext:           pinnedDialContext(),
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          100,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
	}
	client := &http.Client{Timeout: 30 * time.Minute, CheckRedirect: safeRedirect, Transport: transport}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	if resp.ContentLength > maxDownloadSize {
		return fmt.Errorf("download %s exceeds size limit (%d bytes)", url, maxDownloadSize)
	}
	f, err := os.Create(dest) // #nosec G304 -- dest 为插件下载的目标临时路径
	if err != nil {
		return err
	}
	defer f.Close()
	// 限制实际读取的字节数（Content-Length 可缺失/不可信），超限报错。
	limited := io.LimitReader(resp.Body, maxDownloadSize+1)
	if progress == nil {
		n, err := io.Copy(f, limited)
		if err != nil {
			return err
		}
		if n > maxDownloadSize {
			return fmt.Errorf("download %s exceeds size limit (%d bytes)", url, maxDownloadSize)
		}
		return nil
	}
	buf := make([]byte, 64*1024)
	var written int64
	total := resp.ContentLength
	for {
		n, rerr := limited.Read(buf)
		if n > 0 {
			if _, werr := f.Write(buf[:n]); werr != nil {
				return werr
			}
			written += int64(n)
			if written > maxDownloadSize {
				return fmt.Errorf("download %s exceeds size limit (%d bytes)", url, maxDownloadSize)
			}
			progress(written, total)
		}
		if rerr == io.EOF {
			return nil
		}
		if rerr != nil {
			return rerr
		}
	}
}

// extractArchive unpacks a .zip/.tar.gz archive into dest, stripping a single
// top-level directory (GitHub-style "<repo>-<branch>/..." layout).
func extractArchive(archive, dest string) error {
	tmp, err := os.MkdirTemp("", "astrbot-extract-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmp)

	switch {
	case strings.HasSuffix(archive, ".zip"):
		err = extractZip(archive, tmp)
	case strings.HasSuffix(archive, ".tar.gz"), strings.HasSuffix(archive, ".tgz"):
		err = extractTarGz(archive, tmp)
	default:
		return fmt.Errorf("unsupported archive: %s", archive)
	}
	if err != nil {
		return err
	}
	return moveContentsUp(tmp, dest)
}

func moveContentsUp(src, dest string) error {
	entries, err := os.ReadDir(src)
	if err != nil {
		return err
	}
	if len(entries) == 1 && entries[0].IsDir() {
		src = filepath.Join(src, entries[0].Name())
		if entries, err = os.ReadDir(src); err != nil {
			return err
		}
	}
	// #nosec G301 -- 插件目录需可被子进程/前端读取
	if err := os.MkdirAll(dest, 0o755); err != nil {
		return err
	}
	for _, e := range entries {
		if err := os.Rename(filepath.Join(src, e.Name()), filepath.Join(dest, e.Name())); err != nil {
			return err
		}
	}
	return nil
}

func extractZip(src, dest string) error {
	zr, err := zip.OpenReader(src)
	if err != nil {
		return err
	}
	defer zr.Close()
	if len(zr.File) > int(maxExtractFiles) {
		return fmt.Errorf("archive has too many entries (%d > %d)", len(zr.File), maxExtractFiles)
	}
	var total int64
	for _, zf := range zr.File {
		if total >= maxExtractSize {
			return fmt.Errorf("archive exceeds total size limit (%d bytes)", maxExtractSize)
		}
		// #nosec G115 -- 上一行已保证 total < maxExtractSize，差值为正
		if zf.UncompressedSize64 > uint64(maxExtractSize-total) {
			return fmt.Errorf("archive exceeds total size limit (%d bytes)", maxExtractSize)
		}
		target, err := safeJoin(dest, zf.Name)
		if err != nil {
			return err
		}
		if zf.FileInfo().IsDir() {
			// #nosec G301 -- target 经 safeJoin 校验
			if err := os.MkdirAll(target, 0o755); err != nil {
				return err
			}
			continue
		}
		// #nosec G301 -- target 经 safeJoin 校验
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		rc, err := zf.Open()
		if err != nil {
			return err
		}
		out, err := os.Create(target) // #nosec G304 -- target 经 safeJoin 校验防穿越
		if err != nil {
			_ = rc.Close()
			return err
		}
		n, err := io.Copy(out, io.LimitReader(rc, maxExtractSize-total+1))
		cerr := out.Close()
		_ = rc.Close()
		if err != nil {
			return err
		}
		if cerr != nil {
			return cerr
		}
		total += n
		if total > maxExtractSize {
			return fmt.Errorf("archive exceeds total size limit (%d bytes)", maxExtractSize)
		}
	}
	return nil
}

func extractTarGz(src, dest string) error {
	f, err := os.Open(src) // #nosec G304 -- src 为插件下载归档的本地路径
	if err != nil {
		return err
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return err
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	var total int64
	entries := 0
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		entries++
		if int64(entries) > maxExtractFiles {
			return fmt.Errorf("archive has too many entries (%d > %d)", entries, maxExtractFiles)
		}
		if total >= maxExtractSize || hdr.Size > maxExtractSize-total {
			return fmt.Errorf("archive exceeds total size limit (%d bytes)", maxExtractSize)
		}
		target, err := safeJoin(dest, hdr.Name)
		if err != nil {
			return err
		}
		switch hdr.Typeflag {
		case tar.TypeDir:
			// #nosec G301 -- target 经 safeJoin 校验
			if err := os.MkdirAll(target, 0o755); err != nil {
				return err
			}
		case tar.TypeReg:
			// #nosec G301 -- target 经 safeJoin 校验
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return err
			}
			out, err := os.Create(target) // #nosec G304 -- target 经 safeJoin 校验防穿越
			if err != nil {
				return err
			}
			n, err := io.Copy(out, io.LimitReader(tr, maxExtractSize-total+1))
			cerr := out.Close()
			if err != nil {
				return err
			}
			if cerr != nil {
				return cerr
			}
			total += n
			if total > maxExtractSize {
				return fmt.Errorf("archive exceeds total size limit (%d bytes)", maxExtractSize)
			}
		}
	}
}

// safeJoin joins base+rel and rejects paths escaping base (zip slip).
func safeJoin(base, rel string) (string, error) {
	// 绝对路径条目与 ".." 穿越直接拒绝（即使 Join 后不会逃逸也要拦截，
	// 防止在 Windows 盘符/UNC 等边界产生歧义）。
	if filepath.IsAbs(rel) {
		return "", fmt.Errorf("archive entry uses absolute path: %s", rel)
	}
	clean := filepath.Clean(rel)
	if clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("archive entry escapes destination: %s", rel)
	}
	target := filepath.Join(base, rel)
	absBase, err := filepath.Abs(base)
	if err != nil {
		return "", err
	}
	absTarget, err := filepath.Abs(target)
	if err != nil {
		return "", err
	}
	relPath, err := filepath.Rel(absBase, absTarget)
	if err != nil || relPath == ".." || strings.HasPrefix(relPath, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("archive entry escapes destination: %s", rel)
	}
	return target, nil
}

// copyDir recursively copies a local directory.
func copyDir(src, dest string) error {
	return filepath.WalkDir(src, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		target := filepath.Join(dest, rel)
		if d.IsDir() {
			return os.MkdirAll(target, 0o755) // #nosec G301 -- 拷贝用户提供的插件源码目录
		}
		if d.Type()&fs.ModeSymlink != 0 {
			return nil
		}
		// #nosec G301 -- 拷贝用户提供的插件源码目录
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		in, err := os.Open(path) // #nosec G304 -- 遍历用户提供的插件源码目录
		if err != nil {
			return err
		}
		defer in.Close()
		out, err := os.Create(target) // #nosec G304 -- target 由 WalkDir 相对路径拼装，未穿越 dest
		if err != nil {
			return err
		}
		_, err = io.Copy(out, in)
		cerr := out.Close()
		if err != nil {
			return err
		}
		return cerr
	})
}

// copyDirMerge copies src into dest, merging with any existing content
// (source files overwrite same-named targets). 迁移场景用：旧源码目录并入
// 已被文档缓存占位的目标目录。
func copyDirMerge(src, dest string) error {
	return filepath.WalkDir(src, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		target := filepath.Join(dest, rel)
		if d.IsDir() {
			return os.MkdirAll(target, 0o755) // #nosec G301 -- 迁移合并插件源码目录
		}
		if d.Type()&fs.ModeSymlink != 0 {
			return nil
		}
		// #nosec G301 -- 迁移合并插件源码目录
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		in, err := os.Open(path) // #nosec G304 -- 遍历迁移的插件源码目录
		if err != nil {
			return err
		}
		out, err := os.Create(target) // #nosec G304 -- target 由 WalkDir 相对路径拼装，未穿越 dest
		if err != nil {
			_ = in.Close()
			return err
		}
		_, cerr := io.Copy(out, in)
		ierr := in.Close()
		oerr := out.Close()
		if cerr != nil {
			return cerr
		}
		if ierr != nil {
			return ierr
		}
		return oerr
	})
}
