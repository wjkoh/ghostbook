package server_test

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wjkoh/ghostbook/internal/engine"
	"github.com/wjkoh/ghostbook/internal/renderer"
	"golang.org/x/net/websocket"
)

const macChromeBin = "/Applications/Google Chrome.app/Contents/MacOS/Google Chrome"

type cdpClient struct {
	ws             *websocket.Conn
	nextID         atomic.Int64
	networkURLs    []string
	xhrOrFetchURLs []string
}

type cdpMessage struct {
	ID     int64           `json:"id,omitempty"`
	Method string          `json:"method,omitempty"`
	Params map[string]any  `json:"params,omitempty"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"error,omitempty"`
}

func startChromeCDP(t *testing.T, initialURL string) *cdpClient {
	t.Helper()
	if _, err := os.Stat(macChromeBin); err != nil {
		t.Skipf("Google Chrome not found at %s: %v", macChromeBin, err)
	}

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("allocate free port: %v", err)
	}
	debugPort := ln.Addr().(*net.TCPAddr).Port
	_ = ln.Close()

	userDataDir := filepath.Join(t.TempDir(), "chrome-profile")
	cmd := exec.Command(
		macChromeBin,
		"--headless=new",
		"--disable-gpu",
		"--no-first-run",
		"--no-default-browser-check",
		"--disable-extensions",
		"--remote-allow-origins=*",
		"--window-size=1440,900",
		fmt.Sprintf("--user-data-dir=%s", userDataDir),
		fmt.Sprintf("--remote-debugging-port=%d", debugPort),
		initialURL,
	)
	if err := cmd.Start(); err != nil {
		t.Fatalf("start chrome: %v", err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
	})

	listURL := fmt.Sprintf("http://127.0.0.1:%d/json/list", debugPort)
	var wsURL string
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		resp, err := http.Get(listURL)
		if err == nil {
			var targets []struct {
				Type                 string `json:"type"`
				URL                  string `json:"url"`
				WebSocketDebuggerURL string `json:"webSocketDebuggerUrl"`
			}
			_ = json.NewDecoder(resp.Body).Decode(&targets)
			_ = resp.Body.Close()
			for _, tgt := range targets {
				if tgt.Type == "page" && tgt.WebSocketDebuggerURL != "" {
					wsURL = tgt.WebSocketDebuggerURL
					break
				}
			}
			if wsURL != "" {
				break
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	if wsURL == "" {
		t.Fatalf("timed out waiting for Chrome DevTools page target on %s", listURL)
	}

	ws, err := websocket.Dial(wsURL, "", "http://127.0.0.1")
	if err != nil {
		t.Fatalf("dial cdp websocket %s: %v", wsURL, err)
	}
	t.Cleanup(func() { _ = ws.Close() })

	c := &cdpClient{ws: ws}
	_, _ = c.call("Page.enable", nil)
	_, _ = c.call("Runtime.enable", nil)
	_, _ = c.call("Network.enable", nil)
	return c
}

func (c *cdpClient) call(method string, params map[string]any) (json.RawMessage, error) {
	id := c.nextID.Add(1)
	req := cdpMessage{
		ID:     id,
		Method: method,
		Params: params,
	}
	if err := websocket.JSON.Send(c.ws, req); err != nil {
		return nil, fmt.Errorf("cdp send %s: %w", method, err)
	}
	for {
		var msg cdpMessage
		if err := websocket.JSON.Receive(c.ws, &msg); err != nil {
			return nil, fmt.Errorf("cdp recv %s: %w", method, err)
		}
		if msg.Method == "Network.requestWillBeSent" {
			resType, _ := msg.Params["type"].(string)
			if reqMap, ok := msg.Params["request"].(map[string]any); ok {
				if u, ok := reqMap["url"].(string); ok && u != "" {
					c.networkURLs = append(c.networkURLs, u)
					switch resType {
					case "XHR", "Fetch", "WebSocket", "EventSource":
						c.xhrOrFetchURLs = append(c.xhrOrFetchURLs, resType+":"+u)
					}
				}
			}
		}
		if msg.ID == id {
			if msg.Error != nil {
				return nil, fmt.Errorf("cdp error %s (%d): %s", method, msg.Error.Code, msg.Error.Message)
			}
			return msg.Result, nil
		}
	}
}

func (c *cdpClient) setViewport(t *testing.T, width, height int, mobile bool) {
	t.Helper()
	_, err := c.call("Emulation.setDeviceMetricsOverride", map[string]any{
		"width":             width,
		"height":            height,
		"deviceScaleFactor": 2,
		"mobile":            mobile,
	})
	if err != nil {
		t.Fatalf("setViewport(%dx%d): %v", width, height, err)
	}
	time.Sleep(150 * time.Millisecond)
}

func (c *cdpClient) navigate(t *testing.T, url string) {
	t.Helper()
	_, err := c.call("Page.navigate", map[string]any{"url": url})
	if err != nil {
		t.Fatalf("navigate(%s): %v", url, err)
	}
	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) {
		state := c.evalString(t, `document.readyState`)
		if state == "complete" {
			break
		}
		time.Sleep(80 * time.Millisecond)
	}
	time.Sleep(250 * time.Millisecond)
}

func (c *cdpClient) eval(t *testing.T, expr string) any {
	t.Helper()
	raw, err := c.call("Runtime.evaluate", map[string]any{
		"expression":    expr,
		"awaitPromise":  true,
		"returnByValue": true,
	})
	if err != nil {
		t.Fatalf("eval(%s): %v", expr, err)
	}
	var res struct {
		Result struct {
			Value any `json:"value"`
		} `json:"result"`
		ExceptionDetails *struct {
			Text string `json:"text"`
		} `json:"exceptionDetails"`
	}
	if err := json.Unmarshal(raw, &res); err != nil {
		t.Fatalf("unmarshal eval result: %v", err)
	}
	if res.ExceptionDetails != nil {
		t.Fatalf("JS exception in eval(%s): %s", expr, res.ExceptionDetails.Text)
	}
	return res.Result.Value
}

func (c *cdpClient) evalString(t *testing.T, expr string) string {
	t.Helper()
	v := c.eval(t, expr)
	if s, ok := v.(string); ok {
		return s
	}
	return fmt.Sprintf("%v", v)
}

func (c *cdpClient) screenshot(t *testing.T, outPath string) {
	t.Helper()
	if outPath == "" {
		return
	}
	raw, err := c.call("Page.captureScreenshot", map[string]any{
		"format": "png",
	})
	if err != nil {
		t.Fatalf("captureScreenshot: %v", err)
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

// UT-SSR-05: Static Source Audit on studio.js & diagram.html for Zero Browser Fetch / XHR / RPC Coupling
func TestStudioJS_ZeroBrowserFetchAndRPCCoupling_UT_SSR_05(t *testing.T) {
	js := renderer.StudioJS()
	for _, forbidden := range []string{
		"fetch(",
		"XMLHttpRequest",
		"sendBeacon",
		"BookService",
		"rpcCall",
		"/api/studio/",
		"revertToCommit",
		"openStudioDrawer",
		"openSearchModal",
	} {
		if strings.Contains(js, forbidden) {
			t.Fatalf("UT-SSR-05: studio.js still contains forbidden browser RPC/fetch token %q", forbidden)
		}
	}
}

// TestBrowserEndToEnd_BT01_to_BT05 runs BT-01 through BT-16 + UT-SSR-06 in a real headless Google Chrome instance.
func TestBrowserEndToEnd_BT01_to_BT05(t *testing.T) {
	ts, eng, st := setupTestServer(t)
	ctx := context.Background()

	carol, _ := st.ResolveAuthorName(ctx, "carol")
	alice, _ := st.ResolveAuthorName(ctx, "alice")

	screenshotDir := os.Getenv("BOOK_SCREENSHOT_DIR")
	shotPath := func(name string) string {
		if screenshotDir == "" {
			return filepath.Join(t.TempDir(), name)
		}
		return filepath.Join(screenshotDir, name)
	}

	cdp := startChromeCDP(t, ts.URL+"/")

	// =========================================================================
	// BT-01: ReaderLayoutAndSidebarTOC (Desktop 1440x900, Themes, Mobile 390x844)
	// =========================================================================
	t.Run("BT-01_ReaderLayoutAndSidebarTOC", func(t *testing.T) {
		cdp.setViewport(t, 1440, 900, false)
		cdp.navigate(t, ts.URL+"/chapters/01-system-architecture")

		title := cdp.evalString(t, `document.querySelector('h1').textContent`)
		if !strings.Contains(title, "System Architecture") {
			t.Fatalf("expected Chapter 1 H1 title, got %q", title)
		}

		// Verify active chapter automatically unfolds H2 section links (A., B., ...) in the left sidebar
		firstSectionText := cdp.evalString(t, `document.querySelector('#activeChapterOutline .toc-section-link').textContent`)
		if !strings.HasPrefix(firstSectionText, "A. ") {
			t.Fatalf("expected active chapter H2 section link in sidebar to start with 'A. ', got %q", firstSectionText)
		}

		// Verify zero horizontal overflow on desktop
		overflow := cdp.evalString(t, `String(document.documentElement.scrollWidth > document.documentElement.clientWidth)`)
		if overflow != "false" {
			t.Fatalf("unexpected horizontal overflow on desktop 1440x900")
		}
		cdp.screenshot(t, shotPath("bt01-desktop-light.png"))

		// Switch to Sepia theme, Dark theme, and System auto-detection
		cdp.eval(t, `setReaderTheme('sepia')`)
		themeAttr := cdp.evalString(t, `document.documentElement.getAttribute('data-theme')`)
		if themeAttr != "sepia" {
			t.Fatalf("expected sepia theme attribute, got %q", themeAttr)
		}
		cdp.screenshot(t, shotPath("bt01-desktop-sepia.png"))

		cdp.eval(t, `setReaderTheme('dark')`)
		cdp.screenshot(t, shotPath("bt01-desktop-dark.png"))

		// Verify default "system" theme follows OS prefers-color-scheme: dark and light
		_, _ = cdp.call("Emulation.setEmulatedMedia", map[string]any{
			"features": []map[string]string{{"name": "prefers-color-scheme", "value": "dark"}},
		})
		cdp.eval(t, `setReaderTheme('system')`)
		if sysDark := cdp.evalString(t, `document.documentElement.getAttribute('data-theme')`); sysDark != "dark" {
			t.Fatalf("expected system theme to resolve to dark when OS prefers-color-scheme is dark, got %q", sysDark)
		}
		_, _ = cdp.call("Emulation.setEmulatedMedia", map[string]any{
			"features": []map[string]string{{"name": "prefers-color-scheme", "value": "light"}},
		})
		cdp.eval(t, `setReaderTheme('system')`)
		if sysLight := cdp.evalString(t, `document.documentElement.getAttribute('data-theme')`); sysLight != "light" {
			t.Fatalf("expected system theme to live-update to light when OS prefers-color-scheme changes to light, got %q", sysLight)
		}

		// Verify mobile 390x844 responsive layout & zero horizontal overflow across Chapter, Guide, and Changelog
		cdp.setViewport(t, 390, 844, true)
		cdp.navigate(t, ts.URL+"/chapters/01-system-architecture")
		mobileOverflow := cdp.evalString(t, `String(document.documentElement.scrollWidth > document.documentElement.clientWidth)`)
		if mobileOverflow != "false" {
			t.Fatalf("unexpected horizontal overflow on mobile 390x844 chapter view")
		}
		cdp.screenshot(t, shotPath("bt01-mobile-chapter.png"))

		cdp.eval(t, `toggleSidebar()`)
		time.Sleep(250 * time.Millisecond)
		cdp.screenshot(t, shotPath("bt01-mobile-toc.png"))
		cdp.eval(t, `closeMobileSidebar()`)

		cdp.navigate(t, ts.URL+"/guide")
		guideMobileOverflow := cdp.evalString(t, `String(document.documentElement.scrollWidth > document.documentElement.clientWidth)`)
		if guideMobileOverflow != "false" {
			t.Fatalf("unexpected horizontal overflow on mobile 390x844 /guide view")
		}
		cdp.screenshot(t, shotPath("bt01-mobile-guide.png"))

		cdp.navigate(t, ts.URL+"/changelog")
		changelogMobileOverflow := cdp.evalString(t, `String(document.documentElement.scrollWidth > document.documentElement.clientWidth)`)
		if changelogMobileOverflow != "false" {
			t.Fatalf("unexpected horizontal overflow on mobile 390x844 /changelog view")
		}
		cdp.screenshot(t, shotPath("bt01-mobile-changelog.png"))

		cdp.setViewport(t, 1440, 900, false)
		cdp.navigate(t, ts.URL+"/changelog")
		for _, deskW := range []int{1100, 1280, 1440} {
			cdp.setViewport(t, deskW, 900, false)
			ledgerBounds := cdp.evalString(t, `(() => {
				const tbl = document.getElementById('rawCommitLedger');
				const main = document.querySelector('main.chapter-container');
				if (!tbl || !main) return 'missing';
				if (document.documentElement.scrollWidth > document.documentElement.clientWidth) {
					return 'doc-overflow:' + document.documentElement.scrollWidth + '>' + document.documentElement.clientWidth;
				}
				const tr = tbl.getBoundingClientRect();
				const mr = main.getBoundingClientRect();
				if (tr.right > mr.right + 1) {
					return 'table-overflows-container:' + tr.right + '>' + mr.right;
				}
				return 'ok';
			})()`)
			if ledgerBounds != "ok" {
				t.Fatalf("desktop %dpx /changelog overflow check failed: %s", deskW, ledgerBounds)
			}
		}
		cdp.screenshot(t, shotPath("bt01-desktop-changelog.png"))

		// Verify Chapter 2 KaTeX math rendering (inline & display) and Mermaid diagram
		cdp.navigate(t, ts.URL+"/chapters/02-agent-workflow")
		time.Sleep(300 * time.Millisecond)
		katexCount := cdp.evalString(t, `String(document.querySelectorAll('.katex').length)`)
		katexDisplayCount := cdp.evalString(t, `String(document.querySelectorAll('.katex-display').length)`)
		katexErrorCount := cdp.evalString(t, `String(document.querySelectorAll('.katex-error').length)`)
		if katexCount == "0" || katexDisplayCount == "0" {
			t.Fatalf("expected KaTeX inline & display equations on Chapter 2, got katex=%s display=%s", katexCount, katexDisplayCount)
		}
		if katexErrorCount != "0" {
			t.Fatalf("expected 0 KaTeX errors on Chapter 2, got %s", katexErrorCount)
		}
		cdp.screenshot(t, shotPath("bt01-chapter2-math-mermaid.png"))

		// Verify sidebar TOC items never disappear when navigating between Chapter, Guide, and Changelog pages
		evalSidebarHrefs := `Array.from(document.querySelectorAll('#bookSidebar .toc-item')).map(a => a.getAttribute('href')).join(',')`
		chapterHrefs := cdp.evalString(t, evalSidebarHrefs)
		if !strings.Contains(chapterHrefs, "/guide") || !strings.Contains(chapterHrefs, "/changelog") || !strings.Contains(chapterHrefs, "/print") {
			t.Fatalf("expected all studio links in chapter sidebar, got %q", chapterHrefs)
		}

		cdp.navigate(t, ts.URL+"/guide")
		guideHrefs := cdp.evalString(t, evalSidebarHrefs)
		if guideHrefs != chapterHrefs {
			t.Fatalf("sidebar links drifted on /guide:\ngot:  %s\nwant: %s", guideHrefs, chapterHrefs)
		}

		cdp.navigate(t, ts.URL+"/changelog")
		changelogHrefs := cdp.evalString(t, evalSidebarHrefs)
		if changelogHrefs != chapterHrefs {
			t.Fatalf("sidebar links drifted on /changelog:\ngot:  %s\nwant: %s", changelogHrefs, chapterHrefs)
		}
	})

	// =========================================================================
	// BT-02: AI-Agent-First Pure SSR Web UI & Engine Surgical Patch Verification
	// =========================================================================
	t.Run("BT-02_BrowserSurgicalSectionEdit", func(t *testing.T) {
		cdp.navigate(t, ts.URL+"/u/"+carol.AuthIdentifier()+"/chapters/01-system-architecture")
		beforeRev := cdp.evalString(t, `document.getElementById('chapterRevBadge').textContent.trim()`)

		// Verify web edit buttons, RPC drawers, and JS search modals are absent (pure SSR AI-agent-first web interface)
		hasWebAuthoringControls := cdp.evalString(t, `String(!!(document.getElementById('activeAuthorSelect') || document.getElementById('btnOpenEditModal') || document.getElementById('studioDrawer') || document.getElementById('searchModalBackdrop')))`)
		if hasWebAuthoringControls != "false" {
			t.Fatalf("expected web authoring controls, JS drawer, and JS search modal to be absent in pure SSR UI")
		}

		// Perform an authenticated server-side patch as Carol Park (simulating MCP / ConnectRPC agent write)
		if _, err := eng.PatchChapter(engine.WithAuthor(ctx, carol), &engine.PatchChapterInput{
			Slug:          "01-system-architecture",
			OldText:       "embedded SQLite FTS5 search",
			NewText:       "embedded SQLite FTS5 BM25 search",
			CommitMessage: "Annotate SQLite FTS5 BM25 via agent",
		}); err != nil {
			t.Fatalf("PatchChapter: %v", err)
		}

		cdp.navigate(t, ts.URL+"/chapters/01-system-architecture")
		afterRev := cdp.evalString(t, `document.getElementById('chapterRevBadge').textContent.trim()`)
		if afterRev == beforeRev {
			t.Fatalf("expected chapter revision badge to update after surgical edit (stayed %q)", afterRev)
		}
		bodyText := cdp.evalString(t, `document.getElementById('chapterProse').textContent`)
		if !strings.Contains(bodyText, "embedded SQLite FTS5 BM25 search") {
			t.Fatalf("expected updated prose in chapter view after surgical patch")
		}
	})

	// =========================================================================
	// BT-03 & UT-SSR-06a: Pure SSR Chapter History & Blame Page (/chapters/{slug}/history)
	// =========================================================================
	t.Run("BT-03_SSRChapterHistoryAndBlame", func(t *testing.T) {
		cdp.setViewport(t, 1440, 900, false)
		cdp.navigate(t, ts.URL+"/chapters/01-system-architecture")
		histHref := cdp.evalString(t, `document.getElementById('linkChapterHistory').getAttribute('href')`)
		if histHref != "/chapters/01-system-architecture/history" {
			t.Fatalf("expected History & Blame link href='/chapters/01-system-architecture/history', got %q", histHref)
		}

		cdp.navigate(t, ts.URL+histHref)
		blameText := cdp.evalString(t, `document.getElementById('ssrBlameList').textContent`)
		if !strings.Contains(blameText, "Carol Park") && !strings.Contains(blameText, "Bob Rivera") {
			t.Fatalf("expected author attribution on SSR history page, got: %q", blameText)
		}
		diffText := cdp.evalString(t, `document.getElementById('ssrCommitsList').textContent`)
		if !strings.Contains(diffText, "embedded SQLite FTS5 BM25 search") {
			t.Fatalf("expected unified diff preview on SSR history page, got: %q", diffText)
		}
		// Verify native <details> diff is expanded on latest commit with zero horizontal overflow on 375px mobile
		cdp.setViewport(t, 375, 812, true)
		if ov := cdp.evalString(t, `String(document.documentElement.scrollWidth > document.documentElement.clientWidth)`); ov != "false" {
			t.Fatalf("unexpected mobile overflow on SSR history page")
		}
		cdp.setViewport(t, 1440, 900, false)
		cdp.screenshot(t, shotPath("bt03-ssr-history-diffs-blame.png"))
	})

	// =========================================================================
	// BT-04 & UT-SSR-06b: Cmd+K Focus & Pure SSR Full-Text Search (/search?q=...)
	// =========================================================================
	t.Run("BT-04_CmdKAndSSRFullTextSearch", func(t *testing.T) {
		for _, pagePath := range []string{"/chapters/01-system-architecture", "/changelog", "/guide"} {
			cdp.navigate(t, ts.URL+pagePath)
			cdp.eval(t, `focusSidebarOrPageSearch()`)
			activeID := cdp.evalString(t, `document.activeElement ? document.activeElement.id : ''`)
			if activeID != "sidebarSearchInput" {
				t.Fatalf("expected Cmd+K helper on %s to focus #sidebarSearchInput, got %q", pagePath, activeID)
			}
		}

		// Navigate via GET /search?q=BM25 and verify server-rendered <mark> highlight & exact line match
		cdp.navigate(t, ts.URL+"/search?q=BM25")
		resultsHTML := cdp.evalString(t, `document.getElementById('ssrSearchResults').innerHTML`)
		if !strings.Contains(resultsHTML, "<mark>") {
			t.Fatalf("expected <mark> highlighted FTS5 search result for BM25 on /search, got: %s", resultsHTML)
		}
		cdp.screenshot(t, shotPath("bt04-ssr-fts5-search.png"))

		cdp.navigate(t, ts.URL+"/search?q=ConnectRPC")
		polyHTML := cdp.evalString(t, `document.getElementById('ssrSearchResults').innerHTML`)
		if !strings.Contains(polyHTML, "appendix-a-mcp-reference") {
			t.Fatalf("expected /search?q=ConnectRPC to find appendix-a-mcp-reference, got: %s", polyHTML)
		}

		// Verify 375px mobile layout on /search and /activity
		cdp.setViewport(t, 375, 812, true)
		if ov := cdp.evalString(t, `String(document.documentElement.scrollWidth > document.documentElement.clientWidth)`); ov != "false" {
			t.Fatalf("unexpected mobile overflow on /search")
		}
		cdp.navigate(t, ts.URL+"/activity")
		if ov := cdp.evalString(t, `String(document.documentElement.scrollWidth > document.documentElement.clientWidth)`); ov != "false" {
			t.Fatalf("unexpected mobile overflow on /activity")
		}
		cdp.setViewport(t, 1440, 900, false)
	})

	// =========================================================================
	// BT-05: PrintView
	// =========================================================================
	t.Run("BT-05_PrintView", func(t *testing.T) {
		cdp.navigate(t, ts.URL+"/print")
		printText := cdp.evalString(t, `document.body.textContent`)
		if !strings.Contains(printText, "US-Letter Grayscale-Laser-Printer Edition") {
			t.Fatalf("expected US-Letter print header on /print")
		}
		tocExists := cdp.evalString(t, `String(!!document.getElementById('printToc'))`)
		if tocExists != "true" {
			t.Fatalf("expected #printToc Table of Contents section on /print")
		}
		chapterSections := cdp.evalString(t, `String(document.querySelectorAll('.print-chapter').length)`)
		if chapterSections != "7" {
			t.Fatalf("expected 7 chapters rendered sequentially on /print, got %s", chapterSections)
		}
		cdp.screenshot(t, shotPath("bt05-us-letter-print-view.png"))
	})

	// =========================================================================
	// BT-06 to BT-12: Zero-CDN Network Verification, Self-Hosted Fonts, & Mermaid Pan/Pinch/Zoom Lightbox
	// =========================================================================
	t.Run("BT-06_to_BT-12_ZeroCDNAndInteractiveMermaidLightbox", func(t *testing.T) {
		cdp.setViewport(t, 1440, 900, false)

		// BT-06: Visit all 5 studio/reference page types and verify 0 external network requests leave 127.0.0.1
		for _, pagePath := range []string{
			"/chapters/02-agent-workflow",
			"/print",
			"/guide",
			"/changelog",
			"/api-docs",
		} {
			cdp.navigate(t, ts.URL+pagePath)
			resNames := cdp.evalString(t, `performance.getEntriesByType('resource').map(r => r.name).join('\n')`)
			for _, line := range strings.Split(resNames, "\n") {
				u := strings.TrimSpace(line)
				if u == "" || strings.HasPrefix(u, "data:") || strings.HasPrefix(u, "blob:") {
					continue
				}
				if !strings.HasPrefix(u, ts.URL) {
					t.Fatalf("BT-06: page %s made external subresource request to %q (want only %s)", pagePath, u, ts.URL)
				}
			}
		}
		for _, u := range cdp.networkURLs {
			if strings.HasPrefix(u, "data:") || strings.HasPrefix(u, "blob:") || strings.HasPrefix(u, "chrome") || u == "about:blank" {
				continue
			}
			if !strings.HasPrefix(u, ts.URL) {
				t.Fatalf("BT-06: CDP Network.requestWillBeSent captured external request %q (want only %s)", u, ts.URL)
			}
		}

		// Verify self-hosted Scalar mounted /openapi.json inside #openapi-viewer-mount with zero external CDN/font/proxy requests
		var scalarRendered string
		for attempt := 0; attempt < 25; attempt++ {
			scalarRendered = cdp.evalString(t, `(() => {
				const mount = document.getElementById('openapi-viewer-mount');
				if (!mount) return 'no-mount';
				const txt = mount.innerText || mount.textContent || '';
				if (txt.includes('Ghostbook') && (txt.includes('GetTableOfContents') || txt.includes('Return Table of Contents'))) {
					return 'ok';
				}
				return 'waiting:' + txt.slice(0, 120);
			})()`)
			if scalarRendered == "ok" {
				break
			}
			time.Sleep(150 * time.Millisecond)
		}
		if scalarRendered != "ok" {
			t.Fatalf("BT-06: expected self-hosted Scalar to render OpenAPI 3.1 reference inside #openapi-viewer-mount, got %q", scalarRendered)
		}
		// Re-check network requests after Scalar finishes async mount & /openapi.json fetch
		for _, u := range cdp.networkURLs {
			if strings.HasPrefix(u, "data:") || strings.HasPrefix(u, "blob:") || strings.HasPrefix(u, "chrome") || u == "about:blank" {
				continue
			}
			if !strings.HasPrefix(u, ts.URL) {
				t.Fatalf("BT-06: post-Scalar CDP Network.requestWillBeSent captured external request %q (want only %s)", u, ts.URL)
			}
		}

		// BT-07: Verify KaTeX self-hosted WOFF2 math fonts & equation rendering on Chapter 2
		cdp.navigate(t, ts.URL+"/chapters/02-agent-workflow")
		time.Sleep(350 * time.Millisecond)
		fontCheck := cdp.evalString(t, `(async () => {
			await document.fonts.ready;
			return String(document.fonts.check('16px KaTeX_Main'));
		})()`)
		if fontCheck != "true" {
			t.Fatalf("BT-07: expected self-hosted KaTeX_Main WOFF2 font to be loaded and verified, got %s", fontCheck)
		}

		// BT-08: Verify inline Mermaid SVG rendering, native <a href="/chapters/.../diagrams/1"> link, and .mermaid-expand-badge
		hasMermaidSVG := cdp.evalString(t, `String(!!document.querySelector('.mermaid-container a.mermaid-diagram-link svg') && document.querySelector('.mermaid-container svg').getBoundingClientRect().width > 200)`)
		if hasMermaidSVG != "true" {
			t.Fatalf("BT-08: expected rendered .mermaid-container a.mermaid-diagram-link svg with width > 200px")
		}
		linkHref := cdp.evalString(t, `document.querySelector('.mermaid-container a.mermaid-diagram-link').getAttribute('href')`)
		activeRev := cdp.evalString(t, `document.body.getAttribute('data-active-rev')`)
		expectedDiagramHref := "/chapters/02-agent-workflow/rev/" + activeRev + "/diagrams/1"
		if linkHref != expectedDiagramHref {
			t.Fatalf("BT-08: expected a.mermaid-diagram-link href=%q, got %q", expectedDiagramHref, linkHref)
		}
		badgeText := cdp.evalString(t, `document.querySelector('.mermaid-container .mermaid-expand-badge').textContent.trim()`)
		if !strings.Contains(badgeText, "Open Diagram") {
			t.Fatalf("BT-08: expected .mermaid-expand-badge on .mermaid-container, got %q", badgeText)
		}

		// BT-09..12: Theme switching survival, @media print hiding, and standalone white-canvas diagram rendering
		cdp.eval(t, `setReaderTheme('dark')`)
		time.Sleep(150 * time.Millisecond)
		badgeSurvived := cdp.evalString(t, `String(!!document.querySelector('.mermaid-container .mermaid-expand-badge') && !!document.querySelector('.mermaid-container a.mermaid-diagram-link svg') && getComputedStyle(document.querySelector('.mermaid-container')).backgroundColor === 'rgb(255, 255, 255)')`)
		darkHref := cdp.evalString(t, `document.querySelector('.mermaid-container a.mermaid-diagram-link').getAttribute('href')`)
		if badgeSurvived != "true" || darkHref != expectedDiagramHref {
			t.Fatalf("BT-12: expected .mermaid-container to keep #ffffff background and href=%q in dark mode, got survived=%s href=%q", expectedDiagramHref, badgeSurvived, darkHref)
		}
		cdp.eval(t, `setReaderTheme('light')`)
		time.Sleep(150 * time.Millisecond)

		// Emulate @media print at US-Letter width (816px) and verify .book-sidebar, .studio-topbar, and .mermaid-expand-badge are display:none
		cdp.setViewport(t, 816, 1056, false)
		_, _ = cdp.call("Emulation.setEmulatedMedia", map[string]any{"media": "print"})
		printSidebarDisplay := cdp.evalString(t, `getComputedStyle(document.getElementById('bookSidebar')).display`)
		printBadgeDisplay := cdp.evalString(t, `getComputedStyle(document.querySelector('.mermaid-expand-badge')).display`)
		if printSidebarDisplay != "none" || printBadgeDisplay != "none" {
			t.Fatalf("unexpected @media print styles: sidebar=%s badge=%s", printSidebarDisplay, printBadgeDisplay)
		}
		_, _ = cdp.call("Emulation.setEmulatedMedia", map[string]any{"media": "screen"})

		// Navigate browser to /chapters/02-agent-workflow/rev/{sha}/diagrams/1 and verify standalone white-canvas SVG rendering with zero POST write-back
		cdp.setViewport(t, 1440, 900, false)
		cdp.navigate(t, ts.URL+expectedDiagramHref)
		time.Sleep(300 * time.Millisecond)
		standaloneSVG := cdp.evalString(t, `String(!!document.querySelector('#standaloneMermaidPre svg') && getComputedStyle(document.body).backgroundColor === 'rgb(255, 255, 255)')`)
		if standaloneSVG != "true" {
			t.Fatalf("expected standalone diagram page to render SVG on #ffffff canvas without network write-back")
		}
		cdp.screenshot(t, shotPath("bt09-mermaid-native-svg.png"))

		// Verify minimal permalink banner on outdated revision (r1 of 01-system-architecture) in both Light and Dark themes
		cdp.navigate(t, ts.URL+"/chapters/01-system-architecture/rev/r1")
		time.Sleep(250 * time.Millisecond)
		for _, th := range []string{"light", "dark"} {
			cdp.eval(t, fmt.Sprintf("setReaderTheme(%q)", th))
			bannerCheck := cdp.evalString(t, `(() => {
				const b = document.getElementById('permalinkLineageBanner');
				const btn = document.getElementById('btnPermalinkViewLatest');
				if (!b || !btn) return 'missing';
				const cs = getComputedStyle(btn);
				const rect = b.getBoundingClientRect();
				const noRestore = !b.innerText.includes('Restore');
				return String(noRestore && rect.height <= 80 && cs.color !== cs.backgroundColor && cs.textDecorationLine === 'none');
			})()`)
			if bannerCheck != "true" {
				t.Fatalf("permalink banner check failed in theme %s: got %s", th, bannerCheck)
			}
		}
	})

	// =========================================================================
	// BT-13 to BT-16: HTML Agent Discovery Tags, JSON-LD, Fetch Content Negotiation, & Raw Markdown Rendering
	// =========================================================================
	t.Run("BT-13_to_BT-16_AgentDiscoveryJSONLDAndMarkdownNegotiation", func(t *testing.T) {
		cdp.setViewport(t, 1440, 900, false)

		// Create a fresh chapter with a Mermaid diagram so diagram.html?noreplace=1 can be inspected before SVG caching
		if _, err := eng.CreateChapter(context.Background(), &engine.CreateChapterInput{
			Slug:            "90-agent-diagram-check",
			Title:           "Chapter 90: Agent Diagram Check",
			InitialMarkdown: "# Chapter 90: Agent Diagram Check\n\n```mermaid\nflowchart LR\n  Agent[\"AI Agent\"] --> MD[\"Raw Markdown\"]\n```\n",
			CommitMessage:   "Add chapter 90 for BT-13 diagram.html discovery verification",
		}); err != nil {
			t.Fatalf("CreateChapter 90: %v", err)
		}

		ch1Info, err := eng.ReadChapter(context.Background(), &engine.ReadChapterInput{Slug: "01-system-architecture", Revision: "r1"})
		if err != nil {
			t.Fatalf("ReadChapter ch1 r1: %v", err)
		}
		ch90Info, err := eng.ReadChapter(context.Background(), &engine.ReadChapterInput{Slug: "90-agent-diagram-check"})
		if err != nil {
			t.Fatalf("ReadChapter ch90: %v", err)
		}

		// BT-13 & BT-16: Verify <link>, <meta>, JSON-LD, and 0x0 visual footprint across all 6 HTML templates
		discoveryPages := []struct {
			path       string
			wantMDHref string
			wantLDType string
		}{
			{"/chapters/01-system-architecture", "/chapters/01-system-architecture.md", "TechArticle"},
			{"/chapters/01-system-architecture/rev/r1", "/rev/" + ch1Info.RevisionSHA + "/chapters/01-system-architecture.md", "TechArticle"},
			{"/print", "/book.md", "Book"},
			{"/guide", "/guide.md", "TechArticle"},
			{"/changelog", "/changelog.md", "TechArticle"},
			{"/api-docs", "/guide.md", "WebAPI"},
			{"/chapters/90-agent-diagram-check/diagrams/1?noreplace=1", "/chapters/90-agent-diagram-check/rev/" + ch90Info.RevisionSHA + "/diagrams/1.mmd", "ImageObject"},
		}

		for _, dp := range discoveryPages {
			cdp.navigate(t, ts.URL+dp.path)
			time.Sleep(150 * time.Millisecond)

			checkResult := cdp.evalString(t, fmt.Sprintf(`(() => {
				const llms = document.querySelector('link[rel="llms-txt"]');
				const llmsFull = document.querySelector('link[rel="llms-full-txt"]');
				const alt = document.querySelector('link[rel="alternate"][type="text/markdown"]');
				const meta = document.querySelector('meta[name="markdown-url"]');
				const ldEl = document.getElementById('agentDiscoveryJsonLd');
				if (!llms || llms.getAttribute('href') !== '/llms.txt') return 'bad-llms';
				if (!llmsFull || llmsFull.getAttribute('href') !== '/llms-full.txt') return 'bad-llms-full';
				if (!alt || alt.getAttribute('href') !== %q) return 'bad-alt:' + (alt ? alt.getAttribute('href') : 'null');
				if (!meta || meta.getAttribute('content') !== %q) return 'bad-meta:' + (meta ? meta.getAttribute('content') : 'null');
				if (!ldEl) return 'missing-jsonld';
				const rect = ldEl.getBoundingClientRect();
				if (rect.width !== 0 || rect.height !== 0) return 'visible-jsonld';
				let parsed;
				try { parsed = JSON.parse(ldEl.textContent); } catch (e) { return 'invalid-json:' + e.message; }
				if (parsed['@context'] !== 'https://schema.org' || parsed['@type'] !== %q) return 'bad-ld-type:' + parsed['@type'];
				return 'ok';
			})()`, dp.wantMDHref, dp.wantMDHref, dp.wantLDType))
			if checkResult != "ok" {
				t.Fatalf("BT-13/BT-16 failed on %s: %s", dp.path, checkResult)
			}
		}

		// BT-14 & UT-SSR-06c: Verify browser fetch() is strictly blocked by CSP connect-src 'none', while HTTP Accept: text/markdown works natively
		cdp.navigate(t, ts.URL+"/u/"+alice.AuthIdentifier()+"/chapters/01-system-architecture")
		time.Sleep(150 * time.Millisecond)
		for _, u := range cdp.xhrOrFetchURLs {
			if u != "Fetch:"+ts.URL+"/openapi.json" {
				t.Fatalf("UT-SSR-06: expected ZERO book-reader XHR/Fetch/WebSocket calls (only standalone Scalar /openapi.json allowed), got %v", cdp.xhrOrFetchURLs)
			}
		}
		cspBlockedFetch := cdp.evalString(t, `(async () => {
			try {
				await fetch('/openapi.json');
				return 'unexpected-fetch-allowed';
			} catch (e) {
				return 'blocked-by-csp';
			}
		})()`)
		if cspBlockedFetch != "blocked-by-csp" {
			t.Fatalf("UT-SSR-06: expected CSP connect-src 'none' to block in-page fetch(), got %s", cspBlockedFetch)
		}

		// BT-15: Direct Chrome navigation to /llms.txt and /chapters/01-system-architecture.md renders raw text/markdown with zero HTML sniffing
		for _, rawPath := range []string{"/llms.txt", "/chapters/01-system-architecture.md"} {
			cdp.navigate(t, ts.URL+rawPath)
			time.Sleep(150 * time.Millisecond)
			docMime := cdp.evalString(t, `document.contentType`)
			if docMime != "text/markdown" {
				t.Fatalf("BT-15: expected Chrome document.contentType === 'text/markdown' on %s, got %q", rawPath, docMime)
			}
		}

		// Re-verify zero external network requests across all BT-13..BT-16 navigations
		for _, u := range cdp.networkURLs {
			if strings.HasPrefix(u, "data:") || strings.HasPrefix(u, "blob:") || strings.HasPrefix(u, "chrome") || u == "about:blank" {
				continue
			}
			if !strings.HasPrefix(u, ts.URL) {
				t.Fatalf("BT-16: unexpected external network request %q (want only %s)", u, ts.URL)
			}
		}
	})

	t.Run("UT-TL-06_FrontPageAndChangelogTimelineDesktopAndMobileLayout", func(t *testing.T) {
		mockGemini := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{
				"candidates": [{
					"content": {
						"parts": [{"text": "{\"milestones\":[{\"revisions\":[3,4],\"title\":\"pure-Go Git Custody Economics\",\"summary\":\"Updated institutional credit yield targets and custody fee tier assumptions across two sleeves.\",\"chapter_slugs\":[\"01-system-architecture\",\"02-agent-workflow\"],\"authors\":[\"Alice Chen\",\"Bob Rivera\"]},{\"revisions\":[2],\"title\":\"Tool-First Onboarding Sequence\",\"summary\":\"Clarified zero-migration portfolio ingestion steps ahead of balance-sheet integration.\",\"chapter_slugs\":[\"04-deployment-ops\"],\"authors\":[\"Carol Park\"]}]}"}]
					}
				}]
			}`))
		}))
		defer mockGemini.Close()

		eng.SetBriefSynthesizer(engine.NewBriefSynthesizer("gm_cdp_test", "gemini-3-flash-preview", mockGemini.URL))
		if _, err := eng.SynthesizeBriefingIfNeeded(context.Background()); err != nil {
			t.Fatalf("SynthesizeBriefingIfNeeded: %v", err)
		}

		// Desktop 1440x900 check on / + local <time> formatting + dark theme + collapse/expand
		_, _ = cdp.call("Emulation.setDeviceMetricsOverride", map[string]any{
			"width":             1440,
			"height":            900,
			"deviceScaleFactor": 1,
			"mobile":            false,
		})
		cdp.navigate(t, ts.URL+"/")
		time.Sleep(200 * time.Millisecond)

		desktopCheck := cdp.evalString(t, `(() => {
			const root = document.getElementById('frontPageBriefing');
			const feed = document.getElementById('frontPageTimeline');
			if (!root || !feed) return 'missing-top-level-timeline';
			if (root.querySelector('details')) return 'unexpected-card-details-wrapper';
			const entries = feed.querySelectorAll('.timeline-entry');
			if (entries.length !== 2) return 'bad-entry-count:' + entries.length;
			const timeEl = feed.querySelector('time.timeline-time');
			if (!timeEl || !timeEl.textContent.trim()) return 'missing-time';
			const activeSidebar = document.querySelector('.book-sidebar .toc-item.active');
			if (!activeSidebar || activeSidebar.getAttribute('href') !== '/timeline') return 'bad-active-sidebar';
			if (document.documentElement.scrollWidth > document.documentElement.clientWidth) {
				return 'desktop-overflow:' + document.documentElement.scrollWidth + '>' + document.documentElement.clientWidth;
			}
			setReaderTheme('dark');
			return 'ok';
		})()`)
		if desktopCheck != "ok" {
			t.Fatalf("UT-TL-06 desktop check failed: %s", desktopCheck)
		}

		// Mobile 375x812 viewport overflow check on both / and /changelog
		_, _ = cdp.call("Emulation.setDeviceMetricsOverride", map[string]any{
			"width":             375,
			"height":            812,
			"deviceScaleFactor": 2,
			"mobile":            true,
		})
		time.Sleep(150 * time.Millisecond)
		mobileHomeCheck := cdp.evalString(t, `(() => {
			const card = document.getElementById('frontPageBriefing');
			if (!card) return 'missing-card';
			const rect = card.getBoundingClientRect();
			if (rect.width <= 0 || rect.right > window.innerWidth + 1) {
				return 'card-bounds:' + rect.right + '>' + window.innerWidth;
			}
			if (document.documentElement.scrollWidth > document.documentElement.clientWidth) {
				return 'mobile-overflow:' + document.documentElement.scrollWidth + '>' + document.documentElement.clientWidth;
			}
			return 'ok';
		})()`)
		if mobileHomeCheck != "ok" {
			t.Fatalf("UT-TL-06 mobile 375px check on / failed: %s", mobileHomeCheck)
		}

		cdp.navigate(t, ts.URL+"/changelog")
		time.Sleep(150 * time.Millisecond)
		changelogCheck := cdp.evalString(t, `(() => {
			const h1 = document.querySelector('h1');
			const tbl = document.getElementById('rawCommitLedger');
			if (!h1 || !h1.textContent.includes('Git Commit Log') || !tbl) return 'missing-commit-log';
			if (document.querySelector('.timeline-feed')) return 'unexpected-duplicate-timeline';
			if (document.documentElement.scrollWidth > document.documentElement.clientWidth) {
				return 'changelog-mobile-overflow:' + document.documentElement.scrollWidth + '>' + document.documentElement.clientWidth;
			}
			return 'ok';
		})()`)
		if changelogCheck != "ok" {
			t.Fatalf("UT-TL-06 /changelog mobile check failed: %s", changelogCheck)
		}

		// E2E-CDP-01: Navigate to /STYLE_GUIDE via sidebar link and verify governance card + zero overflow
		_, _ = cdp.call("Emulation.setDeviceMetricsOverride", map[string]any{
			"width":             1440,
			"height":            900,
			"deviceScaleFactor": 1,
			"mobile":            false,
		})
		cdp.navigate(t, ts.URL+"/")
		time.Sleep(150 * time.Millisecond)
		styleLinkHref := cdp.evalString(t, `(() => {
			const a = document.querySelector('.book-sidebar a[href="/STYLE_GUIDE"]');
			return a ? a.getAttribute('href') : 'missing';
		})()`)
		if styleLinkHref != "/STYLE_GUIDE" {
			t.Fatalf("E2E-CDP-01: expected sidebar link to /STYLE_GUIDE, got %q", styleLinkHref)
		}
		cdp.navigate(t, ts.URL+"/STYLE_GUIDE")
		time.Sleep(150 * time.Millisecond)
		stylePageCheck := cdp.evalString(t, `(() => {
			const gov = document.getElementById('styleGuideGovernanceBanner');
			const cfg = document.getElementById('activeBookConfigCard');
			const active = document.querySelector('.book-sidebar .toc-item.active');
			if (!gov || !cfg) return 'missing-governance-card';
			if (!active || active.getAttribute('href') !== '/STYLE_GUIDE') return 'bad-active-link';
			if (document.documentElement.scrollWidth > document.documentElement.clientWidth) {
				return 'style-guide-overflow:' + document.documentElement.scrollWidth + '>' + document.documentElement.clientWidth;
			}
			return 'ok';
		})()`)
		if stylePageCheck != "ok" {
			t.Fatalf("E2E-CDP-01 /STYLE_GUIDE check failed: %s", stylePageCheck)
		}
	})
}
