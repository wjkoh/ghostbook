package engine

import (
	"context"
	"log/slog"
	"time"
)

func (e *BookEngine) triggerSync() {
	select {
	case e.syncCh <- struct{}{}:
	default:
	}
}

// SyncWithRemote synchronizes the Git repository with the configured "origin" remote
// using bounded context timeouts and rebuilds the in-memory snapshot if new commits arrive.
func (e *BookEngine) SyncWithRemote(ctx context.Context) error {
	start := time.Now()
	e.syncMu.Lock()
	defer e.syncMu.Unlock()

	pulledNewCommits, err := e.pullAndPushOrigin(ctx)
	if err != nil {
		slog.Warn("git_remote_sync_failed", "error", err.Error(), "duration_ms", time.Since(start).Milliseconds())
		if ctx.Err() == nil {
			e.NotifyAlert(ctx, "git_remote_sync", "Git remote sync failed", err.Error())
		}
		return err
	}
	if pulledNewCommits {
		e.mu.Lock()
		slog.Info("git_remote_sync_pulled_commits", "duration_ms", time.Since(start).Milliseconds())
		err := e.rebuildSnapshotAndFTSLocked(context.WithoutCancel(ctx))
		e.mu.Unlock()
		if err != nil {
			return err
		}
		e.triggerBriefingRefresh()
	}
	return nil
}

func (e *BookEngine) StartPeriodicGitHubSync(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		interval = 60 * time.Second
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-e.syncCh:
			select {
			case <-ctx.Done():
				return
			case <-time.After(250 * time.Millisecond):
				select {
				case <-e.syncCh:
				default:
				}
				_ = e.SyncWithRemote(ctx)
			}
		case <-ticker.C:
			_ = e.SyncWithRemote(ctx)
		}
	}
}
