package dashboard

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeSkillFixture creates <dataDir>/skills/<name>/SKILL.md plus one extra
// file, mirroring a real local skill dir for archive tests.
func writeSkillFixture(t *testing.T, dataDir, name string) {
	t.Helper()
	dir := filepath.Join(dataDir, "skills", name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte("# "+name+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "aux.txt"), []byte("data\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}

// assertSkillZipBody validates that an archive response body is a zip carrying
// exactly SKILL.md and aux.txt with the expected content.
func assertSkillZipBody(t *testing.T, body []byte) {
	t.Helper()
	zr, err := zip.NewReader(bytes.NewReader(body), int64(len(body)))
	if err != nil {
		t.Fatalf("响应不是有效 zip: %v", err)
	}
	files := map[string]string{}
	for _, f := range zr.File {
		rc, err := f.Open()
		if err != nil {
			t.Fatalf("打开 zip 条目 %s: %v", f.Name, err)
		}
		data, _ := io.ReadAll(rc)
		_ = rc.Close()
		files[f.Name] = string(data)
	}
	if got := files["SKILL.md"]; got == "" {
		t.Fatalf("zip 缺少 SKILL.md, 条目=%v", files)
	}
	if got := files["aux.txt"]; got != "data\n" {
		t.Fatalf("aux.txt 内容异常: %q", got)
	}
}

// TestSkillArchiveFlat: GET /api/v1/skills/archive?skill_name=hello 返回可回导
// 入的 zip（扁平查询形态，前端 downloadSkillByName）。
func TestSkillArchiveFlat(t *testing.T) {
	s := &Server{dataDir: t.TempDir()}
	writeSkillFixture(t, s.dataDir, "hello")

	req := httptest.NewRequest(http.MethodGet, "/api/v1/skills/archive?skill_name=hello", nil)
	w := httptest.NewRecorder()
	s.handleSkills(w, req, []string{"archive"})

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", w.Code, w.Body.String())
	}
	if ct := w.Header().Get("Content-Type"); ct != "application/zip" {
		t.Fatalf("Content-Type = %q, want application/zip", ct)
	}
	if cd := w.Header().Get("Content-Disposition"); !strings.Contains(cd, "hello.zip") {
		t.Fatalf("Content-Disposition = %q, want hello.zip", cd)
	}
	assertSkillZipBody(t, w.Body.Bytes())
}

// TestSkillArchivePath: GET /api/v1/skills/hello/archive 同样返回 zip（路径
// 形态，前端 downloadSkill）。
func TestSkillArchivePath(t *testing.T) {
	s := &Server{dataDir: t.TempDir()}
	writeSkillFixture(t, s.dataDir, "hello")

	req := httptest.NewRequest(http.MethodGet, "/api/v1/skills/hello/archive", nil)
	w := httptest.NewRecorder()
	s.handleSkills(w, req, []string{"hello", "archive"})

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", w.Code, w.Body.String())
	}
	if cd := w.Header().Get("Content-Disposition"); !strings.Contains(cd, "hello.zip") {
		t.Fatalf("Content-Disposition = %q, want hello.zip", cd)
	}
	assertSkillZipBody(t, w.Body.Bytes())
}

// TestSkillArchiveMissing: 不存在的技能返回 404 JSON 而非 200 占位。
func TestSkillArchiveMissing(t *testing.T) {
	s := &Server{dataDir: t.TempDir()}
	req := httptest.NewRequest(http.MethodGet, "/api/v1/skills/archive?skill_name=nope", nil)
	w := httptest.NewRecorder()
	s.handleSkills(w, req, []string{"archive"})

	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, body=%s", w.Code, w.Body.String())
	}
	var resp map[string]interface{}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("响应非 JSON: %v (%s)", err, w.Body.String())
	}
}

// TestToolArchive: GET /api/v1/tools/archive?name=<builtin> 下载工具定义 JSON；
// 未知工具返回 404。
func TestToolArchive(t *testing.T) {
	s := &Server{}

	req := httptest.NewRequest(http.MethodGet, "/api/v1/tools/archive?name=web_search_tavily", nil)
	w := httptest.NewRecorder()
	s.handleTools(w, req, []string{"archive"})
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", w.Code, w.Body.String())
	}
	if ct := w.Header().Get("Content-Type"); ct != "application/json" {
		t.Fatalf("Content-Type = %q", ct)
	}
	if cd := w.Header().Get("Content-Disposition"); !strings.Contains(cd, "web_search_tavily.json") {
		t.Fatalf("Content-Disposition = %q", cd)
	}
	var def map[string]interface{}
	if err := json.Unmarshal(w.Body.Bytes(), &def); err != nil {
		t.Fatalf("工具定义非 JSON: %v", err)
	}
	if def["name"] != "web_search_tavily" {
		t.Fatalf("name = %v, want web_search_tavily", def["name"])
	}

	req2 := httptest.NewRequest(http.MethodGet, "/api/v1/tools/archive?name=no_such_tool", nil)
	w2 := httptest.NewRecorder()
	s.handleTools(w2, req2, []string{"archive"})
	if w2.Code != http.StatusNotFound {
		t.Fatalf("未知工具 status = %d, want 404", w2.Code)
	}
}
