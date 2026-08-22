package dashboard

// 升级包完整性校验（审查项 3.2-3）的单元测试：GitHub release 资产 digest
// 匹配通过 / 不匹配拒绝 / 无 digest 时告警放行 / assets 解析。

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
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

// newDigestTestServer 起一个模拟 GitHub release API 的 httptest 服务，
// assets 返回调用方给定的列表；返回 server 与指向它的 Server（proxy 前缀
// 指向测试服务器，绕过 validateOutboundURL 的私网拒绝——httptest 监听
// 127.0.0.1，而 isBlockedIP 放行回环）。
func newDigestTestServer(t *testing.T, tag string, assetsJSON string) (*httptest.Server, *Server) {
	t.Helper()
	mux := http.NewServeMux()
	// ghproxy 风格代理：前缀 + 完整原始 URL（Go 1.17+ ServeMux 会把
	// "//" 清理成 "/"，故匹配单斜杠形式）。
	// ghproxy 风格代理：前缀 + 完整原始 URL；ServeMux 把 "//" 清洗为 "/"，
	// 故 "https://" 变成 "/https:/"。
	mux.HandleFunc("/https:/api.github.com/repos/WaterGodFurina/Astrbot-golang/releases/tags/"+tag, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"assets": ` + assetsJSON + `}`))
	})
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)
	s := &Server{}
	return ts, s
}

func TestCompareUpdateChecksumMatches(t *testing.T) {
	content := []byte("payload")
	path := writeTestZip(t, content)
	if err := compareUpdateChecksum(path, sha256Hex(content)); err != nil {
		t.Fatalf("matching digest must pass: %v", err)
	}
}

func TestCompareUpdateChecksumMismatch(t *testing.T) {
	path := writeTestZip(t, []byte("payload"))
	want := sha256Hex([]byte("other payload"))
	if err := compareUpdateChecksum(path, want); err == nil {
		t.Fatal("mismatched digest must fail")
	}
}

func TestVerifyUpdateChecksumDigestMatch(t *testing.T) {
	content := []byte("payload")
	path := writeTestZip(t, content)
	ts, s := newDigestTestServer(t, "v1.0.0",
		fmt.Sprintf(`[{"name": %q, "digest": "sha256:%s"}]`, testZipName, sha256Hex(content)))
	if err := s.verifyUpdateChecksum(
		buildDownloadURL("v1.0.0", "linux-x86_64-gnu"), path, "v1.0.0", ts.URL,
	); err != nil {
		t.Fatalf("digest match must pass: %v", err)
	}
}

func TestVerifyUpdateChecksumDigestMismatchHardFail(t *testing.T) {
	path := writeTestZip(t, []byte("tampered payload"))
	want := sha256Hex([]byte("original payload"))
	ts, s := newDigestTestServer(t, "v1.0.0",
		fmt.Sprintf(`[{"name": %q, "digest": "sha256:%s"}]`, testZipName, want))
	if err := s.verifyUpdateChecksum(
		buildDownloadURL("v1.0.0", "linux-x86_64-gnu"), path, "v1.0.0", ts.URL,
	); err == nil {
		t.Fatal("digest mismatch must hard-fail the update")
	}
}

func TestVerifyUpdateChecksumNoDigestWarnPass(t *testing.T) {
	path := writeTestZip(t, []byte("payload"))
	// 历史版本资产无 digest 字段 → 告警放行。
	ts, s := newDigestTestServer(t, "1.1.0",
		fmt.Sprintf(`[{"name": %q}]`, testZipName))
	if err := s.verifyUpdateChecksum(
		buildDownloadURL("1.1.0", "linux-x86_64-gnu"), path, "1.1.0", ts.URL,
	); err != nil {
		t.Fatalf("missing digest must warn-and-pass: %v", err)
	}
}

func TestReleaseAssetDigest(t *testing.T) {
	hexSum := "2151b604e3429bff440b9fbc03eb3617bc2603cda96c95b9bb05277f9ddba255"
	assets := []map[string]interface{}{
		{"name": testZipName, "digest": "sha256:" + hexSum},
	}
	if got := releaseAssetDigest(assets, testZipName); got != hexSum {
		t.Fatalf("releaseAssetDigest = %q, want %q", got, hexSum)
	}
	// 非 sha256 前缀 / 非法 hex / 缺失文件名均返回空串。
	bad := []map[string]interface{}{{"name": testZipName, "digest": "md5:abc"}}
	if got := releaseAssetDigest(bad, testZipName); got != "" {
		t.Fatalf("non-sha256 digest must be ignored, got %q", got)
	}
	if got := releaseAssetDigest(nil, testZipName); got != "" {
		t.Fatalf("empty assets must yield empty digest, got %q", got)
	}
}
