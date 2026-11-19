package engine_test

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/wjkoh/ghostbook/internal/engine"
)

// CT-01: TestMultiAgentConcurrentPatches_ZeroIndexCorruption
func TestMultiAgentConcurrentPatches_ZeroIndexCorruption(t *testing.T) {
	eng, st := setupTestEngine(t)
	ctx := context.Background()

	// Create a dedicated chapter with 15 distinct section markers so 15 concurrent
	// agent goroutines (5 Alice, 5 Carol, 5 Bob) can patch in parallel without text collision.
	var sb strings.Builder
	for i := 0; i < 15; i++ {
		sb.WriteString(fmt.Sprintf("## Concurrent Section %02d\n\nPlaceholder paragraph for agent slot %02d awaiting concurrent patch.\n\n", i, i))
	}

	_, err := eng.CreateChapter(ctx, &engine.CreateChapterInput{
		Slug:            "06-concurrent-multi-agent-stress",
		Title:           "Chapter 6: Multi-Agent Concurrency Verification",
		InitialMarkdown: sb.String(),
		CommitMessage:   "Initialize 15-slot concurrency test chapter",
	})
	if err != nil {
		t.Fatalf("create test chapter: %v", err)
	}

	authorIDs := []string{"alice", "carol", "bob"}
	var creds []string
	for _, id := range authorIDs {
		a, rErr := st.ResolveAuthorName(ctx, id)
		if rErr != nil {
			t.Fatalf("resolve %s: %v", id, rErr)
		}
		creds = append(creds, a.AuthIdentifier())
	}

	var wg sync.WaitGroup
	errCh := make(chan error, 30)

	for i := 0; i < 15; i++ {
		wg.Add(1)
		go func(slot int) {
			defer wg.Done()
			cred := creds[slot%len(creds)]
			author, err := st.Authenticate(ctx, cred)
			if err != nil {
				errCh <- fmt.Errorf("slot %d auth: %w", slot, err)
				return
			}
			aCtx := engine.WithAuthor(ctx, author)

			oldLine := fmt.Sprintf("Placeholder paragraph for agent slot %02d awaiting concurrent patch.", slot)
			newLine := fmt.Sprintf("Verified concurrent patch by %s (%s) in slot %02d.", author.DisplayName, author.GitEmail, slot)

			_, err = eng.PatchChapter(aCtx, &engine.PatchChapterInput{
				Slug:          "06-concurrent-multi-agent-stress",
				OldText:       oldLine,
				NewText:       newLine,
				CommitMessage: fmt.Sprintf("Agent slot %02d patch by %s", slot, author.ID),
			})
			if err != nil {
				errCh <- fmt.Errorf("slot %d patch: %w", slot, err)
				return
			}
		}(i)
	}

	wg.Wait()
	close(errCh)
	for e := range errCh {
		t.Fatalf("concurrent agent error: %v", e)
	}

	finalChap, err := eng.ReadChapter(ctx, &engine.ReadChapterInput{Slug: "06-concurrent-multi-agent-stress"})
	if err != nil {
		t.Fatalf("read final chapter: %v", err)
	}

	for i := 0; i < 15; i++ {
		expected := fmt.Sprintf("in slot %02d.", i)
		if !strings.Contains(finalChap.ContentMarkdown, expected) {
			t.Fatalf("lost write for slot %02d in final chapter content", i)
		}
	}

	changes, err := eng.ListRecentChanges(ctx, &engine.ListRecentChangesInput{
		Slug:  "06-concurrent-multi-agent-stress",
		Limit: 25,
	})
	if err != nil {
		t.Fatalf("list recent changes: %v", err)
	}
	// 1 creation commit + 15 patch commits = 16 commits
	if len(changes.Commits) != 16 {
		t.Fatalf("expected 16 linear Git commits for chapter, got %d", len(changes.Commits))
	}
}

// CT-02: TestConcurrentReadersAndSearchersDuringBuild
func TestConcurrentReadersAndSearchersDuringBuild(t *testing.T) {
	eng, _ := setupTestEngine(t)
	ctx := context.Background()

	var wg sync.WaitGroup
	errCh := make(chan error, 100)

	// 50 concurrent reader + FTS5 searcher goroutines
	for r := 0; r < 50; r++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			for iter := 0; iter < 5; iter++ {
				snap := eng.CurrentSnapshot()
				if snap == nil || len(snap.Chapters) == 0 {
					errCh <- fmt.Errorf("reader %d saw nil or empty snapshot", id)
					return
				}
				if _, err := eng.ReadChapter(ctx, &engine.ReadChapterInput{Slug: "02-agent-workflow"}); err != nil {
					errCh <- fmt.Errorf("reader %d ReadChapter: %w", id, err)
					return
				}
				if _, err := eng.SearchBook(ctx, &engine.SearchBookInput{Query: "Ghostbook", Limit: 5}); err != nil {
					errCh <- fmt.Errorf("reader %d SearchBook: %w", id, err)
					return
				}
			}
		}(r)
	}

	// Concurrent writer goroutine performing sequential patches while 50 readers hammer the engine
	wg.Add(1)
	go func() {
		defer wg.Done()
		curTarget := "# Chapter 2: Agent Authoring Workflow"
		for w := 0; w < 5; w++ {
			nextTarget := fmt.Sprintf("# Chapter 2: Agent Authoring Workflow (Iter %d)", w+1)
			_, err := eng.PatchChapter(ctx, &engine.PatchChapterInput{
				Slug:          "02-agent-workflow",
				OldText:       curTarget,
				NewText:       nextTarget,
				CommitMessage: fmt.Sprintf("Concurrent writer iteration %d", w+1),
			})
			if err != nil {
				errCh <- fmt.Errorf("writer iter %d: %w", w, err)
				return
			}
			curTarget = nextTarget
		}
	}()

	wg.Wait()
	close(errCh)
	for e := range errCh {
		t.Fatalf("CT-02 error: %v", e)
	}
}
