package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/wjkoh/ghostbook/internal/engine"
	"github.com/wjkoh/ghostbook/internal/server"
	"github.com/wjkoh/ghostbook/internal/store"
)

// version can be overridden at build time via -ldflags "-X main.version=v0.1.0".
var version = "dev"

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := run(ctx, os.Args[1:], os.Stdout); err != nil {
		log.Fatalf("ghostbook: %v", err)
	}
}

func validateLoopbackListenAddr(rawAddr string) error {
	host, _, err := net.SplitHostPort(strings.TrimSpace(rawAddr))
	if err != nil {
		return fmt.Errorf("invalid pprof listen address %q: %w", rawAddr, err)
	}
	host = strings.TrimSpace(host)
	if host == "" {
		return fmt.Errorf("pprof-addr %q must bind an explicit loopback interface (e.g. 127.0.0.1:6060)", rawAddr)
	}
	if strings.EqualFold(host, "localhost") {
		return nil
	}
	ip := net.ParseIP(host)
	if ip == nil || !ip.IsLoopback() {
		return fmt.Errorf("pprof-addr %q must bind a loopback IP (127.0.0.1 or ::1), got host %q", rawAddr, host)
	}
	return nil
}

func redactRemoteURL(raw string) string {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return ""
	}
	if u, err := url.Parse(trimmed); err == nil && u.Scheme != "" {
		return u.Redacted()
	}
	return trimmed
}

func splitCSV(raw string) []string {
	var out []string
	for _, part := range strings.Split(raw, ",") {
		if trimmed := strings.TrimSpace(part); trimmed != "" {
			out = append(out, trimmed)
		}
	}
	return out
}

func run(ctx context.Context, args []string, out io.Writer) error {
	if out == nil {
		out = os.Stdout
	}
	if len(args) > 0 {
		switch args[0] {
		case "version", "-version", "--version":
			_, _ = fmt.Fprintf(out, "ghostbook %s\n", version)
			return nil
		case "authors":
			return runAuthorsCommand(ctx, args[1:], out)
		}
	}

	fs := flag.NewFlagSet("ghostbook", flag.ContinueOnError)
	var (
		addr             = fs.String("addr", firstNonEmpty(os.Getenv("GHOSTBOOK_ADDR"), ":8090"), "HTTP listen address")
		pprofAddr        = fs.String("pprof-addr", "", "Optional loopback-only pprof HTTP listen address (e.g. 127.0.0.1:6060)")
		dataDir          = fs.String("data-dir", firstNonEmpty(os.Getenv("GHOSTBOOK_DATA_DIR"), "./data"), "Directory for Git repo and SQLite WAL database")
		baseURL          = fs.String("base-url", firstNonEmpty(os.Getenv("GHOSTBOOK_BASE_URL"), "http://127.0.0.1:8090"), "Public base URL for the book, RSS feed, and OpenAPI spec")
		bookTitle        = fs.String("book-title", "", "Book display title (defaults to GHOSTBOOK_TITLE or \"Ghostbook\")")
		bookKicker       = fs.String("book-kicker", "", "Sidebar kicker subtitle (defaults to GHOSTBOOK_KICKER or \"Living Book\")")
		mcpName          = fs.String("mcp-name", "", "MCP server identifier (defaults to GHOSTBOOK_MCP_NAME or \"ghostbook\")")
		authorsFile      = fs.String("authors-file", "", "Optional path to JSON file defining co-authors (defaults to GHOSTBOOK_AUTHORS_FILE)")
		allowedHosts     = fs.String("allowed-hosts", "", "Comma-separated list of allowed HTTP Host headers (defaults to GHOSTBOOK_ALLOWED_HOSTS)")
		urlPrefixStrip   = fs.String("url-prefix-strip", "", "Optional reverse-proxy URL path prefix to strip before routing (defaults to GHOSTBOOK_URL_PREFIX_STRIP)")
		extraLLMsFile    = fs.String("extra-llms-file", "", "Optional Markdown file appended to /llms.txt (defaults to GHOSTBOOK_EXTRA_LLMS_FILE)")
		bannedWords      = fs.String("banned-words", "", "Optional comma-separated editorial lint banned words (defaults to GHOSTBOOK_BANNED_WORDS)")
		remoteURL        = fs.String("remote-url", "", "Optional Git remote URL for periodic go-git sync (defaults to GITHUB_REMOTE_URL)")
		syncInterval     = fs.Duration("sync-interval", parseEnvDuration("GHOSTBOOK_SYNC_INTERVAL", 60*time.Second), "Periodic Git remote sync interval")
		sendInvites      = fs.Bool("send-invites", parseEnvBool("GHOSTBOOK_SEND_INVITES"), "Send pending co-author secret-key invite emails via Resend on startup")
		resendAPIKey     = fs.String("resend-api-key", "", "Resend API key for co-author invites and operational alerts (defaults to RESEND_API_KEY)")
		resendEndpoint   = fs.String("resend-endpoint", "", "Optional Resend API endpoint override (defaults to RESEND_ENDPOINT)")
		mailFrom         = fs.String("mail-from", "", "From header for outgoing Resend emails (defaults to GHOSTBOOK_MAIL_FROM)")
		mailReplyTo      = fs.String("mail-reply-to", "", "Reply-To header for outgoing Resend emails (defaults to GHOSTBOOK_MAIL_REPLY_TO)")
		alertRecipients  = fs.String("alert-recipients", "", "Comma-separated recipient addresses for operational alert emails (defaults to GHOSTBOOK_ALERT_RECIPIENTS)")
		geminiAPIKey     = fs.String("gemini-api-key", "", "Gemini API key for front-page editorial timeline synthesis (defaults to GEMINI_API_KEY)")
		geminiModel      = fs.String("gemini-model", "", "Gemini model ID for editorial timeline synthesis (defaults to GEMINI_MODEL)")
		geminiEndpoint   = fs.String("gemini-endpoint", "", "Optional Gemini or Vertex AI API endpoint override (defaults to GEMINI_ENDPOINT)")
		vertexProject    = fs.String("vertex-project", "", "Optional GCP project ID for Vertex AI editorial timeline synthesis (defaults to VERTEX_PROJECT)")
		vertexLocation   = fs.String("vertex-location", "", "Optional GCP location for Vertex AI (defaults to VERTEX_LOCATION)")
		vertexCredsFile  = fs.String("vertex-credentials-file", "", "Path to GCP Service Account JSON key for Vertex AI")
		briefingDebounce = fs.Duration("briefing-debounce", parseEnvDuration("GHOSTBOOK_BRIEFING_DEBOUNCE", engine.DefaultBriefingDebounce), "Quiet-period debounce duration before synthesizing front-page editorial timeline")
	)
	if err := fs.Parse(args); err != nil {
		return fmt.Errorf("parse flags: %w", err)
	}

	effectivePprofAddr := firstNonEmpty(*pprofAddr, os.Getenv("GHOSTBOOK_PPROF_ADDR"))
	if effectivePprofAddr != "" {
		if err := validateLoopbackListenAddr(effectivePprofAddr); err != nil {
			return err
		}
	}
	effectiveTitle := firstNonEmpty(*bookTitle, os.Getenv("GHOSTBOOK_TITLE"), "Ghostbook")
	effectiveKicker := firstNonEmpty(*bookKicker, os.Getenv("GHOSTBOOK_KICKER"), "Living Book")
	effectiveMCPName := firstNonEmpty(*mcpName, os.Getenv("GHOSTBOOK_MCP_NAME"), "ghostbook")
	effectiveAuthorsFile := firstNonEmpty(*authorsFile, os.Getenv("GHOSTBOOK_AUTHORS_FILE"))
	effectiveAllowedHosts := splitCSV(firstNonEmpty(*allowedHosts, os.Getenv("GHOSTBOOK_ALLOWED_HOSTS")))
	effectivePrefixStrip := firstNonEmpty(*urlPrefixStrip, os.Getenv("GHOSTBOOK_URL_PREFIX_STRIP"))
	effectiveExtraLLMsFile := firstNonEmpty(*extraLLMsFile, os.Getenv("GHOSTBOOK_EXTRA_LLMS_FILE"))
	effectiveBannedWords := splitCSV(firstNonEmpty(*bannedWords, os.Getenv("GHOSTBOOK_BANNED_WORDS")))
	effectiveRemoteURL := firstNonEmpty(*remoteURL, os.Getenv("GITHUB_REMOTE_URL"))
	effectiveResendKey := firstNonEmpty(*resendAPIKey, os.Getenv("RESEND_API_KEY"))
	effectiveResendEndpoint := firstNonEmpty(*resendEndpoint, os.Getenv("RESEND_ENDPOINT"))
	effectiveMailFrom := firstNonEmpty(*mailFrom, os.Getenv("GHOSTBOOK_MAIL_FROM"))
	effectiveMailReplyTo := firstNonEmpty(*mailReplyTo, os.Getenv("GHOSTBOOK_MAIL_REPLY_TO"))
	effectiveAlertRecipients := splitCSV(firstNonEmpty(*alertRecipients, os.Getenv("GHOSTBOOK_ALERT_RECIPIENTS")))
	effectiveGeminiKey := firstNonEmpty(*geminiAPIKey, os.Getenv("GEMINI_API_KEY"))
	effectiveGeminiModel := firstNonEmpty(*geminiModel, os.Getenv("GEMINI_MODEL"))
	effectiveGeminiEndpoint := firstNonEmpty(*geminiEndpoint, os.Getenv("GEMINI_ENDPOINT"))
	effectiveVertexProject := firstNonEmpty(*vertexProject, os.Getenv("VERTEX_PROJECT"))
	effectiveVertexLocation := firstNonEmpty(*vertexLocation, os.Getenv("VERTEX_LOCATION"))
	effectiveVertexCreds := firstNonEmpty(*vertexCredsFile, firstNonEmptyEnv("VERTEX_CREDENTIALS_FILE", "GOOGLE_APPLICATION_CREDENTIALS"))

	var extraLLMsMD string
	if effectiveExtraLLMsFile != "" {
		raw, err := os.ReadFile(effectiveExtraLLMsFile)
		if err != nil {
			return fmt.Errorf("read extra-llms-file %s: %w", effectiveExtraLLMsFile, err)
		}
		extraLLMsMD = strings.TrimSpace(string(raw))
	}

	if err := os.MkdirAll(*dataDir, 0o700); err != nil {
		return fmt.Errorf("mkdir data-dir: %w", err)
	}

	mailCfg := store.MailConfig{
		BookTitle:  effectiveTitle,
		From:       effectiveMailFrom,
		ReplyTo:    effectiveMailReplyTo,
		Recipients: effectiveAlertRecipients,
	}

	dbPath := filepath.Join(*dataDir, "book_studio.db")
	repoDir := filepath.Join(*dataDir, "repo")
	alerter := store.NewAlertNotifier(effectiveResendKey, effectiveResendEndpoint, *baseURL, 0, mailCfg)
	defer alerter.Wait()

	st, err := store.Open(dbPath)
	if err != nil {
		alerter.NotifyError(ctx, "startup_sqlite_error", err.Error(), "Failed to open SQLite store at "+dbPath)
		return fmt.Errorf("open sqlite store: %w", err)
	}
	defer func() { _ = st.Close() }()

	if effectiveAuthorsFile != "" {
		authorsCfg, err := store.LoadAuthorsFile(effectiveAuthorsFile)
		if err != nil {
			return fmt.Errorf("load authors-file %s: %w", effectiveAuthorsFile, err)
		}
		if err := st.SyncConfiguredAuthors(ctx, authorsCfg); err != nil {
			return fmt.Errorf("sync authors-file %s: %w", effectiveAuthorsFile, err)
		}
		log.Printf("synced %d co-author(s) from %s", len(authorsCfg), effectiveAuthorsFile)
	} else {
		existingAuthors, err := st.ListAuthors(ctx)
		if err == nil && len(existingAuthors) == 0 {
			log.Printf("no co-authors configured yet; add one with: ghostbook authors add --data-dir %s --id alice --name \"Alice Chen\" --email alice@example.com", *dataDir)
		}
	}

	if *sendInvites && effectiveResendKey != "" && effectiveMailFrom != "" {
		sentCount, err := st.SendPendingAuthorInvites(ctx, *baseURL, effectiveResendKey, effectiveResendEndpoint, mailCfg)
		if err != nil {
			log.Printf("warning: send pending author invites failed (sent=%d): %v", sentCount, err)
			alerter.NotifyError(ctx, "startup_invite_error", err.Error(), "Failed to send pending author invites")
		} else if sentCount > 0 {
			log.Printf("dispatched %d co-author invite email(s) via Resend", sentCount)
		}
	}

	eng, err := engine.NewBookEngineWithConfig(repoDir, st, *baseURL, engine.EngineConfig{
		BookTitle:         effectiveTitle,
		BookKicker:        effectiveKicker,
		MCPName:           effectiveMCPName,
		ExtraLLMsMarkdown: extraLLMsMD,
		BannedWords:       effectiveBannedWords,
	}, effectiveRemoteURL)
	if err != nil {
		alerter.NotifyError(ctx, "startup_engine_error", err.Error(), "Failed to initialize BookEngine at "+repoDir)
		return fmt.Errorf("init book engine: %w", err)
	}
	eng.SetAlertNotifier(alerter)
	briefSynth := engine.NewBriefSynthesizerWithConfig(engine.BriefSynthesizerConfig{
		APIKey:                effectiveGeminiKey,
		Model:                 effectiveGeminiModel,
		Endpoint:              effectiveGeminiEndpoint,
		VertexProject:         effectiveVertexProject,
		VertexLocation:        effectiveVertexLocation,
		VertexCredentialsFile: effectiveVertexCreds,
	})
	eng.SetBriefSynthesizer(briefSynth)

	runCtx, cancelRun := context.WithCancel(ctx)
	var wg sync.WaitGroup
	defer func() {
		cancelRun()
		wg.Wait()
	}()

	if briefSynth.Enabled() {
		wg.Add(1)
		go func() {
			defer wg.Done()
			eng.StartBriefingSynthesizer(runCtx, *briefingDebounce)
		}()
	}

	if effectiveRemoteURL != "" {
		safeRemote := redactRemoteURL(effectiveRemoteURL)
		if err := eng.ConfigureRemote(effectiveRemoteURL); err != nil {
			log.Printf("warning: configure git remote (%s) failed: %v", safeRemote, err)
		} else {
			wg.Add(1)
			go func() {
				defer wg.Done()
				if err := eng.SyncWithRemote(runCtx); err != nil && runCtx.Err() == nil {
					log.Printf("warning: initial git remote sync (%s) failed: %v", safeRemote, err)
				} else if err == nil {
					log.Printf("synced initial book repository with remote %s", safeRemote)
				}
				eng.StartPeriodicGitHubSync(runCtx, *syncInterval)
			}()
		}
	}

	ln, err := net.Listen("tcp", *addr)
	if err != nil {
		return fmt.Errorf("listen %s: %w", *addr, err)
	}

	var pprofLn net.Listener
	var pprofSrv *http.Server
	if effectivePprofAddr != "" {
		pprofLn, err = net.Listen("tcp", effectivePprofAddr)
		if err != nil {
			_ = ln.Close()
			return fmt.Errorf("listen pprof %s: %w", effectivePprofAddr, err)
		}
		pprofSrv = &http.Server{
			Handler:           server.NewPprofHandler(),
			ReadHeaderTimeout: 10 * time.Second,
			IdleTimeout:       60 * time.Second,
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			log.Printf("Ghostbook loopback pprof listening on %s", pprofLn.Addr().String())
			if pErr := pprofSrv.Serve(pprofLn); pErr != nil && !errors.Is(pErr, http.ErrServerClosed) {
				log.Printf("warning: pprof server stopped with error: %v", pErr)
			}
		}()
	}

	handler := server.NewHTTPHandler(eng, server.HTTPHandlerOptions{
		AllowedHosts:   effectiveAllowedHosts,
		URLPrefixStrip: effectivePrefixStrip,
	})
	httpSrv := &http.Server{
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       120 * time.Second,
		MaxHeaderBytes:    1 << 20,
	}

	shutdownDone := make(chan struct{})
	go func() {
		defer close(shutdownDone)
		<-runCtx.Done()
		shutCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		var shutWG sync.WaitGroup
		if pprofSrv != nil {
			shutWG.Add(1)
			go func() {
				defer shutWG.Done()
				_ = pprofSrv.Shutdown(shutCtx)
			}()
		}
		shutWG.Add(1)
		go func() {
			defer shutWG.Done()
			_ = httpSrv.Shutdown(shutCtx)
		}()
		shutWG.Wait()
	}()

	log.Printf("%s (Ghostbook %s) listening on %s (base=%s, data=%s)", effectiveTitle, version, ln.Addr().String(), *baseURL, *dataDir)
	serveErr := httpSrv.Serve(ln)
	cancelRun()
	<-shutdownDone
	if serveErr != nil && !errors.Is(serveErr, http.ErrServerClosed) {
		return fmt.Errorf("http server error: %w", serveErr)
	}
	return nil
}

func runAuthorsCommand(ctx context.Context, args []string, out io.Writer) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: ghostbook authors <list|add> [flags]")
	}
	sub := args[0]
	switch sub {
	case "list":
		fs := flag.NewFlagSet("ghostbook authors list", flag.ContinueOnError)
		dataDir := fs.String("data-dir", firstNonEmpty(os.Getenv("GHOSTBOOK_DATA_DIR"), "./data"), "Directory containing book_studio.db")
		baseURL := fs.String("base-url", firstNonEmpty(os.Getenv("GHOSTBOOK_BASE_URL"), "http://127.0.0.1:8090"), "Public base URL for personalized URLs")
		if err := fs.Parse(args[1:]); err != nil {
			return fmt.Errorf("parse flags: %w", err)
		}
		if err := os.MkdirAll(*dataDir, 0o700); err != nil {
			return fmt.Errorf("mkdir data-dir: %w", err)
		}
		st, err := store.Open(filepath.Join(*dataDir, "book_studio.db"))
		if err != nil {
			return fmt.Errorf("open sqlite store: %w", err)
		}
		defer func() { _ = st.Close() }()

		authors, err := st.ListAuthors(ctx)
		if err != nil {
			return fmt.Errorf("list authors: %w", err)
		}
		if len(authors) == 0 {
			_, _ = fmt.Fprintln(out, "No co-authors configured.")
			return nil
		}
		base := strings.TrimRight(strings.TrimSpace(*baseURL), "/")
		for _, a := range authors {
			_, _ = fmt.Fprintf(out, "%s\t%s\t%s\n  Guide: %s/u/%s/guide\n  MCP:   %s/u/%s/mcp\n",
				a.ID, a.DisplayName, a.GitEmail, base, a.AuthIdentifier(), base, a.AuthIdentifier())
		}
		return nil

	case "add":
		fs := flag.NewFlagSet("ghostbook authors add", flag.ContinueOnError)
		dataDir := fs.String("data-dir", firstNonEmpty(os.Getenv("GHOSTBOOK_DATA_DIR"), "./data"), "Directory containing book_studio.db")
		baseURL := fs.String("base-url", firstNonEmpty(os.Getenv("GHOSTBOOK_BASE_URL"), "http://127.0.0.1:8090"), "Public base URL for personalized URLs")
		id := fs.String("id", "", "Short lowercase author ID (e.g. alice)")
		name := fs.String("name", "", "Author full display name (e.g. \"Alice Chen\")")
		email := fs.String("email", "", "Author Git commit email (e.g. alice@example.com)")
		badge := fs.String("badge", "Co-Author", "Optional role badge shown on setup guide")
		aliases := fs.String("aliases", "", "Optional comma-separated login aliases")
		if err := fs.Parse(args[1:]); err != nil {
			return fmt.Errorf("parse flags: %w", err)
		}
		if strings.TrimSpace(*id) == "" || strings.TrimSpace(*name) == "" || strings.TrimSpace(*email) == "" {
			return fmt.Errorf("--id, --name, and --email are required")
		}
		if err := os.MkdirAll(*dataDir, 0o700); err != nil {
			return fmt.Errorf("mkdir data-dir: %w", err)
		}
		st, err := store.Open(filepath.Join(*dataDir, "book_studio.db"))
		if err != nil {
			return fmt.Errorf("open sqlite store: %w", err)
		}
		defer func() { _ = st.Close() }()

		author, err := st.UpsertAuthor(ctx, store.AuthorConfig{
			ID:          *id,
			DisplayName: *name,
			Email:       *email,
			Badge:       *badge,
			Aliases:     splitCSV(*aliases),
		})
		if err != nil {
			return fmt.Errorf("upsert author: %w", err)
		}
		base := strings.TrimRight(strings.TrimSpace(*baseURL), "/")
		_, _ = fmt.Fprintf(out, "Saved author %s (%s <%s>)\nGuide: %s/u/%s/guide\nMCP:   %s/u/%s/mcp\n",
			author.ID, author.DisplayName, author.GitEmail, base, author.AuthIdentifier(), base, author.AuthIdentifier())
		return nil

	default:
		return fmt.Errorf("unknown authors subcommand %q (expected list or add)", sub)
	}
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if trimmed := strings.TrimSpace(v); trimmed != "" {
			return trimmed
		}
	}
	return ""
}

func firstNonEmptyEnv(keys ...string) string {
	for _, k := range keys {
		if v := strings.TrimSpace(os.Getenv(k)); v != "" {
			return v
		}
	}
	return ""
}

func parseEnvBool(envKey string) bool {
	v := strings.TrimSpace(strings.ToLower(os.Getenv(envKey)))
	return v == "1" || v == "true" || v == "yes"
}

func parseEnvDuration(envKey string, fallback time.Duration) time.Duration {
	raw := strings.TrimSpace(os.Getenv(envKey))
	if raw == "" {
		return fallback
	}
	if d, err := time.ParseDuration(raw); err == nil && d > 0 {
		return d
	}
	return fallback
}
