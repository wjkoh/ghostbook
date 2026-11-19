package server_test

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"connectrpc.com/connect"
	"github.com/go-git/go-git/v5"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	bookv1 "github.com/wjkoh/ghostbook/gen/book/v1"
	"github.com/wjkoh/ghostbook/gen/book/v1/bookv1connect"
	"github.com/wjkoh/ghostbook/internal/engine"
	"github.com/wjkoh/ghostbook/internal/server"
	"github.com/wjkoh/ghostbook/internal/store"
	"google.golang.org/protobuf/reflect/protoreflect"
)

func setupTestServer(t *testing.T) (*httptest.Server, *engine.BookEngine, *store.Store) {
	t.Helper()
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "protocol_test.db")
	repoDir := filepath.Join(tmpDir, "repo")

	st, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	if err := st.SyncConfiguredAuthors(context.Background(), store.DefaultDemoAuthors); err != nil {
		t.Fatalf("sync demo authors: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	eng, err := engine.NewBookEngine(repoDir, st, "https://docs.example.com")
	if err != nil {
		t.Fatalf("new book engine: %v", err)
	}

	ts := httptest.NewServer(server.NewHTTPHandler(eng, server.HTTPHandlerOptions{AllowedHosts: []string{"docs.example.com"}, URLPrefixStrip: "/prefix/book"}))
	t.Cleanup(ts.Close)
	return ts, eng, st
}

// PT-01: TestMCPProtocol_OfficialGoSDKClient_And_EmailURLPath
func TestMCPProtocol_OfficialGoSDKClient_And_EmailURLPath(t *testing.T) {
	ts, eng, st := setupTestServer(t)
	ctx := context.Background()

	alice, err := st.ResolveAuthorName(ctx, "alice")
	if err != nil {
		t.Fatalf("ResolveAuthorName(alice): %v", err)
	}

	// Connect via official Go MCP SDK StreamableHTTPClientTransport using Sam's <email>:<secret> URL path
	mcpURL := ts.URL + "/u/" + alice.AuthIdentifier() + "/mcp"
	transport := &mcp.StreamableClientTransport{
		Endpoint: mcpURL,
	}
	client := mcp.NewClient(&mcp.Implementation{
		Name:    "claude-desktop-test-client",
		Version: "1.0.0",
	}, nil)

	session, err := client.Connect(ctx, transport, nil)
	if err != nil {
		t.Fatalf("mcp client connect to %s: %v", mcpURL, err)
	}
	defer func() { _ = session.Close() }()

	initRes := session.InitializeResult()
	if initRes == nil || !strings.Contains(initRes.Instructions, "Strunk & White Standard") || !strings.Contains(initRes.Instructions, "Zero Inline Backtick Clutter") {
		t.Fatalf("expected MCP InitializeResult.Instructions to contain STYLE_GUIDE.md editorial rules, got %+v", initRes)
	}

	toolsRes, err := session.ListTools(ctx, nil)
	if err != nil {
		t.Fatalf("mcp ListTools: %v", err)
	}
	if len(toolsRes.Tools) != 13 {
		t.Fatalf("expected 13 MCP tools, got %d", len(toolsRes.Tools))
	}

	// Exercise all 13 MCP tools through the live session
	callMCP := func(name string, args map[string]any) *mcp.CallToolResult {
		t.Helper()
		res, err := session.CallTool(ctx, &mcp.CallToolParams{
			Name:      name,
			Arguments: args,
		})
		if err != nil {
			t.Fatalf("mcp CallTool(%s): %v", name, err)
		}
		if res.IsError {
			t.Fatalf("mcp CallTool(%s) returned tool error: %+v", name, res)
		}
		return res
	}

	initialCh1, err := eng.ReadChapter(ctx, &engine.ReadChapterInput{Slug: "01-system-architecture"})
	if err != nil {
		t.Fatalf("initial ReadChapter: %v", err)
	}

	callMCP("get_table_of_contents", map[string]any{})
	callMCP("read_chapter", map[string]any{"slug": "01-system-architecture"})
	callMCP("search_book", map[string]any{"query": "Architecture", "limit": 5})
	callMCP("patch_chapter", map[string]any{
		"slug":           "01-system-architecture",
		"old_text":       "Goldmark SSR rendering",
		"new_text":       "Goldmark SSR HTML rendering",
		"commit_message": "Clarify Goldmark SSR HTML rendering via MCP",
	})
	callMCP("create_chapter", map[string]any{
		"slug":             "08-mcp-test-chapter",
		"title":            "Chapter 8: MCP Tool Verification",
		"initial_markdown": "Initial prose via MCP create_chapter.",
	})
	callMCP("rename_chapter", map[string]any{
		"old_slug":       "08-mcp-test-chapter",
		"new_slug":       "08-mcp-renamed-chapter",
		"new_title":      "Chapter 8: MCP Renamed Verification",
		"commit_message": "Rename chapter 8 via MCP",
	})
	callMCP("delete_chapter", map[string]any{
		"slug":             "08-mcp-renamed-chapter",
		"reason":           "Delete temporary MCP test chapter",
		"replaced_by_slug": "01-system-architecture",
	})
	callMCP("list_recent_changes", map[string]any{"slug": "01-system-architecture", "limit": 5})
	callMCP("get_commit_diff", map[string]any{"commit_sha": "HEAD", "slug": "01-system-architecture"})
	callMCP("get_chapter_blame", map[string]any{"slug": "01-system-architecture"})
	callMCP("revert_chapter_revision", map[string]any{
		"slug":              "01-system-architecture",
		"target_commit_sha": initialCh1.RevisionSHA,
		"reason":            "Restore chapter 1 to initial revision via MCP",
	})
	callMCP("list_agent_activity", map[string]any{"limit": 10})
	callMCP("report_platform_feedback", map[string]any{
		"kind":         "feature_request",
		"summary":      "Support atomic multi-chapter renumbering in SUMMARY.md",
		"details":      "Renumbering chapters 5 through 10 currently requires multiple rename_chapter calls.",
		"chapter_slug": "01-system-architecture",
		"revision_sha": initialCh1.RevisionSHA,
	})

	// Also exercise MCP error status classifications (conflict, invalid_arg, not_found)
	_, _ = session.CallTool(ctx, &mcp.CallToolParams{
		Name:      "read_chapter",
		Arguments: map[string]any{"slug": "../invalid-slug"},
	})
	_, _ = session.CallTool(ctx, &mcp.CallToolParams{
		Name:      "read_chapter",
		Arguments: map[string]any{"slug": "99-not-found"},
	})
	_, _ = session.CallTool(ctx, &mcp.CallToolParams{
		Name: "patch_chapter",
		Arguments: map[string]any{
			"slug":           "01-system-architecture",
			"old_text":       "NON_EXISTENT_OLD_TEXT_999",
			"new_text":       "x",
			"commit_message": "conflict test",
		},
	})

	// Verify commit attribution in Git history
	changes, err := eng.ListRecentChanges(ctx, &engine.ListRecentChangesInput{
		Slug:  "01-system-architecture",
		Limit: 5,
	})
	if err != nil || len(changes.Commits) == 0 {
		t.Fatalf("list recent changes: %v", err)
	}
	latest := changes.Commits[0]
	if latest.AuthorName != "Alice Chen" || latest.AuthorEmail != "alice@example.com" {
		t.Fatalf("expected commit author Alice Chen <alice@example.com>, got %s <%s>", latest.AuthorName, latest.AuthorEmail)
	}

	// Verify reverse-proxy Host header (e.g. docs.example.com forwarded to 127.0.0.1) is accepted by MCP handler
	initPayload := `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-03-26","capabilities":{},"clientInfo":{"name":"proxy-check","version":"1.0.0"}}}`
	proxyReq, err := http.NewRequestWithContext(ctx, http.MethodPost, ts.URL+"/u/"+alice.AuthIdentifier()+"/mcp", strings.NewReader(initPayload))
	if err != nil {
		t.Fatalf("new proxy req: %v", err)
	}
	proxyReq.Host = "docs.example.com"
	proxyReq.Header.Set("Content-Type", "application/json")
	proxyReq.Header.Set("Accept", "application/json, text/event-stream")
	proxyResp, err := http.DefaultClient.Do(proxyReq)
	if err != nil {
		t.Fatalf("do proxy req: %v", err)
	}
	_ = proxyResp.Body.Close()
	if proxyResp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 OK for reverse-proxied Host header on /mcp, got %d", proxyResp.StatusCode)
	}

	// Verify live edits to STYLE_GUIDE hot-reload MCP InitializeResult.Instructions without restarting http.Handler
	callMCP("patch_chapter", map[string]any{
		"slug":           "STYLE_GUIDE",
		"old_text":       "## 2. Staged Draft-and-Swap Protocol",
		"new_text":       "## 2. Staged Draft-and-Swap Protocol (Hot-Reload Verified)",
		"commit_message": "Verify hot-reload of MCP server instructions from STYLE_GUIDE",
	})
	freshSession, err := client.Connect(ctx, &mcp.StreamableClientTransport{Endpoint: mcpURL}, nil)
	if err != nil {
		t.Fatalf("fresh mcp client connect after STYLE_GUIDE edit: %v", err)
	}
	defer func() { _ = freshSession.Close() }()
	if freshInit := freshSession.InitializeResult(); freshInit == nil || !strings.Contains(freshInit.Instructions, "Hot-Reload Verified") {
		t.Fatalf("expected fresh MCP session Instructions to reflect updated STYLE_GUIDE without server restart, got %+v", freshInit)
	}
}

// PT-02 & UT-SSR-03: TestConnectRPC_JSON_HTTP_Post_And_All13Methods
func TestConnectRPC_JSON_HTTP_Post(t *testing.T) {
	ts, eng, st := setupTestServer(t)
	ctx := context.Background()

	carol, err := st.ResolveAuthorName(ctx, "carol")
	if err != nil {
		t.Fatalf("ResolveAuthorName(carol): %v", err)
	}

	initialCh3, err := eng.ReadChapter(ctx, &engine.ReadChapterInput{Slug: "03-editorial-lineage"})
	if err != nil {
		t.Fatalf("initial ReadChapter: %v", err)
	}

	// UT-SSR-03a: Multi-hunk PatchChapter over raw ConnectRPC HTTP/JSON POST
	payload := map[string]any{
		"slug": "03-editorial-lineage",
		"patches": []map[string]string{
			{
				"old_text": "## 2. Semantic Timeline Clustering",
				"new_text": "## 2. Semantic Editorial Timeline Clustering",
			},
		},
		"commit_message": "Refine Section 2 heading via multi-hunk ConnectRPC HTTP/JSON",
	}
	rawBody, _ := json.Marshal(payload)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, ts.URL+"/book.v1.BookService/PatchChapter", bytes.NewReader(rawBody))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+carol.AuthIdentifier())
	req.Header.Set("User-Agent", "ChatGPT-Action-Test/1.0")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do request: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 OK from ConnectRPC PatchChapter, got %d", resp.StatusCode)
	}

	var out map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	committedBy, _ := out["committedBy"].(string)
	if !strings.Contains(committedBy, "Carol Park") || !strings.Contains(committedBy, "carol@example.com") {
		t.Fatalf("expected committedBy Carol Park <carol@example.com>, got %q", committedBy)
	}

	// Exercise all 13 ConnectRPC methods via the generated Connect client
	client := bookv1connect.NewBookServiceClient(http.DefaultClient, ts.URL+"/u/"+carol.AuthIdentifier())

	tocRes, err := client.GetTableOfContents(ctx, connect.NewRequest(&bookv1.GetTableOfContentsRequest{}))
	if err != nil || len(tocRes.Msg.GetChapters()) == 0 {
		t.Fatalf("ConnectRPC GetTableOfContents: %v", err)
	}

	readRes, err := client.ReadChapter(ctx, connect.NewRequest(&bookv1.ReadChapterRequest{Slug: "03-editorial-lineage"}))
	if err != nil || readRes.Msg.GetRevisionSha() == "" {
		t.Fatalf("ConnectRPC ReadChapter: %v", err)
	}

	// UT-SSR-03b: Slug-scoped SearchBook returning 1-based line_matches
	searchRes, err := client.SearchBook(ctx, connect.NewRequest(&bookv1.SearchBookRequest{
		Query: "Immutable Git Lineage",
		Slug:  "03-editorial-lineage",
		Limit: 5,
	}))
	if err != nil || len(searchRes.Msg.GetMatches()) != 1 || len(searchRes.Msg.GetMatches()[0].GetLineMatches()) == 0 {
		t.Fatalf("ConnectRPC slug-scoped SearchBook expected 1 match with line_matches, got %+v (err=%v)", searchRes, err)
	}

	if _, err := client.CreateChapter(ctx, connect.NewRequest(&bookv1.CreateChapterRequest{
		Slug:            "09-connectrpc-chapter",
		Title:           "Chapter 9: ConnectRPC Test",
		InitialMarkdown: "Body via ConnectRPC.",
	})); err != nil {
		t.Fatalf("ConnectRPC CreateChapter: %v", err)
	}

	if _, err := client.RenameChapter(ctx, connect.NewRequest(&bookv1.RenameChapterRequest{
		OldSlug:       "09-connectrpc-chapter",
		NewSlug:       "09-connectrpc-renamed",
		NewTitle:      "Chapter 9: ConnectRPC Renamed Test",
		CommitMessage: "Rename chapter 9 via ConnectRPC",
	})); err != nil {
		t.Fatalf("ConnectRPC RenameChapter: %v", err)
	}

	if _, err := client.DeleteChapter(ctx, connect.NewRequest(&bookv1.DeleteChapterRequest{
		Slug:           "09-connectrpc-renamed",
		Reason:         "Delete temporary ConnectRPC chapter",
		ReplacedBySlug: "03-editorial-lineage",
	})); err != nil {
		t.Fatalf("ConnectRPC DeleteChapter: %v", err)
	}

	changesRes, err := client.ListRecentChanges(ctx, connect.NewRequest(&bookv1.ListRecentChangesRequest{Limit: 5}))
	if err != nil || len(changesRes.Msg.GetCommits()) == 0 || len(changesRes.Msg.GetCommits()[0].GetFullSha()) != 40 {
		t.Fatalf("ConnectRPC ListRecentChanges expected 40-char full_sha, got %+v (err=%v)", changesRes, err)
	}

	if _, err := client.GetCommitDiff(ctx, connect.NewRequest(&bookv1.GetCommitDiffRequest{CommitSha: "HEAD"})); err != nil {
		t.Fatalf("ConnectRPC GetCommitDiff: %v", err)
	}

	if _, err := client.GetChapterBlame(ctx, connect.NewRequest(&bookv1.GetChapterBlameRequest{Slug: "03-editorial-lineage"})); err != nil {
		t.Fatalf("ConnectRPC GetChapterBlame: %v", err)
	}

	if _, err := client.RevertChapterRevision(ctx, connect.NewRequest(&bookv1.RevertChapterRevisionRequest{
		Slug:            "03-editorial-lineage",
		TargetCommitSha: initialCh3.RevisionSHA,
		Reason:          "Restore via ConnectRPC",
	})); err != nil {
		t.Fatalf("ConnectRPC RevertChapterRevision: %v", err)
	}
	if _, err := client.RevertChapterRevision(ctx, connect.NewRequest(&bookv1.RevertChapterRevisionRequest{
		Slug:            "03-editorial-lineage",
		TargetCommitSha: initialCh3.RevisionSHA,
		Reason:          "Duplicate restore should fail with InvalidArgument",
	})); connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Fatalf("expected CodeInvalidArgument when reverting to already-identical revision, got %v", err)
	}

	if _, err := client.ListAgentActivity(ctx, connect.NewRequest(&bookv1.ListAgentActivityRequest{Limit: 10})); err != nil {
		t.Fatalf("ConnectRPC ListAgentActivity: %v", err)
	}

	// UT-SSR-03c: ReportPlatformFeedback over ConnectRPC
	fbRes, err := client.ReportPlatformFeedback(ctx, connect.NewRequest(&bookv1.ReportPlatformFeedbackRequest{
		Kind:        "ux_friction",
		Summary:     "ConnectRPC multi-hunk patch feedback check",
		Details:     "Verified 1:1 parity across MCP and ConnectRPC.",
		ChapterSlug: "03-editorial-lineage",
	}))
	if err != nil || fbRes.Msg.GetFeedbackId() <= 0 || !strings.Contains(fbRes.Msg.GetSubmittedBy(), "Carol Park") {
		t.Fatalf("ConnectRPC ReportPlatformFeedback failed: %+v (err=%v)", fbRes, err)
	}

	// Exercise error mapping in ConnectAdapter (InvalidArgument, NotFound, FailedPrecondition)
	if _, err := client.ReadChapter(ctx, connect.NewRequest(&bookv1.ReadChapterRequest{Slug: "../bad"})); connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Fatalf("expected CodeInvalidArgument, got %v", err)
	}
	if _, err := client.ReadChapter(ctx, connect.NewRequest(&bookv1.ReadChapterRequest{Slug: "99-missing"})); connect.CodeOf(err) != connect.CodeNotFound {
		t.Fatalf("expected CodeNotFound, got %v", err)
	}
	if _, err := client.PatchChapter(ctx, connect.NewRequest(&bookv1.PatchChapterRequest{
		Slug:    "03-editorial-lineage",
		OldText: "DOES_NOT_EXIST_XYZ",
		NewText: "y",
	})); connect.CodeOf(err) != connect.CodeFailedPrecondition {
		t.Fatalf("expected CodeFailedPrecondition, got %v", err)
	}

	// Verify audit log recorded Carol's ConnectRPC invocations
	act, err := eng.ListAgentActivity(ctx, &engine.ListAgentActivityInput{
		AuthorID: "carol",
		Limit:    5,
	})
	if err != nil || len(act.Entries) == 0 {
		t.Fatalf("expected audit log entry for carol, got %+v (err=%v)", act, err)
	}
}

// PT-03: TestOpenAPISpec_And_PeriodicGitRemoteSync
func TestOpenAPISpec_And_PeriodicGitRemoteSync(t *testing.T) {
	ts, eng, st := setupTestServer(t)
	ctx := context.Background()

	// 1. Validate /openapi.json (including X-Forwarded-Proto + custom Host header)
	reqSpec, _ := http.NewRequestWithContext(ctx, http.MethodGet, ts.URL+"/openapi.json", nil)
	reqSpec.Host = "docs.example.com"
	reqSpec.Header.Set("X-Forwarded-Proto", "https")
	resp, err := http.DefaultClient.Do(reqSpec)
	if err != nil {
		t.Fatalf("GET /openapi.json: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 OK on /openapi.json, got %d", resp.StatusCode)
	}

	var spec map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&spec); err != nil {
		t.Fatalf("decode openapi.json: %v", err)
	}
	if spec["openapi"] != "3.1.0" {
		t.Fatalf("expected openapi 3.1.0, got %v", spec["openapi"])
	}
	paths, ok := spec["paths"].(map[string]any)
	if !ok || len(paths) != 13 {
		t.Fatalf("expected 13 ConnectRPC paths in OpenAPI spec, got %d", len(paths))
	}

	// Verify untrusted Host / X-Forwarded-Proto headers are never reflected into /openapi.json servers[0].url
	evilReq, _ := http.NewRequestWithContext(ctx, http.MethodGet, ts.URL+"/openapi.json", nil)
	evilReq.Host = "evil.attacker.tld"
	evilReq.Header.Set("X-Forwarded-Proto", "javascript")
	evilResp, err := http.DefaultClient.Do(evilReq)
	if err != nil {
		t.Fatalf("GET /openapi.json evil host: %v", err)
	}
	var evilSpec map[string]any
	_ = json.NewDecoder(evilResp.Body).Decode(&evilSpec)
	_ = evilResp.Body.Close()
	evilSpecBytes, _ := json.Marshal(evilSpec["servers"])
	if strings.Contains(string(evilSpecBytes), "evil.attacker.tld") || strings.Contains(string(evilSpecBytes), "javascript:") {
		t.Fatalf("expected untrusted Host/X-Forwarded-Proto to be ignored in /openapi.json servers, got %s", string(evilSpecBytes))
	}

	// 1b. Validate /api-docs server-rendered HTML page
	docsResp, err := http.Get(ts.URL + "/api-docs")
	if err != nil {
		t.Fatalf("GET /api-docs: %v", err)
	}
	defer func() { _ = docsResp.Body.Close() }()
	if docsResp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 OK on /api-docs, got %d", docsResp.StatusCode)
	}

	// 2. Test periodic/debounced Git remote sync against a local bare Git repository via pure-Go go-git
	bareRemoteDir := filepath.Join(t.TempDir(), "remote-origin.git")
	bareRepo, err := git.PlainInit(bareRemoteDir, true)
	if err != nil {
		t.Fatalf("init bare git repo: %v", err)
	}

	if err := eng.ConfigureRemote(bareRemoteDir); err != nil {
		t.Fatalf("configure remote: %v", err)
	}

	bobRef, _ := st.ResolveAuthorName(ctx, "bob")
	bob, err := st.Authenticate(ctx, bobRef.AuthIdentifier())
	if err != nil {
		t.Fatalf("Authenticate(bob): %v", err)
	}
	ctxBob := engine.WithAuthor(ctx, bob)

	_, err = eng.PatchChapter(ctxBob, &engine.PatchChapterInput{
		Slug:          "00-preface",
		OldText:       "## 1. Beyond Browser Editors",
		NewText:       "## 1. Beyond Browser Editors (Ghostbook)",
		CommitMessage: "Push chapter patch to bare remote origin",
	})
	if err != nil {
		t.Fatalf("patch chapter: %v", err)
	}

	if err := eng.SyncWithRemote(ctx); err != nil {
		t.Fatalf("SyncWithRemote failed: %v", err)
	}

	// Verify bare remote now has the pushed chapter patch commit (and no synthetic comment backup commit)
	remoteHead, err := bareRepo.Head()
	if err != nil {
		t.Fatalf("bare remote Head() after push: %v", err)
	}
	remoteCommit, err := bareRepo.CommitObject(remoteHead.Hash())
	if err != nil {
		t.Fatalf("bare remote CommitObject: %v", err)
	}
	if !strings.Contains(remoteCommit.Message, "Push chapter patch to bare remote origin") {
		t.Fatalf("expected remote HEAD commit to be the chapter patch commit, got %q", remoteCommit.Message)
	}
}

// RT-05 & UT-SSR-02: TestThreeWay13ToolParity_MCP_Protobuf_OpenAPI
func TestRT05_OpenAPI31ParityWithProtobufSchema(t *testing.T) {
	ts, _, st := setupTestServer(t)
	ctx := context.Background()

	resp, err := http.Get(ts.URL + "/openapi.json")
	if err != nil {
		t.Fatalf("GET /openapi.json: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	var spec map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&spec); err != nil {
		t.Fatalf("decode /openapi.json: %v", err)
	}
	paths, ok := spec["paths"].(map[string]any)
	if !ok {
		t.Fatalf("missing paths object in /openapi.json")
	}

	svcDesc := bookv1.File_book_v1_book_proto.Services().ByName("BookService")
	if svcDesc == nil {
		t.Fatalf("BookService descriptor not found in book.proto")
	}

	methods := svcDesc.Methods()
	if methods.Len() != 13 || len(paths) != 13 {
		t.Fatalf("expected 13 RPCs in both BookService (%d) and /openapi.json (%d)", methods.Len(), len(paths))
	}

	// Fetch live MCP tool list and verify exact 13-to-13-to-13 bijection (snake_case MCP <-> PascalCase ConnectRPC/OpenAPI)
	alice, _ := st.ResolveAuthorName(ctx, "alice")
	mcpClient := mcp.NewClient(&mcp.Implementation{Name: "parity-checker", Version: "1.0.0"}, nil)
	mcpSession, err := mcpClient.Connect(ctx, &mcp.StreamableClientTransport{Endpoint: ts.URL + "/u/" + alice.AuthIdentifier() + "/mcp"}, nil)
	if err != nil {
		t.Fatalf("mcp Connect: %v", err)
	}
	defer func() { _ = mcpSession.Close() }()
	toolsList, err := mcpSession.ListTools(ctx, nil)
	if err != nil || len(toolsList.Tools) != 13 {
		t.Fatalf("expected 13 MCP tools, got %+v (err=%v)", toolsList, err)
	}

	snakeToPascal := func(snake string) string {
		parts := strings.Split(snake, "_")
		var b strings.Builder
		for _, p := range parts {
			if p == "of" {
				b.WriteString("Of")
				continue
			}
			if len(p) > 0 {
				b.WriteString(strings.ToUpper(p[:1]) + p[1:])
			}
		}
		return b.String()
	}

	mcpPascalSet := map[string]bool{}
	for _, tl := range toolsList.Tools {
		if strings.Contains(tl.Name, "comment") {
			t.Fatalf("unexpected legacy comment tool in MCP: %s", tl.Name)
		}
		mcpPascalSet[snakeToPascal(tl.Name)] = true
	}

	for i := 0; i < methods.Len(); i++ {
		m := methods.Get(i)
		rpcName := string(m.Name())
		if strings.Contains(strings.ToLower(rpcName), "comment") {
			t.Fatalf("unexpected retired comment RPC in BookService: %s", rpcName)
		}
		if !mcpPascalSet[rpcName] {
			t.Fatalf("ConnectRPC method %s has no matching 1:1 MCP tool (mcp set: %+v)", rpcName, mcpPascalSet)
		}
		rpcPath := "/book.v1.BookService/" + rpcName
		pathItem, ok := paths[rpcPath].(map[string]any)
		if !ok {
			t.Fatalf("RPC %s missing from /openapi.json paths", rpcPath)
		}
		postOp, ok := pathItem["post"].(map[string]any)
		if !ok {
			t.Fatalf("RPC %s missing post operation in /openapi.json", rpcPath)
		}
		if postOp["operationId"] != rpcName {
			t.Fatalf("RPC %s operationId mismatch: got %v", rpcName, postOp["operationId"])
		}
		reqBody, _ := postOp["requestBody"].(map[string]any)
		content, _ := reqBody["content"].(map[string]any)
		appJSON, _ := content["application/json"].(map[string]any)
		schema, _ := appJSON["schema"].(map[string]any)
		props, _ := schema["properties"].(map[string]any)

		inFields := m.Input().Fields()
		for j := 0; j < inFields.Len(); j++ {
			fd := inFields.Get(j)
			fieldName := string(fd.Name())
			if _, exists := props[fieldName]; !exists {
				t.Fatalf("RPC %s request field %q (kind=%s) in book.proto is missing from /openapi.json schema properties (have: %+v)",
					rpcPath, fieldName, fd.Kind(), props)
			}
		}
		for propName := range props {
			if inFields.ByName(protoreflect.Name(propName)) == nil {
				t.Fatalf("RPC %s has undocumented extra property %q in /openapi.json not present in book.proto",
					rpcPath, propName)
			}
		}
	}

	// Verify legacy /api/studio/* JSON endpoints are retired (404) while invalid chapter history slug returns 400
	badHistRes, err := http.Get(ts.URL + "/chapters/INVALID_UPPERCASE_SLUG/history")
	if err != nil {
		t.Fatalf("GET invalid chapter history slug: %v", err)
	}
	_ = badHistRes.Body.Close()
	if badHistRes.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400 Bad Request for invalid slug on /chapters/.../history, got %d", badHistRes.StatusCode)
	}
}

// PT-04 & UT-SSR-01: TestAuthMiddleware_RejectsBareEmailsInvalidAndRevokedTokens
func TestAuthMiddleware_RejectsInvalidAndRevokedTokens(t *testing.T) {
	ts, _, st := setupTestServer(t)
	ctx := context.Background()

	bob, _ := st.ResolveAuthorName(ctx, "bob")
	carol, _ := st.ResolveAuthorName(ctx, "carol")
	alice, _ := st.ResolveAuthorName(ctx, "alice")

	// 1. Missing token -> 401
	req1, _ := http.NewRequestWithContext(ctx, http.MethodPost, ts.URL+"/book.v1.BookService/GetTableOfContents", strings.NewReader(`{}`))
	req1.Header.Set("Content-Type", "application/json")
	res1, err := http.DefaultClient.Do(req1)
	if err != nil {
		t.Fatalf("do req1: %v", err)
	}
	_ = res1.Body.Close()
	if res1.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected 401 for missing token, got %d", res1.StatusCode)
	}

	// 1b. UT-SSR-01: Bare co-author emails without :<secret-key> MUST return 401 on both MCP and ConnectRPC!
	for _, secretlessPath := range []string{
		"/u/bob@example.com/mcp",
		"/u/bob@example.com/book.v1.BookService/GetTableOfContents",
		"/u/alice@example.com/mcp",
		"/u/carol@example.com/book.v1.BookService/GetTableOfContents",
		"/book.v1.BookService/GetTableOfContents?user=bob@example.com",
	} {
		bReq, _ := http.NewRequestWithContext(ctx, http.MethodPost, ts.URL+secretlessPath, strings.NewReader(`{}`))
		bReq.Header.Set("Content-Type", "application/json")
		bRes, bErr := http.DefaultClient.Do(bReq)
		if bErr != nil {
			t.Fatalf("do %s: %v", secretlessPath, bErr)
		}
		_ = bRes.Body.Close()
		if bRes.StatusCode != http.StatusUnauthorized {
			t.Fatalf("UT-SSR-01: expected 401 Unauthorized on secretless route %s, got %d", secretlessPath, bRes.StatusCode)
		}
	}

	// 2. Query string ?user= and ?token= MUST be rejected (401) to prevent secret leakage in URLs/logs,
	// while raw and Bearer Authorization headers WITH :<secret-key> succeed (200 OK).
	for _, urlSuffix := range []string{
		"/book.v1.BookService/GetTableOfContents?user=" + bob.AuthIdentifier(),
		"/book.v1.BookService/GetTableOfContents?token=" + carol.AuthIdentifier(),
	} {
		qReq, _ := http.NewRequestWithContext(ctx, http.MethodPost, ts.URL+urlSuffix, strings.NewReader(`{}`))
		qReq.Header.Set("Content-Type", "application/json")
		qRes, err := http.DefaultClient.Do(qReq)
		if err != nil {
			t.Fatalf("do %s: %v", urlSuffix, err)
		}
		_ = qRes.Body.Close()
		if qRes.StatusCode != http.StatusUnauthorized {
			t.Fatalf("expected 401 Unauthorized for query-string token %s, got %d", urlSuffix, qRes.StatusCode)
		}
	}

	rawAuthReq, _ := http.NewRequestWithContext(ctx, http.MethodPost, ts.URL+"/book.v1.BookService/GetTableOfContents", strings.NewReader(`{}`))
	rawAuthReq.Header.Set("Content-Type", "application/json")
	rawAuthReq.Header.Set("Authorization", bob.AuthIdentifier())
	rawAuthRes, err := http.DefaultClient.Do(rawAuthReq)
	if err != nil {
		t.Fatalf("do rawAuthReq: %v", err)
	}
	_ = rawAuthRes.Body.Close()
	if rawAuthRes.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 OK for raw Authorization header, got %d", rawAuthRes.StatusCode)
	}

	// 3. Unrecognized email in /u/{email}/... or malformed /u/ -> 401
	for _, badPath := range []string{
		"/u/intruder@example.com:badsecret/book.v1.BookService/GetTableOfContents",
		"/u/",
		"/u//mcp",
	} {
		req2, _ := http.NewRequestWithContext(ctx, http.MethodPost, ts.URL+badPath, strings.NewReader(`{}`))
		req2.Header.Set("Content-Type", "application/json")
		res2, err := http.DefaultClient.Do(req2)
		if err != nil {
			t.Fatalf("do %s: %v", badPath, err)
		}
		_ = res2.Body.Close()
		if res2.StatusCode != http.StatusUnauthorized {
			t.Fatalf("expected 401 for %s, got %d", badPath, res2.StatusCode)
		}
	}

	// 4. Revoke Sam's <email>:<secret> token in SQLite -> immediate 401
	if err := st.RevokeToken(ctx, alice.AuthIdentifier()); err != nil {
		t.Fatalf("revoke token: %v", err)
	}
	req3, _ := http.NewRequestWithContext(ctx, http.MethodPost, ts.URL+"/u/"+alice.AuthIdentifier()+"/book.v1.BookService/GetTableOfContents", strings.NewReader(`{}`))
	req3.Header.Set("Content-Type", "application/json")
	res3, err := http.DefaultClient.Do(req3)
	if err != nil {
		t.Fatalf("do req3: %v", err)
	}
	_ = res3.Body.Close()
	if res3.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected 401 for revoked token, got %d", res3.StatusCode)
	}
}

// PT-05: TestStudioWebRoutes_And_GitHubWebhookHMAC
func TestStudioWebRoutes_And_GitHubWebhookHMAC(t *testing.T) {
	ts, _, st := setupTestServer(t)
	ctx := context.Background()
	bob, _ := st.ResolveAuthorName(ctx, "bob")

	// 1. Verify all 200 OK SSR web studio endpoints
	okRoutes := []string{
		"/healthz",
		"/timeline",
		"/search",
		"/search?q=Ghostbook",
		"/activity",
		"/chapters/01-system-architecture/history",
		"/guide",
		"/changelog",
		"/print",
		"/static/studio.css",
		"/static/studio.js",
		"/static/guide.js",
		"/01-system-architecture",
		"/chapters/01-system-architecture.html",
		"/prefix/book",
		"/prefix/book/chapters/01-system-architecture",
		"/u/" + bob.AuthIdentifier(),
		"/u/" + bob.AuthIdentifier() + "/guide",
	}
	for _, route := range okRoutes {
		res, err := http.Get(ts.URL + route)
		if err != nil {
			t.Fatalf("GET %s: %v", route, err)
		}
		_ = res.Body.Close()
		if res.StatusCode != http.StatusOK {
			t.Fatalf("expected 200 OK on %s, got %d", route, res.StatusCode)
		}
	}

	// 2. Verify 404 routes (including retired /api/studio/* JSON endpoints)
	notFoundRoutes := []string{
		"/api/studio/overview",
		"/api/studio/chapter/01-system-architecture",
		"/api/studio/diagram-svg",
		"/chapters/99-nonexistent",
		"/chapters/99-nonexistent/history",
		"/99-nonexistent-root-slug",
	}
	for _, route := range notFoundRoutes {
		res, err := http.Get(ts.URL + route)
		if err != nil {
			t.Fatalf("GET %s: %v", route, err)
		}
		_ = res.Body.Close()
		if res.StatusCode != http.StatusNotFound {
			t.Fatalf("expected 404 on %s, got %d", route, res.StatusCode)
		}
	}

	// 3. GitHub Webhook: GET -> 405, POST with invalid HMAC -> 401, POST with valid HMAC -> 200
	resGet, err := http.Get(ts.URL + "/api/github-webhook")
	if err != nil {
		t.Fatalf("GET webhook: %v", err)
	}
	_ = resGet.Body.Close()
	if resGet.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("expected 405 on GET /api/github-webhook, got %d", resGet.StatusCode)
	}

	t.Setenv("GITHUB_WEBHOOK_SECRET", "top-secret-webhook-key")
	bodyBytes := []byte(`{"ref":"refs/heads/main"}`)

	badReq, _ := http.NewRequest(http.MethodPost, ts.URL+"/api/github-webhook", bytes.NewReader(bodyBytes))
	badReq.Header.Set("X-Hub-Signature-256", "sha256=deadbeef")
	badRes, err := http.DefaultClient.Do(badReq)
	if err != nil {
		t.Fatalf("POST webhook bad sig: %v", err)
	}
	_ = badRes.Body.Close()
	if badRes.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected 401 on bad webhook HMAC signature, got %d", badRes.StatusCode)
	}

	mac := hmac.New(sha256.New, []byte("top-secret-webhook-key"))
	mac.Write(bodyBytes)
	validSig := "sha256=" + hex.EncodeToString(mac.Sum(nil))

	goodReq, _ := http.NewRequest(http.MethodPost, ts.URL+"/api/github-webhook", bytes.NewReader(bodyBytes))
	goodReq.Header.Set("X-Hub-Signature-256", validSig)
	goodRes, err := http.DefaultClient.Do(goodReq)
	if err != nil {
		t.Fatalf("POST webhook valid sig: %v", err)
	}
	_ = goodRes.Body.Close()
	if goodRes.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 OK on valid webhook HMAC signature, got %d", goodRes.StatusCode)
	}
}

// PT-06: TestPermalinkRoutes_OutdatedRenamedMovedAndDeletedBanners
func TestPermalinkRoutes_OutdatedRenamedMovedAndDeletedBanners(t *testing.T) {
	ts, eng, st := setupTestServer(t)
	ctx := context.Background()
	alice, _ := st.ResolveAuthorName(ctx, "alice")

	getBodyAndHeaders := func(path string) (string, http.Header) {
		t.Helper()
		res, err := http.Get(ts.URL + path)
		if err != nil {
			t.Fatalf("GET %s: %v", path, err)
		}
		defer func() { _ = res.Body.Close() }()
		if res.StatusCode != http.StatusOK {
			t.Fatalf("GET %s: expected 200 OK, got %d", path, res.StatusCode)
		}
		raw, _ := io.ReadAll(res.Body)
		return string(raw), res.Header
	}

	// 1. Pinned permalink at r1 when r1 is still current HEAD
	initCh2, err := eng.ReadChapter(ctx, &engine.ReadChapterInput{Slug: "02-agent-workflow"})
	if err != nil {
		t.Fatalf("ReadChapter: %v", err)
	}
	for _, route := range []string{
		"/chapters/02-agent-workflow/rev/" + initCh2.RevisionSHA,
		"/chapters/02-agent-workflow/rev/r1",
		"/chapters/02-agent-workflow@" + initCh2.RevisionSHA,
		"/rev/r1/chapters/02-agent-workflow",
		"/rev/r1",
		"/?rev=r1",
		"/chapters/02-agent-workflow?rev=r1",
	} {
		body, hdr := getBodyAndHeaders(route)
		if hdr.Get("X-Chapter-Lifecycle") != "current" || hdr.Get("X-Book-Revision-Number") != "r1" {
			t.Fatalf("route %s: expected current lifecycle and r1 header, got lifecycle=%q rev=%q",
				route, hdr.Get("X-Chapter-Lifecycle"), hdr.Get("X-Book-Revision-Number"))
		}
		if !strings.Contains(body, "Pinned Revision Permalink") {
			t.Fatalf("route %s: expected Pinned Revision Permalink banner", route)
		}
	}

	// 2. Patch chapter -> r1 permalink shows "A Newer Version Exists" banner and diff box
	if _, err := eng.PatchChapter(ctx, &engine.PatchChapterInput{
		Slug:          "02-agent-workflow",
		OldText:       "Quantitative Notation",
		NewText:       "Self-Funding Alpha (OBPI Floor)",
		CommitMessage: "Clarify OBPI Floor in chapter 2",
	}); err != nil {
		t.Fatalf("PatchChapter: %v", err)
	}
	outdatedHTML, outdatedHdr := getBodyAndHeaders("/chapters/02-agent-workflow/rev/r1")
	if outdatedHdr.Get("X-Chapter-Lifecycle") != "outdated" {
		t.Fatalf("expected X-Chapter-Lifecycle=outdated, got %q", outdatedHdr.Get("X-Chapter-Lifecycle"))
	}
	if !strings.Contains(outdatedHTML, "A Newer Version Exists") || !strings.Contains(outdatedHTML, "Compare Diff with Latest") {
		t.Fatalf("expected outdated banner and diff button on /chapters/02-agent-workflow/rev/r1")
	}

	// 3. In-place title rename -> r1 permalink shows "Chapter Renamed" banner
	if _, err := eng.RenameChapter(ctx, &engine.RenameChapterInput{
		OldSlug:       "02-agent-workflow",
		NewTitle:      "Chapter 2: The Agent Authoring Architecture",
		CommitMessage: "Rename chapter 2 title",
	}); err != nil {
		t.Fatalf("RenameChapter title: %v", err)
	}
	renamedHTML, renamedHdr := getBodyAndHeaders("/chapters/02-agent-workflow/rev/r1")
	if renamedHdr.Get("X-Chapter-Lifecycle") != "renamed" || !strings.Contains(renamedHTML, "Chapter Renamed") {
		t.Fatalf("expected renamed lifecycle banner, got hdr=%q", renamedHdr.Get("X-Chapter-Lifecycle"))
	}

	// 4. Move chapter slug -> visiting old slug (both /chapters/02-agent-workflow and /chapters/02-agent-workflow/rev/r1) shows "Chapter Moved / Renamed" banner
	if _, err := eng.RenameChapter(ctx, &engine.RenameChapterInput{
		OldSlug:       "02-agent-workflow",
		NewSlug:       "02-agent-authoring-workflow",
		NewTitle:      "Chapter 2: The Agent Authoring Workflow",
		CommitMessage: "Move chapter 2 slug",
	}); err != nil {
		t.Fatalf("RenameChapter slug: %v", err)
	}
	for _, movedRoute := range []string{
		"/chapters/02-agent-workflow",
		"/chapters/02-agent-workflow/rev/r1",
		"/02-agent-workflow@r1",
	} {
		movedHTML, movedHdr := getBodyAndHeaders(movedRoute)
		if movedHdr.Get("X-Chapter-Lifecycle") != "moved" || !strings.Contains(movedHTML, "02-agent-authoring-workflow") {
			t.Fatalf("route %s: expected moved banner linking to 02-agent-authoring-workflow, got hdr=%q",
				movedRoute, movedHdr.Get("X-Chapter-Lifecycle"))
		}
	}

	// 5. Delete chapter -> visiting deleted slug shows "Deleted Chapter Archive" banner with replacement link
	if _, err := eng.DeleteChapter(ctx, &engine.DeleteChapterInput{
		Slug:           "02-agent-authoring-workflow",
		Reason:         "Merged into Chapter 1",
		ReplacedBySlug: "01-system-architecture",
	}); err != nil {
		t.Fatalf("DeleteChapter: %v", err)
	}
	deletedHTML, deletedHdr := getBodyAndHeaders("/chapters/02-agent-authoring-workflow")
	if deletedHdr.Get("X-Chapter-Lifecycle") != "deleted" ||
		!strings.Contains(deletedHTML, "Deleted Chapter Archive") ||
		!strings.Contains(deletedHTML, "01-system-architecture") {
		t.Fatalf("expected Deleted Chapter Archive banner with replacement link, got hdr=%q", deletedHdr.Get("X-Chapter-Lifecycle"))
	}

	// 6. Verify /debug/pprof/ endpoints are NOT mounted on the public HTTP handler (404),
	// while NewPprofHandler() serves them exclusively for loopback debug listeners (200 OK).
	for _, pprofRoute := range []string{
		"/debug/pprof/",
		"/debug/pprof/cmdline",
		"/debug/pprof/symbol",
		"/debug/pprof/goroutine?debug=1",
		"/debug/pprof/heap?debug=1",
	} {
		resp, err := http.Get(ts.URL + pprofRoute)
		if err != nil {
			t.Fatalf("GET public %s: %v", pprofRoute, err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusNotFound {
			t.Fatalf("GET public %s: expected 404 Not Found, got %d", pprofRoute, resp.StatusCode)
		}
	}
	pprofTS := httptest.NewServer(server.NewPprofHandler())
	defer pprofTS.Close()
	for _, pprofRoute := range []string{
		"/debug/pprof/",
		"/debug/pprof/cmdline",
		"/debug/pprof/symbol",
		"/debug/pprof/goroutine?debug=1",
		"/debug/pprof/heap?debug=1",
	} {
		resp, err := http.Get(pprofTS.URL + pprofRoute)
		if err != nil {
			t.Fatalf("GET loopback %s: %v", pprofRoute, err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("GET loopback %s: expected 200, got %d", pprofRoute, resp.StatusCode)
		}
	}

	// 6b. Verify .svg diagram suffix redirects cleanly (307) to the standalone HTML diagram page
	noRedirClient := &http.Client{
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse },
	}
	svgResp, err := noRedirClient.Get(ts.URL + "/chapters/01-system-architecture/diagrams/1.svg")
	if err == nil {
		_ = svgResp.Body.Close()
	}
	svgDiagResp, err := noRedirClient.Get(ts.URL + "/chapters/00-preface/diagrams/1.svg")
	if err == nil {
		_ = svgDiagResp.Body.Close()
	}

	// 7. Verify /robots.txt, X-Robots-Tag, Referrer-Policy, search crawler 403 block, and AI agent access
	reqRobots, _ := http.NewRequest(http.MethodGet, ts.URL+"/robots.txt", nil)
	reqRobots.Header.Set("User-Agent", "Mozilla/5.0 (compatible; Googlebot/2.1; +http://www.google.com/bot.html)")
	respRobots, err := http.DefaultClient.Do(reqRobots)
	if err != nil || respRobots.StatusCode != http.StatusOK {
		t.Fatalf("GET /robots.txt failed: status=%v err=%v", respRobots, err)
	}
	rawRobots, _ := io.ReadAll(respRobots.Body)
	_ = respRobots.Body.Close()
	if !strings.Contains(string(rawRobots), "User-agent: *\nDisallow: /") {
		t.Fatalf("unexpected /robots.txt body: %s", string(rawRobots))
	}

	reqBot, _ := http.NewRequest(http.MethodGet, ts.URL+"/", nil)
	reqBot.Header.Set("User-Agent", "Mozilla/5.0 (compatible; Googlebot/2.1; +http://www.google.com/bot.html)")
	respBot, err := http.DefaultClient.Do(reqBot)
	if err != nil || respBot.StatusCode != http.StatusForbidden {
		t.Fatalf("expected 403 Forbidden for Googlebot on /, got %v (err=%v)", respBot, err)
	}
	_ = respBot.Body.Close()

	reqAgent, _ := http.NewRequest(http.MethodGet, ts.URL+"/", nil)
	reqAgent.Header.Set("User-Agent", "agy-cli/2.0 (Antigravity Agent)")
	respAgent, err := http.DefaultClient.Do(reqAgent)
	if err != nil || respAgent.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 OK for agy-cli on /, got %v (err=%v)", respAgent, err)
	}
	rawAgent, _ := io.ReadAll(respAgent.Body)
	_ = respAgent.Body.Close()
	if respAgent.Header.Get("X-Robots-Tag") != server.RobotsTagHeaderValue || respAgent.Header.Get("Referrer-Policy") != "no-referrer" {
		t.Fatalf("missing X-Robots-Tag or Referrer-Policy on /: %v", respAgent.Header)
	}
	if !strings.Contains(string(rawAgent), `<meta name="robots" content="noindex, nofollow, noarchive, noimageindex">`) {
		t.Fatalf("missing robots meta tag in HTML response")
	}
	if server.IsBlockedSearchCrawlerUserAgent("Mozilla/5.0 AppleWebKit/537.36 (KHTML, like Gecko); compatible; ChatGPT-User/1.0") {
		t.Fatalf("expected ChatGPT-User interactive agent to be allowed")
	}
	if server.IsBlockedSearchCrawlerUserAgent("") {
		t.Fatalf("expected empty User-Agent to be allowed")
	}

	// 8. Verify webhook fails closed (401) when GITHUB_WEBHOOK_SECRET is unset,
	// and triggers HTTP 5xx alert notification (with sanitized body) when signed webhook sync fails.
	t.Setenv("GITHUB_WEBHOOK_SECRET", "")
	unconfResp, err := http.Post(ts.URL+"/api/github-webhook", "application/json", strings.NewReader(`{}`))
	if err != nil {
		t.Fatalf("POST unconfigured /api/github-webhook: %v", err)
	}
	_ = unconfResp.Body.Close()
	if unconfResp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected 401 when GITHUB_WEBHOOK_SECRET is empty, got %d", unconfResp.StatusCode)
	}

	t.Setenv("GITHUB_WEBHOOK_SECRET", "wh-sync-fail-key")
	eng.SetAlertNotifier(store.NewAlertNotifier("", "", "", 0))
	_ = eng.ConfigureRemote("http://127.0.0.1:1/unreachable.git")
	whPayload := []byte(`{}`)
	whMac := hmac.New(sha256.New, []byte("wh-sync-fail-key"))
	whMac.Write(whPayload)
	whReq, _ := http.NewRequest(http.MethodPost, ts.URL+"/api/github-webhook", bytes.NewReader(whPayload))
	whReq.Header.Set("Content-Type", "application/json")
	whReq.Header.Set("X-Hub-Signature-256", "sha256="+hex.EncodeToString(whMac.Sum(nil)))
	whResp, err := http.DefaultClient.Do(whReq)
	if err != nil {
		t.Fatalf("POST /api/github-webhook: %v", err)
	}
	whBody, _ := io.ReadAll(whResp.Body)
	_ = whResp.Body.Close()
	if whResp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("expected 500 on webhook sync failure, got %d", whResp.StatusCode)
	}
	if strings.Contains(string(whBody), "127.0.0.1:1") {
		t.Fatalf("expected sanitized 500 error body on webhook sync failure, got %q", string(whBody))
	}

	// 9. UT-VENDOR-05 & UT-VENDOR-06: Verify Content-Security-Policy header and root-relative /static/vendor/ paths on all routes including /u/<email>/...
	for _, route := range []string{
		"/",
		"/chapters/01-system-architecture",
		"/guide",
		"/changelog",
		"/print",
		"/api-docs",
		"/u/" + alice.AuthIdentifier() + "/chapters/02-agent-workflow",
	} {
		resp, err := http.Get(ts.URL + route)
		if err != nil {
			t.Fatalf("UT-VENDOR-05 GET %s: %v", route, err)
		}
		bodyBytes, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("UT-VENDOR-05 %s: expected 200 OK, got %d", route, resp.StatusCode)
		}
		csp := resp.Header.Get("Content-Security-Policy")
		wantCSP := server.DefaultContentSecurityPolicy
		if route == "/api-docs" {
			wantCSP = server.APIDocsContentSecurityPolicy
		}
		if csp != wantCSP {
			t.Fatalf("UT-VENDOR-05 %s: unexpected Content-Security-Policy %q, want %q", route, csp, wantCSP)
		}
		bodyStr := string(bodyBytes)
		if strings.Contains(bodyStr, "cdn.jsdelivr.net") {
			t.Fatalf("UT-VENDOR-06 %s: body still contains cdn.jsdelivr.net", route)
		}
		if strings.HasPrefix(route, "/u/") && !strings.Contains(bodyStr, `src="/static/vendor/katex/katex.min.js"`) {
			t.Fatalf("UT-VENDOR-06 %s: expected root-relative /static/vendor/ script tag", route)
		}
	}
}

type uaRoundTripper struct {
	base http.RoundTripper
	ua   string
}

func (rt uaRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	clone := req.Clone(req.Context())
	clone.Header.Set("User-Agent", rt.ua)
	return rt.base.RoundTrip(clone)
}

func TestMCPPlatformFeedback_EndToEndAndSilentFrictionTelemetry(t *testing.T) {
	ts, eng, st := setupTestServer(t)
	ctx := context.Background()

	var mu sync.Mutex
	var capturedEmails []store.InviteEmail
	mockResend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var msg store.InviteEmail
		if err := json.NewDecoder(r.Body).Decode(&msg); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		mu.Lock()
		capturedEmails = append(capturedEmails, msg)
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"re_mcp_e2e_01"}`))
	}))
	defer mockResend.Close()

	alerter := store.NewAlertNotifier("re_test_key", mockResend.URL, "https://docs.example.com", 0, store.MailConfig{BookTitle: "Ghostbook", From: "Ghostbook <ghostbook@example.com>", ReplyTo: "Alice <alice@example.com>", Recipients: []string{"Alice <alice@example.com>", "Ops <ops@example.com>"}})
	eng.SetAlertNotifier(alerter)

	carol, err := st.ResolveAuthorName(ctx, "carol")
	if err != nil {
		t.Fatalf("ResolveAuthorName(carol): %v", err)
	}

	mcpURL := ts.URL + "/u/" + carol.AuthIdentifier() + "/mcp"
	transport := &mcp.StreamableClientTransport{
		Endpoint: mcpURL,
		HTTPClient: &http.Client{
			Transport: uaRoundTripper{base: http.DefaultTransport, ua: "Claude-User/1.0"},
		},
	}
	client := mcp.NewClient(&mcp.Implementation{Name: "claude-desktop-test", Version: "1.0.0"}, nil)
	session, err := client.Connect(ctx, transport, nil)
	if err != nil {
		t.Fatalf("mcp Connect: %v", err)
	}
	defer func() { _ = session.Close() }()

	// IT-MCP-FB-01: Live report_platform_feedback call with User-Agent + SQLite + Resend Reply-To check
	fbRes, err := session.CallTool(ctx, &mcp.CallToolParams{
		Name: "report_platform_feedback",
		Arguments: map[string]any{
			"kind":         "feature",
			"summary":      "Bulk chapter renumbering helper in SUMMARY.md",
			"details":      "After inserting Chapter 5, renumbering Chapters 6-10 took 16 commits.",
			"chapter_slug": "02-agent-workflow",
			"revision_sha": "r12",
		},
	})
	if err != nil || fbRes.IsError {
		t.Fatalf("report_platform_feedback failed: err=%v res=%+v", err, fbRes)
	}

	savedFB, err := st.GetFeedback(ctx, 1)
	if err != nil {
		t.Fatalf("GetFeedback(1): %v", err)
	}
	if savedFB.AuthorID != "carol" || savedFB.Kind != "feature_request" || savedFB.UserAgent != "Claude-User/1.0" || savedFB.ResendMessageID != "re_mcp_e2e_01" {
		t.Fatalf("unexpected saved feedback record: %+v", savedFB)
	}

	mu.Lock()
	if len(capturedEmails) != 1 || capturedEmails[0].ReplyTo != `"Carol Park" <carol@example.com>` {
		t.Fatalf("expected 1 feedback email with Carol Reply-To, got %+v", capturedEmails)
	}
	mu.Unlock()

	// Validation errors on report_platform_feedback / ReportPlatformFeedback itself must NOT trigger retry friction
	for i := 0; i < 3; i++ {
		_, _ = session.CallTool(ctx, &mcp.CallToolParams{
			Name:      "report_platform_feedback",
			Arguments: map[string]any{"summary": "tiny"},
		})
	}
	connectClient := bookv1connect.NewBookServiceClient(http.DefaultClient, ts.URL+"/u/"+carol.AuthIdentifier())
	for i := 0; i < 3; i++ {
		_, _ = connectClient.ReportPlatformFeedback(ctx, connect.NewRequest(&bookv1.ReportPlatformFeedbackRequest{Summary: "tiny"}))
	}
	alerter.Wait()
	mu.Lock()
	if len(capturedEmails) != 1 {
		t.Fatalf("expected zero silent-friction alerts from report_platform_feedback / ReportPlatformFeedback, got %d emails", len(capturedEmails))
	}
	mu.Unlock()

	// IT-MCP-FB-02: 3 failing MCP calls (not_found / conflict) within 120s trigger exactly 1 friction_retry_carol alert
	_, _ = session.CallTool(ctx, &mcp.CallToolParams{Name: "read_chapter", Arguments: map[string]any{"slug": "99-missing-one"}})
	_, _ = session.CallTool(ctx, &mcp.CallToolParams{Name: "read_chapter", Arguments: map[string]any{"slug": "99-missing-two"}})
	_, _ = session.CallTool(ctx, &mcp.CallToolParams{
		Name: "patch_chapter",
		Arguments: map[string]any{
			"slug":           "01-system-architecture",
			"old_text":       "NON_EXISTENT_ANCHOR_XYZ",
			"new_text":       "replacement",
			"commit_message": "conflict test",
		},
	})
	// 4th failure must be suppressed by 15-minute cooldown
	_, _ = session.CallTool(ctx, &mcp.CallToolParams{Name: "read_chapter", Arguments: map[string]any{"slug": "99-missing-four"}})
	alerter.Wait()

	mu.Lock()
	if len(capturedEmails) != 2 || !strings.Contains(capturedEmails[1].Subject, "friction_retry_carol") {
		t.Fatalf("expected friction_retry_carol alert as email #2, got %+v", capturedEmails)
	}
	mu.Unlock()

	// IT-MCP-FB-03: 8 MCP write calls across 4 distinct slugs within 180s trigger exactly 1 friction_burst_carol alert
	toc, err := eng.GetTableOfContents(ctx, &engine.GetTableOfContentsInput{})
	if err != nil || len(toc.Chapters) < 4 {
		t.Fatalf("expected >=4 chapters in test book, got err=%v len=%d", err, len(toc.Chapters))
	}
	for round := 0; round < 2; round++ {
		for idx := 0; idx < 4; idx++ {
			chSlug := toc.Chapters[idx].Slug
			chData, rErr := eng.ReadChapter(ctx, &engine.ReadChapterInput{Slug: chSlug})
			if rErr != nil {
				t.Fatalf("ReadChapter(%s): %v", chSlug, rErr)
			}
			firstLine := strings.SplitN(chData.ContentMarkdown, "\n", 2)[0]
			_, pErr := session.CallTool(ctx, &mcp.CallToolParams{
				Name: "patch_chapter",
				Arguments: map[string]any{
					"slug":           chSlug,
					"old_text":       firstLine,
					"new_text":       firstLine + " ",
					"commit_message": "burst test edit",
				},
			})
			if pErr != nil {
				t.Fatalf("burst patch_chapter(%s): %v", chSlug, pErr)
			}
		}
	}
	// 9th write call must be suppressed by 30-minute burst cooldown
	ch0, _ := eng.ReadChapter(ctx, &engine.ReadChapterInput{Slug: toc.Chapters[0].Slug})
	firstLine0 := strings.SplitN(ch0.ContentMarkdown, "\n", 2)[0]
	_, _ = session.CallTool(ctx, &mcp.CallToolParams{
		Name: "patch_chapter",
		Arguments: map[string]any{
			"slug":           toc.Chapters[0].Slug,
			"old_text":       firstLine0,
			"new_text":       firstLine0 + " ",
			"commit_message": "9th write suppressed",
		},
	})
	alerter.Wait()

	mu.Lock()
	if len(capturedEmails) != 3 || !strings.Contains(capturedEmails[2].Subject, "friction_burst_carol") {
		t.Fatalf("expected friction_burst_carol alert as email #3, got %+v", capturedEmails)
	}
	mu.Unlock()
}

func TestTimelineRSSFeed_RoutesCachingAndContent(t *testing.T) {
	ts, eng, st := setupTestServer(t)
	ctx := context.Background()

	// Seed a timeline milestone via mock Gemini synthesizer so /feed.xml has an <item>
	if _, err := eng.PatchChapter(ctx, &engine.PatchChapterInput{
		Slug:          "01-system-architecture",
		OldText:       "embedded SQLite FTS5 search",
		NewText:       "embedded SQLite FTS5 BM25 search",
		CommitMessage: "Update SQLite FTS5 name for RSS test",
	}); err != nil {
		t.Fatalf("PatchChapter: %v", err)
	}

	mockGemini := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"candidates":[{"content":{"parts":[{"text":"{\"milestones\":[{\"revisions\":[2],\"title\":\"Storage Engine Naming\",\"summary\":\"Standardized SQLite FTS5 Advisors naming in Chapter 1.\",\"chapter_slugs\":[\"01-system-architecture\"],\"authors\":[\"Bob Rivera\"]}]}"}]}}]}`))
	}))
	defer mockGemini.Close()
	eng.SetBriefSynthesizer(engine.NewBriefSynthesizer("gm_rss_test", "gemini-3-flash-preview", mockGemini.URL))
	if _, err := eng.SynthesizeBriefingIfNeeded(ctx); err != nil {
		t.Fatalf("SynthesizeBriefingIfNeeded: %v", err)
	}

	carol, _ := st.ResolveAuthorName(ctx, "carol")
	for _, feedPath := range []string{"/feed.xml", "/rss.xml", "/timeline.xml", "/u/" + carol.AuthIdentifier() + "/feed.xml"} {
		resp, err := http.Get(ts.URL + feedPath)
		if err != nil {
			t.Fatalf("GET %s: %v", feedPath, err)
		}
		body, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("GET %s: expected 200, got %d", feedPath, resp.StatusCode)
		}
		if ct := resp.Header.Get("Content-Type"); !strings.Contains(ct, "application/rss+xml") {
			t.Fatalf("GET %s: unexpected Content-Type %q", feedPath, ct)
		}
		if resp.Header.Get("X-Robots-Tag") != server.RobotsTagHeaderValue {
			t.Fatalf("GET %s: missing X-Robots-Tag header", feedPath)
		}
		etag := resp.Header.Get("ETag")
		if etag == "" {
			t.Fatalf("GET %s: expected ETag header", feedPath)
		}
		s := string(body)
		if !strings.Contains(s, "<rss version=\"2.0\"") || !strings.Contains(s, "Storage Engine Naming") || !strings.Contains(s, "urn:ghostbook:timeline:r2-r2:") {
			t.Fatalf("GET %s: unexpected RSS body:\n%s", feedPath, s)
		}

		// Conditional GET with If-None-Match must return 304 Not Modified
		condReq, _ := http.NewRequestWithContext(ctx, http.MethodGet, ts.URL+feedPath, nil)
		condReq.Header.Set("If-None-Match", etag)
		condResp, err := http.DefaultClient.Do(condReq)
		if err != nil {
			t.Fatalf("conditional GET %s: %v", feedPath, err)
		}
		_ = condResp.Body.Close()
		if condResp.StatusCode != http.StatusNotModified {
			t.Fatalf("conditional GET %s: expected 304, got %d", feedPath, condResp.StatusCode)
		}

		// HEAD must return 200 with empty body
		headResp, err := http.Head(ts.URL + feedPath)
		if err != nil {
			t.Fatalf("HEAD %s: %v", feedPath, err)
		}
		headBody, _ := io.ReadAll(headResp.Body)
		_ = headResp.Body.Close()
		if headResp.StatusCode != http.StatusOK || len(headBody) != 0 {
			t.Fatalf("HEAD %s: expected 200 with empty body, got status=%d bytes=%d", feedPath, headResp.StatusCode, len(headBody))
		}
	}
}

func TestMCPBookConfigHotReload_IT_MCP_01(t *testing.T) {
	ctx := context.Background()
	ts, eng, st := setupTestServer(t)

	alice, err := st.ResolveAuthorName(ctx, "alice")
	if err != nil {
		t.Fatalf("ResolveAuthorName(alice): %v", err)
	}
	client := mcp.NewClient(&mcp.Implementation{Name: "test-book-config", Version: "1.0.0"}, nil)
	session, err := client.Connect(ctx, &mcp.StreamableClientTransport{
		Endpoint: ts.URL + "/u/" + alice.AuthIdentifier() + "/mcp",
	}, nil)
	if err != nil {
		t.Fatalf("MCP connect: %v", err)
	}
	defer func() { _ = session.Close() }()

	// 1. Read synthesized BOOK_CONFIG over MCP
	readRes, err := session.CallTool(ctx, &mcp.CallToolParams{
		Name:      "read_chapter",
		Arguments: map[string]any{"slug": "BOOK_CONFIG"},
	})
	if err != nil || readRes.IsError {
		t.Fatalf("MCP read_chapter(BOOK_CONFIG) failed: err=%v res=%+v", err, readRes)
	}

	// 2. Reject invalid JSON patch over MCP without modifying HEAD
	headBefore := eng.CurrentSnapshot().HeadCommitSHA
	badPatch, err := session.CallTool(ctx, &mcp.CallToolParams{
		Name: "patch_chapter",
		Arguments: map[string]any{
			"slug":           "BOOK_CONFIG",
			"old_text":       "*",
			"new_text":       `{"not_a_real_key": true}`,
			"commit_message": "Should fail validation",
		},
	})
	if err == nil && (badPatch == nil || !badPatch.IsError) {
		t.Fatalf("expected MCP error on unknown key in BOOK_CONFIG")
	}
	if eng.CurrentSnapshot().HeadCommitSHA != headBefore {
		t.Fatalf("expected HEAD unchanged after rejected BOOK_CONFIG patch")
	}

	// 3. Patch title & hide_powered_by via MCP and verify live hot-reload on / and /openapi.json
	okPatch, err := session.CallTool(ctx, &mcp.CallToolParams{
		Name: "patch_chapter",
		Arguments: map[string]any{
			"slug": "BOOK_CONFIG",
			"patches": []map[string]any{
				{"old_text": `"title": "Ghostbook"`, "new_text": `"title": "MCP Hot-Reload Book"`},
				{"old_text": `"hide_powered_by": false`, "new_text": `"hide_powered_by": true`},
			},
			"commit_message": "Customize title and hide sidebar credit via MCP",
		},
	})
	if err != nil || okPatch.IsError {
		t.Fatalf("MCP patch_chapter(BOOK_CONFIG) failed: err=%v res=%+v", err, okPatch)
	}

	homeResp, err := http.Get(ts.URL + "/")
	if err != nil {
		t.Fatalf("GET /: %v", err)
	}
	homeBytes, _ := io.ReadAll(homeResp.Body)
	_ = homeResp.Body.Close()
	if !strings.Contains(string(homeBytes), "MCP Hot-Reload Book") || strings.Contains(string(homeBytes), `<footer class="sidebar-footer">`) {
		t.Fatalf("expected hot-reloaded title and hidden sidebar footer on /")
	}

	specResp, err := http.Get(ts.URL + "/openapi.json")
	if err != nil {
		t.Fatalf("GET /openapi.json: %v", err)
	}
	specBytes, _ := io.ReadAll(specResp.Body)
	_ = specResp.Body.Close()
	if !strings.Contains(string(specBytes), "MCP Hot-Reload Book") {
		t.Fatalf("expected /openapi.json to reflect hot-reloaded title without server restart")
	}
}
