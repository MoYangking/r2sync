package syncer

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"time"

	"r2sync/internal/config"
	"r2sync/internal/r2"
	"r2sync/internal/state"
)

// RemoteFactory builds an R2 object store for the given configuration.
type RemoteFactory func(context.Context, config.Config) (r2.ObjectStore, error)

const (
	retryBaseDelay = 30 * time.Second
	retryMaxDelay  = 10 * time.Minute
	busyRetryDelay = 5 * time.Second
)

// Manager owns the background sync lifecycle: it validates remote access,
// runs the initial sync (with backoff retries), then keeps a scheduled sync
// loop running. Reconfigure applies new configuration without a process
// restart, so a deployment can start unconfigured and be completed from the
// web UI.
type Manager struct {
	Store   *state.Store
	Log     *slog.Logger
	Factory RunnerFactory

	mu       sync.Mutex
	baseCtx  context.Context
	cancel   context.CancelFunc
	readySig string
}

func NewManager(store *state.Store, log *slog.Logger, factory RunnerFactory) *Manager {
	if log == nil {
		log = slog.Default()
	}
	return &Manager{Store: store, Log: log, Factory: factory}
}

// Start begins managing syncs under ctx. If cfg is incomplete the manager
// stays idle until Reconfigure is called with a usable configuration.
func (m *Manager) Start(ctx context.Context, cfg config.Config) {
	m.mu.Lock()
	m.baseCtx = ctx
	m.mu.Unlock()
	m.Reconfigure(cfg)
}

// MarkInitialSynced records that an initial sync already completed for cfg,
// so the next loop start goes straight to scheduled syncs. Used by `run`
// mode, which gates the child command on a synchronous initial sync.
func (m *Manager) MarkInitialSynced(cfg config.Config) {
	m.mu.Lock()
	m.readySig = connectionSignature(cfg)
	m.mu.Unlock()
}

// Reconfigure stops the current loop and starts a new one with cfg. When the
// remote connection identity changed (method, bucket, repo, token, prefix,
// base dir) the new loop re-runs the initial sync; otherwise it resumes
// scheduling directly, so an interval or targets edit does not re-trigger
// remote-wins semantics on files that are already being synced.
func (m *Manager) Reconfigure(cfg config.Config) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.baseCtx == nil {
		return
	}
	if m.cancel != nil {
		m.cancel()
		m.cancel = nil
	}
	if !cfg.HasSyncConfig() {
		m.Log.Warn("sync config is incomplete; waiting for credentials via management UI", "method", cfg.SyncMethod)
		return
	}
	ctx, cancel := context.WithCancel(m.baseCtx)
	m.cancel = cancel
	go m.loop(ctx, cfg)
}

// Stop cancels the current loop, if any.
func (m *Manager) Stop() {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.cancel != nil {
		m.cancel()
		m.cancel = nil
	}
}

func (m *Manager) loop(ctx context.Context, cfg config.Config) {
	sig := connectionSignature(cfg)
	delay := retryBaseDelay
	for {
		if ctx.Err() != nil {
			return
		}
		runner, err := m.Factory(ctx, cfg)
		if err == nil {
			if m.initialDone(sig) {
				m.runScheduled(ctx, cfg, runner)
				return
			}
			_, err = runner.Sync(ctx, ModeInitial)
			if err == nil {
				m.setInitialDone(sig)
				m.Log.Info("initial sync complete; scheduled sync active", "method", cfg.SyncMethod, "interval", cfg.SyncInterval.String())
				m.runScheduled(ctx, cfg, runner)
				return
			}
			if errors.Is(err, ErrSyncBusy) {
				if !sleepCtx(ctx, busyRetryDelay) {
					return
				}
				continue
			}
		}
		if ctx.Err() != nil {
			return
		}
		m.Log.Error("sync startup failed; will retry", "error", err, "retry_in", delay.String())
		if !sleepCtx(ctx, delay) {
			return
		}
		delay = min(delay*2, retryMaxDelay)
	}
}

func (m *Manager) runScheduled(ctx context.Context, cfg config.Config, runner Runner) {
	interval := cfg.SyncInterval
	if interval <= 0 {
		interval = config.DefaultSyncInterval
	}
	timer := time.NewTimer(interval)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		}
		if _, err := runner.Sync(ctx, ModeScheduled); err != nil {
			if errors.Is(err, ErrSyncBusy) {
				m.Log.Info("scheduled sync skipped; another sync is running")
			} else {
				m.Log.Error("scheduled sync failed", "error", err)
			}
		}
		timer.Reset(interval)
	}
}

func (m *Manager) initialDone(sig string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.readySig == sig
}

func (m *Manager) setInitialDone(sig string) {
	m.mu.Lock()
	m.readySig = sig
	m.mu.Unlock()
}

// connectionSignature identifies the remote namespace a configuration syncs
// against. Targets and interval are deliberately excluded: they do not change
// which remote objects existing records refer to.
func connectionSignature(cfg config.Config) string {
	h := sha256.Sum256([]byte(strings.Join([]string{
		cfg.SyncMethod,
		cfg.BucketName, cfg.AccountID, cfg.CloudflareToken, cfg.ObjectPrefix,
		cfg.GitHubRepo, cfg.GitHubToken, cfg.GitHubBranch, cfg.RepoDir,
		cfg.BaseDir,
	}, "\x00")))
	return hex.EncodeToString(h[:])
}

func sleepCtx(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}
