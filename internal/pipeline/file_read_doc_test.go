package pipeline

import (
	"archive/zip"
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func buildDocx(t *testing.T, body string) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	f, _ := zw.Create("[Content_Types].xml")
	f.Write([]byte(`<?xml version="1.0" encoding="UTF-8"?><Types xmlns="http://schemas.openxmlformats.org/package/2006/content-types"><Default Extension="xml" ContentType="application/xml"/><Override PartName="/word/document.xml" ContentType="application/vnd.openxmlformats-officedocument.wordprocessingml.document.main+xml"/></Types>`))
	d, _ := zw.Create("word/document.xml")
	d.Write([]byte(`<?xml version="1.0" encoding="UTF-8"?><w:document xmlns:w="http://schemas.openxmlformats.org/wordprocessingml/2006/main"><w:body><w:p><w:r><w:t>` + body + `</w:t></w:r></w:p></w:body></w:document>`))
	zw.Close()
	return buf.Bytes()
}

func TestExecuteFileReadDocx(t *testing.T) {
	inTempDir(t)
	ws := workspaceRoot("t:doc")
	if err := os.MkdirAll(ws, 0o750); err != nil {
		t.Fatal(err)
	}
	docx := filepath.Join(ws, "report.docx")
	if err := os.WriteFile(docx, buildDocx(t, "quarterly revenue up 42 percent"), 0o600); err != nil {
		t.Fatal(err)
	}
	text, b64, mime := executeFileRead("report.docx", "t:doc", 0, 0, false)
	if b64 != "" || mime != "" {
		t.Fatalf("docx must return extracted text, got image channel")
	}
	if !strings.Contains(text, "quarterly revenue up 42 percent") {
		t.Fatalf("extracted text mismatch: %q", text)
	}
	if !strings.Contains(text, "Extracted text from") {
		t.Fatalf("missing extracted-text header: %q", text)
	}
}

func TestExecuteFileReadOversizedDocConvertsToWorkspace(t *testing.T) {
	inTempDir(t)
	ws := workspaceRoot("t:big")
	if err := os.MkdirAll(ws, 0o750); err != nil {
		t.Fatal(err)
	}
	body := strings.Repeat("long paragraph for conversion threshold. \n", 40000)
	docx := filepath.Join(ws, "big.docx")
	if err := os.WriteFile(docx, buildDocx(t, body), 0o600); err != nil {
		t.Fatal(err)
	}
	if int64(len(body)) <= maxFileReadBytes {
		t.Skip("test body not large enough")
	}
	text, _, _ := executeFileRead("big.docx", "t:big", 0, 0, false)
	if !strings.Contains(text, "Converted text was saved to") {
		t.Fatalf("oversized doc must store converted file: %q", text[:min(len(text), 200)])
	}
	i := strings.Index(text, "`")
	j := strings.Index(text[i+1:], "`")
	convPath := text[i+1 : i+1+j]
	data, err := os.ReadFile(convPath)
	if err != nil {
		t.Fatalf("converted file missing: %v", err)
	}
	if !strings.Contains(string(data), "long paragraph for conversion threshold") {
		t.Fatal("converted content mismatch")
	}
	text2, _, _ := executeFileRead("big.docx", "t:big", 0, 5, false)
	if !strings.Contains(text2, "long paragraph") || !strings.Contains(text2, "Full converted text is also available at") {
		t.Fatalf("windowed oversized read mismatch: %q", text2[:min(len(text2), 200)])
	}
}

func TestExecuteFileReadBinaryGuard(t *testing.T) {
	inTempDir(t)
	ws := workspaceRoot("t:bin")
	if err := os.MkdirAll(ws, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ws, "raw.dat"), []byte("abc\x00def"), 0o600); err != nil {
		t.Fatal(err)
	}
	text, b64, _ := executeFileRead("raw.dat", "t:bin", 0, 0, false)
	if b64 != "" || !strings.Contains(text, "binary files are not supported") {
		t.Fatalf("binary guard mismatch: %q", text)
	}
}

func TestExecuteFileReadFakeDocxFallsToBinary(t *testing.T) {
	inTempDir(t)
	ws := workspaceRoot("t:fake")
	if err := os.MkdirAll(ws, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ws, "fake.docx"), []byte("not a real zip\x00"), 0o600); err != nil {
		t.Fatal(err)
	}
	text, _, _ := executeFileRead("fake.docx", "t:fake", 0, 0, false)
	if !strings.Contains(text, "binary files are not supported") {
		t.Fatalf("undecodable docx must report binary rejection: %q", text)
	}
}
