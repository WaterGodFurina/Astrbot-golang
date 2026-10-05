package utils

import (
	"bytes"
	"context"
	"errors"

	"image"
	"image/color"
	"image/gif"
	"image/jpeg"
	"image/png"
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

// ---------- 测试工具 ----------

func encodePNG(t *testing.T, img image.Image) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatalf("png encode: %v", err)
	}
	return buf.Bytes()
}

func encodeJPEG(t *testing.T, img image.Image, quality int) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: quality}); err != nil {
		t.Fatalf("jpeg encode: %v", err)
	}
	return buf.Bytes()
}

func decodeImage(t *testing.T, data []byte) image.Image {
	t.Helper()
	img, _, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	return img
}

// near 判断两色是否接近（JPEG 有损，容差 24）。
func near(a, b color.RGBA) bool {
	diff := func(x, y uint8) bool {
		d := int(x) - int(y)
		return d <= 24 && d >= -24
	}
	return diff(a.R, b.R) && diff(a.G, b.G) && diff(a.B, b.B)
}

func sampleRGBA(t *testing.T, img image.Image, x, y int) color.RGBA {
	t.Helper()
	r, g, b, a := img.At(x, y).RGBA()
	return color.RGBA{R: uint8(r >> 8), G: uint8(g >> 8), B: uint8(b >> 8), A: uint8(a >> 8)}
}

var (
	red     = color.RGBA{R: 220, A: 255}
	green   = color.RGBA{G: 200, A: 255}
	blue    = color.RGBA{B: 220, A: 255}
	yellow  = color.RGBA{R: 220, G: 200, A: 255}
	white   = color.RGBA{R: 255, G: 255, B: 255, A: 255}
	nearWht = func(c color.RGBA) bool { return c.R > 230 && c.G > 230 && c.B > 230 }
)

// buildGIF 按帧/处置方式编码 GIF；palette[0] 固定为白色并作为 BackgroundIndex，
// 使 "背景恢复" 与 Go 实现的 "清为透明后压白底" 两种语义结果一致。
func buildGIF(t *testing.T, size image.Point, colors []color.RGBA, disposals []byte, rects []image.Rectangle) []byte {
	t.Helper()
	pal := color.Palette{white, red, green, blue, yellow}
	g := &gif.GIF{
		Config:          image.Config{ColorModel: pal, Width: size.X, Height: size.Y},
		BackgroundIndex: 0,
	}
	for i, c := range colors {
		rect := image.Rect(0, 0, size.X, size.Y)
		if rects != nil {
			rect = rects[i]
		}
		frame := image.NewPaletted(rect, pal)
		idx := uint8(1)
		for j, pc := range pal {
			if pc == c {
				idx = uint8(j)
				break
			}
		}
		for y := rect.Min.Y; y < rect.Max.Y; y++ {
			for x := rect.Min.X; x < rect.Max.X; x++ {
				frame.SetColorIndex(x, y, idx)
			}
		}
		g.Image = append(g.Image, frame)
		g.Delay = append(g.Delay, 10)
		g.Disposal = append(g.Disposal, disposals[i])
	}
	var buf bytes.Buffer
	if err := gif.EncodeAll(&buf, g); err != nil {
		t.Fatalf("gif encode: %v", err)
	}
	return buf.Bytes()
}

// ---------- EXIF 方向 ----------

// labeled3x2 生成 3x2 唯一色块图：A B C / D E F。
func labeled3x2() *image.NRGBA {
	img := image.NewNRGBA(image.Rect(0, 0, 3, 2))
	pal := []color.NRGBA{
		{R: 10, G: 10, B: 10, A: 255}, {R: 60, G: 60, B: 60, A: 255}, {R: 110, G: 110, B: 110, A: 255},
		{R: 160, G: 160, B: 160, A: 255}, {R: 210, G: 210, B: 210, A: 255}, {R: 250, G: 250, B: 250, A: 255},
	}
	for i := 0; i < 6; i++ {
		img.SetNRGBA(i%3, i/3, pal[i])
	}
	return img
}

// TestApplyEXIFOrientation 以硬编码期望矩阵验证 8 种方向的旋转/镜像。
func TestApplyEXIFOrientation(t *testing.T) {
	// A B C / D E F 的灰度值
	a, b, c := uint8(10), uint8(60), uint8(110)
	d, e, f := uint8(160), uint8(210), uint8(250)
	cases := []struct {
		orientation int
		w, h        int
		want        [][]uint8 // [y][x]
	}{
		{1, 3, 2, [][]uint8{{a, b, c}, {d, e, f}}},
		{2, 3, 2, [][]uint8{{c, b, a}, {f, e, d}}},
		{3, 3, 2, [][]uint8{{f, e, d}, {c, b, a}}},
		{4, 3, 2, [][]uint8{{d, e, f}, {a, b, c}}},
		{5, 2, 3, [][]uint8{{a, d}, {b, e}, {c, f}}},
		{6, 2, 3, [][]uint8{{d, a}, {e, b}, {f, c}}},
		{7, 2, 3, [][]uint8{{f, c}, {e, b}, {d, a}}},
		{8, 2, 3, [][]uint8{{c, f}, {b, e}, {a, d}}},
	}
	src := labeled3x2()
	for _, tc := range cases {
		out := applyEXIFOrientation(src, tc.orientation)
		b := out.Bounds()
		if b.Dx() != tc.w || b.Dy() != tc.h {
			t.Fatalf("orientation %d: size=%dx%d want %dx%d", tc.orientation, b.Dx(), b.Dy(), tc.w, tc.h)
		}
		for y := 0; y < tc.h; y++ {
			for x := 0; x < tc.w; x++ {
				r, _, _, _ := out.At(x, y).RGBA()
				got := uint8(r >> 8)
				if got != tc.want[y][x] {
					t.Fatalf("orientation %d pixel(%d,%d)=%d want %d", tc.orientation, x, y, got, tc.want[y][x])
				}
			}
		}
	}
	// 非法方向原样返回。
	if got := applyEXIFOrientation(src, 0); got != image.Image(src) {
		t.Fatal("orientation 0 should return original image")
	}
}

// buildTIFF 构造含方向标签(274)的最小 TIFF。
func buildTIFF(little bool, orientation uint16) []byte {
	buf := new(bytes.Buffer)
	if little {
		buf.WriteString("II")
	} else {
		buf.WriteString("MM")
	}
	u16 := func(v uint16) {
		if little {
			buf.Write([]byte{byte(v), byte(v >> 8)})
		} else {
			buf.Write([]byte{byte(v >> 8), byte(v)})
		}
	}
	u32 := func(v uint32) {
		if little {
			buf.Write([]byte{byte(v), byte(v >> 8), byte(v >> 16), byte(v >> 24)})
		} else {
			buf.Write([]byte{byte(v >> 24), byte(v >> 16), byte(v >> 8), byte(v)})
		}
	}
	u16(42)
	u32(8) // IFD offset
	u16(1) // entry count
	u16(0x0112)
	u16(3) // SHORT
	u32(1) // count
	u16(orientation)
	u16(0)
	u32(0) // next IFD
	return buf.Bytes()
}

// wrapJPEGWithEXIF 在 SOI 后插入 APP1 Exif 段。
func wrapJPEGWithEXIF(t *testing.T, jpegBytes []byte, tiff []byte) []byte {
	t.Helper()
	payload := append([]byte("Exif\x00\x00"), tiff...)
	segLen := len(payload) + 2
	seg := []byte{0xff, 0xe1, byte(segLen >> 8), byte(segLen & 0xff)}
	var out bytes.Buffer
	out.Write(jpegBytes[:2]) // SOI
	out.Write(seg)
	out.Write(payload)
	out.Write(jpegBytes[2:])
	return out.Bytes()
}

func TestJPEGAndTIFFOrientation(t *testing.T) {
	for _, little := range []bool{true, false} {
		for o := 1; o <= 8; o++ {
			if got := tiffOrientation(buildTIFF(little, uint16(o))); got != o {
				t.Fatalf("tiffOrientation(little=%v, %d)=%d", little, o, got)
			}
		}
	}
	// 畸形/越界 → 1
	if got := tiffOrientation([]byte("II\x2a\x00")); got != 1 {
		t.Fatalf("short tiff=%d", got)
	}
	if got := tiffOrientation(buildTIFF(true, 9)); got != 1 {
		t.Fatalf("out-of-range orientation=%d", got)
	}
	if got := tiffOrientation([]byte("XX\x2a\x00\x08\x00\x00\x00\x00\x00")); got != 1 {
		t.Fatalf("bad byte order=%d", got)
	}
	// JPEG 包装 + 缺失/非 JPEG
	base := encodeJPEG(t, labeled3x2(), 90)
	if got := jpegEXIFOrientation(base); got != 1 {
		t.Fatalf("plain jpeg orientation=%d", got)
	}
	for o := 1; o <= 8; o++ {
		wrapped := wrapJPEGWithEXIF(t, base, buildTIFF(true, uint16(o)))
		if got := jpegEXIFOrientation(wrapped); got != o {
			t.Fatalf("jpeg exif orientation %d -> %d", o, got)
		}
	}
	if got := jpegEXIFOrientation([]byte("not a jpeg")); got != 1 {
		t.Fatalf("non-jpeg=%d", got)
	}
	// 端到端：带方向 6 的 JPEG 经 convert 后方向被应用且清掉 EXIF 方向。
	wrapped := wrapJPEGWithEXIF(t, encodeJPEG(t, labeled3x2(), 90), buildTIFF(true, 6))
	out, err := convertImageBytesSync(wrapped, 1000, 90)
	if err != nil {
		t.Fatalf("convert rotated jpeg: %v", err)
	}
	if got := jpegEXIFOrientation(out); got != 1 {
		t.Fatalf("output retains orientation=%d", got)
	}
	img := decodeImage(t, out)
	if img.Bounds().Dx() != 2 || img.Bounds().Dy() != 3 {
		t.Fatalf("rotated size=%dx%d want 2x3", img.Bounds().Dx(), img.Bounds().Dy())
	}
}

// ---------- alpha / 压白底 ----------

func TestFlattenAndAlpha(t *testing.T) {
	img := image.NewNRGBA(image.Rect(0, 0, 2, 2))
	img.SetNRGBA(0, 0, color.NRGBA{R: red.R, A: 255}) // 不透明红（与 red 期望一致）
	// (1,0) 透明
	flat := flattenOntoWhite(img)
	if got := sampleRGBA(t, flat, 0, 0); !near(got, red) {
		t.Fatalf("opaque pixel=%v", got)
	}
	if got := sampleRGBA(t, flat, 1, 0); !nearWht(got) {
		t.Fatalf("transparent pixel should flatten to white, got %v", got)
	}
	if !imageHasAlpha(img) {
		t.Fatal("NRGBA must report alpha")
	}
	if imageHasAlpha(image.NewGray(image.Rect(0, 0, 1, 1))) {
		t.Fatal("Gray must not report alpha")
	}
	pal := color.Palette{color.RGBA{R: 255, A: 255}, color.RGBA{}} // index1 透明
	if !imageHasAlpha(image.NewPaletted(image.Rect(0, 0, 1, 1), pal)) {
		t.Fatal("palette with transparent entry must report alpha")
	}
}

// TestEncodeImageFrameBytes 验证 alpha→PNG、非 alpha→JPEG、缩放。
func TestEncodeImageFrameBytes(t *testing.T) {
	alpha := image.NewNRGBA(image.Rect(0, 0, 4, 4))
	for i := 0; i < 4; i++ {
		alpha.SetNRGBA(i, 0, color.NRGBA{R: 255, A: 128})
	}
	out, err := encodeImageFrameBytes(alpha, 1, 100, 90)
	if err != nil {
		t.Fatal(err)
	}
	if _, format, _ := image.DecodeConfig(bytes.NewReader(out)); format != "png" {
		t.Fatalf("alpha image should encode png, got %s", format)
	}
	gray := image.NewGray(image.Rect(0, 0, 8, 4))
	out, err = encodeImageFrameBytes(gray, 1, 4, 90)
	if err != nil {
		t.Fatal(err)
	}
	img := decodeImage(t, out)
	if _, format, _ := image.DecodeConfig(bytes.NewReader(out)); format != "jpeg" {
		t.Fatalf("opaque should encode jpeg, got %s", format)
	}
	if img.Bounds().Dx() != 4 || img.Bounds().Dy() != 2 {
		t.Fatalf("scaleDown size=%dx%d want 4x2", img.Bounds().Dx(), img.Bounds().Dy())
	}
}

// ---------- 直通 / 缓存 ----------

func TestConvertImageBytesPassthroughAndCache(t *testing.T) {
	t.Setenv("ASTRBOT_DATA_PATH", t.TempDir())
	// 合规 JPEG（无 EXIF、尺寸小）→ 原样返回。
	base := encodeJPEG(t, labeled3x2(), 90)
	out, err := convertImageBytesSync(base, 1280, 95)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(out, base) {
		t.Fatal("compliant jpeg should pass through unchanged")
	}
	// 超限图片 → 缩放并写缓存；二次调用命中缓存结果一致。
	big := image.NewRGBA(image.Rect(0, 0, 64, 32))
	for y := 0; y < 32; y++ {
		for x := 0; x < 64; x++ {
			big.SetRGBA(x, y, color.RGBA{R: uint8(x * 4), B: uint8(y * 8), A: 255})
		}
	}
	src := encodeJPEG(t, big, 95)
	first, err := convertImageBytesSync(src, 16, 95)
	if err != nil {
		t.Fatal(err)
	}
	if firstImg := decodeImage(t, first); firstImg.Bounds().Dx() > 16 || firstImg.Bounds().Dy() > 16 {
		t.Fatalf("oversized not scaled: %v", firstImg.Bounds())
	}
	second, err := convertImageBytesSync(src, 16, 95)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(first, second) {
		t.Fatal("cache hit should return identical bytes")
	}
	// 损坏缓存 → miss 并自愈重算。
	entries, err := os.ReadDir(imageConvertCacheDir())
	if err != nil || len(entries) == 0 {
		t.Fatalf("cache dir empty: %v", err)
	}
	target := filepath.Join(imageConvertCacheDir(), entries[0].Name())
	if err := os.WriteFile(target, []byte("broken"), 0o600); err != nil {
		t.Fatal(err)
	}
	third, err := convertImageBytesSync(src, 16, 95)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(first, third) {
		t.Fatal("corrupted cache should be regenerated")
	}
}

// ---------- GIF montage / disposal ----------

// montageTileCenter 返回第 idx 个 3x3 tile 的中心像素（canvas 已解码为 JPEG）。
func montageTileCenter(t *testing.T, canvas image.Image, idx int) color.RGBA {
	t.Helper()
	cellW := canvas.Bounds().Dx() / AnimatedMontageGrid
	cellH := canvas.Bounds().Dy() / AnimatedMontageGrid
	x := (idx%AnimatedMontageGrid)*cellW + cellW/2
	y := (idx/AnimatedMontageGrid)*cellH + cellH/2
	return sampleRGBA(t, canvas, x, y)
}

func TestExtractAnimationMontageDisposal(t *testing.T) {
	t.Setenv("ASTRBOT_DATA_PATH", t.TempDir())

	// A) 三帧纯色 DisposalNone：tile0..2 = 红/绿/蓝，其余白色。
	gifA := buildGIF(t, image.Pt(3, 3),
		[]color.RGBA{red, green, blue},
		[]byte{gif.DisposalNone, gif.DisposalNone, gif.DisposalNone}, nil)
	outA, ran, err := extractAnimationMontageSync(gifA, 30, 90)
	if err != nil || !ran {
		t.Fatalf("montage A: ran=%v err=%v", ran, err)
	}
	canvasA := decodeImage(t, outA)
	for i, want := range []color.RGBA{red, green, blue} {
		if got := montageTileCenter(t, canvasA, i); !near(got, want) {
			t.Fatalf("tile %d = %v want %v", i, got, want)
		}
	}
	if got := montageTileCenter(t, canvasA, 4); !nearWht(got) {
		t.Fatalf("unused tile should stay white, got %v", got)
	}

	// B) DisposalPrevious：f2 用 Previous，f3 画左半黄色 → 右半应恢复为 f2 前的绿色。
	gifB := buildGIF(t, image.Pt(4, 4),
		[]color.RGBA{red, green, blue, yellow},
		[]byte{gif.DisposalNone, gif.DisposalNone, gif.DisposalPrevious, gif.DisposalNone},
		[]image.Rectangle{
			image.Rect(0, 0, 4, 4), image.Rect(0, 0, 4, 4),
			image.Rect(0, 0, 4, 4), image.Rect(0, 0, 2, 4),
		})
	outB, _, err := extractAnimationMontageSync(gifB, 30, 90)
	if err != nil {
		t.Fatalf("montage B: %v", err)
	}
	canvasB := decodeImage(t, outB)
	// tile3 = f3 合成：左黄右绿（Previous 恢复）
	cellW := canvasB.Bounds().Dx() / AnimatedMontageGrid
	cellH := canvasB.Bounds().Dy() / AnimatedMontageGrid
	tile3x := (3 % AnimatedMontageGrid) * cellW
	tile3y := (3 / AnimatedMontageGrid) * cellH
	left := sampleRGBA(t, canvasB, tile3x+cellW/4, tile3y+cellH/2)
	right := sampleRGBA(t, canvasB, tile3x+3*cellW/4, tile3y+cellH/2)
	if !near(left, yellow) {
		t.Fatalf("Previous frame left = %v want yellow", left)
	}
	if !near(right, green) {
		t.Fatalf("Previous frame right = %v want restored green", right)
	}

	// C) DisposalBackground：f0 全红(Background)，f1 仅左半绿 → 右半应为背景白。
	gifC := buildGIF(t, image.Pt(4, 4),
		[]color.RGBA{red, green},
		[]byte{gif.DisposalBackground, gif.DisposalNone},
		[]image.Rectangle{image.Rect(0, 0, 4, 4), image.Rect(0, 0, 2, 4)})
	outC, _, err := extractAnimationMontageSync(gifC, 30, 90)
	if err != nil {
		t.Fatalf("montage C: %v", err)
	}
	canvasC := decodeImage(t, outC)
	cellW = canvasC.Bounds().Dx() / AnimatedMontageGrid
	cellH = canvasC.Bounds().Dy() / AnimatedMontageGrid
	tile1x := (1 % AnimatedMontageGrid) * cellW
	tile1y := 0
	if got := sampleRGBA(t, canvasC, tile1x+cellW/4, tile1y+cellH/2); !near(got, green) {
		t.Fatalf("Background frame left = %v want green", got)
	}
	if got := sampleRGBA(t, canvasC, tile1x+3*cellW/4, tile1y+cellH/2); !nearWht(got) {
		t.Fatalf("Background frame right = %v want background white (no red bleed)", got)
	}

	// D) 缓存：第二次调用返回相同结果且 ran=false。
	outA2, ran2, err := extractAnimationMontageSync(gifA, 30, 90)
	if err != nil {
		t.Fatal(err)
	}
	if ran2 || !bytes.Equal(outA, outA2) {
		t.Fatalf("montage cache miss: ran=%v equal=%v", ran2, bytes.Equal(outA, outA2))
	}
}

func TestEvenFrameIndices(t *testing.T) {
	idx := evenFrameIndices(20, 9)
	if len(idx) != 9 || idx[0] != 0 || idx[len(idx)-1] != 19 {
		t.Fatalf("indices=%v", idx)
	}
	for i := 1; i < len(idx); i++ {
		if idx[i] <= idx[i-1] {
			t.Fatalf("not strictly increasing: %v", idx)
		}
	}
	if got := evenFrameIndices(0, 9); len(got) != 1 || got[0] != 0 {
		t.Fatalf("zero frames=%v", got)
	}
	if got := evenFrameIndices(3, 9); len(got) != 3 {
		t.Fatalf("few frames=%v", got)
	}
	if got := gifDisposalAt(nil, 0); got != gif.DisposalNone {
		t.Fatalf("nil gif disposal=%v", got)
	}
}

// ---------- 校验 / MIME / 归一化 / 错误分类 ----------

func TestInspectAndDetectMime(t *testing.T) {
	img := image.NewRGBA(image.Rect(0, 0, 4, 4))
	jpegBytes := encodeJPEG(t, img, 90)
	pngBytes := encodePNG(t, img)
	gifBytes := buildGIF(t, image.Pt(2, 2), []color.RGBA{red, green, blue},
		[]byte{gif.DisposalNone, gif.DisposalNone, gif.DisposalNone}, nil)

	cases := []struct {
		name   string
		data   []byte
		mime   string
		frames int
	}{
		{"jpeg", jpegBytes, "image/jpeg", 1},
		{"png", pngBytes, "image/png", 1},
		{"gif", gifBytes, "image/gif", 3},
	}
	for _, tc := range cases {
		if got := DetectImageMimeType(tc.data); got != tc.mime {
			t.Errorf("%s mime=%q want %q", tc.name, got, tc.mime)
		}
		if n, err := InspectImage(tc.data); err != nil || n != tc.frames {
			t.Errorf("%s inspect=(%d,%v) want (%d,nil)", tc.name, n, err, tc.frames)
		}
	}
	if got := DetectImageMimeType([]byte("plain text")); got != "" {
		t.Fatalf("text mime=%q", got)
	}
	if _, err := InspectImage([]byte("plain text")); err == nil {
		t.Fatal("invalid image should fail inspect")
	}
}

func TestNormalizeModelImageMaxSizeAndErrors(t *testing.T) {
	if got := NormalizeModelImageMaxSize(nil); got != ImageCompressDefaultMaxSize {
		t.Fatalf("nil=%d", got)
	}
	if got := NormalizeModelImageMaxSize(true); got != ImageCompressDefaultMaxSize {
		t.Fatalf("bool=%d", got)
	}
	if got := NormalizeModelImageMaxSize(2); got != ImageCompressDefaultMaxSize {
		t.Fatalf("too small=%d", got)
	}
	if got := NormalizeModelImageMaxSize(1280); got != 1280 {
		t.Fatalf("int=%d", got)
	}
	if got := NormalizeModelImageMaxSize(int64(100)); got != 100 {
		t.Fatalf("int64=%d", got)
	}
	if got := NormalizeModelImageMaxSize(float64(100)); got != 100 {
		t.Fatalf("float64=%d", got)
	}
	if got := NormalizeModelImageMaxSize(1.5); got != ImageCompressDefaultMaxSize {
		t.Fatalf("fractional=%d", got)
	}
	if got := NormalizeModelImageMaxSize("200"); got != 200 {
		t.Fatalf("string=%d", got)
	}
	if got := NormalizeModelImageMaxSize("abc"); got != ImageCompressDefaultMaxSize {
		t.Fatalf("bad string=%d", got)
	}
	if got := NormalizeModelImageMaxSize(1e13); got != ImageCompressDefaultMaxSize {
		t.Fatalf("huge=%d", got)
	}

	if IsRecoverableImageError(nil) {
		t.Fatal("nil is not an error")
	}
	if IsRecoverableImageError(context.Canceled) {
		t.Fatal("context canceled must propagate")
	}
	if IsRecoverableImageError(syscall.ENOMEM) {
		t.Fatal("ENOMEM must propagate")
	}
	if !IsRecoverableImageError(errors.New("decode failed")) {
		t.Fatal("ordinary decode error should be recoverable")
	}
}

// ---------- 引用解析 / PrepareModelImage ----------

func TestResolveAndMaterializeImageRef(t *testing.T) {
	dir := t.TempDir()
	pngBytes := encodePNG(t, image.NewRGBA(image.Rect(0, 0, 2, 2)))
	filePath := filepath.Join(dir, "pic.png")
	if err := os.WriteFile(filePath, pngBytes, 0o600); err != nil {
		t.Fatal(err)
	}
	// 本地路径：不复制、不接管。
	gotPath, owned, err := MaterializeImageRef(context.Background(), filePath, dir)
	if err != nil || owned {
		t.Fatalf("local file: path=%q owned=%v err=%v", gotPath, owned, err)
	}
	if abs, _ := filepath.Abs(filePath); gotPath != abs {
		t.Fatalf("local path=%q want %q", gotPath, abs)
	}
	// data URI：落临时文件并接管。
	dataURI := "data:image/png;base64," + base64Of(t, pngBytes)
	gotPath, owned, err = MaterializeImageRef(context.Background(), dataURI, dir)
	if err != nil || !owned {
		t.Fatalf("data uri: path=%q owned=%v err=%v", gotPath, owned, err)
	}
	if _, err := os.Stat(gotPath); err != nil {
		t.Fatalf("materialized file missing: %v", err)
	}
	resolved, err := ResolveImageRefToDataURL(dataURI)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.HasPrefix([]byte(resolved), []byte("data:image/png;base64,")) {
		preview := resolved
		if len(preview) > 40 {
			preview = preview[:40]
		}
		t.Fatalf("resolved=%q", preview)
	}
	// base64:// 分支。
	resolved, err = ResolveImageRefToDataURL("base64://" + base64Of(t, pngBytes))
	if err != nil || resolved == "" {
		t.Fatalf("base64 resolve=%q err=%v", resolved, err)
	}
	// 非图片 data URI → 空（按非法图片跳过）。
	if got, err := ResolveImageRefToDataURL("data:text/plain;base64," + base64Of(t, []byte("hi"))); err != nil || got != "" {
		t.Fatalf("non-image data uri=%q err=%v", got, err)
	}
	// 不存在的本地路径 → 错误。
	if _, _, err := MaterializeImageRef(context.Background(), filepath.Join(dir, "missing.png"), dir); err == nil {
		t.Fatal("missing file should error")
	}
}

func base64Of(t *testing.T, data []byte) string {
	t.Helper()
	const table = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/"
	var out []byte
	for i := 0; i < len(data); i += 3 {
		var chunk [3]byte
		n := copy(chunk[:], data[i:])
		out = append(out, table[chunk[0]>>2], table[(chunk[0]&0x3)<<4|chunk[1]>>4])
		if n > 1 {
			out = append(out, table[(chunk[1]&0xf)<<2|chunk[2]>>6])
		} else {
			out = append(out, '=')
		}
		if n > 2 {
			out = append(out, table[chunk[2]&0x3f])
		} else {
			out = append(out, '=')
		}
	}
	return string(out)
}

// TestPrepareModelImage 验证工作文件落输出目录、后缀与动图拼图路径。
func TestPrepareModelImage(t *testing.T) {
	t.Setenv("ASTRBOT_DATA_PATH", t.TempDir())
	outDir := t.TempDir()

	// 合规 JPEG → .jpg 工作文件且字节一致。
	jpegBytes := encodeJPEG(t, labeled3x2(), 90)
	path, err := PrepareModelImage(context.Background(), "base64://"+base64Of(t, jpegBytes), ModelImageOptions{
		MaxSize: 1280, Quality: 95, OutputDir: outDir,
	})
	if err != nil || path == "" {
		t.Fatalf("prepare jpeg: path=%q err=%v", path, err)
	}
	if filepath.Ext(path) != ".jpg" {
		t.Fatalf("jpeg output ext=%q", filepath.Ext(path))
	}
	got, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(got, jpegBytes) {
		t.Fatalf("jpeg passthrough mismatch: %v", err)
	}
	// GIF 3 帧 → 拼图 JPEG。
	gifBytes := buildGIF(t, image.Pt(3, 3), []color.RGBA{red, green, blue},
		[]byte{gif.DisposalNone, gif.DisposalNone, gif.DisposalNone}, nil)
	path, err = PrepareModelImage(context.Background(), "base64://"+base64Of(t, gifBytes), ModelImageOptions{
		MaxSize: 30, Quality: 90, OutputDir: outDir,
	})
	if err != nil || path == "" {
		t.Fatalf("prepare gif: path=%q err=%v", path, err)
	}
	if filepath.Ext(path) != ".jpg" {
		t.Fatalf("montage output ext=%q", filepath.Ext(path))
	}
	// 非法引用 → 可恢复，返回空路径且不报错。
	path, err = PrepareModelImage(context.Background(), "base64://"+base64Of(t, []byte("not an image")), ModelImageOptions{
		MaxSize: 1280, OutputDir: outDir,
	})
	if err != nil || path != "" {
		t.Fatalf("invalid image should be skipped: path=%q err=%v", path, err)
	}

	// 输出目录不存在时自动创建。
	missingDir := filepath.Join(t.TempDir(), "nested", "out")
	path, err = PrepareModelImage(context.Background(), "base64://"+base64Of(t, jpegBytes), ModelImageOptions{
		MaxSize: 1280, OutputDir: missingDir,
	})
	if err != nil || path == "" {
		t.Fatalf("create output dir failed: %q %v", path, err)
	}
}
