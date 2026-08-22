// kb_documents 的分发壳表驱动测试：验证 handleKBDocuments 拆分后路由匹配
// 顺序（rest 段解析）、各分支的 HTTP 方法判断与兜底列表行为与拆分前一致。
package dashboard

import (
	"bytes"
	"encoding/json"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
)

func TestHandleKBDocumentsDispatch(t *testing.T) {
	s := NewServer(0, filepath.Join(t.TempDir(), "pw.json"))
	defer s.Stop()

	// 一个合法但不含文件字段的 multipart 请求体（命中上传分支后应报缺少文件）。
	var mpBuf bytes.Buffer
	mpw := multipart.NewWriter(&mpBuf)
	_ = mpw.WriteField("chunk_size", "256")
	_ = mpw.Close()

	cases := []struct {
		name        string
		method      string
		parts       []string
		contentType string
		body        string
		wantAPI     string // 期望响应 status：ok / error
		wantMsg     string // 期望 message 子串（列表分支为空，另查 items）
		wantList    bool   // 列表分支：校验 data.items / data.page
	}{
		{
			name:    "POST import-url 缺 url 命中导入分支",
			method:  http.MethodPost,
			parts:   []string{"import-url"},
			body:    `{"url":""}`,
			wantAPI: "error", wantMsg: "缺少或无效的参数 url",
		},
		{
			name:    "GET import-url 同样命中导入分支（导入不限方法）",
			method:  http.MethodGet,
			parts:   []string{"import-url"},
			wantAPI: "error", wantMsg: "缺少或无效的参数 url",
		},
		{
			name:    "GET 单文档不存在命中详情分支",
			method:  http.MethodGet,
			parts:   []string{"doc_missing"},
			wantAPI: "error", wantMsg: "文档不存在",
		},
		{
			name:    "DELETE 单文档命中删除分支（database 为 nil 时视作成功）",
			method:  http.MethodDelete,
			parts:   []string{"doc_missing"},
			wantAPI: "ok", wantMsg: "文档已删除",
		},
		{
			name:    "POST 非 multipart 命中上传分支",
			method:  http.MethodPost,
			wantAPI: "error", wantMsg: "解析上传文件失败",
		},
		{
			name:        "POST multipart 缺文件命中上传分支",
			method:      http.MethodPost,
			contentType: mpw.FormDataContentType(),
			body:        mpBuf.String(),
			wantAPI:     "error", wantMsg: "缺少文件",
		},
		{
			name: "GET 空余段命中列表分支", method: http.MethodGet,
			wantAPI: "ok", wantList: true,
		},
		{
			name:   "PUT 带段名不匹配详情/上传，兜底命中列表分支",
			method: http.MethodPut, parts: []string{"doc_x"},
			wantAPI: "ok", wantList: true,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			req := httptest.NewRequest(c.method, "/api/knowledge-bases/kb1/documents", strings.NewReader(c.body))
			if c.contentType != "" {
				req.Header.Set("Content-Type", c.contentType)
			}
			w := httptest.NewRecorder()
			s.handleKBDocuments(w, req, "kb1", c.parts)

			var v map[string]interface{}
			if err := json.Unmarshal(w.Body.Bytes(), &v); err != nil {
				t.Fatalf("invalid json response: %v (%s)", err, w.Body.String())
			}
			if st, _ := v["status"].(string); st != c.wantAPI {
				t.Fatalf("expected status %q, got: %s", c.wantAPI, w.Body.String())
			}
			if c.wantMsg != "" {
				if msg, _ := v["message"].(string); !strings.Contains(msg, c.wantMsg) {
					t.Fatalf("expected message containing %q, got: %s", c.wantMsg, w.Body.String())
				}
			}
			if c.wantList {
				data, _ := v["data"].(map[string]interface{})
				if data == nil {
					t.Fatalf("expected list data object, got: %s", w.Body.String())
				}
				if _, ok := data["items"]; !ok {
					t.Fatalf("expected items in list response, got: %s", w.Body.String())
				}
				if page, _ := data["page"].(float64); page != 1 {
					t.Fatalf("expected page 1 in list response, got: %s", w.Body.String())
				}
			}
		})
	}
}
