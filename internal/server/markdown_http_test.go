package server_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"

	"github.com/wjkoh/ghostbook/internal/engine"
	"github.com/wjkoh/ghostbook/internal/server"
	"github.com/wjkoh/ghostbook/internal/store"
)

func fetchHTTP(t *testing.T, method, fullURL string, headers map[string]string) (int, string, http.Header) {
	t.Helper()
	req, err := http.NewRequest(method, fullURL, nil)
	if err != nil {
		t.Fatalf("new request %s %s: %v", method, fullURL, err)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do %s %s: %v", method, fullURL, err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body %s %s: %v", method, fullURL, err)
	}
	return resp.StatusCode, string(raw), resp.Header
}

func assertMarkdownHeaders(t *testing.T, label string, hdr http.Header) {
	t.Helper()
	if got := hdr.Get("Content-Type"); got != "text/markdown; charset=utf-8" {
		t.Fatalf("%s: Content-Type = %q, want text/markdown; charset=utf-8", label, got)
	}
	if got := hdr.Get("X-Content-Type-Options"); got != "nosniff" {
		t.Fatalf("%s: X-Content-Type-Options = %q, want nosniff", label, got)
	}
	if got := hdr.Get("Vary"); !strings.Contains(got, "Accept") {
		t.Fatalf("%s: Vary = %q, want Accept", label, got)
	}
	if got := hdr.Get("X-Robots-Tag"); got != server.RobotsTagHeaderValue {
		t.Fatalf("%s: X-Robots-Tag = %q, want %q", label, got, server.RobotsTagHeaderValue)
	}
	if got := hdr.Get("ETag"); !strings.HasPrefix(got, `"sha256-`) {
		t.Fatalf("%s: ETag = %q, want strong sha256 tag", label, got)
	}
	if got := hdr.Get("Link"); !strings.Contains(got, `</llms.txt>; rel="llms-txt"`) || !strings.Contains(got, `</llms-full.txt>; rel="llms-full-txt"`) {
		t.Fatalf("%s: missing discovery Link headers, got %q", label, got)
	}
}

func TestSuiteB_HTTPRawMarkdownAndPermalinks_UT_MD_HTTP_01_to_14(t *testing.T) {
	ctx := context.Background()

	t.Run("UT-MD-HTTP-01_LLMsTxtAndIndexEndpoints", func(t *testing.T) {
		ts, eng, _ := setupTestServer(t)

		status, body, hdr := fetchHTTP(t, http.MethodGet, ts.URL+"/llms.txt", nil)
		if status != http.StatusOK {
			t.Fatalf("GET /llms.txt status = %d", status)
		}
		assertMarkdownHeaders(t, "/llms.txt", hdr)
		if hdr.Get("X-Book-Revision-Number") != "r1" || hdr.Get("X-Book-Head-Revision-Number") != "r1" || hdr.Get("X-Chapter-Is-Latest") != "true" {
			t.Fatalf("unexpected /llms.txt revision headers: %v", hdr)
		}
		if !strings.Contains(body, "# Ghostbook") || !strings.Contains(body, "/chapters/01-system-architecture.md") {
			t.Fatalf("unexpected /llms.txt body:\n%s", body)
		}

		// /index.md must mirror /llms.txt
		statusIdx, bodyIdx, hdrIdx := fetchHTTP(t, http.MethodGet, ts.URL+"/index.md", nil)
		if statusIdx != http.StatusOK || bodyIdx != body {
			t.Fatalf("/index.md mismatch with /llms.txt (status=%d)", statusIdx)
		}
		assertMarkdownHeaders(t, "/index.md", hdrIdx)

		// Patch chapter -> r2, then verify /rev/r1/llms.txt and /llms.txt?rev=r1
		if _, err := eng.CreateChapter(ctx, &engine.CreateChapterInput{
			Slug:            "88-new-strategy",
			Title:           "Chapter 88: New Strategy",
			InitialMarkdown: "# Chapter 88: New Strategy\n\nBody.\n",
			CommitMessage:   "Add chapter 88",
		}); err != nil {
			t.Fatalf("CreateChapter: %v", err)
		}

		stHist, bodyHist, hdrHist := fetchHTTP(t, http.MethodGet, ts.URL+"/rev/r1/llms.txt", nil)
		if stHist != http.StatusOK || hdrHist.Get("X-Book-Revision-Number") != "r1" || hdrHist.Get("X-Book-Head-Revision-Number") != "r2" || hdrHist.Get("X-Chapter-Is-Latest") != "false" {
			t.Fatalf("unexpected /rev/r1/llms.txt response: status=%d hdr=%v", stHist, hdrHist)
		}
		if strings.Contains(bodyHist, "88-new-strategy") {
			t.Fatalf("/rev/r1/llms.txt leaked r2 chapter")
		}
		_, bodyQueryHist, _ := fetchHTTP(t, http.MethodGet, ts.URL+"/llms.txt?rev=r1", nil)
		if bodyQueryHist != bodyHist {
			t.Fatalf("expected /llms.txt?rev=r1 to match /rev/r1/llms.txt")
		}
	})

	t.Run("UT-MD-HTTP-02_FullBookRawMarkdownEndpoints", func(t *testing.T) {
		ts, eng, _ := setupTestServer(t)

		stFull, bodyFull, hdrFull := fetchHTTP(t, http.MethodGet, ts.URL+"/llms-full.txt", nil)
		if stFull != http.StatusOK {
			t.Fatalf("GET /llms-full.txt status = %d", stFull)
		}
		assertMarkdownHeaders(t, "/llms-full.txt", hdrFull)
		if strings.Contains(bodyFull, "<!DOCTYPE") || !strings.Contains(bodyFull, "# Ghostbook (Complete Raw Markdown Edition)") {
			t.Fatalf("unexpected /llms-full.txt body")
		}

		for _, alias := range []string{"/book.md", "/print.md"} {
			stA, bodyA, hdrA := fetchHTTP(t, http.MethodGet, ts.URL+alias, nil)
			if stA != http.StatusOK || bodyA != bodyFull {
				t.Fatalf("alias %s mismatch (status=%d)", alias, stA)
			}
			assertMarkdownHeaders(t, alias, hdrA)
		}

		if _, err := eng.PatchChapter(ctx, &engine.PatchChapterInput{
			Slug:          "01-system-architecture",
			OldText:       "Ghostbook bundles pure-Go",
			NewText:       "pure-Go Git Updated R2",
			CommitMessage: "Update ch1 in r2",
		}); err != nil {
			t.Fatalf("PatchChapter: %v", err)
		}

		for _, histRoute := range []string{"/rev/r1/book.md", "/rev/r1/llms-full.txt", "/book.md?rev=r1"} {
			stH, bodyH, hdrH := fetchHTTP(t, http.MethodGet, ts.URL+histRoute, nil)
			if stH != http.StatusOK || hdrH.Get("X-Book-Revision-Number") != "r1" || hdrH.Get("X-Chapter-Is-Latest") != "false" {
				t.Fatalf("%s unexpected status=%d hdr=%v", histRoute, stH, hdrH)
			}
			if !strings.Contains(bodyH, "Ghostbook bundles pure-Go") || strings.Contains(bodyH, "pure-Go Git Updated R2") {
				t.Fatalf("%s did not return exact r1 chapter content", histRoute)
			}
		}
	})

	t.Run("UT-MD-HTTP-03_MetaAndAuditMarkdownEndpoints", func(t *testing.T) {
		ts, _, _ := setupTestServer(t)

		for _, route := range []string{
			"/SUMMARY.md",
			"/summary.md",
			"/chapters/SUMMARY.md",
			"/rev/r1/SUMMARY.md",
			"/STYLE_GUIDE.md",
			"/style-guide.md",
			"/style_guide.md",
			"/chapters/STYLE_GUIDE.md",
			"/changelog.md",
			"/guide.md",
		} {
			st, body, hdr := fetchHTTP(t, http.MethodGet, ts.URL+route, nil)
			if st != http.StatusOK || len(body) < 40 || strings.Contains(body, "<!DOCTYPE") {
				t.Fatalf("route %s failed: status=%d len=%d", route, st, len(body))
			}
			assertMarkdownHeaders(t, route, hdr)
		}
	})

	t.Run("UT-MD-HTTP-04_LiveChapterExactByteForBytePatchCompatibility", func(t *testing.T) {
		ts, eng, _ := setupTestServer(t)

		readOut, err := eng.ReadChapter(ctx, &engine.ReadChapterInput{Slug: "01-system-architecture"})
		if err != nil {
			t.Fatalf("ReadChapter: %v", err)
		}

		for _, route := range []string{"/chapters/01-system-architecture.md", "/01-system-architecture.md"} {
			st, body, hdr := fetchHTTP(t, http.MethodGet, ts.URL+route, nil)
			if st != http.StatusOK {
				t.Fatalf("GET %s status=%d", route, st)
			}
			assertMarkdownHeaders(t, route, hdr)
			if body != readOut.ContentMarkdown {
				t.Fatalf("GET %s was not 100%% byte-for-byte identical to src/01-system-architecture.md:\ngot len=%d want len=%d", route, len(body), len(readOut.ContentMarkdown))
			}
			if hdr.Get("X-Chapter-Slug") != "01-system-architecture" || hdr.Get("X-Chapter-Lifecycle") != "current" || hdr.Get("X-Chapter-Is-Latest") != "true" {
				t.Fatalf("unexpected chapter provenance headers on %s: %v", route, hdr)
			}
		}

		// Verify multi-line slice from HTTP .md body works directly in PatchChapter old_text
		_, httpMD, _ := fetchHTTP(t, http.MethodGet, ts.URL+"/chapters/01-system-architecture.md", nil)
		lines := strings.Split(httpMD, "\n")
		oldSlice := strings.Join(lines[:3], "\n")
		newSlice := oldSlice + "\n\n<!-- Verified via HTTP .md exact bytes -->"
		if _, err := eng.PatchChapter(ctx, &engine.PatchChapterInput{
			Slug:          "01-system-architecture",
			OldText:       oldSlice,
			NewText:       newSlice,
			CommitMessage: "Patch directly using slice from HTTP .md",
		}); err != nil {
			t.Fatalf("PatchChapter using HTTP .md body failed: %v", err)
		}
	})

	t.Run("UT-MD-HTTP-05_All6PermalinkURLShapesParity", func(t *testing.T) {
		ts, eng, _ := setupTestServer(t)

		initCh, err := eng.ReadChapter(ctx, &engine.ReadChapterInput{Slug: "02-agent-workflow"})
		if err != nil {
			t.Fatalf("ReadChapter: %v", err)
		}
		sha := initCh.RevisionSHA

		shapes := []string{
			"/rev/" + sha + "/chapters/02-agent-workflow.md",
			"/rev/" + sha + "/02-agent-workflow.md",
			"/chapters/02-agent-workflow/rev/" + sha + ".md",
			"/chapters/02-agent-workflow/commits/" + sha + ".md",
			"/chapters/02-agent-workflow@" + sha + ".md",
			"/02-agent-workflow@" + sha + ".md",
			"/chapters/02-agent-workflow.md?rev=r1",
		}

		var refBody string
		for i, p := range shapes {
			st, body, hdr := fetchHTTP(t, http.MethodGet, ts.URL+p, nil)
			if st != http.StatusOK {
				t.Fatalf("shape %s returned status %d", p, st)
			}
			assertMarkdownHeaders(t, p, hdr)
			if hdr.Get("X-Chapter-Lifecycle") != "current" || hdr.Get("X-Book-Revision") != sha {
				t.Fatalf("shape %s unexpected headers: %v", p, hdr)
			}
			if i == 0 {
				refBody = body
			} else if body != refBody {
				t.Fatalf("shape %s body differed from %s", p, shapes[0])
			}
		}

		// Patch chapter 2 so r1 becomes outdated, and verify all 7 shapes still match identically
		if _, err := eng.PatchChapter(ctx, &engine.PatchChapterInput{
			Slug:          "02-agent-workflow",
			OldText:       "Quantitative Notation",
			NewText:       "Self-Funding Alpha (OBPI Institutional)",
			CommitMessage: "Advance ch2 to r2",
		}); err != nil {
			t.Fatalf("PatchChapter: %v", err)
		}

		var refOutdated string
		for i, p := range shapes {
			st, body, hdr := fetchHTTP(t, http.MethodGet, ts.URL+p, nil)
			if st != http.StatusOK || hdr.Get("X-Chapter-Lifecycle") != "outdated" || hdr.Get("X-Chapter-Is-Latest") != "false" {
				t.Fatalf("outdated shape %s failed: status=%d hdr=%v", p, st, hdr)
			}
			if i == 0 {
				refOutdated = body
			} else if body != refOutdated {
				t.Fatalf("outdated shape %s body differed from %s", p, shapes[0])
			}
		}
	})

	t.Run("UT-MD-HTTP-06_and_07_All5LifecycleStatesAndRawExact", func(t *testing.T) {
		ts, eng, _ := setupTestServer(t)

		origCh2, err := eng.ReadChapter(ctx, &engine.ReadChapterInput{Slug: "02-agent-workflow"})
		if err != nil {
			t.Fatalf("ReadChapter: %v", err)
		}
		origExact := origCh2.ContentMarkdown

		// 1. current (pinned)
		_, bodyPinned, hdrPinned := fetchHTTP(t, http.MethodGet, ts.URL+"/rev/r1/chapters/02-agent-workflow.md", nil)
		if hdrPinned.Get("X-Chapter-Lifecycle") != "current" || !strings.Contains(bodyPinned, "Pinned Revision Permalink (current)") {
			t.Fatalf("expected current pinned banner, got hdr=%v\n%s", hdrPinned, bodyPinned)
		}
		_, bodyPinnedExact, _ := fetchHTTP(t, http.MethodGet, ts.URL+"/rev/r1/chapters/02-agent-workflow.md?raw=exact", nil)
		if bodyPinnedExact != origExact {
			t.Fatalf("expected ?raw=exact on current pinned permalink to match exact r1 bytes")
		}

		// 2. outdated
		if _, err := eng.PatchChapter(ctx, &engine.PatchChapterInput{
			Slug:          "02-agent-workflow",
			OldText:       "Quantitative Notation",
			NewText:       "Self-Funding Alpha (OBPI v2)",
			CommitMessage: "Patch ch2",
		}); err != nil {
			t.Fatalf("PatchChapter: %v", err)
		}
		_, bodyOut, hdrOut := fetchHTTP(t, http.MethodGet, ts.URL+"/rev/r1/chapters/02-agent-workflow.md", nil)
		if hdrOut.Get("X-Chapter-Lifecycle") != "outdated" || !strings.Contains(bodyOut, "Permalink Revision Notice (outdated)") {
			t.Fatalf("expected outdated banner")
		}
		_, bodyOutExact, _ := fetchHTTP(t, http.MethodGet, ts.URL+"/rev/r1/chapters/02-agent-workflow.md?raw=exact", nil)
		if bodyOutExact != origExact {
			t.Fatalf("expected ?raw=exact on outdated permalink to match exact r1 bytes")
		}

		// 3. renamed
		if _, err := eng.RenameChapter(ctx, &engine.RenameChapterInput{
			OldSlug:       "02-agent-workflow",
			NewTitle:      "Chapter 2: Renamed Two-Bucket Architecture",
			CommitMessage: "Rename ch2 H1",
		}); err != nil {
			t.Fatalf("RenameChapter H1: %v", err)
		}
		_, bodyRen, hdrRen := fetchHTTP(t, http.MethodGet, ts.URL+"/rev/r1/chapters/02-agent-workflow.md", nil)
		if hdrRen.Get("X-Chapter-Lifecycle") != "renamed" || !strings.Contains(bodyRen, "Permalink Revision Notice (renamed)") {
			t.Fatalf("expected renamed banner")
		}

		// 4. moved
		if _, err := eng.RenameChapter(ctx, &engine.RenameChapterInput{
			OldSlug:       "02-agent-workflow",
			NewSlug:       "02-moved-agent-workflow",
			NewTitle:      "Chapter 2: Moved Two-Bucket Architecture",
			CommitMessage: "Move ch2 slug",
		}); err != nil {
			t.Fatalf("RenameChapter slug: %v", err)
		}
		for _, movedURL := range []string{"/chapters/02-agent-workflow.md", "/rev/r1/chapters/02-agent-workflow.md"} {
			_, bodyMov, hdrMov := fetchHTTP(t, http.MethodGet, ts.URL+movedURL, nil)
			if hdrMov.Get("X-Chapter-Lifecycle") != "moved" ||
				hdrMov.Get("X-Chapter-Latest-Markdown-URL") != "https://docs.example.com/chapters/02-moved-agent-workflow.md" ||
				!strings.Contains(bodyMov, "Permalink Revision Notice (moved)") {
				t.Fatalf("expected moved banner & header on %s: hdr=%v", movedURL, hdrMov)
			}
		}
		_, bodyMovExact, _ := fetchHTTP(t, http.MethodGet, ts.URL+"/rev/r1/chapters/02-agent-workflow.md?raw=exact", nil)
		if bodyMovExact != origExact {
			t.Fatalf("expected ?raw=exact on moved r1 permalink to match exact r1 bytes")
		}

		// 5. deleted
		if _, err := eng.DeleteChapter(ctx, &engine.DeleteChapterInput{
			Slug:           "02-moved-agent-workflow",
			Reason:         "Consolidated into Chapter 1",
			ReplacedBySlug: "01-system-architecture",
		}); err != nil {
			t.Fatalf("DeleteChapter: %v", err)
		}
		_, bodyDel, hdrDel := fetchHTTP(t, http.MethodGet, ts.URL+"/chapters/02-moved-agent-workflow.md", nil)
		if hdrDel.Get("X-Chapter-Lifecycle") != "deleted" ||
			hdrDel.Get("X-Chapter-Latest-Markdown-URL") != "https://docs.example.com/chapters/01-system-architecture.md" ||
			!strings.Contains(bodyDel, "Deleted Chapter Archive (deleted)") {
			t.Fatalf("expected deleted banner & replacement URL header: hdr=%v\n%s", hdrDel, bodyDel)
		}
		_, bodyDelExact, _ := fetchHTTP(t, http.MethodGet, ts.URL+"/chapters/02-moved-agent-workflow.md?raw=exact", nil)
		if strings.Contains(bodyDelExact, "Deleted Chapter Archive") {
			t.Fatalf("expected ?raw=exact on deleted chapter to omit lineage banner")
		}
	})

	t.Run("UT-MD-HTTP-08_ETagConditional304AndCacheControl", func(t *testing.T) {
		ts, _, _ := setupTestServer(t)

		st, body, hdr := fetchHTTP(t, http.MethodGet, ts.URL+"/chapters/01-system-architecture.md", nil)
		if st != http.StatusOK || hdr.Get("Cache-Control") != "no-cache" {
			t.Fatalf("expected 200 and Cache-Control=no-cache, got status=%d cc=%q", st, hdr.Get("Cache-Control"))
		}
		etag := hdr.Get("ETag")
		if etag == "" || len(body) == 0 {
			t.Fatalf("missing ETag or body")
		}

		st304, body304, _ := fetchHTTP(t, http.MethodGet, ts.URL+"/chapters/01-system-architecture.md", map[string]string{
			"If-None-Match": etag,
		})
		if st304 != http.StatusNotModified || body304 != "" {
			t.Fatalf("expected 304 Not Modified with empty body, got status=%d len=%d", st304, len(body304))
		}

		// Weak W/ prefix on If-None-Match also matches per RFC 9110
		stWeak, _, _ := fetchHTTP(t, http.MethodGet, ts.URL+"/chapters/01-system-architecture.md", map[string]string{
			"If-None-Match": "W/" + etag,
		})
		if stWeak != http.StatusNotModified {
			t.Fatalf("expected 304 for weak If-None-Match, got %d", stWeak)
		}

		// Annotated permalink uses Cache-Control: no-cache; ?raw=exact uses immutable
		_, _, hdrPermDefault := fetchHTTP(t, http.MethodGet, ts.URL+"/rev/r1/chapters/01-system-architecture.md", nil)
		if hdrPermDefault.Get("Cache-Control") != "no-cache" {
			t.Fatalf("expected Cache-Control=no-cache on default annotated permalink, got %q", hdrPermDefault.Get("Cache-Control"))
		}
		_, _, hdrPermExact := fetchHTTP(t, http.MethodGet, ts.URL+"/rev/r1/chapters/01-system-architecture.md?raw=exact", nil)
		if hdrPermExact.Get("Cache-Control") != "public, max-age=31536000, immutable" {
			t.Fatalf("expected immutable Cache-Control on ?raw=exact permalink, got %q", hdrPermExact.Get("Cache-Control"))
		}
	})

	t.Run("UT-MD-HTTP-09_HEADSupportAndExactContentLength", func(t *testing.T) {
		ts, _, _ := setupTestServer(t)

		for _, route := range []string{
			"/llms.txt",
			"/llms-full.txt",
			"/chapters/01-system-architecture.md",
			"/rev/r1/chapters/01-system-architecture.md",
		} {
			stGet, bodyGet, hdrGet := fetchHTTP(t, http.MethodGet, ts.URL+route, nil)
			stHead, bodyHead, hdrHead := fetchHTTP(t, http.MethodHead, ts.URL+route, nil)
			if stGet != http.StatusOK || stHead != http.StatusOK {
				t.Fatalf("%s GET=%d HEAD=%d", route, stGet, stHead)
			}
			if bodyHead != "" {
				t.Fatalf("%s HEAD returned non-empty body (%d bytes)", route, len(bodyHead))
			}
			wantLen := strconv.Itoa(len(bodyGet))
			if hdrHead.Get("Content-Length") != wantLen || hdrGet.Get("Content-Length") != wantLen {
				t.Fatalf("%s Content-Length mismatch: HEAD=%q GET=%q actual=%s", route, hdrHead.Get("Content-Length"), hdrGet.Get("Content-Length"), wantLen)
			}
			if hdrHead.Get("ETag") != hdrGet.Get("ETag") || hdrHead.Get("Content-Type") != hdrGet.Get("Content-Type") {
				t.Fatalf("%s HEAD headers drifted from GET", route)
			}
		}
	})

	t.Run("UT-MD-HTTP-10_RelativeMarkdownLinkResolutionAnd307Fallback", func(t *testing.T) {
		ts, _, _ := setupTestServer(t)

		relRef, _ := url.Parse("02-agent-workflow.md")

		for _, baseRoute := range []string{
			"/chapters/01-system-architecture.md",
			"/rev/r1/chapters/01-system-architecture.md",
			"/chapters/01-system-architecture/rev/r1.md",
		} {
			baseU, err := url.Parse(ts.URL + baseRoute)
			if err != nil {
				t.Fatalf("parse %s: %v", baseRoute, err)
			}
			resolved := baseU.ResolveReference(relRef).String()
			st, body, hdr := fetchHTTP(t, http.MethodGet, resolved, nil)
			if st != http.StatusOK || hdr.Get("X-Chapter-Slug") != "02-agent-workflow" || !strings.Contains(body, "Agent Authoring") {
				t.Fatalf("relative resolution from %s -> %s failed: status=%d slug=%q", baseRoute, resolved, st, hdr.Get("X-Chapter-Slug"))
			}
		}

		// Explicitly verify 307 Temporary Redirect status before redirect following on /chapters/01-system-architecture/rev/02-agent-workflow.md
		noFollow := &http.Client{
			CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
				return http.ErrUseLastResponse
			},
		}
		resp, err := noFollow.Get(ts.URL + "/chapters/01-system-architecture/rev/02-agent-workflow.md")
		if err != nil {
			t.Fatalf("noFollow GET: %v", err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusTemporaryRedirect || resp.Header.Get("Location") != "/chapters/02-agent-workflow.md" {
			t.Fatalf("expected 307 Temporary Redirect to /chapters/02-agent-workflow.md, got status=%d loc=%q", resp.StatusCode, resp.Header.Get("Location"))
		}
	})

	t.Run("UT-MD-HTTP-11_SecretRedactionHubPrefixAndZeroAuditWrites", func(t *testing.T) {
		ts, eng, st := setupTestServer(t)

		aliceRef, _ := st.ResolveAuthorName(ctx, "alice")
		samAuthor, err := st.Authenticate(ctx, aliceRef.AuthIdentifier())
		if err != nil {
			t.Fatalf("authenticate Sam: %v", err)
		}
		secretPrefix := "/u/alice@example.com:" + samAuthor.SecretKey

		actBefore, err := eng.ListAgentActivity(ctx, &engine.ListAgentActivityInput{Limit: 50})
		if err != nil {
			t.Fatalf("ListAgentActivity before: %v", err)
		}

		for _, route := range []string{
			secretPrefix + "/llms.txt",
			secretPrefix + "/llms-full.txt",
			secretPrefix + "/chapters/01-system-architecture.md",
			"/prefix/book/llms.txt",
			"/prefix/book/chapters/01-system-architecture.md",
		} {
			code, body, hdr := fetchHTTP(t, http.MethodGet, ts.URL+route, nil)
			if code != http.StatusOK {
				t.Fatalf("GET %s returned %d", route, code)
			}
			assertMarkdownHeaders(t, route, hdr)
			if samAuthor.SecretKey != "" && (strings.Contains(body, samAuthor.SecretKey) || strings.Contains(hdr.Get("Link"), samAuthor.SecretKey) || strings.Contains(hdr.Get("X-Chapter-Latest-Markdown-URL"), samAuthor.SecretKey)) {
				t.Fatalf("SECRET LEAKED on route %s!", route)
			}
			if strings.Contains(body, "/u/alice@example.com:") {
				t.Fatalf("authenticated /u/ secret prefix leaked into Markdown body on %s", route)
			}
		}

		actAfter, err := eng.ListAgentActivity(ctx, &engine.ListAgentActivityInput{Limit: 50})
		if err != nil {
			t.Fatalf("ListAgentActivity after: %v", err)
		}
		if len(actAfter.Entries) != len(actBefore.Entries) {
			t.Fatalf("passive HTTP .md reads wrote to SQLite agent_audit_log (before=%d after=%d), which would block 03:00 AM PT idle maintenance!", len(actBefore.Entries), len(actAfter.Entries))
		}
	})

	t.Run("UT-MD-HTTP-12_ErrorAndMethodContracts404_400_405", func(t *testing.T) {
		ts, _, _ := setupTestServer(t)

		for _, nf := range []string{
			"/chapters/99-does-not-exist.md",
			"/rev/deadbeef999/chapters/01-system-architecture.md",
			"/rev/deadbeef999/book.md",
			"/chapters/01-system-architecture/diagrams/99.mmd",
		} {
			st, _, _ := fetchHTTP(t, http.MethodGet, ts.URL+nf, nil)
			if st != http.StatusNotFound {
				t.Fatalf("expected 404 on %s, got %d", nf, st)
			}
		}

		for _, badSlug := range []string{
			"/chapters/bad$slug!.md",
			"/chapters/UPPERCASE_INVALID.md",
			"/bad$slug!.md",
		} {
			st, _, _ := fetchHTTP(t, http.MethodGet, ts.URL+badSlug, nil)
			if st != http.StatusBadRequest {
				t.Fatalf("expected 400 Bad Request on invalid slug %s, got %d", badSlug, st)
			}
		}

		for _, m := range []struct {
			method string
			path   string
		}{
			{http.MethodPost, "/llms.txt"},
			{http.MethodDelete, "/chapters/01-system-architecture.md"},
			{http.MethodPut, "/book.md"},
			{http.MethodPost, "/api-docs"},
		} {
			st, _, hdr := fetchHTTP(t, m.method, ts.URL+m.path, nil)
			if st != http.StatusMethodNotAllowed || hdr.Get("Allow") != "GET, HEAD" {
				t.Fatalf("expected 405 with Allow: GET, HEAD on %s %s, got status=%d allow=%q", m.method, m.path, st, hdr.Get("Allow"))
			}
		}
	})

	t.Run("UT-MD-HTTP-13_SearchCrawlerBlockAndInteractiveAgentAllowlist", func(t *testing.T) {
		ts, _, _ := setupTestServer(t)

		// Verify /robots.txt explicitly allows user-initiated fetchers before User-agent: * Disallow: /
		stRobots, robotsBody, _ := fetchHTTP(t, http.MethodGet, ts.URL+"/robots.txt", map[string]string{
			"User-Agent": "Mozilla/5.0 AppleWebKit/537.36 (KHTML, like Gecko; compatible; Claude-User/1.0; +Claude-User@anthropic.com)",
		})
		if stRobots != http.StatusOK {
			t.Fatalf("GET /robots.txt status=%d", stRobots)
		}
		wildcardIdx := strings.Index(robotsBody, "User-agent: *\nDisallow: /")
		if wildcardIdx < 0 {
			t.Fatalf("missing wildcard disallow in /robots.txt:\n%s", robotsBody)
		}
		for _, allowStanza := range []string{
			"User-agent: Claude-User\nAllow: /",
			"User-agent: Claude-Web\nAllow: /",
			"User-agent: ChatGPT-User\nAllow: /",
			"User-agent: OAI-Operator\nAllow: /",
			"User-agent: Perplexity-User\nAllow: /",
			"User-agent: Google-NotebookLM\nAllow: /",
			"User-agent: Google-CloudVertexBot\nAllow: /",
		} {
			idx := strings.Index(robotsBody, allowStanza)
			if idx < 0 || idx > wildcardIdx {
				t.Fatalf("missing or misplaced allow stanza %q in /robots.txt:\n%s", allowStanza, robotsBody)
			}
		}
		for _, disallowStanza := range []string{
			"User-agent: ClaudeBot\nDisallow: /",
			"User-agent: Claude-SearchBot\nDisallow: /",
			"User-agent: GPTBot\nDisallow: /",
			"User-agent: Googlebot\nDisallow: /",
		} {
			if !strings.Contains(robotsBody, disallowStanza) {
				t.Fatalf("missing disallow stanza %q in /robots.txt", disallowStanza)
			}
		}

		blockedUAs := []string{
			"Mozilla/5.0 (compatible; Googlebot/2.1; +http://www.google.com/bot.html)",
			"Mozilla/5.0 AppleWebKit/537.36 (KHTML, like Gecko; compatible; GPTBot/1.2; +https://openai.com/gptbot)",
			"Mozilla/5.0 AppleWebKit/537.36 (KHTML, like Gecko; compatible; ClaudeBot/1.0; +claudebot@anthropic.com)",
			"Mozilla/5.0 AppleWebKit/537.36 (KHTML, like Gecko; compatible; Claude-SearchBot/1.0; +searchbot@anthropic.com)",
			"OAI-SearchBot/1.0",
			"Mozilla/5.0 (compatible; PerplexityBot/1.0; +https://perplexity.ai/perplexitybot)",
			"CCBot/2.0",
		}
		for _, ua := range blockedUAs {
			for _, route := range []string{"/llms.txt", "/chapters/01-system-architecture.md"} {
				st, _, _ := fetchHTTP(t, http.MethodGet, ts.URL+route, map[string]string{"User-Agent": ua})
				if st != http.StatusForbidden {
					t.Fatalf("expected 403 Forbidden for crawler UA %q on %s, got %d", ua, route, st)
				}
			}
		}

		externalInteractiveUAs := []string{
			"Mozilla/5.0 AppleWebKit/537.36 (KHTML, like Gecko); compatible; ChatGPT-User/1.0",
			"Mozilla/5.0 AppleWebKit/537.36 (KHTML, like Gecko; compatible; Claude-User/1.0; +Claude-User@anthropic.com)",
			"Mozilla/5.0 (compatible; Claude-Web/1.0; +https://support.anthropic.com)",
			"Mozilla/5.0 (compatible; Perplexity-User/1.0)",
			"OAI-Operator/1.0",
			"Google-NotebookLM/1.0",
			"Google-CloudVertexBot/1.0",
			"MistralAI-User/1.0",
			"DuckAssistBot/1.0",
		}
		for _, ua := range externalInteractiveUAs {
			for _, route := range []string{"/llms.txt", "/chapters/01-system-architecture.md"} {
				st, _, hdr := fetchHTTP(t, http.MethodGet, ts.URL+route, map[string]string{"User-Agent": ua})
				if st != http.StatusOK {
					t.Fatalf("expected 200 OK for interactive agent UA %q on %s, got %d", ua, route, st)
				}
				rt := hdr.Get("X-Robots-Tag")
				if !strings.Contains(rt, "noindex") || strings.Contains(rt, "nosnippet") || strings.Contains(rt, "max-snippet:0") {
					t.Fatalf("expected noindex without nosnippet/max-snippet:0 for external fetcher %q, got X-Robots-Tag=%q", ua, rt)
				}
			}
		}

		generalAllowedUAs := []string{
			"agy-cli/2.0 (Antigravity)",
			"curl/8.7.1",
			"python-httpx/0.27.0",
			"Go-http-client/2.0",
			"",
		}
		for _, ua := range generalAllowedUAs {
			for _, route := range []string{"/llms.txt", "/chapters/01-system-architecture.md"} {
				st, _, _ := fetchHTTP(t, http.MethodGet, ts.URL+route, map[string]string{"User-Agent": ua})
				if st != http.StatusOK {
					t.Fatalf("expected 200 OK for interactive agent UA %q on %s, got %d", ua, route, st)
				}
			}
		}
	})

	t.Run("UT-MD-HTTP-14_MermaidDiagramRawSourceEndpoints", func(t *testing.T) {
		ts, _, _ := setupTestServer(t)

		for _, route := range []string{
			"/chapters/02-agent-workflow/diagrams/1.mmd",
			"/chapters/02-agent-workflow/diagrams/1.md",
			"/rev/r1/chapters/02-agent-workflow/diagrams/1.mmd",
			"/chapters/02-agent-workflow/rev/r1/diagrams/1.mmd",
		} {
			st, body, hdr := fetchHTTP(t, http.MethodGet, ts.URL+route, nil)
			if st != http.StatusOK {
				t.Fatalf("GET %s status=%d", route, st)
			}
			assertMarkdownHeaders(t, route, hdr)
			if strings.Contains(body, "<html") || strings.Contains(body, "<svg") || len(strings.TrimSpace(body)) < 10 {
				t.Fatalf("expected raw Mermaid source on %s, got:\n%s", route, body)
			}
		}
	})
}

func TestSuiteC_RFC9110ContentNegotiation_UT_MD_NEG_01_to_06(t *testing.T) {
	ts, _, _ := setupTestServer(t)

	t.Run("UT-MD-NEG-01_ExplicitTextMarkdownAndXMarkdown", func(t *testing.T) {
		for _, mime := range []string{"text/markdown", "text/x-markdown"} {
			for _, route := range []string{
				"/",
				"/rev/r1",
				"/print",
				"/guide",
				"/changelog",
				"/api-docs",
				"/chapters/01-system-architecture",
				"/01-system-architecture",
				"/chapters/01-system-architecture/rev/r1",
				"/chapters/02-agent-workflow/diagrams/1",
			} {
				st, body, hdr := fetchHTTP(t, http.MethodGet, ts.URL+route, map[string]string{"Accept": mime})
				if st != http.StatusOK {
					t.Fatalf("Accept=%s on %s status=%d", mime, route, st)
				}
				assertMarkdownHeaders(t, route+" ["+mime+"]", hdr)
				if strings.Contains(body, "<!DOCTYPE html>") {
					t.Fatalf("Accept=%s on %s returned HTML instead of Markdown", mime, route)
				}
			}
		}
	})

	t.Run("UT-MD-NEG-02_BrowserAndWildcardAcceptAlwaysReturnsHTML", func(t *testing.T) {
		for _, acceptHdr := range []string{
			"text/html,application/xhtml+xml,application/xml;q=0.9,image/avif,image/webp,*/*;q=0.8",
			"*/*",
			"text/*",
			"",
		} {
			for _, route := range []string{"/", "/rev/r1", "/print", "/guide", "/changelog", "/chapters/01-system-architecture"} {
				st, body, hdr := fetchHTTP(t, http.MethodGet, ts.URL+route, map[string]string{"Accept": acceptHdr})
				if st != http.StatusOK {
					t.Fatalf("Accept=%q on %s status=%d", acceptHdr, route, st)
				}
				if !strings.HasPrefix(hdr.Get("Content-Type"), "text/html") || !strings.Contains(body, "<!DOCTYPE html>") {
					t.Fatalf("Accept=%q on %s did not return HTML: ct=%q", acceptHdr, route, hdr.Get("Content-Type"))
				}
			}
		}
	})

	t.Run("UT-MD-NEG-03_and_04_QualityWeightQValueOrderingAndQZero", func(t *testing.T) {
		// q(markdown)=0.9 > q(html)=0.8 -> Markdown
		_, _, hdrMD := fetchHTTP(t, http.MethodGet, ts.URL+"/chapters/01-system-architecture", map[string]string{
			"Accept": "text/markdown;q=0.9, text/html;q=0.8",
		})
		if hdrMD.Get("Content-Type") != "text/markdown; charset=utf-8" {
			t.Fatalf("expected Markdown when q=0.9 > q=0.8, got %q", hdrMD.Get("Content-Type"))
		}

		// q(markdown)=0.5 < q(html)=0.9 -> HTML
		_, _, hdrHTML := fetchHTTP(t, http.MethodGet, ts.URL+"/chapters/01-system-architecture", map[string]string{
			"Accept": "text/markdown;q=0.5, text/html;q=0.9",
		})
		if !strings.HasPrefix(hdrHTML.Get("Content-Type"), "text/html") {
			t.Fatalf("expected HTML when q(md)=0.5 < q(html)=0.9, got %q", hdrHTML.Get("Content-Type"))
		}

		// Equal q=1.0 -> Markdown preferred for explicit text/markdown caller
		_, _, hdrTie := fetchHTTP(t, http.MethodGet, ts.URL+"/chapters/01-system-architecture", map[string]string{
			"Accept": "text/markdown, text/html",
		})
		if hdrTie.Get("Content-Type") != "text/markdown; charset=utf-8" {
			t.Fatalf("expected Markdown when both text/markdown and text/html have q=1.0, got %q", hdrTie.Get("Content-Type"))
		}

		// UT-MD-NEG-04: q=0 explicitly rejects text/markdown -> HTML
		_, _, hdrQ0 := fetchHTTP(t, http.MethodGet, ts.URL+"/chapters/01-system-architecture", map[string]string{
			"Accept": "text/markdown;q=0, */*;q=0.8",
		})
		if !strings.HasPrefix(hdrQ0.Get("Content-Type"), "text/html") {
			t.Fatalf("expected HTML when text/markdown;q=0, got %q", hdrQ0.Get("Content-Type"))
		}
	})

	t.Run("UT-MD-NEG-05_QueryFormatOverridesAcceptHeader", func(t *testing.T) {
		// ?format=md overrides browser Accept: text/html
		_, _, hdrForceMD := fetchHTTP(t, http.MethodGet, ts.URL+"/chapters/01-system-architecture?format=md", map[string]string{
			"Accept": "text/html,application/xhtml+xml",
		})
		if hdrForceMD.Get("Content-Type") != "text/markdown; charset=utf-8" {
			t.Fatalf("expected ?format=md to override Accept: text/html, got %q", hdrForceMD.Get("Content-Type"))
		}

		// ?raw=1 overrides browser Accept: text/html
		_, _, hdrRaw1 := fetchHTTP(t, http.MethodGet, ts.URL+"/chapters/01-system-architecture?raw=1", map[string]string{
			"Accept": "text/html",
		})
		if hdrRaw1.Get("Content-Type") != "text/markdown; charset=utf-8" {
			t.Fatalf("expected ?raw=1 to return Markdown, got %q", hdrRaw1.Get("Content-Type"))
		}

		// ?format=html overrides Accept: text/markdown
		_, _, hdrForceHTML := fetchHTTP(t, http.MethodGet, ts.URL+"/chapters/01-system-architecture?format=html", map[string]string{
			"Accept": "text/markdown",
		})
		if !strings.HasPrefix(hdrForceHTML.Get("Content-Type"), "text/html") {
			t.Fatalf("expected ?format=html to override Accept: text/markdown, got %q", hdrForceHTML.Get("Content-Type"))
		}
	})

	t.Run("UT-MD-NEG-06_VaryAcceptAndDistinctETagsBetweenHTMLAndMarkdown", func(t *testing.T) {
		_, _, hdrHTML := fetchHTTP(t, http.MethodGet, ts.URL+"/chapters/01-system-architecture", map[string]string{
			"Accept": "text/html",
		})
		_, _, hdrMD := fetchHTTP(t, http.MethodGet, ts.URL+"/chapters/01-system-architecture", map[string]string{
			"Accept": "text/markdown",
		})
		if !strings.Contains(hdrHTML.Get("Vary"), "Accept") || !strings.Contains(hdrMD.Get("Vary"), "Accept") {
			t.Fatalf("missing Vary: Accept header: html=%q md=%q", hdrHTML.Get("Vary"), hdrMD.Get("Vary"))
		}
		if hdrHTML.Get("ETag") == "" || hdrMD.Get("ETag") == "" || hdrHTML.Get("ETag") == hdrMD.Get("ETag") {
			t.Fatalf("expected distinct strong ETags for HTML vs Markdown representations, got html=%q md=%q", hdrHTML.Get("ETag"), hdrMD.Get("ETag"))
		}
		if !strings.Contains(hdrHTML.Get("Link"), `</chapters/01-system-architecture.md>; rel="alternate"; type="text/markdown"`) {
			t.Fatalf("expected HTML Link header to advertise raw Markdown alternate, got %q", hdrHTML.Get("Link"))
		}
		if !strings.Contains(hdrMD.Get("Link"), `</chapters/01-system-architecture>; rel="alternate"; type="text/html"`) {
			t.Fatalf("expected Markdown Link header to advertise HTML alternate, got %q", hdrMD.Get("Link"))
		}
	})

	t.Run("UT-MD-DISC-03_OpenAPISpecDocumentsRawMarkdownAndKeeps15Paths", func(t *testing.T) {
		st, body, _ := fetchHTTP(t, http.MethodGet, ts.URL+"/openapi.json", nil)
		if st != http.StatusOK {
			t.Fatalf("GET /openapi.json status=%d", st)
		}
		var doc map[string]any
		if err := json.Unmarshal([]byte(body), &doc); err != nil {
			t.Fatalf("unmarshal /openapi.json: %v", err)
		}
		info, _ := doc["info"].(map[string]any)
		desc, _ := info["description"].(string)
		for _, want := range []string{"/llms.txt", "/llms-full.txt", "Accept: text/markdown", "?raw=exact", ".mmd"} {
			if !strings.Contains(desc, want) {
				t.Fatalf("/openapi.json info.description missing %q", want)
			}
		}
		paths, _ := doc["paths"].(map[string]any)
		if len(paths) != 13 {
			t.Fatalf("expected exactly 13 ConnectRPC paths in /openapi.json, got %d", len(paths))
		}
	})
}

func TestFrontPageEditorialTimeline_HomeScopingChangelogAndLLMsTxt_UT_TL_05(t *testing.T) {
	ctx := context.Background()
	ts, eng, _ := setupTestServer(t)

	// Before any timeline milestone exists, GET / renders the dedicated Timeline home page without #frontPageTimeline items
	stInit, htmlInit, _ := fetchHTTP(t, http.MethodGet, ts.URL+"/", nil)
	if stInit != http.StatusOK || !strings.Contains(htmlInit, "<h1>Timeline</h1>") || strings.Contains(htmlInit, `id="frontPageTimeline"`) {
		t.Fatalf("expected clean empty-state Timeline home page before synthesis, status=%d", stInit)
	}

	mockGemini := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"candidates": [{
				"content": {
					"parts": [{"text": "{\"milestones\":[{\"revisions\":[2],\"title\":\"Render Pipeline Tuning\",\"summary\":\"Cut snapshot rebuild latency below 15ms across all chapters.\",\"chapter_slugs\":[\"01-system-architecture\",\"02-agent-workflow\"],\"authors\":[\"Alice Chen\",\"Bob Rivera\"]}]}"}]
				}
			}]
		}`))
	}))
	defer mockGemini.Close()
	eng.SetBriefSynthesizer(engine.NewBriefSynthesizer("gm_http_test", "gemini-2.5-flash", mockGemini.URL))

	if _, err := eng.PatchChapter(ctx, &engine.PatchChapterInput{
		Slug:          "01-system-architecture",
		OldText:       "Goldmark SSR rendering",
		NewText:       "Goldmark SSR rendering\n\n> **[TODO · Carol · 2026-11-18]:** Validate Q4 latency benchmark.",
		CommitMessage: "Add Carol TODO and update ch1",
	}); err != nil {
		t.Fatalf("PatchChapter: %v", err)
	}
	if _, err := eng.SynthesizeBriefingIfNeeded(ctx); err != nil {
		t.Fatalf("SynthesizeBriefingIfNeeded: %v", err)
	}

	// GET / renders #frontPageBriefing vertical timeline + open callouts drawer
	stHome, htmlHome, _ := fetchHTTP(t, http.MethodGet, ts.URL+"/", nil)
	if stHome != http.StatusOK {
		t.Fatalf("GET / status=%d", stHome)
	}
	for _, want := range []string{
		`id="frontPageBriefing"`,
		`<h1>Timeline</h1>`,
		`id="frontPageTimeline"`,
		`class="timeline-feed"`,
		`Render Pipeline Tuning`,
		`Cut snapshot rebuild latency below 15ms across all chapters.`,
		`/chapters/01-system-architecture`,
		`Ch. 1 · System Architecture`,
		`id="frontPageOpenCallouts"`,
		`Validate Q4 latency benchmark.`,
		`Start Reading →`,
	} {
		if !strings.Contains(htmlHome, want) {
			t.Fatalf("GET / missing %q", want)
		}
	}
	if strings.Contains(htmlHome, "<details") {
		t.Fatalf("expected top-level front-page timeline on / with zero <details> card wrapper")
	}

	// Direct chapter routes (including /chapters/00-intro!) and historical permalinks omit #frontPageBriefing
	for _, nonHomeRoute := range []string{
		"/chapters/00-intro",
		"/chapters/01-system-architecture",
		"/chapters/02-agent-workflow",
		"/rev/r1",
		"/?rev=r1",
		"/chapters/00-intro/rev/r1",
	} {
		st, htmlBody, _ := fetchHTTP(t, http.MethodGet, ts.URL+nonHomeRoute, nil)
		if st != http.StatusOK {
			t.Fatalf("GET %s status=%d", nonHomeRoute, st)
		}
		if strings.Contains(htmlBody, `id="frontPageBriefing"`) {
			t.Fatalf("expected %s to omit #frontPageBriefing", nonHomeRoute)
		}
	}

	// GET /changelog renders strictly the Git Commit Log without duplicating the timeline
	stCL, htmlCL, _ := fetchHTTP(t, http.MethodGet, ts.URL+"/changelog", nil)
	if stCL != http.StatusOK || !strings.Contains(htmlCL, "<h1>Git Commit Log</h1>") || strings.Contains(htmlCL, `class="timeline-feed"`) {
		t.Fatalf("GET /changelog unexpected body")
	}
	_, mdCL, _ := fetchHTTP(t, http.MethodGet, ts.URL+"/changelog.md", nil)
	if !strings.Contains(mdCL, "# Git Commit Log") || strings.Contains(mdCL, "## Timeline") {
		t.Fatalf("GET /changelog.md unexpected body:\n%s", mdCL)
	}

	// GET /llms.txt includes ## Timeline at HEAD, while /rev/r1/llms.txt and /chapters/00-intro.md remain pure
	_, llmsLive, _ := fetchHTTP(t, http.MethodGet, ts.URL+"/llms.txt", nil)
	for _, want := range []string{
		"## Timeline",
		"**Render Pipeline Tuning**",
		"### Open Editorial Callouts",
		"Validate Q4 latency benchmark.",
	} {
		if !strings.Contains(llmsLive, want) {
			t.Fatalf("GET /llms.txt missing %q:\n%s", want, llmsLive)
		}
	}

	_, llmsHist, _ := fetchHTTP(t, http.MethodGet, ts.URL+"/rev/r1/llms.txt", nil)
	if strings.Contains(llmsHist, "## Timeline") {
		t.Fatalf("expected historical /rev/r1/llms.txt to omit live timeline")
	}

	_, introMD, _ := fetchHTTP(t, http.MethodGet, ts.URL+"/chapters/00-intro.md", nil)
	if strings.Contains(introMD, "## Timeline") || strings.Contains(introMD, "Render Pipeline Tuning") {
		t.Fatalf("expected /chapters/00-intro.md to remain pure unmutated chapter Markdown, got:\n%s", introMD)
	}

	// Verify 20-item HTML cap on GET /, full expansion on GET /?all=1, and 15-item cap on GET /llms.txt
	var extra []store.TimelineEntryRecord
	for i := 1; i <= 24; i++ {
		rev := int32(10 + i)
		extra = append(extra, store.TimelineEntryRecord{
			StartRevision:     rev,
			EndRevision:       rev,
			StartCommitSHA:    "abc1234",
			EndCommitSHA:      "abc1234",
			TimestampISO:      "2026-11-18T19:00:00Z",
			StartTimestampISO: "2026-11-18T19:00:00Z",
			CommitCount:       1,
			Revisions:         []int32{rev},
			Title:             "Seeded Milestone #" + strconv.Itoa(i),
			Summary:           "Historical milestone summary #" + strconv.Itoa(i) + ".",
			ChapterSlugs:      []string{"01-system-architecture"},
			Authors:           []string{"Bob Rivera"},
		})
	}
	if _, err := eng.Store().AppendEditorialTimelineEntries(ctx, extra, store.TimelineStateRecord{
		LastHeadSHA:      "abc1234",
		LastHeadRevision: 99,
		ModelName:        "gemini-2.5-flash",
	}); err != nil {
		t.Fatalf("AppendEditorialTimelineEntries: %v", err)
	}
	if _, err := eng.PatchChapter(ctx, &engine.PatchChapterInput{
		Slug:          "02-agent-workflow",
		OldText:       "optimistic revision locks",
		NewText:       "optimistic Git revision locks",
		CommitMessage: "Refresh snapshot after seeding 25 total timeline entries",
	}); err != nil {
		t.Fatalf("PatchChapter refresh: %v", err)
	}

	_, htmlCapped, _ := fetchHTTP(t, http.MethodGet, ts.URL+"/", nil)
	if got := strings.Count(htmlCapped, `class="timeline-entry"`); got != 20 {
		t.Fatalf("expected GET / to cap rendered milestones at 20, got %d", got)
	}
	if !strings.Contains(htmlCapped, `href="/timeline?all=1"`) || !strings.Contains(htmlCapped, "Show 5 older milestones") {
		t.Fatalf("expected GET / to show older-milestones link to /timeline?all=1 when >20 entries exist")
	}

	stTL, htmlTL, _ := fetchHTTP(t, http.MethodGet, ts.URL+"/timeline", nil)
	if stTL != http.StatusOK || strings.Count(htmlTL, `class="timeline-entry"`) != 20 || !strings.Contains(htmlTL, `<link rel="canonical" href="/timeline">`) {
		t.Fatalf("expected GET /timeline to render 20 milestones with /timeline canonical link (status=%d)", stTL)
	}

	_, htmlAll, _ := fetchHTTP(t, http.MethodGet, ts.URL+"/timeline?all=1", nil)
	if got := strings.Count(htmlAll, `class="timeline-entry"`); got != 25 {
		t.Fatalf("expected GET /timeline?all=1 to render all 25 milestones, got %d", got)
	}
	if !strings.Contains(htmlAll, `href="/timeline"`) || !strings.Contains(htmlAll, "Show latest 20 only") {
		t.Fatalf("expected GET /timeline?all=1 to render collapse link back to /timeline")
	}

	stTLMD, mdTimeline, hdrTLMD := fetchHTTP(t, http.MethodGet, ts.URL+"/timeline.md", nil)
	if stTLMD != http.StatusOK || !strings.HasPrefix(hdrTLMD.Get("Content-Type"), "text/markdown") || !strings.Contains(mdTimeline, "# Timeline · Ghostbook") || !strings.Contains(mdTimeline, "Render Pipeline Tuning") {
		t.Fatalf("expected GET /timeline.md to return complete pure Markdown timeline (including oldest #25), status=%d body:\n%s", stTLMD, mdTimeline)
	}
	_, mdTimelineNeg, hdrNeg := fetchHTTP(t, http.MethodGet, ts.URL+"/timeline", map[string]string{"Accept": "text/markdown"})
	if !strings.HasPrefix(hdrNeg.Get("Content-Type"), "text/markdown") || mdTimelineNeg != mdTimeline {
		t.Fatalf("expected Accept: text/markdown on /timeline to match /timeline.md")
	}
	if _, err := engine.ValidateSlug("timeline"); err == nil {
		t.Fatalf("expected reserved route slug 'timeline' to be rejected by ValidateSlug")
	}

	_, llmsCapped, _ := fetchHTTP(t, http.MethodGet, ts.URL+"/llms.txt", nil)
	if !strings.Contains(llmsCapped, "Showing latest 15 of 25 milestones") || !strings.Contains(llmsCapped, "/timeline?all=1") || strings.Contains(llmsCapped, "Render Pipeline Tuning") {
		t.Fatalf("expected GET /llms.txt to cap timeline at latest 15 of 25 milestones, got:\n%s", llmsCapped)
	}
}

// UT-SSR-04: Pure SSR Search, Chapter History & Blame, and Activity Routes + Markdown Negotiation + Zero Audit Pollution
func TestPureSSRRoutes_SearchHistoryActivityAndCSP_UT_SSR_04(t *testing.T) {
	ts, eng, st := setupTestServer(t)
	ctx := context.Background()

	carol, _ := st.ResolveAuthorName(ctx, "carol")
	if _, err := eng.PatchChapter(engine.WithAuthor(ctx, carol), &engine.PatchChapterInput{
		Slug:          "01-system-architecture",
		OldText:       "Ghostbook bundles pure-Go",
		NewText:       "Ghostbook bundles pure-Go (v2 benchmark)",
		CommitMessage: "Add pure-Go Git AUM for SSR history diff verification",
	}); err != nil {
		t.Fatalf("PatchChapter: %v", err)
	}

	beforeAudit, _ := st.ListActivity(ctx, "", 100)

	// 1. SSR Search HTML & Raw Markdown (/search?q=... & /search.md?q=...)
	stSearch, htmlSearch, hdrSearch := fetchHTTP(t, http.MethodGet, ts.URL+"/search?q=pure-Go+Git&slug=01-system-architecture", nil)
	if stSearch != http.StatusOK {
		t.Fatalf("GET /search status=%d", stSearch)
	}
	if !strings.Contains(hdrSearch.Get("Content-Security-Policy"), "connect-src 'none'") {
		t.Fatalf("expected connect-src 'none' CSP header on /search, got %q", hdrSearch.Get("Content-Security-Policy"))
	}
	for _, want := range []string{
		`id="ssrSearchRoot"`,
		`<mark>pure</mark>`,
		`01-system-architecture`,
		`(v2 benchmark)`,
	} {
		if !strings.Contains(htmlSearch, want) {
			t.Fatalf("GET /search missing %q", want)
		}
	}

	stSearchMD, mdSearch, hdrSearchMD := fetchHTTP(t, http.MethodGet, ts.URL+"/search.md?q=pure-Go+Git", nil)
	if stSearchMD != http.StatusOK || !strings.HasPrefix(hdrSearchMD.Get("Content-Type"), "text/markdown") || !strings.Contains(mdSearch, "# Full-Text Book Search") || !strings.Contains(mdSearch, "01-system-architecture") {
		t.Fatalf("GET /search.md unexpected response (status=%d):\n%s", stSearchMD, mdSearch)
	}

	// 2. SSR Chapter History & Blame (/chapters/{slug}/history & /chapters/{slug}/history.md)
	stHist, htmlHist, hdrHist := fetchHTTP(t, http.MethodGet, ts.URL+"/chapters/01-system-architecture/history", nil)
	if stHist != http.StatusOK || !strings.Contains(hdrHist.Get("Content-Security-Policy"), "connect-src 'none'") {
		t.Fatalf("GET /chapters/.../history status=%d csp=%q", stHist, hdrHist.Get("Content-Security-Policy"))
	}
	for _, want := range []string{
		`id="ssrChapterHistoryRoot"`,
		`id="ssrBlameList"`,
		`id="ssrCommitsList"`,
		`Carol Park`,
		`diff-line-add`,
		`Ghostbook bundles pure-Go (v2 benchmark)`,
	} {
		if !strings.Contains(htmlHist, want) {
			t.Fatalf("GET /chapters/01-system-architecture/history missing %q", want)
		}
	}

	stHistMD, mdHist, hdrHistMD := fetchHTTP(t, http.MethodGet, ts.URL+"/chapters/01-system-architecture/history", map[string]string{"Accept": "text/markdown"})
	if stHistMD != http.StatusOK || !strings.HasPrefix(hdrHistMD.Get("Content-Type"), "text/markdown") || !strings.Contains(mdHist, "## Section-Level Git Blame") || !strings.Contains(mdHist, "```diff") {
		t.Fatalf("Accept: text/markdown on /chapters/.../history failed:\n%s", mdHist)
	}

	// 3. SSR Agent Activity (/activity & /activity.md)
	stAct, htmlAct, _ := fetchHTTP(t, http.MethodGet, ts.URL+"/activity", nil)
	if stAct != http.StatusOK || !strings.Contains(htmlAct, `id="ssrActivityRoot"`) {
		t.Fatalf("GET /activity status=%d", stAct)
	}
	stActMD, mdAct, hdrActMD := fetchHTTP(t, http.MethodGet, ts.URL+"/activity.md", nil)
	if stActMD != http.StatusOK || !strings.HasPrefix(hdrActMD.Get("Content-Type"), "text/markdown") || !strings.Contains(mdAct, "# Agent Tool Audit Log") {
		t.Fatalf("GET /activity.md unexpected:\n%s", mdAct)
	}

	// 4. Verify passive SSR page views wrote ZERO synthetic rows to agent_audit_log
	afterAudit, _ := st.ListActivity(ctx, "", 100)
	if len(afterAudit) != len(beforeAudit) {
		t.Fatalf("expected SSR page views to write 0 rows to agent_audit_log (before=%d after=%d)", len(beforeAudit), len(afterAudit))
	}
}

func TestBookConfigHTTPEndpoints_IT_HTTP_01(t *testing.T) {
	ctx := context.Background()
	ts, eng, _ := setupTestServer(t)

	// Before src/book.json exists: live /book.json returns synthesized 200 JSON (no-cache),
	// while pinned /rev/r1/book.json returns 404 (avoiding immutable caching of synthetic bytes).
	stLive, liveBody, liveHdr := fetchHTTP(t, http.MethodGet, ts.URL+"/book.json", nil)
	if stLive != http.StatusOK || !strings.HasPrefix(liveHdr.Get("Content-Type"), "application/json") || liveHdr.Get("Cache-Control") != "no-cache" {
		t.Fatalf("GET /book.json before create: status=%d ct=%q cc=%q", stLive, liveHdr.Get("Content-Type"), liveHdr.Get("Cache-Control"))
	}
	if !strings.Contains(liveBody, `"max_heading_chars": 42`) {
		t.Fatalf("unexpected synthesized /book.json body:\n%s", liveBody)
	}
	stHistMissing, _, _ := fetchHTTP(t, http.MethodGet, ts.URL+"/rev/r1/book.json", nil)
	if stHistMissing != http.StatusNotFound {
		t.Fatalf("expected 404 for /rev/r1/book.json before src/book.json existed, got %d", stHistMissing)
	}

	// Alias paths redirect 302 to /book.json instead of Goldmark-rendering raw JSON
	noRedirectClient := &http.Client{CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
		return http.ErrUseLastResponse
	}}
	for _, alias := range []string{"/BOOK_CONFIG", "/book-config", "/chapters/BOOK_CONFIG"} {
		resp, err := noRedirectClient.Get(ts.URL + alias)
		if err != nil {
			t.Fatalf("GET %s: %v", alias, err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusFound || resp.Header.Get("Location") != "/book.json" {
			t.Fatalf("GET %s: expected 302 -> /book.json, got %d -> %q", alias, resp.StatusCode, resp.Header.Get("Location"))
		}
	}

	// Commit src/book.json and verify pinned /rev/{sha}/book.json returns 200 immutable JSON
	patchOut, err := eng.PatchChapter(ctx, &engine.PatchChapterInput{
		Slug:          "BOOK_CONFIG",
		OldText:       `"title": "Ghostbook"`,
		NewText:       `"title": "HTTP Config Book"`,
		CommitMessage: "Create src/book.json via engine",
	})
	if err != nil {
		t.Fatalf("PatchChapter(BOOK_CONFIG): %v", err)
	}
	stPinned, pinnedBody, pinnedHdr := fetchHTTP(t, http.MethodGet, ts.URL+"/rev/"+patchOut.NewRevisionSHA+"/book.json", nil)
	if stPinned != http.StatusOK || !strings.Contains(pinnedHdr.Get("Cache-Control"), "immutable") || !strings.Contains(pinnedBody, `"HTTP Config Book"`) {
		t.Fatalf("GET /rev/%s/book.json: status=%d cc=%q body=%s", patchOut.NewRevisionSHA, stPinned, pinnedHdr.Get("Cache-Control"), pinnedBody)
	}

	// GET /STYLE_GUIDE renders HTML with governance banner and updated config card
	stStyle, styleHTML, styleHdr := fetchHTTP(t, http.MethodGet, ts.URL+"/STYLE_GUIDE", nil)
	if stStyle != http.StatusOK || !strings.HasPrefix(styleHdr.Get("Content-Type"), "text/html") {
		t.Fatalf("GET /STYLE_GUIDE: status=%d ct=%q", stStyle, styleHdr.Get("Content-Type"))
	}
	if !strings.Contains(styleHTML, `id="styleGuideGovernanceBanner"`) || !strings.Contains(styleHTML, `HTTP Config Book`) || !strings.Contains(styleHTML, `src/book.json`) {
		t.Fatalf("GET /STYLE_GUIDE missing governance banner or active book.json values")
	}

	// GET /llms.txt lists /book.json
	stLLMs, llmsBody, _ := fetchHTTP(t, http.MethodGet, ts.URL+"/llms.txt", nil)
	if stLLMs != http.StatusOK || !strings.Contains(llmsBody, "/book.json") {
		t.Fatalf("GET /llms.txt missing /book.json link")
	}
}
