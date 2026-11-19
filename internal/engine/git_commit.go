package engine

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/config"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/format/index"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/go-git/go-git/v5/plumbing/transport"
	githttp "github.com/go-git/go-git/v5/plumbing/transport/http"
	gitssh "github.com/go-git/go-git/v5/plumbing/transport/ssh"
	"github.com/skeema/knownhosts"
	"github.com/wjkoh/ghostbook/internal/store"
	"golang.org/x/crypto/ssh"
)

const defaultStyleGuideMarkdown = `# Editorial Style Guide

## 1. Core Editorial Principles (Strunk & White Standard)

1. **Concise, Single-Line Titles and Headings (<= 5 Words / <= 42 Characters):** Keep all Part headers, Chapter titles (H1), and Section headings (H2, H3) to 2-5 words and at most 42 characters after the prefix.
2. **Active Voice and Unpacked Clauses:** Use direct subjects and active verbs over passive constructions. Unpack parenthetical asides into clean clauses or dedicated sentences.
3. **Omit Needless Words:** Eliminate throat-clearing introductions, redundant adverbs, and empty qualifiers.
4. **Zero Inline Backtick Clutter in Prose and Tables:** Avoid wrapping plain numbers, percentages, dollar amounts, dates, or proper nouns in inline backticks.
5. **Punctuation, Math, and Typographic Discipline:** Avoid em dashes in narrative prose; use commas, colons, semicolons, or separate sentences instead. Use native Unicode symbols and format equations with \(...\) and $$...$$.
6. **Surgical Authoring and Inline Editorial Callouts:** Always call read_chapter before calling patch_chapter. Record review questions or open action items directly in chapter Markdown using blockquote callouts.

## 2. Staged Draft-and-Swap Protocol

Apply targeted chapter updates directly via patch_chapter. For major structural refactors that reshape co-authors' sections, stage an indented sub-page under parent_slug with a draft- slug prefix, let co-authors' AI agents review and edit the draft directly, and once aligned patch the canonical parent slug and call delete_chapter with replaced_by_slug set to the parent slug.
`

// defaultBootstrapFiles provides a neutral starter book used when initializing
// a brand-new empty repository directory (for local quickstart or offline tests).
var defaultBootstrapFiles = map[string]string{
	"STYLE_GUIDE.md": defaultStyleGuideMarkdown,
	"SUMMARY.md": `# Summary

[Introduction: Living Book Handbook](00-intro.md)
[Preface: Why Agents Write](00-preface.md)

# Part I: Architecture & Workflow
- [Chapter 1: System Architecture](01-system-architecture.md)
- [Chapter 2: Agent Authoring Workflow](02-agent-workflow.md)

# Part II: Lineage & Operations
- [Chapter 3: Immutable Git Lineage](03-editorial-lineage.md)
- [Chapter 4: Single-Binary Deployment](04-deployment-ops.md)

# Appendices
- [Appendix A: MCP Tool Reference](appendix-a-mcp-reference.md)
`,
	"00-intro.md": `# Introduction: Living Book Handbook

Living Edition · Co-Authors: Alice Chen, Bob Rivera, Carol Park

## 1. Purpose

Ghostbook establishes a single, Git-backed, AI-native book where humans read cleanly on the web and AI agents perform all writing over MCP and ConnectRPC.
`,
	"00-preface.md": `# Preface: Why Agents Write

Living Edition · Co-Authors: Alice Chen, Bob Rivera, Carol Park

## 1. Beyond Browser Editors

Removing the browser WYSIWYG editor keeps every change atomic, attributed to a real co-author in Git, and immediately mirrored to raw Markdown and RSS feeds.
`,
	"01-system-architecture.md": `# Chapter 1: System Architecture

Living Edition · Co-Authors: Alice Chen, Bob Rivera, Carol Park

## 1. Core Components

Ghostbook bundles pure-Go Git storage, embedded SQLite FTS5 search, Goldmark SSR rendering, and self-hosted KaTeX and Mermaid assets inside one static binary.

## 2. Search & Indexing

Full-text indexing combines SQLite FTS5 BM25 ranking with exact 1-based line matching for punctuation-heavy queries and callout markers.
`,
	"02-agent-workflow.md": `# Chapter 2: Agent Authoring Workflow

Living Edition · Co-Authors: Alice Chen, Bob Rivera, Carol Park

## 1. Surgical Patching

AI agents inspect chapters with read_chapter and apply exact multi-hunk updates through patch_chapter under optimistic revision locks:

` + "```mermaid\nflowchart LR\n    Agent[\"AI Agent\"] --> MCP[\"Remote MCP\"]\n    MCP --> Git[\"Git Commit\"]\n```\n" + `
## 2. Quantitative Notation

Let \(f(q, D)\) denote term frequency in document \(D\) of length \(|D|\) with average length \(\bar{L}\):

$$\text{BM25}(D, q) = \text{IDF}(q) \cdot \frac{f(q, D) \cdot (k_1 + 1)}{f(q, D) + k_1 \cdot \left(1 - b + b \cdot \frac{|D|}{\bar{L}}\right)}$$
`,
	"03-editorial-lineage.md": `# Chapter 3: Immutable Git Lineage

Living Edition · Co-Authors: Alice Chen, Bob Rivera, Carol Park

## 1. Permalink Stability

Every commit receives a sequential revision number from r1 upward, and renamed or deleted chapters keep permanent historical URLs with diff-to-HEAD inspection.

## 2. Semantic Timeline Clustering

Quiet-period debouncing groups bursty agent commit sessions into readable executive timeline milestones and RSS feed items.
`,
	"04-deployment-ops.md": `# Chapter 4: Single-Binary Deployment

Living Edition · Co-Authors: Alice Chen, Bob Rivera, Carol Park

## 1. Zero-Dependency Runtime

Ghostbook compiles with CGO_ENABLED=0 and runs behind Caddy with automatic TLS, loopback-only pprof diagnostics, and staged idle-window upgrades.

## 2. Remote Git Sync

Every write triggers a debounced push to a private Git remote alongside a 60-second background synchronization loop.
`,
	"appendix-a-mcp-reference.md": `# Appendix A: MCP Tool Reference

Living Edition · Co-Authors: Alice Chen, Bob Rivera, Carol Park

## 1. Protocol Parity

All 13 authoring and audit operations are mirrored 1-for-1 across Streamable HTTP MCP, ConnectRPC JSON, and OpenAPI 3.1.
`,
}

func initOrOpenGitRepo(repoDir, explicitRemoteURL string) (*git.Repository, error) {
	if repo, err := git.PlainOpen(repoDir); err == nil {
		return repo, nil
	}

	remoteURL := strings.TrimSpace(explicitRemoteURL)
	if remoteURL == "" {
		remoteURL = strings.TrimSpace(os.Getenv("GHOSTBOOK_REMOTE_URL"))
	}
	if remoteURL == "" {
		remoteURL = strings.TrimSpace(os.Getenv("GITHUB_REMOTE_URL"))
	}
	if remoteURL != "" {
		// Refuse to overwrite or wipe a pre-existing non-empty directory on clone
		if entries, readErr := os.ReadDir(repoDir); readErr == nil && len(entries) > 0 {
			return nil, fmt.Errorf("cannot clone git remote into non-empty non-git directory %s", repoDir)
		}
		if err := os.MkdirAll(filepath.Dir(repoDir), 0o755); err != nil {
			return nil, fmt.Errorf("mkdir repo parent: %w", err)
		}
		auth, authErr := resolveGitAuth(remoteURL)
		if authErr != nil {
			return nil, fmt.Errorf("resolve git auth: %w", authErr)
		}
		tmpCloneDir, err := os.MkdirTemp(filepath.Dir(repoDir), ".ghostbook-clone-*")
		if err != nil {
			return nil, fmt.Errorf("create temp clone dir: %w", err)
		}
		cloneCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		_, err = git.PlainCloneContext(cloneCtx, tmpCloneDir, false, &git.CloneOptions{
			URL:  remoteURL,
			Auth: auth,
		})
		if err == nil {
			_ = os.Remove(repoDir)
			if renameErr := os.Rename(tmpCloneDir, repoDir); renameErr != nil {
				_ = os.RemoveAll(tmpCloneDir)
				return nil, fmt.Errorf("move cloned repo into place: %w", renameErr)
			}
			return git.PlainOpen(repoDir)
		}
		_ = os.RemoveAll(tmpCloneDir)
		if !errors.Is(err, transport.ErrEmptyRemoteRepository) {
			return nil, fmt.Errorf("clone configured git remote: %w", err)
		}
	}

	if err := os.MkdirAll(filepath.Join(repoDir, "src"), 0o755); err != nil {
		return nil, fmt.Errorf("mkdir repo src: %w", err)
	}

	repo, err := git.PlainInit(repoDir, false)
	if err != nil {
		return nil, fmt.Errorf("git plain init: %w", err)
	}

	wt, err := repo.Worktree()
	if err != nil {
		return nil, fmt.Errorf("git worktree: %w", err)
	}

	names := make([]string, 0, len(defaultBootstrapFiles))
	for name := range defaultBootstrapFiles {
		names = append(names, name)
	}
	sort.Strings(names)

	for _, name := range names {
		dst := filepath.Join(repoDir, "src", name)
		if err := os.WriteFile(dst, []byte(defaultBootstrapFiles[name]), 0o644); err != nil {
			return nil, err
		}
		if _, err := wt.Add(filepath.ToSlash(filepath.Join("src", name))); err != nil {
			return nil, err
		}
	}

	_, err = wt.Commit("Initial Ghostbook seed", &git.CommitOptions{
		Author: &object.Signature{
			Name:  "Ghostbook",
			Email: "ghostbook@example.com",
			When:  time.Now().UTC(),
		},
	})
	if err != nil {
		return nil, fmt.Errorf("initial git commit: %w", err)
	}

	if remoteURL != "" {
		_, _ = repo.CreateRemote(&config.RemoteConfig{
			Name: "origin",
			URLs: []string{remoteURL},
		})
	}

	return repo, nil
}

func (e *BookEngine) resetWorktreeLocked(extraRemoveFullPaths ...string) {
	for _, p := range extraRemoveFullPaths {
		if strings.TrimSpace(p) != "" {
			_ = os.Remove(p)
		}
	}
	if wt, err := e.repo.Worktree(); err == nil {
		_ = wt.Reset(&git.ResetOptions{Mode: git.HardReset})
	}
}

func (e *BookEngine) commitPathsLocked(author *store.Author, commitMsg string, relPaths ...string) (string, error) {
	return e.removeAndCommitPathsLocked(author, commitMsg, nil, relPaths)
}

func (e *BookEngine) removeAndCommitPathsLocked(author *store.Author, commitMsg string, removeRelPaths []string, addRelPaths []string) (string, error) {
	wt, err := e.repo.Worktree()
	if err != nil {
		return "", fmt.Errorf("worktree: %w", err)
	}
	for _, p := range removeRelPaths {
		if _, rmErr := wt.Remove(filepath.ToSlash(p)); rmErr != nil && !errors.Is(rmErr, os.ErrNotExist) && !errors.Is(rmErr, git.ErrGlobNoMatches) && !errors.Is(rmErr, index.ErrEntryNotFound) {
			return "", fmt.Errorf("git rm %s: %w", p, rmErr)
		}
	}
	for _, p := range addRelPaths {
		if _, err := wt.Add(filepath.ToSlash(p)); err != nil {
			return "", fmt.Errorf("git add %s: %w", p, err)
		}
	}
	now := time.Now().UTC()
	sig := &object.Signature{
		Name:  author.DisplayName,
		Email: author.GitEmail,
		When:  now,
	}
	hash, err := wt.Commit(commitMsg, &git.CommitOptions{
		Author:    sig,
		Committer: sig,
	})
	if err != nil {
		return "", fmt.Errorf("git commit: %w", err)
	}
	return hash.String(), nil
}

func shortSHA(full string) string {
	if len(full) > 7 {
		return full[:7]
	}
	return full
}

func (e *BookEngine) headCommitLocked() (*object.Commit, error) {
	ref, err := e.repo.Head()
	if err != nil {
		return nil, err
	}
	return e.repo.CommitObject(ref.Hash())
}

// orderedCommitsLocked returns all commits from root (index 0, revision 1) to HEAD (revision N)
// by walking the cryptographic first-parent hash chain, along with a map from full SHA to 1-based revision number.
func (e *BookEngine) orderedCommitsLocked() ([]*object.Commit, map[string]int32, error) {
	head, err := e.headCommitLocked()
	if err != nil {
		return nil, nil, err
	}
	var reverseCommits []*object.Commit
	seen := make(map[plumbing.Hash]bool)
	for cur := head; cur != nil && !seen[cur.Hash]; {
		seen[cur.Hash] = true
		reverseCommits = append(reverseCommits, cur)
		if cur.NumParents() == 0 {
			break
		}
		parent, pErr := cur.Parent(0)
		if pErr != nil {
			break
		}
		cur = parent
	}
	n := len(reverseCommits)
	chronological := make([]*object.Commit, n)
	revMap := make(map[string]int32, n)
	for i := 0; i < n; i++ {
		c := reverseCommits[n-1-i]
		chronological[i] = c
		revMap[c.Hash.String()] = int32(i + 1)
	}
	return chronological, revMap, nil
}

func parseRevisionNumber(raw string) (int, bool) {
	s := strings.TrimSpace(strings.ToLower(raw))
	switch {
	case strings.HasPrefix(s, "rev-"):
		s = strings.TrimPrefix(s, "rev-")
	case strings.HasPrefix(s, "rev"):
		s = strings.TrimPrefix(s, "rev")
	case strings.HasPrefix(s, "r"):
		s = strings.TrimPrefix(s, "r")
	}
	if s == "" || len(s) > 6 {
		return 0, false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return 0, false
		}
	}
	n, err := strconv.Atoi(s)
	if err != nil || n <= 0 {
		return 0, false
	}
	return n, true
}

func (e *BookEngine) resolveCommitByPrefixLocked(shaOrRev string) (*object.Commit, error) {
	cleaned := strings.TrimSpace(strings.ToLower(shaOrRev))
	if cleaned == "" || cleaned == "head" || cleaned == "latest" {
		return e.headCommitLocked()
	}
	chronological, _, err := e.orderedCommitsLocked()
	if err != nil {
		return nil, err
	}
	if revNum, ok := parseRevisionNumber(cleaned); ok {
		if revNum >= 1 && revNum <= len(chronological) {
			return chronological[revNum-1], nil
		}
		if strings.HasPrefix(cleaned, "r") || len(cleaned) < 6 {
			return nil, fmt.Errorf("%w: revision r%d", ErrNotFound, revNum)
		}
	}
	if len(cleaned) == 40 {
		h := plumbing.NewHash(cleaned)
		if c, err := e.repo.CommitObject(h); err == nil {
			return c, nil
		}
	}
	for i := len(chronological) - 1; i >= 0; i-- {
		c := chronological[i]
		if strings.HasPrefix(strings.ToLower(c.Hash.String()), cleaned) {
			return c, nil
		}
	}
	return nil, fmt.Errorf("%w: commit %s", ErrNotFound, shaOrRev)
}

func commitModifiedFiles(c *object.Commit) ([]string, error) {
	curTree, err := c.Tree()
	if err != nil {
		return nil, err
	}
	if c.NumParents() == 0 {
		var files []string
		_ = curTree.Files().ForEach(func(f *object.File) error {
			files = append(files, f.Name)
			return nil
		})
		return files, nil
	}
	parent, err := c.Parent(0)
	if err != nil {
		return nil, err
	}
	parentTree, err := parent.Tree()
	if err != nil {
		return nil, err
	}
	changes, err := parentTree.Diff(curTree)
	if err != nil {
		return nil, err
	}
	seen := make(map[string]bool)
	var files []string
	for _, ch := range changes {
		name := ch.To.Name
		if name == "" {
			name = ch.From.Name
		}
		if name != "" && !seen[name] {
			seen[name] = true
			files = append(files, name)
		}
	}
	return files, nil
}

func containsString(list []string, target string) bool {
	for _, item := range list {
		if item == target {
			return true
		}
	}
	return false
}

func (e *BookEngine) latestCommitForFileLocked(relPath string) (*object.Commit, error) {
	chronological, _, err := e.orderedCommitsLocked()
	if err != nil || len(chronological) == 0 {
		return e.headCommitLocked()
	}
	for i := len(chronological) - 1; i >= 0; i-- {
		c := chronological[i]
		files, fErr := commitModifiedFiles(c)
		if fErr == nil && containsString(files, relPath) {
			return c, nil
		}
	}
	return chronological[len(chronological)-1], nil
}

func (e *BookEngine) listCommitsLocked(slug string, limit int) ([]CommitEntryItem, error) {
	if limit <= 0 {
		limit = 20
	}
	if limit > 500 {
		limit = 500
	}
	var targetRel string
	if strings.TrimSpace(slug) != "" {
		cleanSlug, _, err := NormalizeChapterOrMetaSlug(slug)
		if err != nil {
			return nil, err
		}
		targetRel = srcRelPathForSlug(cleanSlug)
	}

	chronological, revMap, err := e.orderedCommitsLocked()
	if err != nil {
		return nil, err
	}

	var out []CommitEntryItem
	for i := len(chronological) - 1; i >= 0; i-- {
		c := chronological[i]
		files, _ := commitModifiedFiles(c)
		if targetRel != "" && !containsString(files, targetRel) {
			continue
		}
		fullSHA := c.Hash.String()
		out = append(out, CommitEntryItem{
			CommitSHA:      shortSHA(fullSHA),
			FullSHA:        fullSHA,
			RevisionNumber: revMap[fullSHA],
			AuthorName:     c.Author.Name,
			AuthorEmail:    c.Author.Email,
			TimestampISO:   c.Author.When.UTC().Format(time.RFC3339),
			CommitMessage:  strings.TrimSpace(c.Message),
			FilesChanged:   files,
		})
		if len(out) >= limit {
			break
		}
	}
	return out, nil
}

func (e *BookEngine) ConfigureRemote(remoteURL string) error {
	cleanURL := strings.TrimSpace(remoteURL)
	if cleanURL == "" {
		return fmt.Errorf("%w: remote URL cannot be empty", ErrInvalidArgument)
	}
	e.mu.Lock()
	defer e.mu.Unlock()

	_ = e.repo.DeleteRemote("origin")
	_, err := e.repo.CreateRemote(&config.RemoteConfig{
		Name: "origin",
		URLs: []string{cleanURL},
	})
	return err
}

func resolveSSHHostKeyCallback() (ssh.HostKeyCallback, error) {
	if khPath := strings.TrimSpace(os.Getenv("GITHUB_KNOWN_HOSTS_PATH")); khPath != "" {
		cb, err := knownhosts.New(khPath)
		if err != nil {
			return nil, fmt.Errorf("load GITHUB_KNOWN_HOSTS_PATH (%s): %w", khPath, err)
		}
		return cb.HostKeyCallback(), nil
	}
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		defaultKH := filepath.Join(home, ".ssh", "known_hosts")
		if _, statErr := os.Stat(defaultKH); statErr == nil {
			if cb, err := knownhosts.New(defaultKH); err == nil {
				return cb.HostKeyCallback(), nil
			}
		}
	}
	insecure := strings.ToLower(strings.TrimSpace(os.Getenv("GHOSTBOOK_INSECURE_IGNORE_HOST_KEY")))
	if insecure == "true" || insecure == "1" {
		slog.Warn("git_ssh_insecure_ignore_host_key_enabled")
		return ssh.InsecureIgnoreHostKey(), nil
	}
	return nil, fmt.Errorf("no valid SSH known_hosts file found (set GITHUB_KNOWN_HOSTS_PATH or populate ~/.ssh/known_hosts)")
}

func isSSHRemoteURL(remoteURL string) bool {
	u := strings.TrimSpace(strings.ToLower(remoteURL))
	return strings.HasPrefix(u, "ssh://") || strings.HasPrefix(u, "git@")
}

func isHTTPRemoteURL(remoteURL string) bool {
	u := strings.TrimSpace(strings.ToLower(remoteURL))
	return strings.HasPrefix(u, "https://") || strings.HasPrefix(u, "http://")
}

func resolveGitAuth(remoteURL string) (transport.AuthMethod, error) {
	if isSSHRemoteURL(remoteURL) {
		if keyPath := strings.TrimSpace(os.Getenv("GITHUB_SSH_KEY_PATH")); keyPath != "" {
			pk, err := gitssh.NewPublicKeysFromFile("git", keyPath, "")
			if err != nil {
				return nil, fmt.Errorf("load GITHUB_SSH_KEY_PATH (%s): %w", keyPath, err)
			}
			cb, err := resolveSSHHostKeyCallback()
			if err != nil {
				return nil, err
			}
			pk.HostKeyCallback = cb
			return pk, nil
		}
		return nil, nil
	}
	if isHTTPRemoteURL(remoteURL) {
		if pat := strings.TrimSpace(os.Getenv("GITHUB_PAT")); pat != "" {
			return &githttp.BasicAuth{
				Username: "x-access-token",
				Password: pat,
			}, nil
		}
	}
	return nil, nil
}

func isIgnorablePullError(err error) bool {
	if err == nil {
		return true
	}
	if errors.Is(err, git.NoErrAlreadyUpToDate) || errors.Is(err, transport.ErrEmptyRemoteRepository) {
		return true
	}
	msg := err.Error()
	return strings.Contains(msg, "couldn't find remote ref")
}

func (e *BookEngine) originRemoteURL() (string, bool) {
	e.mu.RLock()
	defer e.mu.RUnlock()
	rem, err := e.repo.Remote("origin")
	if err != nil || rem == nil || len(rem.Config().URLs) == 0 {
		return "", false
	}
	return rem.Config().URLs[0], true
}

// pullAndPushOrigin synchronizes e.repo with the configured "origin" remote using bounded
// 15-second context timeouts derived from ctx, holding exclusive write lock only during pull
// and downgrading to a read lock during push so readers are not blocked by slow remotes.
func (e *BookEngine) pullAndPushOrigin(ctx context.Context) (bool, error) {
	remoteURL, ok := e.originRemoteURL()
	if ctx.Err() != nil || !ok {
		return false, ctx.Err()
	}

	auth, authErr := resolveGitAuth(remoteURL)
	if authErr != nil {
		return false, fmt.Errorf("git auth: %w", authErr)
	}

	e.mu.Lock()
	headBefore, _ := e.headCommitLocked()
	wt, wtErr := e.repo.Worktree()
	if wtErr != nil {
		e.mu.Unlock()
		return false, fmt.Errorf("git worktree: %w", wtErr)
	}

	pullCtx, cancelPull := context.WithTimeout(ctx, 15*time.Second)
	pullErr := wt.PullContext(pullCtx, &git.PullOptions{
		RemoteName: "origin",
		Auth:       auth,
	})
	cancelPull()
	if !isIgnorablePullError(pullErr) {
		e.mu.Unlock()
		return false, fmt.Errorf("git pull origin: %w", pullErr)
	}

	pulledNewCommits := false
	headAfter, _ := e.headCommitLocked()
	if headBefore != nil && headAfter != nil && headBefore.Hash != headAfter.Hash {
		pulledNewCommits = true
	}
	e.mu.Unlock()

	if ctx.Err() != nil {
		return pulledNewCommits, ctx.Err()
	}
	e.mu.RLock()
	pushCtx, cancelPush := context.WithTimeout(ctx, 15*time.Second)
	pushErr := e.repo.PushContext(pushCtx, &git.PushOptions{
		RemoteName: "origin",
		Auth:       auth,
	})
	cancelPush()
	e.mu.RUnlock()
	if pushErr != nil && !errors.Is(pushErr, git.NoErrAlreadyUpToDate) {
		return pulledNewCommits, fmt.Errorf("git push origin: %w", pushErr)
	}
	return pulledNewCommits, nil
}
