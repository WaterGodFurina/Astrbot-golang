package t2i

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"io"
	"mime/multipart"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/fogleman/gg"
	"github.com/golang/freetype/truetype"
	"github.com/rivo/uniseg"
	"golang.org/x/image/font"

	"github.com/WaterGodFurina/Astrbot-golang/internal/log"
)

var logger = log.GetDefault().WithComponent("T2I")

// twemojiBase is the CDN base for Twemoji PNG assets (CC-BY 4.0, jdecked fork
// of the twitter/twemoji artwork). Files are cached locally after first fetch.
const twemojiBase = "https://cdn.jsdelivr.net/gh/jdecked/twemoji@15.1.0/assets/72x72/"

// maxDownloadBytes bounds the downloaded Twemoji asset size.
const maxDownloadBytes = 64 << 20

var (
	emojiDirOnce     sync.Once
	emojiDir         string
	emojiImgCache    sync.Map // codepoint -> image.Image
	emojiFailedCache sync.Map // codepoint -> bool (negative cache for offline)
	emojiFlightLocks sync.Map // codepoint -> *sync.Mutex，合并并发下载
	glyphFontCache   sync.Map // font path -> *truetype.Font
)

// emojiFlightLock returns the per-codepoint mutex that serializes the
// download/decode of the same emoji across concurrent renders, so a cache
// miss only triggers one HTTP request per codepoint.
func emojiFlightLock(cp string) *sync.Mutex {
	l, _ := emojiFlightLocks.LoadOrStore(cp, &sync.Mutex{})
	return l.(*sync.Mutex)
}

// glyphFont returns a parsed truetype.Font for glyph-existence checks.
func glyphFont(path string) (*truetype.Font, error) {
	if v, ok := glyphFontCache.Load(path); ok {
		return v.(*truetype.Font), nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	f, err := truetype.Parse(data)
	if err != nil {
		return nil, err
	}
	glyphFontCache.Store(path, f)
	return f, nil
}

// fontHasGlyph reports whether the font at path defines a glyph for r. A
// missing glyph (GlyphIndex 0 = .notdef) would otherwise render as a tofu box.
func fontHasGlyph(path string, r rune) bool {
	f, err := glyphFont(path)
	if err != nil {
		return true // conservative: cannot parse, assume it renders
	}
	return f.Index(r) != 0
}

// ImageOptions controls the whole rendered image.
type ImageOptions struct {
	// Width is the canvas width in pixels (0 defaults to 1080).
	Width int
	// BgColor is the background color (zero value = white).
	BgColor color.RGBA
	// FontPath is a TTF/OTF font file. When empty a system CJK-capable font
	// is searched (Droid Sans Fallback etc.); an error is returned if none
	// is found.
	FontPath string
	// FontSize is the body text size in points (0 defaults to 28).
	FontSize float64
	// LineSpacing is the line spacing multiplier (0 defaults to 1.6).
	LineSpacing float64
	// Padding is the inner margin on all four sides (0 defaults to 48).
	Padding int
	// TextColor is the body text color (zero value = near black).
	TextColor color.RGBA
	// Title is an optional heading drawn at the top (empty = none).
	Title string
	// TitleSize is the title font size (0 defaults to FontSize+10).
	TitleSize float64
	// TitleColor is the title color (zero value = TextColor).
	TitleColor color.RGBA
	// JPEGQuality is used by RenderJPEG (default 90).
	JPEGQuality int
}

func (o ImageOptions) withDefaults() ImageOptions {
	if o.Width <= 0 {
		o.Width = 1080
	}
	if o.BgColor == (color.RGBA{}) {
		o.BgColor = color.RGBA{255, 255, 255, 255}
	}
	if o.FontSize <= 0 {
		o.FontSize = 28
	}
	if o.LineSpacing <= 0 {
		o.LineSpacing = 1.6
	}
	if o.Padding <= 0 {
		o.Padding = 48
	}
	if o.TextColor == (color.RGBA{}) {
		o.TextColor = color.RGBA{30, 30, 30, 255}
	}
	if o.TitleSize <= 0 {
		o.TitleSize = o.FontSize + 10
	}
	if o.TitleColor == (color.RGBA{}) {
		o.TitleColor = o.TextColor
	}
	if o.JPEGQuality <= 0 {
		o.JPEGQuality = 90
	}
	return o
}

// imageLine is a unit of layout: either a wrapped text block or a picture.
type imageLine struct {
	kind     string // "text" | "image"
	text     []string
	size     float64
	color    color.RGBA
	align    string // "left" | "center" | "right"
	hasEmoji bool
	img      image.Image
	imgW     int
	imgH     int
}

// cachedFontFace returns a font.Face for the given path and size. The parsed
// *truetype.Font is cached (concurrency-safe) via glyphFontCache; the face
// itself is created fresh on every call because truetype faces mutate internal
// buffers during Glyph/GlyphAdvance and are not safe for concurrent use.
func cachedFontFace(path string, size float64) (font.Face, error) {
	f, err := glyphFont(path)
	if err != nil {
		return nil, err
	}
	return truetype.NewFace(f, &truetype.Options{Size: size}), nil
}

// systemCJKFont returns a usable CJK-capable font file, preferring an explicit
// path then scanning well-known system locations.
func systemCJKFont(explicit string) string {
	if explicit != "" {
		return explicit
	}
	candidates := []string{
		"/usr/share/fonts/truetype/droid/DroidSansFallbackFull.ttf",
		"/usr/share/fonts/opentype/noto/NotoSansCJK-Regular.ttc",
		"/usr/share/fonts/noto-cjk/NotoSansCJK-Regular.ttc",
		"/usr/share/fonts/truetype/wqy/wqy-zenhei.ttc",
		"/usr/share/fonts/truetype/wqy/wqy-microhei.ttc",
		"/System/Library/Fonts/PingFang.ttc",
		"C:/Windows/Fonts/msyh.ttc",
		"C:/Windows/Fonts/simhei.ttf",
	}
	for _, p := range candidates {
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	return ""
}

// systemASCIIFont returns a font that covers Latin/ASCII (used as a fallback
// for glyphs the CJK font lacks, e.g. DroidSansFallback has no ASCII).
func systemASCIIFont() string {
	candidates := []string{
		"/usr/share/fonts/truetype/noto/NotoSansMono-Regular.ttf",
		"/usr/share/fonts/truetype/noto/NotoMono-Regular.ttf",
		"/usr/share/fonts/truetype/dejavu/DejaVuSans.ttf",
		"/usr/share/fonts/truetype/liberation/LiberationSans-Regular.ttf",
		"/usr/share/fonts/truetype/liberation2/LiberationSans-Regular.ttf",
		"/System/Library/Fonts/Helvetica.ttc",
		"C:/Windows/Fonts/arial.ttf",
	}
	for _, p := range candidates {
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	return ""
}

// ImageRenderer builds a text/picture image (gg-based). It is not safe for
// concurrent use; create one renderer per image.
type ImageRenderer struct {
	opts      ImageOptions
	font      string // CJK-capable font (also default)
	asciiFont string // Latin/ASCII font (optional fallback for glyphs the CJK font lacks)
	lines     []*imageLine
	dc        *gg.Context // scratch context for measuring (uses cached font face)
	face      font.Face
}

// NewImageRenderer creates an image renderer, resolving the font (explicit
// path or system CJK fallback) plus an optional ASCII/Latin fallback font.
func NewImageRenderer(opts ImageOptions) (*ImageRenderer, error) {
	o := opts.withDefaults()
	fontPath := systemCJKFont(o.FontPath)
	if fontPath == "" {
		return nil, fmt.Errorf("t2i: no CJK font found, set ImageOptions.FontPath")
	}
	face, err := cachedFontFace(fontPath, o.FontSize)
	if err != nil {
		return nil, fmt.Errorf("t2i: load font %s: %w", fontPath, err)
	}
	r := &ImageRenderer{
		opts:      o,
		font:      fontPath,
		asciiFont: systemASCIIFont(),
		face:      face,
		dc:        gg.NewContext(1, 1),
	}
	r.dc.SetFontFace(face)
	return r, nil
}

// AddText appends a text block (auto word/rune wrapped to the content width).
// Emoji are preserved and rendered as Twemoji images.
func (r *ImageRenderer) AddText(text string, size float64, c color.RGBA, align string) error {
	if size <= 0 {
		size = r.opts.FontSize
	}
	if c == (color.RGBA{}) {
		c = r.opts.TextColor
	}
	face, err := cachedFontFace(r.font, size)
	if err != nil {
		return err
	}
	mdc := gg.NewContext(1, 1)
	mdc.SetFontFace(face)
	maxW := float64(r.opts.Width - 2*r.opts.Padding)
	var wrapped []string
	hasEmoji := strings.ContainsFunc(text, isEmojiRune)
	if hasEmoji {
		// Emoji occupy roughly font-size width; wrap at grapheme boundaries so
		// ZWJ sequences / skin tones stay together and take correct width.
		wrapped = r.wrapByGraphemes(mdc, text, maxW, size)
	} else {
		wrapped = wrapSmart(mdc, text, maxW)
	}
	r.lines = append(r.lines, &imageLine{
		kind:     "text",
		text:     wrapped,
		size:     size,
		color:    c,
		align:    align,
		hasEmoji: hasEmoji,
	})
	return nil
}

// AddImage appends a picture scaled to fit within maxWidth (and the canvas).
func (r *ImageRenderer) AddImage(img image.Image) {
	w := img.Bounds().Dx()
	h := img.Bounds().Dy()
	maxW := r.opts.Width - 2*r.opts.Padding
	if w > maxW {
		nh := int(float64(h) * float64(maxW) / float64(w))
		if nh <= 0 {
			nh = 1
		}
		w, h = maxW, nh
	}
	r.lines = append(r.lines, &imageLine{kind: "image", img: img, imgW: w, imgH: h})
}

// layoutHeight computes the total content height by simulating the layout.
func (r *ImageRenderer) layoutHeight() int {
	y := float64(r.opts.Padding)
	if r.opts.Title != "" {
		face, _ := cachedFontFace(r.font, r.opts.TitleSize)
		mdc := gg.NewContext(1, 1)
		mdc.SetFontFace(face)
		_, th := mdc.MeasureString(r.opts.Title)
		y += th * 1.4
		y += float64(r.opts.Padding) / 2 // gap after title
	}
	for _, ln := range r.lines {
		if ln.kind == "image" {
			y += float64(ln.imgH) + float64(r.opts.Padding)/2
			continue
		}
		face, _ := cachedFontFace(r.font, ln.size)
		mdc := gg.NewContext(1, 1)
		mdc.SetFontFace(face)
		_, th := mdc.MeasureString("中")
		lineH := th * r.opts.LineSpacing
		y += lineH*float64(len(ln.text)) + float64(r.opts.Padding)/3
	}
	y += float64(r.opts.Padding)
	return int(y)
}

// render draws everything onto a new canvas of the computed height.
func (r *ImageRenderer) render() (*image.RGBA, error) {
	height := r.layoutHeight()
	dc := gg.NewContext(r.opts.Width, height)
	dc.SetColor(r.opts.BgColor)
	dc.Clear()

	x := float64(r.opts.Padding)
	y := float64(r.opts.Padding)
	contentW := float64(r.opts.Width - 2*r.opts.Padding)

	if r.opts.Title != "" {
		face, err := cachedFontFace(r.font, r.opts.TitleSize)
		if err != nil {
			return nil, err
		}
		dc.SetFontFace(face)
		dc.SetColor(r.opts.TitleColor)
		dc.DrawStringAnchored(r.opts.Title, float64(r.opts.Width)/2, y, 0.5, 0)
		_, th := dc.MeasureString(r.opts.Title)
		y += th*1.4 + float64(r.opts.Padding)/2
		// separator line
		dc.SetLineWidth(2)
		dc.SetColor(color.RGBA{0xCC, 0xCC, 0xCC, 0xFF})
		dc.DrawLine(x, y-float64(r.opts.Padding)/3, x+contentW, y-float64(r.opts.Padding)/3)
		dc.Stroke()
	}

	for _, ln := range r.lines {
		if ln.kind == "image" {
			ix := x
			if ln.imgW < int(contentW) {
				ix = x + float64(int(contentW)-ln.imgW)/2
			}
			dc.DrawImage(ln.img, int(ix), int(y))
			y += float64(ln.imgH) + float64(r.opts.Padding)/2
			continue
		}
		face, err := cachedFontFace(r.font, ln.size)
		if err != nil {
			return nil, err
		}
		dc.SetFontFace(face)
		dc.SetColor(ln.color)
		_, th := dc.MeasureString("中")
		lineH := th * r.opts.LineSpacing
		for _, line := range ln.text {
			w := r.measureLineWidth(dc, line, ln.size)
			tx := x
			switch ln.align {
			case "center":
				tx = x + (contentW-w)/2
			case "right":
				tx = x + contentW - w
			}
			r.drawMixedLine(dc, line, tx, y, ln.size, ln.color)
			y += lineH
		}
		y += float64(r.opts.Padding) / 3
	}
	return dc.Image().(*image.RGBA), nil
}

// measureLineWidth computes a line's advance width, treating emoji clusters as
// font-size wide (matching the wrap and the emoji image size). Missing-glyph
// characters (neither the CJK nor the ASCII font has them) also take one
// font-size so lines do not shift.
func (r *ImageRenderer) measureLineWidth(dc *gg.Context, line string, size float64) float64 {
	if !strings.ContainsFunc(line, isEmojiRune) && !r.lineHasMissingGlyph(line) {
		w, _ := dc.MeasureString(line)
		return w
	}
	var w float64
	gr := uniseg.NewGraphemes(line)
	for gr.Next() {
		cluster := gr.Str()
		switch {
		case isEmojiCluster(cluster):
			w += size
		case r.clusterHasGlyph(cluster):
			face := r.faceFor(cluster, size)
			md := gg.NewContext(1, 1)
			md.SetFontFace(face)
			cw, _ := md.MeasureString(cluster)
			w += cw
		default:
			w += size
		}
	}
	return w
}

// faceFor picks the font face that can render the cluster: the ASCII font if
// it covers every rune, otherwise the CJK/default font.
func (r *ImageRenderer) faceFor(cluster string, size float64) font.Face {
	if r.asciiFont != "" {
		allASCII := true
		for _, ch := range cluster {
			if !fontHasGlyph(r.asciiFont, ch) {
				allASCII = false
				break
			}
		}
		if allASCII {
			if face, err := cachedFontFace(r.asciiFont, size); err == nil {
				return face
			}
		}
	}
	face, _ := cachedFontFace(r.font, size)
	return face
}

// clusterHasGlyph reports whether any of the two fonts can render the cluster.
func (r *ImageRenderer) clusterHasGlyph(cluster string) bool {
	for _, ch := range cluster {
		if isEmojiRune(ch) {
			continue
		}
		if fontHasGlyph(r.font, ch) || (r.asciiFont != "" && fontHasGlyph(r.asciiFont, ch)) {
			return true
		}
	}
	return false
}

// lineHasMissingGlyph reports whether any rune is missing from both fonts.
func (r *ImageRenderer) lineHasMissingGlyph(line string) bool {
	for _, ch := range line {
		if isEmojiRune(ch) {
			continue
		}
		if !fontHasGlyph(r.font, ch) && (r.asciiFont == "" || !fontHasGlyph(r.asciiFont, ch)) {
			return true
		}
	}
	return false
}

// drawMixedLine draws a line mixing font text (with ASCII/CJK fallback) and
// Twemoji images.
func (r *ImageRenderer) drawMixedLine(dc *gg.Context, line string, x, y, size float64, c color.RGBA) {
	dc.SetColor(c)
	cur := x
	gr := uniseg.NewGraphemes(line)
	for gr.Next() {
		cluster := gr.Str()
		switch {
		case isEmojiCluster(cluster):
			if img, ok := emojiImage(cluster); ok {
				// Emoji image height ≈ font size; bottom aligned with the
				// baseline so it sits on the same line as the text.
				dc.DrawImage(img, int(cur), int(y-size))
				cur += size
				continue
			}
			// Emoji unavailable (offline/unknown): skip, don't draw tofu.
			cur += size
			continue
		case r.clusterHasGlyph(cluster):
			f := r.faceFor(cluster, size)
			dc.SetFontFace(f)
			dc.DrawString(cluster, cur, y)
			cw, _ := dc.MeasureString(cluster)
			cur += cw
		default:
			// Missing in both fonts: skip (no tofu), keep the advance.
			cur += size
		}
	}
}

// wrapByGraphemes wraps text at grapheme-cluster boundaries so emoji sequences
// (ZWJ, skin tone, flags) stay intact and are counted as emojiWidth each.
func (r *ImageRenderer) wrapByGraphemes(dc *gg.Context, text string, maxWidth, emojiWidth float64) []string {
	var lines []string
	var cur []string
	curW := 0.0
	flush := func() {
		if len(cur) > 0 {
			lines = append(lines, strings.Join(cur, ""))
			cur = cur[:0]
			curW = 0
		}
	}
	gr := uniseg.NewGraphemes(text)
	for gr.Next() {
		cluster := gr.Str()
		var w float64
		switch {
		case isEmojiCluster(cluster):
			w = emojiWidth
		case r.clusterHasGlyph(cluster):
			face := r.faceFor(cluster, emojiWidth)
			md := gg.NewContext(1, 1)
			md.SetFontFace(face)
			w, _ = md.MeasureString(cluster)
		default:
			w = emojiWidth
		}
		if curW+w > maxWidth && len(cur) > 0 {
			flush()
		}
		cur = append(cur, cluster)
		curW += w
	}
	flush()
	return lines
}

// RenderPNG encodes the result as PNG bytes.
func (r *ImageRenderer) RenderPNG() ([]byte, error) {
	img, err := r.render()
	if err != nil {
		return nil, err
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// RenderJPEG encodes the result as JPEG bytes.
func (r *ImageRenderer) RenderJPEG() ([]byte, error) {
	img, err := r.render()
	if err != nil {
		return nil, err
	}
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: r.opts.JPEGQuality}); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// wrapSmart wraps text to maxWidth: prefers breaking at whitespace for
// Latin text, and breaks between any runes for CJK (which has no spaces).
func wrapSmart(dc *gg.Context, text string, maxWidth float64) []string {
	var out []string
	for _, seg := range strings.Split(text, "\n") {
		if seg == "" {
			out = append(out, "")
			continue
		}
		fields := splitKeepSpaces(seg)
		line := ""
		// lineW 缓存当前行累计宽度，避免每次候选都重新测量整行（O(n) 增量）。
		lineW := 0.0
		for _, field := range fields {
			if field == " " {
				if line != "" {
					line += " "
					sw, _ := dc.MeasureString(" ")
					lineW += sw
				}
				continue
			}
			// candidate = line + (space if needed) + field
			fw, _ := dc.MeasureString(field)
			sep := ""
			sepW := 0.0
			if line != "" && !strings.HasSuffix(line, " ") {
				sep = " "
				sepW, _ = dc.MeasureString(" ")
			}
			if lineW+sepW+fw <= maxWidth {
				line += sep + field
				lineW += sepW + fw
				continue
			}
			// field alone fits?
			if line != "" {
				out = append(out, strings.TrimRight(line, " "))
				line = ""
				lineW = 0
			}
			if fw <= maxWidth {
				line = field
				lineW = fw
				continue
			}
			// field itself exceeds: break by runes
			cur := ""
			curW := 0.0
			for _, ch := range field {
				cw, _ := dc.MeasureString(string(ch))
				if curW+cw > maxWidth && cur != "" {
					out = append(out, strings.TrimRight(cur, " "))
					cur = ""
					curW = 0
				}
				cur += string(ch)
				curW += cw
			}
			if cur != "" {
				line = cur
				lineW = curW
			}
		}
		if strings.TrimSpace(line) != "" {
			out = append(out, strings.TrimRight(line, " "))
		}
	}
	// Drop purely-empty trailing lines but keep interior blanks.
	var res []string
	lastNonEmpty := -1
	for i, l := range out {
		if strings.TrimSpace(l) != "" {
			lastNonEmpty = i
		}
	}
	for i, l := range out {
		if i > lastNonEmpty {
			break
		}
		res = append(res, l)
	}
	return res
}

// splitKeepSpaces splits a line into non-space chunks (spaces dropped but
// re-added as separators by the caller). Punctuation sticks to its preceding
// word; CJK runs are one chunk (broken per-rune later if needed).
func splitKeepSpaces(s string) []string {
	var fields []string
	var cur []rune
	flush := func() {
		if len(cur) > 0 {
			fields = append(fields, string(cur))
			cur = cur[:0]
		}
	}
	for _, ch := range s {
		if ch == ' ' {
			flush()
			continue
		}
		cur = append(cur, ch)
	}
	flush()
	return fields
}

// isEmojiRune reports whether r belongs to an emoji codepoint range (which the
// CJK fallback font has no glyph for; rendered as Twemoji images instead).
func isEmojiRune(r rune) bool {
	switch {
	case r == 0x200D, r == 0xFE0F, r == 0xFE0E, r == 0x20E3: // ZWJ / VS16 / VS15 / keycap
		return true
	case r >= 0x1F000 && r <= 0x1FAFF: // emoji & pictographs
		return true
	case r >= 0x1F1E6 && r <= 0x1F1FF: // regional indicator symbols (flags)
		return true
	case r >= 0x2600 && r <= 0x27BF: // misc symbols / dingbats / arrows
		return true
	case r >= 0x2B00 && r <= 0x2BFF: // misc symbols and arrows
		return true
	case r == 0x231A || r == 0x231B || r == 0x2934 || r == 0x2935 ||
		r == 0x3030 || r == 0x303D || r == 0x3297 || r == 0x3299 ||
		r == 0x25B6 || r == 0x25C0 || r == 0x25AA || r == 0x25AB ||
		r == 0x25FB || r == 0x25FC || r == 0x25FD || r == 0x25FE:
		return true
	}
	return false
}

// isEmojiCluster reports whether a grapheme cluster contains any emoji
// codepoint (covers ZWJ sequences, skin tones and flag pairs).
func isEmojiCluster(cluster string) bool {
	for _, r := range cluster {
		if isEmojiRune(r) {
			return true
		}
	}
	return false
}

// emojiCodepoint converts a grapheme cluster to a Twemoji filename codepoint
// (lowercase hex joined by "-", variation selectors dropped).
func emojiCodepoint(cluster string) string {
	var parts []string
	for _, r := range cluster {
		if r == 0xFE0F || r == 0xFE0E {
			continue
		}
		parts = append(parts, fmt.Sprintf("%x", r))
	}
	if len(parts) == 0 {
		return ""
	}
	return strings.Join(parts, "-")
}

func ensureEmojiDir() string {
	emojiDirOnce.Do(func() {
		if dir := os.Getenv("ASTRBOT_TWEMOJI_DIR"); dir != "" {
			emojiDir = dir
		} else if cache, err := os.UserCacheDir(); err == nil {
			emojiDir = filepath.Join(cache, "astrbot-go", "twemoji")
		} else {
			emojiDir = filepath.Join(os.TempDir(), "astrbot-twemoji")
		}
		_ = os.MkdirAll(emojiDir, 0o755)
	})
	return emojiDir
}

// emojiImage returns a decoded Twemoji PNG for the cluster, downloading and
// caching it on first use. Failed downloads are cached negatively so an
// offline render does not retry every emoji on every line. Returns ok=false on
// any failure so the caller can fall back to skipping the glyph.
func emojiImage(cluster string) (image.Image, bool) {
	cp := emojiCodepoint(cluster)
	if cp == "" {
		return nil, false
	}
	if v, ok := emojiImgCache.Load(cp); ok {
		return v.(image.Image), true
	}
	if _, failed := emojiFailedCache.Load(cp); failed {
		return nil, false
	}
	// per-codepoint 锁合并并发下载：同一未缓存 emoji 只发一次 HTTP 请求。
	lock := emojiFlightLock(cp)
	lock.Lock()
	defer lock.Unlock()
	// 持锁后再查缓存：等待者可能已完成下载或写入失败缓存。
	if v, ok := emojiImgCache.Load(cp); ok {
		return v.(image.Image), true
	}
	if _, failed := emojiFailedCache.Load(cp); failed {
		return nil, false
	}
	path := filepath.Join(ensureEmojiDir(), cp+".png")
	data, err := os.ReadFile(path)
	if err != nil {
		data, err = downloadTwemoji(cp)
		if err != nil {
			emojiFailedCache.Store(cp, true)
			logger.Debug("twemoji %s: %v", cp, err)
			return nil, false
		}
		// Write to a temp file then rename so a concurrent download never
		// exposes a partially-written cache file (which would be permanently
		// negative-cached as corrupt).
		if err := writeCacheFile(path, data); err != nil {
			return nil, false
		}
	}
	img, err := png.Decode(bytes.NewReader(data))
	if err != nil {
		emojiFailedCache.Store(cp, true)
		return nil, false
	}
	emojiImgCache.Store(cp, img)
	return img, true
}

func downloadTwemoji(cp string) ([]byte, error) {
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Get(twemojiBase + cp + ".png")
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	return io.ReadAll(io.LimitReader(resp.Body, maxDownloadBytes))
}

// writeCacheFile atomically writes data to path via a temp file + rename, so a
// concurrent writer never leaves a partially-written cache file behind.
func writeCacheFile(path string, data []byte) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, filepath.Base(path)+".*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	if _, err = tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return err
	}
	if err = tmp.Close(); err != nil {
		os.Remove(tmpName)
		return err
	}
	if err = os.Rename(tmpName, path); err != nil {
		os.Remove(tmpName)
		return err
	}
	return nil
}

// RenderTextToPNG renders text to a PNG image with the given options
// (a convenience wrapper over ImageRenderer).
func RenderTextToPNG(text string, opts ImageOptions) ([]byte, error) {
	r, err := NewImageRenderer(opts)
	if err != nil {
		return nil, err
	}
	if err := r.AddText(text, opts.FontSize, opts.TextColor, "left"); err != nil {
		return nil, err
	}
	return r.RenderPNG()
}

// RenderTextToJPEG renders text to a JPEG image.
func RenderTextToJPEG(text string, opts ImageOptions) ([]byte, error) {
	r, err := NewImageRenderer(opts)
	if err != nil {
		return nil, err
	}
	if err := r.AddText(text, opts.FontSize, opts.TextColor, "left"); err != nil {
		return nil, err
	}
	return r.RenderJPEG()
}

// RenderRemote renders text via a remote t2i service (Python AstrBot t2i
// server protocol): POST multipart form {text, template_name} to the endpoint
// and read the returned image bytes. The response may be raw image data or a
// JSON envelope {code, data: {url|base64}}; anything else is an error.
func RenderRemote(endpoint, text, templateName string) ([]byte, error) {
	if endpoint == "" {
		return nil, fmt.Errorf("t2i: remote endpoint is empty")
	}
	url := strings.TrimRight(endpoint, "/") + "/t2i/"
	var buf bytes.Buffer
	writer := multipart.NewWriter(&buf)
	if err := writer.WriteField("text", text); err != nil {
		return nil, err
	}
	if templateName == "" {
		templateName = "base"
	}
	if err := writer.WriteField("template_name", templateName); err != nil {
		return nil, err
	}
	if err := writer.Close(); err != nil {
		return nil, err
	}

	client := &http.Client{Timeout: 120 * time.Second}
	req, err := http.NewRequest(http.MethodPost, url, &buf)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", writer.FormDataContentType())
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("t2i remote request: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("t2i remote returned HTTP %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if err != nil {
		return nil, err
	}
	if isImageData(body) {
		return body, nil
	}
	// JSON envelope fallback.
	var env struct {
		Data struct {
			URL    string `json:"url"`
			Base64 string `json:"base64"`
		} `json:"data"`
	}
	if json.Unmarshal(body, &env) == nil {
		if env.Data.Base64 != "" {
			raw, err := decodeBase64Image(env.Data.Base64)
			if err != nil {
				return nil, err
			}
			return raw, nil
		}
		if env.Data.URL != "" {
			return fetchRemoteImage(env.Data.URL)
		}
	}
	return nil, fmt.Errorf("t2i remote: unrecognized response (%d bytes)", len(body))
}

// RenderCustomTemplate renders an HTML template + data via a remote t2i service
// (Python AstrBot HtmlRenderer 协议)，POST multipart 表单 {template, data,
// options} 到 endpoint 的 text2img 路径并读取返回的图片字节。响应可能是原始
// 图片数据或 JSON envelope {code, data:{url|base64}}；其余视为错误。
//
// 路径约定：endpoint 为 t2i 服务根地址。若以 /t2i 或 /t2i/ 结尾（RenderRemote
// 的 endpoint 约定），取其父路径再拼 /text2img；已是 /text2img 结尾则原样使用；
// 否则直接拼接 /text2img（对齐 Python 原版
// ASTRBOT_T2I_DEFAULT_ENDPOINT="https://t2i.soulter.top/text2img"）。
//
// endpoint 为空时使用官方默认端点；官方端点列表（api.soulter.top/astrbot/
// t2i-endpoints）拉取成功时按序逐个尝试（对齐原版 network_strategy 的多端点
// 容灾），全部失败返回最后错误。
func RenderCustomTemplate(endpoint, template, data, options string) ([]byte, error) {
	endpoints := t2iResolvedEndpoints(endpoint)
	if len(endpoints) == 0 {
		return nil, fmt.Errorf("t2i: remote endpoint is empty")
	}
	var lastErr error
	for _, ep := range endpoints {
		img, err := renderCustomTemplateAt(ep, template, data, options)
		if err == nil {
			return img, nil
		}
		lastErr = err
	}
	return nil, lastErr
}

func renderCustomTemplateAt(endpoint, template, data, options string) ([]byte, error) {
	url := strings.TrimRight(endpoint, "/")
	if strings.HasSuffix(url, "/t2i") {
		url = strings.TrimSuffix(url, "/t2i")
	}
	if !strings.HasSuffix(url, "/text2img") {
		url += "/text2img"
	}
	var buf bytes.Buffer
	writer := multipart.NewWriter(&buf)
	if err := writer.WriteField("template", template); err != nil {
		return nil, err
	}
	if err := writer.WriteField("data", data); err != nil {
		return nil, err
	}
	if err := writer.WriteField("options", options); err != nil {
		return nil, err
	}
	if err := writer.Close(); err != nil {
		return nil, err
	}

	client := &http.Client{Timeout: 120 * time.Second}
	req, err := http.NewRequest(http.MethodPost, url, &buf)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", writer.FormDataContentType())
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("t2i remote html render request: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("t2i remote html render returned HTTP %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if err != nil {
		return nil, err
	}
	if isImageData(body) {
		return body, nil
	}
	// JSON envelope 兜底（同 RenderRemote）。
	var env struct {
		Data struct {
			URL    string `json:"url"`
			Base64 string `json:"base64"`
		} `json:"data"`
	}
	if json.Unmarshal(body, &env) == nil {
		if env.Data.Base64 != "" {
			raw, err := decodeBase64Image(env.Data.Base64)
			if err != nil {
				return nil, err
			}
			return raw, nil
		}
		if env.Data.URL != "" {
			return fetchRemoteImage(env.Data.URL)
		}
	}
	return nil, fmt.Errorf("t2i remote html render: unrecognized response (%d bytes)", len(body))
}

// isImageData reports whether b looks like PNG/JPEG/GIF/WEBP image bytes.
func isImageData(b []byte) bool {
	return bytes.HasPrefix(b, []byte("\x89PNG")) ||
		bytes.HasPrefix(b, []byte("\xFF\xD8")) ||
		bytes.HasPrefix(b, []byte("GIF8")) ||
		bytes.HasPrefix(b, []byte("RIFF"))
}

// 官方远程 t2i 默认端点与官方端点列表（对齐 Python 原版
// ASTRBOT_T2I_DEFAULT_ENDPOINT / get_official_endpoints）。
const (
	T2IDefaultEndpoint       = "https://t2i.soulter.top/text2img"
	t2iOfficialEndpointsURL  = "https://api.soulter.top/astrbot/t2i-endpoints"
	t2iEndpointsCacheTTL     = 10 * time.Minute
	t2iEndpointsRequestLimit = 16 << 10 // 16 KiB
)

var (
	t2iEndpointsMu    sync.Mutex
	t2iEndpointsCache []string
	t2iEndpointsAt   time.Time
)

// t2iResolvedEndpoints 解析远程 t2i 端点列表：
//   - endpoint 非空 → 仅该端点（显式配置优先）
//   - 否则默认官方端点 + 官方端点列表（拉取失败回退默认端点）
func t2iResolvedEndpoints(endpoint string) []string {
	if ep := strings.TrimSpace(endpoint); ep != "" {
		return []string{ep}
	}
	t2iEndpointsMu.Lock()
	defer t2iEndpointsMu.Unlock()
	if len(t2iEndpointsCache) > 0 && time.Since(t2iEndpointsAt) < t2iEndpointsCacheTTL {
		return t2iEndpointsCache
	}
	eps := []string{T2IDefaultEndpoint}
	if list := fetchOfficialT2IEndpoints(); len(list) > 0 {
		eps = list
	}
	t2iEndpointsCache, t2iEndpointsAt = eps, time.Now()
	return eps
}

// fetchOfficialT2IEndpoints 从官方接口拉取可用 t2i 端点（网络失败返回 nil，
// 调用方回退默认端点）。
func fetchOfficialT2IEndpoints() []string {
	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Get(t2iOfficialEndpointsURL)
	if err != nil {
		return nil
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, t2iEndpointsRequestLimit))
	if err != nil {
		return nil
	}
	var payload struct {
		Data []struct {
			URL    string `json:"url"`
			Active bool   `json:"active"`
		} `json:"data"`
	}
	if json.Unmarshal(body, &payload) != nil {
		return nil
	}
	var out []string
	for _, ep := range payload.Data {
		u := strings.TrimSpace(ep.URL)
		if u == "" || !ep.Active {
			continue
		}
		out = append(out, u)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func decodeBase64Image(s string) ([]byte, error) {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, ','); i >= 0 && strings.HasPrefix(s, "data:") {
		s = s[i+1:]
	}
	raw, err := base64.StdEncoding.DecodeString(s)
	if err != nil {
		raw, err = base64.RawStdEncoding.DecodeString(s)
	}
	if err != nil {
		return nil, err
	}
	if !isImageData(raw) {
		return nil, fmt.Errorf("decoded payload is not an image")
	}
	return raw, nil
}

func fetchRemoteImage(u string) ([]byte, error) {
	// SSRF 防御：t2i 服务返回的图片 URL 应指向公网对象存储。白名单模式——
	// 在连接建立时按解析出的实际对端 IP 校验，拒绝环回/私网/链路本地/组播/
	// 未指定/广播地址（同时覆盖域名解析、非点分 IP 文本与重定向目标）。
	parsed, err := url.Parse(u)
	if err != nil {
		return nil, fmt.Errorf("fetch image: invalid URL: %w", err)
	}
	switch parsed.Scheme {
	case "http", "https":
	default:
		return nil, fmt.Errorf("fetch image: unsupported scheme %q", parsed.Scheme)
	}
	if parsed.Hostname() == "" {
		return nil, fmt.Errorf("fetch image: missing host")
	}
	checkRedirect := func(req *http.Request, via []*http.Request) error {
		if len(via) >= 3 {
			return fmt.Errorf("too many redirects")
		}
		return nil // 每个重定向目标仍由下面的 dialer 按实际对端 IP 校验
	}
	dialer := &net.Dialer{Timeout: 10 * time.Second}
	transport := &http.Transport{
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			host, _, err := net.SplitHostPort(addr)
			if err != nil {
				return nil, err
			}
			ips, err := net.DefaultResolver.LookupIPAddr(ctx, host)
			if err != nil {
				return nil, err
			}
			for _, ip := range ips {
				if !isPublicTarget(ip.IP) {
					return nil, fmt.Errorf("fetch image: non-public target %s rejected", ip.IP)
				}
			}
			return dialer.DialContext(ctx, network, addr)
		},
		TLSHandshakeTimeout: 10 * time.Second,
	}
	client := &http.Client{Timeout: 120 * time.Second, Transport: transport, CheckRedirect: checkRedirect}
	resp, err := client.Get(u)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("fetch image HTTP %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if err != nil {
		return nil, err
	}
	if !isImageData(body) {
		return nil, fmt.Errorf("not an image")
	}
	return body, nil
}

// isPublicTarget reports whether ip is a public unicast address (rejects
// loopback, private, link-local, multicast, unspecified and broadcast).
func isPublicTarget(ip net.IP) bool {
	return ip.IsGlobalUnicast() && !ip.IsPrivate() && !ip.IsLinkLocalUnicast()
}
