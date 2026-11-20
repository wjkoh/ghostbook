package server_test

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/wjkoh/ghostbook/internal/engine"
	"github.com/wjkoh/ghostbook/internal/server"
	"github.com/wjkoh/ghostbook/internal/store"
)

func (c *cdpClient) screenshotWithOptions(t *testing.T, outPath string, fullPage bool) {
	t.Helper()
	if outPath == "" {
		return
	}
	params := map[string]any{"format": "png"}
	if fullPage {
		params["captureBeyondViewport"] = true
	}
	raw, err := c.call("Page.captureScreenshot", params)
	if err != nil {
		t.Fatalf("captureScreenshot(%s): %v", outPath, err)
	}
	var res struct {
		Data string `json:"data"`
	}
	if err := json.Unmarshal(raw, &res); err != nil {
		t.Fatalf("unmarshal screenshot: %v", err)
	}
	pngBytes, err := base64.StdEncoding.DecodeString(res.Data)
	if err != nil {
		t.Fatalf("decode screenshot base64: %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(outPath), 0o755); err != nil {
		t.Fatalf("mkdir screenshot dir: %v", err)
	}
	if err := os.WriteFile(outPath, pngBytes, 0o644); err != nil {
		t.Fatalf("write screenshot %s: %v", outPath, err)
	}
}

// TestDemoVisualAudit_AllPages runs headless Chrome against a live SeedDemoBook instance,
// auditing every Ghostbook page across Light, Sepia, Dark, Desktop (1440px, 1180px), and
// Mobile (390px) viewports for overflow, rendering errors, and external network leaks.
// Set DEMO_VISUAL_QA_DIR=/path/to/dir to export PNG screenshots for visual inspection.
func TestDemoVisualAudit_AllPages(t *testing.T) {
	ctx := context.Background()
	tmp := t.TempDir()

	st, err := store.Open(filepath.Join(tmp, "book_studio.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	var ts *httptest.Server
	eng, err := engine.NewBookEngineWithConfig(filepath.Join(tmp, "repo"), st, "http://127.0.0.1:8090", engine.EngineConfig{
		BookTitle:         "Atlas Systems Handbook",
		BookKicker:        "Interactive Demo",
		MCPName:           "ghostbook-demo",
		InitialCommitTime: time.Now().UTC().Truncate(time.Second).Add(-120 * time.Hour),
	})
	if err != nil {
		t.Fatalf("new engine: %v", err)
	}
	demoRes, err := engine.SeedDemoBook(ctx, eng)
	if err != nil {
		t.Fatalf("SeedDemoBook: %v", err)
	}

	ts = httptest.NewServer(server.NewHTTPHandler(eng, server.HTTPHandlerOptions{}))
	t.Cleanup(ts.Close)

	shotDir := os.Getenv("DEMO_VISUAL_QA_DIR")
	shot := func(name string) string {
		if shotDir == "" {
			return ""
		}
		return filepath.Join(shotDir, name)
	}

	cdp := startChromeCDP(t, ts.URL+"/")

	assertNoOverflow := func(label string) {
		t.Helper()
		res := cdp.evalString(t, `(() => {
			const sw = document.documentElement.scrollWidth;
			const cw = document.documentElement.clientWidth;
			if (sw > cw) return 'overflow:' + sw + '>' + cw;
			return 'ok';
		})()`)
		if res != "ok" {
			t.Errorf("%s horizontal overflow: %s", label, res)
		}
	}

	aliceAuth := demoRes.Authors[0].AuthIdentifier()
	diagramPath := fmt.Sprintf("/chapters/%s/rev/%s/diagrams/1", demoRes.DiagramChapterSlug, demoRes.DiagramChapterSHA)

	// 1. Home / Timeline (Light, Sepia, Dark, Mobile)
	cdp.setViewport(t, 1440, 900, false)
	cdp.navigate(t, ts.URL+"/")
	cdp.eval(t, `setReaderTheme('light')`)
	time.Sleep(150 * time.Millisecond)
	assertNoOverflow("home-desktop-light")
	cdp.screenshotWithOptions(t, shot("01-home-light-full.png"), true)

	cdp.eval(t, `setReaderTheme('sepia')`)
	time.Sleep(120 * time.Millisecond)
	cdp.screenshotWithOptions(t, shot("01-home-sepia.png"), false)

	cdp.eval(t, `setReaderTheme('dark')`)
	time.Sleep(120 * time.Millisecond)
	cdp.screenshotWithOptions(t, shot("01-home-dark.png"), false)

	cdp.eval(t, `setReaderTheme('light')`)
	cdp.setViewport(t, 390, 844, true)
	cdp.navigate(t, ts.URL+"/")
	assertNoOverflow("home-mobile")
	cdp.screenshotWithOptions(t, shot("01-home-mobile-full.png"), true)

	// 2. Chapter 1: Architecture (Mermaid flowchart + sequenceDiagram + table + callout)
	cdp.setViewport(t, 1440, 900, false)
	cdp.navigate(t, ts.URL+"/chapters/01-system-architecture")
	time.Sleep(450 * time.Millisecond)
	assertNoOverflow("ch1-desktop-light")
	if svgs := cdp.evalString(t, `String(document.querySelectorAll('.mermaid-container svg').length)`); svgs != "2" {
		t.Errorf("expected 2 rendered Mermaid SVGs in Chapter 1, got %s", svgs)
	}
	cdp.screenshotWithOptions(t, shot("02-chapter1-light-full.png"), true)

	cdp.eval(t, `setReaderTheme('dark')`)
	time.Sleep(200 * time.Millisecond)
	cdp.screenshotWithOptions(t, shot("02-chapter1-dark-full.png"), true)

	cdp.eval(t, `setReaderTheme('light')`)
	cdp.setViewport(t, 390, 844, true)
	cdp.navigate(t, ts.URL+"/chapters/01-system-architecture")
	time.Sleep(350 * time.Millisecond)
	assertNoOverflow("ch1-mobile")
	cdp.screenshotWithOptions(t, shot("02-chapter1-mobile.png"), false)
	cdp.eval(t, `toggleSidebar()`)
	time.Sleep(250 * time.Millisecond)
	cdp.screenshotWithOptions(t, shot("02-chapter1-mobile-toc.png"), false)
	cdp.eval(t, `closeMobileSidebar()`)

	// 3. Chapter 2: Math (KaTeX) + Code Block + Table
	cdp.setViewport(t, 1440, 900, false)
	cdp.navigate(t, ts.URL+"/chapters/02-agent-workflow")
	time.Sleep(350 * time.Millisecond)
	assertNoOverflow("ch2-desktop-light")
	if kErr := cdp.evalString(t, `String(document.querySelectorAll('.katex-error').length)`); kErr != "0" {
		t.Errorf("expected 0 KaTeX errors in Chapter 2, got %s", kErr)
	}
	cdp.screenshotWithOptions(t, shot("03-chapter2-light-full.png"), true)
	cdp.eval(t, `setReaderTheme('dark')`)
	time.Sleep(150 * time.Millisecond)
	cdp.screenshotWithOptions(t, shot("03-chapter2-dark.png"), false)
	cdp.eval(t, `setReaderTheme('light')`)

	// 4. Outdated revision permalink (with diff expanded)
	cdp.navigate(t, ts.URL+"/chapters/"+demoRes.OutdatedChapterSlug+"/rev/"+demoRes.OutdatedRevision)
	time.Sleep(300 * time.Millisecond)
	cdp.eval(t, `(() => { const d = document.getElementById('permalinkDiffDetails'); if (d) d.open = true; })()`)
	time.Sleep(120 * time.Millisecond)
	assertNoOverflow("permalink-outdated-light")
	cdp.screenshotWithOptions(t, shot("04-permalink-outdated-light.png"), false)
	cdp.eval(t, `setReaderTheme('dark')`)
	time.Sleep(120 * time.Millisecond)
	cdp.screenshotWithOptions(t, shot("04-permalink-outdated-dark.png"), false)
	cdp.eval(t, `setReaderTheme('light')`)

	// 5. Moved / Renamed chapter lineage banner
	cdp.navigate(t, ts.URL+"/chapters/"+demoRes.RenamedFromSlug)
	time.Sleep(200 * time.Millisecond)
	assertNoOverflow("moved-chapter")
	cdp.screenshotWithOptions(t, shot("05-moved-chapter-light.png"), false)

	// 6. Deleted / Retired draft chapter lineage banner
	cdp.navigate(t, ts.URL+"/chapters/"+demoRes.RetiredDraftSlug)
	time.Sleep(200 * time.Millisecond)
	assertNoOverflow("deleted-draft")
	cdp.screenshotWithOptions(t, shot("06-deleted-draft-light.png"), false)

	// 7. History & Blame (multi-author)
	cdp.navigate(t, ts.URL+"/chapters/01-system-architecture/history")
	time.Sleep(200 * time.Millisecond)
	assertNoOverflow("history-desktop-light")
	cdp.screenshotWithOptions(t, shot("07-history-light-full.png"), true)
	cdp.eval(t, `setReaderTheme('dark')`)
	time.Sleep(120 * time.Millisecond)
	cdp.screenshotWithOptions(t, shot("07-history-dark.png"), false)
	cdp.eval(t, `setReaderTheme('light')`)
	cdp.setViewport(t, 390, 844, true)
	cdp.navigate(t, ts.URL+"/chapters/01-system-architecture/history")
	assertNoOverflow("history-mobile")
	cdp.screenshotWithOptions(t, shot("07-history-mobile.png"), false)

	// 8. Standalone Diagram Viewer
	cdp.setViewport(t, 1440, 900, false)
	cdp.navigate(t, ts.URL+diagramPath)
	time.Sleep(350 * time.Millisecond)
	cdp.screenshotWithOptions(t, shot("08-diagram-standalone.png"), false)

	// 9. STYLE_GUIDE & Active Book Config Card
	cdp.navigate(t, ts.URL+"/STYLE_GUIDE")
	time.Sleep(200 * time.Millisecond)
	assertNoOverflow("style-guide-desktop")
	cdp.screenshotWithOptions(t, shot("09-style-guide-light-full.png"), true)
	cdp.eval(t, `setReaderTheme('dark')`)
	time.Sleep(120 * time.Millisecond)
	cdp.screenshotWithOptions(t, shot("09-style-guide-dark.png"), false)
	cdp.eval(t, `setReaderTheme('light')`)
	cdp.setViewport(t, 390, 844, true)
	cdp.navigate(t, ts.URL+"/STYLE_GUIDE")
	assertNoOverflow("style-guide-mobile")
	cdp.screenshotWithOptions(t, shot("09-style-guide-mobile.png"), false)

	// 10. Git Commit Log (/changelog)
	cdp.setViewport(t, 1440, 900, false)
	cdp.navigate(t, ts.URL+"/changelog")
	assertNoOverflow("changelog-desktop-1440")
	cdp.screenshotWithOptions(t, shot("10-changelog-light.png"), false)
	cdp.eval(t, `setReaderTheme('dark')`)
	time.Sleep(120 * time.Millisecond)
	cdp.screenshotWithOptions(t, shot("10-changelog-dark.png"), false)
	cdp.eval(t, `setReaderTheme('light')`)
	cdp.setViewport(t, 1180, 800, false)
	assertNoOverflow("changelog-laptop-1180")
	cdp.setViewport(t, 390, 844, true)
	cdp.navigate(t, ts.URL+"/changelog")
	assertNoOverflow("changelog-mobile")
	cdp.screenshotWithOptions(t, shot("10-changelog-mobile.png"), false)

	// 11. Agent Tool Audit Log (/activity)
	cdp.setViewport(t, 1440, 900, false)
	cdp.navigate(t, ts.URL+"/activity")
	assertNoOverflow("activity-desktop")
	cdp.screenshotWithOptions(t, shot("11-activity-light.png"), false)
	cdp.eval(t, `setReaderTheme('dark')`)
	time.Sleep(120 * time.Millisecond)
	cdp.screenshotWithOptions(t, shot("11-activity-dark.png"), false)
	cdp.eval(t, `setReaderTheme('light')`)
	cdp.setViewport(t, 390, 844, true)
	cdp.navigate(t, ts.URL+"/activity")
	assertNoOverflow("activity-mobile")
	cdp.screenshotWithOptions(t, shot("11-activity-mobile.png"), false)

	// 12. Full-Text Search (/search?q=quorum)
	cdp.setViewport(t, 1440, 900, false)
	cdp.navigate(t, ts.URL+"/search?q=quorum")
	assertNoOverflow("search-desktop")
	cdp.screenshotWithOptions(t, shot("12-search-light.png"), false)
	cdp.eval(t, `setReaderTheme('dark')`)
	time.Sleep(120 * time.Millisecond)
	cdp.screenshotWithOptions(t, shot("12-search-dark.png"), false)
	cdp.eval(t, `setReaderTheme('light')`)
	cdp.setViewport(t, 390, 844, true)
	cdp.navigate(t, ts.URL+"/search?q=quorum")
	assertNoOverflow("search-mobile")
	cdp.screenshotWithOptions(t, shot("12-search-mobile.png"), false)

	// 13. Personalized Co-Author Setup Guide (/u/{alice}/guide)
	cdp.setViewport(t, 1440, 900, false)
	cdp.navigate(t, ts.URL+"/u/"+aliceAuth+"/guide")
	time.Sleep(200 * time.Millisecond)
	assertNoOverflow("guide-desktop")
	cdp.screenshotWithOptions(t, shot("13-guide-light.png"), false)
	cdp.eval(t, `setReaderTheme('dark')`)
	time.Sleep(120 * time.Millisecond)
	cdp.screenshotWithOptions(t, shot("13-guide-dark.png"), false)
	cdp.eval(t, `setReaderTheme('light')`)
	cdp.setViewport(t, 390, 844, true)
	cdp.navigate(t, ts.URL+"/u/"+aliceAuth+"/guide")
	assertNoOverflow("guide-mobile")
	cdp.screenshotWithOptions(t, shot("13-guide-mobile.png"), false)

	// 14. US-Letter Print Edition (/print)
	cdp.setViewport(t, 1440, 900, false)
	cdp.navigate(t, ts.URL+"/print")
	time.Sleep(450 * time.Millisecond)
	assertNoOverflow("print-desktop")
	cdp.screenshotWithOptions(t, shot("14-print-top.png"), false)

	// 15. Interactive API Docs (/api-docs)
	cdp.navigate(t, ts.URL+"/api-docs")
	for attempt := 0; attempt < 25; attempt++ {
		ok := cdp.evalString(t, `String((document.getElementById('openapi-viewer-mount')?.innerText || '').includes('GetTableOfContents'))`)
		if ok == "true" {
			break
		}
		time.Sleep(150 * time.Millisecond)
	}
	cdp.screenshotWithOptions(t, shot("15-api-docs.png"), false)

	// Verify zero external network requests across the entire demo visual audit
	for _, u := range cdp.networkURLs {
		if strings.HasPrefix(u, "data:") || strings.HasPrefix(u, "blob:") || strings.HasPrefix(u, "chrome") || u == "about:blank" {
			continue
		}
		if !strings.HasPrefix(u, ts.URL) {
			t.Errorf("external network request leaked during demo audit: %s", u)
		}
	}
}
