package server

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/wjkoh/ghostbook/internal/engine"
	"github.com/wjkoh/ghostbook/internal/store"
)

const (
	mcpFrictionRetryCooldown = 15 * time.Minute
	mcpFrictionBurstCooldown = 30 * time.Minute
)

func classifyErrorStatus(err error) string {
	if err == nil {
		return "ok"
	}
	switch {
	case errors.Is(err, context.Canceled),
		errors.Is(err, context.DeadlineExceeded):
		return "canceled"
	case errors.Is(err, engine.ErrPatchNotFound),
		errors.Is(err, engine.ErrPatchAmbiguous),
		errors.Is(err, engine.ErrRevisionConflict):
		return "conflict"
	case errors.Is(err, engine.ErrInvalidSlug),
		errors.Is(err, engine.ErrInvalidArgument):
		return "invalid_arg"
	case errors.Is(err, engine.ErrNotFound),
		errors.Is(err, store.ErrNotFound):
		return "not_found"
	default:
		return "error"
	}
}

func auditToolCall(ctx context.Context, eng *engine.BookEngine, protocol, toolName, slug, commitSHA string, start time.Time, err error) {
	authorID := "anonymous"
	if a := engine.AuthorFromContext(ctx); a != nil {
		authorID = a.ID
	}
	errMsg := ""
	if err != nil {
		errMsg = err.Error()
	}
	status := classifyErrorStatus(err)
	durationMs := time.Since(start).Milliseconds()
	ua := userAgentFromContext(ctx)

	if err != nil {
		slog.Warn("tool_invocation_failed",
			"protocol", protocol,
			"tool", toolName,
			"author", authorID,
			"slug", slug,
			"status", status,
			"duration_ms", durationMs,
			"error", errMsg,
		)
		if status == "error" && !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
			eng.NotifyAlert(ctx, "tool_error_"+toolName, protocol+" "+toolName+" failed: "+errMsg, "Author: "+authorID+"\nSlug: "+slug+"\nError: "+errMsg)
		}
	} else {
		slog.Info("tool_invocation",
			"protocol", protocol,
			"tool", toolName,
			"author", authorID,
			"slug", slug,
			"commit_sha", commitSHA,
			"status", status,
			"duration_ms", durationMs,
		)
	}

	_ = eng.Store().LogActivity(ctx, store.AuditEntry{
		AuthorID:     authorID,
		Protocol:     protocol,
		UserAgent:    ua,
		ToolName:     toolName,
		ChapterSlug:  slug,
		GitCommitSHA: commitSHA,
		Status:       status,
		ErrorMessage: errMsg,
		DurationMs:   durationMs,
	})

	evaluateSilentMCPFriction(ctx, eng, authorID, ua, protocol, toolName)
}

func evaluateSilentMCPFriction(ctx context.Context, eng *engine.BookEngine, authorID, ua, protocol, toolName string) {
	if (protocol != "mcp" && protocol != "connectrpc") || toolName == "report_platform_feedback" || toolName == "ReportPlatformFeedback" {
		return
	}
	alerter := eng.AlertNotifier()
	if alerter == nil || !alerter.Enabled() {
		return
	}

	retryKey := "friction_retry_" + authorID
	burstKey := "friction_burst_" + authorID
	retryCooling := alerter.IsInCooldown(retryKey, mcpFrictionRetryCooldown)
	burstCooling := alerter.IsInCooldown(burstKey, mcpFrictionBurstCooldown)
	if retryCooling && burstCooling {
		return
	}

	stats, err := eng.Store().CheckMCPFrictionWindow(ctx, authorID, time.Now().UTC())
	if err != nil || stats == nil {
		return
	}

	if !retryCooling && stats.FailedCalls120s >= 3 {
		var b strings.Builder
		fmt.Fprintf(&b, "Author ID:         %s\n", authorID)
		fmt.Fprintf(&b, "User-Agent:        %s\n", ua)
		fmt.Fprintf(&b, "Failed Calls 120s: %d\n\nRecent Failing MCP Calls:\n", stats.FailedCalls120s)
		for _, f := range stats.RecentFailures {
			fmt.Fprintf(&b, "- [%s] %s slug=%q status=%s err=%q\n",
				f.TimestampISO, f.ToolName, f.ChapterSlug, f.Status, f.ErrorMessage)
		}
		alerter.DispatchAsyncAlert(
			ctx,
			retryKey,
			mcpFrictionRetryCooldown,
			fmt.Sprintf("%d failing MCP calls by %s within 120s", stats.FailedCalls120s, authorID),
			b.String(),
		)
	}

	if !burstCooling && stats.WriteCalls180s >= 8 && stats.DistinctWriteSlug >= 4 {
		var b strings.Builder
		fmt.Fprintf(&b, "Author ID:              %s\n", authorID)
		fmt.Fprintf(&b, "User-Agent:             %s\n", ua)
		fmt.Fprintf(&b, "Write Calls 180s:       %d\n", stats.WriteCalls180s)
		fmt.Fprintf(&b, "Distinct Chapter Slugs: %d (%s)\n", stats.DistinctWriteSlug, strings.Join(stats.RecentWriteSlugs, ", "))
		alerter.DispatchAsyncAlert(
			ctx,
			burstKey,
			mcpFrictionBurstCooldown,
			fmt.Sprintf("%d MCP write calls across %d chapters by %s within 180s", stats.WriteCalls180s, stats.DistinctWriteSlug, authorID),
			b.String(),
		)
	}
}

func registerMCPTool[In, Out any](
	s *mcp.Server,
	eng *engine.BookEngine,
	name, desc string,
	fn func(context.Context, *In) (*Out, error),
	metaFn func(*In, *Out) (slug, sha string),
) {
	mcp.AddTool(s, &mcp.Tool{Name: name, Description: desc}, func(ctx context.Context, _ *mcp.CallToolRequest, in *In) (*mcp.CallToolResult, *Out, error) {
		start := time.Now()
		out, err := fn(ctx, in)
		var slug, sha string
		if metaFn != nil && in != nil {
			slug, sha = metaFn(in, out)
		}
		auditToolCall(ctx, eng, "mcp", name, slug, sha, start, err)
		return nil, out, err
	})
}

const mcpPlatformFeedbackInstruction = "\n\n## Platform Feedback & Feature Requests\nUse `report_platform_feedback` strictly for bugs, UX friction, or feature requests concerning the book server, Web Studio, or MCP tools (editorial notes on book content belong inline in Markdown via `patch_chapter`). Call it immediately when the user asks to report a bug or request a feature; never call it unprompted mid-task—if you hit a reproducible tool defect or repetitive multi-call workaround (>=5 calls), finish the user's primary task first and ask in one brief postscript sentence before calling it."

func NewMCPServer(eng *engine.BookEngine) *mcp.Server {
	instructions := eng.StyleGuide()
	if !strings.Contains(instructions, "report_platform_feedback") {
		instructions += mcpPlatformFeedbackInstruction
	}
	s := mcp.NewServer(&mcp.Implementation{
		Name:    eng.MCPName(),
		Version: "1.0.0",
	}, &mcp.ServerOptions{
		Instructions: instructions,
	})

	registerMCPTool(s, eng,
		"get_table_of_contents",
		"Return the book Table of Contents (SUMMARY.md), word counts, and live URLs.",
		eng.GetTableOfContents,
		nil,
	)

	registerMCPTool(s, eng,
		"read_chapter",
		"Read the full Markdown prose or JSON config and current Git revision SHA of a chapter by slug (or SUMMARY / STYLE_GUIDE / BOOK_CONFIG).",
		eng.ReadChapter,
		func(in *engine.ReadChapterInput, out *engine.ReadChapterOutput) (string, string) {
			if out != nil {
				return in.Slug, out.RevisionSHA
			}
			return in.Slug, in.Revision
		},
	)

	registerMCPTool(s, eng,
		"search_book",
		"Search across all book chapters (or within a specific chapter slug) using SQLite FTS5 BM25 ranking plus exact line-level substring matching, returning highlighted snippets and 1-based line_matches.",
		eng.SearchBook,
		func(in *engine.SearchBookInput, _ *engine.SearchBookOutput) (string, string) {
			return in.Slug, ""
		},
	)

	registerMCPTool(s, eng,
		"patch_chapter",
		"Apply a surgical search-and-replace edit (or multi-hunk patches array) to a chapter or meta-file (SUMMARY, STYLE_GUIDE, BOOK_CONFIG), commit to Git via go-git under the author's identity, update FTS5, and re-render the live book.",
		eng.PatchChapter,
		func(in *engine.PatchChapterInput, out *engine.PatchChapterOutput) (string, string) {
			if out != nil {
				return in.Slug, out.NewRevisionSHA
			}
			return in.Slug, ""
		},
	)

	registerMCPTool(s, eng,
		"create_chapter",
		"Create a new chapter Markdown file, register it in SUMMARY.md, commit to Git, and index in FTS5.",
		eng.CreateChapter,
		func(in *engine.CreateChapterInput, out *engine.CreateChapterOutput) (string, string) {
			if out != nil {
				return in.Slug, out.NewRevisionSHA
			}
			return in.Slug, ""
		},
	)

	registerMCPTool(s, eng,
		"list_recent_changes",
		"List recent Git commits across the book or filtered to a single chapter.",
		eng.ListRecentChanges,
		func(in *engine.ListRecentChangesInput, _ *engine.ListRecentChangesOutput) (string, string) {
			return in.Slug, ""
		},
	)

	registerMCPTool(s, eng,
		"get_commit_diff",
		"Inspect the exact unified Markdown diff for a specific Git commit SHA.",
		eng.GetCommitDiff,
		func(in *engine.GetCommitDiffInput, _ *engine.GetCommitDiffOutput) (string, string) {
			return in.Slug, in.CommitSHA
		},
	)

	registerMCPTool(s, eng,
		"get_chapter_blame",
		"Return section-by-section Git blame attribution for a chapter via go-git.",
		eng.GetChapterBlame,
		func(in *engine.GetChapterBlameInput, _ *engine.GetChapterBlameOutput) (string, string) {
			return in.Slug, ""
		},
	)

	registerMCPTool(s, eng,
		"revert_chapter_revision",
		"Restore a chapter to a prior commit SHA via a clean forward Git commit.",
		eng.RevertChapterRevision,
		func(in *engine.RevertChapterRevisionInput, out *engine.RevertChapterRevisionOutput) (string, string) {
			if out != nil {
				return in.Slug, out.NewRevisionSHA
			}
			return in.Slug, ""
		},
	)

	registerMCPTool(s, eng,
		"list_agent_activity",
		"List recent AI agent and studio tool invocations from the SQLite audit log.",
		eng.ListAgentActivity,
		nil,
	)

	registerMCPTool(s, eng,
		"rename_chapter",
		"Rename or move a chapter slug and/or H1 title, update SUMMARY.md and cross-chapter links, and commit to Git.",
		eng.RenameChapter,
		func(in *engine.RenameChapterInput, out *engine.RenameChapterOutput) (string, string) {
			if out != nil {
				return out.NewSlug, out.NewRevisionSHA
			}
			return in.OldSlug, ""
		},
	)

	registerMCPTool(s, eng,
		"delete_chapter",
		"Remove a chapter from the active book and SUMMARY.md while preserving its full Git history for archived permalink resolution.",
		eng.DeleteChapter,
		func(in *engine.DeleteChapterInput, out *engine.DeleteChapterOutput) (string, string) {
			if out != nil {
				return out.DeletedSlug, out.NewRevisionSHA
			}
			return in.Slug, ""
		},
	)

	registerMCPTool(s, eng,
		"report_platform_feedback",
		"Report a bug, UX friction, or feature request concerning the book server, Web Studio, or MCP tools (stored in SQLite and optionally emailed to the maintainer with recent tool context). Use strictly for software/platform feedback—not editorial chapter prose comments. Call immediately when the user asks to report an issue or suggest a feature; do not call unprompted mid-task.",
		eng.ReportPlatformFeedback,
		func(in *engine.ReportPlatformFeedbackInput, _ *engine.ReportPlatformFeedbackOutput) (string, string) {
			return in.ChapterSlug, in.RevisionSHA
		},
	)

	return s
}
