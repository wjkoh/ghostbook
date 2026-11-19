package server

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/pprof"
	"net/url"
	"os"
	"runtime/debug"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/wjkoh/ghostbook/gen/book/v1/bookv1connect"
	"github.com/wjkoh/ghostbook/internal/engine"
	"github.com/wjkoh/ghostbook/internal/renderer"
)

func withUserAgent(ctx context.Context, ua string) context.Context {
	return engine.WithUserAgent(ctx, ua)
}

func userAgentFromContext(ctx context.Context) string {
	return engine.UserAgentFromContext(ctx)
}

func extractTokenFromRequest(r *http.Request) string {
	authHeader := strings.TrimSpace(r.Header.Get("Authorization"))
	if strings.HasPrefix(strings.ToLower(authHeader), "bearer ") {
		return strings.TrimSpace(authHeader[7:])
	}
	return authHeader
}

func writeUnauthorized(w http.ResponseWriter, msg string) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(http.StatusUnauthorized)
	_ = json.NewEncoder(w).Encode(map[string]string{
		"code":    "unauthenticated",
		"message": msg,
	})
}

func writeJSON(w http.ResponseWriter, payload any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_ = json.NewEncoder(w).Encode(payload)
}

func computeStrongETag(body []byte) string {
	sum := sha256.Sum256(body)
	return `"sha256-` + hex.EncodeToString(sum[:]) + `"`
}

func etagMatches(ifNoneMatchHeader, etag string) bool {
	hdr := strings.TrimSpace(ifNoneMatchHeader)
	if hdr == "" || etag == "" {
		return false
	}
	if hdr == "*" {
		return true
	}
	target := strings.TrimPrefix(etag, "W/")
	for _, part := range strings.Split(hdr, ",") {
		candidate := strings.TrimPrefix(strings.TrimSpace(part), "W/")
		if candidate == target {
			return true
		}
	}
	return false
}

func buildDiscoveryLinkHeader(alternateHref, alternateType, canonicalHref, canonicalType string) string {
	var parts []string
	if strings.TrimSpace(alternateHref) != "" && strings.TrimSpace(alternateType) != "" {
		parts = append(parts, fmt.Sprintf("<%s>; rel=\"alternate\"; type=%q", alternateHref, alternateType))
	}
	if strings.TrimSpace(canonicalHref) != "" && strings.TrimSpace(canonicalType) != "" {
		parts = append(parts, fmt.Sprintf("<%s>; rel=\"canonical\"; type=%q", canonicalHref, canonicalType))
	}
	parts = append(parts,
		`</llms.txt>; rel="llms-txt"; type="text/markdown"`,
		`</llms-full.txt>; rel="llms-full-txt"; type="text/markdown"`,
		`</openapi.json>; rel="service-desc"; type="application/vnd.oai.openapi+json"`,
	)
	return strings.Join(parts, ", ")
}

func writeHTMLResponse(w http.ResponseWriter, r *http.Request, pageBytes []byte, err error, markdownAltPath string) {
	if err != nil {
		slog.Error("render_html_failed", "error", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	etag := computeStrongETag(pageBytes)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Vary", "Accept, User-Agent")
	if w.Header().Get("Cache-Control") == "" {
		w.Header().Set("Cache-Control", "no-cache")
	}
	w.Header().Set("ETag", etag)
	w.Header().Set("Link", buildDiscoveryLinkHeader(markdownAltPath, "text/markdown", "", ""))
	if r != nil && etagMatches(r.Header.Get("If-None-Match"), etag) {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	w.Header().Set("Content-Length", strconv.Itoa(len(pageBytes)))
	if r != nil && r.Method == http.MethodHead {
		w.WriteHeader(http.StatusOK)
		return
	}
	_, _ = w.Write(pageBytes)
}

func writeMarkdownPayload(w http.ResponseWriter, r *http.Request, body string, immutable bool, alternateHTMLPath, canonicalMDPath string) {
	rawBytes := []byte(body)
	etag := computeStrongETag(rawBytes)
	w.Header().Set("Content-Type", "text/markdown; charset=utf-8")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Vary", "Accept, User-Agent")
	if immutable {
		w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	} else {
		w.Header().Set("Cache-Control", "no-cache")
	}
	w.Header().Set("ETag", etag)
	w.Header().Set("Link", buildDiscoveryLinkHeader(alternateHTMLPath, "text/html", canonicalMDPath, "text/markdown"))
	if r != nil && etagMatches(r.Header.Get("If-None-Match"), etag) {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	w.Header().Set("Content-Length", strconv.Itoa(len(rawBytes)))
	if r != nil && r.Method == http.MethodHead {
		w.WriteHeader(http.StatusOK)
		return
	}
	_, _ = w.Write(rawBytes)
}

func writeMarkdownExportResult(w http.ResponseWriter, r *http.Request, res *engine.MarkdownExportResult, immutable bool, alternateHTMLPath, canonicalMDPath string) {
	if res.HeadSHA != "" {
		w.Header().Set("X-Book-Head-Revision", res.HeadSHA)
	}
	if res.HeadRevisionNumber > 0 {
		w.Header().Set("X-Book-Head-Revision-Number", fmt.Sprintf("r%d", res.HeadRevisionNumber))
	}
	if res.RevisionSHA != "" {
		w.Header().Set("X-Book-Revision", res.RevisionSHA)
	}
	if res.RevisionNumber > 0 {
		w.Header().Set("X-Book-Revision-Number", fmt.Sprintf("r%d", res.RevisionNumber))
	}
	w.Header().Set("X-Chapter-Is-Latest", strconv.FormatBool(res.IsLatest))
	if res.WordCount >= 0 {
		w.Header().Set("X-Chapter-Word-Count", strconv.Itoa(int(res.WordCount)))
	}
	writeMarkdownPayload(w, r, res.Markdown, immutable, alternateHTMLPath, canonicalMDPath)
}

func AuthMiddleware(eng *engine.BookEngine, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx := withUserAgent(r.Context(), r.Header.Get("User-Agent"))
		if existing := engine.AuthorFromContext(ctx); existing != nil {
			next.ServeHTTP(w, r.WithContext(ctx))
			return
		}
		rawToken := extractTokenFromRequest(r)
		if rawToken == "" {
			slog.Warn("auth_rejected", "path", redactSecretInPath(r.URL.Path), "reason", "missing_token", "remote_addr", r.RemoteAddr)
			writeUnauthorized(w, "missing co-author email or bearer token")
			return
		}
		author, err := eng.Store().Authenticate(ctx, rawToken)
		if err != nil {
			slog.Warn("auth_rejected", "path", redactSecretInPath(r.URL.Path), "reason", "invalid_token", "remote_addr", r.RemoteAddr)
			writeUnauthorized(w, "invalid or revoked co-author email/token")
			return
		}
		ctx = engine.WithAuthor(ctx, author)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

func splitEmailRoutePath(p string) (rawEmailPart, suffix string, ok bool) {
	rest := strings.TrimPrefix(p, "/u/")
	if rest == "" {
		return "", "", false
	}
	slashIdx := strings.IndexByte(rest, '/')
	if slashIdx < 0 {
		return rest, "/", true
	}
	if slashIdx == 0 {
		return "", "", false
	}
	return rest[:slashIdx], rest[slashIdx:], true
}

func EmailURLRouterMiddleware(eng *engine.BookEngine, mcpHandler http.Handler, connectHandler http.Handler, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, "/u/") {
			next.ServeHTTP(w, r)
			return
		}
		rawEmailPart, suffix, ok := splitEmailRoutePath(r.URL.Path)
		if !ok {
			slog.Warn("email_route_rejected", "path", redactSecretInPath(r.URL.Path), "reason", "malformed_path", "remote_addr", r.RemoteAddr)
			writeUnauthorized(w, "malformed /u/{email}/... URL path")
			return
		}

		decodedEmail, err := url.PathUnescape(rawEmailPart)
		if err != nil {
			decodedEmail = rawEmailPart
		}
		ctx := withUserAgent(r.Context(), r.Header.Get("User-Agent"))
		author, err := eng.Store().Authenticate(ctx, decodedEmail)
		if err != nil {
			slog.Warn("email_route_rejected", "path", redactSecretInPath(r.URL.Path), "reason", "unrecognized_author", "remote_addr", r.RemoteAddr)
			writeUnauthorized(w, "unrecognized or revoked co-author email in URL path")
			return
		}
		ctx = engine.WithAuthor(ctx, author)
		r2 := r.Clone(ctx)
		r2.URL.Path = suffix
		r2.URL.RawPath = ""

		switch {
		case suffix == "/mcp" || strings.HasPrefix(suffix, "/mcp/"):
			mcpHandler.ServeHTTP(w, r2)
		case strings.HasPrefix(suffix, "/book.v1.BookService/"):
			connectHandler.ServeHTTP(w, r2)
		default:
			next.ServeHTTP(w, r2)
		}
	})
}

func handleHealthz(eng *engine.BookEngine) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		headSHA := ""
		headRev := ""
		chaptersCount := 0
		var totalWords int32
		var openComments int32
		recentActivityCount := 0
		if snap := eng.CurrentSnapshot(); snap != nil {
			headSHA = snap.HeadCommitSHA
			if snap.HeadRevisionNumber > 0 {
				headRev = fmt.Sprintf("r%d", snap.HeadRevisionNumber)
			}
			chaptersCount = len(snap.Chapters)
			for _, ch := range snap.Chapters {
				totalWords += ch.WordCount
				openComments += ch.OpenCommentCount
			}
			recentActivityCount = len(snap.RecentActivity)
		}
		writeJSON(w, map[string]any{
			"status":                "ok",
			"head_sha":              headSHA,
			"head_revision":         headRev,
			"chapters_count":        chaptersCount,
			"total_words":           totalWords,
			"open_comments":         openComments,
			"recent_activity_count": recentActivityCount,
		})
	}
}

func verifyWebhookSignature(secret, sigHeader string, body []byte) bool {
	if secret == "" || sigHeader == "" {
		return false
	}
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	expectedSig := "sha256=" + hex.EncodeToString(mac.Sum(nil))
	return hmac.Equal([]byte(sigHeader), []byte(expectedSig))
}

func handleGitHubWebhook(eng *engine.BookEngine) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.Header().Set("Allow", "POST")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, 4<<20)
		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, "read body failed", http.StatusBadRequest)
			return
		}
		secret := strings.TrimSpace(os.Getenv("GITHUB_WEBHOOK_SECRET"))
		sigHeader := strings.TrimSpace(r.Header.Get("X-Hub-Signature-256"))
		if !verifyWebhookSignature(secret, sigHeader, body) {
			http.Error(w, "invalid webhook signature", http.StatusUnauthorized)
			return
		}
		if err := eng.SyncWithRemote(r.Context()); err != nil {
			slog.Error("github_webhook_sync_failed", "error", err)
			http.Error(w, "webhook sync failed", http.StatusInternalServerError)
			return
		}
		writeJSON(w, map[string]string{"status": "synced"})
	}
}

func maxBodyBytesMiddleware(maxBytes int64, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Body != nil {
			r.Body = http.MaxBytesReader(w, r.Body, maxBytes)
		}
		next.ServeHTTP(w, r)
	})
}

// NewPprofHandler returns an HTTP handler serving Go runtime profiling routes
// strictly for mounting on a dedicated 127.0.0.1 loopback debug listener.
func NewPprofHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/debug/pprof/", pprof.Index)
	mux.HandleFunc("/debug/pprof/cmdline", pprof.Cmdline)
	mux.HandleFunc("/debug/pprof/profile", pprof.Profile)
	mux.HandleFunc("/debug/pprof/symbol", pprof.Symbol)
	mux.HandleFunc("/debug/pprof/trace", pprof.Trace)
	return mux
}

func newDynamicMCPProvider(eng *engine.BookEngine) func(*http.Request) *mcp.Server {
	var (
		mu     sync.RWMutex
		lastSG = eng.StyleGuide()
		cached = NewMCPServer(eng)
	)
	return func(_ *http.Request) *mcp.Server {
		currentSG := eng.StyleGuide()
		mu.RLock()
		if currentSG == lastSG {
			srv := cached
			mu.RUnlock()
			return srv
		}
		mu.RUnlock()

		mu.Lock()
		defer mu.Unlock()
		currentSG = eng.StyleGuide()
		if currentSG != lastSG {
			cached = NewMCPServer(eng)
			lastSG = currentSG
		}
		return cached
	}
}

// HTTPHandlerOptions customizes optional host reflection and path-prefix stripping on NewHTTPHandler.
type HTTPHandlerOptions struct {
	AllowedHosts   []string
	URLPrefixStrip string
}

const maxRPCBodyBytes = 16 << 20

func NewHTTPHandler(eng *engine.BookEngine, opts ...HTTPHandlerOptions) http.Handler {
	var cfg HTTPHandlerOptions
	if len(opts) > 0 {
		cfg = opts[0]
	}
	rawMCPHandler := mcp.NewStreamableHTTPHandler(newDynamicMCPProvider(eng), &mcp.StreamableHTTPOptions{
		DisableLocalhostProtection: true,
		Stateless:                  true,
	})
	mcpHTTPHandler := maxBodyBytesMiddleware(maxRPCBodyBytes, rawMCPHandler)

	connectAdapter := NewConnectAdapter(eng)
	connectPath, rawConnectHandler := bookv1connect.NewBookServiceHandler(connectAdapter)
	connectHandler := maxBodyBytesMiddleware(maxRPCBodyBytes, rawConnectHandler)

	mux := http.NewServeMux()
	mux.Handle("/mcp", AuthMiddleware(eng, mcpHTTPHandler))
	mux.Handle("/mcp/", AuthMiddleware(eng, mcpHTTPHandler))
	mux.Handle(connectPath, AuthMiddleware(eng, connectHandler))
	mux.HandleFunc("/openapi.json", ServeOpenAPISpec(eng.BaseURL(), eng.BookTitle(), cfg.AllowedHosts...))
	mux.HandleFunc("/api-docs", ServeOpenAPIDocs(eng))
	mux.HandleFunc("/docs", ServeOpenAPIDocs(eng))
	mux.Handle("/static/", http.StripPrefix("/static/", renderer.StaticAssetsHandler()))
	mux.HandleFunc("/robots.txt", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.Header().Set("Cache-Control", "public, max-age=300")
		_, _ = io.WriteString(w, RobotsTxtBody)
	})
	mux.HandleFunc("/healthz", handleHealthz(eng))
	mux.HandleFunc("/api/github-webhook", handleGitHubWebhook(eng))
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		serveWebStudio(eng, cfg.URLPrefixStrip, w, r)
	})

	return requestLoggerMiddleware(eng, EmailURLRouterMiddleware(eng, mcpHTTPHandler, connectHandler, mux))
}

type statusRecorder struct {
	http.ResponseWriter
	status int
	bytes  int
}

func (r *statusRecorder) WriteHeader(code int) {
	r.status = code
	r.ResponseWriter.WriteHeader(code)
}

func (r *statusRecorder) Write(b []byte) (int, error) {
	if r.status == 0 {
		r.status = http.StatusOK
	}
	n, err := r.ResponseWriter.Write(b)
	r.bytes += n
	return n, err
}

func (r *statusRecorder) Flush() {
	if f, ok := r.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (r *statusRecorder) Unwrap() http.ResponseWriter {
	return r.ResponseWriter
}

func redactSecretInPath(p string) string {
	if !strings.HasPrefix(p, "/u/") {
		return p
	}
	rawEmailPart, suffix, ok := splitEmailRoutePath(p)
	if !ok {
		return p
	}
	if colonIdx := strings.IndexByte(rawEmailPart, ':'); colonIdx > 0 {
		return "/u/" + rawEmailPart[:colonIdx] + ":***" + suffix
	}
	if pctIdx := strings.Index(strings.ToLower(rawEmailPart), "%3a"); pctIdx > 0 {
		return "/u/" + rawEmailPart[:pctIdx] + ":***" + suffix
	}
	return p
}

// RobotsTagHeaderValue instructs all search engines and web archivers never to index, follow, cache, or snippet any response.
const RobotsTagHeaderValue = "noindex, nofollow, noarchive, nosnippet, noimageindex, notranslate, max-snippet:0"

// InteractiveFetcherRobotsTagHeaderValue preserves strict noindex/nofollow/noarchive protection for user-initiated AI link readers
// without setting nosnippet/max-snippet:0 (which can cause strict web-fetch tools to suppress quoting user-requested pages).
const InteractiveFetcherRobotsTagHeaderValue = "noindex, nofollow, noarchive, noimageindex, notranslate"

// DefaultContentSecurityPolicy enforces strict self-hosted origin boundaries and blocks all browser XHR/Fetch/WebSocket calls (connect-src 'none') on book reader pages.
const DefaultContentSecurityPolicy = "default-src 'self'; script-src 'self' 'unsafe-inline'; style-src 'self' 'unsafe-inline'; font-src 'self' data:; img-src 'self' data: blob:; connect-src 'none'; object-src 'none'; base-uri 'self'; frame-ancestors 'none'"

// APIDocsContentSecurityPolicy scopes standalone /api-docs (self-hosted Scalar OpenAPI viewer) strictly to same-origin scripts, styles, and /openapi.json + ConnectRPC test requests, blocking all external CDNs, fonts, and proxies.
const APIDocsContentSecurityPolicy = "default-src 'self'; script-src 'self' 'unsafe-inline'; style-src 'self' 'unsafe-inline'; font-src 'self' data:; img-src 'self' data: blob:; connect-src 'self'; worker-src 'self' blob:; object-src 'none'; base-uri 'self'; frame-ancestors 'none'"

// RobotsTxtBody explicitly allows user-initiated interactive AI link fetchers (RFC 9309 specific-group precedence)
// while disallowing all search engine spiders, SEO crawlers, and bulk AI training scrapers across the entire site.
const RobotsTxtBody = `# User-initiated interactive AI link readers (explicit on-demand URL fetches only)
User-agent: Claude-User
Allow: /

User-agent: Claude-Web
Allow: /

User-agent: ChatGPT-User
Allow: /

User-agent: OAI-Operator
Allow: /

User-agent: Perplexity-User
Allow: /

User-agent: Google-NotebookLM
Allow: /

User-agent: Google-CloudVertexBot
Allow: /

User-agent: MistralAI-User
Allow: /

User-agent: DuckAssistBot
Allow: /

User-agent: Meta-ExternalFetcher
Allow: /

# Search engine indexers, archivers, and bulk AI training crawlers (disallowed)
User-agent: Googlebot
Disallow: /

User-agent: Googlebot-Image
Disallow: /

User-agent: Googlebot-News
Disallow: /

User-agent: Storebot-Google
Disallow: /

User-agent: Google-InspectionTool
Disallow: /

User-agent: GoogleOther
Disallow: /

User-agent: Google-Extended
Disallow: /

User-agent: Bingbot
Disallow: /

User-agent: Slurp
Disallow: /

User-agent: DuckDuckBot
Disallow: /

User-agent: Baiduspider
Disallow: /

User-agent: YandexBot
Disallow: /

User-agent: Applebot
Disallow: /

User-agent: Applebot-Extended
Disallow: /

User-agent: GPTBot
Disallow: /

User-agent: OAI-SearchBot
Disallow: /

User-agent: ClaudeBot
Disallow: /

User-agent: Claude-SearchBot
Disallow: /

User-agent: anthropic-ai
Disallow: /

User-agent: CCBot
Disallow: /

User-agent: Bytespider
Disallow: /

User-agent: Amazonbot
Disallow: /

User-agent: Meta-ExternalAgent
Disallow: /

User-agent: PerplexityBot
Disallow: /

User-agent: AhrefsBot
Disallow: /

User-agent: SemrushBot
Disallow: /

User-agent: MJ12bot
Disallow: /

User-agent: DotBot
Disallow: /

User-agent: PetalBot
Disallow: /

User-agent: ia_archiver
Disallow: /

User-agent: archive.org_bot
Disallow: /

User-agent: *
Disallow: /
`

var blockedSearchCrawlerTokens = []string{
	"googlebot",
	"adsbot-google",
	"mediapartners-google",
	"apis-google",
	"googleother",
	"google-extended",
	"google-inspectiontool",
	"storebot-google",
	"bingbot",
	"adidxbot",
	"bingpreview",
	"msnbot",
	"slurp",
	"duckduckbot",
	"baiduspider",
	"yandexbot",
	"yandeximages",
	"sogou",
	"exabot",
	"ia_archiver",
	"archive.org_bot",
	"wayback",
	"ahrefsbot",
	"semrushbot",
	"mj12bot",
	"dotbot",
	"petalbot",
	"bytespider",
	"ccbot",
	"gptbot",
	"oai-searchbot",
	"claudebot",
	"claude-searchbot",
	"anthropic-ai",
	"perplexitybot",
	"amazonbot",
	"applebot",
	"applebot-extended",
	"meta-externalagent",
	"dataforseobot",
	"rogerbot",
	"blexbot",
	"seznambot",
	"seekportbot",
	"zoominfobot",
	"censysinspect",
	"shodan",
}

var externalInteractiveFetcherTokens = []string{
	"chatgpt-user",
	"claude-user",
	"claude-web",
	"perplexity-user",
	"oai-operator",
	"google-notebooklm",
	"google-cloudvertexbot",
	"mistralai-user",
	"duckassistbot",
	"meta-externalfetcher",
}

var allowedInteractiveAgentTokens = []string{
	"chatgpt-user",
	"claude-user",
	"claude-web",
	"perplexity-user",
	"oai-operator",
	"google-notebooklm",
	"google-cloudvertexbot",
	"mistralai-user",
	"duckassistbot",
	"meta-externalfetcher",
	"antigravity",
	"agy-cli",
	"agy/",
}

// IsExternalInteractiveFetcherUserAgent returns true if the User-Agent identifies an external user-initiated AI link reader
// (such as Claude-User, ChatGPT-User, Perplexity-User, or Google-NotebookLM).
func IsExternalInteractiveFetcherUserAgent(ua string) bool {
	lower := strings.ToLower(strings.TrimSpace(ua))
	if lower == "" {
		return false
	}
	for _, tok := range externalInteractiveFetcherTokens {
		if strings.Contains(lower, tok) {
			return true
		}
	}
	return false
}

// IsBlockedSearchCrawlerUserAgent returns true if the User-Agent string belongs to a known search engine indexing spider,
// SEO crawler, or bulk web archiver, while allowing direct AI agent tools (agy, curl, Go-http-client, ChatGPT-User, Claude-User, MCP clients) and browsers.
func IsBlockedSearchCrawlerUserAgent(ua string) bool {
	lower := strings.ToLower(strings.TrimSpace(ua))
	if lower == "" {
		return false
	}
	for _, tok := range blockedSearchCrawlerTokens {
		if strings.Contains(lower, tok) {
			return true
		}
	}
	for _, allow := range allowedInteractiveAgentTokens {
		if strings.Contains(lower, allow) {
			return false
		}
	}
	return false
}

func isAuthenticatedAgentRoute(r *http.Request) bool {
	p := r.URL.Path
	return strings.HasPrefix(p, "/u/") ||
		p == "/mcp" ||
		strings.HasPrefix(p, "/mcp/") ||
		strings.HasPrefix(p, "/book.v1.BookService/") ||
		strings.TrimSpace(r.Header.Get("Authorization")) != ""
}

func requestLoggerMiddleware(eng *engine.BookEngine, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		safePath := redactSecretInPath(r.URL.Path)
		ua := r.Header.Get("User-Agent")
		if IsExternalInteractiveFetcherUserAgent(ua) {
			w.Header().Set("X-Robots-Tag", InteractiveFetcherRobotsTagHeaderValue)
		} else {
			w.Header().Set("X-Robots-Tag", RobotsTagHeaderValue)
		}
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("Content-Security-Policy", DefaultContentSecurityPolicy)
		if r.URL.Path != "/robots.txt" && r.URL.Path != "/healthz" && !isAuthenticatedAgentRoute(r) && IsBlockedSearchCrawlerUserAgent(ua) {
			slog.Warn("http_crawler_blocked", "method", r.Method, "path", safePath, "ua", ua)
			http.Error(w, "Forbidden: search engine indexing and bulk scraping are disallowed", http.StatusForbidden)
			return
		}
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}

		defer func() {
			if recVal := recover(); recVal != nil {
				stack := string(debug.Stack())
				slog.Error("http_panic_recovered", "method", r.Method, "path", safePath, "panic", fmt.Sprintf("%v", recVal))
				if eng != nil {
					eng.NotifyAlert(r.Context(), "http_panic", fmt.Sprintf("Panic on %s %s: %v", r.Method, safePath, recVal), stack)
				}
				if rec.bytes == 0 {
					http.Error(w, "internal server error", http.StatusInternalServerError)
				}
			}
		}()

		next.ServeHTTP(rec, r)

		// Skip noisy static asset and health-probe logs when status is 2xx/3xx
		if rec.status < 400 && (r.URL.Path == "/healthz" || strings.HasPrefix(r.URL.Path, "/static/")) {
			return
		}
		durationMs := time.Since(start).Milliseconds()
		if rec.status >= 500 && eng != nil {
			eng.NotifyAlert(r.Context(), "http_5xx_"+safePath, fmt.Sprintf("HTTP %d on %s %s", rec.status, r.Method, safePath), fmt.Sprintf("RemoteAddr: %s\nStatus: %d\nDurationMs: %d", r.RemoteAddr, rec.status, durationMs))
		}
		if rec.status >= 400 {
			slog.Warn("http_request",
				"method", r.Method,
				"path", safePath,
				"status", rec.status,
				"bytes", rec.bytes,
				"duration_ms", durationMs,
				"remote_addr", r.RemoteAddr,
				"ua", ua,
			)
		} else if r.URL.Path == "/robots.txt" {
			slog.Info("http_request",
				"method", r.Method,
				"path", safePath,
				"status", rec.status,
				"bytes", rec.bytes,
				"duration_ms", durationMs,
				"ua", ua,
			)
		} else {
			slog.Info("http_request",
				"method", r.Method,
				"path", safePath,
				"status", rec.status,
				"bytes", rec.bytes,
				"duration_ms", durationMs,
			)
		}
	})
}

func normalizeStudioPath(rawPath, stripPrefix string) string {
	cleanPath := strings.TrimSuffix(rawPath, "/")
	if p := strings.TrimSuffix(strings.TrimSpace(stripPrefix), "/"); p != "" {
		cleanPath = strings.TrimPrefix(cleanPath, p)
	}
	if cleanPath == "" {
		return "/"
	}
	return cleanPath
}

func trimChapterSlugSuffix(raw string) string {
	s := strings.TrimSuffix(raw, ".html")
	return strings.TrimSuffix(s, ".md")
}

// splitChapterAndRevision parses chapter permalink paths of the form:
// - "{slug}/rev/{rev}"
// - "{slug}/commits/{rev}"
// - "{slug}@{rev}"
// - "{slug}" with query "?rev={rev}"
func splitChapterAndRevision(rawSegment, queryRev string) (slug, rev string, isPermalink bool) {
	cleaned := strings.Trim(rawSegment, "/")
	for _, sep := range []string{"/rev/", "/commits/", "/revision/"} {
		if idx := strings.Index(cleaned, sep); idx > 0 {
			s := trimChapterSlugSuffix(cleaned[:idx])
			r := strings.TrimSpace(cleaned[idx+len(sep):])
			return s, r, r != ""
		}
	}
	if atIdx := strings.LastIndexByte(cleaned, '@'); atIdx > 0 {
		s := trimChapterSlugSuffix(cleaned[:atIdx])
		r := strings.TrimSpace(cleaned[atIdx+1:])
		return s, r, r != ""
	}
	s := trimChapterSlugSuffix(cleaned)
	q := strings.TrimSpace(queryRev)
	return s, q, q != ""
}

var (
	chapterHistoryCacheMu sync.RWMutex
	chapterHistoryCache   = make(map[string][]byte, 32)
)

func getCachedChapterHistoryHTML(key string) ([]byte, bool) {
	chapterHistoryCacheMu.RLock()
	b, ok := chapterHistoryCache[key]
	chapterHistoryCacheMu.RUnlock()
	return b, ok
}

func putCachedChapterHistoryHTML(key string, pageBytes []byte) {
	chapterHistoryCacheMu.Lock()
	if len(chapterHistoryCache) >= 128 {
		clear(chapterHistoryCache)
	}
	chapterHistoryCache[key] = pageBytes
	chapterHistoryCacheMu.Unlock()
}

func splitHistorySuffix(rawSegment string) (chapterPart string, isHistory bool) {
	cleaned := strings.Trim(rawSegment, "/")
	for _, suffix := range []string{"/history.md", "/history.html", "/history"} {
		if strings.HasSuffix(cleaned, suffix) {
			part := strings.TrimSuffix(cleaned, suffix)
			if part != "" {
				return part, true
			}
		}
	}
	return rawSegment, false
}

func serveChapterHistory(eng *engine.BookEngine, snap *renderer.RenderedBook, rawSlug string, wantsMarkdown bool, w http.ResponseWriter, r *http.Request) {
	normSlug, _, slugErr := engine.NormalizeChapterOrMetaSlug(rawSlug)
	if slugErr != nil {
		writeEngineHTTPError(w, r, slugErr)
		return
	}
	if wantsMarkdown {
		res, err := eng.AssembleChapterHistoryMarkdown(r.Context(), normSlug)
		if err != nil {
			writeEngineHTTPError(w, r, err)
			return
		}
		writeMarkdownExportResult(w, r, res, false, "/chapters/"+normSlug+"/history", "/chapters/"+normSlug+"/history.md")
		return
	}
	ch, ok := snap.BySlug[normSlug]
	if !ok || ch == nil {
		http.NotFound(w, r)
		return
	}
	cacheKey := eng.BaseURL() + "|" + snap.HeadCommitSHA + "|" + normSlug
	mdPath := "/chapters/" + normSlug + "/history.md"
	if cachedBytes, hit := getCachedChapterHistoryHTML(cacheKey); hit {
		writeHTMLResponse(w, r, cachedBytes, nil, mdPath)
		return
	}
	blameOut, err := eng.GetChapterBlame(r.Context(), &engine.GetChapterBlameInput{Slug: normSlug})
	if err != nil {
		slog.Error("chapter_history_blame_failed", "slug", normSlug, "error", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	commitsOut, err := eng.ListRecentChanges(r.Context(), &engine.ListRecentChangesInput{Slug: normSlug, Limit: 15})
	if err != nil {
		slog.Error("chapter_history_commits_failed", "slug", normSlug, "error", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	var blameViews []renderer.HistoryBlameSectionView
	for _, s := range blameOut.Sections {
		blameViews = append(blameViews, renderer.HistoryBlameSectionView{
			Heading:        s.Heading,
			LastCommitSHA:  s.LastCommitSHA,
			LastAuthor:     s.LastAuthor,
			LastUpdatedISO: s.LastUpdatedISO,
			StartLine:      s.StartLine,
			EndLine:        s.EndLine,
		})
	}
	var commitViews []renderer.HistoryCommitDiffView
	for _, c := range commitsOut.Commits {
		var diffText string
		if dOut, dErr := eng.GetCommitDiff(r.Context(), &engine.GetCommitDiffInput{CommitSHA: c.CommitSHA, Slug: normSlug}); dErr == nil {
			diffText = dOut.UnifiedDiff
		}
		commitViews = append(commitViews, renderer.HistoryCommitDiffView{
			CommitSHA:      c.CommitSHA,
			RevisionNumber: c.RevisionNumber,
			AuthorName:     c.AuthorName,
			AuthorEmail:    c.AuthorEmail,
			TimestampISO:   c.TimestampISO,
			CommitMessage:  c.CommitMessage,
			UnifiedDiff:    diffText,
		})
	}
	histData := renderer.ChapterHistoryPageData{
		Book:          snap,
		ActiveChapter: ch,
		BaseURL:       eng.BaseURL(),
		BlameSections: blameViews,
		Commits:       commitViews,
	}
	pageBytes, err := renderer.RenderChapterHistoryPage(histData)
	if err == nil && snap.HeadCommitSHA != "" {
		putCachedChapterHistoryHTML(cacheKey, pageBytes)
	}
	writeHTMLResponse(w, r, pageBytes, err, histData.MarkdownPath())
}

func detectPathFormatExtension(cleanPath string) (basePath, explicitFormat string) {
	switch {
	case cleanPath == "/llms.txt" || strings.HasSuffix(cleanPath, "/llms.txt"):
		return cleanPath, "md"
	case cleanPath == "/llms-full.txt" || strings.HasSuffix(cleanPath, "/llms-full.txt"):
		return cleanPath, "md"
	case strings.HasSuffix(cleanPath, ".mmd"):
		b := strings.TrimSuffix(cleanPath, ".mmd")
		if b == "" {
			b = "/"
		}
		return b, "mmd"
	case strings.HasSuffix(cleanPath, ".md"):
		b := strings.TrimSuffix(cleanPath, ".md")
		if b == "" {
			b = "/"
		}
		return b, "md"
	case strings.HasSuffix(cleanPath, ".html"):
		b := strings.TrimSuffix(cleanPath, ".html")
		if b == "" {
			b = "/"
		}
		return b, "html"
	case strings.HasSuffix(cleanPath, ".svg"):
		return cleanPath, "svg"
	default:
		return cleanPath, ""
	}
}

func acceptPrefersMarkdown(acceptHeader string) bool {
	if strings.TrimSpace(acceptHeader) == "" {
		return false
	}
	maxMarkdownQ := -1.0
	maxHTMLQ := -1.0
	maxWildcardQ := -1.0

	for _, rawClause := range strings.Split(acceptHeader, ",") {
		parts := strings.Split(rawClause, ";")
		mediaType := strings.ToLower(strings.TrimSpace(parts[0]))
		if mediaType == "" {
			continue
		}
		q := 1.0
		validQ := true
		for _, param := range parts[1:] {
			kv := strings.SplitN(strings.TrimSpace(param), "=", 2)
			if len(kv) == 2 && strings.EqualFold(strings.TrimSpace(kv[0]), "q") {
				parsed, err := strconv.ParseFloat(strings.TrimSpace(kv[1]), 64)
				if err != nil || parsed < 0 || parsed > 1 {
					validQ = false
					break
				}
				q = parsed
			}
		}
		if !validQ {
			continue
		}
		switch mediaType {
		case "text/markdown", "text/x-markdown":
			if q > maxMarkdownQ {
				maxMarkdownQ = q
			}
		case "text/html", "application/xhtml+xml":
			if q > maxHTMLQ {
				maxHTMLQ = q
			}
		case "text/*", "*/*":
			if q > maxWildcardQ {
				maxWildcardQ = q
			}
		}
	}

	effectiveHTMLQ := maxHTMLQ
	if effectiveHTMLQ < 0 && maxWildcardQ >= 0 {
		effectiveHTMLQ = maxWildcardQ
	}
	return maxMarkdownQ > 0 && maxMarkdownQ >= effectiveHTMLQ
}

func negotiateResponseFormat(r *http.Request, explicitFormat string) (wantsMarkdown, rawExact bool) {
	if r == nil {
		return explicitFormat == "md" || explicitFormat == "mmd", false
	}
	q := r.URL.Query()
	qRaw := strings.ToLower(strings.TrimSpace(q.Get("raw")))
	rawExact = qRaw == "exact"
	if qRaw == "1" || qRaw == "true" || qRaw == "exact" {
		return true, rawExact
	}
	qFmt := strings.ToLower(strings.TrimSpace(q.Get("format")))
	switch qFmt {
	case "md", "markdown":
		return true, rawExact
	case "html":
		return false, false
	}
	switch explicitFormat {
	case "md", "mmd":
		return true, rawExact
	case "html", "svg":
		return false, false
	}
	return acceptPrefersMarkdown(r.Header.Get("Accept")), rawExact
}

func splitDiagramSuffix(rawSegment string) (chapterPart string, diagramIdx int, hasDiagram bool) {
	cleaned := strings.Trim(rawSegment, "/")
	const marker = "/diagrams/"
	idx := strings.LastIndex(cleaned, marker)
	if idx <= 0 {
		return rawSegment, 0, false
	}
	numStr := strings.TrimSpace(cleaned[idx+len(marker):])
	for _, ext := range []string{".svg", ".mmd", ".md", ".html"} {
		numStr = strings.TrimSuffix(numStr, ext)
	}
	n, err := strconv.Atoi(numStr)
	if err != nil || n < 1 {
		return rawSegment, 0, false
	}
	return cleaned[:idx], n, true
}

func writeEngineHTTPError(w http.ResponseWriter, r *http.Request, err error) {
	if errors.Is(err, engine.ErrInvalidSlug) || errors.Is(err, engine.ErrInvalidArgument) {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	http.NotFound(w, r)
}

func serveChapterDiagram(eng *engine.BookEngine, snap *renderer.RenderedBook, slug, rev string, isPermalink bool, diagramIdx int, explicitFormat string, wantsMarkdown, rawExact bool, w http.ResponseWriter, r *http.Request) {
	normSlug, _, slugErr := engine.NormalizeChapterOrMetaSlug(slug)
	if slugErr != nil {
		if wantsMarkdown {
			http.Error(w, slugErr.Error(), http.StatusBadRequest)
			return
		}
		http.NotFound(w, r)
		return
	}
	var ch *renderer.ChapterView
	if !isPermalink && rev == "" {
		ch = snap.BySlug[normSlug]
	}
	if ch == nil {
		cv, err := eng.RenderChapterAtRevision(r.Context(), normSlug, rev, isPermalink)
		if err != nil {
			if wantsMarkdown {
				writeEngineHTTPError(w, r, err)
				return
			}
			http.NotFound(w, r)
			return
		}
		ch = cv
	}
	diagrams := renderer.ExtractMermaidDiagrams(ch.Markdown)
	if diagramIdx < 1 || diagramIdx > len(diagrams) {
		http.NotFound(w, r)
		return
	}
	if explicitFormat == "svg" {
		target := fmt.Sprintf("/chapters/%s/diagrams/%d", ch.Slug, diagramIdx)
		if isPermalink && rev != "" {
			target = fmt.Sprintf("/chapters/%s/rev/%s/diagrams/%d", ch.Slug, rev, diagramIdx)
		}
		http.Redirect(w, r, target, http.StatusTemporaryRedirect)
		return
	}
	if snap.HeadCommitSHA != "" {
		w.Header().Set("X-Book-Head-Revision", snap.HeadCommitSHA)
	}
	if snap.HeadRevisionNumber > 0 {
		w.Header().Set("X-Book-Head-Revision-Number", fmt.Sprintf("r%d", snap.HeadRevisionNumber))
	}
	if ch.LastCommitSHA != "" {
		w.Header().Set("X-Book-Revision", ch.LastCommitSHA)
	}
	if ch.RevisionNumber > 0 {
		w.Header().Set("X-Book-Revision-Number", fmt.Sprintf("r%d", ch.RevisionNumber))
	}
	w.Header().Set("X-Chapter-Slug", ch.Slug)
	if ch.LifecycleStatus != "" {
		w.Header().Set("X-Chapter-Lifecycle", ch.LifecycleStatus)
	} else {
		w.Header().Set("X-Chapter-Lifecycle", "current")
	}

	if wantsMarkdown {
		mmdBody := diagrams[diagramIdx-1]
		if !strings.HasSuffix(mmdBody, "\n") {
			mmdBody += "\n"
		}
		altHTML := fmt.Sprintf("/chapters/%s/diagrams/%d", ch.Slug, diagramIdx)
		canonMD := fmt.Sprintf("/chapters/%s/diagrams/%d.mmd", ch.Slug, diagramIdx)
		if isPermalink && rev != "" && ch.LastCommitSHA != "" {
			altHTML = fmt.Sprintf("/chapters/%s/rev/%s/diagrams/%d", ch.Slug, ch.LastCommitSHA, diagramIdx)
		}
		w.Header().Set("X-Chapter-Is-Latest", strconv.FormatBool(ch.IsLatest || (!isPermalink && rev == "")))
		writeMarkdownPayload(w, r, mmdBody, rawExact && isPermalink && rev != "", altHTML, canonMD)
		return
	}

	diagData := renderer.DiagramPageData{
		Book:          snap,
		ActiveChapter: ch,
		BaseURL:       eng.BaseURL(),
		DiagramIndex:  diagramIdx,
		DiagramSource: diagrams[diagramIdx-1],
	}
	pageBytes, err := renderer.RenderDiagramPage(diagData)
	writeHTMLResponse(w, r, pageBytes, err, diagData.MarkdownPath())
}

func tryRelativeSlugRedirect(snap *renderer.RenderedBook, rev string, wantsMarkdown bool, w http.ResponseWriter, r *http.Request) bool {
	if strings.TrimSpace(rev) == "" || snap == nil {
		return false
	}
	candSlug, isMeta, err := engine.NormalizeChapterOrMetaSlug(rev)
	if err != nil {
		return false
	}
	if _, ok := snap.BySlug[candSlug]; !ok && !isMeta {
		return false
	}
	target := "/chapters/" + candSlug
	if isMeta {
		target = "/" + candSlug
	}
	if wantsMarkdown {
		target += ".md"
	}
	http.Redirect(w, r, target, http.StatusTemporaryRedirect)
	return true
}

func serveChapterMarkdown(eng *engine.BookEngine, snap *renderer.RenderedBook, rawSlug, rev string, isPermalink, rawExact bool, w http.ResponseWriter, r *http.Request) {
	normSlug, isMeta, err := engine.NormalizeChapterOrMetaSlug(rawSlug)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	readOut, err := eng.ReadChapter(r.Context(), &engine.ReadChapterInput{
		Slug:     normSlug,
		Revision: rev,
	})
	if err != nil {
		if isPermalink && rev != "" && errors.Is(err, engine.ErrNotFound) {
			if tryRelativeSlugRedirect(snap, rev, true, w, r) {
				return
			}
		}
		writeEngineHTTPError(w, r, err)
		return
	}

	if snap.HeadCommitSHA != "" {
		w.Header().Set("X-Book-Head-Revision", snap.HeadCommitSHA)
	}
	if snap.HeadRevisionNumber > 0 {
		w.Header().Set("X-Book-Head-Revision-Number", fmt.Sprintf("r%d", snap.HeadRevisionNumber))
	}
	if readOut.RevisionSHA != "" {
		w.Header().Set("X-Book-Revision", readOut.RevisionSHA)
	}
	if readOut.RevisionNumber > 0 {
		w.Header().Set("X-Book-Revision-Number", fmt.Sprintf("r%d", readOut.RevisionNumber))
	}

	lifecycle := readOut.LifecycleStatus
	if lifecycle == "" {
		lifecycle = "current"
	}
	w.Header().Set("X-Chapter-Slug", readOut.Slug)
	w.Header().Set("X-Chapter-Lifecycle", lifecycle)
	w.Header().Set("X-Chapter-Is-Latest", strconv.FormatBool(readOut.IsLatest))

	base := eng.BaseURL()
	liveTargetSlug := readOut.Slug
	if (lifecycle == "moved" || lifecycle == "deleted") && readOut.MovedToSlug != "" {
		liveTargetSlug = readOut.MovedToSlug
	}
	var canonMDPath string
	if isMeta {
		canonMDPath = "/" + readOut.Slug + ".md"
		w.Header().Set("X-Chapter-Latest-Markdown-URL", base+canonMDPath)
	} else if lifecycle != "deleted" || readOut.MovedToSlug != "" {
		canonMDPath = "/chapters/" + liveTargetSlug + ".md"
		w.Header().Set("X-Chapter-Latest-Markdown-URL", base+canonMDPath)
	}

	words := len(strings.Fields(readOut.ContentMarkdown))
	w.Header().Set("X-Chapter-Word-Count", strconv.Itoa(words))

	body := readOut.ContentMarkdown
	shouldAnnotate := !rawExact && (isPermalink || rev != "" || !readOut.IsLatest || lifecycle != "current")
	if shouldAnnotate {
		body = engine.FormatPermalinkLineageMarkdownBanner(base, readOut) + body
	}

	altHTMLPath := "/chapters/" + readOut.Slug
	if isPermalink && readOut.RevisionSHA != "" {
		altHTMLPath = "/rev/" + readOut.RevisionSHA + "/chapters/" + readOut.Slug
	}
	immutable := rawExact && (isPermalink || rev != "") && readOut.RevisionSHA != ""
	writeMarkdownPayload(w, r, body, immutable, altHTMLPath, canonMDPath)
}

func serveTimelineRSS(eng *engine.BookEngine, snap *renderer.RenderedBook, w http.ResponseWriter, r *http.Request) {
	xmlBytes, err := renderer.RenderTimelineRSS(snap, eng.BaseURL())
	if err != nil {
		slog.Error("render_timeline_rss_failed", "error", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	if snap.HeadCommitSHA != "" {
		w.Header().Set("X-Book-Head-Revision", snap.HeadCommitSHA)
	}
	if snap.HeadRevisionNumber > 0 {
		w.Header().Set("X-Book-Head-Revision-Number", fmt.Sprintf("r%d", snap.HeadRevisionNumber))
	}
	etag := computeStrongETag(xmlBytes)
	w.Header().Set("Content-Type", "application/rss+xml; charset=utf-8")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Vary", "Accept, User-Agent")
	w.Header().Set("ETag", etag)
	w.Header().Set("Link", buildDiscoveryLinkHeader("/timeline", "text/html", "/feed.xml", "application/rss+xml"))
	if r != nil && etagMatches(r.Header.Get("If-None-Match"), etag) {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	w.Header().Set("Content-Length", strconv.Itoa(len(xmlBytes)))
	if r != nil && r.Method == http.MethodHead {
		w.WriteHeader(http.StatusOK)
		return
	}
	_, _ = w.Write(xmlBytes)
}

func serveWebStudio(eng *engine.BookEngine, urlPrefixStrip string, w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	snap := eng.CurrentSnapshot()
	if snap == nil || len(snap.Chapters) == 0 {
		http.Error(w, "book snapshot not initialized", http.StatusServiceUnavailable)
		return
	}

	cleanPath := normalizeStudioPath(r.URL.Path, urlPrefixStrip)
	if cleanPath == "/feed.xml" || cleanPath == "/rss.xml" || cleanPath == "/timeline.xml" {
		serveTimelineRSS(eng, snap, w, r)
		return
	}
	basePath, explicitFormat := detectPathFormatExtension(cleanPath)
	wantsMarkdown, rawExact := negotiateResponseFormat(r, explicitFormat)
	queryRev := strings.TrimSpace(r.URL.Query().Get("rev"))

	switch {
	case cleanPath == "/llms.txt" || basePath == "/index" || (basePath == "/" && wantsMarkdown):
		res, err := eng.AssembleLLMsIndex(r.Context(), queryRev)
		if err != nil {
			writeEngineHTTPError(w, r, err)
			return
		}
		writeMarkdownExportResult(w, r, res, rawExact && queryRev != "", "/", "/llms.txt")

	case cleanPath == "/llms-full.txt" || basePath == "/book" || (basePath == "/print" && wantsMarkdown):
		res, err := eng.AssembleFullBookMarkdown(r.Context(), queryRev)
		if err != nil {
			writeEngineHTTPError(w, r, err)
			return
		}
		writeMarkdownExportResult(w, r, res, rawExact && queryRev != "", "/print", "/llms-full.txt")

	case basePath == "/" || basePath == "/timeline":
		if basePath == "/timeline" && wantsMarkdown {
			res, err := eng.AssembleTimelineMarkdown(r.Context())
			if err != nil {
				slog.Error("assemble_timeline_markdown_failed", "error", err)
				http.Error(w, "internal server error", http.StatusInternalServerError)
				return
			}
			writeMarkdownExportResult(w, r, res, false, "/timeline", "/timeline.md")
			return
		}
		if basePath == "/" && queryRev != "" {
			serveChapterPermalinkOrLive(eng, snap, snap.Chapters[0].Slug, queryRev, true, w, r)
			return
		}
		if snap.HeadCommitSHA != "" {
			w.Header().Set("X-Book-Head-Revision", snap.HeadCommitSHA)
		}
		if snap.HeadRevisionNumber > 0 {
			w.Header().Set("X-Book-Head-Revision-Number", fmt.Sprintf("r%d", snap.HeadRevisionNumber))
		}
		qAll := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("all")))
		canonRoute := "/"
		if basePath == "/timeline" {
			canonRoute = "/timeline"
		}
		homeData := renderer.HomePageData{
			Book:            snap,
			BaseURL:         eng.BaseURL(),
			ShowAllTimeline: qAll == "1" || qAll == "true",
			CanonicalRoute:  canonRoute,
		}
		pageBytes, err := renderer.RenderHomePage(homeData)
		writeHTMLResponse(w, r, pageBytes, err, homeData.MarkdownPath())

	case basePath == "/search":
		q := strings.TrimSpace(r.URL.Query().Get("q"))
		slugFilter := strings.TrimSpace(r.URL.Query().Get("slug"))
		if wantsMarkdown {
			res, err := eng.AssembleSearchMarkdown(r.Context(), q, slugFilter)
			if err != nil {
				writeEngineHTTPError(w, r, err)
				return
			}
			writeMarkdownExportResult(w, r, res, false, "/search", "/search.md")
			return
		}
		var results []renderer.SearchResultView
		if q != "" {
			out, err := eng.SearchBook(r.Context(), &engine.SearchBookInput{
				Query: q,
				Slug:  slugFilter,
				Limit: 25,
			})
			if err != nil {
				writeEngineHTTPError(w, r, err)
				return
			}
			for _, m := range out.Matches {
				var lines []renderer.SearchResultLineView
				for _, lm := range m.LineMatches {
					lines = append(lines, renderer.SearchResultLineView{
						LineNumber: lm.LineNumber,
						LineText:   lm.LineText,
					})
				}
				results = append(results, renderer.SearchResultView{
					Slug:        m.Slug,
					Title:       m.Title,
					Snippet:     m.Snippet,
					BM25Score:   m.BM25Score,
					LineMatches: lines,
				})
			}
		}
		sData := renderer.SearchPageData{
			Book:       snap,
			BaseURL:    eng.BaseURL(),
			Query:      q,
			SlugFilter: slugFilter,
			Results:    results,
		}
		pageBytes, err := renderer.RenderSearchPage(sData)
		writeHTMLResponse(w, r, pageBytes, err, sData.MarkdownPath())

	case basePath == "/activity":
		authorFilter := strings.TrimSpace(r.URL.Query().Get("author"))
		if wantsMarkdown {
			res, err := eng.AssembleActivityMarkdown(r.Context(), authorFilter)
			if err != nil {
				slog.Error("assemble_activity_markdown_failed", "error", err)
				http.Error(w, "internal server error", http.StatusInternalServerError)
				return
			}
			writeMarkdownExportResult(w, r, res, false, "/activity", "/activity.md")
			return
		}
		actOut, err := eng.ListAgentActivity(r.Context(), &engine.ListAgentActivityInput{
			Limit:    50,
			AuthorID: authorFilter,
		})
		if err != nil {
			slog.Error("list_agent_activity_failed", "error", err)
			http.Error(w, "internal server error", http.StatusInternalServerError)
			return
		}
		actData := renderer.ActivityPageData{
			Book:         snap,
			BaseURL:      eng.BaseURL(),
			AuthorFilter: authorFilter,
			Entries:      actOut.Entries,
		}
		pageBytes, err := renderer.RenderActivityPage(actData)
		writeHTMLResponse(w, r, pageBytes, err, actData.MarkdownPath())

	case basePath == "/guide":
		if wantsMarkdown {
			res, err := eng.AssembleGuideMarkdown(r.Context())
			if err != nil {
				slog.Error("assemble_guide_markdown_failed", "error", err)
				http.Error(w, "internal server error", http.StatusInternalServerError)
				return
			}
			writeMarkdownExportResult(w, r, res, false, "/guide", "/guide.md")
			return
		}
		gdData := renderer.GuidePageData{
			Book:    snap,
			BaseURL: eng.BaseURL(),
		}
		pageBytes, err := renderer.RenderGuidePage(gdData)
		writeHTMLResponse(w, r, pageBytes, err, gdData.MarkdownPath())

	case basePath == "/changelog":
		if wantsMarkdown {
			res, err := eng.AssembleChangelogMarkdown(r.Context())
			if err != nil {
				slog.Error("assemble_changelog_markdown_failed", "error", err)
				http.Error(w, "internal server error", http.StatusInternalServerError)
				return
			}
			writeMarkdownExportResult(w, r, res, false, "/changelog", "/changelog.md")
			return
		}
		clData := renderer.ChangelogPageData{
			Book:    snap,
			BaseURL: eng.BaseURL(),
		}
		pageBytes, err := renderer.RenderChangelogPage(clData)
		writeHTMLResponse(w, r, pageBytes, err, clData.MarkdownPath())

	case basePath == "/print":
		printSnap := snap
		if queryRev != "" {
			revBook, revErr := eng.RenderBookAtRevision(queryRev)
			if revErr != nil {
				writeEngineHTTPError(w, r, revErr)
				return
			}
			printSnap = revBook
		}
		prData := renderer.PrintPageData{
			Book:           printSnap,
			BaseURL:        eng.BaseURL(),
			PinnedRevision: queryRev,
		}
		pageBytes, err := renderer.RenderPrintPage(prData)
		writeHTMLResponse(w, r, pageBytes, err, prData.MarkdownPath())

	case strings.HasPrefix(basePath, "/rev/"):
		// Support /rev/{rev}/llms.txt, /rev/{rev}/book.md, /rev/{rev}/chapters/{slug}[.md], /rev/{rev}/{slug}[.md], or /rev/{rev}
		rest := strings.TrimPrefix(basePath, "/rev/")
		slashIdx := strings.IndexByte(rest, '/')
		if slashIdx < 0 {
			if wantsMarkdown {
				res, err := eng.AssembleLLMsIndex(r.Context(), rest)
				if err != nil {
					writeEngineHTTPError(w, r, err)
					return
				}
				writeMarkdownExportResult(w, r, res, rawExact && rest != "", "/rev/"+rest, "/llms.txt")
				return
			}
			serveChapterPermalinkOrLive(eng, snap, snap.Chapters[0].Slug, rest, true, w, r)
			return
		}
		rev := rest[:slashIdx]
		sub := rest[slashIdx+1:]
		switch {
		case sub == "llms.txt" || sub == "index":
			res, err := eng.AssembleLLMsIndex(r.Context(), rev)
			if err != nil {
				writeEngineHTTPError(w, r, err)
				return
			}
			writeMarkdownExportResult(w, r, res, rawExact && rev != "", "/rev/"+rev, "/llms.txt")
			return
		case sub == "llms-full.txt" || sub == "book" || (sub == "print" && wantsMarkdown):
			res, err := eng.AssembleFullBookMarkdown(r.Context(), rev)
			if err != nil {
				writeEngineHTTPError(w, r, err)
				return
			}
			writeMarkdownExportResult(w, r, res, rawExact && rev != "", "/print?rev="+rev, "/llms-full.txt")
			return
		case sub == "print":
			revBook, revErr := eng.RenderBookAtRevision(rev)
			if revErr != nil {
				writeEngineHTTPError(w, r, revErr)
				return
			}
			prData := renderer.PrintPageData{
				Book:           revBook,
				BaseURL:        eng.BaseURL(),
				PinnedRevision: rev,
			}
			pageBytes, err := renderer.RenderPrintPage(prData)
			writeHTMLResponse(w, r, pageBytes, err, prData.MarkdownPath())
			return
		}
		rem := strings.TrimPrefix(sub, "chapters/")
		if chPart, diagIdx, hasDiag := splitDiagramSuffix(rem); hasDiag {
			slug := trimChapterSlugSuffix(chPart)
			serveChapterDiagram(eng, snap, slug, rev, true, diagIdx, explicitFormat, wantsMarkdown, rawExact, w, r)
			return
		}
		slug := trimChapterSlugSuffix(rem)
		if wantsMarkdown {
			serveChapterMarkdown(eng, snap, slug, rev, true, rawExact, w, r)
			return
		}
		serveChapterPermalinkOrLive(eng, snap, slug, rev, true, w, r)

	case strings.HasPrefix(basePath, "/chapters/"):
		raw := strings.TrimPrefix(basePath, "/chapters/")
		if chPart, isHist := splitHistorySuffix(raw); isHist {
			serveChapterHistory(eng, snap, trimChapterSlugSuffix(chPart), wantsMarkdown, w, r)
			return
		}
		if chPart, diagIdx, hasDiag := splitDiagramSuffix(raw); hasDiag {
			slug, rev, isPermalink := splitChapterAndRevision(chPart, queryRev)
			serveChapterDiagram(eng, snap, slug, rev, isPermalink, diagIdx, explicitFormat, wantsMarkdown, rawExact, w, r)
			return
		}
		slug, rev, isPermalink := splitChapterAndRevision(raw, queryRev)
		if wantsMarkdown {
			serveChapterMarkdown(eng, snap, slug, rev, isPermalink, rawExact, w, r)
			return
		}
		serveChapterPermalinkOrLive(eng, snap, slug, rev, isPermalink, w, r)

	default:
		raw := strings.TrimPrefix(basePath, "/")
		if chPart, isHist := splitHistorySuffix(raw); isHist {
			serveChapterHistory(eng, snap, trimChapterSlugSuffix(chPart), wantsMarkdown, w, r)
			return
		}
		if chPart, diagIdx, hasDiag := splitDiagramSuffix(raw); hasDiag {
			slug, rev, isPermalink := splitChapterAndRevision(chPart, queryRev)
			serveChapterDiagram(eng, snap, slug, rev, isPermalink, diagIdx, explicitFormat, wantsMarkdown, rawExact, w, r)
			return
		}
		slug, rev, isPermalink := splitChapterAndRevision(raw, queryRev)
		if wantsMarkdown {
			serveChapterMarkdown(eng, snap, slug, rev, isPermalink, rawExact, w, r)
			return
		}
		if !isPermalink {
			if ch, ok := snap.BySlug[slug]; ok {
				renderAndWriteChapter(eng, snap, ch, w, r)
				return
			}
		}
		if cv, err := eng.RenderChapterAtRevision(r.Context(), slug, rev, isPermalink); err == nil {
			renderAndWriteChapter(eng, snap, cv, w, r)
			return
		} else if isPermalink && rev != "" && errors.Is(err, engine.ErrNotFound) {
			if tryRelativeSlugRedirect(snap, rev, false, w, r) {
				return
			}
		}
		http.NotFound(w, r)
	}
}

func serveChapterPermalinkOrLive(eng *engine.BookEngine, snap *renderer.RenderedBook, slug, rev string, isPermalink bool, w http.ResponseWriter, r *http.Request) {
	if !isPermalink && rev == "" {
		if ch, ok := snap.BySlug[slug]; ok {
			renderAndWriteChapter(eng, snap, ch, w, r)
			return
		}
	}
	cv, err := eng.RenderChapterAtRevision(r.Context(), slug, rev, isPermalink)
	if err != nil {
		if isPermalink && rev != "" && errors.Is(err, engine.ErrNotFound) {
			if tryRelativeSlugRedirect(snap, rev, false, w, r) {
				return
			}
		}
		http.NotFound(w, r)
		return
	}
	renderAndWriteChapter(eng, snap, cv, w, r)
}

func renderAndWriteChapter(eng *engine.BookEngine, snap *renderer.RenderedBook, ch *renderer.ChapterView, w http.ResponseWriter, r *http.Request) {
	if snap.HeadCommitSHA != "" {
		w.Header().Set("X-Book-Head-Revision", snap.HeadCommitSHA)
	}
	if snap.HeadRevisionNumber > 0 {
		w.Header().Set("X-Book-Head-Revision-Number", fmt.Sprintf("r%d", snap.HeadRevisionNumber))
	}
	if ch.LastCommitSHA != "" {
		w.Header().Set("X-Book-Revision", ch.LastCommitSHA)
	}
	if ch.RevisionNumber > 0 {
		w.Header().Set("X-Book-Revision-Number", fmt.Sprintf("r%d", ch.RevisionNumber))
	}
	if ch.LifecycleStatus != "" {
		w.Header().Set("X-Chapter-Lifecycle", ch.LifecycleStatus)
	} else {
		w.Header().Set("X-Chapter-Lifecycle", "current")
	}
	w.Header().Set("X-Chapter-Slug", ch.Slug)
	chData := renderer.ChapterPageData{
		Book:          snap,
		ActiveChapter: ch,
		BaseURL:       eng.BaseURL(),
	}
	pageBytes, err := renderer.RenderChapterPage(chData)
	writeHTMLResponse(w, r, pageBytes, err, chData.MarkdownPath())
}
