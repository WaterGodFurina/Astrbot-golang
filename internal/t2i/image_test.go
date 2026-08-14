package t2i

import (
	"bytes"
	"fmt"
	"strings"
	"sync"

	"github.com/fogleman/gg"
	"image"
	"image/color"
	"image/png"
	"testing"
)

func TestRenderTextToPNGChinese(t *testing.T) {
	text := "这是一段用于测试中文字符自动换行的长文本。" +
		"北京铁路局今天凌晨发布消息称，京沪高铁廊坊至北京南间发生设备故障，导致部分列车晚点。" +
		"This is an English sentence that should wrap at word boundaries correctly when the line is long enough."
	data, err := RenderTextToPNG(text, ImageOptions{FontPath: "/usr/share/fonts/truetype/droid/DroidSansFallbackFull.ttf"})
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	img, err := png.Decode(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	b := img.Bounds()
	if b.Dx() <= 0 || b.Dy() <= 0 {
		t.Fatalf("invalid bounds: %v", b)
	}
	// Non-background pixels must exist (text was actually drawn).
	if !hasNonBackgroundPixel(img, color.RGBA{255, 255, 255, 255}) {
		t.Fatal("image contains no text pixels")
	}
	t.Logf("rendered %dx%d", b.Dx(), b.Dy())
}

func TestImageRendererMixedContent(t *testing.T) {
	r, err := NewImageRenderer(ImageOptions{
		Title:    "测试标题",
		FontPath: "/usr/share/fonts/truetype/droid/DroidSansFallbackFull.ttf",
	})
	if err != nil {
		t.Fatalf("new renderer: %v", err)
	}
	if err := r.AddText("第一行文本，用于测试图文混排的效果。", 0, color.RGBA{0, 0, 0, 255}, "left"); err != nil {
		t.Fatalf("add text: %v", err)
	}
	img := image.NewRGBA(image.Rect(0, 0, 200, 100))
	for x := 0; x < 200; x++ {
		for y := 0; y < 100; y++ {
			img.Set(x, y, color.RGBA{0, 128, 255, 255})
		}
	}
	r.AddImage(img)
	if err := r.AddText("图片之后的第二段文字。", 0, color.RGBA{0, 0, 0, 255}, "center"); err != nil {
		t.Fatalf("add text 2: %v", err)
	}
	data, err := r.RenderPNG()
	if err != nil {
		t.Fatalf("render png: %v", err)
	}
	decoded, err := png.Decode(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if decoded.Bounds().Dy() <= 100 {
		t.Fatalf("expected height > picture height, got %d", decoded.Bounds().Dy())
	}
	t.Logf("mixed image height=%d", decoded.Bounds().Dy())
}

func TestEmojiWrappingAndCodepoint(t *testing.T) {
	// Grapheme-level wrapping keeps an emoji sequence on one line and assigns
	// it a font-size width.
	r, err := NewImageRenderer(ImageOptions{FontPath: "/usr/share/fonts/truetype/droid/DroidSansFallbackFull.ttf"})
	if err != nil {
		t.Fatalf("new renderer: %v", err)
	}
	mdc := gg.NewContext(1, 1)
	face, _ := cachedFontFace(r.font, r.opts.FontSize)
	mdc.SetFontFace(face)
	lines := r.wrapByGraphemes(mdc, "你好😂世界", 200, r.opts.FontSize)
	if len(lines) == 0 {
		t.Fatal("no lines")
	}
	// Ensure the emoji survives as its own cluster in the output.
	joined := strings.Join(lines, "")
	if !strings.Contains(joined, "😂") {
		t.Fatalf("emoji lost during wrap: %q", joined)
	}

	if cp := emojiCodepoint("😂"); cp != "1f602" {
		t.Fatalf("emojiCodepoint(😂) = %q, want 1f602", cp)
	}
	// ZWJ family sequence keeps all codepoints (variation selectors dropped).
	if cp := emojiCodepoint("👨\u200d👩\u200d👧"); cp != "1f468-200d-1f469-200d-1f467" {
		t.Fatalf("ZWJ codepoint = %q", cp)
	}
	if !isEmojiCluster("👨\u200d👩\u200d👧") {
		t.Fatal("ZWJ family should be an emoji cluster")
	}
	// Fonts without a glyph for a codepoint must be routed to the image path.
	if fontHasGlyph(r.font, '中') != true {
		t.Fatal("CJK font should have a glyph for 中")
	}
	if fontHasGlyph(r.font, '😀') {
		t.Fatal("CJK fallback font should NOT have a glyph for emoji")
	}
}

func TestRenderTextWithEmoji(t *testing.T) {
	text := "这是一段包含表情的文本：😂 哈哈 👨\u200d👩\u200d👧 家庭 👋🏻 再见，换行也要正常。"
	data, err := RenderTextToPNG(text, ImageOptions{FontPath: "/usr/share/fonts/truetype/droid/DroidSansFallbackFull.ttf"})
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	img, err := png.Decode(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if img.Bounds().Dy() <= 0 {
		t.Fatal("empty image")
	}
	t.Logf("emoji image %dx%d (%d bytes)", img.Bounds().Dx(), img.Bounds().Dy(), len(data))
}

func TestConcurrentRenderNoRace(t *testing.T) {
	// Rendering must be safe across goroutines: the shared font cache holds a
	// concurrency-safe *truetype.Font and each render creates its own face.
	const n = 8
	var wg sync.WaitGroup
	errs := make([]error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, err := RenderTextToPNG(fmt.Sprintf("并发渲染测试 %d", i), ImageOptions{FontPath: "/usr/share/fonts/truetype/droid/DroidSansFallbackFull.ttf"})
			errs[i] = err
		}(i)
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("render %d: %v", i, err)
		}
	}
}

func hasNonBackgroundPixel(img image.Image, bg color.RGBA) bool {
	b := img.Bounds()
	for y := b.Min.Y; y < b.Max.Y; y++ {
		for x := b.Min.X; x < b.Max.X; x++ {
			r, g, bl, _ := img.At(x, y).RGBA()
			if r>>8 != uint32(bg.R) || g>>8 != uint32(bg.G) || bl>>8 != uint32(bg.B) {
				return true
			}
		}
	}
	return false
}
