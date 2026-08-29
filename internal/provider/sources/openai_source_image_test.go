package sources

import (
	"encoding/base64"
	"os"
	"path/filepath"
	"testing"
)

// TestImageRefToDataURL 锁定 openai_source.go 图片引用解析（astrbot-py 对齐）：
// 本地路径与 file:// 引用 → inline data URI；data: 透传；坏引用返回空。
func TestImageRefToDataURL(t *testing.T) {
	raw := []byte("fake-png-bytes")
	dir := t.TempDir()
	png := filepath.Join(dir, "img.png")
	if err := os.WriteFile(png, raw, 0o644); err != nil {
		t.Fatal(err)
	}
	want := "data:image/png;base64," + base64.StdEncoding.EncodeToString(raw)

	for _, ref := range []string{png, "file://" + png} {
		if got := imageRefToDataURL(ref); got != want {
			t.Fatalf("imageRefToDataURL(%q) = %q, want %q", ref, got, want)
		}
	}
	if got := imageRefToDataURL(want); got != want {
		t.Fatalf("data URI 应原样透传, got %q", got)
	}
	if got := imageRefToDataURL(filepath.Join(dir, "missing.png")); got != "" {
		t.Fatalf("缺失引用应返回空, got %q", got)
	}
}

// TestResolveOpenAIImageParts 锁定 messages 内容块的解析：可解析的 image_url
// 转为 data URI，不可解析的块被丢弃，文字块保留。
func TestResolveOpenAIImageParts(t *testing.T) {
	raw := []byte("png-bytes")
	dir := t.TempDir()
	png := filepath.Join(dir, "img.png")
	if err := os.WriteFile(png, raw, 0o644); err != nil {
		t.Fatal(err)
	}
	want := "data:image/png;base64," + base64.StdEncoding.EncodeToString(raw)

	userMsg := map[string]interface{}{
		"role": "user",
		"content": []map[string]interface{}{
			{"type": "text", "text": "[图片]"},
			{"type": "image_url", "image_url": map[string]interface{}{"url": png}},
			{"type": "image_url", "image_url": map[string]interface{}{"url": "file://" + png}},
			{"type": "image_url", "image_url": map[string]interface{}{"url": filepath.Join(dir, "missing.png")}},
		},
	}
	resolveOpenAIImageParts([]map[string]interface{}{userMsg})

	parts, ok := userMsg["content"].([]map[string]interface{})
	if !ok {
		t.Fatalf("content 类型异常: %T", userMsg["content"])
	}
	if len(parts) != 3 {
		t.Fatalf("content len = %d, want 3: %#v", len(parts), parts)
	}
	var got []string
	for _, p := range parts {
		if p["type"] == "image_url" {
			u, _ := p["image_url"].(map[string]interface{})["url"].(string)
			got = append(got, u)
		}
	}
	if len(got) != 2 || got[0] != want || got[1] != want {
		t.Fatalf("image urls = %v, want [%s %s]", got, want, want)
	}
}
