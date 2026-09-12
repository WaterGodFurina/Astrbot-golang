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
