package server

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"net/url"
	"strings"

	"github.com/wjkoh/ghostbook/internal/engine"
	"github.com/wjkoh/ghostbook/internal/renderer"
)

func buildOpenAPISpecDocument(baseURL, bookTitle string) map[string]any {
	type prop map[string]any
	if strings.TrimSpace(bookTitle) == "" {
		bookTitle = "Ghostbook"
	}

	paths := map[string]any{}

	addPostOp := func(pathKey, tag, opID, summary, desc string, reqProps map[string]any, reqRequired []string) {
		reqSchema := map[string]any{
			"type":       "object",
			"properties": reqProps,
		}
		if len(reqRequired) > 0 {
			reqSchema["required"] = reqRequired
		}
		paths[pathKey] = map[string]any{
			"post": map[string]any{
				"tags":        []string{tag},
				"operationId": opID,
				"summary":     summary,
				"description": desc,
				"requestBody": map[string]any{
					"required": true,
					"content": map[string]any{
						"application/json": map[string]any{
							"schema": reqSchema,
						},
					},
				},
				"responses": map[string]any{
					"200": map[string]any{
						"description": "Successful ConnectRPC JSON response",
						"content": map[string]any{
							"application/json": map[string]any{
								"schema": map[string]any{"type": "object"},
							},
						},
					},
				},
			},
		}
	}

	addPostOp(
		"/book.v1.BookService/GetTableOfContents",
		"Reading & Search",
		"GetTableOfContents",
		"Return Table of Contents and chapter metadata",
		"Returns SUMMARY.md, word counts, last author, and live + permalink URLs for all chapters.",
		map[string]any{}, nil,
	)

	addPostOp(
		"/book.v1.BookService/ReadChapter",
		"Reading & Search",
		"ReadChapter",
		"Read chapter Markdown and permalink revision state",
		"Reads full Markdown prose or JSON config, revision number/SHA, and permalink lineage state (current, outdated, renamed, moved, deleted) for any chapter, SUMMARY, STYLE_GUIDE, or BOOK_CONFIG.",
		map[string]any{
			"slug":     prop{"type": "string", "description": "Chapter or meta slug (e.g. 01-system-architecture, SUMMARY, STYLE_GUIDE, BOOK_CONFIG)", "example": "01-system-architecture"},
			"revision": prop{"type": "string", "description": "Optional commit SHA (e.g. 6ca162a) or sequential revision number (e.g. r1, r2) to read a historical permalink revision"},
		},
		[]string{"slug"},
	)

	addPostOp(
		"/book.v1.BookService/SearchBook",
		"Reading & Search",
		"SearchBook",
		"Search chapters via SQLite FTS5 BM25 and exact line matching",
		"Searches across all chapters (or within a single chapter slug) using SQLite FTS5 BM25 ranking plus 1-based exact line matching.",
		map[string]any{
			"query": prop{"type": "string", "description": "Search query keywords or exact phrase", "example": "SQLite FTS5"},
			"slug":  prop{"type": "string", "description": "Optional chapter slug to restrict search to a single chapter"},
			"limit": prop{"type": "integer", "description": "Maximum matches to return", "example": 10},
		},
		[]string{"query"},
	)

	addPostOp(
		"/book.v1.BookService/PatchChapter",
		"Surgical Authoring",
		"PatchChapter",
		"Apply surgical search-and-replace or multi-hunk edits to a chapter",
		"Applies old_text -> new_text or an atomic patches[] array, commits to Git via go-git under the authenticated author, updates FTS5, and returns style_warnings.",
		map[string]any{
			"slug":                  prop{"type": "string", "description": "Target chapter or meta slug (or SUMMARY / STYLE_GUIDE / BOOK_CONFIG)", "example": "01-system-architecture"},
			"old_text":              prop{"type": "string", "description": "Exact contiguous Markdown text to replace (optional when patches is provided)"},
			"new_text":              prop{"type": "string", "description": "Replacement Markdown text (optional when patches is provided)"},
			"patches":               prop{"type": "array", "description": "Optional array of surgical {old_text, new_text} hunks applied atomically in one commit", "items": map[string]any{"type": "object", "properties": map[string]any{"old_text": prop{"type": "string"}, "new_text": prop{"type": "string"}}}},
			"commit_message":        prop{"type": "string", "description": "Optional concise Git commit summary (defaults to 'Update <slug>')", "example": "Clarify SQLite FTS5 indexing flow"},
			"expected_revision_sha": prop{"type": "string", "description": "Optional optimistic lock SHA"},
			"author_name":           prop{"type": "string", "description": "Optional author name or ID override"},
		},
		[]string{"slug"},
	)

	addPostOp(
		"/book.v1.BookService/CreateChapter",
		"Surgical Authoring",
		"CreateChapter",
		"Create a new chapter and register in SUMMARY.md",
		"Creates a new Markdown chapter file, adds it to SUMMARY.md, commits to Git, indexes in FTS5, and returns style_warnings.",
		map[string]any{
			"slug":             prop{"type": "string", "description": "New chapter slug", "example": "08-deployment-guide"},
			"title":            prop{"type": "string", "description": "Chapter title", "example": "Chapter 8: Deployment & Operations Guide"},
			"initial_markdown": prop{"type": "string", "description": "Optional initial Markdown content (defaults to '# <title>')"},
			"commit_message":   prop{"type": "string", "description": "Optional commit message", "example": "Add Chapter 8 on deployment and operations"},
			"author_name":      prop{"type": "string", "description": "Optional author name or ID override"},
			"parent_slug":      prop{"type": "string", "description": "Optional parent chapter slug for nested sub-chapters"},
			"part_title":       prop{"type": "string", "description": "Optional Part heading in SUMMARY.md"},
		},
		[]string{"slug", "title"},
	)

	addPostOp(
		"/book.v1.BookService/RenameChapter",
		"Surgical Authoring",
		"RenameChapter",
		"Rename or move a chapter slug and/or H1 title",
		"Moves src/<old_slug>.md to src/<new_slug>.md, updates H1 title and SUMMARY.md, and rewrites cross-chapter links.",
		map[string]any{
			"old_slug":       prop{"type": "string", "description": "Existing chapter slug", "example": "04-deployment-ops"},
			"new_slug":       prop{"type": "string", "description": "Optional new chapter slug"},
			"new_title":      prop{"type": "string", "description": "Optional new H1 chapter title"},
			"commit_message": prop{"type": "string", "description": "Optional Git commit message"},
			"author_name":    prop{"type": "string", "description": "Optional author name or ID override"},
		},
		[]string{"old_slug"},
	)

	addPostOp(
		"/book.v1.BookService/DeleteChapter",
		"Surgical Authoring",
		"DeleteChapter",
		"Delete a chapter from the active book while preserving archived permalinks",
		"Removes src/<slug>.md and its SUMMARY.md entry while keeping its full Git history accessible via permalink URLs.",
		map[string]any{
			"slug":             prop{"type": "string", "description": "Chapter slug to delete"},
			"reason":           prop{"type": "string", "description": "Editorial reason for deleting the chapter"},
			"replaced_by_slug": prop{"type": "string", "description": "Optional replacement chapter slug to link on archived permalink pages"},
			"author_name":      prop{"type": "string", "description": "Optional author name or ID override"},
		},
		[]string{"slug"},
	)

	addPostOp(
		"/book.v1.BookService/ListRecentChanges",
		"Git History & Audit",
		"ListRecentChanges",
		"List recent Git commits",
		"Lists recent Git commits across the book or for a single chapter.",
		map[string]any{
			"slug":  prop{"type": "string", "description": "Optional chapter slug", "example": ""},
			"limit": prop{"type": "integer", "description": "Max commits", "example": 15},
		},
		nil,
	)

	addPostOp(
		"/book.v1.BookService/GetCommitDiff",
		"Git History & Audit",
		"GetCommitDiff",
		"Get unified Markdown diff for a commit or between two revisions",
		"Returns the unified diff for a specific commit SHA (or between base_commit_sha and commit_sha).",
		map[string]any{
			"commit_sha":      prop{"type": "string", "description": "Target commit SHA or revision number (e.g. HEAD or r3)", "example": "HEAD"},
			"slug":            prop{"type": "string", "description": "Optional chapter slug"},
			"base_commit_sha": prop{"type": "string", "description": "Optional base commit SHA or revision number (e.g. r1) to diff against"},
		},
		[]string{"commit_sha"},
	)

	addPostOp(
		"/book.v1.BookService/GetChapterBlame",
		"Git History & Audit",
		"GetChapterBlame",
		"Get section-level Git blame attribution",
		"Computes section-by-section blame attribution for a chapter via go-git.",
		map[string]any{
			"slug": prop{"type": "string", "description": "Chapter slug", "example": "01-system-architecture"},
		},
		[]string{"slug"},
	)

	addPostOp(
		"/book.v1.BookService/RevertChapterRevision",
		"Git History & Audit",
		"RevertChapterRevision",
		"Restore a chapter to a prior commit SHA or revision number",
		"Creates a forward Git commit restoring a chapter to a historical commit SHA or revision number.",
		map[string]any{
			"slug":              prop{"type": "string", "description": "Chapter slug", "example": "01-system-architecture"},
			"target_commit_sha": prop{"type": "string", "description": "Target historical commit SHA or revision number (e.g. r1)"},
			"reason":            prop{"type": "string", "description": "Reason for reverting"},
			"author_name":       prop{"type": "string", "description": "Optional author name or ID override"},
		},
		[]string{"slug", "target_commit_sha"},
	)

	addPostOp(
		"/book.v1.BookService/ListAgentActivity",
		"Git History & Audit",
		"ListAgentActivity",
		"List recent AI agent tool calls",
		"Returns recent MCP and ConnectRPC tool invocations from the SQLite audit log.",
		map[string]any{
			"limit":     prop{"type": "integer", "description": "Max entries", "example": 20},
			"author_id": prop{"type": "string", "description": "Optional author ID filter (e.g. alice, bob)"},
		},
		nil,
	)

	addPostOp(
		"/book.v1.BookService/ReportPlatformFeedback",
		"Platform Feedback",
		"ReportPlatformFeedback",
		"Submit a platform bug report, UX friction note, or feature request",
		"Persists platform feedback in SQLite with recent tool context and dispatches an immediate maintainer email notification.",
		map[string]any{
			"kind":         prop{"type": "string", "enum": []string{"bug", "feature_request", "ux_friction"}, "description": "Feedback kind: bug, feature_request, or ux_friction", "example": "feature_request"},
			"summary":      prop{"type": "string", "description": "Concise single-line summary (5 to 200 chars)"},
			"details":      prop{"type": "string", "description": "Optional reproduction steps or rationale"},
			"chapter_slug": prop{"type": "string", "description": "Optional chapter or meta slug"},
			"revision_sha": prop{"type": "string", "description": "Optional revision SHA or rN label"},
		},
		[]string{"summary"},
	)

	spec := map[string]any{
		"openapi": "3.1.0",
		"info": map[string]any{
			"title":       bookTitle + " · API Reference",
			"version":     "1.0.0",
			"description": "ConnectRPC HTTP/JSON, Remote MCP, and Zero-HTML Raw Markdown interface for collaborative reading and AI-agent authoring of " + bookTitle + " (13 operations mirroring MCP 1-for-1).\n\n### Zero-HTML HTTP Raw Markdown, RSS & Agent Discovery (Read-Only)\n- **Discovery Index (`llms.txt`):** `GET /llms.txt` (or `GET /` with `Accept: text/markdown`)\n- **Full Book Raw Markdown (`llms-full.txt` / `book.md`):** `GET /llms-full.txt`, `GET /book.md`, or historical `GET /rev/{rev}/book.md`\n- **Live Chapter Raw Markdown:** `GET /chapters/{slug}.md`, `GET /{slug}.md`, or `GET /chapters/{slug}` with `Accept: text/markdown`\n- **Permalink Chapter Raw Markdown:** `GET /rev/{rev}/chapters/{slug}.md`, `GET /chapters/{slug}/rev/{rev}.md`, `GET /chapters/{slug}@{rev}.md` (append `?raw=exact` for unannotated historical bytes)\n- **Editorial Timeline RSS 2.0:** `GET /feed.xml` (aliases: `/rss.xml`, `/timeline.xml`)\n- **Meta & Audit Documents:** `GET /timeline.md`, `GET /SUMMARY.md`, `GET /STYLE_GUIDE.md`, `GET /book.json`, `GET /changelog.md`, `GET /guide.md`\n- **Mermaid Diagram Source:** `GET /chapters/{slug}/diagrams/{n}.mmd`",
		},
		"tags": []map[string]any{
			{"name": "Reading & Search", "description": "Inspect the Table of Contents, read chapter Markdown, and run SQLite FTS5 BM25 + line queries."},
			{"name": "Surgical Authoring", "description": "Apply exact search-and-replace or multi-hunk patches, create, rename, or archive chapters."},
			{"name": "Git History & Audit", "description": "Inspect chronological Git commits, unified diffs, section-level blame, and multi-agent audit logs."},
			{"name": "Platform Feedback", "description": "Report platform software bugs, UX friction, or feature requests."},
		},
		"servers": []map[string]any{
			{"url": baseURL, "description": bookTitle + " Server"},
		},
		"components": map[string]any{
			"securitySchemes": map[string]any{
				"BearerAuth": map[string]any{
					"type":         "http",
					"scheme":       "bearer",
					"bearerFormat": "<email>:<secret-key>",
					"description":  "Pass your secret co-author credential (<email>:<secret-key>) as the Bearer token or via the /u/<email>:<secret-key>/book.v1.BookService/... URL prefix.",
				},
			},
		},
		"security": []map[string]any{
			{"BearerAuth": []string{}},
		},
		"paths": paths,
	}

	return spec
}

func resolveSafeOpenAPIServerURL(baseURL string, allowedHosts []string, r *http.Request) string {
	cleanBase := strings.TrimRight(strings.TrimSpace(baseURL), "/")
	if cleanBase == "" {
		cleanBase = "http://127.0.0.1:8090"
	}
	if r == nil || strings.TrimSpace(r.Host) == "" {
		return cleanBase
	}
	reqHost := strings.ToLower(strings.TrimSpace(r.Host))
	var configuredHost, configuredScheme, configuredPath string
	if parsed, err := url.Parse(cleanBase); err == nil {
		configuredHost = strings.ToLower(parsed.Host)
		configuredScheme = strings.ToLower(parsed.Scheme)
		configuredPath = strings.TrimRight(parsed.Path, "/")
	}
	allowedHost := reqHost == configuredHost
	if !allowedHost {
		for _, h := range allowedHosts {
			if strings.EqualFold(strings.TrimSpace(h), reqHost) {
				allowedHost = true
				break
			}
		}
	}
	if !allowedHost {
		return cleanBase
	}
	scheme := configuredScheme
	if scheme == "" {
		if r.TLS != nil {
			scheme = "https"
		} else {
			scheme = "http"
		}
	}
	return scheme + "://" + reqHost + configuredPath
}

func ServeOpenAPISpec(baseURL string, bookTitle string, allowedHosts ...string) http.HandlerFunc {
	return ServeOpenAPISpecWithTitleFn(baseURL, func() string { return bookTitle }, allowedHosts...)
}

func ServeOpenAPISpecWithTitleFn(baseURL string, titleFn func() string, allowedHosts ...string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		bookTitle := ""
		if titleFn != nil {
			bookTitle = strings.TrimSpace(titleFn())
		}
		if bookTitle == "" {
			bookTitle = "Ghostbook"
		}
		spec := buildOpenAPISpecDocument(baseURL, bookTitle)
		serverURL := resolveSafeOpenAPIServerURL(baseURL, allowedHosts, r)
		doc := map[string]any{
			"openapi":    spec["openapi"],
			"info":       spec["info"],
			"tags":       spec["tags"],
			"servers":    []map[string]any{{"url": serverURL, "description": bookTitle + " Server"}},
			"components": spec["components"],
			"security":   spec["security"],
			"paths":      spec["paths"],
		}
		body, err := json.MarshalIndent(doc, "", "  ")
		if err != nil {
			http.Error(w, "internal server error", http.StatusInternalServerError)
			return
		}
		body = append(body, '\n')
		etag := computeStrongETag(body)
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("ETag", etag)
		if etagMatches(r.Header.Get("If-None-Match"), etag) {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		if r.Method == http.MethodHead {
			w.WriteHeader(http.StatusOK)
			return
		}
		_, _ = w.Write(body)
	}
}

func ServeOpenAPIDocs(engOpt ...*engine.BookEngine) http.HandlerFunc {
	var eng *engine.BookEngine
	if len(engOpt) > 0 {
		eng = engOpt[0]
	}

	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		wantsMD, _ := negotiateResponseFormat(r, "")
		if wantsMD && eng != nil {
			res, err := eng.AssembleGuideMarkdown(r.Context())
			if err != nil {
				slog.Error("assemble_guide_markdown_failed", "error", err)
				http.Error(w, "internal server error", http.StatusInternalServerError)
				return
			}
			writeMarkdownExportResult(w, r, res, false, "/api-docs", "/guide.md")
			return
		}
		baseURL := ""
		bookTitle := "Ghostbook"
		if eng != nil {
			baseURL = eng.BaseURL()
			bookTitle = eng.BookTitle()
		}
		w.Header().Set("Content-Security-Policy", APIDocsContentSecurityPolicy)
		out, err := renderer.RenderAPIDocsPageWithSpec(renderer.APIDocsPageData{
			BaseURL:     baseURL,
			BookTitle:   bookTitle,
			SpecVersion: "3.1.0",
		})
		writeHTMLResponse(w, r, out, err, "/guide.md")
	}
}
