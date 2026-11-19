package engine

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/wjkoh/ghostbook/internal/renderer"
)

// MarkdownExportResult holds an assembled raw Markdown document together with its
// resolved Git revision and HEAD comparison metadata for HTTP headers and caching.
type MarkdownExportResult struct {
	Markdown           string
	RevisionSHA        string
	RevisionNumber     int32
	HeadSHA            string
	HeadRevisionNumber int32
	IsLatest           bool
	WordCount          int32
}

type exportedChapter struct {
	Slug           string
	Title          string
	PartTitle      string
	StartsNewPart  bool
	Depth          int32
	Markdown       string
	WordCount      int32
	LastCommitSHA  string
	RevisionNumber int32
	LastAuthor     string
	LastUpdatedISO string
}

type exportedBookSnapshot struct {
	RevisionSHA        string
	RevisionNumber     int32
	HeadSHA            string
	HeadRevisionNumber int32
	IsLatest           bool
	EffectiveConfig    EffectiveBookConfig
	SummaryMarkdown    string
	Chapters           []exportedChapter
	TotalWords         int32
}

// ensureBalancedCodeFences guarantees that an unclosed backtick or tilde fenced code block
// at the end of a chapter is closed before concatenating subsequent chapters in a whole-book bundle.
func ensureBalancedCodeFences(md string) string {
	var openChar byte
	openLen := 0

	for _, rawLine := range strings.Split(md, "\n") {
		trimmedLeft := strings.TrimLeft(rawLine, " \t")
		trimmed := strings.TrimRight(trimmedLeft, " \t\r")
		if len(trimmed) < 3 {
			continue
		}
		ch := trimmed[0]
		if ch != '`' && ch != '~' {
			continue
		}
		run := 0
		for run < len(trimmed) && trimmed[run] == ch {
			run++
		}
		if run < 3 {
			continue
		}
		rest := trimmed[run:]
		if openChar == 0 {
			// In CommonMark, backtick info strings cannot contain backticks.
			if ch == '`' && strings.ContainsRune(rest, '`') {
				continue
			}
			openChar = ch
			openLen = run
		} else if ch == openChar && run >= openLen && strings.TrimSpace(rest) == "" {
			openChar = 0
			openLen = 0
		}
	}

	out := md
	if !strings.HasSuffix(out, "\n") {
		out += "\n"
	}
	if openChar != 0 {
		out += strings.Repeat(string(openChar), openLen) + "\n"
	}
	return out
}

func (e *BookEngine) collectBookAtRevision(revSpec string) (*exportedBookSnapshot, error) {
	cleanRev := strings.TrimSpace(revSpec)
	snap := e.CurrentSnapshot()
	matchesCurrentHead := cleanRev == "" || strings.EqualFold(cleanRev, "HEAD")
	if !matchesCurrentHead && snap != nil {
		if strings.EqualFold(cleanRev, snap.HeadCommitSHA) ||
			(snap.HeadRevisionNumber > 0 && strings.EqualFold(cleanRev, fmt.Sprintf("r%d", snap.HeadRevisionNumber))) {
			matchesCurrentHead = true
		}
	}
	if matchesCurrentHead {
		if snap == nil {
			return nil, fmt.Errorf("%w: snapshot not initialized", ErrNotFound)
		}
		chapters := make([]exportedChapter, 0, len(snap.Chapters))
		var totalWords int32
		for _, ch := range snap.Chapters {
			chapters = append(chapters, exportedChapter{
				Slug:           ch.Slug,
				Title:          ch.Title,
				PartTitle:      ch.PartTitle,
				StartsNewPart:  ch.StartsNewPart,
				Depth:          ch.Depth,
				Markdown:       ch.Markdown,
				WordCount:      ch.WordCount,
				LastCommitSHA:  ch.LastCommitSHA,
				RevisionNumber: ch.RevisionNumber,
				LastAuthor:     ch.LastAuthor,
				LastUpdatedISO: ch.LastUpdatedISO,
			})
			totalWords += ch.WordCount
		}
		return &exportedBookSnapshot{
			RevisionSHA:        snap.HeadCommitSHA,
			RevisionNumber:     snap.HeadRevisionNumber,
			HeadSHA:            snap.HeadCommitSHA,
			HeadRevisionNumber: snap.HeadRevisionNumber,
			IsLatest:           true,
			EffectiveConfig:    e.EffectiveConfig(),
			SummaryMarkdown:    snap.SummaryMarkdown,
			Chapters:           chapters,
			TotalWords:         totalWords,
		}, nil
	}

	e.mu.RLock()
	defer e.mu.RUnlock()

	chronological, revMap, err := e.orderedCommitsLocked()
	if err != nil || len(chronological) == 0 {
		return nil, fmt.Errorf("%w: commit history unavailable", ErrNotFound)
	}
	headCommit := chronological[len(chronological)-1]
	headSHA := shortSHA(headCommit.Hash.String())
	headRevNum := revMap[headCommit.Hash.String()]

	targetCommit, err := e.resolveCommitByPrefixLocked(cleanRev)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrNotFound, err)
	}

	targetIdx := len(chronological) - 1
	for i, c := range chronological {
		if c.Hash == targetCommit.Hash {
			targetIdx = i
			break
		}
	}

	summaryText, _ := readFileAtCommit(targetCommit, "src/SUMMARY.md")
	parsed := renderer.ParseSummaryMarkdown(summaryText)
	seen := make(map[string]bool, len(parsed))
	var entries []renderer.SummaryEntry
	for _, item := range parsed {
		if slug, vErr := ValidateSlug(item.Slug); vErr == nil && !seen[slug] {
			seen[slug] = true
			item.Slug = slug
			entries = append(entries, item)
		}
	}

	if tree, tErr := targetCommit.Tree(); tErr == nil {
		var extras []string
		_ = tree.Files().ForEach(func(f *object.File) error {
			if !strings.HasPrefix(f.Name, "src/") || !strings.HasSuffix(f.Name, ".md") {
				return nil
			}
			base := strings.TrimSuffix(strings.TrimPrefix(f.Name, "src/"), ".md")
			if base == "SUMMARY" || base == "STYLE_GUIDE" {
				return nil
			}
			if slug, vErr := ValidateSlug(base); vErr == nil && !seen[slug] {
				seen[slug] = true
				extras = append(extras, slug)
			}
			return nil
		})
		sort.Strings(extras)
		for _, slug := range extras {
			entries = append(entries, renderer.SummaryEntry{Slug: slug})
		}
	}

	latestByPath := make(map[string]*object.Commit, len(entries))
	for i := targetIdx; i >= 0; i-- {
		files, _ := commitModifiedFiles(chronological[i])
		for _, p := range files {
			if _, seen := latestByPath[p]; !seen {
				latestByPath[p] = chronological[i]
			}
		}
	}

	var chapters []exportedChapter
	var totalWords int32
	carryNewPart := false
	carryPartTitle := ""

	for _, item := range entries {
		if item.StartsNewPart {
			carryNewPart = true
			carryPartTitle = item.PartTitle
		} else if item.PartTitle != carryPartTitle {
			carryNewPart = false
			carryPartTitle = item.PartTitle
		}

		relPath := "src/" + item.Slug + ".md"
		mdText, ok := readFileAtCommit(targetCommit, relPath)
		if !ok {
			continue
		}

		chapCommit := latestByPath[relPath]
		if chapCommit == nil {
			chapCommit = targetCommit
		}

		title := extractMarkdownH1(mdText)
		if title == "" {
			if item.SummaryTitle != "" {
				title = item.SummaryTitle
			} else {
				title = item.Slug
			}
		}
		words := int32(len(strings.Fields(mdText)))
		chapSHA := shortSHA(chapCommit.Hash.String())
		chapRev := revMap[chapCommit.Hash.String()]
		if chapRev <= 0 {
			chapRev = 1
		}

		startsPart := item.StartsNewPart || (carryNewPart && item.PartTitle == carryPartTitle && item.PartTitle != "")
		carryNewPart = false

		chapters = append(chapters, exportedChapter{
			Slug:           item.Slug,
			Title:          title,
			PartTitle:      item.PartTitle,
			StartsNewPart:  startsPart,
			Depth:          item.Depth,
			Markdown:       mdText,
			WordCount:      words,
			LastCommitSHA:  chapSHA,
			RevisionNumber: chapRev,
			LastAuthor:     chapCommit.Author.Name,
			LastUpdatedISO: chapCommit.Author.When.UTC().Format(time.RFC3339),
		})
		totalWords += words
	}

	targetSHA := shortSHA(targetCommit.Hash.String())
	targetRevNum := revMap[targetCommit.Hash.String()]
	if targetRevNum <= 0 {
		targetRevNum = 1
	}
	var histConfigRaw []byte
	if cfgText, ok := readFileAtCommit(targetCommit, "src/book.json"); ok {
		histConfigRaw = []byte(cfgText)
	}
	histEffCfg, _ := ResolveEffectiveBookConfig(e.cfg, histConfigRaw)

	return &exportedBookSnapshot{
		RevisionSHA:        targetSHA,
		RevisionNumber:     targetRevNum,
		HeadSHA:            headSHA,
		HeadRevisionNumber: headRevNum,
		IsLatest:           targetCommit.Hash == headCommit.Hash,
		EffectiveConfig:    histEffCfg,
		SummaryMarkdown:    summaryText,
		Chapters:           chapters,
		TotalWords:         totalWords,
	}, nil
}

// RenderBookAtRevision renders a full RenderedBook snapshot at a specific Git revision
// (used by historical /print?rev=... and /rev/{rev}/print routes).
func (e *BookEngine) RenderBookAtRevision(revSpec string) (*renderer.RenderedBook, error) {
	cleanRev := strings.TrimSpace(revSpec)
	snap := e.CurrentSnapshot()
	if cleanRev == "" || strings.EqualFold(cleanRev, "HEAD") {
		if snap == nil {
			return nil, fmt.Errorf("%w: snapshot not initialized", ErrNotFound)
		}
		return snap, nil
	}
	exp, err := e.collectBookAtRevision(cleanRev)
	if err != nil {
		return nil, err
	}
	if exp.IsLatest && snap != nil {
		return snap, nil
	}
	chapters := make([]*renderer.ChapterView, 0, len(exp.Chapters))
	bySlug := make(map[string]*renderer.ChapterView, len(exp.Chapters))
	for _, c := range exp.Chapters {
		renderedHTML, parsedTitle, _, wordCount, rErr := renderer.RenderMarkdownToHTML(c.Markdown)
		if rErr != nil {
			return nil, rErr
		}
		title := c.Title
		if title == "" {
			title = parsedTitle
		}
		cv := &renderer.ChapterView{
			Slug:             c.Slug,
			Title:            title,
			PartTitle:        c.PartTitle,
			StartsNewPart:    c.StartsNewPart,
			Depth:            c.Depth,
			Markdown:         c.Markdown,
			RenderedHTML:     renderedHTML,
			WordCount:        wordCount,
			OpenCommentCount: countChapterInlineCallouts(c.Markdown),
			LastCommitSHA:    c.LastCommitSHA,
			RevisionNumber:   c.RevisionNumber,
			LastAuthor:       c.LastAuthor,
			LastUpdatedISO:   c.LastUpdatedISO,
			IsPermalinkView:  true,
			PermalinkURL:     fmt.Sprintf("%s/chapters/%s/rev/%s", e.baseURL, c.Slug, c.LastCommitSHA),
		}
		chapters = append(chapters, cv)
		bySlug[c.Slug] = cv
	}
	var authorViews []renderer.AuthorProfileView
	if snap := e.CurrentSnapshot(); snap != nil && len(snap.Authors) > 0 {
		authorViews = snap.Authors
	} else if dbAuthors, aErr := e.store.ListAuthors(context.Background()); aErr == nil {
		for _, a := range dbAuthors {
			authorViews = append(authorViews, renderer.AuthorProfileView{
				ID:          a.ID,
				DisplayName: a.DisplayName,
				Email:       a.GitEmail,
				RoleBadge:   a.RoleBadge,
			})
		}
	}
	book := &renderer.RenderedBook{
		BookTitle:          exp.EffectiveConfig.BookTitle,
		BookKicker:         exp.EffectiveConfig.BookKicker,
		MCPName:            e.MCPName(),
		HidePoweredBy:      exp.EffectiveConfig.HidePoweredBy,
		Authors:            authorViews,
		HeadCommitSHA:      exp.RevisionSHA,
		HeadRevisionNumber: exp.RevisionNumber,
		SummaryMarkdown:    exp.SummaryMarkdown,
		Chapters:           chapters,
		BySlug:             bySlug,
	}
	return book, nil
}

// AssembleLLMsIndex generates the llmstxt.org discovery manifest (/llms.txt) at HEAD
// or at any historical commit SHA / sequential revision number (r1..rN).
func (e *BookEngine) AssembleLLMsIndex(_ context.Context, revSpec string) (*MarkdownExportResult, error) {
	book, err := e.collectBookAtRevision(revSpec)
	if err != nil {
		return nil, err
	}
	base := e.BaseURL()
	var b strings.Builder

	fmt.Fprintf(&b, "# %s\n\n", book.EffectiveConfig.BookTitle)
	if book.IsLatest {
		kickerClause := ""
		if k := strings.TrimSpace(book.EffectiveConfig.BookKicker); k != "" && !strings.EqualFold(k, "Living Book") {
			kickerClause = " (" + k + ")"
		}
		fmt.Fprintf(&b, "> Canonical Git-backed living book%s. Current HEAD revision: r%d (%s) · %d chapters · %d words.\n\n", kickerClause,
			book.RevisionNumber, book.RevisionSHA, len(book.Chapters), book.TotalWords)
		if snap := e.CurrentSnapshot(); snap != nil && (len(snap.TimelineEntries) > 0 || snap.PendingTimelineCommits > 0 || len(snap.OpenCallouts) > 0) {
			b.WriteString("## Timeline\n\n")
			writeTimelineMarkdownItems(&b, base, snap, DefaultLLMsTimelineLimit, true)
			b.WriteString("\n")
		}
	} else {
		fmt.Fprintf(&b, "> Historical Snapshot of %s pinned to revision r%d (%s) · %d chapters · %d words. Newer HEAD revision r%d (%s) is available at %s/llms.txt.\n\n", book.EffectiveConfig.BookTitle,
			book.RevisionNumber, book.RevisionSHA, len(book.Chapters), book.TotalWords,
			book.HeadRevisionNumber, book.HeadSHA, base)
	}

	b.WriteString("## Whole-Book Raw Markdown (Single-Request Ingest)\n\n")
	if book.IsLatest {
		fmt.Fprintf(&b, "- [Full Book Raw Markdown (Live HEAD)](%s/llms-full.txt): Complete %d-chapter book (%d words) concatenated in Table of Contents order with zero HTML.\n",
			base, len(book.Chapters), book.TotalWords)
		fmt.Fprintf(&b, "- [Full Book Raw Markdown Mirror](%s/book.md): Identical whole-book raw Markdown bundle (supports ?rev=r1..r%d).\n",
			base, book.RevisionNumber)
	}
	fmt.Fprintf(&b, "- [Pinned Revision r%d (%s) Full Book Raw Markdown](%s/rev/%s/book.md): Immutable whole-book Markdown snapshot at commit %s.\n\n",
		book.RevisionNumber, book.RevisionSHA, base, book.RevisionSHA, book.RevisionSHA)

	b.WriteString("## Table of Contents (Per-Chapter Raw Markdown)\n\n")
	for idx, ch := range book.Chapters {
		if ch.StartsNewPart && ch.PartTitle != "" {
			if idx > 0 {
				b.WriteByte('\n')
			}
			fmt.Fprintf(&b, "### %s\n\n", ch.PartTitle)
		}
		indent := strings.Repeat("  ", int(ch.Depth))
		primaryMD := fmt.Sprintf("%s/chapters/%s.md", base, ch.Slug)
		permMD := fmt.Sprintf("%s/rev/%s/chapters/%s.md", base, ch.LastCommitSHA, ch.Slug)
		permRevNum := ch.RevisionNumber
		permSHA := ch.LastCommitSHA
		if !book.IsLatest {
			primaryMD = fmt.Sprintf("%s/rev/%s/chapters/%s.md", base, book.RevisionSHA, ch.Slug)
			permMD = primaryMD
			permRevNum = book.RevisionNumber
			permSHA = book.RevisionSHA
		}
		fmt.Fprintf(&b, "%s- [%s](%s) · [Permalink r%d (%s)](%s) · %d words · Updated %s by %s\n",
			indent, ch.Title, primaryMD, permRevNum, permSHA, permMD, ch.WordCount, ch.LastUpdatedISO, ch.LastAuthor)
	}
	b.WriteString("\n")

	b.WriteString("## Meta Documents & Agent Protocols\n\n")
	fmt.Fprintf(&b, "- [Raw Table of Contents (SUMMARY.md)](%s/SUMMARY.md): GitBook/mdBook chapter hierarchy manifest.\n", base)
	fmt.Fprintf(&b, "- [Editorial Style Guide (STYLE_GUIDE.md)](%s/STYLE_GUIDE.md): Prose, math, formatting, and live MCP agent rules.\n", base)
	fmt.Fprintf(&b, "- [Book Configuration (book.json)](%s/book.json): Active title, kicker, linter toggles, banned words, and timeline guidance.\n", base)
	fmt.Fprintf(&b, "- [Editorial Timeline RSS Feed (feed.xml)](%s/feed.xml): RSS 2.0 subscription feed of synthesized Timeline milestones (aliases: /timeline.xml, /rss.xml).\n", base)
	fmt.Fprintf(&b, "- [Git Commit Log (changelog.md)](%s/changelog.md): Chronological Git commits r1..r%d with modified files.\n", base, book.HeadRevisionNumber)
	fmt.Fprintf(&b, "- [Co-Author AI & MCP Setup Guide (guide.md)](%s/guide.md): Instructions for HTTP raw Markdown, Accept: text/markdown content negotiation, Remote MCP, and ConnectRPC.\n", base)
	fmt.Fprintf(&b, "- [OpenAPI 3.1 Specification (openapi.json)](%s/openapi.json): Machine-readable ConnectRPC JSON schema for authenticated authoring.\n", base)
	fmt.Fprintf(&b, "- Remote MCP Endpoint: %s/u/<email>:<secret-key>/mcp\n", base)
	if extra := strings.TrimSpace(book.EffectiveConfig.ExtraLLMsMarkdown); extra != "" {
		if !strings.HasPrefix(extra, "- ") && !strings.HasPrefix(extra, "* ") {
			b.WriteByte('\n')
		}
		b.WriteString(extra)
		if !strings.HasSuffix(extra, "\n") {
			b.WriteByte('\n')
		}
	}

	md := b.String()
	return &MarkdownExportResult{
		Markdown:           md,
		RevisionSHA:        book.RevisionSHA,
		RevisionNumber:     book.RevisionNumber,
		HeadSHA:            book.HeadSHA,
		HeadRevisionNumber: book.HeadRevisionNumber,
		IsLatest:           book.IsLatest,
		WordCount:          int32(len(strings.Fields(md))),
	}, nil
}

// AssembleFullBookMarkdown concatenates the entire book in Table of Contents order into
// a single zero-HTML Markdown document (/llms-full.txt, /book.md, /print.md, /rev/{rev}/book.md).
func (e *BookEngine) AssembleFullBookMarkdown(_ context.Context, revSpec string) (*MarkdownExportResult, error) {
	book, err := e.collectBookAtRevision(revSpec)
	if err != nil {
		return nil, err
	}
	base := e.BaseURL()
	var b strings.Builder

	fmt.Fprintf(&b, "# %s (Complete Raw Markdown Edition)\n\n", book.EffectiveConfig.BookTitle)
	if !book.IsLatest {
		fmt.Fprintf(&b, "> [!WARNING]\n> **Historical Whole-Book Snapshot:** Viewing revision **r%d (%s)**. Newer HEAD revision **r%d (%s)** is available at %s/book.md (and %s/llms-full.txt).\n\n",
			book.RevisionNumber, book.RevisionSHA, book.HeadRevisionNumber, book.HeadSHA, base, base)
	}
	fmt.Fprintf(&b, "- **Repository Revision:** r%d (%s)\n", book.RevisionNumber, book.RevisionSHA)
	fmt.Fprintf(&b, "- **Latest HEAD Revision:** r%d (%s)\n", book.HeadRevisionNumber, book.HeadSHA)
	fmt.Fprintf(&b, "- **Chapters:** %d\n", len(book.Chapters))
	fmt.Fprintf(&b, "- **Total Chapter Words:** %d\n", book.TotalWords)
	if snap := e.CurrentSnapshot(); snap != nil && len(snap.Authors) > 0 {
		var parts []string
		for _, a := range snap.Authors {
			if strings.TrimSpace(a.Email) != "" {
				parts = append(parts, fmt.Sprintf("%s (%s)", a.DisplayName, a.Email))
			} else {
				parts = append(parts, a.DisplayName)
			}
		}
		fmt.Fprintf(&b, "- **Co-Authors:** %s\n", strings.Join(parts, " · "))
	}
	fmt.Fprintf(&b, "- **Discovery Index:** %s/llms.txt\n\n", base)

	b.WriteString("## Table of Contents\n\n")
	for idx, ch := range book.Chapters {
		if ch.StartsNewPart && ch.PartTitle != "" {
			if idx > 0 {
				b.WriteByte('\n')
			}
			fmt.Fprintf(&b, "### %s\n\n", ch.PartTitle)
		}
		indent := strings.Repeat("  ", int(ch.Depth))
		tocLink := fmt.Sprintf("%s/chapters/%s.md", base, ch.Slug)
		if !book.IsLatest {
			tocLink = fmt.Sprintf("%s/rev/%s/chapters/%s.md", base, book.RevisionSHA, ch.Slug)
		}
		fmt.Fprintf(&b, "%s- [%s](%s) (r%d · %s · %d words)\n",
			indent, ch.Title, tocLink, ch.RevisionNumber, ch.LastCommitSHA, ch.WordCount)
	}
	b.WriteString("\n")

	for _, ch := range book.Chapters {
		if ch.StartsNewPart && ch.PartTitle != "" {
			fmt.Fprintf(&b, "# %s\n\n", ch.PartTitle)
		}
		permRev := ch.LastCommitSHA
		if !book.IsLatest {
			permRev = book.RevisionSHA
		}
		fmt.Fprintf(&b, "> **Chapter Slug:** %s · **Revision:** r%d (%s) · **Last Author:** %s · **Updated:** %s  \n",
			ch.Slug, ch.RevisionNumber, ch.LastCommitSHA, ch.LastAuthor, ch.LastUpdatedISO)
		fmt.Fprintf(&b, "> **Live Markdown:** %s/chapters/%s.md · **Permalink Markdown:** %s/rev/%s/chapters/%s.md\n\n",
			base, ch.Slug, base, permRev, ch.Slug)

		b.WriteString(ensureBalancedCodeFences(ch.Markdown))
		b.WriteString("\n")
	}

	md := b.String()
	return &MarkdownExportResult{
		Markdown:           md,
		RevisionSHA:        book.RevisionSHA,
		RevisionNumber:     book.RevisionNumber,
		HeadSHA:            book.HeadSHA,
		HeadRevisionNumber: book.HeadRevisionNumber,
		IsLatest:           book.IsLatest,
		WordCount:          book.TotalWords,
	}, nil
}

// AssembleChangelogMarkdown renders the chronological Git revision ledger in pure Markdown (/changelog.md).
func (e *BookEngine) AssembleChangelogMarkdown(_ context.Context) (*MarkdownExportResult, error) {
	snap := e.CurrentSnapshot()
	if snap == nil {
		return nil, fmt.Errorf("%w: snapshot not initialized", ErrNotFound)
	}
	base := e.BaseURL()
	var b strings.Builder

	fmt.Fprintf(&b, "# Git Commit Log · %s\n\n", e.BookTitle())
	fmt.Fprintf(&b, "Current HEAD: **r%d (%s)** · Timeline & Discovery Index: %s/llms.txt · Full Book Raw Markdown: %s/llms-full.txt\n\n",
		snap.HeadRevisionNumber, snap.HeadCommitSHA, base, base)

	b.WriteString("| Revision | Author | Timestamp (UTC) | Summary | Modified Files & Raw Markdown Permalinks |\n")
	b.WriteString("| :--- | :--- | :--- | :--- | :--- |\n")

	for _, c := range snap.RecentCommits {
		revLabel := c.ShortSHA
		if c.RevisionNumber > 0 {
			revLabel = fmt.Sprintf("r%d · %s", c.RevisionNumber, c.ShortSHA)
		}
		bookPerm := fmt.Sprintf("[%s](%s/rev/%s/book.md)", revLabel, base, c.ShortSHA)
		var fileLinks []string
		for _, f := range c.FilesChanged {
			if strings.HasPrefix(f, "src/") && strings.HasSuffix(f, ".md") {
				slug := strings.TrimSuffix(strings.TrimPrefix(f, "src/"), ".md")
				if slug == "SUMMARY" || slug == "STYLE_GUIDE" {
					fileLinks = append(fileLinks, fmt.Sprintf("[%s](%s/rev/%s/%s.md)", f, base, c.ShortSHA, slug))
				} else {
					fileLinks = append(fileLinks, fmt.Sprintf("[%s](%s/rev/%s/chapters/%s.md)", slug, base, c.ShortSHA, slug))
				}
			} else if f == "src/book.json" {
				fileLinks = append(fileLinks, fmt.Sprintf("[%s](%s/rev/%s/book.json)", f, base, c.ShortSHA))
			} else {
				fileLinks = append(fileLinks, f)
			}
		}
		escCell := func(s string) string {
			return strings.TrimSpace(strings.NewReplacer("\r", " ", "\n", " ", "|", "\\|").Replace(s))
		}
		summary := escCell(c.CommitMessage)
		filesCell := escCell(strings.Join(fileLinks, ", "))
		fmt.Fprintf(&b, "| %s | %s (%s) | %s | %s | %s |\n",
			bookPerm, escCell(c.AuthorName), escCell(c.AuthorEmail), c.TimestampISO, summary, filesCell)
	}

	md := b.String()
	return &MarkdownExportResult{
		Markdown:           md,
		RevisionSHA:        snap.HeadCommitSHA,
		RevisionNumber:     snap.HeadRevisionNumber,
		HeadSHA:            snap.HeadCommitSHA,
		HeadRevisionNumber: snap.HeadRevisionNumber,
		IsLatest:           true,
		WordCount:          int32(len(strings.Fields(md))),
	}, nil
}

const DefaultLLMsTimelineLimit = 15

func writeTimelineMarkdownItems(b *strings.Builder, base string, snap *renderer.RenderedBook, limit int, includeCallouts bool) {
	if snap.PendingTimelineCommits > 0 {
		revSpan := fmt.Sprintf("r%d", snap.PendingEndRevision)
		if snap.PendingStartRevision != snap.PendingEndRevision {
			revSpan = fmt.Sprintf("r%d → r%d", snap.PendingStartRevision, snap.PendingEndRevision)
		}
		fmt.Fprintf(b, "> Active editing session: %d recent commit(s) (%s) settling before next immutable timeline milestone (%s/changelog.md).\n\n",
			snap.PendingTimelineCommits, revSpan, base)
	}

	entries := snap.TimelineEntries
	if limit > 0 && len(entries) > limit {
		entries = entries[:limit]
	}
	for _, m := range entries {
		revLabel := fmt.Sprintf("r%d", m.EndRevision)
		if m.StartRevision != m.EndRevision {
			revLabel = fmt.Sprintf("r%d → r%d", m.StartRevision, m.EndRevision)
		}
		authorMeta := ""
		if m.Authors != "" {
			authorMeta = " · " + m.Authors
		}
		var chapRefs []string
		for _, ch := range m.Chapters {
			chapRefs = append(chapRefs, fmt.Sprintf("[%s](%s/chapters/%s.md)", ch.Label, base, ch.Slug))
		}
		chapSuffix := ""
		if len(chapRefs) > 0 {
			chapSuffix = " — " + strings.Join(chapRefs, ", ")
		}
		fmt.Fprintf(b, "- **%s** (%s · %s · %d commit(s)%s): %s%s\n",
			m.Title, m.TimestampISO, revLabel, m.CommitCount, authorMeta, m.Summary, chapSuffix)
	}
	if limit > 0 && len(snap.TimelineEntries) > limit {
		fmt.Fprintf(b, "- _Showing latest %d of %d milestones (full timeline at %s/timeline?all=1 or %s/timeline.md)._\n",
			limit, len(snap.TimelineEntries), base, base)
	}

	if includeCallouts && len(snap.OpenCallouts) > 0 {
		b.WriteString("\n### Open Editorial Callouts\n\n")
		for _, oc := range snap.OpenCallouts {
			authorPrefix := ""
			if oc.AuthorLabel != "" {
				authorPrefix = oc.AuthorLabel + " · "
			}
			fmt.Fprintf(b, "- [%s] %s[%s](%s/chapters/%s.md): %s\n",
				oc.Kind, authorPrefix, oc.ChapterName, base, oc.ChapterSlug, oc.Excerpt)
		}
	}
}

// AssembleTimelineMarkdown renders the complete semantic Timeline and Open Callouts in pure Markdown (/timeline.md).
func (e *BookEngine) AssembleTimelineMarkdown(_ context.Context) (*MarkdownExportResult, error) {
	snap := e.CurrentSnapshot()
	if snap == nil {
		return nil, fmt.Errorf("%w: snapshot not initialized", ErrNotFound)
	}
	base := e.BaseURL()
	var b strings.Builder

	fmt.Fprintf(&b, "# Timeline · %s\n\n", e.BookTitle())
	fmt.Fprintf(&b, "> Current HEAD: **r%d (%s)** · %d chapters · %d milestones · RSS 2.0: %s/feed.xml.\n\n",
		snap.HeadRevisionNumber, snap.HeadCommitSHA, len(snap.Chapters), len(snap.TimelineEntries), base)

	if len(snap.TimelineEntries) == 0 && snap.PendingTimelineCommits == 0 && len(snap.OpenCallouts) == 0 {
		b.WriteString("_No semantic timeline milestones synthesized yet._\n")
	} else {
		if len(snap.TimelineEntries) == 0 && snap.PendingTimelineCommits == 0 {
			b.WriteString("_No semantic timeline milestones synthesized yet._\n")
		}
		writeTimelineMarkdownItems(&b, base, snap, 0, true)
	}

	md := b.String()
	return &MarkdownExportResult{
		Markdown:           md,
		RevisionSHA:        snap.HeadCommitSHA,
		RevisionNumber:     snap.HeadRevisionNumber,
		HeadSHA:            snap.HeadCommitSHA,
		HeadRevisionNumber: snap.HeadRevisionNumber,
		IsLatest:           true,
		WordCount:          int32(len(strings.Fields(md))),
	}, nil
}

// AssembleSearchMarkdown renders full-text book search results in pure Markdown (/search.md?q=...).
func (e *BookEngine) AssembleSearchMarkdown(ctx context.Context, query, slugFilter string) (*MarkdownExportResult, error) {
	snap := e.CurrentSnapshot()
	if snap == nil {
		return nil, fmt.Errorf("%w: snapshot not initialized", ErrNotFound)
	}
	base := e.BaseURL()
	q := strings.TrimSpace(query)
	slug := strings.TrimSpace(slugFilter)

	var b strings.Builder
	fmt.Fprintf(&b, "# Full-Text Book Search · %s\n\n", e.BookTitle())
	if q == "" {
		fmt.Fprintf(&b, "Pass `?q=<keywords>` (and optional `&slug=<chapter-slug>`) to search across %d chapters.\n", len(snap.Chapters))
	} else {
		out, err := e.SearchBook(ctx, &SearchBookInput{
			Query: q,
			Slug:  slug,
			Limit: 25,
		})
		if err != nil {
			return nil, err
		}
		fmt.Fprintf(&b, "> Query: **%q**", q)
		if slug != "" {
			fmt.Fprintf(&b, " · Slug filter: %q", slug)
		}
		fmt.Fprintf(&b, " · Matches: **%d**\n\n", len(out.Matches))

		if len(out.Matches) == 0 {
			b.WriteString("_No matching chapters found._\n")
		} else {
			for _, m := range out.Matches {
				cleanSnip := strings.ReplaceAll(strings.ReplaceAll(m.Snippet, "<mark>", "**"), "</mark>", "**")
				fmt.Fprintf(&b, "## [%s](%s/chapters/%s.md) (`%s`)\n\n", m.Title, base, m.Slug, m.Slug)
				if cleanSnip != "" {
					fmt.Fprintf(&b, "%s\n\n", cleanSnip)
				}
				for _, lm := range m.LineMatches {
					fmt.Fprintf(&b, "- `L%d`: %s\n", lm.LineNumber, lm.LineText)
				}
				if len(m.LineMatches) > 0 {
					b.WriteString("\n")
				}
			}
		}
	}

	md := b.String()
	return &MarkdownExportResult{
		Markdown:           md,
		RevisionSHA:        snap.HeadCommitSHA,
		RevisionNumber:     snap.HeadRevisionNumber,
		HeadSHA:            snap.HeadCommitSHA,
		HeadRevisionNumber: snap.HeadRevisionNumber,
		IsLatest:           true,
		WordCount:          int32(len(strings.Fields(md))),
	}, nil
}

// safeBacktickFenceFor returns a backtick fence strictly longer than any backtick run in body.
func safeBacktickFenceFor(body string) string {
	maxRun := 0
	curRun := 0
	for i := 0; i < len(body); i++ {
		if body[i] == '`' {
			curRun++
			if curRun > maxRun {
				maxRun = curRun
			}
		} else {
			curRun = 0
		}
	}
	if maxRun < 3 {
		return "```"
	}
	return strings.Repeat("`", maxRun+1)
}

// AssembleChapterHistoryMarkdown renders section-level Git blame and recent unified diffs for a chapter (/chapters/{slug}/history.md).
func (e *BookEngine) AssembleChapterHistoryMarkdown(ctx context.Context, slug string) (*MarkdownExportResult, error) {
	snap := e.CurrentSnapshot()
	if snap == nil {
		return nil, fmt.Errorf("%w: snapshot not initialized", ErrNotFound)
	}
	normSlug, _, err := NormalizeChapterOrMetaSlug(slug)
	if err != nil {
		return nil, err
	}
	ch, ok := snap.BySlug[normSlug]
	if !ok || ch == nil {
		return nil, ErrNotFound
	}
	blameOut, err := e.GetChapterBlame(ctx, &GetChapterBlameInput{Slug: normSlug})
	if err != nil {
		return nil, err
	}
	commitsOut, err := e.ListRecentChanges(ctx, &ListRecentChangesInput{Slug: normSlug, Limit: 15})
	if err != nil {
		return nil, err
	}

	var b strings.Builder
	fmt.Fprintf(&b, "# History & Blame · %s (`%s`)\n\n", ch.Title, ch.Slug)
	fmt.Fprintf(&b, "> Current chapter revision: **r%d (%s)** · Last edited by **%s**.\n\n", ch.RevisionNumber, ch.LastCommitSHA, ch.LastAuthor)

	b.WriteString("## Section-Level Git Blame\n\n")
	for _, s := range blameOut.Sections {
		fmt.Fprintf(&b, "- **%s** (`%s` · lines %d–%d): %s (%s)\n",
			s.Heading, s.LastCommitSHA, s.StartLine, s.EndLine, s.LastAuthor, s.LastUpdatedISO)
	}
	b.WriteString("\n## Recent Commits & Unified Diffs\n\n")
	for _, c := range commitsOut.Commits {
		fmt.Fprintf(&b, "### r%d (`%s`) · %s · %s\n\n%s\n\n",
			c.RevisionNumber, c.CommitSHA, c.AuthorName, c.TimestampISO, c.CommitMessage)
		if diffOut, dErr := e.GetCommitDiff(ctx, &GetCommitDiffInput{CommitSHA: c.CommitSHA, Slug: normSlug}); dErr == nil && strings.TrimSpace(diffOut.UnifiedDiff) != "" {
			diffBody := strings.TrimRight(diffOut.UnifiedDiff, "\n")
			fence := safeBacktickFenceFor(diffBody)
			b.WriteString(fence + "diff\n")
			b.WriteString(diffBody + "\n")
			b.WriteString(fence + "\n\n")
		}
	}

	md := b.String()
	return &MarkdownExportResult{
		Markdown:           md,
		RevisionSHA:        ch.LastCommitSHA,
		RevisionNumber:     ch.RevisionNumber,
		HeadSHA:            snap.HeadCommitSHA,
		HeadRevisionNumber: snap.HeadRevisionNumber,
		IsLatest:           true,
		WordCount:          int32(len(strings.Fields(md))),
	}, nil
}

// AssembleActivityMarkdown renders the authenticated agent tool audit log in pure Markdown (/activity.md).
func (e *BookEngine) AssembleActivityMarkdown(ctx context.Context, authorFilter string) (*MarkdownExportResult, error) {
	snap := e.CurrentSnapshot()
	if snap == nil {
		return nil, fmt.Errorf("%w: snapshot not initialized", ErrNotFound)
	}
	actOut, err := e.ListAgentActivity(ctx, &ListAgentActivityInput{
		Limit:    50,
		AuthorID: strings.TrimSpace(authorFilter),
	})
	if err != nil {
		return nil, err
	}

	var b strings.Builder
	fmt.Fprintf(&b, "# Agent Tool Audit Log · %s\n\n", e.BookTitle())
	if len(actOut.Entries) == 0 {
		b.WriteString("_No agent tool invocations recorded yet._\n")
	} else {
		b.WriteString("| Timestamp (UTC) | Author | Protocol | Tool | Target | Status | Duration |\n")
		b.WriteString("| :--- | :--- | :--- | :--- | :--- | :--- | :--- |\n")
		for _, item := range actOut.Entries {
			target := strings.TrimSpace(strings.NewReplacer("\r", " ", "\n", " ", "|", "\\|").Replace(item.ChapterSlug))
			if target == "" {
				target = "—"
			}
			fmt.Fprintf(&b, "| %s | %s | %s | %s | %s | %s | %d ms |\n",
				item.TimestampISO, item.AuthorID, item.Protocol, item.ToolName, target, item.Status, item.DurationMs)
		}
	}

	md := b.String()
	return &MarkdownExportResult{
		Markdown:           md,
		RevisionSHA:        snap.HeadCommitSHA,
		RevisionNumber:     snap.HeadRevisionNumber,
		HeadSHA:            snap.HeadCommitSHA,
		HeadRevisionNumber: snap.HeadRevisionNumber,
		IsLatest:           true,
		WordCount:          int32(len(strings.Fields(md))),
	}, nil
}

// AssembleGuideMarkdown renders the AI Agent & Co-Author Setup Guide in pure Markdown (/guide.md).
func (e *BookEngine) AssembleGuideMarkdown(_ context.Context) (*MarkdownExportResult, error) {
	snap := e.CurrentSnapshot()
	if snap == nil {
		return nil, fmt.Errorf("%w: snapshot not initialized", ErrNotFound)
	}
	base := e.BaseURL()
	var b strings.Builder

	fmt.Fprintf(&b, "# AI Agent & Co-Author Setup Guide · %s\n\n", e.BookTitle())
	fmt.Fprintf(&b, "%s is a Git-backed living book where humans read on the web and AI agents perform all writing (current HEAD: **r%d · %s**).\n\n", e.BookTitle(),
		snap.HeadRevisionNumber, snap.HeadCommitSHA)

	b.WriteString("## 1. Zero-HTML HTTP Raw Markdown & Content Negotiation (Read-Only Agents)\n\n")
	b.WriteString("Any AI web-fetching agent can read individual chapters, pinned permalinks, or the entire book without parsing HTML:\n\n")
	fmt.Fprintf(&b, "- **Book Discovery Index:** %s/llms.txt (or GET %s/ with header Accept: text/markdown)\n", base, base)
	fmt.Fprintf(&b, "- **Full Book in One Request:** %s/llms-full.txt or %s/book.md (historical snapshot: %s/rev/r1/book.md)\n", base, base, base)
	fmt.Fprintf(&b, "- **Live Chapter Raw Markdown:** %s/chapters/{slug}.md (or GET %s/chapters/{slug} with Accept: text/markdown)\n", base, base)
	fmt.Fprintf(&b, "- **Pinned Permalink Raw Markdown:** %s/rev/{rev}/chapters/{slug}.md or %s/chapters/{slug}/rev/{rev}.md (append ?raw=exact to omit lineage banner)\n", base, base)
	fmt.Fprintf(&b, "- **Meta & Audit Documents:** %s/timeline.md · %s/SUMMARY.md · %s/STYLE_GUIDE.md · %s/book.json · %s/changelog.md\n", base, base, base, base, base)
	fmt.Fprintf(&b, "- **Mermaid Diagram Source:** %s/chapters/{slug}/diagrams/{n}.mmd\n\n", base)

	b.WriteString("## 2. Remote Model Context Protocol (Authenticated Read/Write Co-Authoring)\n\n")
	b.WriteString("Connect Claude, ChatGPT, Gemini, Cursor, or any MCP-compatible client using your personal secret URL:\n\n")
	fmt.Fprintf(&b, "- **Remote MCP Endpoint:** %s/u/<email>:<secret-key>/mcp\n", base)
	fmt.Fprintf(&b, "- **OpenAPI 3.1 Specification:** %s/openapi.json (HTML reference: %s/api-docs)\n\n", base, base)

	b.WriteString("## 3. All 13 MCP Authoring & Audit Tools\n\n")
	b.WriteString("| Tool Name | Capability |\n")
	b.WriteString("| :--- | :--- |\n")
	b.WriteString("| **get_table_of_contents** | Returns SUMMARY.md, chapter word counts, last author, and live + permalink URLs. |\n")
	b.WriteString("| **read_chapter** | Reads full raw Markdown or JSON, revision SHA/number (r1..rN), and lineage status of any chapter, SUMMARY, STYLE_GUIDE, or BOOK_CONFIG. |\n")
	b.WriteString("| **search_book** | Searches chapters via SQLite FTS5 BM25 ranking plus 1-based exact line matching. |\n")
	b.WriteString("| **patch_chapter** | Applies surgical search-and-replace edits (or multi-hunk patches) to chapters, SUMMARY, STYLE_GUIDE, or BOOK_CONFIG, auto-syncs H1 titles to SUMMARY.md, and commits to Git. |\n")
	b.WriteString("| **create_chapter** | Creates a new chapter file, registers it in SUMMARY.md, commits to Git, and indexes in FTS5. |\n")
	b.WriteString("| **rename_chapter** | Renames or moves a chapter slug and/or H1 title and updates SUMMARY.md and cross-chapter links. |\n")
	b.WriteString("| **delete_chapter** | Removes a chapter from SUMMARY.md while preserving historical revision permalinks and optional replacement link. |\n")
	b.WriteString("| **list_recent_changes** | Lists chronological Git commits across the book or filtered to a single chapter. |\n")
	b.WriteString("| **get_commit_diff** | Returns the unified Markdown diff for any commit SHA or revision range. |\n")
	b.WriteString("| **get_chapter_blame** | Computes section-by-section Git blame attribution for a chapter. |\n")
	b.WriteString("| **revert_chapter_revision** | Restores a chapter to a prior commit SHA or revision number via a forward Git commit. |\n")
	b.WriteString("| **list_agent_activity** | Lists recent MCP and ConnectRPC tool calls from the SQLite audit log. |\n")
	b.WriteString("| **report_platform_feedback** | Submits a platform bug report, UX friction note, or feature request directly to the maintainer with recent tool context. |\n\n")

	b.WriteString("## 4. Reporting Platform Bugs & Requesting Features\n\n")
	fmt.Fprintf(&b, "Use **report_platform_feedback** strictly for software bugs, UX friction, or feature requests concerning the %s server, Web Studio, or MCP tools (editorial chapter comments belong inline in Markdown via **patch_chapter**):\n\n", e.BookTitle())
	b.WriteString("- **Report a Bug:** \"Report a platform bug: [what happened]\"\n")
	b.WriteString("- **Request a Feature:** \"Request a platform feature: [what would make editing easier]\"\n")
	b.WriteString("- **Schema Refresh Note:** Open a fresh chat session once after server tool updates so your AI client refreshes its 13-tool MCP schema.\n")

	md := b.String()
	return &MarkdownExportResult{
		Markdown:           md,
		RevisionSHA:        snap.HeadCommitSHA,
		RevisionNumber:     snap.HeadRevisionNumber,
		HeadSHA:            snap.HeadCommitSHA,
		HeadRevisionNumber: snap.HeadRevisionNumber,
		IsLatest:           true,
		WordCount:          int32(len(strings.Fields(md))),
	}, nil
}

// FormatPermalinkLineageMarkdownBanner produces a concise, deterministic Markdown callout
// prepended to pinned permalink or non-current (outdated, renamed, moved, deleted) raw Markdown responses.
func FormatPermalinkLineageMarkdownBanner(baseURL string, ch *ReadChapterOutput) string {
	if ch == nil {
		return ""
	}
	base := strings.TrimRight(strings.TrimSpace(baseURL), "/")
	switch ch.LifecycleStatus {
	case "outdated":
		return fmt.Sprintf(
			"> [!WARNING]\n> **Permalink Revision Notice (outdated):** Viewing historical snapshot **r%d (%s)** of **%s** (%d commit(s) behind latest chapter revision **r%d (%s)**).\n> **Latest Live Raw Markdown:** %s/chapters/%s.md (pass `?raw=exact` on permalink for unannotated historical bytes)\n\n",
			ch.RevisionNumber, ch.RevisionSHA, ch.Slug, ch.CommitsBehind, ch.LatestRevisionNumber, ch.LatestRevisionSHA, base, ch.Slug,
		)
	case "renamed":
		return fmt.Sprintf(
			"> [!WARNING]\n> **Permalink Revision Notice (renamed):** Viewing historical snapshot **r%d (%s)** of **%s**. Chapter title updated at commit %s to **“%s”**.\n> **Latest Live Raw Markdown:** %s/chapters/%s.md\n\n",
			ch.RevisionNumber, ch.RevisionSHA, ch.Slug, ch.LifecycleCommitSHA, ch.MovedToTitle, base, ch.Slug,
		)
	case "moved":
		targetSlug := ch.MovedToSlug
		if targetSlug == "" {
			targetSlug = ch.Slug
		}
		return fmt.Sprintf(
			"> [!WARNING]\n> **Permalink Revision Notice (moved):** Chapter slug **%s** (snapshot **r%d (%s)**) moved to **%s** at commit %s.\n> **Latest Live Raw Markdown:** %s/chapters/%s.md\n\n",
			ch.Slug, ch.RevisionNumber, ch.RevisionSHA, targetSlug, ch.LifecycleCommitSHA, base, targetSlug,
		)
	case "deleted":
		if ch.MovedToSlug != "" {
			return fmt.Sprintf(
				"> [!CAUTION]\n> **Deleted Chapter Archive (deleted):** Chapter **%s** (final snapshot **r%d (%s)**) was removed from the active book at commit %s by %s (%s).\n> **Replacement Chapter Raw Markdown:** %s/chapters/%s.md\n\n",
				ch.Slug, ch.RevisionNumber, ch.RevisionSHA, ch.LifecycleCommitSHA, ch.LifecycleAuthor, ch.LifecycleMessage, base, ch.MovedToSlug,
			)
		}
		return fmt.Sprintf(
			"> [!CAUTION]\n> **Deleted Chapter Archive (deleted):** Chapter **%s** (final snapshot **r%d (%s)**) was removed from the active book at commit %s by %s (%s).\n> **Active Book Discovery Index:** %s/llms.txt\n\n",
			ch.Slug, ch.RevisionNumber, ch.RevisionSHA, ch.LifecycleCommitSHA, ch.LifecycleAuthor, ch.LifecycleMessage, base,
		)
	default:
		return fmt.Sprintf(
			"> [!NOTE]\n> **Pinned Revision Permalink (current):** Viewing immutable snapshot **r%d (%s)** of **%s** · Matches current HEAD.\n> **Canonical Live Raw Markdown:** %s/chapters/%s.md\n\n",
			ch.RevisionNumber, ch.RevisionSHA, ch.Slug, base, ch.Slug,
		)
	}
}
