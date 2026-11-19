package engine

import (
	"context"

	"github.com/wjkoh/ghostbook/internal/store"
)

type authorCtxKey struct{}
type userAgentCtxKey struct{}

func WithAuthor(ctx context.Context, a *store.Author) context.Context {
	return context.WithValue(ctx, authorCtxKey{}, a)
}

func AuthorFromContext(ctx context.Context) *store.Author {
	if a, ok := ctx.Value(authorCtxKey{}).(*store.Author); ok && a != nil {
		return a
	}
	return nil
}

func WithUserAgent(ctx context.Context, ua string) context.Context {
	return context.WithValue(ctx, userAgentCtxKey{}, ua)
}

func UserAgentFromContext(ctx context.Context) string {
	if v, ok := ctx.Value(userAgentCtxKey{}).(string); ok {
		return v
	}
	return ""
}

type GetTableOfContentsInput struct{}

type ChapterSummaryItem struct {
	Slug             string `json:"slug" jsonschema:"Chapter filename slug without .md extension"`
	Title            string `json:"title" jsonschema:"Full chapter heading title"`
	WordCount        int32  `json:"word_count" jsonschema:"Total word count in the chapter"`
	OpenCommentCount int32  `json:"open_comment_count" jsonschema:"Number of unresolved editorial comments on this chapter"`
	LastCommitSHA    string `json:"last_commit_sha" jsonschema:"Short 7-character Git commit SHA that last modified this chapter"`
	LastAuthor       string `json:"last_author" jsonschema:"Display name of the co-author who last edited this chapter"`
	LastUpdatedISO   string `json:"last_updated_iso" jsonschema:"ISO-8601 UTC timestamp of the last edit"`
	LiveURL          string `json:"live_url" jsonschema:"Direct browser URL to view the rendered chapter"`
	RevisionNumber   int32  `json:"revision_number" jsonschema:"1-based sequential repository revision number (e.g. 4 for r4)"`
	PermalinkURL     string `json:"permalink_url" jsonschema:"Permanent revision URL pinned to this chapter's current commit SHA"`
	PartTitle        string `json:"part_title,omitempty" jsonschema:"Part header title in SUMMARY.md under which this chapter is grouped (e.g. Part I — Strategic Thesis)"`
	Depth            int32  `json:"depth" jsonschema:"Hierarchy depth in SUMMARY.md (0 for top-level chapter, 1 for sub-chapter)"`
	ParentSlug       string `json:"parent_slug,omitempty" jsonschema:"Parent chapter slug if this is an indented sub-chapter"`
	NumberLabel      string `json:"number_label,omitempty" jsonschema:"Hierarchical chapter number (e.g. 1, 1.1, 1.2, 2)"`
}

type GetTableOfContentsOutput struct {
	HeadCommitSHA      string               `json:"head_commit_sha" jsonschema:"Current HEAD commit SHA of the book repository"`
	HeadRevisionNumber int32                `json:"head_revision_number" jsonschema:"Current 1-based sequential HEAD revision number"`
	SummaryMarkdown    string               `json:"summary_markdown" jsonschema:"Raw Markdown content of src/SUMMARY.md"`
	Chapters           []ChapterSummaryItem `json:"chapters" jsonschema:"Ordered list of chapters in the book"`
	BookHomeURL        string               `json:"book_home_url" jsonschema:"Root URL of the live rendered web book"`
	BookPDFURL         string               `json:"book_pdf_url" jsonschema:"URL of the US-Letter printable full-book view"`
}

type ReadChapterInput struct {
	Slug     string `json:"slug" jsonschema:"Chapter slug to read, such as 00-preface or 01-mfo-stack-teardown"`
	Revision string `json:"revision,omitempty" jsonschema:"Optional revision SHA (e.g. 6ca162a) or sequential revision number (e.g. r1, r2) to read a historical permalink snapshot"`
}

type ReadChapterOutput struct {
	Slug                  string `json:"slug" jsonschema:"Canonical chapter slug"`
	Title                 string `json:"title" jsonschema:"Chapter title at the requested revision"`
	ContentMarkdown       string `json:"content_markdown" jsonschema:"Full Markdown prose of the chapter at the requested revision"`
	RevisionSHA           string `json:"revision_sha" jsonschema:"Git revision SHA of the chapter"`
	RevisionNumber        int32  `json:"revision_number" jsonschema:"1-based sequential revision number (r1, r2, ...)"`
	LastAuthor            string `json:"last_author" jsonschema:"Author who modified this revision"`
	LastUpdatedISO        string `json:"last_updated_iso" jsonschema:"ISO-8601 UTC timestamp of this revision"`
	LiveURL               string `json:"live_url" jsonschema:"Direct URL to the chapter"`
	PermalinkURL          string `json:"permalink_url" jsonschema:"Immutable permalink URL for this specific chapter revision"`
	LifecycleStatus       string `json:"lifecycle_status" jsonschema:"Lifecycle state relative to HEAD: current, outdated, renamed, moved, or deleted"`
	IsLatest              bool   `json:"is_latest" jsonschema:"True if this revision matches the latest HEAD version of the chapter"`
	CommitsBehind         int32  `json:"commits_behind" jsonschema:"Number of newer commits to this chapter since the requested revision"`
	LatestRevisionSHA     string `json:"latest_revision_sha" jsonschema:"Latest commit SHA of this chapter at HEAD"`
	LatestRevisionNumber  int32  `json:"latest_revision_number" jsonschema:"Latest 1-based revision number at HEAD"`
	LatestLiveURL         string `json:"latest_live_url" jsonschema:"Canonical live URL to the latest version (or moved destination) of this chapter"`
	MovedToSlug           string `json:"moved_to_slug,omitempty" jsonschema:"If moved, renamed, or replaced, the destination chapter slug"`
	MovedToTitle          string `json:"moved_to_title,omitempty" jsonschema:"If moved or renamed, the new chapter title at HEAD"`
	LifecycleCommitSHA    string `json:"lifecycle_commit_sha,omitempty" jsonschema:"Commit SHA where the chapter was updated, renamed, moved, or deleted"`
	LifecycleAuthor       string `json:"lifecycle_author,omitempty" jsonschema:"Author who performed the latest update, rename, move, or deletion"`
	LifecycleTimestampISO string `json:"lifecycle_timestamp_iso,omitempty" jsonschema:"ISO-8601 UTC timestamp of the lifecycle event"`
	LifecycleMessage      string `json:"lifecycle_message,omitempty" jsonschema:"Commit summary of the lifecycle event"`
}

type SearchBookInput struct {
	Query string `json:"query" jsonschema:"Search query keywords, exact phrase, or number/symbol (e.g. 'SQLite', '$2.8B', '[TODO') for full-text BM25 and line-level find"`
	Slug  string `json:"slug,omitempty" jsonschema:"Optional chapter slug to restrict search/find to a single chapter"`
	Limit int32  `json:"limit,omitempty" jsonschema:"Maximum number of chapter results to return (default 10)"`
}

type SearchLineMatch struct {
	LineNumber int32  `json:"line_number" jsonschema:"1-based line number in the chapter Markdown"`
	LineText   string `json:"line_text" jsonschema:"Full Markdown line containing the match"`
}

type SearchMatchItem struct {
	Slug        string            `json:"slug" jsonschema:"Matching chapter slug"`
	Title       string            `json:"title" jsonschema:"Matching chapter title"`
	Snippet     string            `json:"snippet" jsonschema:"Highlighted excerpt containing matching terms"`
	BM25Score   float64           `json:"bm25_score" jsonschema:"Positive BM25 relevance score"`
	LiveURL     string            `json:"live_url" jsonschema:"Direct URL to the matching chapter"`
	LineMatches []SearchLineMatch `json:"line_matches,omitempty" jsonschema:"Exact 1-based line numbers and Markdown lines matching the query within this chapter"`
}

type SearchBookOutput struct {
	Matches []SearchMatchItem `json:"matches" jsonschema:"Ranked FTS5 search matches"`
}

type ChapterPatchHunk struct {
	OldText string `json:"old_text" jsonschema:"Exact contiguous Markdown text in the chapter to replace; must match uniquely"`
	NewText string `json:"new_text" jsonschema:"Replacement Markdown text"`
}

type PatchChapterInput struct {
	Slug                string             `json:"slug" jsonschema:"Target chapter slug (e.g. 01-mfo-stack-teardown, SUMMARY, or STYLE_GUIDE)"`
	OldText             string             `json:"old_text,omitempty" jsonschema:"Exact contiguous Markdown text in the chapter to replace; must match uniquely (optional if patches array is used)"`
	NewText             string             `json:"new_text,omitempty" jsonschema:"Replacement Markdown text (optional if patches array is used)"`
	Patches             []ChapterPatchHunk `json:"patches,omitempty" jsonschema:"Optional array of surgical {old_text, new_text} hunks to apply atomically in a single Git commit"`
	CommitMessage       string             `json:"commit_message" jsonschema:"Concise editorial commit summary explaining the change"`
	ExpectedRevisionSHA string             `json:"expected_revision_sha,omitempty" jsonschema:"Optional revision SHA from ReadChapter to enforce optimistic concurrency"`
	AuthorName          string             `json:"author_name,omitempty" jsonschema:"Optional author override (email or name)"`
}

type PatchChapterOutput struct {
	Slug           string   `json:"slug" jsonschema:"Updated chapter slug"`
	NewRevisionSHA string   `json:"new_revision_sha" jsonschema:"New Git commit SHA created by this edit"`
	CommittedBy    string   `json:"committed_by" jsonschema:"Author identity recorded in Git"`
	DiffPreview    string   `json:"diff_preview" jsonschema:"Unified diff preview of the applied change"`
	LiveChapterURL string   `json:"live_chapter_url" jsonschema:"Direct URL to view the updated chapter"`
	StyleWarnings  []string `json:"style_warnings,omitempty" jsonschema:"Non-blocking editorial style warnings from STYLE_GUIDE.md"`
}

type CreateChapterInput struct {
	Slug            string `json:"slug" jsonschema:"New chapter slug (lowercase alphanumeric and hyphens, e.g. 05-execution-roadmap)"`
	Title           string `json:"title" jsonschema:"Chapter title to register in SUMMARY.md"`
	InitialMarkdown string `json:"initial_markdown" jsonschema:"Initial Markdown body of the chapter"`
	ParentSlug      string `json:"parent_slug,omitempty" jsonschema:"Optional parent chapter slug for indented sub-chapters"`
	PartTitle       string `json:"part_title,omitempty" jsonschema:"Optional Part header title in SUMMARY.md (e.g. Part I — Market & Portfolio) to group the chapter under"`
	CommitMessage   string `json:"commit_message,omitempty" jsonschema:"Optional commit summary"`
	AuthorName      string `json:"author_name,omitempty" jsonschema:"Optional author override"`
}

type CreateChapterOutput struct {
	Slug           string   `json:"slug" jsonschema:"Created chapter slug"`
	NewRevisionSHA string   `json:"new_revision_sha" jsonschema:"New Git commit SHA"`
	LiveChapterURL string   `json:"live_chapter_url" jsonschema:"Direct URL to the new chapter"`
	StyleWarnings  []string `json:"style_warnings,omitempty" jsonschema:"Non-blocking editorial style warnings from STYLE_GUIDE.md"`
}

type ListRecentChangesInput struct {
	Slug  string `json:"slug,omitempty" jsonschema:"Optional chapter slug filter"`
	Limit int32  `json:"limit,omitempty" jsonschema:"Maximum number of commits to return (default 20)"`
}

type CommitEntryItem struct {
	CommitSHA      string   `json:"commit_sha" jsonschema:"Short Git commit SHA"`
	FullSHA        string   `json:"full_sha" jsonschema:"Full 40-character Git commit SHA"`
	RevisionNumber int32    `json:"revision_number" jsonschema:"1-based sequential revision number (e.g. 1 for r1)"`
	AuthorName     string   `json:"author_name" jsonschema:"Commit author display name"`
	AuthorEmail    string   `json:"author_email" jsonschema:"Commit author email"`
	TimestampISO   string   `json:"timestamp_iso" jsonschema:"ISO-8601 UTC commit timestamp"`
	CommitMessage  string   `json:"commit_message" jsonschema:"Commit message"`
	FilesChanged   []string `json:"files_changed" jsonschema:"List of modified chapter files"`
}

type ListRecentChangesOutput struct {
	Commits []CommitEntryItem `json:"commits" jsonschema:"Chronological list of Git commits"`
}

type GetCommitDiffInput struct {
	CommitSHA     string `json:"commit_sha" jsonschema:"Commit SHA (short, full, or revision number like r2) to inspect"`
	Slug          string `json:"slug,omitempty" jsonschema:"Optional chapter slug filter"`
	BaseCommitSHA string `json:"base_commit_sha,omitempty" jsonschema:"Optional base commit SHA or revision number to diff against (defaults to parent of commit_sha)"`
}

type GetCommitDiffOutput struct {
	CommitSHA     string `json:"commit_sha" jsonschema:"Commit SHA"`
	AuthorName    string `json:"author_name" jsonschema:"Commit author name"`
	TimestampISO  string `json:"timestamp_iso" jsonschema:"ISO-8601 UTC timestamp"`
	CommitMessage string `json:"commit_message" jsonschema:"Commit message"`
	UnifiedDiff   string `json:"unified_diff" jsonschema:"Unified Markdown diff for the commit"`
}

type GetChapterBlameInput struct {
	Slug string `json:"slug" jsonschema:"Chapter slug to compute section-level Git blame for"`
}

type BlameSectionItem struct {
	Heading        string `json:"heading" jsonschema:"Markdown section heading"`
	LastCommitSHA  string `json:"last_commit_sha" jsonschema:"Short Git SHA that last modified this section"`
	LastAuthor     string `json:"last_author" jsonschema:"Author who last modified this section"`
	LastUpdatedISO string `json:"last_updated_iso" jsonschema:"ISO-8601 UTC timestamp of the last edit in this section"`
	StartLine      int32  `json:"start_line" jsonschema:"1-based starting line number"`
	EndLine        int32  `json:"end_line" jsonschema:"1-based ending line number"`
}

type GetChapterBlameOutput struct {
	Slug     string             `json:"slug" jsonschema:"Chapter slug"`
	Sections []BlameSectionItem `json:"sections" jsonschema:"Section-by-section blame attribution"`
}

type RevertChapterRevisionInput struct {
	Slug            string `json:"slug" jsonschema:"Chapter slug to restore"`
	TargetCommitSHA string `json:"target_commit_sha" jsonschema:"Historical commit SHA or revision number (e.g. r1) whose version of the chapter should be restored"`
	Reason          string `json:"reason,omitempty" jsonschema:"Editorial reason for reverting to the prior revision"`
	AuthorName      string `json:"author_name,omitempty" jsonschema:"Optional author override"`
}

type RevertChapterRevisionOutput struct {
	Slug           string `json:"slug" jsonschema:"Restored chapter slug"`
	NewRevisionSHA string `json:"new_revision_sha" jsonschema:"New forward Git commit SHA"`
	LiveChapterURL string `json:"live_chapter_url" jsonschema:"Direct URL to the restored chapter"`
}

type ListAgentActivityInput struct {
	Limit    int32  `json:"limit,omitempty" jsonschema:"Maximum number of audit log entries to return (default 30)"`
	AuthorID string `json:"author_id,omitempty" jsonschema:"Optional filter by author ID (e.g. alice, bob)"`
}

type ListAgentActivityOutput struct {
	Entries []store.AuditEntry `json:"entries" jsonschema:"Recent AI agent and studio tool invocations"`
}

type RenameChapterInput struct {
	OldSlug       string `json:"old_slug" jsonschema:"Existing chapter slug to rename or move"`
	NewSlug       string `json:"new_slug,omitempty" jsonschema:"New chapter slug (if moving/renaming the filename; defaults to old_slug if only renaming the title)"`
	NewTitle      string `json:"new_title,omitempty" jsonschema:"Optional new H1 chapter title to update in the chapter and SUMMARY.md"`
	CommitMessage string `json:"commit_message,omitempty" jsonschema:"Optional commit summary"`
	AuthorName    string `json:"author_name,omitempty" jsonschema:"Optional author override"`
}

type RenameChapterOutput struct {
	OldSlug        string `json:"old_slug" jsonschema:"Previous chapter slug"`
	NewSlug        string `json:"new_slug" jsonschema:"Updated chapter slug"`
	NewTitle       string `json:"new_title" jsonschema:"Updated chapter title"`
	NewRevisionSHA string `json:"new_revision_sha" jsonschema:"New Git commit SHA"`
	LiveChapterURL string `json:"live_chapter_url" jsonschema:"Canonical URL of the renamed/moved chapter"`
	PermalinkURL   string `json:"permalink_url" jsonschema:"Permalink URL pinned to the new revision SHA"`
}

type DeleteChapterInput struct {
	Slug           string `json:"slug" jsonschema:"Chapter slug to remove from the active book"`
	Reason         string `json:"reason,omitempty" jsonschema:"Editorial reason for deleting the chapter"`
	ReplacedBySlug string `json:"replaced_by_slug,omitempty" jsonschema:"Optional replacement chapter slug to display on archived permalinks"`
	AuthorName     string `json:"author_name,omitempty" jsonschema:"Optional author override"`
}

type DeleteChapterOutput struct {
	DeletedSlug          string `json:"deleted_slug" jsonschema:"Deleted chapter slug"`
	NewRevisionSHA       string `json:"new_revision_sha" jsonschema:"New Git commit SHA recording the deletion"`
	ArchivedPermalinkURL string `json:"archived_permalink_url" jsonschema:"Permalink URL where the archived chapter and deletion notice remain accessible"`
}

type ReportPlatformFeedbackInput struct {
	Kind        string `json:"kind,omitempty" jsonschema:"Feedback category: bug, feature_request, or ux_friction (defaults to feature_request)"`
	Summary     string `json:"summary" jsonschema:"Concise single-line summary of the bug, UX friction, or feature request (5 to 200 characters)"`
	Details     string `json:"details,omitempty" jsonschema:"Reproduction steps, desired workflow, error output, or rationale (up to 8,000 characters)"`
	ChapterSlug string `json:"chapter_slug,omitempty" jsonschema:"Optional chapter or meta slug (e.g. 05-ai-native-service, SUMMARY, STYLE_GUIDE) where the issue or workflow occurred"`
	RevisionSHA string `json:"revision_sha,omitempty" jsonschema:"Optional Git commit SHA or revision label (e.g. r179 or 15ed76b) related to the report"`
}

type ReportPlatformFeedbackOutput struct {
	FeedbackID    int64  `json:"feedback_id" jsonschema:"Recorded SQLite feedback ID"`
	RecordedAtISO string `json:"recorded_at_iso" jsonschema:"UTC timestamp when the feedback was recorded"`
	SubmittedBy   string `json:"submitted_by" jsonschema:"Authenticated co-author display name and email"`
	EmailNotified bool   `json:"email_notified" jsonschema:"True if the email notification was immediately dispatched"`
	Confirmation  string `json:"confirmation" jsonschema:"Human-readable confirmation message"`
}
