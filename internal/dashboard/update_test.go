package dashboard

// 升级包完整性校验（审查项 3.2-3）的单元测试：sha256 匹配通过 / 不匹配
// 拒绝 / release 无校验资产时告警放行 / assets 兜底（checksums.txt）解析。

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// testZipName 对齐 buildDownloadURL 产出的平台 zip 命名。
const testZipName = "astrbot-golang-v1.0.0-linux-x86_64-gnu.zip"

// sha256Hex 计算字节流的 sha256 十六进制摘要。
func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// writeTestZip 把伪造的升级包内容写入临时文件并返回路径。
func writeTestZip(t *testing.T, content []byte) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "release.zip")
	if err := os.WriteFile(path, content, 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestExtractChecksumForFile(t *testing.T) {
	hash := sha256Hex([]byte("payload"))
	other := sha256Hex([]byte("other payload"))
	cases := []struct {
		name     string
		content  string
		filename string
		want     string
	}{
		{"sidecar 带文件名", hash + "  " + testZipName + "\n", testZipName, hash},
		{"二进制模式星号前缀", hash + " *" + testZipName, testZipName, hash},
		{"checksums 多行取匹配行", other + "  other.zip\n" + hash + "  " + testZipName + "\n", testZipName, hash},
		{"纯摘要单行", hash, testZipName, hash},
		{"大写摘要归一化", strings.ToUpper(hash) + "  " + testZipName, testZipName, hash},
		{"仅其它文件名", other + "  other.zip\n", testZipName, ""},
		{"垃圾内容", "not a checksum at all", testZipName, ""},
		{"截断摘要", hash[:63] + "  " + testZipName, testZipName, ""},
	}
	for _, c := range cases {
		if got := extractChecksumForFile(c.content, c.filename); got != c.want {
			t.Errorf("%s: got %q, want %q", c.name, got, c.want)
		}
	}
}

func TestCompareUpdateChecksum(t *testing.T) {
	content := []byte("fake release zip payload")
	path := writeTestZip(t, content)
	correct := sha256Hex(content)

	if err := compareUpdateChecksum(path, correct); err != nil {
		t.Errorf("匹配摘要应通过, got: %v", err)
	}
	wrong := strings.Repeat("0", 64)
	if err := compareUpdateChecksum(path, wrong); err == nil {
		t.Error("不匹配摘要必须拒绝")
	} else if !strings.Contains(err.Error(), "SHA-256") {
		t.Errorf("错误信息应说明 SHA-256 不匹配, got: %v", err)
	}
}

// TestVerifyUpdateChecksumSidecar 覆盖校验源 1：<downloadURL>.sha256 同目录
// 约定——匹配通过。
func TestVerifyUpdateChecksumSidecar(t *testing.T) {
	zipBytes := []byte("fake release zip payload")
	zipPath := writeTestZip(t, zipBytes)
	s := &Server{}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, ".sha256") {
			fmt.Fprintf(w, "%s  %s\n", sha256Hex(zipBytes), testZipName)
			return
		}
		_, _ = w.Write(zipBytes)
	}))
	defer srv.Close()

	if err := s.verifyUpdateChecksum(srv.URL+"/"+testZipName, zipPath, "v1.0.0", ""); err != nil {
		t.Errorf("sha256 匹配应通过, got: %v", err)
	}
}

// TestVerifyUpdateChecksumSidecarMismatch 校验文件存在但摘要错误 → 硬失败。
func TestVerifyUpdateChecksumSidecarMismatch(t *testing.T) {
	zipBytes := []byte("tampered zip payload")
	zipPath := writeTestZip(t, zipBytes)
	s := &Server{}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, ".sha256") {
			fmt.Fprintf(w, "%s  %s\n", sha256Hex([]byte("original payload")), testZipName)
			return
		}
		_, _ = w.Write(zipBytes)
	}))
	defer srv.Close()

	err := s.verifyUpdateChecksum(srv.URL+"/"+testZipName, zipPath, "v1.0.0", "")
	if err == nil {
		t.Fatal("摘要不匹配必须硬失败，绝不放行替换")
	}
	if !strings.Contains(err.Error(), "SHA-256") {
		t.Errorf("错误应说明摘要不匹配, got: %v", err)
	}
}

// TestVerifyUpdateChecksumNoAssetsWarnPass 历史旧版本：.sha256 404 且 release
// assets 里没有任何 checksum 资产 → 告警后放行（不能砍死旧版本更新）。
func TestVerifyUpdateChecksumNoAssetsWarnPass(t *testing.T) {
	zipBytes := []byte("legacy release zip")
	zipPath := writeTestZip(t, zipBytes)
	s := &Server{}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// 代理前缀形态：/https://api.github.com/repos/.../releases/tags/<tag>
		if strings.Contains(r.URL.Path, "/releases/tags/") {
			fmt.Fprintf(w, `{"assets":[{"name":%q,"browser_download_url":"https://github.com/x/%s"}]}`, testZipName, testZipName)
			return
		}
		http.NotFound(w, r) // <zip>.sha256 → 404，触发 assets 兜底
	}))
	defer srv.Close()

	// proxy 指向假服务，使 release API 请求也落在本地。
	if err := s.verifyUpdateChecksum(srv.URL+"/"+testZipName, zipPath, "v1.0.0", srv.URL); err != nil {
		t.Errorf("无校验资产应告警放行, got: %v", err)
	}
}

// TestVerifyUpdateChecksumAssetsFallback 校验源 2：.sha256 404 时从 release
// assets 里找 checksums.txt 汇总文件解析比对——多行按 zip 文件名取行，匹配
// 通过。
func TestVerifyUpdateChecksumAssetsFallback(t *testing.T) {
	zipBytes := []byte("assets fallback payload")
	zipPath := writeTestZip(t, zipBytes)
	s := &Server{}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.Contains(r.URL.Path, "checksums.txt"):
			// 汇总文件多行，须按 zip 文件名取对应行。
			fmt.Fprintf(w, "%s  other.zip\n%s  %s\n", sha256Hex([]byte("x")), sha256Hex(zipBytes), testZipName)
		case strings.Contains(r.URL.Path, "/releases/tags/"):
			fmt.Fprint(w, `{"assets":[{"name":"checksums.txt","browser_download_url":"https://github.com/x/checksums.txt"}]}`)
		default:
			http.NotFound(w, r) // <zip>.sha256 → 404
		}
	}))
	defer srv.Close()

	if err := s.verifyUpdateChecksum(srv.URL+"/"+testZipName, zipPath, "v1.0.0", srv.URL); err != nil {
		t.Errorf("assets 兜底 sha256 匹配应通过, got: %v", err)
	}
}

// TestVerifyUpdateChecksumAssetsFallbackMismatch assets 兜底路径的摘要不匹
// 配同样必须硬失败（资产名后缀 .sha256 也应命中校验资产匹配）。
func TestVerifyUpdateChecksumAssetsFallbackMismatch(t *testing.T) {
	zipBytes := []byte("tampered assets payload")
	zipPath := writeTestZip(t, zipBytes)
	s := &Server{}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.Contains(r.URL.Path, "a.sha256"):
			fmt.Fprintf(w, "%s  %s\n", sha256Hex([]byte("original payload")), testZipName)
		case strings.Contains(r.URL.Path, "/releases/tags/"):
			fmt.Fprint(w, `{"assets":[{"name":"astrbot-golang-v1.0.0.zip.sha256","browser_download_url":"https://github.com/x/a.sha256"}]}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	if err := s.verifyUpdateChecksum(srv.URL+"/"+testZipName, zipPath, "v1.0.0", srv.URL); err == nil {
		t.Fatal("assets 兜底摘要不匹配必须硬失败")
	}
}
