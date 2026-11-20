package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
)

func TestRun_FlagErrorsAndInvalidListenAddr(t *testing.T) {
	t.Setenv("RESEND_API_KEY", "")
	t.Setenv("GHOSTBOOK_SEND_INVITES", "false")
	ctx := context.Background()

	var buf bytes.Buffer
	if err := run(ctx, []string{"version"}, &buf); err != nil || !strings.Contains(buf.String(), "ghostbook ") {
		t.Fatalf("expected version subcommand output, got %q (err=%v)", buf.String(), err)
	}

	// --help must succeed cleanly with nil error
	var helpBuf bytes.Buffer
	if err := run(ctx, []string{"--help"}, &helpBuf); err != nil || !strings.Contains(helpBuf.String(), "Usage of ghostbook:") {
		t.Fatalf("expected --help to return nil and print usage, got err=%v out=%q", err, helpBuf.String())
	}

	if err := run(ctx, []string{"-unknown-flag"}, io.Discard); err == nil {
		t.Fatalf("expected error for unknown flag")
	}

	if err := run(ctx, []string{
		"-data-dir", "/dev/null/impossible-dir",
	}, io.Discard); err == nil {
		t.Fatalf("expected error for unwritable data-dir")
	}

	// Force store.Open failure by placing a directory at book_studio.db
	badStoreDir := t.TempDir()
	_ = os.MkdirAll(filepath.Join(badStoreDir, "book_studio.db"), 0o755)
	if err := run(ctx, []string{
		"-data-dir", badStoreDir,
	}, io.Discard); err == nil {
		t.Fatalf("expected error when book_studio.db is a directory")
	}

	// Force engine.NewBookEngineWithConfig failure by placing a regular file at repo
	badRepoDir := t.TempDir()
	_ = os.WriteFile(filepath.Join(badRepoDir, "repo"), []byte("not-a-dir"), 0o644)
	if err := run(ctx, []string{
		"-data-dir", badRepoDir,
	}, io.Discard); err == nil {
		t.Fatalf("expected error when repo is a regular file")
	}

	if err := run(ctx, []string{
		"-data-dir", t.TempDir(),
		"-addr", "127.0.0.1:999999",
		"-send-invites=true",
		"-mail-from=Ghostbook <ghostbook@example.com>",
		"-resend-api-key=re_invalid_key",
		"-remote-url=http://127.0.0.1:1/nonexistent.git",
	}, io.Discard); err == nil {
		t.Fatalf("expected error for invalid listen port")
	}

	// Non-loopback --pprof-addr MUST be rejected at startup
	for _, badPprof := range []string{":6060", "0.0.0.0:6060", "8.8.8.8:6060", "not-host-port"} {
		if err := run(ctx, []string{
			"-data-dir", t.TempDir(),
			"-pprof-addr", badPprof,
		}, io.Discard); err == nil {
			t.Fatalf("expected error for non-loopback or invalid -pprof-addr %q", badPprof)
		}
	}

	if got := redactRemoteURL("https://x-access-token:ghp_secret123@github.com/example/ghostbook-data.git"); got == "" || strings.Contains(got, "ghp_secret123") {
		t.Fatalf("expected redactRemoteURL to mask credentials, got %q", got)
	}
}

func TestRun_AuthorsSubcommands(t *testing.T) {
	ctx := context.Background()
	dataDir := filepath.Join(t.TempDir(), "nonexistent-sub")

	// authors list on nonexistent dataDir must NOT create the directory or DB
	var listEmpty bytes.Buffer
	if err := run(ctx, []string{"authors", "list", "-data-dir", dataDir}, &listEmpty); err != nil {
		t.Fatalf("authors list empty: %v", err)
	}
	if !strings.Contains(listEmpty.String(), "No co-authors configured.") {
		t.Fatalf("unexpected empty authors list output: %q", listEmpty.String())
	}
	if _, err := os.Stat(dataDir); !os.IsNotExist(err) {
		t.Fatalf("expected authors list not to create missing dataDir %s", dataDir)
	}

	var addOut bytes.Buffer
	if err := run(ctx, []string{
		"authors", "add",
		"-data-dir", dataDir,
		"-base-url", "https://docs.example.com",
		"-id", "alice",
		"-name", "Alice Chen",
		"-email", "alice@example.com",
		"-badge", "Lead Author",
		"-aliases", "alice,achen",
	}, &addOut); err != nil {
		t.Fatalf("authors add: %v", err)
	}
	if !strings.Contains(addOut.String(), "https://docs.example.com/u/alice@example.com:") || !strings.Contains(addOut.String(), "/mcp") {
		t.Fatalf("expected personalized guide and MCP URLs, got:\n%s", addOut.String())
	}

	// Re-running add must preserve existing secret key
	var addOut2 bytes.Buffer
	if err := run(ctx, []string{
		"authors", "add",
		"-data-dir", dataDir,
		"-base-url", "https://docs.example.com",
		"-id", "alice",
		"-name", "Alice Chen",
		"-email", "alice@example.com",
	}, &addOut2); err != nil {
		t.Fatalf("authors add idempotent: %v", err)
	}
	if addOut.String() != addOut2.String() {
		t.Fatalf("expected secret key to be preserved across authors add:\nfirst:\n%s\nsecond:\n%s", addOut.String(), addOut2.String())
	}

	var listOut bytes.Buffer
	if err := run(ctx, []string{"authors", "list", "-data-dir", dataDir, "-base-url", "https://docs.example.com"}, &listOut); err != nil {
		t.Fatalf("authors list: %v", err)
	}
	if !strings.Contains(listOut.String(), "Alice Chen") || !strings.Contains(listOut.String(), "alice@example.com") {
		t.Fatalf("expected alice in authors list output:\n%s", listOut.String())
	}

	// Revoke author and verify list reports none
	var revokeOut bytes.Buffer
	if err := run(ctx, []string{"authors", "revoke", "-data-dir", dataDir, "-id", "alice"}, &revokeOut); err != nil {
		t.Fatalf("authors revoke: %v", err)
	}
	var listAfterRevoke bytes.Buffer
	if err := run(ctx, []string{"authors", "list", "-data-dir", dataDir}, &listAfterRevoke); err != nil {
		t.Fatalf("authors list after revoke: %v", err)
	}
	if !strings.Contains(listAfterRevoke.String(), "No co-authors configured.") {
		t.Fatalf("expected revoked author hidden from list, got:\n%s", listAfterRevoke.String())
	}

	// Rotate key re-enables author with a fresh key
	var rotateOut bytes.Buffer
	if err := run(ctx, []string{"authors", "rotate-key", "-data-dir", dataDir, "-base-url", "https://docs.example.com", "-id", "alice"}, &rotateOut); err != nil {
		t.Fatalf("authors rotate-key: %v", err)
	}
	if !strings.Contains(rotateOut.String(), "Rotated secret key for alice") || rotateOut.String() == addOut.String() {
		t.Fatalf("unexpected rotate-key output:\n%s", rotateOut.String())
	}
}

func TestRun_StartServeRemoteSyncAndGracefulShutdown(t *testing.T) {
	t.Setenv("RESEND_API_KEY", "")
	t.Setenv("GHOSTBOOK_SEND_INVITES", "false")
	bareDir := filepath.Join(t.TempDir(), "remote.git")
	if _, err := git.PlainInit(bareDir, true); err != nil {
		t.Fatalf("init bare remote: %v", err)
	}

	authorsPath := filepath.Join(t.TempDir(), "authors.json")
	if err := os.WriteFile(authorsPath, []byte(`[
		{"id":"alice","display_name":"Alice Chen","email":"alice@example.com","role_badge":"Systems","aliases":["alice"]}
	]`), 0o600); err != nil {
		t.Fatalf("write authors.json: %v", err)
	}

	extraLLMsPath := filepath.Join(t.TempDir(), "extra-llms.md")
	if err := os.WriteFile(extraLLMsPath, []byte("## Companion Archive\n- [Handbook Archive](https://archive.example.com/)\n"), 0o600); err != nil {
		t.Fatalf("write extra-llms.md: %v", err)
	}

	resendMock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":"email_test_ok"}`))
	}))
	defer resendMock.Close()

	// Find available ephemeral ports for HTTP and loopback pprof
	probeLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen ephemeral: %v", err)
	}
	addr := probeLn.Addr().String()
	_ = probeLn.Close()

	pprofProbe, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen pprof ephemeral: %v", err)
	}
	pprofAddr := pprofProbe.Addr().String()
	_ = pprofProbe.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	errCh := make(chan error, 1)
	go func() {
		errCh <- run(ctx, []string{
			"-addr", addr,
			"-pprof-addr", pprofAddr,
			"-data-dir", t.TempDir(),
			"-base-url", fmt.Sprintf("http://%s", addr),
			"-book-title", "Acme Architecture Book",
			"-book-kicker", "Engineering SSoT",
			"-mcp-name", "acme-book",
			"-authors-file", authorsPath,
			"-extra-llms-file", extraLLMsPath,
			"-remote-url", bareDir,
			"-sync-interval", "5s",
			"-send-invites=true",
			"-mail-from", "Acme Book <book@example.com>",
			"-resend-api-key=re_test_valid",
			"-resend-endpoint", resendMock.URL,
		}, io.Discard)
	}()

	// Poll /healthz until server is listening
	healthURL := fmt.Sprintf("http://%s/healthz", addr)
	deadline := time.Now().Add(5 * time.Second)
	var ready bool
	for time.Now().Before(deadline) {
		resp, reqErr := http.Get(healthURL)
		if reqErr == nil {
			_, _ = io.Copy(io.Discard, resp.Body)
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				ready = true
				break
			}
		}
		time.Sleep(25 * time.Millisecond)
	}
	if !ready {
		t.Fatalf("server did not become healthy on %s within 5s", healthURL)
	}

	// Verify custom title & extra llms snippet are active
	llmsResp, err := http.Get(fmt.Sprintf("http://%s/llms.txt", addr))
	if err != nil {
		t.Fatalf("GET /llms.txt: %v", err)
	}
	llmsBody, _ := io.ReadAll(llmsResp.Body)
	_ = llmsResp.Body.Close()
	if !strings.Contains(string(llmsBody), "# Acme Architecture Book") || !strings.Contains(string(llmsBody), "https://archive.example.com/") {
		t.Fatalf("expected custom title and extra-llms-file in /llms.txt, got:\n%s", string(llmsBody))
	}

	// Public addr must return 404 on /debug/pprof/cmdline, while loopback pprofAddr returns 200
	pubPprofResp, err := http.Get(fmt.Sprintf("http://%s/debug/pprof/cmdline", addr))
	if err != nil {
		t.Fatalf("GET public pprof: %v", err)
	}
	_, _ = io.Copy(io.Discard, pubPprofResp.Body)
	_ = pubPprofResp.Body.Close()
	if pubPprofResp.StatusCode != http.StatusNotFound {
		t.Fatalf("expected 404 on public /debug/pprof/cmdline, got %d", pubPprofResp.StatusCode)
	}

	loopPprofResp, err := http.Get(fmt.Sprintf("http://%s/debug/pprof/cmdline", pprofAddr))
	if err != nil {
		t.Fatalf("GET loopback pprof: %v", err)
	}
	_, _ = io.Copy(io.Discard, loopPprofResp.Body)
	_ = loopPprofResp.Body.Close()
	if loopPprofResp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 on loopback /debug/pprof/cmdline, got %d", loopPprofResp.StatusCode)
	}

	// Wait for the initial remote push to reach the bare repo before shutting down
	bareRepo, err := git.PlainOpen(bareDir)
	if err != nil {
		t.Fatalf("open bare remote: %v", err)
	}
	syncDeadline := time.Now().Add(5 * time.Second)
	var synced bool
	for time.Now().Before(syncDeadline) {
		if branches, refErr := bareRepo.Branches(); refErr == nil {
			_ = branches.ForEach(func(_ *plumbing.Reference) error {
				synced = true
				return nil
			})
		}
		if synced {
			break
		}
		time.Sleep(25 * time.Millisecond)
	}
	if !synced {
		t.Fatalf("expected initial remote sync to push branch to %s", bareDir)
	}

	// Cancel context to trigger graceful shutdown
	cancel()

	select {
	case runErr := <-errCh:
		if runErr != nil {
			t.Fatalf("expected clean nil exit on graceful shutdown, got %v", runErr)
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("timed out waiting for graceful shutdown")
	}

	// Exercise main() with SIGTERM graceful shutdown after polling /healthz (never a blind sleep)
	mainProbe, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen main ephemeral: %v", err)
	}
	mainAddr := mainProbe.Addr().String()
	_ = mainProbe.Close()

	origArgs := os.Args
	defer func() { os.Args = origArgs }()
	os.Args = []string{"ghostbook", "-addr", mainAddr, "-data-dir", t.TempDir(), "-send-invites=false"}
	mainDone := make(chan struct{})
	go func() {
		main()
		close(mainDone)
	}()
	mainDeadline := time.Now().Add(5 * time.Second)
	var mainReady bool
	for time.Now().Before(mainDeadline) {
		resp, reqErr := http.Get(fmt.Sprintf("http://%s/healthz", mainAddr))
		if reqErr == nil {
			_, _ = io.Copy(io.Discard, resp.Body)
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				mainReady = true
				break
			}
		}
		time.Sleep(25 * time.Millisecond)
	}
	if !mainReady {
		t.Fatalf("main() server did not become healthy on %s", mainAddr)
	}
	_ = syscall.Kill(os.Getpid(), syscall.SIGTERM)
	select {
	case <-mainDone:
	case <-time.After(5 * time.Second):
		t.Fatalf("timed out waiting for main() SIGTERM shutdown")
	}
}

func TestRun_DemoSubcommand(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// --help on demo subcommand returns nil cleanly
	var helpBuf bytes.Buffer
	if err := run(ctx, []string{"demo", "--help"}, &helpBuf); err != nil || !strings.Contains(helpBuf.String(), "ghostbook demo") {
		t.Fatalf("expected demo --help output, got err=%v out=%q", err, helpBuf.String())
	}

	probeLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen ephemeral: %v", err)
	}
	demoAddr := probeLn.Addr().String()
	_ = probeLn.Close()

	var outBuf bytes.Buffer
	errCh := make(chan error, 1)
	go func() {
		errCh <- run(ctx, []string{"demo", "-addr", demoAddr}, &outBuf)
	}()

	deadline := time.Now().Add(8 * time.Second)
	var ready bool
	for time.Now().Before(deadline) {
		resp, reqErr := http.Get("http://" + demoAddr + "/healthz")
		if reqErr == nil {
			body, _ := io.ReadAll(resp.Body)
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK && strings.Contains(string(body), `"status":"ok"`) {
				ready = true
				break
			}
		}
		time.Sleep(30 * time.Millisecond)
	}
	if !ready {
		cancel()
		t.Fatalf("demo server did not become healthy on %s (err=%v out=%s)", demoAddr, <-errCh, outBuf.String())
	}

	// Verify key demo routes respond 200 OK
	for _, path := range []string{
		"/",
		"/chapters/01-system-architecture",
		"/chapters/01-system-architecture/history",
		"/chapters/03-editorial-lineage",
		"/chapters/draft-quorum-tuning",
		"/STYLE_GUIDE",
		"/book.json",
		"/changelog",
		"/activity",
		"/search?q=BM25",
		"/guide",
		"/print",
		"/api-docs",
		"/llms.txt",
	} {
		resp, reqErr := http.Get("http://" + demoAddr + path)
		if reqErr != nil {
			cancel()
			t.Fatalf("GET %s failed: %v", path, reqErr)
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			cancel()
			t.Fatalf("GET %s returned status %d, want 200", path, resp.StatusCode)
		}
	}

	cancel()
	select {
	case runErr := <-errCh:
		if runErr != nil {
			t.Fatalf("demo server shutdown error: %v", runErr)
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("timed out waiting for demo server shutdown")
	}

	banner := outBuf.String()
	if !strings.Contains(banner, "Ghostbook Interactive Demo") || !strings.Contains(banner, "/u/alice@example.com:") {
		t.Fatalf("expected demo startup banner with alice MCP URL, got:\n%s", banner)
	}
}
