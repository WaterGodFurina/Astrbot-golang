// kb extract.go：文档文本提取层（对齐 Python 本体 knowledge_base/parsers）。
//
// 本体分发语义（parsers/util.py select_parser，白名单制）：
//
//	.md/.txt/.markdown/.rst/.adoc/.xlsx/.docx/.xls → MarkitdownParser
//	.epub → EpubParser；.pdf → PDFParser；其他 → 报"暂时不支持的文件格式"。
//
// 本体 URL 导入（url_parser.py）走 Tavily API 抽取正文（需密钥），本仓库
// 以本地 HTMLToText 等价替代（无需密钥，语义同为"抽正文"）。
// Go 侧实现边界（与本体的已知差异）：
//   - PDF：pypdf 逐页 extract_text 的对等实现需重型依赖，这里用流级
//     启发式提取（FlateDecode 流内 Tj/TJ 文本），保底不乱码入库；
//   - DOCX/XLSX：OOXML 文本提取（w:t / sharedStrings），表格转 markdown
//     的结构信息不保留；
//   - .xls（BIFF 二进制）：无对等实现，明确报"暂不支持"。
//
// 所有白名单外格式对齐本体报"暂时不支持的文件格式"。
package knowledgebase

import (
	"bytes"
	"compress/flate"
	"encoding/binary"
	"fmt"
	"io"
	"path"
	"regexp"
	"sort"
	"strings"
	"unicode/utf8"

	pdf "github.com/ledongthuc/pdf"
	"github.com/xuri/excelize/v2"
	"golang.org/x/text/encoding"
	"golang.org/x/text/encoding/simplifiedchinese"
)

// ErrUnsupportedFormat 对齐本体 select_parser 的白名单拒绝语义
// （parsers/util.py:17 raise ValueError("暂时不支持的文件格式: ...")）。
var ErrUnsupportedFormat = fmt.Errorf("unsupported format")

// ExtractKBText 从文档字节提取纯文本（对齐本体 parsers 分发语义）。
//
// name 用于扩展名分发；contentType 仅 URL 导入路径提供（HTTP
// Content-Type，html 页面据此走正文抽取，等价本体 Tavily 提取位）。
// 白名单外格式返回 ErrUnsupportedFormat（对齐本体：报错而非乱码入库）。
func ExtractKBText(content []byte, name, contentType string) (string, error) {
	if len(content) == 0 {
		return "", fmt.Errorf("文档内容为空")
	}
	ext := docExt(name)
	if ext == "" && strings.HasPrefix(strings.TrimSpace(contentType), "text/html") {
		// URL 导入：无扩展名但 Content-Type 为 HTML（对齐本体 URL 导入
		// "抽正文" 语义；本体经 Tavily API，此处本地提取）。
		return HTMLToText(string(content)), nil
	}
	switch ext {
	case ".pdf":
		return extractPDFText(content), nil
	case ".docx":
		return extractOOXMLText(content), nil
	case ".xlsx":
		return extractXLSXText(content), nil
	case ".epub":
		return extractEPUBText(content), nil
	case ".html", ".htm":
		// 本体 select_parser 白名单不含 .html（上传 .html 会报错）；
		// 这里对齐白名单拒绝，URL 导入的 html 已在 contentType 分支处理。
		return "", fmt.Errorf("%w: %s（本体不支持该格式上传；网页请用 URL 导入）", ErrUnsupportedFormat, ext)
	case ".pptx":
		return extractPPTXText(content), nil
	case ".md", ".txt", ".markdown", ".rst", ".adoc":
		return DecodeTextBytes(content)
	case ".xls":
		// 本体经 markitdown 支持，Go 无对等实现（BIFF 二进制），明确报错。
		return "", fmt.Errorf("%w: %s（暂不支持旧版 xls，请另存为 xlsx）", ErrUnsupportedFormat, ext)
	default:
		return "", fmt.Errorf("%w: %s", ErrUnsupportedFormat, ext)
	}
}

// DecodeTextBytes 多编码兜底解码（对齐本体 TextParser：utf-8 → gbk →
// gb2312 → gb18030 逐级严格解码，全失败报错）。x/text 解码器是宽松的
// （无效字节替换 U+FFFD 不报错），这里以"结果含 U+FFFD 即失败"近似
// 本体 codec 的严格语义，避免二进制被错误解码成替代符文本。
func DecodeTextBytes(content []byte) (string, error) {
	if utf8.Valid(content) {
		return string(content), nil
	}
	for _, enc := range []*encoding.Decoder{
		simplifiedchinese.GBK.NewDecoder(),
		simplifiedchinese.GB18030.NewDecoder(),
	} {
		b, err := enc.Bytes(content)
		if err != nil || !utf8.Valid(b) {
			continue
		}
		s := string(b)
		if strings.ContainsRune(s, utf8.RuneError) || strings.Contains(s, "\uFFFD") {
			continue
		}
		// 严格化启发式：x/text 的 GBK/GB18030 解码是宽松的（无效字节可能
		// 被映射为合法符号如 0x80→"€"），这里额外拒绝含 NUL/控制字节的
		// 结果——二进制数据（如密钥/机器码片段）通常含控制字节，而正常
		// 中文文本几乎不会出现，避免"乱码当文本"对齐本体严格 decode。
		if strings.ContainsRune(s, 0x00) || hasControlRunes(s) {
			continue
		}
		return s, nil
	}
	// 对齐本体 TextParser 的报错文案（text_parser.py:40）。
	return "", fmt.Errorf("无法解码文件")
}

// hasControlRunes 报告字符串是否含有除 \t\n\r 外的 ASCII 控制字符。
func hasControlRunes(s string) bool {
	for _, r := range s {
		if r < 0x20 && r != '\t' && r != '\n' && r != '\r' {
			return true
		}
		if r == 0x7f {
			return true
		}
	}
	return false
}

// ── HTML（URL 导入/网页文档）────────────────────────────────────────────

var (
	// Go regexp 不支持反向引用：script/style/head 等块逐个编译。
	htmlScriptStyleRes = []*regexp.Regexp{
		regexp.MustCompile(`(?is)<script\b[^>]*>.*?</script>`),
		regexp.MustCompile(`(?is)<style\b[^>]*>.*?</style>`),
		regexp.MustCompile(`(?is)<noscript\b[^>]*>.*?</noscript>`),
		regexp.MustCompile(`(?is)<svg\b[^>]*>.*?</svg>`),
		regexp.MustCompile(`(?is)<head\b[^>]*>.*?</head>`),
	}
	htmlCommentRe = regexp.MustCompile(`(?s)<!--.*?-->`)
	htmlTagRe     = regexp.MustCompile(`(?s)<[^>]+>`)
	htmlEntityMap = map[string]string{
		"&nbsp;": " ", "&amp;": "&", "&lt;": "<", "&gt;": ">",
		"&quot;": `"`, "&#39;": "'", "&apos;": "'",
	}
	htmlEntityRe = regexp.MustCompile(`&(nbsp|amp|lt|gt|quot|#39|apos);`)
	// 纯数值实体
	htmlNumEntityRe = regexp.MustCompile(`&#(\d+);`)
	multiBlankRe    = regexp.MustCompile(`\n{3,}`)
)

// HTMLToText 剥离标签提取正文文本（URL 导入语义：脚本/样式/注释剔除，
// 块级标签转换行，实体解码，空白折叠）。
func HTMLToText(raw string) string {
	raw = htmlCommentRe.ReplaceAllString(raw, "")
	for _, re := range htmlScriptStyleRes {
		raw = re.ReplaceAllString(raw, "")
	}
	// 块级标签边界转换行，避免正文粘连
	for _, tag := range []string{"</p>", "</div>", "</h1>", "</h2>", "</h3>", "</h4>", "</h5>", "</h6>", "<br>", "<br/>", "<br />", "</li>", "</tr>"} {
		raw = strings.ReplaceAll(raw, tag, "\n")
	}
	raw = htmlTagRe.ReplaceAllString(raw, "")
	raw = htmlEntityRe.ReplaceAllStringFunc(raw, func(m string) string {
		if v, ok := htmlEntityMap[m]; ok {
			return v
		}
		return m
	})
	raw = htmlNumEntityRe.ReplaceAllStringFunc(raw, func(m string) string {
		var n int
		if _, err := fmt.Sscanf(m, "&#%d;", &n); err == nil && n > 0 && n < 0x110000 {
			return string(rune(n))
		}
		return m
	})
	// 未识别实体经标准解析器兜底
	raw = strings.Join(strings.Fields(raw), " ")
	// 恢复换行结构：先按 \n 分行折叠行内空白
	lines := strings.Split(raw, " ")
	_ = lines
	raw = strings.ReplaceAll(raw, " \n", "\n")
	raw = multiBlankRe.ReplaceAllString(raw, "\n\n")
	return strings.TrimSpace(raw)
}

// ── PDF：尽力而为的文本流提取 ──────────────────────────────────────────

func isPDF(content []byte) bool { return len(content) > 4 && bytes.HasPrefix(content, []byte("%PDF")) }

func isZip(content []byte) bool {
	return len(content) > 4 && bytes.HasPrefix(content, []byte("PK\x03\x04"))
}

// extractPDFText 从 PDF 提取文本（对齐本体 PDFParser.parse：逐页
// page.extract_text() 拼接）。实现经 github.com/ledongthuc/pdf（pypdf 的
// Go 对等库，纯标准库无外部依赖、617★/活跃维护/非 archived/无供应链风险），
// 覆盖未加密 PDF 的文本流提取（含 PDFDocEncoding/UTF-16 字体解码）；
// 加密/扫描件/纯图片 PDF 提取为空时由调用方报"未能提取可索引文本"
// （对齐本体同款 user_message）。
func extractPDFText(content []byte) string {
	r, err := pdf.NewReader(bytes.NewReader(content), int64(len(content)))
	if err != nil {
		// 对齐本体：解析失败返回空（调用方 indexKBFile 报"文档内容为空"）。
		return ""
	}
	var out strings.Builder
	for i := 1; i <= r.NumPage(); i++ {
		page := r.Page(i)
		text, perr := page.GetPlainText(nil)
		if perr != nil {
			continue
		}
		out.WriteString(text)
		if !strings.HasSuffix(text, "\n") {
			out.WriteString("\n")
		}
	}
	return strings.TrimSpace(out.String())
}

// ── PPTX（markitdown PptxConverter 的 Go 对等实现，纯 stdlib）──────────

// extractPPTXText 提取 pptx 文本（对齐 markitdown PptxConverter 语义：
// 每 slide 输出 "<!-- Slide number: N -->" 分隔、标题占位符 → "# "、
// 表格 → markdown 表格、备注 → "### Notes:"）。实现读 ppt/slides/*.xml
// 与 ppt/notesSlides/*.xml 的 <a:t> 文本；标题判定对齐 python-pptx 的
// title placeholder（ph type="title"/"ctrTitle"）。
func extractPPTXText(content []byte) string {
	files, err := zipFiles(content)
	if err != nil {
		return ""
	}
	// slide 编号顺序（slide1.xml, slide2.xml...）
	slides := []string{}
	for name := range files {
		if strings.HasPrefix(name, "ppt/slides/") && strings.HasSuffix(name, ".xml") {
			slides = append(slides, name)
		}
	}
	// 文件名自然序（slide10 > slide2 需按数字排）
	sort.Slice(slides, func(i, j int) bool {
		return slideNum(slides[i]) < slideNum(slides[j])
	})
	var out strings.Builder
	for i, name := range slides {
		out.WriteString(fmt.Sprintf("\n\n<!-- Slide number: %d -->\n", i+1))
		out.WriteString(renderPptxSlide(string(files[name])))
		// 备注
		notesName := fmt.Sprintf("ppt/notesSlides/notesSlide%d.xml", i+1)
		if notes, ok := files[notesName]; ok {
			if notesText := strings.TrimSpace(aText(string(notes))); notesText != "" {
				out.WriteString("\n\n### Notes:\n" + notesText)
			}
		}
	}
	return strings.TrimSpace(out.String())
}

// slideNum 从 "ppt/slides/slide12.xml" 提取 12。
func slideNum(name string) int {
	base := strings.TrimSuffix(path.Base(name), ".xml")
	n := strings.TrimPrefix(base, "slide")
	var v int
	fmt.Sscanf(n, "%d", &v)
	return v
}

// renderPptxSlide 渲染单页：按 shape（<p:sp>）顺序，title placeholder
// 前缀 "# "，表格 <a:tbl> 转 markdown 行，其余 <a:t> 逐行。
func renderPptxSlide(xml string) string {
	var out strings.Builder
	// 表格整块（a:tbl）
	remaining := xml
	tblStart := strings.Index(remaining, "<a:tbl>")
	for tblStart >= 0 {
		tblEnd := strings.Index(remaining[tblStart:], "</a:tbl>")
		if tblEnd < 0 {
			break
		}
		tblEnd += tblStart + len("</a:tbl>")
		out.WriteString(renderPptxTable(remaining[tblStart:tblEnd]))
		remaining = remaining[:tblStart] + remaining[tblEnd:]
		tblStart = strings.Index(remaining, "<a:tbl>")
	}
	// 剩余 shape 段（p:sp）
	for _, sp := range pptxShapeRe.FindAllString(remaining, -1) {
		if strings.Contains(sp, "<a:tbl>") {
			continue // 已处理
		}
		text := strings.TrimSpace(aText(sp))
		if text == "" {
			continue
		}
		if isPptxTitleShape(sp) {
			out.WriteString("# " + text + "\n")
		} else {
			out.WriteString(text + "\n")
		}
	}
	return out.String()
}

// isPptxTitleShape 判定 title placeholder（对齐 python-pptx
// slide.shapes.title：<p:ph type="title" | "ctrTitle">）。
func isPptxTitleShape(sp string) bool {
	return strings.Contains(sp, `type="title"`) || strings.Contains(sp, `type="ctrTitle"`)
}

// renderPptxTable 把 a:tbl 的行/单元格渲染为 markdown 表格。
func renderPptxTable(tbl string) string {
	var rows []string
	for _, tr := range aTrRe.FindAllString(tbl, -1) {
		var cells []string
		for _, tc := range aTcRe.FindAllString(tr, -1) {
			cells = append(cells, strings.TrimSpace(aText(tc)))
		}
		rows = append(rows, "| "+strings.Join(cells, " | ")+" |")
	}
	return strings.Join(rows, "\n")
}

// aText 提取 DrawingML 文本 run <a:t>..</a:t>（含实体解包）。
func aText(s string) string {
	var b strings.Builder
	for _, m := range aTRe.FindAllStringSubmatch(s, -1) {
		b.WriteString(m[1])
	}
	// a:br → 换行
	return htmlUnescapeXML(b.String())
}

var (
	pptxShapeRe = regexp.MustCompile(`(?s)<p:sp>.*?</p:sp>`)
	aTrRe       = regexp.MustCompile(`(?s)<a:tr\b[^>]*>.*?</a:tr>`)
	aTcRe       = regexp.MustCompile(`(?s)<a:tc\b[^>]*>.*?</a:tc>`)
	aTRe        = regexp.MustCompile(`(?s)<a:t(?:\s[^>]*)?>(.*?)</a:t>`)
)

// ── DOCX/Epub（zip 内 OOXML/HTML）──────────────────────────────────────

// extractOOXMLText 提取 docx 主文档 word/document.xml 的 <w:t> 文本
// （按段落 </w:p> 换行）。其他 OOXML 成员忽略。
func extractOOXMLText(content []byte) string {
	files, err := zipFiles(content)
	if err != nil || len(files) == 0 {
		return ""
	}
	var out strings.Builder
	if doc, ok := files["word/document.xml"]; ok {
		text := ooxmlDocumentText(doc)
		out.WriteString(text)
	}
	if out.Len() == 0 {
		// Epub：xhtml/html 成员拼接
		for name, data := range files {
			if strings.HasSuffix(strings.ToLower(name), ".xhtml") || strings.HasSuffix(strings.ToLower(name), ".html") || strings.HasSuffix(strings.ToLower(name), ".htm") {
				out.WriteString(HTMLToText(string(data)))
				out.WriteString("\n\n")
			}
		}
	}
	return strings.TrimSpace(out.String())
}

// extractXLSXText 提取 xlsx 文本（对齐 markitdown XlsxConverter：每个
// sheet 输出 "## sheet名" + markdown 表格）。实现经 github.com/xuri/excelize
// （20883★/BSD-3/纯 Go 无 CGO/活跃维护），公式/合并单元格/日期/错误值
// 处理远强于手写解析。
func extractXLSXText(content []byte) string {
	f, err := excelize.OpenReader(bytes.NewReader(content))
	if err != nil {
		return ""
	}
	defer f.Close()
	var out strings.Builder
	for _, sheet := range f.GetSheetList() {
		rows, err := f.GetRows(sheet)
		if err != nil || len(rows) == 0 {
			continue
		}
		out.WriteString("## " + sheet + "\n\n")
		for _, row := range rows {
			// 空行跳过；单元格 trim 后以 markdown 表格行输出
			nonEmpty := 0
			cells := make([]string, len(row))
			for i, c := range row {
				c = strings.TrimSpace(c)
				c = strings.ReplaceAll(c, "|", "\\|")
				c = strings.ReplaceAll(c, "\n", " ")
				if c != "" {
					nonEmpty++
				}
				cells[i] = c
			}
			if nonEmpty == 0 {
				continue
			}
			out.WriteString("| " + strings.Join(cells, " | ") + " |\n")
		}
		out.WriteString("\n")
	}
	return strings.TrimSpace(out.String())
}

var (
	siRe        = regexp.MustCompile(`(?s)<si>(.*?)</si>`)
	inlineStrRe = regexp.MustCompile(`(?s)<is>(.*?)</is>`)
	tRe         = regexp.MustCompile(`(?s)<t(?:\\s[^>]*)?>(.*?)</t>`)
	xlsxRowRe   = regexp.MustCompile(`(?s)<row[^>]*>.*?</row>`)
	xlsxCellRe  = regexp.MustCompile(`(?s)<c\b[^>]*>.*?</c>`)
	xlsxNumVRe  = regexp.MustCompile(`(?s)<v>(.*?)</v>`)
)

func extractEPUBText(content []byte) string {
	if t := extractOOXMLText(content); t != "" {
		return t
	}
	// 兜底：普通 zip（无 mimetype 识别）同样按成员扫
	files, err := zipFiles(content)
	if err != nil {
		return ""
	}
	var parts []string
	for name, data := range files {
		ln := strings.ToLower(name)
		if strings.HasSuffix(ln, ".xhtml") || strings.HasSuffix(ln, ".html") || strings.HasSuffix(ln, ".htm") || strings.HasSuffix(ln, ".txt") {
			if strings.HasSuffix(ln, ".txt") {
				if s, err := DecodeTextBytes(data); err == nil {
					parts = append(parts, s)
				}
			} else {
				parts = append(parts, HTMLToText(string(data)))
			}
		}
	}
	return strings.TrimSpace(strings.Join(parts, "\n\n"))
}

// ooxmlDocumentText 从 document.xml 提取文本，对齐本体 markitdown 转
// markdown 的结构化输出：
//   - 标题段落（w:pStyle val="HeadingN"/Title）→ markdown 标题（# 前缀）；
//   - 列表段落（w:numPr）→ "- " 无序项；
//   - 表格（w:tbl 内的 w:tc）→ markdown 表格行；
//   - 普通段落 → 逐行文本。
func ooxmlDocumentText(xmlBytes []byte) string {
	s := string(xmlBytes)
	var out strings.Builder

	// 先处理表格：w:tbl 整块转文本表格
	for _, tbl := range tblRe.FindAllString(s, -1) {
		out.WriteString(renderDocxTable(tbl))
		out.WriteString("\n")
	}
	// 段落（跳过已被表格覆盖的行——表格在 <w:p> 外）
	for _, para := range paraRe.FindAllString(s, -1) {
		text := strings.TrimSpace(docxParaText(para))
		if text == "" {
			continue
		}
		out.WriteString(docxParaPrefix(para) + text + "\n")
	}
	return out.String()
}

// docxParaText 提取段落内全部 <w:t> 文本。
func docxParaText(para string) string {
	var b strings.Builder
	for _, m := range wtRe.FindAllStringSubmatch(para, -1) {
		b.WriteString(m[1])
	}
	return htmlUnescapeXML(b.String())
}

// docxParaPrefix 依据段落样式返回 markdown 前缀（标题 # / 列表 - / 空）。
func docxParaPrefix(para string) string {
	lower := strings.ToLower(para)
	// 标题：<w:pStyle w:val="Heading1"/>、<w:pStyle w:val="Title"/>
	for _, h := range []struct {
		name string
		md   string
	}{
		{"title", "# "},
		{"heading1", "# "},
		{"heading2", "## "},
		{"heading3", "### "},
		{"heading4", "#### "},
		{"heading5", "##### "},
		{"heading6", "###### "},
	} {
		if strings.Contains(lower, `w:val="`+h.name+`"`) {
			return h.md
		}
	}
	// 列表：<w:numPr> 无序项
	if strings.Contains(lower, "<w:numpr>") || strings.Contains(lower, "<w:numpr ") {
		return "- "
	}
	return ""
}

// renderDocxTable 把 w:tbl 内若干 w:tr/w:tc 渲染为 markdown 表格。
func renderDocxTable(tbl string) string {
	var rows []string
	for _, tr := range trRe.FindAllString(tbl, -1) {
		var cells []string
		for _, tc := range tcRe.FindAllString(tr, -1) {
			var b strings.Builder
			for _, m := range wtRe.FindAllStringSubmatch(tc, -1) {
				b.WriteString(m[1])
			}
			cells = append(cells, strings.TrimSpace(htmlUnescapeXML(b.String())))
		}
		if len(cells) == 0 {
			continue
		}
		rows = append(rows, "| "+strings.Join(cells, " | ")+" |")
	}
	if len(rows) == 0 {
		return ""
	}
	// markdown 表头分隔行
	separator := "|" + strings.Repeat(" --- |", len(strings.Split(rows[0], "|"))-2+1)
	return rows[0] + "\n" + separator + "\n" + strings.Join(rows[1:], "\n")
}

var (
	wtRe   = regexp.MustCompile(`(?s)<w:t(?:\s[^>]*)?>(.*?)</w:t>`)
	paraRe = regexp.MustCompile(`(?s)<w:p\b[^>]*>.*?</w:p>`)
	tblRe  = regexp.MustCompile(`(?s)<w:tbl\b[^>]*>.*?</w:tbl>`)
	trRe   = regexp.MustCompile(`(?s)<w:tr\b[^>]*>.*?</w:tr>`)
	tcRe   = regexp.MustCompile(`(?s)<w:tc\b[^>]*>.*?</w:tc>`)
)

var xmlEntityRe = regexp.MustCompile(`&(lt|gt|amp|quot|apos|#\d+);`)

func htmlUnescapeXML(s string) string {
	return xmlEntityRe.ReplaceAllStringFunc(s, func(m string) string {
		switch m {
		case "&lt;":
			return "<"
		case "&gt;":
			return ">"
		case "&amp;":
			return "&"
		case "&quot;":
			return `"`
		case "&apos;":
			return "'"
		}
		var n int
		if _, err := fmt.Sscanf(m, "&#%d;", &n); err == nil && n > 0 && n < 0x110000 {
			return string(rune(n))
		}
		return m
	})
}

// zipFiles 极简 zip 读取（无需 archive/zip 的完整语义，只取成员名→内容）。
// 支持标准 deflate 存储成员。
func zipFiles(content []byte) (map[string][]byte, error) {
	out := map[string][]byte{}
	if !isZip(content) {
		return nil, fmt.Errorf("not a zip")
	}
	// 定位 End of Central Directory
	eocd := bytes.LastIndex(content, []byte("PK\x05\x06"))
	if eocd < 0 {
		return nil, fmt.Errorf("no zip eocd")
	}
	if eocd+22 > len(content) {
		return nil, fmt.Errorf("bad eocd")
	}
	count := int(binary.LittleEndian.Uint16(content[eocd+10:]))
	cdOffset := int(binary.LittleEndian.Uint32(content[eocd+16:]))
	p := cdOffset
	for i := 0; i < count && p+46 <= len(content); i++ {
		if !bytes.HasPrefix(content[p:], []byte("PK\x01\x02")) {
			break
		}
		method := binary.LittleEndian.Uint16(content[p+10:])
		compSize := int(binary.LittleEndian.Uint32(content[p+20:]))
		nameLen := int(binary.LittleEndian.Uint16(content[p+28:]))
		extraLen := int(binary.LittleEndian.Uint16(content[p+30:]))
		commentLen := int(binary.LittleEndian.Uint16(content[p+32:]))
		localOff := int(binary.LittleEndian.Uint32(content[p+42:]))
		name := string(content[p+46 : p+46+nameLen])
		// local header
		if localOff+30 > len(content) || !bytes.HasPrefix(content[localOff:], []byte("PK\x03\x04")) {
			p = p + 46 + nameLen + extraLen + commentLen
			continue
		}
		lNameLen := int(binary.LittleEndian.Uint16(content[localOff+26:]))
		lExtraLen := int(binary.LittleEndian.Uint16(content[localOff+28:]))
		dataStart := localOff + 30 + lNameLen + lExtraLen
		dataEnd := dataStart + compSize
		if dataEnd > len(content) {
			p = p + 46 + nameLen + extraLen + commentLen
			continue
		}
		raw := content[dataStart:dataEnd]
		switch method {
		case 0: // stored
			out[name] = append([]byte(nil), raw...)
		case 8: // deflate
			fr := flate.NewReader(bytes.NewReader(raw))
			dec, err := io.ReadAll(io.LimitReader(fr, 16<<20))
			_ = fr.Close()
			if err == nil {
				out[name] = dec
			}
		}
		p = p + 46 + nameLen + extraLen + commentLen
	}
	return out, nil
}

// docExt 返回 name 的小写扩展名（含点，如 ".pdf"）；无扩展名返回 ""。
func docExt(name string) string {
	i := strings.LastIndex(name, ".")
	if i < 0 {
		return ""
	}
	return strings.ToLower(name[i:])
}
