package engine_test

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wjkoh/ghostbook/internal/engine"
)

func TestTimelineSemanticClusteringInterleavingAndCallouts_UT_TL_02(t *testing.T) {
	eng, _ := setupTestEngine(t)
	ctx := context.Background()

	var capturedPrompts []string
	var mu sync.Mutex
	mockGemini := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.RawQuery != "" {
			t.Errorf("expected zero API key in URL query string, got %q", r.URL.RawQuery)
		}
		if r.Header.Get("x-goog-api-key") != "gm_test_key" {
			http.Error(w, "missing x-goog-api-key", http.StatusUnauthorized)
			return
		}
		raw, _ := io.ReadAll(r.Body)
		var req struct {
			Contents []struct {
				Parts []struct {
					Text string `json:"text"`
				} `json:"parts"`
			} `json:"contents"`
		}
		_ = json.Unmarshal(raw, &req)
		if len(req.Contents) > 0 && len(req.Contents[0].Parts) > 0 {
			mu.Lock()
			capturedPrompts = append(capturedPrompts, req.Contents[0].Parts[0].Text)
			mu.Unlock()
		}
		w.Header().Set("Content-Type", "application/json")
		// Return interleaved revision grouping: [2, 4] (Bob's two Ch.1 edits) vs [3] (Carol's Ch.4 edit),
		// plus a thinking part, hallucinated revision 999, hallucinated slug, em dash, and "legacy" to verify sanitization.
		_, _ = w.Write([]byte(`{
			"candidates": [{
				"content": {
					"parts": [
						{"thought": true, "text": "Clustering interleaved revisions 2 and 4 separately from 3..."},
						{"text": "{\"milestones\":[{\"revisions\":[2,4,999],\"title\":\"pure-Go Git Stack & Fee Schedule:\",\"summary\":\"Added $4.2B AUM figure — replacing legacy framing with institutional credit sleeves.\",\"chapter_slugs\":[\"01-system-architecture\",\"99-hallucinated\"],\"authors\":[\"Bob Rivera\"]},{\"revisions\":[3],\"title\":\"Tool-First Onboarding Sequence\",\"summary\":\"Clarified zero-migration portfolio ingestion steps before balance-sheet buyout.\",\"chapter_slugs\":[],\"authors\":[]}]}"}
					]
				}
			}]
		}`))
	}))
	defer mockGemini.Close()

	eng.SetBriefSynthesizer(engine.NewBriefSynthesizer("gm_test_key", "gemini-2.5-flash", mockGemini.URL))

	// 1-commit seed repo alone should be a no-op
	if entries, err := eng.SynthesizeBriefingIfNeeded(ctx); err != nil || len(entries) != 0 {
		t.Fatalf("expected empty on initial 1-commit repo, got %+v (err=%v)", entries, err)
	}

	// r2: Bob edits Chapter 1
	if _, err := eng.PatchChapter(ctx, &engine.PatchChapterInput{
		Slug:          "01-system-architecture",
		OldText:       "Ghostbook bundles pure-Go",
		NewText:       "Ghostbook bundles pure-Go ($4.2B AUM)",
		CommitMessage: "Add pure-Go Git AUM",
	}); err != nil {
		t.Fatalf("patch r2: %v", err)
	}
	// r3: Interleaved edit on Chapter 4 by Carol
	if _, err := eng.PatchChapter(ctx, &engine.PatchChapterInput{
		Slug:          "04-deployment-ops",
		AuthorName:    "carol",
		OldText:       "debounced push to a private Git remote",
		NewText:       "debounced push to a private Git remote with retry",
		CommitMessage: "Clarify ch4 overlay step",
	}); err != nil {
		t.Fatalf("patch r3: %v", err)
	}
	// r4: Follow-up edit on Chapter 1 + real blockquote TODO (plus code-fence fake TODO)
	if _, err := eng.PatchChapter(ctx, &engine.PatchChapterInput{
		Slug:          "01-system-architecture",
		OldText:       "inside one static binary.",
		NewText:       "inside one static binary.\n\n> **[TODO · Alice · 2026-11-18]:** Confirm SQLite \\$250K benchmark.\n\n```markdown\n> [TODO · IgnoredInsideFence · 2026-11-18]: Ignored.\n```",
		CommitMessage: "Add Alice TODO callout in ch1",
	}); err != nil {
		t.Fatalf("patch r4: %v", err)
	}
	// r5: Instructional placeholder in 00-intro -> ignored by callout scanner & omitted as minor edit by AI
	if _, err := eng.PatchChapter(ctx, &engine.PatchChapterInput{
		Slug:          "00-intro",
		OldText:       "over MCP and ConnectRPC.",
		NewText:       "over MCP and ConnectRPC.\n\n> **[TODO · Co-Author Name · YYYY-MM-DD]:** Template example.",
		CommitMessage: "Add template example in 00-intro",
	}); err != nil {
		t.Fatalf("patch r5: %v", err)
	}

	// Verify deterministic callout extraction unescapes \$ -> $ and ignores code fences & YYYY-MM-DD templates
	snap := eng.CurrentSnapshot()
	if len(snap.OpenCallouts) != 1 || !strings.Contains(snap.OpenCallouts[0].Excerpt, "$250K") || strings.Contains(snap.OpenCallouts[0].Excerpt, `\$`) {
		t.Fatalf("unexpected open callouts: %+v", snap.OpenCallouts)
	}

	entries, err := eng.SynthesizeBriefingIfNeeded(ctx)
	if err != nil || len(entries) != 2 {
		t.Fatalf("expected 2 clustered timeline milestones, got %+v (err=%v)", entries, err)
	}

	// Top entry = interleaved [2, 4] (StartRevision=2, EndRevision=4, CommitCount=2; hallucinated rev 999 dropped)
	top := entries[0]
	if top.StartRevision != 2 || top.EndRevision != 4 || top.CommitCount != 2 {
		t.Fatalf("expected interleaved [2,4] milestone with count=2, got %+v", top)
	}
	if top.Title != "pure-Go Git Stack & Fee Schedule" || strings.Contains(top.Summary, "—") {
		t.Fatalf("expected sanitized title & summary, got title=%q summary=%q", top.Title, top.Summary)
	}
	if len(top.ChapterSlugs) != 1 || top.ChapterSlugs[0] != "01-system-architecture" {
		t.Fatalf("expected hallucinated slug stripped, got %+v", top.ChapterSlugs)
	}

	// Second entry = [3], with empty chapter_slugs & authors auto-filled deterministically from Git commit r3!
	second := entries[1]
	if second.StartRevision != 3 || second.EndRevision != 3 || second.CommitCount != 1 {
		t.Fatalf("expected single-commit milestone r3, got %+v", second)
	}
	if len(second.ChapterSlugs) != 1 || second.ChapterSlugs[0] != "04-deployment-ops" || len(second.Authors) != 1 || second.Authors[0] != "Carol Park" {
		t.Fatalf("expected deterministic Git auto-fill of chapter_slugs & authors on r3, got %+v", second)
	}

	// Repeat call at same HEAD hits SQLite state watermark with 0 extra Gemini HTTP calls
	entriesCached, err := eng.SynthesizeBriefingIfNeeded(ctx)
	if err != nil || len(entriesCached) != 2 {
		t.Fatalf("expected cached 2 milestones, got %+v (err=%v)", entriesCached, err)
	}
	mu.Lock()
	if len(capturedPrompts) != 1 {
		t.Fatalf("expected 1 total Gemini call, got %d", len(capturedPrompts))
	}
	mu.Unlock()
}

func TestAppendOnlyImmutabilityAndSessionDebounce_UT_TL_03(t *testing.T) {
	eng, _ := setupTestEngine(t)
	ctx := context.Background()

	// Seed r2 & r3 before starting background worker
	if _, err := eng.PatchChapter(ctx, &engine.PatchChapterInput{
		Slug:          "01-system-architecture",
		OldText:       "Goldmark SSR rendering",
		NewText:       "Goldmark SSR HTML rendering",
		CommitMessage: "Session 1 edit A",
	}); err != nil {
		t.Fatalf("r2: %v", err)
	}
	if _, err := eng.PatchChapter(ctx, &engine.PatchChapterInput{
		Slug:          "01-system-architecture",
		OldText:       "exact 1-based line matching",
		NewText:       "exact 1-based line substring matching",
		CommitMessage: "Session 1 edit B",
	}); err != nil {
		t.Fatalf("r3: %v", err)
	}

	var calls atomic.Int32
	var lastPrompt atomic.Value
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := calls.Add(1)
		raw, _ := io.ReadAll(r.Body)
		lastPrompt.Store(string(raw))
		w.Header().Set("Content-Type", "application/json")
		if n == 1 {
			_, _ = w.Write([]byte(`{"candidates":[{"content":{"parts":[{"text":"{\"milestones\":[{\"revisions\":[2,3],\"title\":\"Storage Engine Update\",\"summary\":\"Expanded SQLite WAL and FTS5 index comparisons.\",\"chapter_slugs\":[\"01-system-architecture\"],\"authors\":[\"Bob Rivera\"]}]}"}]}}]}`))
			return
		}
		_, _ = w.Write([]byte(`{"candidates":[{"content":{"parts":[{"text":"{\"milestones\":[{\"revisions\":[4,5,6,7],\"title\":\"Phase 1 Lock-In Sequence\",\"summary\":\"Merged four rapid edits refining partner onboarding.\",\"chapter_slugs\":[\"04-deployment-ops\"],\"authors\":[\"Carol Park\"]}]}"}]}}]}`))
	}))
	defer srv.Close()

	eng.SetBriefSynthesizer(engine.NewBriefSynthesizer("gm_debounce_key", "gemini-2.5-flash", srv.URL))

	workerCtx, cancel := context.WithCancel(ctx)
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		eng.StartBriefingSynthesizerWithRetry(workerCtx, 90*time.Millisecond, 5*time.Second)
	}()

	// Wait for cold-start bootstrap pass #1 (r2..r3)
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if len(eng.CurrentSnapshot().TimelineEntries) == 1 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	firstPublished := eng.CurrentSnapshot().TimelineEntries[0]
	if firstPublished.Title != "Storage Engine Update" || firstPublished.EndRevision != 3 {
		cancel()
		wg.Wait()
		t.Fatalf("unexpected initial milestone: %+v", firstPublished)
	}

	// Burst of 4 rapid edits (r4..r7) inside the 90ms debounce window
	prevText := "staged idle-window upgrades."
	for i := 1; i <= 4; i++ {
		nextText := prevText + " Step."
		if _, err := eng.PatchChapter(ctx, &engine.PatchChapterInput{
			Slug:          "04-deployment-ops",
			AuthorName:    "carol",
			OldText:       prevText,
			NewText:       nextText,
			CommitMessage: "Rapid session 2 edit",
		}); err != nil {
			cancel()
			wg.Wait()
			t.Fatalf("burst patch %d: %v", i, err)
		}
		prevText = nextText
	}

	// Immediately after the 4th write (before 90ms debounce elapses):
	// 1) Published milestone #1 MUST still be visible (zero flicker)
	// 2) PendingTimelineCommits MUST report 4 settling commits (r4 -> r7)
	midSnap := eng.CurrentSnapshot()
	if len(midSnap.TimelineEntries) != 1 || midSnap.PendingTimelineCommits != 4 || midSnap.PendingStartRevision != 4 || midSnap.PendingEndRevision != 7 {
		cancel()
		wg.Wait()
		t.Fatalf("expected 1 published milestone + 4 pending commits (r4..r7) during debounce, got entries=%d pending=%d (%d..%d)",
			len(midSnap.TimelineEntries), midSnap.PendingTimelineCommits, midSnap.PendingStartRevision, midSnap.PendingEndRevision)
	}

	// Wait for debounced pass #2 to append the r4..r7 milestone
	deadline = time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if len(eng.CurrentSnapshot().TimelineEntries) == 2 {
			break
		}
		time.Sleep(15 * time.Millisecond)
	}

	cancel()
	wg.Wait()

	if gotCalls := calls.Load(); gotCalls != 2 {
		t.Fatalf("expected 4-commit burst to coalesce into 1 incremental Gemini call (total 2), got %d", gotCalls)
	}
	finalSnap := eng.CurrentSnapshot()
	if finalSnap.PendingTimelineCommits != 0 {
		t.Fatalf("expected PendingTimelineCommits=0 after synthesis, got %d", finalSnap.PendingTimelineCommits)
	}
	if len(finalSnap.TimelineEntries) != 2 {
		t.Fatalf("expected 2 total milestones, got %+v", finalSnap.TimelineEntries)
	}
	// Strictly append-only: older milestone (index 1) must keep identical ID, title, summary, and revision span!
	if finalSnap.TimelineEntries[1].ID != firstPublished.ID ||
		finalSnap.TimelineEntries[1].Title != firstPublished.Title ||
		finalSnap.TimelineEntries[1].Summary != firstPublished.Summary {
		t.Fatalf("published milestone was mutated! before=%+v after=%+v", firstPublished, finalSnap.TimelineEntries[1])
	}
	// And incremental prompt #2 must strictly start at r4..r7 (never re-clustering r2..r3)
	prompt2, _ := lastPrompt.Load().(string)
	if !strings.Contains(prompt2, "Unsummarized Commit Batch: r4 .. r7") {
		t.Fatalf("expected incremental batch strictly r4 .. r7, got:\n%s", prompt2)
	}
}

func TestMinorSkipSafetyGuardAndVertexOAuth2Caching_UT_TL_04(t *testing.T) {
	ctx := context.Background()

	// 1. testing.Testing() safety guard disables live credentials when endpoint is empty
	if engine.NewBriefSynthesizer("AIzaRealKey", "", "").Enabled() {
		t.Fatalf("expected live API key disabled under go test without mock endpoint")
	}

	// 2. Transient 503 retry + cosmetic-only empty milestones array advances watermark with 0 rows and 0 repeat calls
	var attempts atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if attempts.Add(1) == 1 {
			http.Error(w, "service unavailable", http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"candidates":[{"content":{"parts":[{"text":"{\"milestones\":[]}"}]}}]}`))
	}))
	defer srv.Close()

	eng, _ := setupTestEngine(t)
	eng.SetBriefSynthesizer(engine.NewBriefSynthesizer("gm_retry_key", "", srv.URL))
	if _, err := eng.PatchChapter(ctx, &engine.PatchChapterInput{
		Slug:          "02-agent-workflow",
		OldText:       "optimistic revision locks",
		NewText:       "optimistic revision SHA locks",
		CommitMessage: "Minor typo tweak",
	}); err != nil {
		t.Fatalf("PatchChapter: %v", err)
	}

	entries, err := eng.SynthesizeBriefingIfNeeded(ctx)
	if err != nil || len(entries) != 0 || attempts.Load() != 2 {
		t.Fatalf("expected retry + 0 milestones on cosmetic commit, got entries=%d attempts=%d err=%v", len(entries), attempts.Load(), err)
	}
	// Calling again at same HEAD must NOT hit Gemini again because watermark advanced!
	if _, err := eng.SynthesizeBriefingIfNeeded(ctx); err != nil || attempts.Load() != 2 {
		t.Fatalf("expected watermark to prevent re-evaluating cosmetic commit, attempts=%d err=%v", attempts.Load(), err)
	}

	// 3. Vertex AI RS256 JWT assertion + cached Bearer token across batches
	privKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("rsa.GenerateKey: %v", err)
	}
	pkcs8DER, _ := x509.MarshalPKCS8PrivateKey(privKey)
	pemBytes := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: pkcs8DER})

	var tokenCalls atomic.Int32
	tokenSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tokenCalls.Add(1)
		_ = r.ParseForm()
		if r.FormValue("grant_type") != "urn:ietf:params:oauth:grant-type:jwt-bearer" {
			http.Error(w, "bad grant_type", http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"access_token":"ya29.mock_vertex_token","expires_in":3600}`))
	}))
	defer tokenSrv.Close()

	var vertexPath, vertexAuth string
	var vertexCall atomic.Int32
	vertexSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := vertexCall.Add(1)
		vertexPath = r.URL.Path
		vertexAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		revNum := n + 1
		_, _ = fmt.Fprintf(w, `{"candidates":[{"content":{"parts":[{"text":"{\"milestones\":[{\"revisions\":[%d],\"title\":\"Vertex Milestone\",\"summary\":\"Verified RS256 OAuth2 token caching.\",\"chapter_slugs\":[\"02-agent-workflow\"],\"authors\":[\"Bob Rivera\"]}]}"}]}}]}`, revNum)
	}))
	defer vertexSrv.Close()

	saPath := filepath.Join(t.TempDir(), "vertex-sa.json")
	saDoc, _ := json.Marshal(map[string]string{
		"type":         "service_account",
		"project_id":   "ghostbook-demo-project",
		"client_email": "ghostbook-vertex@ghostbook-demo-project.iam.gserviceaccount.com",
		"private_key":  string(pemBytes),
		"token_uri":    tokenSrv.URL,
	})
	_ = os.WriteFile(saPath, saDoc, 0o600)

	if engine.NewBriefSynthesizerWithConfig(engine.BriefSynthesizerConfig{VertexCredentialsFile: saPath}).Enabled() {
		t.Fatalf("expected live Vertex SA disabled under go test without mock endpoint")
	}

	engVtx, _ := setupTestEngine(t)
	engVtx.SetBriefSynthesizer(engine.NewBriefSynthesizerWithConfig(engine.BriefSynthesizerConfig{
		Endpoint:              vertexSrv.URL,
		VertexCredentialsFile: saPath,
	}))
	_, _ = engVtx.PatchChapter(ctx, &engine.PatchChapterInput{
		Slug:          "02-agent-workflow",
		OldText:       "optimistic revision locks",
		NewText:       "optimistic SHA locks",
		CommitMessage: "Vertex r2",
	})
	if _, err := engVtx.SynthesizeBriefingIfNeeded(ctx); err != nil {
		t.Fatalf("Vertex #1: %v", err)
	}
	if vertexAuth != "Bearer ya29.mock_vertex_token" || vertexPath != "/v1/projects/ghostbook-demo-project/locations/global/publishers/google/models/gemini-2.5-flash:generateContent" {
		t.Fatalf("unexpected Vertex request auth=%q path=%q", vertexAuth, vertexPath)
	}

	_, _ = engVtx.PatchChapter(ctx, &engine.PatchChapterInput{
		Slug:          "02-agent-workflow",
		OldText:       "optimistic SHA locks",
		NewText:       "optimistic revision locks v2",
		CommitMessage: "Vertex r3",
	})
	if _, err := engVtx.SynthesizeBriefingIfNeeded(ctx); err != nil {
		t.Fatalf("Vertex #2: %v", err)
	}
	if tokenCalls.Load() != 1 {
		t.Fatalf("expected cached OAuth2 token across calls, got %d token exchanges", tokenCalls.Load())
	}
}
