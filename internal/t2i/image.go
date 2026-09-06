package t2i

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
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
	f, err := parseFontData(data)
	if err != nil {
		return nil, err
	}
	glyphFontCache.Store(path, f)
	return f, nil
}

// parseFontData parses TTF/OTF font bytes into a *truetype.Font. TrueType
// Collection (.ttc) files are handled by extracting the font at index 0 into a
// standalone TTF first (freetype/truetype has no TTC support and TTC table
// offsets are absolute to the file start).
func parseFontData(data []byte) (*truetype.Font, error) {
	if len(data) >= 4 && string(data[:4]) == "ttcf" {
		standalone, err := extractTTFFromTTC(data, 0)
		if err != nil {
			return nil, err
		}
		if standalone == nil {
			return nil, fmt.Errorf("truetype: unrecognized font collection")
		}
		return truetype.Parse(standalone)
	}
	return truetype.Parse(data)
}

// extractTTFFromTTC extracts the font at index from a TrueType Collection
// (.ttc) into a standalone TTF byte stream. In a TTC each subfont's table
// directory stores table offsets that are absolute to the TTC file start;
// freetype/truetype expects offsets relative to the font stream start, so we
// rebuild the sfnt header + table directory and copy each table's raw bytes
// into a fresh, self-contained TTF. Returns (nil, nil) for non-TTC input.
func extractTTFFromTTC(data []byte, index int) ([]byte, error) {
	if len(data) < 4 || string(data[:4]) != "ttcf" {
		return nil, nil
	}
	if len(data) < 12 {
		return nil, fmt.Errorf("truetype: malformed ttc header")
	}
	numFonts := int(binary.BigEndian.Uint32(data[8:12]))
	if index < 0 || index >= numFonts || len(data) < 12+numFonts*4 {
		return nil, fmt.Errorf("truetype: ttc font index %d out of range", index)
	}
	base := int(binary.BigEndian.Uint32(data[12+index*4:]))
	if base < 0 || base+12 > len(data) {
		return nil, fmt.Errorf("truetype: ttc font offset out of range")
	}
	s := data[base:]
	scaler := s[0:4]
	numTables := int(binary.BigEndian.Uint16(s[4:6]))
	if numTables == 0 || base+12+numTables*16 > len(data) {
		return nil, fmt.Errorf("truetype: malformed ttc subfont directory")
	}

	type table struct {
		tag      [4]byte
		checksum [4]byte
		length   int
		body     []byte
	}
	tables := make([]table, 0, numTables)
	for i := 0; i < numTables; i++ {
		rec := s[12+i*16:]
		var tag, checksum [4]byte
		copy(tag[:], rec[0:4])
		copy(checksum[:], rec[4:8])
		tblOff := int(binary.BigEndian.Uint32(rec[8:12]))
		tblLen := int(binary.BigEndian.Uint32(rec[12:16]))
		if tblOff < 0 || tblLen < 0 || tblOff+tblLen > len(data) {
			return nil, fmt.Errorf("truetype: ttc table %q out of range", string(tag[:]))
		}
		tables = append(tables, table{tag: tag, checksum: checksum, length: tblLen, body: data[tblOff : tblOff+tblLen]})
	}

	// Rebuild a standalone sfnt: header + table directory + table data.
	headerSize := 12 + numTables*16
	out := make([]byte, headerSize)
	copy(out[0:4], scaler)
	entrySelector := 0
	for (1 << (entrySelector + 1)) <= numTables {
		entrySelector++
	}
	searchRange := 1 << entrySelector
	binary.BigEndian.PutUint16(out[4:6], uint16(numTables))
	binary.BigEndian.PutUint16(out[6:8], uint16(searchRange*16))
	binary.BigEndian.PutUint16(out[8:10], uint16(entrySelector))
	binary.BigEndian.PutUint16(out[10:12], uint16(numTables*16-searchRange*16))

	pos := headerSize
	for i := range tables {
		padded := tables[i].length
		if r := padded % 4; r != 0 {
			padded += 4 - r
		}
		start := len(out)
		out = append(out, make([]byte, padded)...)
		copy(out[start:start+tables[i].length], tables[i].body)
		recStart := 12 + i*16
		copy(out[recStart:recStart+4], tables[i].tag[:])
		copy(out[recStart+4:recStart+8], tables[i].checksum[:])
		binary.BigEndian.PutUint32(out[recStart+8:recStart+12], uint32(pos))
		binary.BigEndian.PutUint32(out[recStart+12:recStart+16], uint32(tables[i].length))
		pos += padded
	}
	return out, nil
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
	// Width is the canvas width in pixels (0 defaults to 800).
	Width int
	// BgColor is the background color (zero value = PAPER).
	BgColor color.RGBA
	// FontPath is a TTF/OTF font file. When empty a system CJK-capable font
	// is searched (Droid Sans Fallback etc.); an error is returned if none
	// is found.
	FontPath string
	// FontSize is the body text size in points (0 defaults to 25).
	FontSize float64
	// LineSpacing is the line spacing multiplier (0 defaults to 1.72).
	LineSpacing float64
	// Padding is the inner margin on all four sides (0 defaults to 32).
	Padding int
	// TextColor is the body text color (zero value = INK).
	TextColor color.RGBA
	// Title is an optional heading drawn at the top (empty = none).
	// Note: the paper-style renderer uses a branded masthead instead.
	Title string
	// TitleSize is the title font size (0 defaults to 28).
	TitleSize float64
	// TitleColor is the title color (zero value = INK).
	TitleColor color.RGBA
	// JPEGQuality is used by RenderJPEG (default 90).
	JPEGQuality int
}

func (o ImageOptions) withDefaults() ImageOptions {
	if o.Width <= 0 {
		o.Width = 800
	}
	if o.BgColor == (color.RGBA{}) {
		o.BgColor = color.RGBA{251, 251, 250, 255} // PAPER
	}
	if o.FontSize <= 0 {
		o.FontSize = 25
	}
	if o.LineSpacing <= 0 {
		o.LineSpacing = 1.72
	}
	if o.Padding <= 0 {
		o.Padding = 32 // CONTENT_MARGIN
	}
	if o.TextColor == (color.RGBA{}) {
		o.TextColor = color.RGBA{32, 34, 36, 255} // INK
	}
	if o.TitleSize <= 0 {
		o.TitleSize = 28
	}
	if o.TitleColor == (color.RGBA{}) {
		o.TitleColor = color.RGBA{32, 34, 36, 255} // INK
	}
	if o.JPEGQuality <= 0 {
		o.JPEGQuality = 90
	}
	return o
}

// imageLine is a unit of layout: either a raw text block or a picture.
type imageLine struct {
	kind  string // "text" | "image"
	text  []string
	size  float64
	color color.RGBA
	align string // "left" | "center" | "right"
	img   image.Image
	imgW  int
	imgH  int
}

// Paper-style color palette (对齐 Python local_strategy.py).
func paperPaper() color.RGBA      { return color.RGBA{251, 251, 250, 255} }
func paperInk() color.RGBA        { return color.RGBA{32, 34, 36, 255} }
func paperMuted() color.RGBA      { return color.RGBA{102, 107, 112, 255} }
func paperLine() color.RGBA       { return color.RGBA{212, 216, 219, 255} }
func paperStrongLine() color.RGBA { return color.RGBA{188, 194, 198, 255} }
func paperSoftFill() color.RGBA   { return color.RGBA{242, 243, 243, 255} }
func paperCodeFill() color.RGBA   { return color.RGBA{244, 245, 245, 255} }
func paperAccent() color.RGBA     { return color.RGBA{47, 134, 189, 255} }

// mdBlock is a measured markdown block (paragraph, heading, list, code, etc).
type mdBlock interface {
	measure(r *ImageRenderer, width float64) int
	render(r *ImageRenderer, dc *gg.Context, x, y, width float64) float64
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func maxF(a, b float64) float64 {
	if a > b {
		return a
	}
	return b
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

// fontFileUsable reports whether the file exists and can be parsed into a
// *truetype.Font (TrueType-glyf based, or a .ttc whose first subfont is).
// CFF/OpenType (.otf / OTTO ttcs, e.g. Noto Sans CJK) are NOT parseable by
// freetype/truetype and therefore skipped so the resolver can try other fonts.
func fontFileUsable(path string) bool {
	data, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	_, err = parseFontData(data)
	return err == nil
}

// systemCJKFont returns a usable CJK-capable font file, preferring an explicit
// path then scanning well-known system locations. Only fonts that actually
// parse as TrueType are returned (CFF-based candidates are skipped).
func systemCJKFont(explicit string) string {
	if explicit != "" {
		return explicit
	}
	candidates := []string{
		"/usr/share/fonts/truetype/droid/DroidSansFallbackFull.ttf",
		"/usr/share/fonts/wqy-zenhei/wqy-zenhei.ttc",
		"/usr/share/fonts/truetype/wqy/wqy-zenhei.ttc",
		"/usr/share/fonts/truetype/wqy/wqy-microhei.ttc",
		"/System/Library/Fonts/PingFang.ttc",
		"C:/Windows/Fonts/msyh.ttc",
		"C:/Windows/Fonts/simhei.ttf",
		"/usr/share/fonts/noto/NotoSansCJK-Regular.ttc",
		"/usr/share/fonts/opentype/noto/NotoSansCJK-Regular.ttc",
		"/usr/share/fonts/noto-cjk/NotoSansCJK-Regular.ttc",
	}
	for _, p := range candidates {
		if fontFileUsable(p) {
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
		if fontFileUsable(p) {
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

// AddText appends a raw markdown text block. The text is re-parsed as markdown
// at render time (so block-level layout aligns with Python local_strategy).
// Emoji are preserved and rendered as Twemoji images.
func (r *ImageRenderer) AddText(text string, size float64, c color.RGBA, align string) error {
	if size <= 0 {
		size = r.opts.FontSize
	}
	if c == (color.RGBA{}) {
		c = r.opts.TextColor
	}
	r.lines = append(r.lines, &imageLine{
		kind:  "text",
		text:  []string{text},
		size:  size,
		color: c,
		align: align,
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

// rawText concatenates all text lines added via AddText (preserving \n
// between separate AddText calls) so render() can parse the full markdown.
func (r *ImageRenderer) rawText() string {
	var parts []string
	for _, ln := range r.lines {
		if ln.kind == "text" {
			parts = append(parts, strings.Join(ln.text, "\n"))
		}
	}
	return strings.Join(parts, "\n")
}

// collectImages returns all image lines in order (rendered inline after text).
func (r *ImageRenderer) collectImages() []*imageLine {
	var imgs []*imageLine
	for _, ln := range r.lines {
		if ln.kind == "image" {
			imgs = append(imgs, ln)
		}
	}
	return imgs
}

// render draws the paper-style AstrBot image: masthead + markdown blocks.
func (r *ImageRenderer) render() (*image.RGBA, error) {
	mdText := r.rawText()
	contentW := float64(r.opts.Width - 2*r.opts.Padding)
	blocks := parseMarkdownBlocks(mdText)

	// Measure all blocks.
	blockHeights := make([]int, len(blocks))
	totalContent := 0
	for i, b := range blocks {
		h := b.measure(r, contentW)
		blockHeights[i] = h
		totalContent += h
	}

	// Images: append after text blocks (简化——Go 端 AddImage 在 pipeline 里
	// 不与 AddText 混用，但保留兼容：图片接在文字后依次排列）。
	images := r.collectImages()
	imgTotalH := 0.0
	for _, im := range images {
		imgTotalH += float64(im.imgH) + 20
	}

	mastheadH := 91.0
	bottomPad := 34.0
	totalH := mastheadH + float64(totalContent) + imgTotalH + bottomPad
	if totalH < 140 {
		totalH = 140
	}

	dc := gg.NewContext(r.opts.Width, int(totalH))
	dc.SetColor(r.opts.BgColor)
	dc.Clear()

	r.drawMasthead(dc)

	y := mastheadH
	for i, b := range blocks {
		_ = blockHeights[i]
		y = b.render(r, dc, float64(r.opts.Padding), y, contentW)
	}

	// Draw images after text.
	for _, im := range images {
		ix := float64(r.opts.Padding)
		if im.imgW < int(contentW) {
			ix += (contentW - float64(im.imgW)) / 2
		}
		dc.DrawImage(im.img, int(ix), int(y)+10)
		y += float64(im.imgH) + 20
	}

	return dc.Image().(*image.RGBA), nil
}

// drawMasthead draws the AstrBot branded header (star mark + wordmark + version).
func (r *ImageRenderer) drawMasthead(dc *gg.Context) {
	mx := float64(r.opts.Padding) + 14 // center_x
	my := 43.0                         // center_y
	outer := 13.0
	inner := 4.0
	accent := paperAccent()

	// Main star polygon (8-pointed).
	star := []struct{ x, y float64 }{
		{mx, my - outer},
		{mx + inner, my - inner},
		{mx + outer, my},
		{mx + inner, my + inner},
		{mx, my + outer},
		{mx - inner, my + inner},
		{mx - outer, my},
		{mx - inner, my - inner},
	}
	dc.NewSubPath()
	for i, p := range star {
		if i == 0 {
			dc.MoveTo(p.x, p.y)
		} else {
			dc.LineTo(p.x, p.y)
		}
	}
	dc.ClosePath()
	dc.SetColor(accent)
	dc.Fill()

	// Small star (offset up-right).
	smallX := mx + 16
	smallY := my - 14
	sOuter := 5.0
	sInner := 2.0
	dc.NewSubPath()
	smallStar := []struct{ x, y float64 }{
		{smallX, smallY - sOuter},
		{smallX + sInner, smallY - sInner},
		{smallX + sOuter, smallY},
		{smallX + sInner, smallY + sInner},
		{smallX, smallY + sOuter},
		{smallX - sInner, smallY + sInner},
		{smallX - sOuter, smallY},
		{smallX - sInner, smallY - sInner},
	}
	for i, p := range smallStar {
		if i == 0 {
			dc.MoveTo(p.x, p.y)
		} else {
			dc.LineTo(p.x, p.y)
		}
	}
	dc.ClosePath()
	dc.SetColor(accent)
	dc.Fill()

	// Wordmark.
	brandFace, _ := cachedFontFace(r.font, 28)
	dc.SetFontFace(brandFace)
	dc.SetColor(paperInk())
	dc.DrawString("AstrBot", float64(r.opts.Padding)+42, 27)

	// Version.
	if T2IVersion != "" {
		verFace, _ := cachedFontFace(r.font, 18)
		dc.SetFontFace(verFace)
		dc.SetColor(paperMuted())
		verText := "v" + T2IVersion
		vw, _ := dc.MeasureString(verText)
		dc.DrawString(verText, float64(r.opts.Width-r.opts.Padding)-vw, 31)
	}
}

// wrapMarkdownText wraps text to maxWidth using the renderer's font/size.
func (r *ImageRenderer) wrapMarkdownText(text string, maxWidth float64) []string {
	face, _ := cachedFontFace(r.font, r.opts.FontSize)
	mdc := gg.NewContext(1, 1)
	mdc.SetFontFace(face)
	return wrapSmart(mdc, text, maxWidth)
}

// wrapPreserveSpace wraps text preserving leading/trailing whitespace (for code).
func (r *ImageRenderer) wrapPreserveSpace(text string, maxWidth float64) []string {
	face, _ := cachedFontFace(r.font, r.opts.FontSize)
	mdc := gg.NewContext(1, 1)
	mdc.SetFontFace(face)
	var out []string
	for _, seg := range strings.Split(text, "\n") {
		if seg == "" {
			out = append(out, "")
			continue
		}
		cur := ""
		curW := 0.0
		for _, ch := range seg {
			cw, _ := mdc.MeasureString(string(ch))
			if curW+cw > maxWidth && cur != "" {
				out = append(out, cur)
				cur = ""
				curW = 0
			}
			cur += string(ch)
			curW += cw
		}
		if cur != "" {
			out = append(out, cur)
		}
	}
	return out
}

// --- ParagraphBlock ---

type paragraphBlock struct {
	content    string
	lines      []string
	lineHeight float64
	height     int
}

func (b *paragraphBlock) measure(r *ImageRenderer, width float64) int {
	face, _ := cachedFontFace(r.font, r.opts.FontSize)
	mdc := gg.NewContext(1, 1)
	mdc.SetFontFace(face)
	_, th := mdc.MeasureString("Ag")
	b.lineHeight = th + 8
	b.lines = r.wrapMarkdownText(b.content, width)
	b.height = int(float64(len(b.lines))*b.lineHeight) + 7
	return b.height
}

func (b *paragraphBlock) render(r *ImageRenderer, dc *gg.Context, x, y, width float64) float64 {
	face, _ := cachedFontFace(r.font, r.opts.FontSize)
	dc.SetFontFace(face)
	dc.SetColor(paperInk())
	textY := y
	for _, line := range b.lines {
		r.drawMixedLine(dc, line, x, textY, r.opts.FontSize, paperInk())
		textY += b.lineHeight
	}
	return y + float64(b.height)
}

// --- HeadingBlock ---

type headingBlock struct {
	content    string
	level      int
	lines      []string
	lineHeight float64
	topGap     float64
	bottomGap  float64
	height     int
}

func (b *headingBlock) measure(r *ImageRenderer, width float64) int {
	factors := []float64{1.84, 1.48, 1.28, 1.12, 1.0, 0.92}
	lvl := b.level
	if lvl < 1 {
		lvl = 1
	}
	if lvl > 6 {
		lvl = 6
	}
	headingSize := r.opts.FontSize * factors[lvl-1]
	if headingSize < 18 {
		headingSize = 18
	}
	face, _ := cachedFontFace(r.font, headingSize)
	mdc := gg.NewContext(1, 1)
	mdc.SetFontFace(face)
	_, th := mdc.MeasureString("Ag")
	b.lineHeight = th + 6
	b.lines = r.wrapMarkdownText(b.content, width)

	topGaps := []float64{0, 28, 22, 18, 15, 13}
	bottomGaps := []float64{14, 12, 10, 8, 7, 6}
	b.topGap = topGaps[lvl-1]
	b.bottomGap = bottomGaps[lvl-1]
	b.height = int(b.topGap + float64(len(b.lines))*b.lineHeight + b.bottomGap)
	return b.height
}

func (b *headingBlock) render(r *ImageRenderer, dc *gg.Context, x, y, width float64) float64 {
	factors := []float64{1.84, 1.48, 1.28, 1.12, 1.0, 0.92}
	lvl := b.level
	if lvl < 1 {
		lvl = 1
	}
	if lvl > 6 {
		lvl = 6
	}
	headingSize := r.opts.FontSize * factors[lvl-1]
	if headingSize < 18 {
		headingSize = 18
	}
	face, _ := cachedFontFace(r.font, headingSize)
	dc.SetFontFace(face)
	dc.SetColor(paperInk())

	textY := y + b.topGap
	if lvl == 2 {
		ruleY := y + maxF(8, b.topGap-10)
		dc.SetColor(paperLine())
		dc.SetLineWidth(1)
		dc.DrawLine(x, ruleY, x+width, ruleY)
		dc.Stroke()
		dc.SetColor(paperInk())
	}
	for _, line := range b.lines {
		dc.DrawString(line, x, textY)
		textY += b.lineHeight
	}
	return y + float64(b.height)
}

// --- RuleBlock ---

type ruleBlock struct{}

func (b *ruleBlock) measure(r *ImageRenderer, width float64) int { return 30 }

func (b *ruleBlock) render(r *ImageRenderer, dc *gg.Context, x, y, width float64) float64 {
	dc.SetColor(paperLine())
	dc.SetLineWidth(1)
	dc.DrawLine(x, y+15, x+width, y+15)
	dc.Stroke()
	return y + 30
}

// --- QuoteBlock ---

type quoteBlock struct {
	content    string
	lines      []string
	lineHeight float64
	height     int
}

func (b *quoteBlock) measure(r *ImageRenderer, width float64) int {
	faceSize := r.opts.FontSize - 1
	if faceSize < 16 {
		faceSize = 16
	}
	face, _ := cachedFontFace(r.font, faceSize)
	mdc := gg.NewContext(1, 1)
	mdc.SetFontFace(face)
	_, th := mdc.MeasureString("Ag")
	b.lineHeight = th + 7

	b.lines = nil
	for _, src := range strings.Split(b.content, "\n") {
		b.lines = append(b.lines, r.wrapMarkdownText(src, width-32)...)
	}
	b.height = int(float64(len(b.lines))*b.lineHeight) + 28
	return b.height
}

func (b *quoteBlock) render(r *ImageRenderer, dc *gg.Context, x, y, width float64) float64 {
	faceSize := r.opts.FontSize - 1
	if faceSize < 16 {
		faceSize = 16
	}
	face, _ := cachedFontFace(r.font, faceSize)
	dc.SetFontFace(face)

	dc.SetColor(paperSoftFill())
	dc.DrawRectangle(x, y+4, width, float64(b.height)-8)
	dc.Fill()

	dc.SetColor(paperLine())
	dc.SetLineWidth(1)
	dc.DrawLine(x, y+4, x+width, y+4)
	dc.Stroke()
	dc.DrawLine(x, y+float64(b.height)-4, x+width, y+float64(b.height)-4)
	dc.Stroke()

	dc.SetColor(paperMuted())
	textY := y + 13
	for _, line := range b.lines {
		dc.DrawString(line, x+16, textY)
		textY += b.lineHeight
	}
	return y + float64(b.height)
}

// --- ListBlock ---

type listEntry struct {
	marker  string
	content string
	depth   int
	checked *bool
}

type listBlock struct {
	entries    []listEntry
	lines      [][]string
	lineHeight float64
	height     int
}

func (b *listBlock) measure(r *ImageRenderer, width float64) int {
	face, _ := cachedFontFace(r.font, r.opts.FontSize)
	mdc := gg.NewContext(1, 1)
	mdc.SetFontFace(face)
	_, th := mdc.MeasureString("Ag")
	b.lineHeight = th + 7

	b.lines = nil
	totalH := 6.0
	for _, entry := range b.entries {
		indent := float64(min(entry.depth, 3) * 22)
		avail := width - 34 - indent
		if avail < 40 {
			avail = 40
		}
		wrapped := r.wrapMarkdownText(entry.content, avail)
		b.lines = append(b.lines, wrapped)
		totalH += float64(len(wrapped))*b.lineHeight + 5
	}
	b.height = int(totalH) + 3
	return b.height
}

func (b *listBlock) render(r *ImageRenderer, dc *gg.Context, x, y, width float64) float64 {
	face, _ := cachedFontFace(r.font, r.opts.FontSize)
	dc.SetFontFace(face)
	textY := y + 6
	for i, entry := range b.entries {
		indent := float64(min(entry.depth, 3) * 22)
		markerX := x + indent
		textX := markerX + 30

		if entry.checked == nil {
			dc.SetColor(paperAccent())
			dc.DrawString(entry.marker, markerX, textY)
		} else {
			boxTop := textY + 6
			dc.SetColor(paperAccent())
			dc.SetLineWidth(2)
			dc.DrawRectangle(markerX+2, boxTop, 14, 14)
			dc.Stroke()
			if *entry.checked {
				dc.DrawLine(markerX+5, boxTop+7, markerX+9, boxTop+11)
				dc.Stroke()
				dc.DrawLine(markerX+9, boxTop+11, markerX+15, boxTop+3)
				dc.Stroke()
			}
		}

		dc.SetColor(paperInk())
		for _, line := range b.lines[i] {
			dc.DrawString(line, textX, textY)
			textY += b.lineHeight
		}
		textY += 5
	}
	return y + float64(b.height)
}

// --- CodeBlock ---

type codeBlock struct {
	content    string
	language   string
	lines      []string
	lineHeight float64
	labelH     float64
	height     int
}

func (b *codeBlock) measure(r *ImageRenderer, width float64) int {
	codeSize := r.opts.FontSize - 5
	if codeSize < 16 {
		codeSize = 16
	}
	face, _ := cachedFontFace(r.font, codeSize)
	mdc := gg.NewContext(1, 1)
	mdc.SetFontFace(face)
	_, th := mdc.MeasureString("Ag")
	b.lineHeight = th + 6
	if b.language != "" {
		b.labelH = 22
	}

	b.lines = nil
	for _, src := range strings.Split(b.content, "\n") {
		b.lines = append(b.lines, r.wrapPreserveSpace(src, width-30)...)
	}
	b.height = int(24 + b.labelH + float64(len(b.lines))*b.lineHeight)
	return b.height
}

func (b *codeBlock) render(r *ImageRenderer, dc *gg.Context, x, y, width float64) float64 {
	codeSize := r.opts.FontSize - 5
	if codeSize < 16 {
		codeSize = 16
	}
	face, _ := cachedFontFace(r.font, codeSize)
	dc.SetFontFace(face)

	dc.SetColor(paperCodeFill())
	dc.DrawRoundedRectangle(x, y+4, width, float64(b.height)-8, 4)
	dc.Fill()
	dc.SetColor(paperLine())
	dc.SetLineWidth(1)
	dc.DrawRoundedRectangle(x, y+4, width, float64(b.height)-8, 4)
	dc.Stroke()

	textY := y + 13
	if b.language != "" {
		labelSize := codeSize - 5
		if labelSize < 13 {
			labelSize = 13
		}
		labelFace, _ := cachedFontFace(r.font, labelSize)
		dc.SetFontFace(labelFace)
		dc.SetColor(paperMuted())
		label := strings.ToUpper(b.language)
		lw, _ := dc.MeasureString(label)
		dc.DrawString(label, x+width-lw-14, textY)
		dc.SetFontFace(face)
		textY += b.labelH
	}

	dc.SetColor(paperInk())
	for _, line := range b.lines {
		dc.DrawString(line, x+15, textY)
		textY += b.lineHeight
	}
	return y + float64(b.height)
}

// --- TableBlock ---

type tableBlock struct {
	headers    []string
	rows       []string
	aligns     []string
	colWidths  []float64
	rowHeights []float64
	lineHeight float64
	height     int
}

func (b *tableBlock) measure(r *ImageRenderer, width float64) int {
	tableSize := r.opts.FontSize - 4
	if tableSize < 16 {
		tableSize = 16
	}
	bodyFace, _ := cachedFontFace(r.font, tableSize)
	headerFace, _ := cachedFontFace(r.font, tableSize)
	_ = headerFace
	mdc := gg.NewContext(1, 1)
	mdc.SetFontFace(bodyFace)
	_, th := mdc.MeasureString("Ag")
	b.lineHeight = th + 6

	colCount := len(b.headers)
	if colCount == 0 {
		return 0
	}

	natural := make([]float64, colCount)
	allRows := append([][]string{b.headers}, parseTableRows(b.rows)...)
	for ci := 0; ci < colCount; ci++ {
		maxNat := 0.0
		for _, row := range allRows {
			cell := ""
			if ci < len(row) {
				cell = row[ci]
			}
			cw, _ := mdc.MeasureString(cell)
			if cw+24 > maxNat {
				maxNat = cw + 24
			}
		}
		if maxNat > width*0.55 {
			maxNat = width * 0.55
		}
		natural[ci] = maxNat
	}

	minW := width / float64(colCount*2)
	if minW > 78 {
		minW = 78
	}
	if minW < 42 {
		minW = 42
	}
	remaining := width - minW*float64(colCount)
	totalNat := 0.0
	for _, n := range natural {
		totalNat += n
	}
	if totalNat < 1 {
		totalNat = 1
	}

	b.colWidths = make([]float64, colCount)
	for i, n := range natural {
		b.colWidths[i] = minW + remaining*n/totalNat
	}
	sumW := 0.0
	for i := 0; i < colCount-1; i++ {
		sumW += b.colWidths[i]
	}
	b.colWidths[colCount-1] = width - sumW

	b.rowHeights = nil
	for _, row := range allRows {
		maxLines := 0
		for ci, cw := range b.colWidths {
			cell := ""
			if ci < len(row) {
				cell = row[ci]
			}
			wrapped := r.wrapMarkdownText(cell, cw-24)
			if len(wrapped) > maxLines {
				maxLines = len(wrapped)
			}
		}
		b.rowHeights = append(b.rowHeights, float64(maxLines)*b.lineHeight+20)
	}

	totalH := 0.0
	for _, h := range b.rowHeights {
		totalH += h
	}
	b.height = int(totalH) + 16
	return b.height
}

func (b *tableBlock) render(r *ImageRenderer, dc *gg.Context, x, y, width float64) float64 {
	tableSize := r.opts.FontSize - 4
	if tableSize < 16 {
		tableSize = 16
	}
	bodyFace, _ := cachedFontFace(r.font, tableSize)
	headerFace, _ := cachedFontFace(r.font, tableSize)

	tableY := y + 8
	tableH := 0.0
	for _, h := range b.rowHeights {
		tableH += h
	}

	allRows := append([][]string{b.headers}, parseTableRows(b.rows)...)
	rowY := tableY
	for ri, rowH := range b.rowHeights {
		var fill color.RGBA
		if ri == 0 {
			fill = color.RGBA{238, 241, 242, 255}
		} else if ri%2 == 0 {
			fill = color.RGBA{247, 248, 248, 255}
		} else {
			fill = color.RGBA{253, 253, 252, 255}
		}
		dc.SetColor(fill)
		dc.DrawRectangle(x, rowY, width, rowH)
		dc.Fill()

		colX := x
		var face font.Face
		if ri == 0 {
			face = headerFace
		} else {
			face = bodyFace
		}
		dc.SetFontFace(face)
		dc.SetColor(paperInk())

		for ci, cw := range b.colWidths {
			cell := ""
			if ci < len(allRows[ri]) {
				cell = allRows[ri][ci]
			}
			wrapped := r.wrapMarkdownText(cell, cw-24)
			textY := rowY + 10
			align := "left"
			if ci < len(b.aligns) {
				align = b.aligns[ci]
			}
			for _, line := range wrapped {
				lw, _ := dc.MeasureString(line)
				tx := colX + 12
				switch align {
				case "center":
					tx = colX + (cw-lw)/2
				case "right":
					tx = colX + cw - lw - 12
				}
				dc.DrawString(line, tx, textY)
				textY += b.lineHeight
			}
			colX += cw
		}
		rowY += rowH
	}

	dc.SetColor(paperStrongLine())
	dc.SetLineWidth(1)
	dc.DrawRectangle(x, tableY, width, tableH)
	dc.Stroke()

	rowY = tableY
	for _, h := range b.rowHeights[:len(b.rowHeights)-1] {
		rowY += h
		dc.DrawLine(x, rowY, x+width, rowY)
		dc.Stroke()
	}
	colX := x
	for _, cw := range b.colWidths[:len(b.colWidths)-1] {
		colX += cw
		dc.DrawLine(colX, tableY, colX, tableY+tableH)
		dc.Stroke()
	}

	return y + float64(b.height)
}

// --- MathBlock (display-math fallback) ---

type mathBlock struct {
	content    string
	lines      []string
	lineHeight float64
	height     int
}

func (b *mathBlock) measure(r *ImageRenderer, width float64) int {
	mathSize := r.opts.FontSize - 2
	if mathSize < 16 {
		mathSize = 16
	}
	face, _ := cachedFontFace(r.font, mathSize)
	mdc := gg.NewContext(1, 1)
	mdc.SetFontFace(face)
	_, th := mdc.MeasureString("Ag")
	b.lineHeight = th + 7

	b.lines = nil
	for _, src := range strings.Split(b.content, "\n") {
		b.lines = append(b.lines, r.wrapMarkdownText(strings.TrimSpace(src), width-28)...)
	}
	b.height = int(float64(len(b.lines))*b.lineHeight) + 24
	return b.height
}

func (b *mathBlock) render(r *ImageRenderer, dc *gg.Context, x, y, width float64) float64 {
	mathSize := r.opts.FontSize - 2
	if mathSize < 16 {
		mathSize = 16
	}
	face, _ := cachedFontFace(r.font, mathSize)
	dc.SetFontFace(face)
	dc.SetColor(paperInk())
	textY := y + 10
	for _, line := range b.lines {
		lw, _ := dc.MeasureString(line)
		dc.DrawString(line, x+maxF(0, (width-lw)/2), textY)
		textY += b.lineHeight
	}
	return y + float64(b.height)
}

// --- ImageBlock (placeholder for remote images) ---

type mdImageBlock struct {
	altText string
	height  int
}

func (b *mdImageBlock) measure(r *ImageRenderer, width float64) int {
	faceSize := r.opts.FontSize - 2
	if faceSize < 16 {
		faceSize = 16
	}
	face, _ := cachedFontFace(r.font, faceSize)
	mdc := gg.NewContext(1, 1)
	mdc.SetFontFace(face)
	_, th := mdc.MeasureString("Ag")
	lineH := th + 6
	fallback := "[Image unavailable: " + b.altText + "]"
	lines := r.wrapMarkdownText(fallback, width)
	b.height = int(float64(len(lines))*lineH) + 16
	return b.height
}

func (b *mdImageBlock) render(r *ImageRenderer, dc *gg.Context, x, y, width float64) float64 {
	faceSize := r.opts.FontSize - 2
	if faceSize < 16 {
		faceSize = 16
	}
	face, _ := cachedFontFace(r.font, faceSize)
	dc.SetFontFace(face)
	dc.SetColor(paperMuted())
	textY := y + 7
	fallback := "[Image unavailable: " + b.altText + "]"
	lines := r.wrapMarkdownText(fallback, width)
	mdc := gg.NewContext(1, 1)
	mdc.SetFontFace(face)
	_, th := mdc.MeasureString("Ag")
	lineH := th + 6
	for _, line := range lines {
		dc.DrawString(line, x, textY)
		textY += lineH
	}
	return y + float64(b.height)
}

// --- Markdown parser ---

var (
	reHeading   = regexp.MustCompile(`^\s*(#{1,6})\s+(.+?)\s*$`)
	reList      = regexp.MustCompile(`^(\s*)([-+*]|\d+[.)])\s+(.+)$`)
	reRule      = regexp.MustCompile(`^\s{0,3}((\*\s*){3,}|(-\s*){3,}|(_\s*){3,})$`)
	reImage     = regexp.MustCompile(`^\s*!\[([^\]]*)\]\((\S+?)(?:\s+"[^"]*")?\)\s*$`)
	reTableSep  = regexp.MustCompile(`^\s*\|?\s*:?-{3,}:?\s*(\|\s*:?-{3,}:?\s*)+\|?\s*$`)
	reTaskCheck = regexp.MustCompile(`^\[([ xX])\]\s+(.*)$`)
)

func parseTableRows(rows []string) [][]string {
	result := make([][]string, len(rows))
	for i, row := range rows {
		result[i] = splitTableRow(row)
	}
	return result
}

func splitTableRow(line string) []string {
	line = strings.TrimSpace(line)
	line = strings.Trim(line, "|")
	parts := strings.Split(line, "|")
	for i := range parts {
		parts[i] = strings.TrimSpace(parts[i])
	}
	return parts
}

func parseMarkdownBlocks(text string) []mdBlock {
	text = strings.ReplaceAll(text, "\r\n", "\n")
	text = strings.ReplaceAll(text, "\r", "\n")
	expanded := ""
	for _, ch := range text {
		if ch == '\t' {
			expanded += "    "
		} else {
			expanded += string(ch)
		}
	}
	text = expanded

	lines := strings.Split(text, "\n")
	var blocks []mdBlock
	i := 0
	for i < len(lines) {
		line := lines[i]
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			i++
			continue
		}

		if strings.HasPrefix(trimmed, "```") {
			lang := strings.TrimPrefix(trimmed, "```")
			lang = strings.TrimSpace(lang)
			i++
			var codeLines []string
			for i < len(lines) && !strings.HasPrefix(strings.TrimSpace(lines[i]), "```") {
				codeLines = append(codeLines, lines[i])
				i++
			}
			if i < len(lines) {
				i++
			}
			blocks = append(blocks, &codeBlock{content: strings.Join(codeLines, "\n"), language: lang})
			continue
		}

		if strings.HasPrefix(trimmed, "$$") {
			if strings.HasSuffix(trimmed, "$$") && len(trimmed) > 4 {
				mathContent := strings.TrimSpace(trimmed[2 : len(trimmed)-2])
				i++
				blocks = append(blocks, &mathBlock{content: mathContent})
			} else {
				var mathLines []string
				remainder := strings.TrimSpace(trimmed[2:])
				if remainder != "" {
					mathLines = append(mathLines, remainder)
				}
				i++
				for i < len(lines) && !strings.HasSuffix(strings.TrimSpace(lines[i]), "$$") {
					mathLines = append(mathLines, lines[i])
					i++
				}
				if i < len(lines) {
					closing := strings.TrimSpace(lines[i])
					if closing != "$$" {
						mathLines = append(mathLines, closing[:len(closing)-2])
					}
					i++
				}
				blocks = append(blocks, &mathBlock{content: strings.Join(mathLines, "\n")})
			}
			continue
		}

		if m := reImage.FindStringSubmatch(line); m != nil {
			blocks = append(blocks, &mdImageBlock{altText: m[1]})
			i++
			continue
		}

		if m := reHeading.FindStringSubmatch(line); m != nil {
			level := len(m[1])
			blocks = append(blocks, &headingBlock{content: m[2], level: level})
			i++
			continue
		}

		if reRule.MatchString(line) {
			blocks = append(blocks, &ruleBlock{})
			i++
			continue
		}

		if i+1 < len(lines) && strings.Contains(lines[i], "|") && reTableSep.MatchString(lines[i+1]) {
			headers := splitTableRow(lines[i])
			sepCells := splitTableRow(lines[i+1])
			var aligns []string
			for _, cell := range sepCells {
				c := strings.TrimSpace(cell)
				if strings.HasPrefix(c, ":") && strings.HasSuffix(c, ":") {
					aligns = append(aligns, "center")
				} else if strings.HasSuffix(c, ":") {
					aligns = append(aligns, "right")
				} else {
					aligns = append(aligns, "left")
				}
			}
			i += 2
			var rows []string
			for i < len(lines) && strings.Contains(lines[i], "|") && strings.TrimSpace(lines[i]) != "" {
				rows = append(rows, lines[i])
				i++
			}
			blocks = append(blocks, &tableBlock{headers: headers, rows: rows, aligns: aligns})
			continue
		}

		if strings.HasPrefix(trimmed, ">") {
			var quoteLines []string
			for i < len(lines) && strings.HasPrefix(strings.TrimSpace(lines[i]), ">") {
				q := strings.TrimSpace(lines[i])
				q = strings.TrimPrefix(q, ">")
				q = strings.TrimSpace(q)
				quoteLines = append(quoteLines, q)
				i++
			}
			blocks = append(blocks, &quoteBlock{content: strings.Join(quoteLines, "\n")})
			continue
		}

		if m := reList.FindStringSubmatch(line); m != nil {
			marker := m[2]
			var entries []listEntry
			for i < len(lines) {
				itemMatch := reList.FindStringSubmatch(lines[i])
				if itemMatch != nil {
					ind, mk, ct := itemMatch[1], itemMatch[2], itemMatch[3]
					checked := (*bool)(nil)
					if tm := reTaskCheck.FindStringSubmatch(ct); tm != nil {
						val := tm[1] == "x" || tm[1] == "X"
						checked = &val
						ct = tm[2]
					}
					visMarker := marker
					if mk[0] >= '0' && mk[0] <= '9' {
						visMarker = mk
					} else {
						visMarker = "•"
					}
					entries = append(entries, listEntry{
						marker:  visMarker,
						content: ct,
						depth:   min(len(ind)/2, 3),
						checked: checked,
					})
					i++
					continue
				}
				if len(entries) > 0 && strings.HasPrefix(lines[i], "  ") && strings.TrimSpace(lines[i]) != "" {
					entries[len(entries)-1].content += " " + strings.TrimSpace(lines[i])
					i++
					continue
				}
				break
			}
			blocks = append(blocks, &listBlock{entries: entries})
			continue
		}

		// Paragraph: collect lines until a block boundary.
		paraLines := []string{trimmed}
		i++
		for i < len(lines) {
			next := lines[i]
			nextTrim := strings.TrimSpace(next)
			if nextTrim == "" {
				break
			}
			if strings.HasPrefix(nextTrim, "```") || strings.HasPrefix(nextTrim, "$$") ||
				strings.HasPrefix(nextTrim, ">") || reHeading.MatchString(next) ||
				reRule.MatchString(next) || reList.MatchString(next) ||
				reImage.FindStringSubmatch(next) != nil {
				break
			}
			if i+1 < len(lines) && strings.Contains(next, "|") && reTableSep.MatchString(lines[i+1]) {
				break
			}
			paraLines = append(paraLines, nextTrim)
			i++
		}
		blocks = append(blocks, &paragraphBlock{content: strings.Join(paraLines, "\n")})
	}

	return blocks
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

// RenderRemote renders text via a remote t2i service（对齐 Python 原版
// network_strategy.render：取本地模板内容后 POST JSON 到 /text2img/generate）。
func RenderRemote(endpoint, text, templateName string) ([]byte, error) {
	if templateName == "" {
		templateName = "base"
	}
	// tmpl 传完整 HTML 模板内容（对齐 Python 原版 network_strategy.render：
	// get_template(name) 取模板后 POST {tmpl, json:false, tmpldata:{text, version}}）。
	// 传模板名的"base" 字符串会被服务端当字面内容渲染（图片只显示 "base"）。
	// endpoint 为空时交由 RenderCustomTemplate 解析官方默认端点列表。
	tmpl, err := t2iTemplateContent(templateName)
	if err != nil {
		return nil, err
	}
	data := map[string]interface{}{"text": text}
	if T2IVersion != "" {
		data["version"] = T2IVersion
	}
	payload, _ := json.Marshal(data)
	return RenderCustomTemplate(endpoint, tmpl, string(payload), "")
}

// RenderCustomTemplate renders an HTML template + data via a remote t2i service
// (Python AstrBot 新版协议)：POST JSON {tmpl, tmpldata, options, json:false}
// 到 endpoint 的 /text2img/generate 路径并读取返回的图片字节。响应可能是
// 原始图片数据或 JSON envelope {code, data:{url|base64|id}}；其余视为错误。
//
// 路径约定：endpoint 为 t2i 服务根地址。若以 /t2i 或 /t2i/ 结尾（旧 RenderRemote
// 的 endpoint 约定），取其父路径再拼 /text2img/generate；已是 /text2img 结尾则
// 追加 /generate（对齐 Python 原版 ASTRBOT_T2I_DEFAULT_ENDPOINT +
// _clean_url + f"{endpoint}/generate"）。
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

// generateURL 规范化 t2i 服务根地址为 /text2img/generate 端点（对齐 Python
// network_strategy._clean_url + f"{endpoint}/generate"）。
func generateURL(endpoint string) string {
	url := strings.TrimRight(endpoint, "/")
	if strings.HasSuffix(url, "/t2i") {
		url = strings.TrimSuffix(url, "/t2i")
	}
	if !strings.HasSuffix(url, "/text2img") {
		url += "/text2img"
	}
	return url + "/generate"
}

// T2ITemplateDir 是用户 t2i HTML 模板目录（<data>/t2i_templates），由宿主
// （dashboard.NewServer）初始化时注入；RenderRemote 按模板名读取内容。
var T2ITemplateDir string

// T2IVersion 是宿主版本号（如 "1.2.1"），注入 t2i 模板的 {{ version }} 变量
// （对齐 Python 原版 {version: f"v{VERSION}"}）。空串时模板标题省略版本。
var T2IVersion string

// T2IDefaultTemplate 是内置 "base" 模板内容（dashboard t2iDefaultTemplate），
// 用户模板缺失时的回退，由宿主注入。
var T2IDefaultTemplate string

// t2iTemplateContent 返回模板 HTML 内容：优先用户模板目录
// （T2ITemplateDir/<name>.html，防路径穿越），缺失回退内置 base 模板。
func t2iTemplateContent(name string) (string, error) {
	if name != "" && name != "." && name != ".." && !strings.ContainsAny(name, `/\\`) && T2ITemplateDir != "" {
		if data, err := os.ReadFile(filepath.Join(T2ITemplateDir, name+".html")); err == nil {
			return string(data), nil
		}
	}
	if T2IDefaultTemplate != "" {
		return T2IDefaultTemplate, nil
	}
	return "", fmt.Errorf("t2i: template %q not found", name)
}

func renderCustomTemplateAt(endpoint, template, data, options string) ([]byte, error) {
	// options 默认值对齐 Python render_custom_template：full_page+type=jpeg
	// +quality=40，调用方传入的 options 覆盖默认。
	defaultOptions := map[string]interface{}{
		"full_page": true,
		"type":      "jpeg",
		"quality":   40,
	}
	if options != "" {
		var overrides map[string]interface{}
		if json.Unmarshal([]byte(options), &overrides) == nil {
			for k, v := range overrides {
				defaultOptions[k] = v
			}
		}
	}
	// tmpldata 对齐 Python：dict（Jinja 模板数据，含 text/version）。Go 端 data
	// 为 JSON 字符串，解析为对象；解析失败时原样作为字符串传递（服务端容错）。
	// 注意：没有顶层 text 字段——文本在 tmpldata 里，模板内容在 tmpl 里。
	var tmplData interface{} = map[string]interface{}{}
	if data != "" {
		var m map[string]interface{}
		if json.Unmarshal([]byte(data), &m) == nil {
			tmplData = m
		} else {
			tmplData = data
		}
	}
	post := map[string]interface{}{
		"tmpl":     template,
		"json":     false,
		"tmpldata": tmplData,
		"options":  defaultOptions,
	}
	body, err := json.Marshal(post)
	if err != nil {
		return nil, err
	}

	// 用默认 Transport（ProxyFromEnvironment）：内网 t2i 端点靠 NO_PROXY
	// （应为 CIDR，如 192.168.0.0/16 / 172.16.0.0/12）直连；外网 t2i 端点走代理。
	// 不能用 Proxy=nil 硬编码直连，否则外网端点无法走代理。
	client := &http.Client{Timeout: 120 * time.Second}
	req, err := http.NewRequest(http.MethodPost, generateURL(endpoint), bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("t2i remote html render request: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("t2i remote html render returned HTTP %d", resp.StatusCode)
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if err != nil {
		return nil, err
	}
	if isImageData(raw) {
		return raw, nil
	}
	// JSON envelope 兜底（对齐 Python return_url 模式：data.id → /text2img/data/<id>；
	// 以及旧协议的 url/base64）。
	var env struct {
		Data struct {
			URL    string `json:"url"`
			Base64 string `json:"base64"`
			ID     string `json:"id"`
		} `json:"data"`
	}
	if json.Unmarshal(raw, &env) == nil {
		if env.Data.Base64 != "" {
			img, err := decodeBase64Image(env.Data.Base64)
			if err != nil {
				return nil, err
			}
			return img, nil
		}
		if env.Data.URL != "" {
			return fetchRemoteImage(env.Data.URL)
		}
		if env.Data.ID != "" {
			base := strings.TrimSuffix(strings.TrimRight(endpoint, "/"), "/text2img")
			return fetchRemoteImage(base + "/text2img/data/" + strings.TrimPrefix(env.Data.ID, "data/"))
		}
	}
	return nil, fmt.Errorf("t2i remote html render: unrecognized response (%d bytes)", len(raw))
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
	t2iEndpointsAt    time.Time
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
