package knowledgebase

import (
	"os"
	"strings"
	"testing"
)

// 端到端：真实文档（microsoft/markitdown 官方测试文件）→ 提取 → 分块
const kbDocsDir = "/tmp/opencode/kbdocs/"

func TestE2EMarkdownChunk(t *testing.T) {
	content, err := os.ReadFile(kbDocsDir + "sample.md")
	if err != nil {
		t.Skipf("测试文档未准备（外部拉取）: %v", err)
	}
	text := string(content)
	chunks := MarkdownChunk(text, MarkdownChunkOptions{ChunkSize: 1024, IncludeHeadingContext: true})
	if len(chunks) == 0 {
		t.Fatal("无 chunk")
	}
	// markitdown README 有 "## MarkItDown 的特性类标题"——至少一个 chunk 带标题路径
	withCtx := 0
	for _, c := range chunks {
		if strings.Contains(c, " > ") {
			withCtx++
		}
	}
	if withCtx == 0 {
		t.Fatalf("无 chunk 带标题路径前缀（前 3 chunk）: %.300s | %.300s | %.300s", chunks[0], chunks[min(1, len(chunks)-1)], chunks[min(2, len(chunks)-1)])
	}
	t.Logf("md: %d chunks, %d 带标题路径", len(chunks), withCtx)
}

func TestE2EDocxExtractChunk(t *testing.T) {
	// markitdown 官方 test.docx 无 Heading 样式（普通段落+表格）：验证
	// 提取+分块不炸 + 无标题正确回退固定窗口。
	content, err := os.ReadFile(kbDocsDir + "test.docx")
	if err != nil {
		t.Skipf("测试文档未准备（外部拉取）: %v", err)
	}
	text, err := ExtractKBText(content, "test.docx", "")
	if err != nil {
		t.Fatalf("docx 提取: %v", err)
	}
	if strings.TrimSpace(text) == "" {
		t.Fatal("docx 提取为空")
	}
	chunks := ChunkDocument(text, "test.docx", 512, 50)
	if len(chunks) == 0 {
		t.Fatal("docx 分块为空")
	}
	t.Logf("test.docx（无标题）：%d 字 → %d chunks（回退路径）", len(text), len(chunks))

	// 结构化路径：构造带 Heading1/列表/表格的 docx（对齐 markitdown
	// test_files 里带标题样式的文档行为）
	structuredDoc := `<w:document xmlns:w="http://schemas.openxmlformats.org/wordprocessingml/2006/main"><w:body>` +
		`<w:p><w:pPr><w:pStyle w:val="Heading1"/></w:pPr><w:r><w:t>部署手册</w:t></w:r></w:p>` +
		`<w:p><w:pPr><w:pStyle w:val="Heading2"/></w:pPr><w:r><w:t>数据库配置</w:t></w:r></w:p>` +
		`<w:p><w:r><w:t>` + strings.Repeat("数据库连接串说明。", 60) + `</w:t></w:r></w:p>` +
		`<w:p><w:pPr><w:pStyle w:val="Heading2"/></w:pPr><w:r><w:t>环境准备</w:t></w:r></w:p>` +
		`<w:p><w:r><w:t>安装 Docker 与依赖。</w:t></w:r></w:p>` +
		`</w:body></w:document>`
	stext := ooxmlDocumentText([]byte(structuredDoc))
	if !strings.Contains(stext, "# 部署手册") || !strings.Contains(stext, "## 数据库配置") {
		t.Fatalf("标题提取失败: %.200s", stext)
	}
	schunks := ChunkDocument(stext, "structured.docx", 512, 50)
	withCtx := 0
	for _, c := range schunks {
		// 父级标题路径前缀：单级为 "部署手册\n\n"，多级为 "A > B\n\n"
		if strings.HasPrefix(c, "部署手册\n\n") || strings.Contains(c, " > ") {
			withCtx++
		}
	}
	if withCtx == 0 {
		t.Fatalf("docx 结构化 chunk 无标题路径: %.300s", schunks[0])
	}
	t.Logf("structured.docx: %d chunks，%d 带标题路径", len(schunks), withCtx)
}

func TestE2EXlsxExtractChunk(t *testing.T) {
	content, err := os.ReadFile(kbDocsDir + "test.xlsx")
	if err != nil {
		t.Skipf("测试文档未准备（外部拉取）: %v", err)
	}
	text, err := ExtractKBText(content, "test.xlsx", "")
	if err != nil {
		t.Fatalf("xlsx: %v", err)
	}
	chunks := ChunkDocument(text, "test.xlsx", 1024, 50)
	if len(chunks) == 0 {
		t.Fatal("xlsx 分块为空")
	}
	// markitdown 语义：## sheet 名
	if !strings.Contains(text, "## ") {
		t.Fatalf("xlsx 无 sheet 标题: %.200s", text)
	}
	t.Logf("xlsx: %d chunks, text=%.200s", len(chunks), text)
}

func TestE2EPptxExtractChunk(t *testing.T) {
	content, err := os.ReadFile(kbDocsDir + "test.pptx")
	if err != nil {
		t.Skipf("测试文档未准备（外部拉取）: %v", err)
	}
	text, err := ExtractKBText(content, "test.pptx", "")
	if err != nil {
		t.Fatalf("pptx: %v", err)
	}
	if strings.TrimSpace(text) == "" {
		t.Fatal("pptx 提取为空")
	}
	chunks := ChunkDocument(text, "test.pptx", 1024, 50)
	t.Logf("pptx: %d 字 → %d chunks, 含 slide 标记=%v", len(text), len(chunks), strings.Contains(text, "<!-- Slide number:"))
	if len(chunks) == 0 {
		t.Fatal("pptx 分块为空")
	}
}

func TestE2EPdfExtractChunk(t *testing.T) {
	content, err := os.ReadFile(kbDocsDir + "test.pdf")
	if err != nil {
		t.Skipf("测试文档未准备（外部拉取）: %v", err)
	}
	text, err := ExtractKBText(content, "test.pdf", "")
	if err != nil {
		t.Fatalf("pdf: %v", err)
	}
	if strings.TrimSpace(text) == "" {
		t.Fatal("pdf 提取为空（ledongthuc/pdf 或测试 pdf 扫描件）")
	}
	chunks := ChunkDocument(text, "test.pdf", 512, 50)
	t.Logf("pdf: %d 字 → %d chunks, 首块=%.150s", len(text), len(chunks), chunks[0])
}

func TestE2EEpubExtractChunk(t *testing.T) {
	content, err := os.ReadFile(kbDocsDir + "test.epub")
	if err != nil {
		t.Skipf("测试文档未准备（外部拉取）: %v", err)
	}
	text, err := ExtractKBText(content, "test.epub", "")
	if err != nil {
		t.Fatalf("epub: %v", err)
	}
	if strings.TrimSpace(text) == "" {
		t.Fatal("epub 提取为空")
	}
	chunks := ChunkDocument(text, "test.epub", 1024, 50)
	t.Logf("epub: %d 字 → %d chunks", len(text), len(chunks))
}

func TestE2EXlsRejected(t *testing.T) {
	content, err := os.ReadFile(kbDocsDir + "test.xls")
	if err != nil {
		t.Skipf("测试文档未准备（外部拉取）: %v", err)
	}
	_, err = ExtractKBText(content, "test.xls", "")
	if err == nil || !strings.Contains(err.Error(), "暂不支持") {
		t.Fatalf("xls 应报暂不支持: %v", err)
	}
}

func TestE2EHTMLExtract(t *testing.T) {
	content, err := os.ReadFile(kbDocsDir + "test_blog.html")
	if err != nil {
		t.Skipf("测试文档未准备（外部拉取）: %v", err)
	}
	// URL 导入语义（Content-Type text/html）
	text, err := ExtractKBText(content, "import", "text/html; charset=utf-8")
	if err != nil {
		t.Fatalf("html: %v", err)
	}
	if strings.Contains(text, "<div") || strings.Contains(text, "<script") {
		t.Fatalf("html 标签未剥离: %.200s", text)
	}
	chunks := ChunkDocument(text, "import.txt", 512, 50)
	t.Logf("html: %d 字 → %d chunks", len(text), len(chunks))
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
