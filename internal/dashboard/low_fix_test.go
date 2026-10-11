// 回归测试：插件 Web 代理 multipart 上限恢复 413（中危）与 chat_stream
// absorbThink 合并分支重置 plain 锚点（中危）。
package dashboard

import (
	"bytes"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	pluginsdk "github.com/WaterGodFurina/Astrbot-go-plugin-sdk/v2"
)

// buildMultipartHTTPRequest 构造一个 multipart/form-data 请求，包含若干
// 字段（key=value）与若干文件（表单名 → 文件名/内容）。
func buildMultipartHTTPRequest(t *testing.T, fields map[string]string, files map[string]struct {
	name    string
	content []byte
}) *http.Request {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	for k, v := range fields {
		if err := mw.WriteField(k, v); err != nil {
			t.Fatalf("write field: %v", err)
		}
	}
	for name, f := range files {
		fw, err := mw.CreateFormFile(name, f.name)
		if err != nil {
			t.Fatalf("create form file: %v", err)
		}
		if _, err := fw.Write(f.content); err != nil {
			t.Fatalf("write file content: %v", err)
		}
	}
	if err := mw.Close(); err != nil {
		t.Fatalf("close multipart writer: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/plug/demo/upload", &buf)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	return req
}

// TestPluginWebProxyMultipartOversizedFile413 验证超过 64MB 的上传文件触发
// 413（恢复历史行为），而不是被静默截断后继续转发给插件。
func TestPluginWebProxyMultipartOversizedFile413(t *testing.T) {
	oversized := bytes.Repeat([]byte("A"), pluginWebProxyMaxPartSize+1)
	httpReq := buildMultipartHTTPRequest(t, nil, map[string]struct {
		name    string
		content []byte
	}{"file": {name: "big.bin", content: oversized}})
	if err := httpReq.ParseMultipartForm(64 << 20); err != nil {
		t.Fatalf("parse multipart: %v", err)
	}
	req := pluginsdk.HandleWebRequest{Query: map[string][]string{}, Headers: map[string][]string{}}
	if collectPluginWebProxyMultipart(httpReq, &req) {
		t.Fatal("超限文件必须返回 false（由调用方回 413），却收集成功")
	}
	if len(req.Files) != 0 {
		t.Fatalf("超限文件不得进入转发列表，got %d", len(req.Files))
	}
}

// TestPluginWebProxyMultipartOversizedValue413 验证超过 64MB 的表单字段值
// 同样触发 413（不转发截断数据）。
func TestPluginWebProxyMultipartOversizedValue413(t *testing.T) {
	bigValue := strings.Repeat("B", pluginWebProxyMaxPartSize+1)
	httpReq := buildMultipartHTTPRequest(t, map[string]string{"data": bigValue}, nil)
	if err := httpReq.ParseMultipartForm(64 << 20); err != nil {
		t.Fatalf("parse multipart: %v", err)
	}
	req := pluginsdk.HandleWebRequest{Query: map[string][]string{}, Headers: map[string][]string{}}
	if collectPluginWebProxyMultipart(httpReq, &req) {
		t.Fatal("超限字段值必须返回 false（由调用方回 413），却收集成功")
	}
}

// TestPluginWebProxyMultipartWithinLimitOK 验证未超限的文件/字段正常收集：
// 恰好 64MB 的文件允许通过（边界语义与历史实现一致），凭据字段被剥离。
func TestPluginWebProxyMultipartWithinLimitOK(t *testing.T) {
	exact := bytes.Repeat([]byte("C"), pluginWebProxyMaxPartSize)
	httpReq := buildMultipartHTTPRequest(t,
		map[string]string{"api_key": "secret", "note": "hello"},
		map[string]struct {
			name    string
			content []byte
		}{"file": {name: "ok.bin", content: exact}},
	)
	if err := httpReq.ParseMultipartForm(64 << 20); err != nil {
		t.Fatalf("parse multipart: %v", err)
	}
	req := pluginsdk.HandleWebRequest{Query: map[string][]string{}, Headers: map[string][]string{}}
	if !collectPluginWebProxyMultipart(httpReq, &req) {
		t.Fatal("恰好 64MB 的文件不应触发 413")
	}
	if got := req.Query["note"]; len(got) != 1 || got[0] != "hello" {
		t.Fatalf("普通字段应保留，got %v", req.Query)
	}
	if _, ok := req.Query["api_key"]; ok {
		t.Fatalf("凭据字段 api_key 必须剥离，got %v", req.Query)
	}
	if len(req.Files) != 1 || len(req.Files[0].Content) != pluginWebProxyMaxPartSize {
		t.Fatalf("恰好 64MB 的文件应完整收集，got %d files", len(req.Files))
	}
}

// TestAbsorbThinkResetsPlainAnchor 验证 absorbThink 的合并分支同样重置
// lastPlainIndex：plain("A") → think("T") → plain("B") 必须持久化为
// [plain A, think T, plain B] 三个 part，B 不得并回推理前的 A（历史 bug：
// parts 顺序变成 "AB"+"T"，与实际流序不符）。
func TestAbsorbThinkResetsPlainAnchor(t *testing.T) {
	sink := newChainPartsSink()
	sink.absorbPlain("A")
	sink.absorbThink("T1")
	sink.absorbThink("T2") // 相邻推理合并进同一 think part
	sink.absorbPlain("B")

	parts := sink.storageParts()
	if len(parts) != 3 {
		t.Fatalf("期望 3 个 parts（A/T/B），got %d: %+v", len(parts), parts)
	}
	if p := parts[0]; p["type"] != "plain" || p["text"] != "A" {
		t.Fatalf("part0 应为 plain A，got %+v", p)
	}
	if p := parts[1]; p["type"] != "think" || p["think"] != "T1T2" {
		t.Fatalf("part1 应为 think T1T2（相邻推理合并），got %+v", p)
	}
	if p := parts[2]; p["type"] != "plain" || p["text"] != "B" {
		t.Fatalf("part2 应为 plain B（不得并回推理前的 A），got %+v", p)
	}

	// 推理后的新正文继续到达时，应并入 B（最后一个 plain part）。
	sink.absorbPlain("C")
	parts = sink.storageParts()
	if len(parts) != 3 {
		t.Fatalf("C 应并入 B 所在 part，期望仍 3 个 parts，got %d", len(parts))
	}
	if p := parts[2]; p["text"] != "BC" {
		t.Fatalf("C 应并入推理后的 B part，got %+v", p)
	}
}

// TestAbsorbPlainAfterAttachmentStillAppends 保证既有语义不被破坏：
// 媒体附件之后到达的正文追加新 plain part。
func TestAbsorbPlainAfterAttachmentStillAppends(t *testing.T) {
	sink := newChainPartsSink()
	sink.absorbPlain("A")
	sink.absorbAttachment("image", "att-1", "pic.png", "pic.png")
	sink.absorbPlain("B")
	parts := sink.storageParts()
	if len(parts) != 3 {
		t.Fatalf("期望 3 个 parts，got %d: %+v", len(parts), parts)
	}
	if parts[2]["text"] != "B" {
		t.Fatalf("附件后的正文应独立成 part，got %+v", parts[2])
	}
}

// TestServeWebUIExternalDirMissingAsset404 验证外部 webuiDir 存在但具体资产
// 缺失时直接 404，不回落 embedded（避免静默服务旧版本资产造成缓存错配）；
// 非资产路径仍走 SPA fallback（index.html）。
func TestServeWebUIExternalDirMissingAsset404(t *testing.T) {
	s := NewServer(0, filepath.Join(t.TempDir(), "test_pw.json"))
	defer s.Stop()
	external := t.TempDir()
	if err := os.WriteFile(filepath.Join(external, "index.html"), []byte("<html>external</html>"), 0o644); err != nil {
		t.Fatal(err)
	}
	s.SetWebUIDir(external)

	// 外部目录已有的文件正常服务。
	w := httptest.NewRecorder()
	s.serveWebUI(w, httptest.NewRequest(http.MethodGet, "/index.html", nil))
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "external") {
		t.Fatalf("外部目录已有文件应正常服务，got %d %s", w.Code, w.Body.String())
	}

	// 外部目录缺失的资产：404 而不是回落 embedded。
	w = httptest.NewRecorder()
	s.serveWebUI(w, httptest.NewRequest(http.MethodGet, "/assets/no-such-hash.js", nil))
	if w.Code != http.StatusNotFound {
		t.Fatalf("外部目录缺失资产应 404，got %d（可能回落了 embedded 旧资产）", w.Code)
	}

	// 非资产路径仍回退 index.html（SPA fallback 保持不变）。
	w = httptest.NewRecorder()
	s.serveWebUI(w, httptest.NewRequest(http.MethodGet, "/some/route", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("非资产路径应 SPA fallback 200，got %d", w.Code)
	}
}
