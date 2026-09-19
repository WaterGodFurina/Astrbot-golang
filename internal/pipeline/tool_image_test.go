package pipeline

import (
	"encoding/base64"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/WaterGodFurina/Astrbot-golang/internal/core"
)

func pngBytes() []byte {
	// 1x1 transparent PNG.
	b, _ := base64.StdEncoding.DecodeString("iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mP8z8BQDwAEhQGAhKmMIQAAAABJRU5ErkJggg==")
	return b
}

func TestExecuteFileReadImageMultimodal(t *testing.T) {
	inTempDir(t)
	ws := workspaceRoot("t:img")
	if err := os.MkdirAll(ws, 0o750); err != nil {
		t.Fatal(err)
	}
	img := filepath.Join(ws, "pic.png")
	if err := os.WriteFile(img, pngBytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	txt := filepath.Join(ws, "note.txt")
	if err := os.WriteFile(txt, []byte("hello world"), 0o600); err != nil {
		t.Fatal(err)
	}

	text, b64, mime := executeFileRead("pic.png", "t:img", 0, 0, false)
	if text != "" || b64 == "" || mime != "image/png" {
		t.Fatalf("image read mismatch: text=%q b64len=%d mime=%q", text, len(b64), mime)
	}
	if _, err := base64.StdEncoding.DecodeString(b64); err != nil {
		t.Fatalf("payload not base64: %v", err)
	}
	text, b64, mime = executeFileRead("note.txt", "t:img", 0, 0, false)
	if b64 != "" || !strings.Contains(text, "hello world") {
		t.Fatalf("text read must stay textual: text=%q b64=%q", text, b64)
	}
}

func TestRegisterAndDrainToolImages(t *testing.T) {
	inTempDir(t)
	s := &ProcessStage{}
	event := &core.Event{Source: core.EventSource{Platform: "t", ConvID: "dr"}}
	out := s.registerToolImage(event, base64.StdEncoding.EncodeToString(pngBytes()), "image/png", "astrbot_file_read_tool")
	if !strings.Contains(out, "Image returned and cached at path=") || !strings.Contains(out, "send_message_to_user") {
		t.Fatalf("register text mismatch: %q", out)
	}
	imgs := drainToolImages(event)
	if len(imgs) != 1 || (imgs[0].Mime != "image/png" && imgs[0].Mime != "image/jpeg") || imgs[0].Base64 == "" {
		t.Fatalf("drain mismatch: %+v", imgs)
	}
	defer os.Remove(imgs[0].Path)
	if again := drainToolImages(event); len(again) != 0 {
		t.Fatal("drain must clear the sink")
	}
}

func TestProviderSupportsImages(t *testing.T) {
	if !providerSupportsImages(map[string]interface{}{}) {
		t.Fatal("empty modalities must be treated as supported (py backward-compat rule)")
	}
	if !providerSupportsImages(map[string]interface{}{"modalities": []interface{}{"text", "image"}}) {
		t.Fatal("explicit image modality must pass")
	}
	if providerSupportsImages(map[string]interface{}{"modalities": []interface{}{"text"}}) {
		t.Fatal("text-only provider must reject image injection")
	}
}

func TestImageSniffMime(t *testing.T) {
	if m, ok := imageSniffMime(pngBytes()); !ok || m != "image/png" {
		t.Fatalf("png sniff failed: %q %v", m, ok)
	}
	jpeg := []byte{0xFF, 0xD8, 0xFF, 0xE0}
	if m, ok := imageSniffMime(jpeg); !ok || m != "image/jpeg" {
		t.Fatalf("jpeg sniff failed")
	}
	if _, ok := imageSniffMime([]byte("%PDF-1.5")); ok {
		t.Fatal("pdf must not match image")
	}
}

func TestWindowedFileRead(t *testing.T) {
	content := strings.Repeat("l\n", 3000)
	out := windowedFileRead(content, "Read f:", 0, 0)
	if !strings.Contains(out, "1: l") {
		t.Fatal("line numbering missing")
	}
	if !strings.Contains(out, "Showing lines 1-2000 of 3000. Use offset=2000 to continue.") {
		t.Fatalf("window hint mismatch: %q", out[len(out)-80:])
	}
	out2 := windowedFileRead(content, "Read f:", 2000, 0)
	if !strings.Contains(out2, "End of file - total 3000 lines") {
		t.Fatalf("tail window hint mismatch: %q", out2[len(out2)-60:])
	}
	if oob := windowedFileRead(content, "Read f:", 5000, 10); !strings.Contains(oob, "out of range") {
		t.Fatalf("offset guard mismatch: %q", oob)
	}
	long := strings.Repeat("x", 2500)
	if cutline := windowedFileRead(long, "Read f:", 0, 0); !strings.Contains(cutline, "line truncated to 2000 chars") {
		t.Fatal("long line must be char-capped")
	}
	// 小文件一次窗口完结（无 truncated 提示）。
	if small := windowedFileRead("a\nb", "Read f:", 0, 0); !strings.Contains(small, "End of file - total 2 lines") {
		t.Fatalf("small file mismatch: %q", small)
	}
}

func TestShellSessionPollCursor(t *testing.T) {
	dir := t.TempDir()
	log := filepath.Join(dir, "session.log")
	s := &shellSession{ID: "sh_test", OutputFile: log, Owner: "t:c\x00u"}
	shellSessionsMu.Lock()
	shellSessions["sh_test"] = s
	shellSessionsMu.Unlock()
	defer func() {
		shellSessionsMu.Lock()
		delete(shellSessions, "sh_test")
		shellSessionsMu.Unlock()
	}()
	os.WriteFile(log, []byte("line1\nline2\nline3\n"), 0o600)
	out1 := shellSessionPoll("sh_test", "t:c", "u")
	if !strings.Contains(out1, "line1") || !strings.Contains(out1, "line3") {
		t.Fatalf("first poll must carry full content: %q", out1)
	}
	if strings.Contains(out1, "output continues") {
		t.Fatalf("fully consumed log must not claim continuation: %q", out1)
	}
	// Append more: second poll delivers ONLY the new segment (cursor semantics, no replay).
	f, _ := os.OpenFile(log, os.O_APPEND|os.O_WRONLY, 0o600)
	f.WriteString("line4\nline5\n")
	f.Close()
	out2 := shellSessionPoll("sh_test", "t:c", "u")
	if strings.Contains(out2, "line1") || !strings.Contains(out2, "line4") {
		t.Fatalf("second poll must resume at cursor: %q", out2)
	}
}

func TestSpoolWriterFullOutputKept(t *testing.T) {
	dir := t.TempDir()
	w := &spoolWriter{max: 10, dir: dir}
	w.Write([]byte("0123456789abcdefg"))
	head := string(w.Head())
	path := w.Close()
	if head != "0123456789" {
		t.Fatalf("head cap mismatch: %q", head)
	}
	if path == "" {
		t.Fatal("overflow must have spooled to a file")
	}
	full, err := os.ReadFile(path)
	if err != nil || string(full) != "0123456789abcdefg" {
		t.Fatalf("spool must contain the FULL output: %q %v", full, err)
	}
	// Below cap: no spool file at all.
	w2 := &spoolWriter{max: 100, dir: dir}
	w2.Write([]byte("small"))
	if p := w2.Close(); p != "" {
		t.Fatalf("small output must not spool, got %q", p)
	}
}
