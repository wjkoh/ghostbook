package engine

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/wjkoh/ghostbook/internal/renderer"
	"github.com/wjkoh/ghostbook/internal/store"
)

// DemoSeedResult summarizes the seeded demo corpus, co-authors, and notable lineage routes
// created by SeedDemoBook for interactive exploration and automated browser QA.
type DemoSeedResult struct {
	Seeded              bool
	Authors             []store.Author
	HeadRevision        int32
	HeadSHA             string
	OutdatedChapterSlug string
	OutdatedRevision    string
	DiagramChapterSlug  string
	DiagramChapterSHA   string
	RenamedFromSlug     string
	RenamedToSlug       string
	RetiredDraftSlug    string
	RetiredDraftSHA     string
}

// SeedDemoBook populates a fresh Ghostbook engine and SQLite store with a realistic
// multi-author, multi-commit living book ("Atlas Systems Handbook") so users and automated
// browser checks can immediately explore every Ghostbook view without external API keys.
//
// If the repository already has more than 1 commit (for example when resuming a persisted
// --data-dir), SeedDemoBook preserves the existing history and returns Seeded=false.
func SeedDemoBook(ctx context.Context, eng *BookEngine) (*DemoSeedResult, error) {
	if eng == nil || eng.store == nil {
		return nil, fmt.Errorf("%w: nil engine or store", ErrInvalidArgument)
	}

	if err := eng.store.SyncConfiguredAuthors(ctx, store.DefaultDemoAuthors); err != nil {
		return nil, fmt.Errorf("sync demo authors: %w", err)
	}
	authors, err := eng.store.ListAuthors(ctx)
	if err != nil {
		return nil, fmt.Errorf("list demo authors: %w", err)
	}

	snap := eng.CurrentSnapshot()
	if snap != nil && snap.HeadRevisionNumber > 1 {
		eng.mu.Lock()
		_ = eng.rebuildSnapshotAndFTSLocked(ctx)
		eng.mu.Unlock()
		snap = eng.CurrentSnapshot()
		return &DemoSeedResult{
			Seeded:              false,
			Authors:             authors,
			HeadRevision:        snap.HeadRevisionNumber,
			HeadSHA:             snap.HeadCommitSHA,
			OutdatedChapterSlug: "01-system-architecture",
			OutdatedRevision:    "r4",
			DiagramChapterSlug:  "01-system-architecture",
			DiagramChapterSHA:   chapterSHAFromSnap(snap, "01-system-architecture"),
			RenamedFromSlug:     "03-editorial-lineage",
			RenamedToSlug:       "03-lineage-and-audit",
			RetiredDraftSlug:    "draft-quorum-tuning",
		}, nil
	}

	alice, err := eng.store.ResolveAuthorName(ctx, "alice")
	if err != nil {
		return nil, fmt.Errorf("resolve alice: %w", err)
	}
	bob, err := eng.store.ResolveAuthorName(ctx, "bob")
	if err != nil {
		return nil, fmt.Errorf("resolve bob: %w", err)
	}
	carol, err := eng.store.ResolveAuthorName(ctx, "carol")
	if err != nil {
		return nil, fmt.Errorf("resolve carol: %w", err)
	}

	ctxAlice := WithAuthor(ctx, alice)
	ctxBob := WithAuthor(ctx, bob)
	ctxCarol := WithAuthor(ctx, carol)

	now := time.Now().UTC().Truncate(time.Second)
	commitTime := now.Add(-96 * time.Hour)
	eng.mu.Lock()
	eng.commitClock = func() time.Time { return commitTime }
	eng.mu.Unlock()
	defer func() {
		eng.mu.Lock()
		eng.commitClock = nil
		eng.mu.Unlock()
	}()
	setCommitOffset := func(offset time.Duration) {
		commitTime = now.Add(offset)
	}

	headRev := func() int32 {
		if cur := eng.CurrentSnapshot(); cur != nil {
			return cur.HeadRevisionNumber
		}
		return 1
	}

	// r2: Configure src/book.json with book-level title, kicker, lint rules, and timeline guidance
	setCommitOffset(-95 * time.Hour)
	cfgOut, err := eng.PatchChapter(ctxAlice, &PatchChapterInput{
		Slug:          "BOOK_CONFIG",
		OldText:       "*",
		NewText:       demoBookConfigJSON,
		CommitMessage: "Configure book title, kicker, and editorial lint rules in book.json",
	})
	if err != nil {
		return nil, fmt.Errorf("demo r2 book.json: %w", err)
	}
	r2 := headRev()

	// r3: Expand Introduction with overview metrics table and cross-chapter links
	setCommitOffset(-92 * time.Hour)
	introOut, err := eng.PatchChapter(ctxAlice, &PatchChapterInput{
		Slug:          "00-intro",
		OldText:       "*",
		NewText:       demoIntroMarkdown,
		CommitMessage: "Expand handbook introduction with SLO matrix and navigation guide",
	})
	if err != nil {
		return nil, fmt.Errorf("demo r3 00-intro: %w", err)
	}
	r3 := headRev()

	// r4: Author Chapter 1 initial deep technical architecture with Mermaid diagrams and table
	setCommitOffset(-52 * time.Hour)
	ch1Out1, err := eng.PatchChapter(ctxAlice, &PatchChapterInput{
		Slug:          "01-system-architecture",
		OldText:       "*",
		NewText:       demoChapter1V1Markdown,
		CommitMessage: "Draft Atlas storage topology, replication sequence, and capacity targets",
	})
	if err != nil {
		return nil, fmt.Errorf("demo r4 ch1 v1: %w", err)
	}
	r4 := headRev()

	// r5: Bob refines Chapter 1 capacity SLOs and adds product rationale (creates multi-author Git blame + rich diff from r4)
	setCommitOffset(-47 * time.Hour)
	ch1Out2, err := eng.PatchChapter(ctxBob, &PatchChapterInput{
		Slug: "01-system-architecture",
		Patches: []ChapterPatchHunk{
			{
				OldText: "| Hot NVMe | 1.8 ms | 9.5 ms | 3x synchronous quorum | $0.24 / GB-mo |",
				NewText: "| Hot NVMe | 1.4 ms | 7.2 ms | 3x synchronous quorum | $0.22 / GB-mo |",
			},
			{
				OldText: "All tiers run inside a single static Go binary with zero external coordinators.",
				NewText: "All tiers run inside a single static Go binary with zero external coordinators. Product teams ship regional rollouts without separate Zookeeper or etcd fleets, cutting baseline infrastructure overhead by 62%.",
			},
		},
		CommitMessage: "Tighten Hot NVMe P99 SLO to 7.2 ms and document regional cost savings",
	})
	if err != nil {
		return nil, fmt.Errorf("demo r5 ch1 v2: %w", err)
	}
	r5 := headRev()

	// r6: Carol expands Chapter 2 with KaTeX display & inline math, code blocks, and benchmark table
	setCommitOffset(-45 * time.Hour)
	ch2Out1, err := eng.PatchChapter(ctxCarol, &PatchChapterInput{
		Slug:          "02-agent-workflow",
		OldText:       "*",
		NewText:       demoChapter2V1Markdown,
		CommitMessage: "Add BM25 ranking formulas, optimistic lock protocol, and benchmark table",
	})
	if err != nil {
		return nil, fmt.Errorf("demo r6 ch2: %w", err)
	}
	r6 := headRev()

	// r7: Bob adds nested child chapter under Chapter 1 to demonstrate sidebar hierarchy (depth-1)
	setCommitOffset(-42 * time.Hour)
	subOut, err := eng.CreateChapter(ctxBob, &CreateChapterInput{
		Slug:            "01a-failover-runbook",
		Title:           "Section 1A: Regional Failover Runbook",
		ParentSlug:      "01-system-architecture",
		InitialMarkdown: demoFailoverSubchapterMarkdown,
		CommitMessage:   "Add regional failover runbook as nested child of Chapter 1",
	})
	if err != nil {
		return nil, fmt.Errorf("demo r7 01a-failover-runbook: %w", err)
	}
	r7 := headRev()

	// r8: Carol stages a draft sub-chapter under Chapter 2 for staged draft-and-swap review
	setCommitOffset(-20 * time.Hour)
	draftOut, err := eng.CreateChapter(ctxCarol, &CreateChapterInput{
		Slug:            "draft-quorum-tuning",
		Title:           "Draft: Quorum Read Tuning",
		ParentSlug:      "02-agent-workflow",
		InitialMarkdown: demoDraftQuorumMarkdown,
		CommitMessage:   "Stage draft quorum read tuning analysis for co-author review",
	})
	if err != nil {
		return nil, fmt.Errorf("demo r8 draft-quorum-tuning: %w", err)
	}
	r8 := headRev()

	// r9: Alice merges quorum tuning findings into Chapter 2
	setCommitOffset(-17 * time.Hour)
	ch2Out2, err := eng.PatchChapter(ctxAlice, &PatchChapterInput{
		Slug:          "02-agent-workflow",
		OldText:       "> **[Comment · Carol Park · 2026-11-19]:** Confirm whether follower reads should require bounded staleness under 250 ms before promoting the draft.",
		NewText:       "Bounded-staleness follower reads (max skew 200 ms) cut cross-region P99 read latency by 41% without violating monotonic session reads.\n\n> **[Comment · Carol Park · 2026-11-19]:** Verify whether read-repair throttling at 15 MB/s is conservative enough during cold replica rebuilds.",
		CommitMessage: "Merge bounded-staleness follower read findings into Chapter 2",
	})
	if err != nil {
		return nil, fmt.Errorf("demo r9 merge draft: %w", err)
	}
	r9 := headRev()

	// r10: Alice retires the merged draft with replacement lineage pointing to 02-agent-workflow
	setCommitOffset(-16 * time.Hour)
	delOut, err := eng.DeleteChapter(ctxAlice, &DeleteChapterInput{
		Slug:           "draft-quorum-tuning",
		ReplacedBySlug: "02-agent-workflow",
		Reason:         "Retire merged quorum tuning draft after folding results into Chapter 2",
	})
	if err != nil {
		return nil, fmt.Errorf("demo r10 delete draft: %w", err)
	}
	r10 := headRev()

	// r11: Bob renames Chapter 3 slug and title to demonstrate permanent slug redirect & lineage
	setCommitOffset(-3 * time.Hour)
	renOut, err := eng.RenameChapter(ctxBob, &RenameChapterInput{
		OldSlug:       "03-editorial-lineage",
		NewSlug:       "03-lineage-and-audit",
		NewTitle:      "Chapter 3: Lineage & Audit Trail",
		CommitMessage: "Rename Chapter 3 to Lineage & Audit Trail for clarity",
	})
	if err != nil {
		return nil, fmt.Errorf("demo r11 rename ch3: %w", err)
	}
	r11 := headRev()

	// r12: Carol enriches Chapter 4 deployment checklist with Caddy snippet and open TODO
	setCommitOffset(-90 * time.Minute)
	ch4Out, err := eng.PatchChapter(ctxCarol, &PatchChapterInput{
		Slug:          "04-deployment-ops",
		OldText:       "*",
		NewText:       demoChapter4Markdown,
		CommitMessage: "Document loopback Caddy deployment, memory budget, and upgrade checklist",
	})
	if err != nil {
		return nil, fmt.Errorf("demo r12 ch4: %w", err)
	}
	r12 := headRev()

	// Populate realistic staggered Editorial Timeline milestones in SQLite (newest-first)
	milestones := []store.TimelineEntryRecord{
		{
			StartRevision:     r11,
			EndRevision:       r12,
			StartCommitSHA:    renOut.NewRevisionSHA,
			EndCommitSHA:      ch4Out.NewRevisionSHA,
			StartTimestampISO: now.Add(-3 * time.Hour).Format(time.RFC3339),
			TimestampISO:      now.Add(-90 * time.Minute).Format(time.RFC3339),
			CommitCount:       2,
			Revisions:         []int32{r11, r12},
			Title:             "Lineage Rename & Single-VM Ops Playbook",
			Summary:           "Renamed Chapter 3 to Lineage & Audit Trail with automatic cross-link rewrites, and added a complete Caddy reverse-proxy and memory budget checklist in Chapter 4.",
			ChapterSlugs:      []string{"03-lineage-and-audit", "04-deployment-ops"},
			Authors:           []string{"Bob Rivera", "Carol Park"},
		},
		{
			StartRevision:     r8,
			EndRevision:       r10,
			StartCommitSHA:    draftOut.NewRevisionSHA,
			EndCommitSHA:      delOut.NewRevisionSHA,
			StartTimestampISO: now.Add(-20 * time.Hour).Format(time.RFC3339),
			TimestampISO:      now.Add(-16 * time.Hour).Format(time.RFC3339),
			CommitCount:       3,
			Revisions:         []int32{r8, r9, r10},
			Title:             "Quorum Read Tuning Merged & Draft Retired",
			Summary:           "Staged bounded-staleness follower read benchmarks in a review draft, folded the 41% P99 latency reduction into Chapter 2, and retired the draft while preserving its archived permalink.",
			ChapterSlugs:      []string{"02-agent-workflow"},
			Authors:           []string{"Carol Park", "Alice Chen"},
		},
		{
			StartRevision:     r4,
			EndRevision:       r7,
			StartCommitSHA:    ch1Out1.NewRevisionSHA,
			EndCommitSHA:      subOut.NewRevisionSHA,
			StartTimestampISO: now.Add(-52 * time.Hour).Format(time.RFC3339),
			TimestampISO:      now.Add(-42 * time.Hour).Format(time.RFC3339),
			CommitCount:       4,
			Revisions:         []int32{r4, r5, r6, r7},
			Title:             "Storage Topology, BM25 Ranking & Failover Runbook",
			Summary:           "Published core storage architecture diagrams, tightened Hot NVMe P99 targets to 7.2 ms, formalized BM25 ranking equations, and added the nested Regional Failover Runbook.",
			ChapterSlugs:      []string{"01-system-architecture", "01a-failover-runbook", "02-agent-workflow"},
			Authors:           []string{"Alice Chen", "Bob Rivera", "Carol Park"},
		},
		{
			StartRevision:     r2,
			EndRevision:       r3,
			StartCommitSHA:    cfgOut.NewRevisionSHA,
			EndCommitSHA:      introOut.NewRevisionSHA,
			StartTimestampISO: now.Add(-96 * time.Hour).Format(time.RFC3339),
			TimestampISO:      now.Add(-92 * time.Hour).Format(time.RFC3339),
			CommitCount:       2,
			Revisions:         []int32{r2, r3},
			Title:             "Book Governance Config & Handbook Charter",
			Summary:           "Initialized repository-level book.json editorial rules and expanded the introduction with service-level objectives and cross-chapter navigation.",
			ChapterSlugs:      []string{"00-intro"},
			Authors:           []string{"Alice Chen"},
		},
	}

	if _, err := eng.store.AppendEditorialTimelineEntries(ctx, milestones, store.TimelineStateRecord{
		LastHeadSHA:      ch4Out.NewRevisionSHA,
		LastHeadRevision: r12,
		ModelName:        "demo-seed",
		UpdatedAtISO:     now.Add(-90 * time.Minute).Format(store.TimestampMillisLayout),
	}); err != nil {
		return nil, fmt.Errorf("seed demo timeline: %w", err)
	}

	// Populate realistic MCP and ConnectRPC activity audit entries (including a read + optimistic lock conflict)
	auditSeed := []store.AuditEntry{
		{
			TimestampISO: now.Add(-96 * time.Hour).Format(store.TimestampMillisLayout),
			AuthorID:     alice.ID, Protocol: "mcp", UserAgent: "Claude/2.1 (MCP)",
			ToolName: "get_table_of_contents", Status: "ok", DurationMs: 6,
		},
		{
			TimestampISO: now.Add(-95 * time.Hour).Format(store.TimestampMillisLayout),
			AuthorID:     alice.ID, Protocol: "mcp", UserAgent: "Claude/2.1 (MCP)",
			ToolName: "patch_chapter", ChapterSlug: "BOOK_CONFIG", GitCommitSHA: cfgOut.NewRevisionSHA, Status: "ok", DurationMs: 42,
		},
		{
			TimestampISO: now.Add(-92 * time.Hour).Format(store.TimestampMillisLayout),
			AuthorID:     alice.ID, Protocol: "mcp", UserAgent: "Claude/2.1 (MCP)",
			ToolName: "patch_chapter", ChapterSlug: "00-intro", GitCommitSHA: introOut.NewRevisionSHA, Status: "ok", DurationMs: 58,
		},
		{
			TimestampISO: now.Add(-52 * time.Hour).Format(store.TimestampMillisLayout),
			AuthorID:     alice.ID, Protocol: "mcp", UserAgent: "Cursor/0.49 (MCP)",
			ToolName: "patch_chapter", ChapterSlug: "01-system-architecture", GitCommitSHA: ch1Out1.NewRevisionSHA, Status: "ok", DurationMs: 74,
		},
		{
			TimestampISO: now.Add(-48 * time.Hour).Format(store.TimestampMillisLayout),
			AuthorID:     bob.ID, Protocol: "mcp", UserAgent: "ChatGPT-MCP/1.2",
			ToolName: "patch_chapter", ChapterSlug: "01-system-architecture", Status: "conflict",
			ErrorMessage: "optimistic lock conflict: expected r1 but chapter is at r4", DurationMs: 11,
		},
		{
			TimestampISO: now.Add(-47 * time.Hour).Format(store.TimestampMillisLayout),
			AuthorID:     bob.ID, Protocol: "mcp", UserAgent: "ChatGPT-MCP/1.2",
			ToolName: "patch_chapter", ChapterSlug: "01-system-architecture", GitCommitSHA: ch1Out2.NewRevisionSHA, Status: "ok", DurationMs: 63,
		},
		{
			TimestampISO: now.Add(-45 * time.Hour).Format(store.TimestampMillisLayout),
			AuthorID:     carol.ID, Protocol: "mcp", UserAgent: "Antigravity/1.0 (MCP)",
			ToolName: "patch_chapter", ChapterSlug: "02-agent-workflow", GitCommitSHA: ch2Out1.NewRevisionSHA, Status: "ok", DurationMs: 69,
		},
		{
			TimestampISO: now.Add(-42 * time.Hour).Format(store.TimestampMillisLayout),
			AuthorID:     bob.ID, Protocol: "mcp", UserAgent: "ChatGPT-MCP/1.2",
			ToolName: "create_chapter", ChapterSlug: "01a-failover-runbook", GitCommitSHA: subOut.NewRevisionSHA, Status: "ok", DurationMs: 81,
		},
		{
			TimestampISO: now.Add(-20 * time.Hour).Format(store.TimestampMillisLayout),
			AuthorID:     carol.ID, Protocol: "mcp", UserAgent: "Antigravity/1.0 (MCP)",
			ToolName: "create_chapter", ChapterSlug: "draft-quorum-tuning", GitCommitSHA: draftOut.NewRevisionSHA, Status: "ok", DurationMs: 54,
		},
		{
			TimestampISO: now.Add(-17 * time.Hour).Format(store.TimestampMillisLayout),
			AuthorID:     alice.ID, Protocol: "mcp", UserAgent: "Claude/2.1 (MCP)",
			ToolName: "patch_chapter", ChapterSlug: "02-agent-workflow", GitCommitSHA: ch2Out2.NewRevisionSHA, Status: "ok", DurationMs: 52,
		},
		{
			TimestampISO: now.Add(-16 * time.Hour).Format(store.TimestampMillisLayout),
			AuthorID:     alice.ID, Protocol: "mcp", UserAgent: "Claude/2.1 (MCP)",
			ToolName: "delete_chapter", ChapterSlug: "draft-quorum-tuning", GitCommitSHA: delOut.NewRevisionSHA, Status: "ok", DurationMs: 47,
		},
		{
			TimestampISO: now.Add(-3 * time.Hour).Format(store.TimestampMillisLayout),
			AuthorID:     bob.ID, Protocol: "connectrpc", UserAgent: "AtlasCLI/1.4 (ConnectRPC)",
			ToolName: "RenameChapter", ChapterSlug: "03-lineage-and-audit", GitCommitSHA: renOut.NewRevisionSHA, Status: "ok", DurationMs: 49,
		},
		{
			TimestampISO: now.Add(-90 * time.Minute).Format(store.TimestampMillisLayout),
			AuthorID:     carol.ID, Protocol: "mcp", UserAgent: "Cursor/0.49 (MCP)",
			ToolName: "patch_chapter", ChapterSlug: "04-deployment-ops", GitCommitSHA: ch4Out.NewRevisionSHA, Status: "ok", DurationMs: 61,
		},
		{
			TimestampISO: now.Add(-25 * time.Minute).Format(store.TimestampMillisLayout),
			AuthorID:     alice.ID, Protocol: "mcp", UserAgent: "Claude/2.1 (MCP)",
			ToolName: "search_book", Status: "ok", DurationMs: 8,
		},
	}
	for _, ae := range auditSeed {
		if err := eng.store.LogActivity(ctx, ae); err != nil {
			return nil, fmt.Errorf("seed demo audit log: %w", err)
		}
	}

	// Rebuild snapshot once more so timeline milestones, seeded activity, and authors are all live.
	eng.mu.Lock()
	rebuildErr := eng.rebuildSnapshotAndFTSLocked(ctx)
	eng.mu.Unlock()
	if rebuildErr != nil {
		return nil, fmt.Errorf("rebuild demo snapshot: %w", rebuildErr)
	}

	finalSnap := eng.CurrentSnapshot()
	return &DemoSeedResult{
		Seeded:              true,
		Authors:             authors,
		HeadRevision:        finalSnap.HeadRevisionNumber,
		HeadSHA:             finalSnap.HeadCommitSHA,
		OutdatedChapterSlug: "01-system-architecture",
		OutdatedRevision:    fmt.Sprintf("r%d", r4),
		DiagramChapterSlug:  "01-system-architecture",
		DiagramChapterSHA:   chapterSHAFromSnap(finalSnap, "01-system-architecture"),
		RenamedFromSlug:     "03-editorial-lineage",
		RenamedToSlug:       "03-lineage-and-audit",
		RetiredDraftSlug:    "draft-quorum-tuning",
		RetiredDraftSHA:     strings.TrimPrefix(delOut.ArchivedPermalinkURL, eng.baseURL+"/chapters/draft-quorum-tuning/rev/"),
	}, nil
}

func chapterSHAFromSnap(snap *renderer.RenderedBook, slug string) string {
	if snap == nil {
		return ""
	}
	if ch, ok := snap.BySlug[slug]; ok && ch != nil {
		return ch.LastCommitSHA
	}
	return snap.HeadCommitSHA
}

const demoBookConfigJSON = `{
  "title": "Atlas Systems Handbook",
  "kicker": "Interactive Demo",
  "hide_powered_by": false,
  "lint": {
    "max_heading_chars": 42,
    "forbid_em_dash": true,
    "banned_words": [
      "synergy",
      "utilize",
      "leverage"
    ]
  },
  "timeline": {
    "prompt_guidance": "Highlight architecture decisions, latency SLO adjustments, and open review questions."
  }
}
`

const demoIntroMarkdown = `# Introduction: Atlas Systems Handbook

Living Demo Edition · Co-Authors: Alice Chen, Bob Rivera, Carol Park

## 1. Purpose of This Demo Book

This local demo instance runs **Ghostbook** with a complete multi-author Git history, live SQLite FTS5 search, KaTeX math, self-hosted Mermaid diagrams, and pre-registered co-authors. Every chapter below simulates a real engineering and product handbook maintained exclusively by AI agents over remote MCP and ConnectRPC.

Use the left sidebar or jump directly into key areas:

- Read [Chapter 1: System Architecture](/chapters/01-system-architecture) for Mermaid diagrams, comparative tables, and multi-author Git blame.
- Explore [Chapter 2: Agent Authoring Workflow](/chapters/02-agent-workflow) for KaTeX equations, optimistic locking code snippets, and benchmark results.
- Inspect [Style Guide & Rules](/STYLE_GUIDE) to see repository-managed editorial rules and active book configuration.
- Open [Git Commit Log](/changelog) and [Agent Activity](/activity) to trace every commit and tool invocation.

## 2. Handbook Service Objectives

| Domain | Target Metric | Current Baseline | Owning Co-Author | Status |
| :--- | :--- | :--- | :--- | :--- |
| Write Path Latency | P99 <= 8.0 ms | 7.2 ms | Alice Chen | On Track |
| Cross-Region Read | P99 <= 35.0 ms | 28.4 ms | Carol Park | Verified |
| Cold Recovery Time | RTO <= 45 sec | 31 sec | Bob Rivera | Verified |
| Memory Footprint | <= 64 MB RSS | 42 MB RSS | Alice Chen | On Track |

> **[Comment · Alice Chen · 2026-11-18]:** Confirm whether we should publish quarterly p99.9 tail-latency percentiles in the public overview table.
`

const demoChapter1V1Markdown = `# Chapter 1: System Architecture

Living Demo Edition · Co-Authors: Alice Chen, Bob Rivera, Carol Park

## 1. Storage Engine & WAL Topology

Atlas stores canonical state in an append-only local Git repository paired with a single-writer SQLite WAL index. Every write commits atomically to disk before updating the in-memory read snapshot so readers never observe partial state.

` + "```mermaid" + `
flowchart LR
    Agent["AI Agent (MCP / RPC)"] --> Auth["Token Auth"]
    Auth --> Engine["Single-Writer Engine"]
    Engine --> Git["Embedded go-git"]
    Engine --> SQLite["SQLite WAL + FTS5"]
    Engine --> Snap["Atomic Read Snapshot"]
    Snap --> Reader["SSR Web Reader"]
` + "```" + `

## 2. Request Lifecycle & Replication

When a co-author's assistant submits a surgical patch, the engine verifies the optimistic revision lock, commits the updated Markdown file, refreshes full-text search, and triggers an asynchronous background sync.

` + "```mermaid" + `
sequenceDiagram
    participant Client as AI Client
    participant Server as Ghostbook Engine
    participant Git as Local Git Repo
    participant Remote as Git Remote
    Client->>Server: patch_chapter(slug, expected_sha, hunks)
    Server->>Git: Verify HEAD & commit attributed patch
    Git-->>Server: New commit SHA (rN)
    Server-->>Client: Return diff preview & live URL
    Server--)Remote: Debounced background push
` + "```" + `

## 3. Capacity & Latency Targets

| Storage Tier | Median P50 | Tail P99 | Durability Profile | Monthly Cost |
| :--- | :--- | :--- | :--- | :--- |
| Hot NVMe | 1.8 ms | 9.5 ms | 3x synchronous quorum | $0.24 / GB-mo |
| Warm SSD | 4.2 ms | 18.0 ms | 2x regional + WAL archive | $0.11 / GB-mo |
| Cold Object | 85.0 ms | 240.0 ms | Immutable tar snapshots | $0.004 / GB-mo |

All tiers run inside a single static Go binary with zero external coordinators.

## 4. Open Design Questions

See [Section 1A: Regional Failover Runbook](/chapters/01a-failover-runbook) for step-by-step promotion steps during an availability-zone outage.

> **[TODO · Alice Chen · 2026-11-19]:** Benchmark WAL checkpoint pause duration under 50 concurrent read requests on an e2-micro VM.
`

const demoChapter2V1Markdown = `# Chapter 2: Agent Authoring Workflow

Living Demo Edition · Co-Authors: Alice Chen, Bob Rivera, Carol Park

## 1. Ranking & Retrieval Model

Full-text discovery combines SQLite FTS5 BM25 ranking with exact 1-based line matching so agents find both prose concepts and literal callout markers like TODO or Comment.

Let \(f(q, D)\) denote term frequency in chapter \(D\) of length \(|D|\) with corpus mean \(\bar{L}\):

$$\text{BM25}(D, q) = \sum_{t \in q} \text{IDF}(t) \cdot \frac{f(t, D) \cdot (k_1 + 1)}{f(t, D) + k_1 \cdot \left(1 - b + b \cdot \frac{|D|}{\bar{L}}\right)}$$

For tail-latency budgeting across \(n\) replicas with per-replica success probability \(p\), the quorum availability bound is:

$$P(\text{quorum}) = \sum_{k=\lceil (n+1)/2 \rceil}^{n} \binom{n}{k} p^k (1 - p)^{n - k}$$

## 2. Optimistic Locking Protocol

Agents must pass the revision SHA or sequential number returned by read_chapter when submitting a patch. If another co-author's agent committed first, Ghostbook rejects the stale write cleanly instead of clobbering prose.

` + "```json" + `
{
  "tool": "patch_chapter",
  "arguments": {
    "slug": "01-system-architecture",
    "expected_revision_sha": "r4",
    "old_text": "Tail P99: 9.5 ms",
    "new_text": "Tail P99: 7.2 ms",
    "commit_message": "Tighten Hot NVMe P99 SLO to 7.2 ms"
  }
}
` + "```" + `

## 3. Evaluation Benchmarks

| Scenario | Concurrency | Median Latency | Conflict Retry Rate | Data Loss |
| :--- | :--- | :--- | :--- | :--- |
| Single-Chapter Patch | 4 agents | 19 ms | 12.5% (clean retry) | 0 |
| Multi-Chapter Batch | 8 agents | 27 ms | 2.1% (clean retry) | 0 |
| Full-Book FTS5 Query | 32 readers | 3.4 ms | 0.0% | 0 |

> **[Comment · Carol Park · 2026-11-19]:** Confirm whether follower reads should require bounded staleness under 250 ms before promoting the draft.
`

const demoFailoverSubchapterMarkdown = `# Section 1A: Regional Failover Runbook

Living Demo Edition · Co-Authors: Alice Chen, Bob Rivera, Carol Park

## 1. Trigger Conditions

Initiate regional failover when primary zone health checks fail for three consecutive 10-second probes or when write-ahead log replication lag exceeds 5 seconds.

## 2. Promotion Sequence

1. Freeze incoming write tokens at the reverse proxy while draining active in-flight commits.
2. Verify the standby replica has applied the latest Git commit SHA from the backup remote.
3. Flip DNS or tunnel origin to the promoted node and confirm /healthz reports the expected revision number.
4. Resume write traffic and validate the next periodic remote sync succeeds within 60 seconds.
`

const demoDraftQuorumMarkdown = `# Draft: Quorum Read Tuning

Staged Review Draft · Author: Carol Park

## 1. Experimental Summary

Testing bounded-staleness follower reads with a 200 ms clock-skew ceiling reduced cross-region P99 read latency by 41% during simulated burst workloads.
`

const demoChapter4Markdown = `# Chapter 4: Single-Binary Deployment

Living Demo Edition · Co-Authors: Alice Chen, Bob Rivera, Carol Park

## 1. Zero-Dependency Runtime

Ghostbook compiles with CGO_ENABLED=0 and runs behind Caddy with automatic Let's Encrypt TLS, loopback-only pprof diagnostics, and staged idle-window upgrades.

` + "```caddy" + `
book.example.com {
    handle /debug* {
        respond 404
    }
    handle /u/*/mcp* {
        reverse_proxy 127.0.0.1:8090 {
            flush_interval -1
        }
    }
    reverse_proxy 127.0.0.1:8090
}
` + "```" + `

## 2. Host Sizing & Memory Budget

| Workload Profile | Active Co-Authors | Chapters | Steady RSS | Recommended Host |
| :--- | :--- | :--- | :--- | :--- |
| Small Team | 2 to 6 | 15 to 40 | 32 MB | GCE Always Free e2-micro |
| Department Handbook | 10 to 25 | 50 to 150 | 48 MB | 1 vCPU / 1 GB RAM VPS |
| Enterprise Corpus | 25 to 60 | 200+ | 75 MB | 2 vCPU / 2 GB RAM VM |

## 3. Operational Verification Checklist

- Confirm Ghostbook binds strictly to 127.0.0.1:8090 with no public firewall port on 8090.
- Pin GitHub SSH host keys via GITHUB_KNOWN_HOSTS_PATH instead of disabling verification.
- Verify /healthz returns HTTP 200 and matches HEAD before promoting staged binaries.

> **[TODO · Carol Park · 2026-11-19]:** Add a systemd socket-activation benchmark comparing cold start versus staged binary swap.
`
