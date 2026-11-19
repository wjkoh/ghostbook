package engine

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing/transport"
	"golang.org/x/crypto/ssh"
)

func TestInternalHelpers_CoverageAndEdgeCases(t *testing.T) {
	// 1. shortSHA & truncateUTF8
	if got := shortSHA("abc"); got != "abc" {
		t.Fatalf("shortSHA(abc) = %q, want abc", got)
	}
	if got := shortSHA("1234567890"); got != "1234567" {
		t.Fatalf("shortSHA(1234567890) = %q, want 1234567", got)
	}
	if got := truncateUTF8("short", 10); got != "short" {
		t.Fatalf("truncateUTF8(short) = %q", got)
	}
	// Multi-byte UTF-8 truncation boundary (Korean Hangul is 3 bytes per rune)
	if got := truncateUTF8("한글테스트", 4); !strings.HasSuffix(got, "…") {
		t.Fatalf("truncateUTF8 multi-byte = %q", got)
	}

	// 2. parseRevisionNumber
	for _, invalid := range []string{"", "r", "r0", "r1234567", "r12a", "rev-0", "-5"} {
		if _, ok := parseRevisionNumber(invalid); ok {
			t.Fatalf("expected parseRevisionNumber(%q) to be false", invalid)
		}
	}
	if n, ok := parseRevisionNumber("rev-12"); !ok || n != 12 {
		t.Fatalf("parseRevisionNumber(rev-12) = %d, %v", n, ok)
	}

	// 3. isIgnorablePullError
	if !isIgnorablePullError(nil) {
		t.Fatalf("expected nil to be ignorable")
	}
	if !isIgnorablePullError(git.NoErrAlreadyUpToDate) {
		t.Fatalf("expected NoErrAlreadyUpToDate to be ignorable")
	}
	if !isIgnorablePullError(transport.ErrEmptyRemoteRepository) {
		t.Fatalf("expected ErrEmptyRemoteRepository to be ignorable")
	}
	if !isIgnorablePullError(errors.New("couldn't find remote ref refs/heads/main")) {
		t.Fatalf("expected missing remote ref to be ignorable")
	}
	if isIgnorablePullError(errors.New("fatal network failure")) {
		t.Fatalf("expected fatal network failure not to be ignorable")
	}

	// 4. resolveGitAuth with valid temporary Ed25519 SSH key & GITHUB_PAT
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate ed25519 key: %v", err)
	}
	pemBlock, err := ssh.MarshalPrivateKey(priv, "")
	if err != nil {
		t.Fatalf("marshal private key: %v", err)
	}
	keyPath := filepath.Join(t.TempDir(), "id_ed25519")
	if err := os.WriteFile(keyPath, pem.EncodeToMemory(pemBlock), 0o600); err != nil {
		t.Fatalf("write key: %v", err)
	}
	t.Setenv("GITHUB_SSH_KEY_PATH", keyPath)
	if auth := resolveGitAuth(); auth == nil {
		t.Fatalf("expected non-nil SSH auth for valid key")
	}
	t.Setenv("GITHUB_SSH_KEY_PATH", "")
	t.Setenv("GITHUB_PAT", "ghp_test_token")
	if auth := resolveGitAuth(); auth == nil {
		t.Fatalf("expected non-nil HTTP PAT auth")
	}
	t.Setenv("GITHUB_PAT", "")

	// 5. filterUnifiedDiffByPath & readFileAtCommit
	samplePatch := "diff --git a/src/a.md b/src/a.md\n--- a/src/a.md\n+++ b/src/a.md\n@@ -1 +1 @@\n-old\n+new\n"
	if got := filterUnifiedDiffByPath(samplePatch, "src/nonexistent.md"); got != "" {
		t.Fatalf("expected empty patch when targetPath not matched, got %q", got)
	}
	if _, ok := readFileAtCommit(nil, "src/00-preface.md"); ok {
		t.Fatalf("expected readFileAtCommit(nil) to be false")
	}
	if got := detectAddedChapterSlugInCommit(nil, "00-preface"); got != "" {
		t.Fatalf("expected empty slug for nil commit")
	}

	// 6. extractReplacedByHint & extractBlameAuthor
	for _, msg := range []string{
		"no hint here",
		"[replaced-by: unclosed",
		"[replaced-by: ../invalid-slug]",
	} {
		if got := extractReplacedByHint(msg); got != "" {
			t.Fatalf("expected empty hint for %q, got %q", msg, got)
		}
	}
	if got := extractBlameAuthor(&git.Line{Author: "fallback@example.com"}); got != "fallback@example.com" {
		t.Fatalf("extractBlameAuthor fallback = %q", got)
	}
	if got := extractBlameAuthor(&git.Line{AuthorName: "Bob Rivera", Author: "bob@example.com"}); got != "Bob Rivera" {
		t.Fatalf("extractBlameAuthor AuthorName = %q", got)
	}

	// 7. Summary helpers edge cases (indented parent, multiple children, no trailing newline)
	indentedSummary := "# Summary\n  - [Parent](01-parent.md)\n    - [Child1](01a-child.md)\n\n- [Next](02-next.md)"
	if out, ok := insertChildSummaryEntry(indentedSummary, "- [Child2](01b-child.md)", "01-parent"); !ok || !strings.Contains(out, "    - [Child2](01b-child.md)") {
		t.Fatalf("unexpected insertChildSummaryEntry output: %q", out)
	}
	if _, ok := insertChildSummaryEntry(indentedSummary, "- [Orphan](99-orphan.md)", "nonexistent-parent"); ok {
		t.Fatalf("expected false when parent does not exist")
	}
	noNewlineSummary := "# Summary\n- [Preface](00-preface.md)"
	if out := insertPartSummaryEntry(noNewlineSummary, "- [New](01-new.md)", "Part IX"); !strings.Contains(out, "# Part IX") {
		t.Fatalf("expected new part appended, got %q", out)
	}
	if out := insertSummaryEntry(noNewlineSummary, "Appended", "02-appended", "", ""); !strings.Contains(out, "(02-appended.md)") {
		t.Fatalf("expected appended entry, got %q", out)
	}
	if out := rewriteSummaryForRename("# Summary", "unlisted-old", "unlisted-new", ""); !strings.Contains(out, "[unlisted-new](unlisted-new.md)") {
		t.Fatalf("expected fallback title=newSlug when newTitle is empty, got %q", out)
	}
}
