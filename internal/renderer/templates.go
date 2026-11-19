package renderer

import (
	"bytes"
	"embed"
	"encoding/json"
	"fmt"
	"html/template"
	"io/fs"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/wjkoh/ghostbook/internal/store"
)

//go:embed templates/*.html
var templatesFS embed.FS

//go:embed assets
var assetsFS embed.FS

//go:embed assets/studio.css
var rawStudioCSS string

//go:embed assets/studio.js
var rawStudioJS string

//go:embed assets/guide.js
var rawGuideJS string

func cleanDiscoveryBaseURL(raw string) string {
	b := strings.TrimRight(strings.TrimSpace(raw), "/")
	if b == "" {
		return "http://127.0.0.1:8090"
	}
	if u, err := url.Parse(b); err == nil && u.Scheme != "" && u.Host != "" {
		return u.Scheme + "://" + u.Host
	}
	return b
}

func formatRevisionVersionLabel(revNum int32, sha string) string {
	s := strings.TrimSpace(sha)
	if revNum > 0 && s != "" {
		return fmt.Sprintf("r%d (%s)", revNum, s)
	}
	if revNum > 0 {
		return fmt.Sprintf("r%d", revNum)
	}
	return s
}

func buildBookPartOfJSONLD(base string, book *RenderedBook) map[string]any {
	m := map[string]any{
		"@type": "Book",
		"name":  book.DisplayTitle(),
		"url":   base + "/",
		"encoding": []map[string]any{
			{
				"@type":          "MediaObject",
				"name":           "llms.txt Discovery Index",
				"encodingFormat": "text/markdown",
				"contentUrl":     base + "/llms.txt",
			},
			{
				"@type":          "MediaObject",
				"name":           "Full Book Raw Markdown",
				"encodingFormat": "text/markdown",
				"contentUrl":     base + "/llms-full.txt",
			},
		},
	}
	if book != nil {
		if v := formatRevisionVersionLabel(book.HeadRevisionNumber, book.HeadCommitSHA); v != "" {
			m["version"] = v
		}
	}
	return m
}

func renderJSONLDScriptTag(doc map[string]any) template.HTML {
	raw, err := json.Marshal(doc)
	if err != nil {
		return ""
	}
	return template.HTML(`<script type="application/ld+json" id="agentDiscoveryJsonLd">` + string(raw) + `</script>`)
}

func rootRelativePathFromURL(raw string) string {
	s := strings.TrimSpace(raw)
	if s == "" {
		return ""
	}
	if strings.HasPrefix(s, "/") {
		return s
	}
	if u, err := url.Parse(s); err == nil && u.Path != "" {
		return u.Path
	}
	return ""
}

const FrontPageTimelineLimit = 20

type HomePageData struct {
	Book            *RenderedBook
	BaseURL         string
	ShowAllTimeline bool
	CanonicalRoute  string
}

func (d HomePageData) ActiveChapterSlug() string {
	return ""
}

func (d HomePageData) ActiveStudioPage() string {
	return "home"
}

func (d HomePageData) CanonicalURLPath() string {
	if d.CanonicalRoute == "/timeline" {
		return "/timeline"
	}
	return "/"
}

func (d HomePageData) ExpandAllPath() string {
	return "/timeline?all=1"
}

func (d HomePageData) MarkdownPath() string {
	if d.CanonicalRoute == "/timeline" {
		return "/timeline.md"
	}
	return "/llms.txt"
}

func (d HomePageData) FirstChapter() *ChapterView {
	if d.Book != nil && len(d.Book.Chapters) > 0 {
		return d.Book.Chapters[0]
	}
	return nil
}

func (d HomePageData) TotalWords() int32 {
	if d.Book == nil {
		return 0
	}
	var total int32
	for _, ch := range d.Book.Chapters {
		total += ch.WordCount
	}
	return total
}

func (d HomePageData) FrontPageTimelineEntries() []TimelineMilestoneView {
	if d.Book == nil || len(d.Book.TimelineEntries) == 0 {
		return nil
	}
	if d.ShowAllTimeline || len(d.Book.TimelineEntries) <= FrontPageTimelineLimit {
		return d.Book.TimelineEntries
	}
	return d.Book.TimelineEntries[:FrontPageTimelineLimit]
}

func (d HomePageData) FrontPageTimelineLimitValue() int {
	return FrontPageTimelineLimit
}

func (d HomePageData) HasMoreTimelineEntries() bool {
	return !d.ShowAllTimeline && d.Book != nil && len(d.Book.TimelineEntries) > FrontPageTimelineLimit
}

func (d HomePageData) OlderTimelineCount() int {
	if d.Book == nil || len(d.Book.TimelineEntries) <= FrontPageTimelineLimit {
		return 0
	}
	return len(d.Book.TimelineEntries) - FrontPageTimelineLimit
}

func (d HomePageData) CanCollapseTimeline() bool {
	return d.ShowAllTimeline && d.Book != nil && len(d.Book.TimelineEntries) > FrontPageTimelineLimit
}

func (d HomePageData) JSONLDDiscovery() template.HTML {
	base := cleanDiscoveryBaseURL(d.BaseURL)
	doc := buildBookPartOfJSONLD(base, d.Book)
	doc["@context"] = "https://schema.org"
	doc["headline"] = "Timeline · " + d.Book.DisplayTitle()
	return renderJSONLDScriptTag(doc)
}

type ChapterPageData struct {
	Book          *RenderedBook
	ActiveChapter *ChapterView
	BaseURL       string
}

func (d ChapterPageData) ActiveChapterSlug() string {
	if d.ActiveChapter != nil {
		return d.ActiveChapter.Slug
	}
	return ""
}

func (d ChapterPageData) ActiveStudioPage() string {
	return "chapter"
}

func (d ChapterPageData) CanonicalPath() string {
	if d.ActiveChapter == nil {
		return ""
	}
	if p := rootRelativePathFromURL(d.ActiveChapter.LatestLiveURL); p != "" {
		return p
	}
	if d.ActiveChapter.MovedToSlug != "" {
		return "/chapters/" + d.ActiveChapter.MovedToSlug
	}
	if d.ActiveChapter.LifecycleStatus == "deleted" || d.ActiveChapter.Slug == "" {
		return ""
	}
	return "/chapters/" + d.ActiveChapter.Slug
}

func (d ChapterPageData) MarkdownPath() string {
	if d.ActiveChapter == nil || d.ActiveChapter.Slug == "" {
		return "/llms.txt"
	}
	if d.ActiveChapter.IsPermalinkView && d.ActiveChapter.LastCommitSHA != "" {
		return "/rev/" + d.ActiveChapter.LastCommitSHA + "/chapters/" + d.ActiveChapter.Slug + ".md"
	}
	return "/chapters/" + d.ActiveChapter.Slug + ".md"
}

func (d ChapterPageData) JSONLDDiscovery() template.HTML {
	base := cleanDiscoveryBaseURL(d.BaseURL)
	ch := d.ActiveChapter
	if ch == nil {
		return renderJSONLDScriptTag(map[string]any{
			"@context": "https://schema.org",
			"@type":    "Book",
			"name":     d.Book.DisplayTitle(),
			"url":      base + "/",
		})
	}
	status := ch.LifecycleStatus
	if status == "" {
		status = "current"
	}
	pagePath := "/chapters/" + ch.Slug
	if ch.IsPermalinkView && ch.LastCommitSHA != "" {
		pagePath = "/rev/" + ch.LastCommitSHA + "/chapters/" + ch.Slug
	}
	encodings := []map[string]any{
		{
			"@type":          "MediaObject",
			"encodingFormat": "text/markdown",
			"contentUrl":     base + d.MarkdownPath(),
			"description":    "Raw Markdown for this chapter view",
		},
	}
	if ch.IsPermalinkView {
		liveSlug := ch.Slug
		if (status == "moved" || status == "deleted") && ch.MovedToSlug != "" {
			liveSlug = ch.MovedToSlug
		}
		if status != "deleted" || ch.MovedToSlug != "" {
			encodings = append(encodings, map[string]any{
				"@type":          "MediaObject",
				"encodingFormat": "text/markdown",
				"contentUrl":     base + "/chapters/" + liveSlug + ".md",
				"description":    "Latest live chapter raw Markdown",
			})
		}
	} else if ch.LastCommitSHA != "" {
		encodings = append(encodings, map[string]any{
			"@type":          "MediaObject",
			"encodingFormat": "text/markdown",
			"contentUrl":     base + "/rev/" + ch.LastCommitSHA + "/chapters/" + ch.Slug + ".md",
			"description":    "Immutable revision permalink raw Markdown",
		})
	}
	doc := map[string]any{
		"@context":           "https://schema.org",
		"@type":              "TechArticle",
		"headline":           ch.Title,
		"identifier":         ch.Slug,
		"creativeWorkStatus": status,
		"url":                base + pagePath,
		"encoding":           encodings,
		"isPartOf":           buildBookPartOfJSONLD(base, d.Book),
	}
	if v := formatRevisionVersionLabel(ch.RevisionNumber, ch.LastCommitSHA); v != "" {
		doc["version"] = v
	}
	if ch.LastUpdatedISO != "" {
		doc["dateModified"] = ch.LastUpdatedISO
	}
	return renderJSONLDScriptTag(doc)
}

type ChangelogPageData struct {
	Book    *RenderedBook
	BaseURL string
}

func (d ChangelogPageData) ActiveChapterSlug() string {
	return ""
}

func (d ChangelogPageData) ActiveStudioPage() string {
	return "changelog"
}

func (d ChangelogPageData) MarkdownPath() string {
	return "/changelog.md"
}

func (d ChangelogPageData) JSONLDDiscovery() template.HTML {
	base := cleanDiscoveryBaseURL(d.BaseURL)
	doc := map[string]any{
		"@context": "https://schema.org",
		"@type":    "TechArticle",
		"headline": "Git Commit Log · " + d.Book.DisplayTitle(),
		"url":      base + "/changelog",
		"encoding": []map[string]any{
			{
				"@type":          "MediaObject",
				"encodingFormat": "text/markdown",
				"contentUrl":     base + "/changelog.md",
				"description":    "Chronological Git revision history in raw Markdown",
			},
		},
		"isPartOf": buildBookPartOfJSONLD(base, d.Book),
	}
	return renderJSONLDScriptTag(doc)
}

type PrintPageData struct {
	Book           *RenderedBook
	BaseURL        string
	PinnedRevision string
}

func (d PrintPageData) ActiveChapterSlug() string {
	return ""
}

func (d PrintPageData) ActiveStudioPage() string {
	return "print"
}

func (d PrintPageData) MarkdownPath() string {
	if strings.TrimSpace(d.PinnedRevision) != "" {
		return "/rev/" + strings.TrimSpace(d.PinnedRevision) + "/book.md"
	}
	return "/book.md"
}

func (d PrintPageData) JSONLDDiscovery() template.HTML {
	base := cleanDiscoveryBaseURL(d.BaseURL)
	doc := map[string]any{
		"@context": "https://schema.org",
		"@type":    "Book",
		"name":     d.Book.DisplayTitle(),
		"url":      base + "/print",
		"encoding": []map[string]any{
			{
				"@type":          "MediaObject",
				"name":           "Full Book Raw Markdown (book.md)",
				"encodingFormat": "text/markdown",
				"contentUrl":     base + d.MarkdownPath(),
			},
			{
				"@type":          "MediaObject",
				"name":           "Full Book Raw Markdown (llms-full.txt)",
				"encodingFormat": "text/markdown",
				"contentUrl":     base + "/llms-full.txt",
			},
			{
				"@type":          "MediaObject",
				"name":           "llms.txt Discovery Index",
				"encodingFormat": "text/markdown",
				"contentUrl":     base + "/llms.txt",
			},
		},
	}
	if d.Book != nil {
		if v := formatRevisionVersionLabel(d.Book.HeadRevisionNumber, d.Book.HeadCommitSHA); v != "" {
			doc["version"] = v
		}
	}
	return renderJSONLDScriptTag(doc)
}

func (d PrintPageData) TotalWords() int32 {
	if d.Book == nil {
		return 0
	}
	var total int32
	for _, ch := range d.Book.Chapters {
		total += ch.WordCount
	}
	return total
}

type GuidePageData struct {
	Book    *RenderedBook
	BaseURL string
}

func (d GuidePageData) ActiveChapterSlug() string {
	return ""
}

func (d GuidePageData) ActiveStudioPage() string {
	return "guide"
}

func (d GuidePageData) MarkdownPath() string {
	return "/guide.md"
}

func (d GuidePageData) JSONLDDiscovery() template.HTML {
	base := cleanDiscoveryBaseURL(d.BaseURL)
	doc := map[string]any{
		"@context": "https://schema.org",
		"@type":    "TechArticle",
		"headline": "Co-Author AI Setup Guide · " + d.Book.DisplayTitle(),
		"url":      base + "/guide",
		"encoding": []map[string]any{
			{
				"@type":          "MediaObject",
				"encodingFormat": "text/markdown",
				"contentUrl":     base + "/guide.md",
				"description":    "AI agent HTTP raw Markdown, MCP, and ConnectRPC guide",
			},
		},
		"isPartOf": buildBookPartOfJSONLD(base, d.Book),
	}
	return renderJSONLDScriptTag(doc)
}

type APIDocsPageData struct {
	BaseURL     string
	BookTitle   string
	SpecVersion string
	Title       string
}

func (d APIDocsPageData) DisplayBookTitle() string {
	if strings.TrimSpace(d.BookTitle) != "" {
		return strings.TrimSpace(d.BookTitle)
	}
	return "Ghostbook"
}

type SearchResultLineView struct {
	LineNumber int32
	LineText   string
}

type SearchResultView struct {
	Slug        string
	Title       string
	Snippet     string
	BM25Score   float64
	LineMatches []SearchResultLineView
}

type SearchPageData struct {
	Book       *RenderedBook
	BaseURL    string
	Query      string
	SlugFilter string
	Results    []SearchResultView
}

func (d SearchPageData) ActiveChapterSlug() string {
	return d.SlugFilter
}

func (d SearchPageData) ActiveStudioPage() string {
	return "search"
}

func (d SearchPageData) MarkdownPath() string {
	var vals url.Values
	if strings.TrimSpace(d.Query) != "" || strings.TrimSpace(d.SlugFilter) != "" {
		vals = url.Values{}
		if strings.TrimSpace(d.Query) != "" {
			vals.Set("q", strings.TrimSpace(d.Query))
		}
		if strings.TrimSpace(d.SlugFilter) != "" {
			vals.Set("slug", strings.TrimSpace(d.SlugFilter))
		}
	}
	if len(vals) > 0 {
		return "/search.md?" + vals.Encode()
	}
	return "/search.md"
}

func (d SearchPageData) JSONLDDiscovery() template.HTML {
	base := cleanDiscoveryBaseURL(d.BaseURL)
	doc := map[string]any{
		"@context": "https://schema.org",
		"@type":    "TechArticle",
		"headline": "Full-Text Book Search · " + d.Book.DisplayTitle(),
		"url":      base + "/search",
		"encoding": []map[string]any{
			{
				"@type":          "MediaObject",
				"encodingFormat": "text/markdown",
				"contentUrl":     base + d.MarkdownPath(),
				"description":    "Search results in raw Markdown",
			},
		},
		"isPartOf": buildBookPartOfJSONLD(base, d.Book),
	}
	return renderJSONLDScriptTag(doc)
}

type HistoryBlameSectionView struct {
	Heading        string
	LastCommitSHA  string
	LastAuthor     string
	LastUpdatedISO string
	StartLine      int32
	EndLine        int32
}

type HistoryCommitDiffView struct {
	CommitSHA      string
	RevisionNumber int32
	AuthorName     string
	AuthorEmail    string
	TimestampISO   string
	CommitMessage  string
	UnifiedDiff    string
}

type ChapterHistoryPageData struct {
	Book          *RenderedBook
	ActiveChapter *ChapterView
	BaseURL       string
	BlameSections []HistoryBlameSectionView
	Commits       []HistoryCommitDiffView
}

func (d ChapterHistoryPageData) ActiveChapterSlug() string {
	if d.ActiveChapter != nil {
		return d.ActiveChapter.Slug
	}
	return ""
}

func (d ChapterHistoryPageData) ActiveStudioPage() string {
	return "history"
}

func (d ChapterHistoryPageData) MarkdownPath() string {
	if d.ActiveChapter != nil && d.ActiveChapter.Slug != "" {
		return "/chapters/" + d.ActiveChapter.Slug + "/history.md"
	}
	return "/changelog.md"
}

func (d ChapterHistoryPageData) JSONLDDiscovery() template.HTML {
	base := cleanDiscoveryBaseURL(d.BaseURL)
	slug := ""
	title := "Chapter History & Blame · " + d.Book.DisplayTitle()
	if d.ActiveChapter != nil {
		slug = d.ActiveChapter.Slug
		title = "History & Blame · " + d.ActiveChapter.Title
	}
	doc := map[string]any{
		"@context": "https://schema.org",
		"@type":    "TechArticle",
		"headline": title,
		"url":      base + "/chapters/" + slug + "/history",
		"encoding": []map[string]any{
			{
				"@type":          "MediaObject",
				"encodingFormat": "text/markdown",
				"contentUrl":     base + d.MarkdownPath(),
				"description":    "Section blame and unified commit diffs in raw Markdown",
			},
		},
		"isPartOf": buildBookPartOfJSONLD(base, d.Book),
	}
	return renderJSONLDScriptTag(doc)
}

type ActivityPageData struct {
	Book         *RenderedBook
	BaseURL      string
	AuthorFilter string
	Entries      []store.AuditEntry
}

func (d ActivityPageData) ActiveChapterSlug() string {
	return ""
}

func (d ActivityPageData) ActiveStudioPage() string {
	return "activity"
}

func (d ActivityPageData) MarkdownPath() string {
	if strings.TrimSpace(d.AuthorFilter) != "" {
		return "/activity.md?author=" + url.QueryEscape(strings.TrimSpace(d.AuthorFilter))
	}
	return "/activity.md"
}

func (d ActivityPageData) JSONLDDiscovery() template.HTML {
	base := cleanDiscoveryBaseURL(d.BaseURL)
	doc := map[string]any{
		"@context": "https://schema.org",
		"@type":    "TechArticle",
		"headline": "Agent Tool Audit Log · " + d.Book.DisplayTitle(),
		"url":      base + "/activity",
		"encoding": []map[string]any{
			{
				"@type":          "MediaObject",
				"encodingFormat": "text/markdown",
				"contentUrl":     base + d.MarkdownPath(),
				"description":    "MCP and ConnectRPC audit log in raw Markdown",
			},
		},
		"isPartOf": buildBookPartOfJSONLD(base, d.Book),
	}
	return renderJSONLDScriptTag(doc)
}

func (d APIDocsPageData) MarkdownPath() string {
	return "/guide.md"
}

func (d APIDocsPageData) JSONLDDiscovery() template.HTML {
	base := cleanDiscoveryBaseURL(d.BaseURL)
	doc := map[string]any{
		"@context":      "https://schema.org",
		"@type":         "WebAPI",
		"name":          d.DisplayBookTitle() + " API",
		"url":           base + "/api-docs",
		"documentation": base + "/guide.md",
		"encoding": []map[string]any{
			{
				"@type":          "MediaObject",
				"encodingFormat": "text/markdown",
				"contentUrl":     base + "/guide.md",
				"description":    "AI Agent & API Setup Guide in raw Markdown",
			},
			{
				"@type":          "MediaObject",
				"encodingFormat": "application/vnd.oai.openapi+json",
				"contentUrl":     base + "/openapi.json",
				"description":    "OpenAPI 3.1 JSON Specification",
			},
		},
		"isPartOf": buildBookPartOfJSONLD(base, &RenderedBook{BookTitle: d.DisplayBookTitle()}),
	}
	return renderJSONLDScriptTag(doc)
}

type DiagramPageData struct {
	Book          *RenderedBook
	ActiveChapter *ChapterView
	BaseURL       string
	DiagramIndex  int
	DiagramSource string
}

func (d DiagramPageData) ActiveChapterSlug() string {
	if d.ActiveChapter != nil {
		return d.ActiveChapter.Slug
	}
	return ""
}

func (d DiagramPageData) ActiveStudioPage() string {
	return "diagram"
}

func (d DiagramPageData) MarkdownPath() string {
	idx := d.DiagramIndex
	if idx < 1 {
		idx = 1
	}
	if d.ActiveChapter != nil && d.ActiveChapter.Slug != "" {
		if d.ActiveChapter.LastCommitSHA != "" {
			return fmt.Sprintf("/chapters/%s/rev/%s/diagrams/%d.mmd", d.ActiveChapter.Slug, d.ActiveChapter.LastCommitSHA, idx)
		}
		return fmt.Sprintf("/chapters/%s/diagrams/%d.mmd", d.ActiveChapter.Slug, idx)
	}
	return "/llms.txt"
}

func (d DiagramPageData) JSONLDDiscovery() template.HTML {
	base := cleanDiscoveryBaseURL(d.BaseURL)
	idx := d.DiagramIndex
	if idx < 1 {
		idx = 1
	}
	title := fmt.Sprintf("Diagram %d · %s", idx, d.Book.DisplayTitle())
	pagePath := fmt.Sprintf("/diagrams/%d", idx)
	if d.ActiveChapter != nil && d.ActiveChapter.Slug != "" {
		title = fmt.Sprintf("Diagram %d · %s", idx, d.ActiveChapter.Title)
		if d.ActiveChapter.LastCommitSHA != "" {
			pagePath = fmt.Sprintf("/chapters/%s/rev/%s/diagrams/%d", d.ActiveChapter.Slug, d.ActiveChapter.LastCommitSHA, idx)
		} else {
			pagePath = fmt.Sprintf("/chapters/%s/diagrams/%d", d.ActiveChapter.Slug, idx)
		}
	}
	doc := map[string]any{
		"@context": "https://schema.org",
		"@type":    "ImageObject",
		"name":     title,
		"url":      base + pagePath,
		"encoding": []map[string]any{
			{
				"@type":          "MediaObject",
				"encodingFormat": "text/markdown",
				"contentUrl":     base + d.MarkdownPath(),
				"description":    "Raw Mermaid diagram source (.mmd)",
			},
		},
		"isPartOf": buildBookPartOfJSONLD(base, d.Book),
	}
	return renderJSONLDScriptTag(doc)
}

var (
	sidebarPartPrefixRe    = regexp.MustCompile(`(?i)^part\s+`)
	sidebarChapterPrefixRe = regexp.MustCompile(`(?i)^chapter\s+(\d+(?:\.\d+)*)\s*[:.\-—]\s*`)
	sidebarAppPrefixRe     = regexp.MustCompile(`(?i)^appendix\s+([A-Z0-9]+)\s*[:.\-—]\s*`)
	sidebarFrontPrefixRe   = regexp.MustCompile(`(?i)^(?:introduction|preface|foreword|prologue|epilogue)\s*:\s*`)
	tableSepSnippetRe      = regexp.MustCompile(`\|?\s*:?-{3,}:?\s*\|`)
	tablePipeSnippetRe     = regexp.MustCompile(`\s+\|\s+`)
)

func FormatUnifiedDiffHTML(rawDiff string) template.HTML {
	if strings.TrimSpace(rawDiff) == "" {
		return template.HTML(`<span style="color:var(--text-muted);">(no textual diff)</span>`)
	}
	var out []string
	for _, line := range strings.Split(rawDiff, "\n") {
		if strings.HasPrefix(line, "diff --git ") || strings.HasPrefix(line, "index ") {
			continue
		}
		escaped := template.HTMLEscapeString(line)
		switch {
		case strings.HasPrefix(line, "+") && !strings.HasPrefix(line, "+++"):
			out = append(out, `<span class="diff-line-add">`+escaped+`</span>`)
		case strings.HasPrefix(line, "-") && !strings.HasPrefix(line, "---"):
			out = append(out, `<span class="diff-line-del">`+escaped+`</span>`)
		default:
			out = append(out, escaped)
		}
	}
	return template.HTML(strings.Join(out, "\n"))
}

// FormatSearchSnippetHTML escapes all untrusted HTML while preserving SQLite FTS5 <mark>...</mark> highlight tags.
func FormatSearchSnippetHTML(rawSnippet string) template.HTML {
	const openPlaceholder = "@@GHOSTBOOK_MARK_OPEN@@"
	const closePlaceholder = "@@GHOSTBOOK_MARK_CLOSE@@"
	s := strings.ReplaceAll(rawSnippet, "<mark>", openPlaceholder)
	s = strings.ReplaceAll(s, "</mark>", closePlaceholder)
	s = strings.ReplaceAll(s, "**", "")
	s = tableSepSnippetRe.ReplaceAllString(s, " ")
	s = tablePipeSnippetRe.ReplaceAllString(s, " · ")
	escaped := template.HTMLEscapeString(s)
	escaped = strings.ReplaceAll(escaped, openPlaceholder, "<mark>")
	escaped = strings.ReplaceAll(escaped, closePlaceholder, "</mark>")
	return template.HTML(escaped)
}

func FormatSidebarPartTitle(raw string) string {
	return sidebarPartPrefixRe.ReplaceAllString(strings.TrimSpace(raw), "")
}

func FormatSidebarChapterTitle(raw string) string {
	s := strings.TrimSpace(raw)
	if sidebarChapterPrefixRe.MatchString(s) {
		return sidebarChapterPrefixRe.ReplaceAllString(s, "$1. ")
	}
	if sidebarAppPrefixRe.MatchString(s) {
		return sidebarAppPrefixRe.ReplaceAllString(s, "$1. ")
	}
	return sidebarFrontPrefixRe.ReplaceAllString(s, "")
}

func FormatBriefingChapterChip(raw string) string {
	s := strings.TrimSpace(raw)
	if sidebarChapterPrefixRe.MatchString(s) {
		return sidebarChapterPrefixRe.ReplaceAllString(s, "Ch. $1 · ")
	}
	if sidebarAppPrefixRe.MatchString(s) {
		return sidebarAppPrefixRe.ReplaceAllString(s, "App. $1 · ")
	}
	return s
}

var templateFuncs = template.FuncMap{
	"studioCSS": func() template.CSS {
		return template.CSS(rawStudioCSS)
	},
	"studioJS": func() template.JS {
		return template.JS(rawStudioJS)
	},
	"guideJS": func() template.JS {
		return template.JS(rawGuideJS)
	},
	"formatTimestamp": func(raw string) string {
		s := strings.TrimSpace(raw)
		if t, err := time.Parse(time.RFC3339Nano, s); err == nil {
			return t.UTC().Format("2006-01-02 15:04 UTC")
		}
		if t, err := time.Parse(time.RFC3339, s); err == nil {
			return t.UTC().Format("2006-01-02 15:04 UTC")
		}
		return s
	},
	"sidebarPartTitle":      FormatSidebarPartTitle,
	"sidebarChapterTitle":   FormatSidebarChapterTitle,
	"formatUnifiedDiffHTML": FormatUnifiedDiffHTML,
	"formatSearchSnippet":   FormatSearchSnippetHTML,
}

var (
	homeTmpl      = mustParseTemplate("home.html")
	chapterTmpl   = mustParseTemplate("chapter.html")
	changelogTmpl = mustParseTemplate("changelog.html")
	printTmpl     = mustParseTemplate("print.html")
	guideTmpl     = mustParseTemplate("guide.html")
	apiDocsTmpl   = mustParseTemplate("apidocs.html")
	diagramTmpl   = mustParseTemplate("diagram.html")
	searchTmpl    = mustParseTemplate("search.html")
	historyTmpl   = mustParseTemplate("history.html")
	activityTmpl  = mustParseTemplate("activity.html")
)

func mustParseTemplate(fileName string) *template.Template {
	return template.Must(
		template.New(fileName).
			Funcs(templateFuncs).
			ParseFS(templatesFS, "templates/partials.html", "templates/"+fileName),
	)
}

func StudioCSS() string {
	return rawStudioCSS
}

func StudioJS() string {
	return rawStudioJS
}

func GuideJS() string {
	return rawGuideJS
}

func EmbeddedAssetsFS() fs.FS {
	sub, err := fs.Sub(assetsFS, "assets")
	if err != nil {
		panic(err)
	}
	return sub
}

func StaticAssetsHandler() http.Handler {
	sub := EmbeddedAssetsFS()
	fileServer := http.FileServer(http.FS(sub))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := r.URL.Path
		if strings.HasPrefix(p, "/vendor/") || strings.HasPrefix(p, "vendor/") {
			w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		} else {
			w.Header().Set("Cache-Control", "public, max-age=3600")
		}
		switch {
		case strings.HasSuffix(p, ".woff2"):
			w.Header().Set("Content-Type", "font/woff2")
		case strings.HasSuffix(p, ".css"):
			w.Header().Set("Content-Type", "text/css; charset=utf-8")
		case strings.HasSuffix(p, ".js"):
			w.Header().Set("Content-Type", "application/javascript; charset=utf-8")
		}
		fileServer.ServeHTTP(w, r)
	})
}

func RenderHomePage(data HomePageData) ([]byte, error) {
	var buf bytes.Buffer
	if err := homeTmpl.Execute(&buf, data); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func RenderChapterPage(data ChapterPageData) ([]byte, error) {
	var buf bytes.Buffer
	if err := chapterTmpl.Execute(&buf, data); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func RenderChangelogPage(data ChangelogPageData) ([]byte, error) {
	var buf bytes.Buffer
	if err := changelogTmpl.Execute(&buf, data); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func RenderPrintPage(data PrintPageData) ([]byte, error) {
	var buf bytes.Buffer
	if err := printTmpl.Execute(&buf, data); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func RenderGuidePage(data GuidePageData) ([]byte, error) {
	var buf bytes.Buffer
	if err := guideTmpl.Execute(&buf, data); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func RenderAPIDocsPage(baseURL ...string) ([]byte, error) {
	base := ""
	if len(baseURL) > 0 {
		base = baseURL[0]
	}
	return RenderAPIDocsPageWithSpec(APIDocsPageData{
		BaseURL:     base,
		SpecVersion: "3.1.0",
		Title:       "Ghostbook · API Reference",
	})
}

func RenderAPIDocsPageWithSpec(data APIDocsPageData) ([]byte, error) {
	if data.SpecVersion == "" {
		data.SpecVersion = "3.1.0"
	}
	if data.Title == "" {
		data.Title = data.DisplayBookTitle() + " · API Reference"
	}
	var buf bytes.Buffer
	if err := apiDocsTmpl.Execute(&buf, data); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func RenderSearchPage(data SearchPageData) ([]byte, error) {
	var buf bytes.Buffer
	if err := searchTmpl.Execute(&buf, data); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func RenderChapterHistoryPage(data ChapterHistoryPageData) ([]byte, error) {
	var buf bytes.Buffer
	if err := historyTmpl.Execute(&buf, data); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func RenderActivityPage(data ActivityPageData) ([]byte, error) {
	var buf bytes.Buffer
	if err := activityTmpl.Execute(&buf, data); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func RenderDiagramPage(data DiagramPageData) ([]byte, error) {
	var buf bytes.Buffer
	if err := diagramTmpl.Execute(&buf, data); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}
