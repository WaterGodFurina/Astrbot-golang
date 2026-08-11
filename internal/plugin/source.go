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
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// forbiddenImports are packages plugin source may not import (safety
// blacklist). Plugins run as subprocesses; these primitives allow arbitrary
// host execution / memory tricks and are surfaced to the user as risks.
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
}

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
	data, err := os.ReadFile(path)
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
		os.RemoveAll(tmp)
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
		archive := filepath.Join(tmp, "src-archive"+filepath.Ext(source))
		if err := downloadFile(ctx, source, archive); err != nil {
			return cleanup(fmt.Errorf("download %s: %w", source, err))
		}
		if err := extractArchive(archive, dst); err != nil {
			return cleanup(fmt.Errorf("extract %s: %w", source, err))
		}
		return dst, nil

	case isHTTP(source):
		if err := gitClone(ctx, source, dst); err != nil {
			return cleanup(fmt.Errorf("git clone %s: %w", source, err))
		}
		return dst, nil

	default:
		return cleanup(fmt.Errorf("unsupported plugin source %q (use a git URL, archive URL, or local directory)", source))
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

func isArchiveURL(s string) bool {
	return strings.HasSuffix(s, ".zip") ||
		strings.HasSuffix(s, ".tar.gz") ||
		strings.HasSuffix(s, ".tgz")
}

func gitClone(ctx context.Context, url, dest string) error {
	gitBin, err := exec.LookPath("git")
	if err != nil {
		return fmt.Errorf("git not found on PATH (required to clone %s): %w", url, err)
	}
	cmd := exec.CommandContext(ctx, gitBin, "clone", "--depth", "1", "--quiet", url, dest)
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
func downloadFileWithProgress(ctx context.Context, url, dest string, progress func(downloaded, total int64)) error {
	client := &http.Client{Timeout: 30 * time.Minute}
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
	f, err := os.Create(dest)
	if err != nil {
		return err
	}
	defer f.Close()
	if progress == nil {
		_, err = io.Copy(f, resp.Body)
		return err
	}
	buf := make([]byte, 64*1024)
	var written int64
	total := resp.ContentLength
	for {
		n, rerr := resp.Body.Read(buf)
		if n > 0 {
			if _, werr := f.Write(buf[:n]); werr != nil {
				return werr
			}
			written += int64(n)
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
	for _, zf := range zr.File {
		target, err := safeJoin(dest, zf.Name)
		if err != nil {
			return err
		}
		if zf.FileInfo().IsDir() {
			if err := os.MkdirAll(target, 0o755); err != nil {
				return err
			}
			continue
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		rc, err := zf.Open()
		if err != nil {
			return err
		}
		out, err := os.Create(target)
		if err != nil {
			rc.Close()
			return err
		}
		if _, err := io.Copy(out, rc); err != nil {
			out.Close()
			rc.Close()
			return err
		}
		out.Close()
		rc.Close()
	}
	return nil
}

func extractTarGz(src, dest string) error {
	f, err := os.Open(src)
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
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		target, err := safeJoin(dest, hdr.Name)
		if err != nil {
			return err
		}
		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0o755); err != nil {
				return err
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return err
			}
			out, err := os.Create(target)
			if err != nil {
				return err
			}
			if _, err := io.Copy(out, tr); err != nil {
				out.Close()
				return err
			}
			out.Close()
		}
	}
}

// safeJoin joins base+rel and rejects paths escaping base.
func safeJoin(base, rel string) (string, error) {
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
			return os.MkdirAll(target, 0o755)
		}
		if d.Type()&fs.ModeSymlink != 0 {
			return nil
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		in, err := os.Open(path)
		if err != nil {
			return err
		}
		defer in.Close()
		out, err := os.Create(target)
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
