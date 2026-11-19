package renderer

import (
	"bytes"
	"fmt"
	"html/template"
	"regexp"
	"strings"

	"github.com/wjkoh/ghostbook/internal/store"
	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/ast"
	"github.com/yuin/goldmark/extension"
	"github.com/yuin/goldmark/parser"
	"github.com/yuin/goldmark/renderer"
	"github.com/yuin/goldmark/text"
	"github.com/yuin/goldmark/util"
)

type SummaryEntry struct {
	Slug          string
	SummaryTitle  string
	PartTitle     string
	StartsNewPart bool
	Depth         int32
	ParentSlug    string
	NumberLabel   string
}

type ChapterView struct {
	Slug                  string
	NumberLabel           string
	PartTitle             string
	StartsNewPart         bool
	Depth                 int32
	ParentSlug            string
	Title                 string
	Markdown              string
	RenderedHTML          template.HTML
	WordCount             int32
	OpenCommentCount      int32
	LastCommitSHA         string
	RevisionNumber        int32
	LastAuthor            string
	LastUpdatedISO        string
	PrevSlug              string
	PrevTitle             string
	NextSlug              string
	NextTitle             string
	IsPermalinkView       bool
	PermalinkURL          string
	LifecycleStatus       string
	IsLatest              bool
	CommitsBehind         int32
	LatestRevisionSHA     string
	LatestRevisionNumber  int32
	LatestLiveURL         string
	MovedToSlug           string
	MovedToTitle          string
	LifecycleCommitSHA    string
	LifecycleAuthor       string
	LifecycleTimestampISO string
	LifecycleMessage      string
	DiffToLatest          string
}

type CommitSummaryView struct {
	ShortSHA       string
	FullSHA        string
	RevisionNumber int32
	AuthorName     string
	AuthorEmail    string
	TimestampISO   string
	CommitMessage  string
	FilesChanged   []string
}

type BriefingChapterLink struct {
	Slug  string
	Label string
}

type TimelineMilestoneView struct {
	ID                int64
	StartRevision     int32
	EndRevision       int32
	StartCommitSHA    string
	EndCommitSHA      string
	TimestampISO      string
	StartTimestampISO string
	CommitCount       int
	Title             string
	Summary           string
	Chapters          []BriefingChapterLink
	Authors           string
}

type OpenCalloutView struct {
	Kind        string
	AuthorLabel string
	ChapterSlug string
	ChapterName string
	Excerpt     string
}

type AuthorProfileView struct {
	ID          string
	DisplayName string
	Email       string
	RoleBadge   string
}

type RenderedBook struct {
	BookTitle              string
	BookKicker             string
	MCPName                string
	Authors                []AuthorProfileView
	HeadCommitSHA          string
	HeadRevisionNumber     int32
	SummaryMarkdown        string
	Chapters               []*ChapterView
	BySlug                 map[string]*ChapterView
	RecentCommits          []CommitSummaryView
	RecentActivity         []store.AuditEntry
	TimelineEntries        []TimelineMilestoneView
	PendingTimelineCommits int
	PendingStartRevision   int32
	PendingEndRevision     int32
	OpenCallouts           []OpenCalloutView
}

func (b *RenderedBook) DisplayTitle() string {
	if b != nil && strings.TrimSpace(b.BookTitle) != "" {
		return strings.TrimSpace(b.BookTitle)
	}
	return "Ghostbook"
}

func (b *RenderedBook) DisplayKicker() string {
	if b != nil && strings.TrimSpace(b.BookKicker) != "" {
		return strings.TrimSpace(b.BookKicker)
	}
	return "Living Book"
}

func (b *RenderedBook) DisplayMCPName() string {
	if b != nil && strings.TrimSpace(b.MCPName) != "" {
		return strings.TrimSpace(b.MCPName)
	}
	return "ghostbook"
}

var (
	mdEngine = goldmark.New(
		goldmark.WithExtensions(
			extension.GFM,
			extension.Footnote,
			extension.DefinitionList,
			&ghostbookASTExtension{},
		),
		goldmark.WithParserOptions(
			parser.WithAutoHeadingID(),
		),
	)
	textAmpRe       = regexp.MustCompile(`(\\text\{[^}]*?)&([^}]*?\})`)
	mermaidEscaper  = strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;")
	safeInlineTagRe = regexp.MustCompile(`(?i)^<\s*(?:br\s*/?|/?(?:sup|sub|kbd|mark))\s*>$`)
)

func sanitizeLaTeXMath(expr string) string {
	// Fix every unescaped & inside \text{...} with a bounded iteration cap
	for iter := 0; iter < 16; iter++ {
		updated := textAmpRe.ReplaceAllStringFunc(expr, func(m string) string {
			var b strings.Builder
			b.Grow(len(m) + 4)
			for i := 0; i < len(m); i++ {
				if m[i] == '&' && (i == 0 || m[i-1] != '\\') {
					b.WriteString(`\&`)
				} else {
					b.WriteByte(m[i])
				}
			}
			return b.String()
		})
		if updated == expr {
			break
		}
		expr = updated
	}
	expr = strings.ReplaceAll(expr, "&", "&amp;")
	expr = strings.ReplaceAll(expr, "<", "&lt;")
	expr = strings.ReplaceAll(expr, ">", "&gt;")
	return expr
}

func isASCIIAlphaNum(b byte) bool {
	return (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z') || (b >= '0' && b <= '9')
}

func isMathStartByte(b byte) bool {
	return (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z') || b == '\\' || b == '(' || b == '{'
}

// findClosingDollar locates a closing unescaped '$' on the same line that is not
// preceded by whitespace and not followed by an alphanumeric character (preventing
// "$HOME and $PATH" or "$1B to $5B" from being misparsed as math).
func findClosingDollar(line []byte, start int) int {
	n := len(line)
	for j := start; j < n; j++ {
		if line[j] == '\n' {
			return -1
		}
		if line[j] == '$' && line[j-1] != '\\' && line[j-1] != ' ' && line[j-1] != '\t' {
			if j+1 < n && isASCIIAlphaNum(line[j+1]) {
				continue
			}
			return j
		}
	}
	return -1
}

// ============================================================================
// Goldmark AST Nodes & Extensions for Math Passthrough and Mermaid Diagrams
// ============================================================================

var (
	KindMathBlock    = ast.NewNodeKind("MathBlock")
	KindMathInline   = ast.NewNodeKind("MathInline")
	KindMermaidBlock = ast.NewNodeKind("MermaidBlock")
)

type mathBlockNode struct {
	ast.BaseBlock
	closeDelim string
	buf        bytes.Buffer
}

func (n *mathBlockNode) Kind() ast.NodeKind { return KindMathBlock }
func (n *mathBlockNode) Dump(source []byte, level int) {
	ast.DumpHelper(n, source, level, map[string]string{"Formula": n.buf.String()}, nil)
}

type mathInlineNode struct {
	ast.BaseInline
	Formula string
	Display bool
}

func (n *mathInlineNode) Kind() ast.NodeKind { return KindMathInline }
func (n *mathInlineNode) Dump(source []byte, level int) {
	ast.DumpHelper(n, source, level, map[string]string{"Formula": n.Formula}, nil)
}

type mermaidBlockNode struct {
	ast.BaseBlock
	Source string
}

func (n *mermaidBlockNode) Kind() ast.NodeKind { return KindMermaidBlock }
func (n *mermaidBlockNode) Dump(source []byte, level int) {
	ast.DumpHelper(n, source, level, map[string]string{"Source": n.Source}, nil)
}

type mathBlockParser struct{}

func (p *mathBlockParser) Trigger() []byte {
	return []byte{'$', '\\'}
}

func advanceLineContent(reader text.Reader, line []byte) {
	n := len(line)
	if n > 0 && line[n-1] == '\n' {
		n--
	}
	if n > 0 {
		reader.Advance(n)
	}
}

func (p *mathBlockParser) Open(parent ast.Node, reader text.Reader, pc parser.Context) (ast.Node, parser.State) {
	line, _ := reader.PeekLine()
	trimmed := bytes.TrimLeft(line, " \t")
	trimmedRight := bytes.TrimRight(trimmed, " \t\r\n")

	if bytes.HasPrefix(trimmed, []byte("$$")) {
		// Single-line $$...$$ is handled by mathInlineParser inside a standard paragraph so Goldmark never keeps the block open across subsequent lines.
		if len(trimmedRight) > 4 && bytes.HasSuffix(trimmedRight, []byte("$$")) {
			return nil, parser.NoChildren
		}
		rest := bytes.TrimSpace(trimmedRight[2:])
		node := &mathBlockNode{closeDelim: "$$"}
		if len(rest) > 0 {
			node.buf.Write(rest)
			node.buf.WriteByte('\n')
		}
		advanceLineContent(reader, line)
		return node, parser.NoChildren
	}

	if bytes.HasPrefix(trimmed, []byte(`\[`)) {
		// Single-line \[...\] is handled by mathInlineParser inside a standard paragraph.
		if len(trimmedRight) > 4 && bytes.HasSuffix(trimmedRight, []byte(`\]`)) {
			return nil, parser.NoChildren
		}
		rest := bytes.TrimSpace(trimmedRight[2:])
		node := &mathBlockNode{closeDelim: `\]`}
		if len(rest) > 0 {
			node.buf.Write(rest)
			node.buf.WriteByte('\n')
		}
		advanceLineContent(reader, line)
		return node, parser.NoChildren
	}

	return nil, parser.NoChildren
}

func (p *mathBlockParser) Continue(node ast.Node, reader text.Reader, pc parser.Context) parser.State {
	n := node.(*mathBlockNode)
	line, _ := reader.PeekLine()
	trimmed := bytes.TrimSpace(line)
	closeBytes := []byte(n.closeDelim)

	if bytes.HasSuffix(trimmed, closeBytes) {
		before := bytes.TrimSpace(trimmed[:len(trimmed)-len(closeBytes)])
		if len(before) > 0 {
			n.buf.Write(before)
		}
		advanceLineContent(reader, line)
		return parser.Close
	}

	n.buf.Write(line)
	advanceLineContent(reader, line)
	return parser.Continue | parser.NoChildren
}

func (p *mathBlockParser) Close(node ast.Node, reader text.Reader, pc parser.Context) {}
func (p *mathBlockParser) CanInterruptParagraph() bool                                { return true }
func (p *mathBlockParser) CanAcceptIndentedLine() bool                                { return true }

type mathInlineParser struct{}

func (p *mathInlineParser) Trigger() []byte {
	return []byte{'\\', '$'}
}

func (p *mathInlineParser) Parse(parent ast.Node, block text.Reader, pc parser.Context) ast.Node {
	line, _ := block.PeekLine()
	if len(line) < 2 {
		return nil
	}

	// 1. Handle \(...\) and \[...\] inline or single-line display delimiters
	if line[0] == '\\' {
		if line[1] == '(' {
			endIdx := bytes.Index(line[2:], []byte(`\)`))
			if endIdx >= 0 {
				inner := string(bytes.TrimSpace(line[2 : 2+endIdx]))
				block.Advance(2 + endIdx + 2)
				return &mathInlineNode{Formula: inner, Display: false}
			}
			return nil
		}
		if line[1] == '[' {
			endIdx := bytes.Index(line[2:], []byte(`\]`))
			if endIdx >= 0 {
				inner := string(bytes.TrimSpace(line[2 : 2+endIdx]))
				block.Advance(2 + endIdx + 2)
				return &mathInlineNode{Formula: inner, Display: true}
			}
			return nil
		}
		return nil
	}

	// 2. Handle $...$ and $$...$$ when not preceded by backslash or alphanumeric character
	if line[0] == '$' {
		prec := block.PrecendingCharacter()
		if prec == '\\' || (prec >= 0 && prec < 128 && isASCIIAlphaNum(byte(prec))) {
			return nil
		}

		// Inline/list-indented $$...$$ on a single line
		if line[1] == '$' {
			if len(line) < 5 {
				return nil
			}
			endIdx := bytes.Index(line[2:], []byte("$$"))
			if endIdx > 0 {
				inner := string(bytes.TrimSpace(line[2 : 2+endIdx]))
				block.Advance(2 + endIdx + 2)
				return &mathInlineNode{Formula: inner, Display: true}
			}
			return nil
		}

		// Currency & shell-variable safe single-dollar $...$ inline math
		if isMathStartByte(line[1]) {
			end := findClosingDollar(line, 2)
			if end > 1 {
				inner := string(line[1:end])
				block.Advance(end + 1)
				return &mathInlineNode{Formula: inner, Display: false}
			}
		}
	}

	return nil
}

type mermaidASTTransformer struct{}

func (t *mermaidASTTransformer) Transform(doc *ast.Document, reader text.Reader, pc parser.Context) {
	source := reader.Source()
	var toReplace []*ast.FencedCodeBlock

	_ = ast.Walk(doc, func(n ast.Node, entering bool) (ast.WalkStatus, error) {
		if !entering {
			return ast.WalkContinue, nil
		}
		if cb, ok := n.(*ast.FencedCodeBlock); ok {
			lang := strings.TrimSpace(string(cb.Language(source)))
			if strings.EqualFold(lang, "mermaid") {
				toReplace = append(toReplace, cb)
			}
		}
		return ast.WalkContinue, nil
	})

	for _, cb := range toReplace {
		var buf bytes.Buffer
		lines := cb.Lines()
		for i := 0; i < lines.Len(); i++ {
			seg := lines.At(i)
			buf.Write(seg.Value(source))
		}
		mNode := &mermaidBlockNode{Source: buf.String()}
		if parent := cb.Parent(); parent != nil {
			parent.ReplaceChild(parent, cb, mNode)
		}
	}
}

type ghostbookASTHTMLRenderer struct{}

func (r *ghostbookASTHTMLRenderer) RegisterFuncs(reg renderer.NodeRendererFuncRegisterer) {
	reg.Register(KindMathBlock, r.renderMathBlock)
	reg.Register(KindMathInline, r.renderMathInline)
	reg.Register(KindMermaidBlock, r.renderMermaidBlock)
	reg.Register(ast.KindRawHTML, r.renderSafeRawHTML)
}

func (r *ghostbookASTHTMLRenderer) renderSafeRawHTML(w util.BufWriter, source []byte, node ast.Node, entering bool) (ast.WalkStatus, error) {
	if !entering {
		return ast.WalkSkipChildren, nil
	}
	n := node.(*ast.RawHTML)
	var buf bytes.Buffer
	for i := 0; i < n.Segments.Len(); i++ {
		seg := n.Segments.At(i)
		buf.Write(seg.Value(source))
	}
	raw := strings.TrimSpace(buf.String())
	if safeInlineTagRe.MatchString(raw) {
		lower := strings.ToLower(raw)
		if strings.HasPrefix(lower, "<br") {
			_, _ = w.WriteString("<br>")
		} else {
			_, _ = w.WriteString(strings.NewReplacer(" ", "", "\t", "", "\n", "", "\r", "").Replace(lower))
		}
	}
	return ast.WalkSkipChildren, nil
}

func (r *ghostbookASTHTMLRenderer) renderMathBlock(w util.BufWriter, source []byte, node ast.Node, entering bool) (ast.WalkStatus, error) {
	if !entering {
		return ast.WalkContinue, nil
	}
	n := node.(*mathBlockNode)
	formula := sanitizeLaTeXMath(strings.TrimSpace(n.buf.String()))
	_, _ = w.WriteString(`<p>\[` + formula + `\]</p>` + "\n")
	return ast.WalkContinue, nil
}

func (r *ghostbookASTHTMLRenderer) renderMathInline(w util.BufWriter, source []byte, node ast.Node, entering bool) (ast.WalkStatus, error) {
	if !entering {
		return ast.WalkContinue, nil
	}
	n := node.(*mathInlineNode)
	formula := sanitizeLaTeXMath(n.Formula)
	if n.Display {
		_, _ = w.WriteString(`\[` + formula + `\]`)
	} else {
		_, _ = w.WriteString(`\(` + formula + `\)`)
	}
	return ast.WalkContinue, nil
}

func (r *ghostbookASTHTMLRenderer) renderMermaidBlock(w util.BufWriter, source []byte, node ast.Node, entering bool) (ast.WalkStatus, error) {
	if !entering {
		return ast.WalkContinue, nil
	}
	n := node.(*mermaidBlockNode)
	_, _ = w.WriteString(`<div class="mermaid-container"><pre class="mermaid">` + mermaidEscaper.Replace(n.Source) + `</pre></div>` + "\n")
	return ast.WalkContinue, nil
}

type ghostbookASTExtension struct{}

func (e *ghostbookASTExtension) Extend(m goldmark.Markdown) {
	m.Parser().AddOptions(
		parser.WithBlockParsers(
			util.Prioritized(&mathBlockParser{}, 50),
		),
		parser.WithInlineParsers(
			util.Prioritized(&mathInlineParser{}, 50),
		),
		parser.WithASTTransformers(
			util.Prioritized(&mermaidASTTransformer{}, 50),
		),
	)
	m.Renderer().AddOptions(
		renderer.WithNodeRenderers(
			util.Prioritized(&ghostbookASTHTMLRenderer{}, 50),
		),
	)
}

func extractHeadingsFromAST(doc ast.Node, source []byte) (string, []string) {
	var title string
	var headings []string

	_ = ast.Walk(doc, func(n ast.Node, entering bool) (ast.WalkStatus, error) {
		if !entering {
			return ast.WalkContinue, nil
		}
		h, ok := n.(*ast.Heading)
		if !ok || h.Level > 3 {
			return ast.WalkContinue, nil
		}
		var raw bytes.Buffer
		lines := h.Lines()
		for i := 0; i < lines.Len(); i++ {
			seg := lines.At(i)
			raw.Write(seg.Value(source))
		}
		hText := strings.TrimSpace(raw.String())
		hText = strings.ReplaceAll(hText, `\$`, `$`)
		hText = strings.ReplaceAll(hText, `\~`, `~`)
		if title == "" && h.Level == 1 {
			title = hText
		}
		headings = append(headings, hText)
		return ast.WalkSkipChildren, nil
	})

	if title == "" && len(headings) > 0 {
		title = headings[0]
	}
	return title, headings
}

func ExtractMermaidDiagrams(md string) []string {
	source := []byte(md)
	reader := text.NewReader(source)
	doc := mdEngine.Parser().Parse(reader)
	var diagrams []string
	_ = ast.Walk(doc, func(n ast.Node, entering bool) (ast.WalkStatus, error) {
		if !entering {
			return ast.WalkContinue, nil
		}
		if m, ok := n.(*mermaidBlockNode); ok {
			diagrams = append(diagrams, strings.TrimSpace(m.Source))
		}
		return ast.WalkContinue, nil
	})
	return diagrams
}

func RenderMarkdownToHTML(md string) (template.HTML, string, []string, int32, error) {
	source := []byte(md)
	reader := text.NewReader(source)
	doc := mdEngine.Parser().Parse(reader)
	title, headings := extractHeadingsFromAST(doc, source)

	var buf bytes.Buffer
	if err := mdEngine.Renderer().Render(&buf, source, doc); err != nil {
		return "", "", nil, 0, fmt.Errorf("goldmark render: %w", err)
	}
	rawHTML := buf.String()

	// Normalize relative Markdown chapter links (e.g. href="01-system-architecture.md") to /chapters/<slug>
	rawHTML = relativeMDLinkRe.ReplaceAllString(rawHTML, `href="/chapters/$1$2"`)

	words := int32(len(strings.Fields(md)))
	return template.HTML(rawHTML), title, headings, words, nil
}

var (
	relativeMDLinkRe     = regexp.MustCompile(`href="(?:\./)?([a-z0-9][a-z0-9-]*)\.md(#[^"]*)?"`)
	summaryPartHeadingRe = regexp.MustCompile(`^#{1,3}\s+(.+?)\s*$`)
	summaryBulletLinkRe  = regexp.MustCompile(`^([ \t]*)[-*+]\s+\[([^\]]+)\]\(([^)]+\.md)\)\s*$`)
	summaryPlainLinkRe   = regexp.MustCompile(`^\s*\[([^\]]+)\]\(([^)]+\.md)\)\s*$`)
)

// ParseSummaryMarkdown parses an mdBook-compatible src/SUMMARY.md file into ordered
// SummaryEntry records with first-class Part headers (# Part I — ...), indentation
// depth, parent slug tracking, and hierarchical chapter numbering (1, 1.1, 1.2, 2).
func ParseSummaryMarkdown(summaryText string) []SummaryEntry {
	lines := strings.Split(summaryText, "\n")
	var entries []SummaryEntry
	seen := make(map[string]bool)

	currentPart := ""
	pendingNewPart := false
	var counters [4]int
	var parentByDepth [4]string

	for _, rawLine := range lines {
		line := strings.TrimRight(rawLine, "\r")
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			continue
		}

		if m := summaryPartHeadingRe.FindStringSubmatch(trimmed); len(m) == 2 {
			heading := strings.TrimSpace(m[1])
			lower := strings.ToLower(heading)
			if lower != "summary" && lower != "table of contents" {
				currentPart = heading
				pendingNewPart = true
			}
			continue
		}

		if m := summaryBulletLinkRe.FindStringSubmatch(line); len(m) == 4 {
			indentRaw := strings.ReplaceAll(m[1], "\t", "  ")
			depth := int32(len(indentRaw) / 2)
			if depth > 3 {
				depth = 3
			}
			title := strings.TrimSpace(m[2])
			slug := strings.TrimSuffix(strings.TrimSpace(m[3]), ".md")
			slug = strings.TrimPrefix(slug, "./")
			slug = strings.TrimPrefix(slug, "src/")
			if slug == "" || seen[slug] {
				continue
			}
			seen[slug] = true

			counters[depth]++
			for d := depth + 1; d < 4; d++ {
				counters[d] = 0
				parentByDepth[d] = ""
			}
			parentByDepth[depth] = slug

			parentSlug := ""
			if depth > 0 {
				parentSlug = parentByDepth[depth-1]
			}

			var numParts []string
			for d := int32(0); d <= depth; d++ {
				n := counters[d]
				if n == 0 {
					n = 1
				}
				numParts = append(numParts, fmt.Sprintf("%d", n))
			}

			entries = append(entries, SummaryEntry{
				Slug:          slug,
				SummaryTitle:  title,
				PartTitle:     currentPart,
				StartsNewPart: pendingNewPart,
				Depth:         depth,
				ParentSlug:    parentSlug,
				NumberLabel:   strings.Join(numParts, "."),
			})
			pendingNewPart = false
			continue
		}

		if m := summaryPlainLinkRe.FindStringSubmatch(trimmed); len(m) == 3 {
			title := strings.TrimSpace(m[1])
			slug := strings.TrimSuffix(strings.TrimSpace(m[2]), ".md")
			slug = strings.TrimPrefix(slug, "./")
			slug = strings.TrimPrefix(slug, "src/")
			if slug == "" || seen[slug] {
				continue
			}
			seen[slug] = true
			parentByDepth[0] = slug
			entries = append(entries, SummaryEntry{
				Slug:          slug,
				SummaryTitle:  title,
				PartTitle:     currentPart,
				StartsNewPart: pendingNewPart,
				Depth:         0,
				ParentSlug:    "",
				NumberLabel:   "",
			})
			pendingNewPart = false
		}
	}

	return entries
}
