package renderer_test

import (
	"encoding/json"
	"encoding/xml"
	"html/template"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"reflect"
	"regexp"
	"strings"
	"testing"

	"github.com/wjkoh/ghostbook/internal/renderer"
	"github.com/wjkoh/ghostbook/internal/store"
)

func TestRenderMarkdownToHTML_MathCurrencyCodeAndMermaid(t *testing.T) {
	md := `# Chapter 2: Agent Workflow (\$1B to \$5B)

In prose, AUM ranges from $1B to $5B and $HOME and $PATH remain shell strings.
Inline paren math: \(\sum_{i=1}^n w_i r_i \ge 0\) and single-dollar math $r_p = w_1 r_1 + w_2 r_2$.

$$
\text{S&P 500 & R&D} = \frac{a_1 + a_2}{b_1 < b_2 > 0}
$$

\[
E[R] = r_f + \beta (R_m - r_f)
\]

Inline code like ` + "`$x_1 + x_2$`" + ` must stay literal code.

` + "```bash\n# This bash comment must NOT become a chapter heading\necho \"$HOME and $USER\"\n```" + `

` + "```mermaid\nflowchart LR\n  A[\"Author Agent\"] --> B[\"Git Repo\"]\n```" + `

## Section 2.1: Yield Sweep
`

	rendered, title, headings, words, err := renderer.RenderMarkdownToHTML(md)
	if err != nil {
		t.Fatalf("RenderMarkdownToHTML error: %v", err)
	}
	htmlStr := string(rendered)

	if title != "Chapter 2: Agent Workflow ($1B to $5B)" {
		t.Fatalf("unexpected title: %q", title)
	}
	if len(headings) != 2 || headings[1] != "Section 2.1: Yield Sweep" {
		t.Fatalf("expected exactly 2 headings (bash comment excluded), got %+v", headings)
	}
	if words <= 20 {
		t.Fatalf("expected positive word count, got %d", words)
	}

	// Verify inline and display math delimiters survived Goldmark intact
	if !strings.Contains(htmlStr, `\(\sum_{i=1}^n w_i r_i \ge 0\)`) {
		t.Fatalf("expected inline paren math to be preserved, got:\n%s", htmlStr)
	}
	if !strings.Contains(htmlStr, `\(r_p = w_1 r_1 + w_2 r_2\)`) {
		t.Fatalf("expected single-dollar math to be converted to \\(...\\), got:\n%s", htmlStr)
	}
	if !strings.Contains(htmlStr, `\text{S\&amp;P 500 \&amp; R\&amp;D}`) {
		t.Fatalf("expected unescaped & inside \\text{...} to be escaped as \\&, got:\n%s", htmlStr)
	}
	if !strings.Contains(htmlStr, `\[E[R] = r_f + \beta (R_m - r_f)\]`) {
		t.Fatalf("expected \\[...\\] display math to be preserved, got:\n%s", htmlStr)
	}

	// Verify currency $1B to $5B and shell $HOME and $PATH were NOT turned into math
	if !strings.Contains(htmlStr, "from $1B to $5B and $HOME and $PATH remain") {
		t.Fatalf("currency or $HOME/$PATH was corrupted into math:\n%s", htmlStr)
	}

	// Verify inline code `$x_1 + x_2$` was NOT turned into \(x_1 + x_2\)
	if !strings.Contains(htmlStr, "<code>$x_1 + x_2$</code>") {
		t.Fatalf("expected inline code span to remain untouched, got:\n%s", htmlStr)
	}

	// Verify ~~~ tilde code fences are also masked from heading extraction and math conversion
	tildeMD := "# Main Title\n\n~~~python\n# Ignored Heading Inside Tilde Fence\nx = \"$a + b$\"\n~~~\n"
	_, tildeTitle, tildeHeadings, _, err := renderer.RenderMarkdownToHTML(tildeMD)
	if err != nil || tildeTitle != "Main Title" || len(tildeHeadings) != 1 {
		t.Fatalf("expected tilde fence heading to be masked, got title=%q headings=%+v err=%v", tildeTitle, tildeHeadings, err)
	}

	// Verify Mermaid block transformation escapes HTML brackets while preserving quotes for Mermaid entityDecode
	if !strings.Contains(htmlStr, `<div class="mermaid-container"><pre class="mermaid">`) || !strings.Contains(htmlStr, `A["Author Agent"] --&gt; B["Git Repo"]`) {
		t.Fatalf("expected HTML-escaped mermaid container, got:\n%s", htmlStr)
	}

	// Verify XSS neutralization (blocks <script>, <img onerror=...>, javascript: links, and </pre> breakout while preserving safe <br> inside tables)
	xssMD := "# Security Check\n\n| Col A | Col B |\n| :--- | :--- |\n| Line 1<br>Line 2<br/>Line 3 | Safe |\n\n<script>alert('xss')</script>\n\nInline <img src=x onerror=alert(1)> and [bad](javascript:alert(1))\n\n```mermaid\nflowchart LR\n  A[\"</pre><script>alert(2)</script>\"] --> B\n```\n"
	xssHTML, _, _, _, err := renderer.RenderMarkdownToHTML(xssMD)
	if err != nil {
		t.Fatalf("RenderMarkdownToHTML xssMD: %v", err)
	}
	xssStr := string(xssHTML)
	if strings.Contains(xssStr, "<script>") || strings.Contains(xssStr, "onerror=") || strings.Contains(xssStr, "javascript:alert") {
		t.Fatalf("expected raw XSS vectors to be stripped/sanitized, got:\n%s", xssStr)
	}
	if !strings.Contains(xssStr, "Line 1<br>Line 2<br>Line 3") {
		t.Fatalf("expected safe <br> inside Markdown table cells to be preserved, got:\n%s", xssStr)
	}
}

func TestRenderMarkdownToHTML_FallbackTitleFromH2(t *testing.T) {
	md := "## Secondary Heading Only\n\nParagraph text."
	_, title, headings, _, err := renderer.RenderMarkdownToHTML(md)
	if err != nil {
		t.Fatalf("RenderMarkdownToHTML: %v", err)
	}
	if title != "Secondary Heading Only" || len(headings) != 1 {
		t.Fatalf("expected fallback title from H2, got title=%q headings=%+v", title, headings)
	}
}

func TestRenderPages_ChapterChangelogPrintAndGuide(t *testing.T) {
	ch1 := &renderer.ChapterView{
		Slug:             "01-system-architecture",
		Title:            "Chapter 1: MFO Stack Teardown",
		Markdown:         "# Chapter 1\n\nBody",
		RenderedHTML:     template.HTML("<h1>Chapter 1</h1><p>Body</p>"),
		WordCount:        120,
		OpenCommentCount: 1,
		LastCommitSHA:    "1234567",
		LastAuthor:       "Alice Chen",
		LastUpdatedISO:   "2026-11-08T00:00:00Z",
		NextSlug:         "02-agent-workflow",
		NextTitle:        "Chapter 2: Agent Workflow",
	}
	book := &renderer.RenderedBook{
		HeadCommitSHA:   "1234567",
		SummaryMarkdown: "- [Chapter 1](01-system-architecture.md)\n",
		Chapters:        []*renderer.ChapterView{ch1},
		BySlug:          map[string]*renderer.ChapterView{ch1.Slug: ch1},
		RecentCommits: []renderer.CommitSummaryView{
			{
				ShortSHA:      "1234567",
				FullSHA:       "1234567890abcdef1234567890abcdef12345678",
				AuthorName:    "Alice Chen",
				AuthorEmail:   "alice@example.com",
				TimestampISO:  "2026-11-08T00:00:00Z",
				CommitMessage: "Initial commit",
				FilesChanged:  []string{"src/01-system-architecture.md"},
			},
		},
		RecentActivity: []store.AuditEntry{
			{
				ID:          1,
				AuthorID:    "sam",
				Protocol:    "mcp",
				ToolName:    "read_chapter",
				ChapterSlug: "01-system-architecture",
				Status:      "ok",
			},
		},
	}

	chBytes, err := renderer.RenderChapterPage(renderer.ChapterPageData{
		Book:          book,
		ActiveChapter: ch1,
		BaseURL:       "https://docs.example.com",
	})
	if err != nil || !strings.Contains(string(chBytes), "Chapter 1: MFO Stack Teardown") || !strings.Contains(string(chBytes), "/api-docs") {
		t.Fatalf("RenderChapterPage failed: err=%v len=%d", err, len(chBytes))
	}

	clBytes, err := renderer.RenderChangelogPage(renderer.ChangelogPageData{
		Book:    book,
		BaseURL: "https://docs.example.com",
	})
	if err != nil || !strings.Contains(string(clBytes), "Git Commit Log") {
		t.Fatalf("RenderChangelogPage failed: err=%v", err)
	}

	prData := renderer.PrintPageData{
		Book:    book,
		BaseURL: "https://docs.example.com",
	}
	if prData.ActiveChapterSlug() != "" || prData.ActiveStudioPage() != "print" || prData.TotalWords() != 120 {
		t.Fatalf("unexpected PrintPageData methods: slug=%q page=%q words=%d",
			prData.ActiveChapterSlug(), prData.ActiveStudioPage(), prData.TotalWords())
	}
	if (renderer.PrintPageData{}).TotalWords() != 0 {
		t.Fatalf("expected 0 words for nil Book on PrintPageData")
	}
	prBytes, err := renderer.RenderPrintPage(prData)
	if err != nil || !strings.Contains(string(prBytes), "US-Letter Grayscale-Laser-Printer Edition") || !strings.Contains(string(prBytes), `id="printToc"`) {
		t.Fatalf("RenderPrintPage failed: err=%v", err)
	}

	gdBytes, err := renderer.RenderGuidePage(renderer.GuidePageData{
		Book:    book,
		BaseURL: "https://docs.example.com",
	})
	if err != nil || !strings.Contains(string(gdBytes), "Co-Author AI Setup Guide") || !strings.Contains(string(gdBytes), `data-base-url="https://docs.example.com"`) {
		t.Fatalf("RenderGuidePage failed: err=%v", err)
	}
	if !strings.Contains(string(gdBytes), "rename_chapter") || !strings.Contains(string(gdBytes), "delete_chapter") {
		t.Fatalf("expected RenderGuidePage to document all 15 tools including rename_chapter and delete_chapter")
	}
	if strings.Contains(string(gdBytes), "Copy OpenAPI URL") || strings.Contains(string(gdBytes), "ConnectRPC HTTP Endpoint") {
		t.Fatalf("expected RenderGuidePage to omit OpenAPI and ConnectRPC HTTP endpoint sections for human MCP users")
	}
	if !strings.Contains(string(gdBytes), `id="guideOpenApiFootnote"`) || !strings.Contains(string(gdBytes), `href="/api-docs"`) || !strings.Contains(string(gdBytes), `href="/openapi.json"`) {
		t.Fatalf("expected RenderGuidePage bottom footnote to link /api-docs and /openapi.json")
	}

	apiBytes, err := renderer.RenderAPIDocsPage()
	if err != nil || !strings.Contains(string(apiBytes), `id="openapi-viewer-mount"`) || !strings.Contains(string(apiBytes), "/openapi.json") {
		t.Fatalf("RenderAPIDocsPage failed: err=%v", err)
	}

	// Verify all 5 permalink lifecycle banner variants in RenderChapterPage
	for _, status := range []string{"current", "outdated", "renamed", "moved", "deleted"} {
		chCopy := *ch1
		chCopy.IsPermalinkView = true
		chCopy.LifecycleStatus = status
		chCopy.RevisionNumber = 2
		chCopy.LatestRevisionNumber = 5
		chCopy.LatestRevisionSHA = "7654321"
		chCopy.CommitsBehind = 3
		chCopy.MovedToSlug = "01-system-architecture-v2"
		chCopy.MovedToTitle = "Chapter 1 (V2)"
		chCopy.LifecycleCommitSHA = "abcdef0"
		chCopy.LifecycleAuthor = "Bob Rivera"
		chCopy.LifecycleTimestampISO = "2026-11-08T12:00:00Z"
		chCopy.LifecycleMessage = "Lifecycle transition"
		chCopy.DiffToLatest = "@@ -1 +1 @@\n-old\n+new"

		out, err := renderer.RenderChapterPage(renderer.ChapterPageData{
			Book:          book,
			ActiveChapter: &chCopy,
			BaseURL:       "https://docs.example.com",
		})
		if err != nil || !strings.Contains(string(out), `data-status="`+status+`"`) {
			t.Fatalf("expected permalink banner for status=%q, err=%v", status, err)
		}
	}
}

func TestEmbeddedAssets_NonEmptyAndValidSyntax(t *testing.T) {
	if !strings.Contains(renderer.StudioCSS(), ".permalink-banner") || !strings.Contains(renderer.StudioCSS(), ".book-prose") {
		t.Fatalf("expected StudioCSS to contain core styles")
	}
	if !strings.Contains(renderer.StudioJS(), "function focusSidebarOrPageSearch") || !strings.Contains(renderer.StudioJS(), "function setReaderTheme") {
		t.Fatalf("expected StudioJS to contain core progressive-enhancement functions")
	}
	if !strings.Contains(renderer.GuideJS(), "function selectGuideProfile") {
		t.Fatalf("expected GuideJS to contain profile switcher")
	}

	handler := renderer.StaticAssetsHandler()
	for _, assetPath := range []string{"/studio.css", "/studio.js", "/guide.js"} {
		req := httptest.NewRequest(http.MethodGet, assetPath, nil)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("expected 200 OK for embedded asset %s, got %d", assetPath, rec.Code)
		}
		if rec.Header().Get("Cache-Control") != "public, max-age=3600" {
			t.Fatalf("unexpected Cache-Control header for %s: %q", assetPath, rec.Header().Get("Cache-Control"))
		}
		if rec.Body.Len() < 100 {
			t.Fatalf("expected non-trivial asset body for %s, got %d bytes", assetPath, rec.Body.Len())
		}
	}
}

func TestParseSummaryMarkdown_PartsSubChaptersAndNumbering(t *testing.T) {
	summary := `# Summary

[Preface](00-preface.md)

# Part I — Market Teardown & Portfolio Architecture
- [Chapter 1: MFO Stack Teardown](01-system-architecture.md)
  - [1.1 Legacy Bottlenecks](01a-legacy-bottlenecks.md)
  - [1.2 Build vs Buy](01b-build-vs-buy.md)
- [Chapter 2: Agent Workflow](02-agent-workflow.md)

# Part II — Switching-Cost Engines
- [Chapter 3: Five Killer Features](03-five-killer-features.md)
`
	entries := renderer.ParseSummaryMarkdown(summary)
	if len(entries) != 6 {
		t.Fatalf("expected 6 summary entries, got %d: %+v", len(entries), entries)
	}
	if entries[0].Slug != "00-preface" || entries[0].PartTitle != "" || entries[0].NumberLabel != "" {
		t.Fatalf("unexpected preface entry: %+v", entries[0])
	}
	if entries[1].Slug != "01-system-architecture" || !entries[1].StartsNewPart || entries[1].PartTitle != "Part I — Market Teardown & Portfolio Architecture" || entries[1].NumberLabel != "1" {
		t.Fatalf("unexpected chapter 1 entry: %+v", entries[1])
	}
	if entries[2].Slug != "01a-legacy-bottlenecks" || entries[2].StartsNewPart || entries[2].Depth != 1 || entries[2].ParentSlug != "01-system-architecture" || entries[2].NumberLabel != "1.1" {
		t.Fatalf("unexpected sub-chapter 1.1 entry: %+v", entries[2])
	}
	if entries[3].Slug != "01b-build-vs-buy" || entries[3].Depth != 1 || entries[3].ParentSlug != "01-system-architecture" || entries[3].NumberLabel != "1.2" {
		t.Fatalf("unexpected sub-chapter 1.2 entry: %+v", entries[3])
	}
	if entries[4].Slug != "02-agent-workflow" || entries[4].StartsNewPart || entries[4].Depth != 0 || entries[4].NumberLabel != "2" {
		t.Fatalf("unexpected chapter 2 entry: %+v", entries[4])
	}
	if entries[5].Slug != "03-five-killer-features" || !entries[5].StartsNewPart || entries[5].PartTitle != "Part II — Switching-Cost Engines" || entries[5].NumberLabel != "3" {
		t.Fatalf("unexpected chapter 3 entry: %+v", entries[5])
	}
}

func TestSidebarAndTopbarParity_AcrossAllStudioPages(t *testing.T) {
	ch1 := &renderer.ChapterView{
		Slug:             "01-system-architecture",
		Title:            "Chapter 1: MFO Stack Teardown",
		PartTitle:        "Part I — Market Teardown & Portfolio Architecture",
		StartsNewPart:    true,
		NumberLabel:      "1",
		RenderedHTML:     template.HTML("<h1>Chapter 1</h1>"),
		OpenCommentCount: 2,
		LastCommitSHA:    "1234567",
		RevisionNumber:   3,
	}
	ch1a := &renderer.ChapterView{
		Slug:             "01a-legacy-bottlenecks",
		Title:            "1.1 Legacy Bottlenecks",
		PartTitle:        "Part I — Market Teardown & Portfolio Architecture",
		StartsNewPart:    false,
		Depth:            1,
		ParentSlug:       "01-system-architecture",
		NumberLabel:      "1.1",
		RenderedHTML:     template.HTML("<h1>Section 1.1</h1>"),
		OpenCommentCount: 0,
		LastCommitSHA:    "1234567",
		RevisionNumber:   3,
	}
	book := &renderer.RenderedBook{
		HeadCommitSHA:      "1234567",
		HeadRevisionNumber: 3,
		Chapters:           []*renderer.ChapterView{ch1, ch1a},
		BySlug: map[string]*renderer.ChapterView{
			ch1.Slug:  ch1,
			ch1a.Slug: ch1a,
		},
	}

	homeHTMLBytes, err := renderer.RenderHomePage(renderer.HomePageData{
		Book:    book,
		BaseURL: "https://docs.example.com",
	})
	if err != nil {
		t.Fatalf("RenderHomePage: %v", err)
	}
	chapterHTMLBytes, err := renderer.RenderChapterPage(renderer.ChapterPageData{
		Book:          book,
		ActiveChapter: ch1,
		BaseURL:       "https://docs.example.com",
	})
	if err != nil {
		t.Fatalf("RenderChapterPage: %v", err)
	}
	changelogHTMLBytes, err := renderer.RenderChangelogPage(renderer.ChangelogPageData{
		Book:    book,
		BaseURL: "https://docs.example.com",
	})
	if err != nil {
		t.Fatalf("RenderChangelogPage: %v", err)
	}
	guideHTMLBytes, err := renderer.RenderGuidePage(renderer.GuidePageData{
		Book:    book,
		BaseURL: "https://docs.example.com",
	})
	if err != nil {
		t.Fatalf("RenderGuidePage: %v", err)
	}

	sidebarRe := regexp.MustCompile(`(?s)<aside class="book-sidebar" id="bookSidebar">(.*?)</aside>`)
	tocItemRe := regexp.MustCompile(`(?s)<a href="([^"]+)" class="toc-item([^"]*)"[^>]*>\s*<span>([^<]+)</span>`)
	activeClassRe := regexp.MustCompile(`\bactive\b`)

	type navItem struct {
		Href  string
		Label string
	}

	extractSidebarItems := func(pageName, htmlStr string) ([]navItem, []string) {
		m := sidebarRe.FindStringSubmatch(htmlStr)
		if len(m) < 2 {
			t.Fatalf("%s: missing <aside class=\"book-sidebar\" id=\"bookSidebar\">", pageName)
		}
		sidebarHTML := m[1]

		for _, requiredFragment := range []string{
			`id="sidebarHeadSha"`,
			`r3 · 1234567`,
			`id="sidebarSearchForm"`,
			`id="sidebarSearchInput"`,
			`>I — Market Teardown &amp; Portfolio Architecture</div>`,
		} {
			if !strings.Contains(sidebarHTML, requiredFragment) {
				t.Fatalf("%s sidebar is missing required fragment %q", pageName, requiredFragment)
			}
		}

		// Verify topbar, SSR sidebar search form, and machine-only API discovery meta tags across all pages
		for _, requiredShared := range []string{
			`id="sidebarToggleBtn"`,
			`id="themeSelect"`,
			`onchange="setReaderTheme(this.value)"`,
			`<option value="system">◐ System</option>`,
			`<option value="light">☀ Light</option>`,
			`<option value="sepia">📖 Sepia</option>`,
			`<option value="dark">☾ Dark</option>`,
			`id="sidebarBackdrop"`,
			`<link rel="service-desc" type="application/vnd.oai.openapi+json" href="/openapi.json">`,
			`<link rel="service-doc" href="/api-docs">`,
		} {
			if !strings.Contains(htmlStr, requiredShared) {
				t.Fatalf("%s is missing required shared control/modal %q", pageName, requiredShared)
			}
		}

		matches := tocItemRe.FindAllStringSubmatch(sidebarHTML, -1)
		var items []navItem
		var activeHrefs []string
		for _, match := range matches {
			href := match[1]
			classTail := match[2]
			label := strings.TrimSpace(match[3])
			items = append(items, navItem{Href: href, Label: label})
			if activeClassRe.MatchString(classTail) {
				activeHrefs = append(activeHrefs, href)
			}
		}
		return items, activeHrefs
	}

	homeItems, homeActive := extractSidebarItems("HomePage", string(homeHTMLBytes))
	chapterItems, chapterActive := extractSidebarItems("ChapterPage", string(chapterHTMLBytes))
	changelogItems, changelogActive := extractSidebarItems("ChangelogPage", string(changelogHTMLBytes))
	guideItems, guideActive := extractSidebarItems("GuidePage", string(guideHTMLBytes))

	expectedItems := []navItem{
		{Href: "/timeline", Label: "Timeline"},
		{Href: "/chapters/01-system-architecture", Label: "1. MFO Stack Teardown"},
		{Href: "/chapters/01a-legacy-bottlenecks", Label: "1.1 Legacy Bottlenecks"},
		{Href: "/guide", Label: "Co-Author AI Setup Guide"},
		{Href: "/STYLE_GUIDE", Label: "Style Guide &amp; Rules"},
		{Href: "/changelog", Label: "Git Commit Log"},
		{Href: "/print", Label: "US-Letter Full Book Print View"},
	}

	if renderer.FormatSidebarChapterTitle("Introduction: The Master Book as Single Source of Truth") != "The Master Book as Single Source of Truth" {
		t.Fatalf("expected Introduction: prefix to be stripped")
	}
	if renderer.FormatSidebarChapterTitle("Appendix A: Multi-Agent Editorial Prompt Templates") != "A. Multi-Agent Editorial Prompt Templates" {
		t.Fatalf("expected Appendix A: prefix to become A.")
	}

	if !reflect.DeepEqual(homeItems, expectedItems) {
		t.Fatalf("HomePage sidebar items mismatch:\ngot:  %+v\nwant: %+v", homeItems, expectedItems)
	}
	if !reflect.DeepEqual(chapterItems, expectedItems) {
		t.Fatalf("ChapterPage sidebar items mismatch:\ngot:  %+v\nwant: %+v", chapterItems, expectedItems)
	}
	if !reflect.DeepEqual(changelogItems, expectedItems) {
		t.Fatalf("ChangelogPage sidebar items drifted from ChapterPage:\ngot:  %+v\nwant: %+v", changelogItems, expectedItems)
	}
	if !reflect.DeepEqual(guideItems, expectedItems) {
		t.Fatalf("GuidePage sidebar items drifted from ChapterPage:\ngot:  %+v\nwant: %+v", guideItems, expectedItems)
	}

	if !reflect.DeepEqual(homeActive, []string{"/timeline"}) {
		t.Fatalf("expected active item on HomePage to be /timeline, got %+v", homeActive)
	}
	if !reflect.DeepEqual(chapterActive, []string{"/chapters/01-system-architecture"}) {
		t.Fatalf("expected active item on ChapterPage to be /chapters/01-system-architecture, got %+v", chapterActive)
	}
	if !reflect.DeepEqual(changelogActive, []string{"/changelog"}) {
		t.Fatalf("expected active item on ChangelogPage to be /changelog, got %+v", changelogActive)
	}
	if !reflect.DeepEqual(guideActive, []string{"/guide"}) {
		t.Fatalf("expected active item on GuidePage to be /guide, got %+v", guideActive)
	}
}

// ============================================================================
// Suite A: Self-Hosted Vendor Bundle, MIME, & Cache-Control Tests (UT-VENDOR-01..04)
// ============================================================================

func TestSuiteA_VendorAssetsAndZeroCDN_UT_VENDOR_01_to_04(t *testing.T) {
	ch1 := &renderer.ChapterView{
		Slug:         "01-system-architecture",
		Title:        "Chapter 1: MFO Stack Teardown",
		RenderedHTML: template.HTML("<h1>Chapter 1</h1>"),
	}
	book := &renderer.RenderedBook{
		HeadCommitSHA: "1234567",
		Chapters:      []*renderer.ChapterView{ch1},
		BySlug:        map[string]*renderer.ChapterView{ch1.Slug: ch1},
	}

	t.Run("UT-VENDOR-01_ZeroExternalCDNReferences", func(t *testing.T) {
		hmOut, _ := renderer.RenderHomePage(renderer.HomePageData{Book: book, BaseURL: "https://docs.example.com"})
		chOut, _ := renderer.RenderChapterPage(renderer.ChapterPageData{Book: book, ActiveChapter: ch1, BaseURL: "https://docs.example.com"})
		prOut, _ := renderer.RenderPrintPage(renderer.PrintPageData{Book: book, BaseURL: "https://docs.example.com"})
		gdOut, _ := renderer.RenderGuidePage(renderer.GuidePageData{Book: book, BaseURL: "https://docs.example.com"})
		clOut, _ := renderer.RenderChangelogPage(renderer.ChangelogPageData{Book: book, BaseURL: "https://docs.example.com"})
		apiOut, _ := renderer.RenderAPIDocsPage()

		pages := map[string]string{
			"home.html":      string(hmOut),
			"chapter.html":   string(chOut),
			"print.html":     string(prOut),
			"guide.html":     string(gdOut),
			"changelog.html": string(clOut),
			"apidocs.html":   string(apiOut),
		}
		externalAssetRe := regexp.MustCompile(`(?i)<(?:script|link)\b[^>]*(?:src|href)=["']https?://`)
		for name, htmlStr := range pages {
			for _, forbidden := range []string{"cdn.jsdelivr.net", "unpkg.com", "cdnjs.cloudflare.com", "fonts.googleapis.com", "fonts.scalar.com"} {
				if strings.Contains(htmlStr, forbidden) {
					t.Fatalf("%s still contains forbidden external CDN reference %q", name, forbidden)
				}
			}
			if m := externalAssetRe.FindString(htmlStr); m != "" {
				t.Fatalf("%s contains external script/link tag: %s", name, m)
			}
		}
	})

	t.Run("UT-VENDOR-02_EmbeddedVendorAssetsIntegrityAndNonEmpty", func(t *testing.T) {
		handler := renderer.StaticAssetsHandler()
		vendorAssets := []struct {
			path     string
			minBytes int
		}{
			{"/vendor/katex/katex.min.css", 15000},
			{"/vendor/katex/katex.min.js", 200000},
			{"/vendor/katex/auto-render.min.js", 2000},
			{"/vendor/mermaid/mermaid.min.js", 1000000},
			{"/vendor/openapi/scalar-api-reference.min.js", 4000000},
			{"/vendor/katex/fonts/KaTeX_AMS-Regular.woff2", 10000},
			{"/vendor/katex/fonts/KaTeX_Caligraphic-Bold.woff2", 4000},
			{"/vendor/katex/fonts/KaTeX_Caligraphic-Regular.woff2", 4000},
			{"/vendor/katex/fonts/KaTeX_Fraktur-Bold.woff2", 8000},
			{"/vendor/katex/fonts/KaTeX_Fraktur-Regular.woff2", 8000},
			{"/vendor/katex/fonts/KaTeX_Main-Bold.woff2", 20000},
			{"/vendor/katex/fonts/KaTeX_Main-BoldItalic.woff2", 12000},
			{"/vendor/katex/fonts/KaTeX_Main-Italic.woff2", 12000},
			{"/vendor/katex/fonts/KaTeX_Main-Regular.woff2", 20000},
			{"/vendor/katex/fonts/KaTeX_Math-BoldItalic.woff2", 12000},
			{"/vendor/katex/fonts/KaTeX_Math-Italic.woff2", 12000},
			{"/vendor/katex/fonts/KaTeX_SansSerif-Bold.woff2", 10000},
			{"/vendor/katex/fonts/KaTeX_SansSerif-Italic.woff2", 10000},
			{"/vendor/katex/fonts/KaTeX_SansSerif-Regular.woff2", 10000},
			{"/vendor/katex/fonts/KaTeX_Script-Regular.woff2", 7000},
			{"/vendor/katex/fonts/KaTeX_Size1-Regular.woff2", 4000},
			{"/vendor/katex/fonts/KaTeX_Size2-Regular.woff2", 4000},
			{"/vendor/katex/fonts/KaTeX_Size3-Regular.woff2", 2000},
			{"/vendor/katex/fonts/KaTeX_Size4-Regular.woff2", 3000},
			{"/vendor/katex/fonts/KaTeX_Typewriter-Regular.woff2", 10000},
		}
		for _, a := range vendorAssets {
			req := httptest.NewRequest(http.MethodGet, a.path, nil)
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)
			if rec.Code != http.StatusOK {
				t.Fatalf("expected 200 OK for %s, got %d", a.path, rec.Code)
			}
			if rec.Body.Len() < a.minBytes {
				t.Fatalf("expected at least %d bytes for %s, got %d", a.minBytes, a.path, rec.Body.Len())
			}
		}
	})

	t.Run("UT-VENDOR-03_KaTeXCSSLocalFontURLsOnly", func(t *testing.T) {
		efs := renderer.EmbeddedAssetsFS()
		cssBytes, err := fs.ReadFile(efs, "vendor/katex/katex.min.css")
		if err != nil {
			t.Fatalf("read vendor/katex/katex.min.css: %v", err)
		}
		urlRe := regexp.MustCompile(`url\(([^)]+)\)`)
		matches := urlRe.FindAllStringSubmatch(string(cssBytes), -1)
		if len(matches) != 20 {
			t.Fatalf("expected exactly 20 woff2 font url() declarations in katex.min.css, got %d", len(matches))
		}
		for _, m := range matches {
			u := strings.Trim(m[1], `"' `)
			if !strings.HasPrefix(u, "fonts/KaTeX_") || !strings.HasSuffix(u, ".woff2") {
				t.Fatalf("unexpected non-woff2 or external font URL in katex.min.css: %q", u)
			}
			fontData, err := fs.ReadFile(efs, "vendor/katex/"+u)
			if err != nil || len(fontData) == 0 {
				t.Fatalf("referenced font %q missing or empty in embedded FS: %v", u, err)
			}
		}
	})

	t.Run("UT-VENDOR-04_StaticAssetsMIMETypesAndCacheHeaders", func(t *testing.T) {
		handler := renderer.StaticAssetsHandler()

		check := func(path, wantCache, wantMimePrefix string) {
			req := httptest.NewRequest(http.MethodGet, path, nil)
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)
			if rec.Code != http.StatusOK {
				t.Fatalf("%s: expected 200, got %d", path, rec.Code)
			}
			if got := rec.Header().Get("Cache-Control"); got != wantCache {
				t.Fatalf("%s: Cache-Control = %q, want %q", path, got, wantCache)
			}
			if got := rec.Header().Get("Content-Type"); !strings.HasPrefix(got, wantMimePrefix) {
				t.Fatalf("%s: Content-Type = %q, want prefix %q", path, got, wantMimePrefix)
			}
		}

		check("/studio.css", "public, max-age=3600", "text/css")
		check("/studio.js", "public, max-age=3600", "application/javascript")
		check("/vendor/katex/katex.min.css", "public, max-age=31536000, immutable", "text/css")
		check("/vendor/katex/katex.min.js", "public, max-age=31536000, immutable", "application/javascript")
		check("/vendor/mermaid/mermaid.min.js", "public, max-age=31536000, immutable", "application/javascript")
		check("/vendor/katex/fonts/KaTeX_Main-Regular.woff2", "public, max-age=31536000, immutable", "font/woff2")
	})
}

// ============================================================================
// Suite B & Suite C: Goldmark AST Math, Currency, Code, Headings & Mermaid
// ============================================================================

func TestSuiteBAndC_GoldmarkAST_UT_AST_01_to_13_and_MERMAID_01_to_04(t *testing.T) {
	t.Run("UT-AST-01_InlineBackslashParenMath", func(t *testing.T) {
		htmlOut, _, _, _, err := renderer.RenderMarkdownToHTML(`Inline math \(\sum_{i=1}^n w_i r_i \ge 0\) and \(x_i + y_j\).`)
		if err != nil || !strings.Contains(string(htmlOut), `\(\sum_{i=1}^n w_i r_i \ge 0\)`) || !strings.Contains(string(htmlOut), `\(x_i + y_j\)`) || strings.Contains(string(htmlOut), "<em>") {
			t.Fatalf("UT-AST-01 failed: %s (err=%v)", htmlOut, err)
		}
	})

	t.Run("UT-AST-02_SingleDollarInlineMath", func(t *testing.T) {
		htmlOut, _, _, _, err := renderer.RenderMarkdownToHTML(`Portfolio return $r_p = w_1 r_1 + w_2 r_2$ and $\alpha_i \times \beta_j$.`)
		if err != nil || !strings.Contains(string(htmlOut), `\(r_p = w_1 r_1 + w_2 r_2\)`) || !strings.Contains(string(htmlOut), `\(\alpha_i \times \beta_j\)`) || strings.Contains(string(htmlOut), "<em>") {
			t.Fatalf("UT-AST-02 failed: %s (err=%v)", htmlOut, err)
		}
	})

	t.Run("UT-AST-03_MultiLineDoubleDollarDisplayMath", func(t *testing.T) {
		md := "$$\n\\text{NAV}(t) = \\frac{A_t - L_t}{N_t}\n$$"
		htmlOut, _, _, _, err := renderer.RenderMarkdownToHTML(md)
		if err != nil || !strings.Contains(string(htmlOut), `\[\text{NAV}(t) = \frac{A_t - L_t}{N_t}\]`) {
			t.Fatalf("UT-AST-03 failed: %s (err=%v)", htmlOut, err)
		}
	})

	t.Run("UT-AST-04_MultiLineBackslashBracketDisplayMath", func(t *testing.T) {
		md := "\\[\nE[R] = r_f + \\beta (R_m - r_f)\n\\]"
		htmlOut, _, _, _, err := renderer.RenderMarkdownToHTML(md)
		if err != nil || !strings.Contains(string(htmlOut), `\[E[R] = r_f + \beta (R_m - r_f)\]`) {
			t.Fatalf("UT-AST-04 failed: %s (err=%v)", htmlOut, err)
		}
	})

	t.Run("UT-AST-05_SingleLineAndListIndentedDisplayMath", func(t *testing.T) {
		md := "Top-level single-line display block:\n\n$$P_{\\text{share}}(t) = \\frac{\\text{NAV}(t)}{S(t)}$$\n\nParagraph after single-line display block.\n\n1. Zoom equation:\n   $$T_{\\text{new}} = P_{\\text{cursor}} - S$$\n\n> \\[x_1 + x_2 = 1\\]\n\n| Col A | Col B |\n| :--- | :--- |\n| Cell 1 | Cell 2 |\n"
		htmlOut, _, _, _, err := renderer.RenderMarkdownToHTML(md)
		if err != nil || !strings.Contains(string(htmlOut), `\[P_{\text{share}}(t) = \frac{\text{NAV}(t)}{S(t)}\]`) || !strings.Contains(string(htmlOut), "<p>Paragraph after single-line display block.</p>") || !strings.Contains(string(htmlOut), "<table>") || !strings.Contains(string(htmlOut), `\[T_{\text{new}} = P_{\text{cursor}} - S\]`) || !strings.Contains(string(htmlOut), `\[x_1 + x_2 = 1\]`) {
			t.Fatalf("UT-AST-05 failed: %s (err=%v)", htmlOut, err)
		}
	})

	t.Run("UT-AST-06_UnescapedCurrencyRangesInProse", func(t *testing.T) {
		md := "AUM ranges from $1B to $5B, tickets of $250,000 to $10,000,000, and $50/bps."
		htmlOut, _, _, _, err := renderer.RenderMarkdownToHTML(md)
		if err != nil || !strings.Contains(string(htmlOut), "AUM ranges from $1B to $5B, tickets of $250,000 to $10,000,000, and $50/bps.") || strings.Contains(string(htmlOut), `\(`) {
			t.Fatalf("UT-AST-06 failed: %s (err=%v)", htmlOut, err)
		}
	})

	t.Run("UT-AST-07_BackslashEscapedCurrencyInProse", func(t *testing.T) {
		md := `Fees on \$100M to \$500M and \$1,000 minimum.`
		htmlOut, _, _, _, err := renderer.RenderMarkdownToHTML(md)
		if err != nil || !strings.Contains(string(htmlOut), "Fees on $100M to $500M and $1,000 minimum.") || strings.Contains(string(htmlOut), `\(`) {
			t.Fatalf("UT-AST-07 failed: %s (err=%v)", htmlOut, err)
		}
	})

	t.Run("UT-AST-08_ShellVariablesAndTickersWithDollar", func(t *testing.T) {
		md := "Env vars $HOME and $PATH or tickers $NVDA and $BTC stay literal."
		htmlOut, _, _, _, err := renderer.RenderMarkdownToHTML(md)
		if err != nil || !strings.Contains(string(htmlOut), "Env vars $HOME and $PATH or tickers $NVDA and $BTC stay literal.") || strings.Contains(string(htmlOut), `\(`) {
			t.Fatalf("UT-AST-08 failed: %s (err=%v)", htmlOut, err)
		}
	})

	t.Run("UT-AST-09_MathInsideGFMMarkdownTables", func(t *testing.T) {
		md := "| Metric | Target |\n| :--- | :--- |\n| Volatility $\\sigma_p \\le 8\\%$ | \\$25M to \\$100M |\n"
		htmlOut, _, _, _, err := renderer.RenderMarkdownToHTML(md)
		if err != nil || !strings.Contains(string(htmlOut), `\(\sigma_p \le 8\%\)`) || !strings.Contains(string(htmlOut), "$25M to $100M") {
			t.Fatalf("UT-AST-09 failed: %s (err=%v)", htmlOut, err)
		}
	})

	t.Run("UT-AST-10_LaTeXAmpersandAndHTMLEntitySafety", func(t *testing.T) {
		md := "$$\n\\text{S&P 500 & R&D} = \\frac{a < b}{c > d}\n$$"
		htmlOut, _, _, _, err := renderer.RenderMarkdownToHTML(md)
		if err != nil || !strings.Contains(string(htmlOut), `\text{S\&amp;P 500 \&amp; R\&amp;D} = \frac{a &lt; b}{c &gt; d}`) {
			t.Fatalf("UT-AST-10 failed: %s (err=%v)", htmlOut, err)
		}
	})

	t.Run("UT-AST-11_SingleAndDoubleBacktickInlineCodeSpans", func(t *testing.T) {
		md := "Inline `$x_1 + x_2$` and double `` $$a_1 + a_2$$ and `code` `` stay code."
		htmlOut, _, _, _, err := renderer.RenderMarkdownToHTML(md)
		if err != nil || !strings.Contains(string(htmlOut), "<code>$x_1 + x_2$</code>") || !strings.Contains(string(htmlOut), "<code>$$a_1 + a_2$$ and `code`</code>") {
			t.Fatalf("UT-AST-11 failed: %s (err=%v)", htmlOut, err)
		}
	})

	t.Run("UT-AST-12_BacktickAndTildeFencedCodeBlocksAndASTHeadings", func(t *testing.T) {
		md := "# Real Title\n\n```bash\n# Fake Bash Heading\necho \"$x_1 + x_2$\"\n```\n\n~~~python\n# Fake Python Heading\ny = \"$$a_1 + b_1$$\"\n~~~\n\n## Section 2\n"
		htmlOut, title, headings, _, err := renderer.RenderMarkdownToHTML(md)
		if err != nil || title != "Real Title" || !reflect.DeepEqual(headings, []string{"Real Title", "Section 2"}) {
			t.Fatalf("UT-AST-12 headings failed: title=%q headings=%+v err=%v", title, headings, err)
		}
		if strings.Contains(string(htmlOut), `\(`) || strings.Contains(string(htmlOut), `\[`) {
			t.Fatalf("UT-AST-12 code block math leaked: %s", htmlOut)
		}
	})

	t.Run("UT-AST-13_UnclosedSingleOrDoubleDollarResilience", func(t *testing.T) {
		md := "# Resilient Chapter\n\nUnclosed single $x_1 + y_1 in paragraph.\n\n$$\nUnclosed display block at EOF"
		htmlOut, title, _, _, err := renderer.RenderMarkdownToHTML(md)
		if err != nil || title != "Resilient Chapter" || !strings.Contains(string(htmlOut), "$x_1") || !strings.Contains(string(htmlOut), "Unclosed display block at EOF") {
			t.Fatalf("UT-AST-13 failed: %s (err=%v)", htmlOut, err)
		}
	})

	t.Run("UT-MERMAID-01_to_04_ASTMermaidAndRelativeLinks", func(t *testing.T) {
		md := "# Diagrams\n\n" +
			"```mermaid\nflowchart LR\n  A[\"Stage 1 ($100M_1)\"] ==>|\"Delta > 5%\"| B[\"Stage 2\"]\n```\n\n" +
			"Middle paragraph with [Chapter 5](05-multi-signal-ai-active-fund.md#section-3).\n\n" +
			"```mermaid\nsequenceDiagram\n  Alice->>Bob: Hello $x_i + y_j$\n```\n"
		htmlOut, _, _, _, err := renderer.RenderMarkdownToHTML(md)
		if err != nil {
			t.Fatalf("UT-MERMAID error: %v", err)
		}
		s := string(htmlOut)
		if strings.Count(s, `<div class="mermaid-container"><pre class="mermaid">`) != 2 {
			t.Fatalf("expected 2 mermaid-container blocks, got:\n%s", s)
		}
		if !strings.Contains(s, `A["Stage 1 ($100M_1)"] ==&gt;|"Delta &gt; 5%"| B["Stage 2"]`) {
			t.Fatalf("expected HTML-escaped mermaid flowchart definition, got:\n%s", s)
		}
		if !strings.Contains(s, `Alice-&gt;&gt;Bob: Hello $x_i + y_j$`) {
			t.Fatalf("expected math-like syntax inside mermaid block to remain untouched, got:\n%s", s)
		}
		if !strings.Contains(s, `href="/chapters/05-multi-signal-ai-active-fund#section-3"`) {
			t.Fatalf("expected relative .md link to be rewritten, got:\n%s", s)
		}
		extracted := renderer.ExtractMermaidDiagrams(md)
		if len(extracted) != 2 || !strings.HasPrefix(extracted[0], "flowchart LR") || !strings.HasPrefix(extracted[1], "sequenceDiagram") {
			t.Fatalf("expected ExtractMermaidDiagrams to return 2 diagrams, got %+v", extracted)
		}
	})
}

func TestSuiteD_HTMLAgentDiscoveryAndJSONLD_UT_MD_DISC_01_to_03(t *testing.T) {
	chLive := &renderer.ChapterView{
		Slug:            "01-system-architecture",
		Title:           `Chapter 1: "MFO" & </script> Edge Case`,
		Markdown:        "# Chapter 1\n",
		RenderedHTML:    template.HTML("<h1>Chapter 1</h1>"),
		LastCommitSHA:   "6ca162a",
		RevisionNumber:  3,
		LastUpdatedISO:  "2026-11-10T00:00:00Z",
		LifecycleStatus: "current",
		IsLatest:        true,
		LatestLiveURL:   "https://docs.example.com/chapters/01-system-architecture",
	}
	chPerm := *chLive
	chPerm.IsPermalinkView = true
	chPerm.IsLatest = false
	chPerm.LifecycleStatus = "outdated"

	book := &renderer.RenderedBook{
		HeadCommitSHA:      "9f8e7d6",
		HeadRevisionNumber: 5,
		Chapters:           []*renderer.ChapterView{chLive},
		BySlug:             map[string]*renderer.ChapterView{chLive.Slug: chLive},
	}

	jsonLDRe := regexp.MustCompile(`(?s)<script type="application/ld\+json" id="agentDiscoveryJsonLd">(.*?)</script>`)
	externalAssetRe := regexp.MustCompile(`(?i)<(?:script|link)\b[^>]*(?:src|href)=["']https?://`)

	t.Run("UT-MD-DISC-01_and_02_All6HTMLTemplatesDiscoveryTagsAndJSONLD", func(t *testing.T) {
		hmBytes, err := renderer.RenderHomePage(renderer.HomePageData{
			Book:    book,
			BaseURL: "https://docs.example.com",
		})
		if err != nil {
			t.Fatalf("RenderHomePage: %v", err)
		}
		chLiveBytes, err := renderer.RenderChapterPage(renderer.ChapterPageData{
			Book:          book,
			ActiveChapter: chLive,
			BaseURL:       "https://docs.example.com",
		})
		if err != nil {
			t.Fatalf("RenderChapterPage live: %v", err)
		}
		chPermBytes, err := renderer.RenderChapterPage(renderer.ChapterPageData{
			Book:          book,
			ActiveChapter: &chPerm,
			BaseURL:       "https://docs.example.com",
		})
		if err != nil {
			t.Fatalf("RenderChapterPage perm: %v", err)
		}
		prBytes, err := renderer.RenderPrintPage(renderer.PrintPageData{
			Book:    book,
			BaseURL: "https://docs.example.com",
		})
		if err != nil {
			t.Fatalf("RenderPrintPage: %v", err)
		}
		gdBytes, err := renderer.RenderGuidePage(renderer.GuidePageData{
			Book:    book,
			BaseURL: "https://docs.example.com",
		})
		if err != nil {
			t.Fatalf("RenderGuidePage: %v", err)
		}
		clBytes, err := renderer.RenderChangelogPage(renderer.ChangelogPageData{
			Book:    book,
			BaseURL: "https://docs.example.com",
		})
		if err != nil {
			t.Fatalf("RenderChangelogPage: %v", err)
		}
		apiBytes, err := renderer.RenderAPIDocsPage("https://docs.example.com")
		if err != nil {
			t.Fatalf("RenderAPIDocsPage: %v", err)
		}
		diagBytes, err := renderer.RenderDiagramPage(renderer.DiagramPageData{
			Book:          book,
			ActiveChapter: chLive,
			BaseURL:       "https://docs.example.com",
			DiagramIndex:  1,
			DiagramSource: "flowchart LR\n  A --> B",
		})
		if err != nil {
			t.Fatalf("RenderDiagramPage: %v", err)
		}

		cases := []struct {
			name        string
			html        string
			wantMDPath  string
			wantJSONTyp string
		}{
			{"home.html", string(hmBytes), "/llms.txt", "Book"},
			{"chapter.html (live)", string(chLiveBytes), "/chapters/01-system-architecture.md", "TechArticle"},
			{"chapter.html (permalink)", string(chPermBytes), "/rev/6ca162a/chapters/01-system-architecture.md", "TechArticle"},
			{"print.html", string(prBytes), "/book.md", "Book"},
			{"guide.html", string(gdBytes), "/guide.md", "TechArticle"},
			{"changelog.html", string(clBytes), "/changelog.md", "TechArticle"},
			{"apidocs.html", string(apiBytes), "/guide.md", "WebAPI"},
			{"diagram.html", string(diagBytes), "/chapters/01-system-architecture/rev/6ca162a/diagrams/1.mmd", "ImageObject"},
		}

		for _, tc := range cases {
			for _, shared := range []string{
				`<link rel="llms-txt" type="text/markdown" href="/llms.txt"`,
				`<link rel="llms-full-txt" type="text/markdown" href="/llms-full.txt"`,
				`<meta name="llms-txt" content="/llms.txt">`,
				`<meta name="llms-full-txt" content="/llms-full.txt">`,
				`<link rel="alternate" type="text/markdown" href="` + tc.wantMDPath + `"`,
				`<meta name="markdown-url" content="` + tc.wantMDPath + `">`,
			} {
				if !strings.Contains(tc.html, shared) {
					t.Fatalf("%s missing discovery tag %q", tc.name, shared)
				}
			}

			// Zero external <link href="https://..."> or <script src="https://...">
			if m := externalAssetRe.FindString(tc.html); m != "" {
				t.Fatalf("%s unexpectedly emitted external link/script tag: %s", tc.name, m)
			}

			m := jsonLDRe.FindStringSubmatch(tc.html)
			if len(m) != 2 {
				t.Fatalf("%s missing <script type=\"application/ld+json\" id=\"agentDiscoveryJsonLd\">", tc.name)
			}
			var parsed map[string]any
			if err := json.Unmarshal([]byte(m[1]), &parsed); err != nil {
				t.Fatalf("%s invalid JSON-LD (%v): %s", tc.name, err, m[1])
			}
			if parsed["@context"] != "https://schema.org" || parsed["@type"] != tc.wantJSONTyp {
				t.Fatalf("%s unexpected JSON-LD @type=%v (want %s)", tc.name, parsed["@type"], tc.wantJSONTyp)
			}
		}

		if !strings.Contains(string(chLiveBytes), `data-markdown-url="/chapters/01-system-architecture.md"`) {
			t.Fatalf("chapter.html live body missing data-markdown-url")
		}
		if !strings.Contains(string(chPermBytes), `data-markdown-url="/rev/6ca162a/chapters/01-system-architecture.md"`) {
			t.Fatalf("chapter.html permalink body missing data-markdown-url")
		}
		if !strings.Contains(string(apiBytes), `<link rel="alternate" type="application/vnd.oai.openapi+json" href="/openapi.json"`) {
			t.Fatalf("apidocs.html missing OpenAPI alternate link")
		}
		if !strings.Contains(string(hmBytes), `<link rel="alternate" type="application/rss+xml" href="/feed.xml"`) {
			t.Fatalf("home.html missing RSS alternate link")
		}
	})
}

func TestRenderTimelineRSS(t *testing.T) {
	book := &renderer.RenderedBook{
		HeadCommitSHA:      "14f4362",
		HeadRevisionNumber: 287,
		TimelineEntries: []renderer.TimelineMilestoneView{
			{
				ID:            10,
				StartRevision: 285,
				EndRevision:   287,
				EndCommitSHA:  "14f4362",
				TimestampISO:  "2026-11-18T18:42:51Z",
				CommitCount:   3,
				Title:         "Storage & Sync Architecture <Review>",
				Summary:       "Updated SQLite FTS5 indexing and Git sync retry thresholds.",
				Authors:       "Bob Rivera, Alice Chen",
				Chapters: []renderer.BriefingChapterLink{
					{Slug: "01-system-architecture", Label: "Ch. 1"},
					{Slug: "02-agent-workflow", Label: "Ch. 2"},
				},
			},
			{
				ID:            9,
				StartRevision: 284,
				EndRevision:   284,
				EndCommitSHA:  "a1b2c3d",
				TimestampISO:  "2026-11-17T09:15:00Z",
				CommitCount:   1,
				Title:         "Single Revision Milestone",
				Summary:       "Clarified onboarding flow.",
			},
		},
	}

	raw, err := renderer.RenderTimelineRSS(book, "https://docs.example.com")
	if err != nil {
		t.Fatalf("RenderTimelineRSS: %v", err)
	}
	xmlStr := string(raw)
	if !strings.HasPrefix(xmlStr, `<?xml version="1.0" encoding="UTF-8"?>`) {
		t.Fatalf("missing XML header: %s", xmlStr[:60])
	}
	if !strings.Contains(xmlStr, `href="https://docs.example.com/feed.xml" rel="self" type="application/rss+xml"`) {
		t.Fatalf("missing atom:link self element:\n%s", xmlStr)
	}

	var parsed struct {
		Version string `xml:"version,attr"`
		Channel struct {
			Title         string `xml:"title"`
			Link          string `xml:"link"`
			LastBuildDate string `xml:"lastBuildDate"`
			Items         []struct {
				Title       string   `xml:"title"`
				Link        string   `xml:"link"`
				GUID        string   `xml:"guid"`
				PubDate     string   `xml:"pubDate"`
				Categories  []string `xml:"category"`
				Description string   `xml:"description"`
			} `xml:"item"`
		} `xml:"channel"`
	}
	if err := xml.Unmarshal(raw, &parsed); err != nil {
		t.Fatalf("xml.Unmarshal RSS failed: %v\n%s", err, xmlStr)
	}
	if parsed.Version != "2.0" || len(parsed.Channel.Items) != 2 {
		t.Fatalf("unexpected RSS version=%q items=%d", parsed.Version, len(parsed.Channel.Items))
	}
	item0 := parsed.Channel.Items[0]
	if item0.Title != "r285–r287 · Storage & Sync Architecture <Review>" {
		t.Fatalf("unexpected item0 title: %q", item0.Title)
	}
	if item0.Link != "https://docs.example.com/timeline#milestone-r287" {
		t.Fatalf("unexpected item0 link: %q", item0.Link)
	}
	if item0.GUID != "urn:ghostbook:timeline:r285-r287:14f4362" {
		t.Fatalf("unexpected item0 guid: %q", item0.GUID)
	}
	if item0.PubDate != "Wed, 18 Nov 2026 18:42:51 +0000" {
		t.Fatalf("unexpected item0 pubDate: %q", item0.PubDate)
	}
	if len(item0.Categories) != 2 || item0.Categories[0] != "Ch. 1" {
		t.Fatalf("unexpected item0 categories: %+v", item0.Categories)
	}
	if !strings.Contains(item0.Description, "https://docs.example.com/chapters/01-system-architecture") ||
		!strings.Contains(item0.Description, "Bob Rivera, Alice Chen") {
		t.Fatalf("unexpected item0 description: %q", item0.Description)
	}
	if parsed.Channel.Items[1].Title != "r284 · Single Revision Milestone" {
		t.Fatalf("unexpected single-revision item title: %q", parsed.Channel.Items[1].Title)
	}

	// Empty timeline must still emit valid RSS 2.0 XML
	emptyRaw, err := renderer.RenderTimelineRSS(&renderer.RenderedBook{}, "https://docs.example.com")
	if err != nil {
		t.Fatalf("RenderTimelineRSS empty: %v", err)
	}
	if err := xml.Unmarshal(emptyRaw, &parsed); err != nil {
		t.Fatalf("xml.Unmarshal empty RSS failed: %v", err)
	}
}

func TestSidebarPoweredByCredit(t *testing.T) {
	ch := &renderer.ChapterView{
		Slug:         "00-intro",
		Title:        "Introduction",
		RenderedHTML: "<p>Hello</p>",
	}
	book := &renderer.RenderedBook{
		BookTitle:     "Field Guide",
		HeadCommitSHA: "abc1234",
		Chapters:      []*renderer.ChapterView{ch},
		BySlug:        map[string]*renderer.ChapterView{"00-intro": ch},
	}

	homeHTML, err := renderer.RenderHomePage(renderer.HomePageData{Book: book, BaseURL: "https://docs.example.com"})
	if err != nil {
		t.Fatalf("RenderHomePage: %v", err)
	}
	wantLink := `<a href="https://github.com/wjkoh/ghostbook" target="_blank" rel="noopener noreferrer">Ghostbook</a>`
	if !strings.Contains(string(homeHTML), wantLink) {
		t.Fatalf("expected sidebar Powered by Ghostbook link in home page")
	}

	chHTML, err := renderer.RenderChapterPage(renderer.ChapterPageData{Book: book, ActiveChapter: ch, BaseURL: "https://docs.example.com"})
	if err != nil {
		t.Fatalf("RenderChapterPage: %v", err)
	}
	if !strings.Contains(string(chHTML), wantLink) {
		t.Fatalf("expected sidebar Powered by Ghostbook link in chapter page")
	}

	printHTML, err := renderer.RenderPrintPage(renderer.PrintPageData{Book: book, BaseURL: "https://docs.example.com"})
	if err != nil {
		t.Fatalf("RenderPrintPage: %v", err)
	}
	if strings.Contains(string(printHTML), "https://github.com/wjkoh/ghostbook") {
		t.Fatalf("did not expect Ghostbook credit link in /print output")
	}

	book.HidePoweredBy = true
	hiddenHTML, err := renderer.RenderHomePage(renderer.HomePageData{Book: book, BaseURL: "https://docs.example.com"})
	if err != nil {
		t.Fatalf("RenderHomePage(HidePoweredBy=true): %v", err)
	}
	if strings.Contains(string(hiddenHTML), `<footer class="sidebar-footer">`) || strings.Contains(string(hiddenHTML), "https://github.com/wjkoh/ghostbook") {
		t.Fatalf("expected Powered by Ghostbook footer to be suppressed when HidePoweredBy is true")
	}
}

func TestStyleGuideGovernanceAndConfigCard_UT_UI_01(t *testing.T) {
	styleCh := &renderer.ChapterView{
		Slug:         "STYLE_GUIDE",
		Title:        "Editorial Style Guide",
		RenderedHTML: "<h1>Editorial Style Guide</h1><p>Keep prose crisp.</p>",
	}
	book := &renderer.RenderedBook{
		BookTitle:     "Distributed Field Guide",
		BookKicker:    "Engineering Handbook",
		HeadCommitSHA: "abc1234",
		BookConfig: renderer.BookConfigView{
			MaxHeadingChars:  50,
			ForbidEmDash:     false,
			BannedWords:      []string{"synergy", "leverage"},
			TimelineGuidance: "Prefer WAL and Raft terms.",
			FromRepoFile:     true,
			ConfigWarning:    "Sample parse warning",
		},
		Chapters: []*renderer.ChapterView{},
		BySlug:   map[string]*renderer.ChapterView{},
	}

	styleHTML, err := renderer.RenderChapterPage(renderer.ChapterPageData{
		Book:          book,
		ActiveChapter: styleCh,
		BaseURL:       "https://docs.example.com",
	})
	if err != nil {
		t.Fatalf("RenderChapterPage(STYLE_GUIDE): %v", err)
	}
	s := string(styleHTML)
	for _, want := range []string{
		`href="/STYLE_GUIDE"`,
		`id="styleGuideGovernanceBanner"`,
		`id="activeBookConfigCard"`,
		`id="bookConfigWarningBanner"`,
		`Sample parse warning`,
		`50 chars`,
		`synergy`,
		`Prefer WAL and Raft terms.`,
		`href="/book.json"`,
	} {
		if !strings.Contains(s, want) {
			t.Fatalf("STYLE_GUIDE HTML missing %q", want)
		}
	}
	if strings.Contains(s, `id="linkChapterHistory"`) {
		t.Fatalf("did not expect History & Blame button on STYLE_GUIDE page")
	}
}
