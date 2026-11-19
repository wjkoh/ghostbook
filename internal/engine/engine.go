package engine

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"unicode/utf8"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/wjkoh/ghostbook/internal/renderer"
	"github.com/wjkoh/ghostbook/internal/store"
)

var (
	ErrInvalidSlug      = errors.New("invalid chapter slug: only lowercase alphanumeric and hyphens allowed")
	ErrInvalidArgument  = errors.New("invalid argument")
	ErrNotFound         = errors.New("chapter or revision not found")
	ErrPatchNotFound    = errors.New("patch conflict: old_text not found in chapter (re-read chapter to get latest text)")
	ErrPatchAmbiguous   = errors.New("patch ambiguous: old_text appears multiple times in chapter (include more surrounding context lines)")
	ErrRevisionConflict = errors.New("optimistic lock conflict: expected_revision_sha does not match current chapter revision")
)

const maxChapterSlugLength = 120

var validSlugRe = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*$`)

func ValidateSlug(raw string) (string, error) {
	s := strings.TrimSpace(raw)
	if strings.Contains(s, "..") || strings.Contains(s, "/") || strings.Contains(s, `\`) {
		return "", fmt.Errorf("%w: %q", ErrInvalidSlug, raw)
	}
	s = strings.TrimSuffix(s, ".md")
	if s == "" || len(s) > maxChapterSlugLength || !validSlugRe.MatchString(s) {
		return "", fmt.Errorf("%w: %q", ErrInvalidSlug, raw)
	}
	switch s {
	case "timeline", "changelog", "guide", "print", "search", "activity", "api-docs", "docs", "healthz",
		"summary", "style-guide", "book", "index", "llms", "llms-full", "mcp", "rev", "chapters", "static", "u", "robots", "openapi", "debug", "feed", "rss":
		return "", fmt.Errorf("%w: %q is a reserved studio route", ErrInvalidSlug, raw)
	}
	return s, nil
}

// NormalizeChapterOrMetaSlug validates a regular chapter slug or the special meta-files
// SUMMARY and STYLE_GUIDE (case-insensitive).
func NormalizeChapterOrMetaSlug(raw string) (string, bool, error) {
	trimmed := strings.TrimSuffix(strings.TrimSpace(raw), ".md")
	if strings.EqualFold(trimmed, "summary") {
		return "SUMMARY", true, nil
	}
	if strings.EqualFold(trimmed, "style_guide") || strings.EqualFold(trimmed, "style-guide") {
		return "STYLE_GUIDE", true, nil
	}
	slug, err := ValidateSlug(raw)
	return slug, false, err
}

func truncateUTF8(s string, maxRunes int) string {
	if maxRunes <= 0 || utf8.RuneCountInString(s) <= maxRunes {
		return s
	}
	runes := []rune(s)
	return string(runes[:maxRunes]) + "…"
}

// EngineConfig holds optional runtime branding and editorial customization for a Ghostbook instance.
type EngineConfig struct {
	BookTitle         string
	BookKicker        string
	MCPName           string
	ExtraLLMsMarkdown string
	BannedWords       []string
}

type BookEngine struct {
	repoDir          string
	baseURL          string
	cfg              EngineConfig
	repo             *git.Repository
	store            *store.Store
	alerter          atomic.Pointer[store.AlertNotifier]
	briefSynth       atomic.Pointer[BriefSynthesizer]
	mu               sync.RWMutex
	syncMu           sync.Mutex
	briefMu          sync.Mutex
	snapshot         atomic.Pointer[renderer.RenderedBook]
	commitFilesCache map[plumbing.Hash][]string
	syncCh           chan struct{}
	briefCh          chan struct{}
}

func sanitizeMCPName(raw string) string {
	trimmed := strings.ToLower(strings.TrimSpace(raw))
	if trimmed == "" {
		return "ghostbook"
	}
	var b strings.Builder
	for _, r := range trimmed {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-' || r == '_' {
			b.WriteRune(r)
		} else if r == ' ' {
			b.WriteRune('-')
		}
	}
	out := strings.Trim(b.String(), "-_")
	if out == "" {
		return "ghostbook"
	}
	return out
}

func NewBookEngine(repoDir string, st *store.Store, baseURL string, remoteURLOpt ...string) (*BookEngine, error) {
	return NewBookEngineWithConfig(repoDir, st, baseURL, EngineConfig{}, remoteURLOpt...)
}

func NewBookEngineWithConfig(repoDir string, st *store.Store, baseURL string, cfg EngineConfig, remoteURLOpt ...string) (*BookEngine, error) {
	if st == nil {
		return nil, fmt.Errorf("%w: nil store", ErrInvalidArgument)
	}
	baseURL = strings.TrimRight(strings.TrimSpace(baseURL), "/")
	if baseURL == "" {
		baseURL = "http://127.0.0.1:8090"
	}
	if strings.TrimSpace(cfg.BookTitle) == "" {
		cfg.BookTitle = "Ghostbook"
	}
	if strings.TrimSpace(cfg.BookKicker) == "" {
		cfg.BookKicker = "Living Book"
	}
	cfg.MCPName = sanitizeMCPName(cfg.MCPName)
	cfg.BannedWords = slices.Clone(cfg.BannedWords)
	explicitRemote := ""
	if len(remoteURLOpt) > 0 {
		explicitRemote = strings.TrimSpace(remoteURLOpt[0])
	}
	repo, err := initOrOpenGitRepo(repoDir, explicitRemote)
	if err != nil {
		return nil, err
	}

	eng := &BookEngine{
		repoDir:          repoDir,
		baseURL:          baseURL,
		cfg:              cfg,
		repo:             repo,
		store:            st,
		commitFilesCache: make(map[plumbing.Hash][]string),
		syncCh:           make(chan struct{}, 1),
		briefCh:          make(chan struct{}, 1),
	}

	eng.mu.Lock()
	err = eng.rebuildSnapshotAndFTSLocked(context.Background())
	eng.mu.Unlock()
	if err != nil {
		return nil, err
	}
	return eng, nil
}

// readSrcFileSafe reads a regular file strictly inside <repoDir>/src using os.OpenRoot
// and rejects symlinks to prevent arbitrary host file reads from malicious Git commits.
func (e *BookEngine) readSrcFileSafe(name string) ([]byte, error) {
	srcDir := filepath.Join(e.repoDir, "src")
	root, err := os.OpenRoot(srcDir)
	if err != nil {
		return nil, err
	}
	defer func() { _ = root.Close() }()
	fi, err := root.Lstat(name)
	if err != nil {
		return nil, err
	}
	if fi.Mode()&os.ModeSymlink != 0 || !fi.Mode().IsRegular() {
		return nil, fmt.Errorf("%w: %s is not a regular file", ErrInvalidArgument, name)
	}
	return root.ReadFile(name)
}

// writeSrcFileSafe writes a regular file strictly inside <repoDir>/src using os.OpenRoot,
// rejecting symlinks so writes cannot escape the repository directory.
func (e *BookEngine) writeSrcFileSafe(name string, data []byte) error {
	srcDir := filepath.Join(e.repoDir, "src")
	root, err := os.OpenRoot(srcDir)
	if err != nil {
		return err
	}
	defer func() { _ = root.Close() }()
	if fi, err := root.Lstat(name); err == nil {
		if fi.Mode()&os.ModeSymlink != 0 || !fi.Mode().IsRegular() {
			return fmt.Errorf("%w: %s is not a regular file", ErrInvalidArgument, name)
		}
	}
	return root.WriteFile(name, data, 0o644)
}

func (e *BookEngine) BookTitle() string {
	return e.cfg.BookTitle
}

func (e *BookEngine) BookKicker() string {
	return e.cfg.BookKicker
}

func (e *BookEngine) MCPName() string {
	return e.cfg.MCPName
}

func (e *BookEngine) ExtraLLMsMarkdown() string {
	return strings.TrimSpace(e.cfg.ExtraLLMsMarkdown)
}

func (e *BookEngine) BannedWords() []string {
	return slices.Clone(e.cfg.BannedWords)
}

func (e *BookEngine) Store() *store.Store {
	return e.store
}

func (e *BookEngine) SetAlertNotifier(a *store.AlertNotifier) {
	e.alerter.Store(a)
}

func (e *BookEngine) AlertNotifier() *store.AlertNotifier {
	return e.alerter.Load()
}

func (e *BookEngine) NotifyAlert(ctx context.Context, category, summary, details string) {
	if a := e.AlertNotifier(); a != nil {
		a.NotifyError(ctx, category, summary, details)
	}
}

func (e *BookEngine) BaseURL() string {
	return e.baseURL
}

func (e *BookEngine) CurrentSnapshot() *renderer.RenderedBook {
	return e.snapshot.Load()
}

func (e *BookEngine) StyleGuide() string {
	e.mu.RLock()
	diskBytes, err := e.readSrcFileSafe("STYLE_GUIDE.md")
	e.mu.RUnlock()
	if err == nil && len(diskBytes) > 0 {
		return strings.TrimSpace(string(diskBytes))
	}
	return strings.TrimSpace(defaultStyleGuideMarkdown)
}

func (e *BookEngine) resolveEffectiveAuthor(ctx context.Context, override string) *store.Author {
	// Authenticated caller identity always takes precedence over any untrusted author_name parameter
	// so one co-author cannot spoof commits under another co-author's name.
	if a := AuthorFromContext(ctx); a != nil {
		return a
	}
	if strings.TrimSpace(override) != "" {
		if a, err := e.store.ResolveAuthorName(ctx, override); err == nil {
			return a
		}
	}
	if authors, err := e.store.ListAuthors(ctx); err == nil && len(authors) > 0 {
		return &authors[0]
	}
	return &store.Author{
		ID:          "ghostbook",
		DisplayName: "Ghostbook Agent",
		GitEmail:    "agent@ghostbook.local",
		RoleBadge:   "Author",
	}
}

func (e *BookEngine) GetTableOfContents(_ context.Context, _ *GetTableOfContentsInput) (*GetTableOfContentsOutput, error) {
	snap := e.CurrentSnapshot()
	if snap == nil {
		return nil, fmt.Errorf("%w: snapshot not initialized", ErrNotFound)
	}

	items := make([]ChapterSummaryItem, 0, len(snap.Chapters))
	for _, ch := range snap.Chapters {
		items = append(items, ChapterSummaryItem{
			Slug:             ch.Slug,
			Title:            ch.Title,
			WordCount:        ch.WordCount,
			OpenCommentCount: ch.OpenCommentCount,
			LastCommitSHA:    ch.LastCommitSHA,
			RevisionNumber:   ch.RevisionNumber,
			LastAuthor:       ch.LastAuthor,
			LastUpdatedISO:   ch.LastUpdatedISO,
			LiveURL:          fmt.Sprintf("%s/chapters/%s", e.baseURL, ch.Slug),
			PermalinkURL:     ch.PermalinkURL,
			PartTitle:        ch.PartTitle,
			Depth:            ch.Depth,
			ParentSlug:       ch.ParentSlug,
			NumberLabel:      ch.NumberLabel,
		})
	}

	return &GetTableOfContentsOutput{
		HeadCommitSHA:      snap.HeadCommitSHA,
		HeadRevisionNumber: snap.HeadRevisionNumber,
		SummaryMarkdown:    snap.SummaryMarkdown,
		Chapters:           items,
		BookHomeURL:        e.baseURL + "/",
		BookPDFURL:         e.baseURL + "/print",
	}, nil
}

func (e *BookEngine) ReadChapter(_ context.Context, in *ReadChapterInput) (*ReadChapterOutput, error) {
	if in == nil {
		return nil, fmt.Errorf("%w: nil input", ErrInvalidArgument)
	}
	slug, isMeta, err := NormalizeChapterOrMetaSlug(in.Slug)
	if err != nil {
		return nil, err
	}
	revSpec := strings.TrimSpace(in.Revision)

	snap := e.CurrentSnapshot()
	if !isMeta && revSpec == "" && snap != nil {
		if ch, ok := snap.BySlug[slug]; ok {
			liveURL := fmt.Sprintf("%s/chapters/%s", e.baseURL, ch.Slug)
			return &ReadChapterOutput{
				Slug:                 ch.Slug,
				Title:                ch.Title,
				ContentMarkdown:      ch.Markdown,
				RevisionSHA:          ch.LastCommitSHA,
				RevisionNumber:       ch.RevisionNumber,
				LastAuthor:           ch.LastAuthor,
				LastUpdatedISO:       ch.LastUpdatedISO,
				LiveURL:              liveURL,
				PermalinkURL:         ch.PermalinkURL,
				LifecycleStatus:      "current",
				IsLatest:             true,
				CommitsBehind:        0,
				LatestRevisionSHA:    ch.LastCommitSHA,
				LatestRevisionNumber: ch.RevisionNumber,
				LatestLiveURL:        liveURL,
			}, nil
		}
	}

	e.mu.RLock()
	out, err := e.resolveChapterPermalinkLocked(slug, revSpec)
	e.mu.RUnlock()
	if err != nil {
		if slug == "STYLE_GUIDE" && revSpec == "" && errors.Is(err, ErrNotFound) && snap != nil {
			md := e.StyleGuide()
			liveURL := fmt.Sprintf("%s/STYLE_GUIDE.md", e.baseURL)
			return &ReadChapterOutput{
				Slug:                 "STYLE_GUIDE",
				Title:                extractMarkdownH1(md),
				ContentMarkdown:      md + "\n",
				RevisionSHA:          snap.HeadCommitSHA,
				RevisionNumber:       snap.HeadRevisionNumber,
				LiveURL:              liveURL,
				PermalinkURL:         fmt.Sprintf("%s/rev/%s/STYLE_GUIDE.md", e.baseURL, snap.HeadCommitSHA),
				LifecycleStatus:      "current",
				IsLatest:             true,
				LatestRevisionSHA:    snap.HeadCommitSHA,
				LatestRevisionNumber: snap.HeadRevisionNumber,
				LatestLiveURL:        liveURL,
			}, nil
		}
		return nil, err
	}
	return out, nil
}

// RenderChapterAtRevision renders a chapter at a specific revision (or archived/moved/deleted slug)
// into a complete ChapterView with permalink & lineage banner metadata and diff-to-HEAD.
func (e *BookEngine) RenderChapterAtRevision(ctx context.Context, slug, revSpec string, isPermalinkURL bool) (*renderer.ChapterView, error) {
	cleanSlug, _, err := NormalizeChapterOrMetaSlug(slug)
	if err != nil {
		return nil, err
	}
	snap := e.CurrentSnapshot()
	if !isPermalinkURL && strings.TrimSpace(revSpec) == "" && snap != nil {
		if ch, ok := snap.BySlug[cleanSlug]; ok {
			return ch, nil
		}
	}

	readOut, err := e.ReadChapter(ctx, &ReadChapterInput{
		Slug:     cleanSlug,
		Revision: revSpec,
	})
	if err != nil {
		return nil, err
	}

	renderedHTML, title, _, wordCount, err := renderer.RenderMarkdownToHTML(readOut.ContentMarkdown)
	if err != nil {
		return nil, err
	}
	if title == "" {
		title = readOut.Title
	}

	var diffToLatest string
	if !readOut.IsLatest && (readOut.LifecycleStatus == "outdated" || readOut.LifecycleStatus == "renamed") {
		e.mu.RLock()
		if diffOut, diffErr := e.computeCommitDiffLocked("HEAD", cleanSlug, readOut.RevisionSHA); diffErr == nil {
			diffToLatest = diffOut.UnifiedDiff
		}
		e.mu.RUnlock()
	}

	cv := &renderer.ChapterView{
		Slug:                  readOut.Slug,
		Title:                 title,
		Markdown:              readOut.ContentMarkdown,
		RenderedHTML:          renderedHTML,
		WordCount:             wordCount,
		OpenCommentCount:      countChapterInlineCallouts(readOut.ContentMarkdown),
		LastCommitSHA:         readOut.RevisionSHA,
		RevisionNumber:        readOut.RevisionNumber,
		LastAuthor:            readOut.LastAuthor,
		LastUpdatedISO:        readOut.LastUpdatedISO,
		IsPermalinkView:       isPermalinkURL || !readOut.IsLatest,
		PermalinkURL:          readOut.PermalinkURL,
		LifecycleStatus:       readOut.LifecycleStatus,
		IsLatest:              readOut.IsLatest,
		CommitsBehind:         readOut.CommitsBehind,
		LatestRevisionSHA:     readOut.LatestRevisionSHA,
		LatestRevisionNumber:  readOut.LatestRevisionNumber,
		LatestLiveURL:         readOut.LatestLiveURL,
		MovedToSlug:           readOut.MovedToSlug,
		MovedToTitle:          readOut.MovedToTitle,
		LifecycleCommitSHA:    readOut.LifecycleCommitSHA,
		LifecycleAuthor:       readOut.LifecycleAuthor,
		LifecycleTimestampISO: readOut.LifecycleTimestampISO,
		LifecycleMessage:      readOut.LifecycleMessage,
		DiffToLatest:          diffToLatest,
	}
	if snap != nil {
		if existing, ok := snap.BySlug[cleanSlug]; ok {
			cv.NumberLabel = existing.NumberLabel
			cv.PartTitle = existing.PartTitle
			cv.StartsNewPart = existing.StartsNewPart
			cv.Depth = existing.Depth
			cv.ParentSlug = existing.ParentSlug
			cv.PrevSlug = existing.PrevSlug
			cv.PrevTitle = existing.PrevTitle
			cv.NextSlug = existing.NextSlug
			cv.NextTitle = existing.NextTitle
		}
	}
	return cv, nil
}

func findChapterLineMatches(markdown, rawQuery string, maxLines int) []SearchLineMatch {
	q := strings.TrimSpace(rawQuery)
	if q == "" || markdown == "" {
		return nil
	}
	if maxLines <= 0 {
		maxLines = 20
	}
	lowerQ := strings.ToLower(q)
	tokens := strings.Fields(lowerQ)

	lines := strings.Split(markdown, "\n")
	var out []SearchLineMatch
	for idx, line := range lines {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			continue
		}
		lowerLine := strings.ToLower(line)
		matched := strings.Contains(lowerLine, lowerQ)
		if !matched && len(tokens) > 1 {
			allTokens := true
			testedTokens := 0
			for _, tok := range tokens {
				cleanTok := strings.Trim(tok, `"'.,;!?()[]{}`)
				if len(cleanTok) < 2 {
					continue
				}
				testedTokens++
				if !strings.Contains(lowerLine, cleanTok) {
					allTokens = false
					break
				}
			}
			matched = testedTokens > 0 && allTokens
		}
		if matched {
			out = append(out, SearchLineMatch{
				LineNumber: int32(idx + 1),
				LineText:   truncateUTF8(trimmed, 240),
			})
			if len(out) >= maxLines {
				break
			}
		}
	}
	return out
}

func (e *BookEngine) SearchBook(ctx context.Context, in *SearchBookInput) (*SearchBookOutput, error) {
	if in == nil || strings.TrimSpace(in.Query) == "" {
		return &SearchBookOutput{}, nil
	}
	filterSlug := strings.TrimSuffix(strings.TrimSpace(in.Slug), ".md")
	limit := int(in.Limit)
	if limit <= 0 {
		limit = 10
	}
	if limit > 100 {
		limit = 100
	}

	snap := e.CurrentSnapshot()
	hits, err := e.store.SearchFTS(ctx, in.Query, limit*2, filterSlug)
	if err != nil {
		return nil, err
	}

	seen := make(map[string]bool, len(hits))
	matches := make([]SearchMatchItem, 0, len(hits))
	for _, h := range hits {
		if filterSlug != "" && h.Slug != filterSlug {
			continue
		}
		seen[h.Slug] = true
		var lineMatches []SearchLineMatch
		if snap != nil {
			if ch, ok := snap.BySlug[h.Slug]; ok {
				lineMatches = findChapterLineMatches(ch.Markdown, in.Query, 20)
			}
		}
		matches = append(matches, SearchMatchItem{
			Slug:        h.Slug,
			Title:       h.Title,
			Snippet:     h.Snippet,
			BM25Score:   h.BM25Score,
			LiveURL:     fmt.Sprintf("%s/chapters/%s", e.baseURL, h.Slug),
			LineMatches: lineMatches,
		})
		if len(matches) >= limit {
			return &SearchBookOutput{Matches: matches}, nil
		}
	}

	// Fallback / supplement: scan chapter Markdown directly so punctuation-heavy queries
	// (e.g. "p99 < 2ms", "r12", "[TODO") or single-chapter finds always return exact line matches.
	if snap != nil && len(matches) < limit {
		for _, ch := range snap.Chapters {
			if seen[ch.Slug] {
				continue
			}
			if filterSlug != "" && ch.Slug != filterSlug {
				continue
			}
			lineMatches := findChapterLineMatches(ch.Markdown, in.Query, 20)
			if len(lineMatches) == 0 {
				continue
			}
			snippet := lineMatches[0].LineText
			matches = append(matches, SearchMatchItem{
				Slug:        ch.Slug,
				Title:       ch.Title,
				Snippet:     snippet,
				BM25Score:   1.0,
				LiveURL:     fmt.Sprintf("%s/chapters/%s", e.baseURL, ch.Slug),
				LineMatches: lineMatches,
			})
			if len(matches) >= limit {
				break
			}
		}
	}

	return &SearchBookOutput{Matches: matches}, nil
}

func (e *BookEngine) ListRecentChanges(_ context.Context, in *ListRecentChangesInput) (*ListRecentChangesOutput, error) {
	if in == nil {
		in = &ListRecentChangesInput{}
	}
	e.mu.RLock()
	defer e.mu.RUnlock()

	commits, err := e.listCommitsLocked(in.Slug, int(in.Limit))
	if err != nil {
		return nil, err
	}
	return &ListRecentChangesOutput{Commits: commits}, nil
}

func (e *BookEngine) GetCommitDiff(_ context.Context, in *GetCommitDiffInput) (*GetCommitDiffOutput, error) {
	if in == nil {
		return nil, fmt.Errorf("%w: nil input", ErrInvalidArgument)
	}
	e.mu.RLock()
	defer e.mu.RUnlock()

	return e.computeCommitDiffLocked(in.CommitSHA, in.Slug, in.BaseCommitSHA)
}

func (e *BookEngine) GetChapterBlame(_ context.Context, in *GetChapterBlameInput) (*GetChapterBlameOutput, error) {
	if in == nil {
		return nil, fmt.Errorf("%w: nil input", ErrInvalidArgument)
	}
	e.mu.RLock()
	defer e.mu.RUnlock()

	return e.computeChapterBlameLocked(in.Slug)
}

func (e *BookEngine) ListAgentActivity(ctx context.Context, in *ListAgentActivityInput) (*ListAgentActivityOutput, error) {
	if in == nil {
		in = &ListAgentActivityInput{}
	}
	entries, err := e.store.ListActivity(ctx, strings.TrimSpace(in.AuthorID), int(in.Limit))
	if err != nil {
		return nil, err
	}
	return &ListAgentActivityOutput{Entries: entries}, nil
}
