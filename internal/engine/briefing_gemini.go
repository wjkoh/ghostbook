package engine

import (
	"bytes"
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"
)

const (
	DefaultGeminiEndpoint    = "https://generativelanguage.googleapis.com"
	DefaultVertexEndpoint    = "https://aiplatform.googleapis.com"
	DefaultVertexLocation    = "global"
	DefaultGeminiModel       = "gemini-2.5-flash"
	defaultGoogleTokenURI    = "https://oauth2.googleapis.com/token"
	googleCloudPlatformScope = "https://www.googleapis.com/auth/cloud-platform"
)

type serviceAccountCreds struct {
	ProjectID   string `json:"project_id"`
	ClientEmail string `json:"client_email"`
	PrivateKey  string `json:"private_key"`
	TokenURI    string `json:"token_uri"`
	rsaKey      *rsa.PrivateKey
}

type BriefSynthesizerConfig struct {
	APIKey                string
	Model                 string
	Endpoint              string
	VertexProject         string
	VertexLocation        string
	VertexCredentialsFile string
}

type BriefSynthesizer struct {
	apiKey         string
	model          string
	endpoint       string
	vertexProject  string
	vertexLocation string
	saCreds        *serviceAccountCreds
	httpClient     *http.Client
	retryDelay     time.Duration

	tokenMu     sync.Mutex
	cachedToken string
	tokenExpiry time.Time
}

type BriefingChapterRef struct {
	Slug  string
	Title string
}

type TimelineBatchCommit struct {
	RevisionNumber int32
	ShortSHA       string
	TimestampISO   string
	AuthorName     string
	CommitMessage  string
	ChapterSlugs   []string
	CompactDiff    string
}

type TimelineClusterInput struct {
	MinRevision          int32
	MaxRevision          int32
	ValidChapters        []BriefingChapterRef
	PriorPublishedTitles []string
	Commits              []TimelineBatchCommit
}

type SynthesizedMilestone struct {
	Revisions    []int32
	Title        string
	Summary      string
	ChapterSlugs []string
	Authors      []string
}

type TimelineSynthesisResult struct {
	ModelName  string
	Milestones []SynthesizedMilestone
}

func NewBriefSynthesizer(apiKey, model, endpoint string) *BriefSynthesizer {
	return NewBriefSynthesizerWithConfig(BriefSynthesizerConfig{
		APIKey:   apiKey,
		Model:    model,
		Endpoint: endpoint,
	})
}

func NewBriefSynthesizerWithConfig(cfg BriefSynthesizerConfig) *BriefSynthesizer {
	ep := strings.TrimRight(strings.TrimSpace(cfg.Endpoint), "/")
	key := strings.TrimSpace(cfg.APIKey)
	credsFile := strings.TrimSpace(cfg.VertexCredentialsFile)

	if ep == "" && testing.Testing() {
		// Never dispatch real production Gemini or Vertex AI calls during `go test` unless a mock endpoint is explicitly supplied.
		key = ""
		credsFile = ""
	}

	var sa *serviceAccountCreds
	if credsFile != "" {
		parsed, err := loadServiceAccountCreds(credsFile)
		if err != nil {
			slog.Warn("vertex_credentials_load_failed", "path", credsFile, "error", err)
		} else {
			sa = parsed
		}
	}

	proj := strings.TrimSpace(cfg.VertexProject)
	if proj == "" && sa != nil {
		proj = sa.ProjectID
	}
	loc := strings.TrimSpace(cfg.VertexLocation)
	if loc == "" {
		loc = DefaultVertexLocation
	}

	mdl := strings.TrimSpace(cfg.Model)
	if mdl == "" {
		mdl = DefaultGeminiModel
	}

	if ep == "" {
		if sa != nil && proj != "" {
			if loc == "global" {
				ep = DefaultVertexEndpoint
			} else {
				ep = fmt.Sprintf("https://%s-aiplatform.googleapis.com", loc)
			}
		} else {
			ep = DefaultGeminiEndpoint
		}
	}

	return &BriefSynthesizer{
		apiKey:         key,
		model:          mdl,
		endpoint:       ep,
		vertexProject:  proj,
		vertexLocation: loc,
		saCreds:        sa,
		httpClient:     &http.Client{Timeout: 120 * time.Second},
		retryDelay:     600 * time.Millisecond,
	}
}

func loadServiceAccountCreds(path string) (*serviceAccountCreds, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var c serviceAccountCreds
	if err := json.Unmarshal(raw, &c); err != nil {
		return nil, err
	}
	c.ClientEmail = strings.TrimSpace(c.ClientEmail)
	c.ProjectID = strings.TrimSpace(c.ProjectID)
	c.TokenURI = strings.TrimSpace(c.TokenURI)
	if c.TokenURI == "" {
		c.TokenURI = defaultGoogleTokenURI
	}
	if c.ClientEmail == "" || strings.TrimSpace(c.PrivateKey) == "" {
		return nil, fmt.Errorf("missing client_email or private_key in service account json")
	}
	rsaKey, err := parseRSAPrivateKeyPEM(c.PrivateKey)
	if err != nil {
		return nil, err
	}
	c.rsaKey = rsaKey
	return &c, nil
}

func parseRSAPrivateKeyPEM(pemStr string) (*rsa.PrivateKey, error) {
	block, _ := pem.Decode([]byte(pemStr))
	if block == nil {
		return nil, fmt.Errorf("invalid PEM block in private_key")
	}
	if pkcs8Key, err := x509.ParsePKCS8PrivateKey(block.Bytes); err == nil {
		if rk, ok := pkcs8Key.(*rsa.PrivateKey); ok {
			return rk, nil
		}
		return nil, fmt.Errorf("private_key is not RSA")
	}
	return x509.ParsePKCS1PrivateKey(block.Bytes)
}

func (s *BriefSynthesizer) isVertexMode() bool {
	return s != nil && s.saCreds != nil && s.vertexProject != ""
}

func (s *BriefSynthesizer) Enabled() bool {
	if s == nil {
		return false
	}
	return s.isVertexMode() || s.apiKey != ""
}

func (s *BriefSynthesizer) Model() string {
	if s == nil || s.model == "" {
		return DefaultGeminiModel
	}
	return s.model
}

func (s *BriefSynthesizer) vertexAccessToken(ctx context.Context) (string, error) {
	now := time.Now().UTC()
	s.tokenMu.Lock()
	if s.cachedToken != "" && now.Add(2*time.Minute).Before(s.tokenExpiry) {
		tok := s.cachedToken
		s.tokenMu.Unlock()
		return tok, nil
	}
	s.tokenMu.Unlock()

	hdrJSON := `{"alg":"RS256","typ":"JWT"}`
	claimsBytes, err := json.Marshal(map[string]any{
		"iss":   s.saCreds.ClientEmail,
		"scope": googleCloudPlatformScope,
		"aud":   s.saCreds.TokenURI,
		"iat":   now.Unix(),
		"exp":   now.Add(1 * time.Hour).Unix(),
	})
	if err != nil {
		return "", err
	}

	unsigned := base64.RawURLEncoding.EncodeToString([]byte(hdrJSON)) + "." +
		base64.RawURLEncoding.EncodeToString(claimsBytes)
	digest := sha256.Sum256([]byte(unsigned))
	sig, err := rsa.SignPKCS1v15(rand.Reader, s.saCreds.rsaKey, crypto.SHA256, digest[:])
	if err != nil {
		return "", fmt.Errorf("sign service account jwt: %w", err)
	}
	assertion := unsigned + "." + base64.RawURLEncoding.EncodeToString(sig)

	form := url.Values{}
	form.Set("grant_type", "urn:ietf:params:oauth:grant-type:jwt-bearer")
	form.Set("assertion", assertion)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.saCreds.TokenURI, strings.NewReader(form.Encode()))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := s.httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("exchange service account jwt: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	respBytes, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("oauth2 token http %d: %s", resp.StatusCode, truncateUTF8(strings.TrimSpace(string(respBytes)), 200))
	}

	var tokResp struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   int    `json:"expires_in"`
	}
	if err := json.Unmarshal(respBytes, &tokResp); err != nil || strings.TrimSpace(tokResp.AccessToken) == "" {
		return "", fmt.Errorf("invalid oauth2 token response")
	}
	ttl := time.Duration(tokResp.ExpiresIn) * time.Second
	if ttl <= 0 {
		ttl = 55 * time.Minute
	}

	s.tokenMu.Lock()
	s.cachedToken = tokResp.AccessToken
	s.tokenExpiry = now.Add(ttl)
	s.tokenMu.Unlock()

	return tokResp.AccessToken, nil
}

const timelineSystemPrompt = `You are the Editorial Historian for a collaborative, Git-backed living book.

You are given a chronological batch of newly committed Git revisions (with timestamps, authors, commit messages, and compact diffs). Group them into readable, high-signal timeline milestones using relaxed semantic and temporal judgment:
1. RELAXED SEMANTIC + TEMPORAL GROUPING: Merge multiple commits that belong to the same topic, chapter refactor, draft-and-swap promotion, meeting distillation, or cohesive editing session into ONE milestone, even if spaced across 10 minutes to several hours. Conversely, if two commits close in time address genuinely different topics or separate co-author workstreams, keep them as separate milestones. Interleaved commits by concurrent co-authors can be grouped into separate milestones using their exact integer revision numbers in revisions.
2. IGNORE MINOR / COSMETIC EDITS: Completely omit commits that only fix typos, punctuation, heading lengths, formatting, style-linter warnings, or delete retired draft/archive files without changing editorial substance or numbers. If the entire batch contains ONLY minor cosmetic edits, return an empty milestones array ([]).
3. EASY-TO-READ SUMMARIES (STRUNK & WHITE): Give each milestone a clear 3 to 7 word title without a trailing colon, and a crisp, easy-to-read 2 to 3 sentence summary explaining what substantively changed and why it matters, citing concrete figures or entity names when updated. Use active voice, plain builder English, zero em dashes (—), and zero parentheses or brackets in prose.
4. STRICT REVISION & SLUG GROUNDING: Every integer in revisions MUST come strictly from the provided Unsummarized Commit Batch, and each revision number may appear in at most one milestone. Only cite chapter_slugs from Valid Active Chapters, and only cite author names that appear in the commit batch. Order milestones newest-first (highest revision number first).`

type geminiGenerateRequest struct {
	SystemInstruction *geminiContent         `json:"systemInstruction,omitempty"`
	Contents          []geminiContent        `json:"contents"`
	GenerationConfig  map[string]interface{} `json:"generationConfig,omitempty"`
}

type geminiContent struct {
	Role  string       `json:"role,omitempty"`
	Parts []geminiPart `json:"parts"`
}

type geminiPart struct {
	Text    string `json:"text,omitempty"`
	Thought bool   `json:"thought,omitempty"`
}

type geminiGenerateResponse struct {
	Candidates []struct {
		Content struct {
			Parts []geminiPart `json:"parts"`
		} `json:"content"`
	} `json:"candidates"`
}

type rawTimelinePayload struct {
	Milestones []struct {
		Revisions    []int32  `json:"revisions"`
		Title        string   `json:"title"`
		Summary      string   `json:"summary"`
		ChapterSlugs []string `json:"chapter_slugs"`
		Authors      []string `json:"authors"`
	} `json:"milestones"`
}

func buildTimelineResponseSchema() map[string]any {
	return map[string]any{
		"type": "OBJECT",
		"properties": map[string]any{
			"milestones": map[string]any{
				"type":        "ARRAY",
				"description": "Chronological editorial milestones ordered newest-first; empty if all commits in the batch are minor cosmetic edits.",
				"items": map[string]any{
					"type": "OBJECT",
					"properties": map[string]any{
						"revisions": map[string]any{
							"type":        "ARRAY",
							"description": "Integer Git revision numbers from the batch belonging to this milestone.",
							"items":       map[string]any{"type": "INTEGER"},
						},
						"title": map[string]any{
							"type": "STRING",
						},
						"summary": map[string]any{
							"type": "STRING",
						},
						"chapter_slugs": map[string]any{
							"type":  "ARRAY",
							"items": map[string]any{"type": "STRING"},
						},
						"authors": map[string]any{
							"type":  "ARRAY",
							"items": map[string]any{"type": "STRING"},
						},
					},
					"required": []string{"revisions", "title", "summary", "chapter_slugs", "authors"},
				},
			},
		},
		"required": []string{"milestones"},
	}
}

func buildTimelineUserPrompt(in TimelineClusterInput) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Unsummarized Commit Batch: r%d .. r%d (%d commits, ordered oldest to newest)\n\n",
		in.MinRevision, in.MaxRevision, len(in.Commits))

	if len(in.PriorPublishedTitles) > 0 {
		b.WriteString("Most Recent Already-Published Milestones (READ-ONLY CONTEXT — do NOT re-emit these revisions):\n")
		for _, t := range in.PriorPublishedTitles {
			fmt.Fprintf(&b, "- %s\n", t)
		}
		b.WriteString("\n")
	}

	b.WriteString("Valid Active Chapters (slug -> title):\n")
	for _, ch := range in.ValidChapters {
		fmt.Fprintf(&b, "- %s -> %s\n", ch.Slug, ch.Title)
	}
	b.WriteString("\nCommits to Cluster:\n")

	for _, c := range in.Commits {
		fmt.Fprintf(&b, "\n--- Revision r%d (%s) · %s · Author: %s ---\n",
			c.RevisionNumber, c.ShortSHA, c.TimestampISO, c.AuthorName)
		fmt.Fprintf(&b, "Commit Message: %s\n", c.CommitMessage)
		if len(c.ChapterSlugs) > 0 {
			fmt.Fprintf(&b, "Active Chapters Touched: %s\n", strings.Join(c.ChapterSlugs, ", "))
		}
		if c.CompactDiff != "" {
			fmt.Fprintf(&b, "Diff Excerpt:\n%s\n", c.CompactDiff)
		}
	}
	return b.String()
}

func (s *BriefSynthesizer) buildGenerateURL() string {
	if s.isVertexMode() {
		return fmt.Sprintf("%s/v1/projects/%s/locations/%s/publishers/google/models/%s:generateContent",
			s.endpoint, s.vertexProject, s.vertexLocation, s.model)
	}
	return fmt.Sprintf("%s/v1beta/models/%s:generateContent", s.endpoint, s.model)
}

func (s *BriefSynthesizer) SynthesizeTimeline(ctx context.Context, in TimelineClusterInput) (*TimelineSynthesisResult, error) {
	if !s.Enabled() {
		return nil, fmt.Errorf("brief synthesizer is disabled (missing Vertex AI credentials or GEMINI_API_KEY)")
	}
	if len(in.Commits) == 0 {
		return &TimelineSynthesisResult{ModelName: s.model}, nil
	}

	genCfg := map[string]any{
		"temperature":      0.2,
		"responseMimeType": "application/json",
		"responseSchema":   buildTimelineResponseSchema(),
	}
	if strings.HasPrefix(strings.ToLower(s.model), "gemini-3") {
		genCfg["thinkingConfig"] = map[string]any{
			"thinkingLevel": "LOW",
		}
	}

	reqPayload := geminiGenerateRequest{
		SystemInstruction: &geminiContent{
			Parts: []geminiPart{{Text: timelineSystemPrompt}},
		},
		Contents: []geminiContent{
			{
				Role:  "user",
				Parts: []geminiPart{{Text: buildTimelineUserPrompt(in)}},
			},
		},
		GenerationConfig: genCfg,
	}

	bodyBytes, err := json.Marshal(reqPayload)
	if err != nil {
		return nil, fmt.Errorf("marshal gemini request: %w", err)
	}

	genURL := s.buildGenerateURL()
	var parsed rawTimelinePayload
	var lastErr error

	for attempt := 0; attempt < 2; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(s.retryDelay):
			}
		}
		respBytes, retryable, callErr := s.doGenerateRequest(ctx, genURL, bodyBytes)
		if callErr != nil {
			lastErr = callErr
			if !retryable {
				break
			}
			continue
		}
		jsonText, jErr := extractGeminiJSONText(respBytes)
		if jErr != nil {
			lastErr = jErr
			continue
		}
		var candidate rawTimelinePayload
		if uErr := json.Unmarshal([]byte(jsonText), &candidate); uErr != nil {
			lastErr = fmt.Errorf("decode gemini timeline json: %w (raw: %s)", uErr, truncateUTF8(jsonText, 240))
			continue
		}
		parsed = candidate
		lastErr = nil
		break
	}
	if lastErr != nil {
		return nil, lastErr
	}

	validSlugSet := make(map[string]bool, len(in.ValidChapters))
	for _, ch := range in.ValidChapters {
		validSlugSet[ch.Slug] = true
	}
	byRevCommit := make(map[int32]TimelineBatchCommit, len(in.Commits))
	for _, c := range in.Commits {
		byRevCommit[c.RevisionNumber] = c
	}

	sanitized := sanitizeTimelinePayload(s.model, parsed, validSlugSet, byRevCommit)
	if len(parsed.Milestones) > 0 && len(sanitized.Milestones) == 0 {
		return nil, fmt.Errorf("gemini returned %d milestone(s) but none matched batch revisions r%d..r%d",
			len(parsed.Milestones), in.MinRevision, in.MaxRevision)
	}
	return sanitized, nil
}

func (s *BriefSynthesizer) doGenerateRequest(ctx context.Context, genURL string, bodyBytes []byte) ([]byte, bool, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, genURL, bytes.NewReader(bodyBytes))
	if err != nil {
		return nil, false, fmt.Errorf("build gemini request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if s.isVertexMode() {
		tok, tokErr := s.vertexAccessToken(ctx)
		if tokErr != nil {
			return nil, true, tokErr
		}
		req.Header.Set("Authorization", "Bearer "+tok)
	} else {
		req.Header.Set("x-goog-api-key", s.apiKey)
	}

	resp, err := s.httpClient.Do(req)
	if err != nil {
		return nil, true, fmt.Errorf("call gemini endpoint: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	respBytes, err := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
	if err != nil {
		return nil, true, fmt.Errorf("read gemini response: %w", err)
	}
	if resp.StatusCode == http.StatusUnauthorized && s.isVertexMode() {
		s.tokenMu.Lock()
		s.cachedToken = ""
		s.tokenExpiry = time.Time{}
		s.tokenMu.Unlock()
		return nil, true, fmt.Errorf("gemini vertex http 401 (cleared cached token): %s", truncateUTF8(strings.TrimSpace(string(respBytes)), 240))
	}
	if resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500 {
		return nil, true, fmt.Errorf("gemini http %d: %s", resp.StatusCode, truncateUTF8(strings.TrimSpace(string(respBytes)), 240))
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, false, fmt.Errorf("gemini http %d: %s", resp.StatusCode, truncateUTF8(strings.TrimSpace(string(respBytes)), 240))
	}
	return respBytes, false, nil
}

func extractGeminiJSONText(rawResp []byte) (string, error) {
	var env geminiGenerateResponse
	if err := json.Unmarshal(rawResp, &env); err != nil {
		return "", fmt.Errorf("unmarshal gemini envelope: %w", err)
	}
	if len(env.Candidates) == 0 {
		return "", fmt.Errorf("gemini returned zero candidates")
	}
	var sb strings.Builder
	for _, p := range env.Candidates[0].Content.Parts {
		if p.Thought {
			continue
		}
		sb.WriteString(p.Text)
	}
	txt := strings.TrimSpace(sb.String())
	if strings.HasPrefix(txt, "```") {
		txt = strings.TrimPrefix(txt, "```json")
		txt = strings.TrimPrefix(txt, "```")
		txt = strings.TrimSuffix(strings.TrimSpace(txt), "```")
		txt = strings.TrimSpace(txt)
	}
	if txt == "" {
		return "", fmt.Errorf("gemini candidate contained empty non-thought text")
	}
	return txt, nil
}

func sanitizeBriefingProse(raw string, maxRunes int) string {
	s := strings.ReplaceAll(raw, "\r", " ")
	s = strings.ReplaceAll(s, "\n", " ")
	s = strings.ReplaceAll(s, " — ", ", ")
	s = strings.ReplaceAll(s, "—", ", ")
	s = strings.Join(strings.Fields(s), " ")
	if maxRunes > 0 && utf8.RuneCountInString(s) > maxRunes {
		s = truncateUTF8(s, maxRunes)
	}
	return strings.TrimSpace(s)
}

func sanitizeTimelinePayload(
	model string,
	raw rawTimelinePayload,
	validSlugs map[string]bool,
	byRevCommit map[int32]TimelineBatchCommit,
) *TimelineSynthesisResult {
	usedRev := make(map[int32]bool)
	var out []SynthesizedMilestone

	for _, m := range raw.Milestones {
		title := strings.TrimRight(sanitizeBriefingProse(m.Title, 90), ":")
		summary := sanitizeBriefingProse(m.Summary, 580)
		if title == "" || summary == "" {
			continue
		}

		var revs []int32
		for _, r := range m.Revisions {
			if _, exists := byRevCommit[r]; exists && !usedRev[r] {
				usedRev[r] = true
				revs = append(revs, r)
			}
		}
		if len(revs) == 0 {
			continue
		}
		sort.Slice(revs, func(i, j int) bool { return revs[i] < revs[j] })

		var slugs []string
		seenSlug := make(map[string]bool)
		for _, rawSlug := range m.ChapterSlugs {
			clean := strings.TrimSuffix(strings.TrimPrefix(strings.TrimSpace(rawSlug), "/chapters/"), ".md")
			if validSlugs[clean] && !seenSlug[clean] {
				seenSlug[clean] = true
				slugs = append(slugs, clean)
			}
		}
		if len(slugs) == 0 {
			for _, r := range revs {
				for _, cs := range byRevCommit[r].ChapterSlugs {
					if validSlugs[cs] && !seenSlug[cs] {
						seenSlug[cs] = true
						slugs = append(slugs, cs)
					}
				}
			}
		}

		var authors []string
		seenAuthor := make(map[string]bool)
		for _, a := range m.Authors {
			ca := sanitizeBriefingProse(a, 48)
			if ca != "" && !seenAuthor[ca] {
				seenAuthor[ca] = true
				authors = append(authors, ca)
			}
		}
		if len(authors) == 0 {
			for _, r := range revs {
				ca := sanitizeBriefingProse(byRevCommit[r].AuthorName, 48)
				if ca != "" && !seenAuthor[ca] {
					seenAuthor[ca] = true
					authors = append(authors, ca)
				}
			}
		}

		out = append(out, SynthesizedMilestone{
			Revisions:    revs,
			Title:        title,
			Summary:      summary,
			ChapterSlugs: slugs,
			Authors:      authors,
		})
	}

	sort.Slice(out, func(i, j int) bool {
		endI := out[i].Revisions[len(out[i].Revisions)-1]
		endJ := out[j].Revisions[len(out[j].Revisions)-1]
		if endI != endJ {
			return endI > endJ
		}
		return out[i].Revisions[0] > out[j].Revisions[0]
	})

	return &TimelineSynthesisResult{
		ModelName:  model,
		Milestones: out,
	}
}
