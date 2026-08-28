// kb_pagination 回归测试：
//  1. 文档列表分页 —— kbDocList 必须尊重 page/page_size 查询参数（默认
//     page_size=10），不能再把所有文档一页返回。
//  2. 分块列表按文档过滤 —— chunks 接口读取 document_id（前端参数名）而非
//     仅 doc_id，确保"点击单个文档的分块只显示该文档的分块"。
package dashboard

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/WaterGodFurina/Astrbot-golang/internal/db"
)

// newTestServerWithDB 构造一个带内存 SQLite 数据库的 dashboard server（隔离
// 于独立临时目录，避免污染真实 data）。
func newTestServerWithDB(t *testing.T) *Server {
	t.Helper()
	dir := t.TempDir()
	database, err := db.New(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatalf("db.New: %v", err)
	}
	t.Cleanup(func() { _ = database.Close() })
	s := NewServerWithManagers(0, filepath.Join(dir, "cmd_config.json"), map[string]interface{}{
		"database": database,
	})
	return s
}

// seedKBDocs 在 KB 数据目录创建 n 个文档文件（doc_id 由 kbDocList 按文件
// modtime 计算，测试无需自行构造）。
func seedKBDocs(t *testing.T, s *Server, kbID string, n int) {
	t.Helper()
	dir := filepath.Join(s.kbDataDir(), "knowledge_bases", kbID, "documents")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir documents: %v", err)
	}
	for i := 0; i < n; i++ {
		name := "doc_" + string(rune('a'+i%26)) + ".md"
		if i >= 26 {
			name = "doc_" + string(rune('0'+i/26)) + string(rune('a'+i%26)) + ".md"
		}
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte("content"), 0o644); err != nil {
			t.Fatalf("write doc: %v", err)
		}
	}
}

func TestKBDocListPagination(t *testing.T) {
	s := newTestServerWithDB(t)
	const kbID = "kb_1"
	seedKBDocs(t, s, kbID, 15) // 15 个文档，超过默认 page_size=10

	req := httptest.NewRequest(http.MethodGet, "/api/knowledge-bases/kb_1/documents?page=1&page_size=10", nil)
	w := httptest.NewRecorder()
	s.handleKBDocuments(w, req, kbID, nil)

	var v map[string]interface{}
	if err := json.Unmarshal(w.Body.Bytes(), &v); err != nil {
		t.Fatalf("invalid json: %v", err)
	}
	data, _ := v["data"].(map[string]interface{})
	items, _ := data["items"].([]interface{})
	if data == nil || len(items) != 10 {
		t.Fatalf("page 1 must return 10 items, got %d (%s)", len(items), w.Body.String())
	}
	if p, _ := data["total"].(float64); p != 15 {
		t.Fatalf("total must be 15, got %v", data["total"])
	}
	if ps, _ := data["page_size"].(float64); ps != 10 {
		t.Fatalf("page_size must be 10, got %v", data["page_size"])
	}

	// 第 2 页返回剩余 5 个。
	req2 := httptest.NewRequest(http.MethodGet, "/api/knowledge-bases/kb_1/documents?page=2&page_size=10", nil)
	w2 := httptest.NewRecorder()
	s.handleKBDocuments(w2, req2, kbID, nil)
	var v2 map[string]interface{}
	_ = json.Unmarshal(w2.Body.Bytes(), &v2)
	data2, _ := v2["data"].(map[string]interface{})
	items2, _ := data2["items"].([]interface{})
	if len(items2) != 5 {
		t.Fatalf("page 2 must return 5 items, got %d (%s)", len(items2), w2.Body.String())
	}
}

func TestKBChunksFilterByDocumentID(t *testing.T) {
	s := newTestServerWithDB(t)
	const kbID = "kb_2"
	// 两个文档各 3 个分块。
	for _, doc := range []struct{ id, name string }{
		{"docA", "a.md"},
		{"docB", "b.md"},
	} {
		for i := 0; i < 3; i++ {
			err := s.database.InsertKBChunk(db.KBChunk{
				ChunkID:  doc.id + "_" + string(rune('0'+i)),
				KBID:     kbID,
				DocID:    doc.id,
				DocName:  doc.name,
				Content:  doc.name + " chunk " + string(rune('0'+i)),
				ChunkIdx: i,
			})
			if err != nil {
				t.Fatalf("InsertKBChunk: %v", err)
			}
		}
	}

	// 前端传 document_id（见 DocumentDetail.vue），必须只返回该文档的分块。
	req := httptest.NewRequest(http.MethodGet, "/api/knowledge-bases/kb_2/chunks?document_id=docA&page=1&page_size=10", nil)
	w := httptest.NewRecorder()
	s.handleKB(w, req, []string{kbID, "chunks"})

	var v map[string]interface{}
	if err := json.Unmarshal(w.Body.Bytes(), &v); err != nil {
		t.Fatalf("invalid json: %v", err)
	}
	data, _ := v["data"].(map[string]interface{})
	if data == nil {
		t.Fatalf("expected data, got %s", w.Body.String())
	}
	if total, _ := data["total"].(float64); total != 3 {
		t.Fatalf("document_id filter must return 3 chunks for docA, got total=%v (%s)", total, w.Body.String())
	}
	items, _ := data["items"].([]interface{})
	for _, it := range items {
		m, _ := it.(map[string]interface{})
		if did, _ := m["doc_id"].(string); did != "docA" {
			t.Fatalf("chunk from wrong doc: %v", m)
		}
	}
}
