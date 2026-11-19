package server

import (
	"context"
	"errors"
	"time"

	"connectrpc.com/connect"
	bookv1 "github.com/wjkoh/ghostbook/gen/book/v1"
	"github.com/wjkoh/ghostbook/gen/book/v1/bookv1connect"
	"github.com/wjkoh/ghostbook/internal/engine"
	"github.com/wjkoh/ghostbook/internal/store"
)

type ConnectAdapter struct {
	bookv1connect.UnimplementedBookServiceHandler
	eng *engine.BookEngine
}

func NewConnectAdapter(eng *engine.BookEngine) *ConnectAdapter {
	return &ConnectAdapter{eng: eng}
}

func toConnectError(err error) error {
	if err == nil {
		return nil
	}
	switch {
	case errors.Is(err, context.Canceled):
		return connect.NewError(connect.CodeCanceled, err)
	case errors.Is(err, context.DeadlineExceeded):
		return connect.NewError(connect.CodeDeadlineExceeded, err)
	case errors.Is(err, engine.ErrInvalidSlug), errors.Is(err, engine.ErrInvalidArgument):
		return connect.NewError(connect.CodeInvalidArgument, err)
	case errors.Is(err, engine.ErrNotFound), errors.Is(err, store.ErrNotFound):
		return connect.NewError(connect.CodeNotFound, err)
	case errors.Is(err, engine.ErrPatchNotFound), errors.Is(err, engine.ErrPatchAmbiguous), errors.Is(err, engine.ErrRevisionConflict):
		return connect.NewError(connect.CodeFailedPrecondition, err)
	default:
		return connect.NewError(connect.CodeInternal, errors.New("internal server error"))
	}
}

func (a *ConnectAdapter) GetTableOfContents(ctx context.Context, _ *connect.Request[bookv1.GetTableOfContentsRequest]) (*connect.Response[bookv1.GetTableOfContentsResponse], error) {
	start := time.Now()
	out, err := a.eng.GetTableOfContents(ctx, &engine.GetTableOfContentsInput{})
	auditToolCall(ctx, a.eng, "connectrpc", "GetTableOfContents", "", "", start, err)
	if err != nil {
		return nil, toConnectError(err)
	}
	var chapters []*bookv1.ChapterSummary
	for _, ch := range out.Chapters {
		chapters = append(chapters, &bookv1.ChapterSummary{
			Slug:             ch.Slug,
			Title:            ch.Title,
			WordCount:        ch.WordCount,
			OpenCommentCount: ch.OpenCommentCount,
			LastCommitSha:    ch.LastCommitSHA,
			LastAuthor:       ch.LastAuthor,
			LastUpdatedIso:   ch.LastUpdatedISO,
			LiveUrl:          ch.LiveURL,
			RevisionNumber:   ch.RevisionNumber,
			PermalinkUrl:     ch.PermalinkURL,
			PartTitle:        ch.PartTitle,
			Depth:            ch.Depth,
			ParentSlug:       ch.ParentSlug,
			NumberLabel:      ch.NumberLabel,
		})
	}
	return connect.NewResponse(&bookv1.GetTableOfContentsResponse{
		HeadCommitSha:      out.HeadCommitSHA,
		HeadRevisionNumber: out.HeadRevisionNumber,
		SummaryMarkdown:    out.SummaryMarkdown,
		Chapters:           chapters,
		BookHomeUrl:        out.BookHomeURL,
		BookPdfUrl:         out.BookPDFURL,
	}), nil
}

func (a *ConnectAdapter) ReadChapter(ctx context.Context, req *connect.Request[bookv1.ReadChapterRequest]) (*connect.Response[bookv1.ReadChapterResponse], error) {
	start := time.Now()
	out, err := a.eng.ReadChapter(ctx, &engine.ReadChapterInput{
		Slug:     req.Msg.GetSlug(),
		Revision: req.Msg.GetRevision(),
	})
	revSHA := req.Msg.GetRevision()
	if out != nil && out.RevisionSHA != "" {
		revSHA = out.RevisionSHA
	}
	auditToolCall(ctx, a.eng, "connectrpc", "ReadChapter", req.Msg.GetSlug(), revSHA, start, err)
	if err != nil {
		return nil, toConnectError(err)
	}
	return connect.NewResponse(&bookv1.ReadChapterResponse{
		Slug:                  out.Slug,
		Title:                 out.Title,
		ContentMarkdown:       out.ContentMarkdown,
		RevisionSha:           out.RevisionSHA,
		RevisionNumber:        out.RevisionNumber,
		LastAuthor:            out.LastAuthor,
		LastUpdatedIso:        out.LastUpdatedISO,
		LiveUrl:               out.LiveURL,
		PermalinkUrl:          out.PermalinkURL,
		LifecycleStatus:       out.LifecycleStatus,
		IsLatest:              out.IsLatest,
		CommitsBehind:         out.CommitsBehind,
		LatestRevisionSha:     out.LatestRevisionSHA,
		LatestRevisionNumber:  out.LatestRevisionNumber,
		LatestLiveUrl:         out.LatestLiveURL,
		MovedToSlug:           out.MovedToSlug,
		MovedToTitle:          out.MovedToTitle,
		LifecycleCommitSha:    out.LifecycleCommitSHA,
		LifecycleAuthor:       out.LifecycleAuthor,
		LifecycleTimestampIso: out.LifecycleTimestampISO,
		LifecycleMessage:      out.LifecycleMessage,
	}), nil
}

func (a *ConnectAdapter) SearchBook(ctx context.Context, req *connect.Request[bookv1.SearchBookRequest]) (*connect.Response[bookv1.SearchBookResponse], error) {
	start := time.Now()
	out, err := a.eng.SearchBook(ctx, &engine.SearchBookInput{
		Query: req.Msg.GetQuery(),
		Slug:  req.Msg.GetSlug(),
		Limit: req.Msg.GetLimit(),
	})
	auditToolCall(ctx, a.eng, "connectrpc", "SearchBook", req.Msg.GetSlug(), "", start, err)
	if err != nil {
		return nil, toConnectError(err)
	}
	var matches []*bookv1.SearchMatch
	for _, m := range out.Matches {
		var lineMatches []*bookv1.SearchLineMatch
		for _, lm := range m.LineMatches {
			lineMatches = append(lineMatches, &bookv1.SearchLineMatch{
				LineNumber: lm.LineNumber,
				LineText:   lm.LineText,
			})
		}
		matches = append(matches, &bookv1.SearchMatch{
			Slug:        m.Slug,
			Title:       m.Title,
			Snippet:     m.Snippet,
			Bm25Score:   m.BM25Score,
			LiveUrl:     m.LiveURL,
			LineMatches: lineMatches,
		})
	}
	return connect.NewResponse(&bookv1.SearchBookResponse{Matches: matches}), nil
}

func (a *ConnectAdapter) PatchChapter(ctx context.Context, req *connect.Request[bookv1.PatchChapterRequest]) (*connect.Response[bookv1.PatchChapterResponse], error) {
	start := time.Now()
	var patches []engine.ChapterPatchHunk
	for _, h := range req.Msg.GetPatches() {
		if h == nil {
			continue
		}
		patches = append(patches, engine.ChapterPatchHunk{
			OldText: h.GetOldText(),
			NewText: h.GetNewText(),
		})
	}
	out, err := a.eng.PatchChapter(ctx, &engine.PatchChapterInput{
		Slug:                req.Msg.GetSlug(),
		OldText:             req.Msg.GetOldText(),
		NewText:             req.Msg.GetNewText(),
		Patches:             patches,
		CommitMessage:       req.Msg.GetCommitMessage(),
		ExpectedRevisionSHA: req.Msg.GetExpectedRevisionSha(),
		AuthorName:          req.Msg.GetAuthorName(),
	})
	var sha string
	if out != nil {
		sha = out.NewRevisionSHA
	}
	auditToolCall(ctx, a.eng, "connectrpc", "PatchChapter", req.Msg.GetSlug(), sha, start, err)
	if err != nil {
		return nil, toConnectError(err)
	}
	return connect.NewResponse(&bookv1.PatchChapterResponse{
		Slug:           out.Slug,
		NewRevisionSha: out.NewRevisionSHA,
		CommittedBy:    out.CommittedBy,
		DiffPreview:    out.DiffPreview,
		LiveChapterUrl: out.LiveChapterURL,
		StyleWarnings:  out.StyleWarnings,
	}), nil
}

func (a *ConnectAdapter) CreateChapter(ctx context.Context, req *connect.Request[bookv1.CreateChapterRequest]) (*connect.Response[bookv1.CreateChapterResponse], error) {
	start := time.Now()
	out, err := a.eng.CreateChapter(ctx, &engine.CreateChapterInput{
		Slug:            req.Msg.GetSlug(),
		Title:           req.Msg.GetTitle(),
		InitialMarkdown: req.Msg.GetInitialMarkdown(),
		ParentSlug:      req.Msg.GetParentSlug(),
		PartTitle:       req.Msg.GetPartTitle(),
		CommitMessage:   req.Msg.GetCommitMessage(),
		AuthorName:      req.Msg.GetAuthorName(),
	})
	var sha string
	if out != nil {
		sha = out.NewRevisionSHA
	}
	auditToolCall(ctx, a.eng, "connectrpc", "CreateChapter", req.Msg.GetSlug(), sha, start, err)
	if err != nil {
		return nil, toConnectError(err)
	}
	return connect.NewResponse(&bookv1.CreateChapterResponse{
		Slug:           out.Slug,
		NewRevisionSha: out.NewRevisionSHA,
		LiveChapterUrl: out.LiveChapterURL,
		StyleWarnings:  out.StyleWarnings,
	}), nil
}

func (a *ConnectAdapter) RenameChapter(ctx context.Context, req *connect.Request[bookv1.RenameChapterRequest]) (*connect.Response[bookv1.RenameChapterResponse], error) {
	start := time.Now()
	out, err := a.eng.RenameChapter(ctx, &engine.RenameChapterInput{
		OldSlug:       req.Msg.GetOldSlug(),
		NewSlug:       req.Msg.GetNewSlug(),
		NewTitle:      req.Msg.GetNewTitle(),
		CommitMessage: req.Msg.GetCommitMessage(),
		AuthorName:    req.Msg.GetAuthorName(),
	})
	var sha string
	slug := req.Msg.GetOldSlug()
	if out != nil {
		sha = out.NewRevisionSHA
		slug = out.NewSlug
	}
	auditToolCall(ctx, a.eng, "connectrpc", "RenameChapter", slug, sha, start, err)
	if err != nil {
		return nil, toConnectError(err)
	}
	return connect.NewResponse(&bookv1.RenameChapterResponse{
		OldSlug:        out.OldSlug,
		NewSlug:        out.NewSlug,
		NewTitle:       out.NewTitle,
		NewRevisionSha: out.NewRevisionSHA,
		LiveChapterUrl: out.LiveChapterURL,
		PermalinkUrl:   out.PermalinkURL,
	}), nil
}

func (a *ConnectAdapter) DeleteChapter(ctx context.Context, req *connect.Request[bookv1.DeleteChapterRequest]) (*connect.Response[bookv1.DeleteChapterResponse], error) {
	start := time.Now()
	out, err := a.eng.DeleteChapter(ctx, &engine.DeleteChapterInput{
		Slug:           req.Msg.GetSlug(),
		Reason:         req.Msg.GetReason(),
		ReplacedBySlug: req.Msg.GetReplacedBySlug(),
		AuthorName:     req.Msg.GetAuthorName(),
	})
	var sha string
	auditSlug := req.Msg.GetSlug()
	if out != nil {
		sha = out.NewRevisionSHA
		if out.DeletedSlug != "" {
			auditSlug = out.DeletedSlug
		}
	}
	auditToolCall(ctx, a.eng, "connectrpc", "DeleteChapter", auditSlug, sha, start, err)
	if err != nil {
		return nil, toConnectError(err)
	}
	return connect.NewResponse(&bookv1.DeleteChapterResponse{
		DeletedSlug:          out.DeletedSlug,
		NewRevisionSha:       out.NewRevisionSHA,
		ArchivedPermalinkUrl: out.ArchivedPermalinkURL,
	}), nil
}

func (a *ConnectAdapter) ListRecentChanges(ctx context.Context, req *connect.Request[bookv1.ListRecentChangesRequest]) (*connect.Response[bookv1.ListRecentChangesResponse], error) {
	start := time.Now()
	out, err := a.eng.ListRecentChanges(ctx, &engine.ListRecentChangesInput{
		Slug:  req.Msg.GetSlug(),
		Limit: req.Msg.GetLimit(),
	})
	auditToolCall(ctx, a.eng, "connectrpc", "ListRecentChanges", req.Msg.GetSlug(), "", start, err)
	if err != nil {
		return nil, toConnectError(err)
	}
	var commits []*bookv1.CommitEntry
	for _, c := range out.Commits {
		commits = append(commits, &bookv1.CommitEntry{
			CommitSha:      c.CommitSHA,
			FullSha:        c.FullSHA,
			RevisionNumber: c.RevisionNumber,
			AuthorName:     c.AuthorName,
			AuthorEmail:    c.AuthorEmail,
			TimestampIso:   c.TimestampISO,
			CommitMessage:  c.CommitMessage,
			FilesChanged:   c.FilesChanged,
		})
	}
	return connect.NewResponse(&bookv1.ListRecentChangesResponse{Commits: commits}), nil
}

func (a *ConnectAdapter) GetCommitDiff(ctx context.Context, req *connect.Request[bookv1.GetCommitDiffRequest]) (*connect.Response[bookv1.GetCommitDiffResponse], error) {
	start := time.Now()
	out, err := a.eng.GetCommitDiff(ctx, &engine.GetCommitDiffInput{
		CommitSHA:     req.Msg.GetCommitSha(),
		Slug:          req.Msg.GetSlug(),
		BaseCommitSHA: req.Msg.GetBaseCommitSha(),
	})
	auditToolCall(ctx, a.eng, "connectrpc", "GetCommitDiff", req.Msg.GetSlug(), req.Msg.GetCommitSha(), start, err)
	if err != nil {
		return nil, toConnectError(err)
	}
	return connect.NewResponse(&bookv1.GetCommitDiffResponse{
		CommitSha:     out.CommitSHA,
		AuthorName:    out.AuthorName,
		TimestampIso:  out.TimestampISO,
		CommitMessage: out.CommitMessage,
		UnifiedDiff:   out.UnifiedDiff,
	}), nil
}

func (a *ConnectAdapter) GetChapterBlame(ctx context.Context, req *connect.Request[bookv1.GetChapterBlameRequest]) (*connect.Response[bookv1.GetChapterBlameResponse], error) {
	start := time.Now()
	out, err := a.eng.GetChapterBlame(ctx, &engine.GetChapterBlameInput{
		Slug: req.Msg.GetSlug(),
	})
	auditToolCall(ctx, a.eng, "connectrpc", "GetChapterBlame", req.Msg.GetSlug(), "", start, err)
	if err != nil {
		return nil, toConnectError(err)
	}
	var secs []*bookv1.BlameSection
	for _, s := range out.Sections {
		secs = append(secs, &bookv1.BlameSection{
			Heading:        s.Heading,
			LastCommitSha:  s.LastCommitSHA,
			LastAuthor:     s.LastAuthor,
			LastUpdatedIso: s.LastUpdatedISO,
			StartLine:      s.StartLine,
			EndLine:        s.EndLine,
		})
	}
	return connect.NewResponse(&bookv1.GetChapterBlameResponse{
		Slug:     out.Slug,
		Sections: secs,
	}), nil
}

func (a *ConnectAdapter) RevertChapterRevision(ctx context.Context, req *connect.Request[bookv1.RevertChapterRevisionRequest]) (*connect.Response[bookv1.RevertChapterRevisionResponse], error) {
	start := time.Now()
	out, err := a.eng.RevertChapterRevision(ctx, &engine.RevertChapterRevisionInput{
		Slug:            req.Msg.GetSlug(),
		TargetCommitSHA: req.Msg.GetTargetCommitSha(),
		Reason:          req.Msg.GetReason(),
		AuthorName:      req.Msg.GetAuthorName(),
	})
	var sha string
	if out != nil {
		sha = out.NewRevisionSHA
	}
	auditToolCall(ctx, a.eng, "connectrpc", "RevertChapterRevision", req.Msg.GetSlug(), sha, start, err)
	if err != nil {
		return nil, toConnectError(err)
	}
	return connect.NewResponse(&bookv1.RevertChapterRevisionResponse{
		Slug:           out.Slug,
		NewRevisionSha: out.NewRevisionSHA,
		LiveChapterUrl: out.LiveChapterURL,
	}), nil
}

func (a *ConnectAdapter) ListAgentActivity(ctx context.Context, req *connect.Request[bookv1.ListAgentActivityRequest]) (*connect.Response[bookv1.ListAgentActivityResponse], error) {
	start := time.Now()
	out, err := a.eng.ListAgentActivity(ctx, &engine.ListAgentActivityInput{
		Limit:    req.Msg.GetLimit(),
		AuthorID: req.Msg.GetAuthorId(),
	})
	auditToolCall(ctx, a.eng, "connectrpc", "ListAgentActivity", "", "", start, err)
	if err != nil {
		return nil, toConnectError(err)
	}
	var entries []*bookv1.AgentActivityEntry
	for _, e := range out.Entries {
		entries = append(entries, &bookv1.AgentActivityEntry{
			Id:           e.ID,
			TimestampIso: e.TimestampISO,
			AuthorId:     e.AuthorID,
			Protocol:     e.Protocol,
			ToolName:     e.ToolName,
			ChapterSlug:  e.ChapterSlug,
			GitCommitSha: e.GitCommitSHA,
			Status:       e.Status,
			DurationMs:   e.DurationMs,
			UserAgent:    e.UserAgent,
			ErrorMessage: e.ErrorMessage,
		})
	}
	return connect.NewResponse(&bookv1.ListAgentActivityResponse{Entries: entries}), nil
}

func (a *ConnectAdapter) ReportPlatformFeedback(ctx context.Context, req *connect.Request[bookv1.ReportPlatformFeedbackRequest]) (*connect.Response[bookv1.ReportPlatformFeedbackResponse], error) {
	start := time.Now()
	out, err := a.eng.ReportPlatformFeedback(ctx, &engine.ReportPlatformFeedbackInput{
		Kind:        req.Msg.GetKind(),
		Summary:     req.Msg.GetSummary(),
		Details:     req.Msg.GetDetails(),
		ChapterSlug: req.Msg.GetChapterSlug(),
		RevisionSHA: req.Msg.GetRevisionSha(),
	})
	auditToolCall(ctx, a.eng, "connectrpc", "ReportPlatformFeedback", req.Msg.GetChapterSlug(), req.Msg.GetRevisionSha(), start, err)
	if err != nil {
		return nil, toConnectError(err)
	}
	return connect.NewResponse(&bookv1.ReportPlatformFeedbackResponse{
		FeedbackId:    out.FeedbackID,
		RecordedAtIso: out.RecordedAtISO,
		SubmittedBy:   out.SubmittedBy,
		EmailNotified: out.EmailNotified,
		Confirmation:  out.Confirmation,
	}), nil
}
