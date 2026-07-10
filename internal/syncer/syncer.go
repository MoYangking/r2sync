package syncer

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"

	"r2sync/internal/config"
	"r2sync/internal/cost"
	"r2sync/internal/fsutil"
	"r2sync/internal/r2"
	"r2sync/internal/state"
)

type Mode string

const (
	ModeInitial   Mode = "initial"
	ModeScheduled Mode = "scheduled"
	ModeManual    Mode = "manual"
	ModeVerify    Mode = "verify"
)

// ErrSyncBusy is returned when a sync is requested while another sync is
// already running in this process.
var ErrSyncBusy = errors.New("another sync is already running")

// syncGate serializes syncs across all Runner instances in the process, so a
// manual sync cannot interleave with a scheduled one and double-spend
// requests on the same targets.
var syncGate = make(chan struct{}, 1)

// acquireSyncGate reserves the process-wide sync slot. It returns a release
// function, or ErrSyncBusy when another sync currently holds the slot.
func acquireSyncGate() (func(), error) {
	select {
	case syncGate <- struct{}{}:
		return func() { <-syncGate }, nil
	default:
		return nil, ErrSyncBusy
	}
}

type Syncer struct {
	Config config.Config
	Store  *state.Store
	Remote r2.ObjectStore
}

type Result struct {
	Uploaded int `json:"uploaded"`
	Restored int `json:"restored"`
	Skipped  int `json:"skipped"`
	Missing  int `json:"missing"`
	Errors   int `json:"errors"`
}

func New(cfg config.Config, st *state.Store, remote r2.ObjectStore) *Syncer {
	return &Syncer{Config: cfg, Store: st, Remote: remote}
}

func (s *Syncer) InitialSync(ctx context.Context) (Result, error) {
	return s.Sync(ctx, ModeInitial)
}

func (s *Syncer) ManualSync(ctx context.Context) (Result, error) {
	return s.Sync(ctx, ModeManual)
}

func (s *Syncer) ScheduledSync(ctx context.Context) (Result, error) {
	return s.Sync(ctx, ModeScheduled)
}

func (s *Syncer) Verify(ctx context.Context) (Result, error) {
	old := s.Config.StrictVerify
	s.Config.StrictVerify = true
	defer func() { s.Config.StrictVerify = old }()
	return s.Sync(ctx, ModeVerify)
}

func (s *Syncer) Sync(ctx context.Context, mode Mode) (Result, error) {
	if s.Remote == nil {
		return Result{}, config.ErrMissingCloudflareConfig
	}
	if len(s.Config.Targets) == 0 {
		return Result{}, fmt.Errorf("no sync targets configured")
	}
	release, err := acquireSyncGate()
	if err != nil {
		return Result{}, err
	}
	defer release()
	start := time.Now().UTC()
	// Ready means "initial sync completed"; periodic runs must not flip the
	// startup gate back to not-ready while (or after) they run.
	wasReady := s.Store.Snapshot().Status.Ready
	runningReady := wasReady && mode != ModeInitial
	_ = s.setStatus(state.Status{Stage: string(mode), Progress: 0, Ready: runningReady})
	var result Result
	var firstErr error
	for i, target := range s.Config.Targets {
		select {
		case <-ctx.Done():
			return result, ctx.Err()
		default:
		}
		progress := int(float64(i) / float64(len(s.Config.Targets)) * 100)
		_ = s.setStatus(state.Status{Stage: string(mode), Progress: progress, CurrentTarget: target, Ready: runningReady})
		action, err := s.syncTarget(ctx, mode, target)
		if err != nil {
			result.Errors++
			_ = s.Store.AddEvent("error", err.Error(), target)
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		switch action {
		case "uploaded":
			result.Uploaded++
		case "restored":
			result.Restored++
		case "missing":
			result.Missing++
		default:
			result.Skipped++
		}
	}
	ready := firstErr == nil
	if mode != ModeInitial {
		ready = wasReady
	}
	status := state.Status{
		Stage:              "complete",
		Progress:           100,
		Ready:              ready,
		LastSuccessfulSync: time.Now().UTC(),
	}
	switch mode {
	case ModeInitial:
		status.LastInitialSyncAt = start
		status.NextScheduledAt = s.nextScheduledAt(time.Now().UTC())
	case ModeScheduled:
		status.LastScheduledAt = start
		status.NextScheduledAt = s.nextScheduledAt(time.Now().UTC())
	case ModeManual:
		status.LastManualSyncAt = start
	}
	if firstErr != nil {
		status.Stage = "error"
		status.Ready = ready
		status.LastError = firstErr.Error()
		status.LastSuccessfulSync = time.Time{}
		_ = s.setStatus(status)
		return result, firstErr
	}
	_ = s.setStatus(status)
	_ = s.Store.AddEvent("info", fmt.Sprintf("sync complete: uploaded=%d restored=%d skipped=%d missing=%d", result.Uploaded, result.Restored, result.Skipped, result.Missing), "")
	return result, nil
}

func (s *Syncer) DeleteRemote(ctx context.Context, target string, confirm string) error {
	if confirm != "DELETE" {
		return fmt.Errorf("remote delete requires confirmation")
	}
	plan, err := fsutil.PlanTarget(s.Config.BaseDir, s.Config.ObjectPrefix, target)
	if err != nil {
		return err
	}
	if err := s.Remote.Delete(ctx, plan.ObjectKey); err != nil {
		return err
	}
	return s.Store.Update(func(d *state.Data) error {
		cost.RegisterFree(d, 1)
		rec := d.Targets[plan.RelPath]
		rec.Remote = state.RemoteMetadata{}
		rec.LastAction = "remote_deleted"
		rec.UpdatedAt = time.Now().UTC()
		d.Targets[plan.RelPath] = rec
		return nil
	})
}

func (s *Syncer) syncTarget(ctx context.Context, mode Mode, target string) (string, error) {
	plan, err := fsutil.PlanTarget(s.Config.BaseDir, s.Config.ObjectPrefix, target)
	if err != nil {
		return "", err
	}
	local, err := fsutil.Stat(plan.AbsPath)
	if err != nil {
		return "", err
	}

	var remote r2.Object
	var remoteExists bool
	record := s.Store.Snapshot().Targets[plan.RelPath]
	// A target with no record has never been synced by this instance (for
	// example it was just added through the UI). It must go through initial
	// semantics: check the remote first instead of blindly uploading over an
	// existing object or leaving a restorable object unrestored.
	firstSeen := record.UpdatedAt.IsZero()
	needHead := mode == ModeInitial || mode == ModeVerify || s.Config.StrictVerify || firstSeen
	if needHead || record.Remote.Exists {
		guard := cost.Guard{Config: s.Config, State: s.Store.Snapshot()}
		if decision := guard.CheckClassB(1); !decision.Allowed {
			return "", fmt.Errorf("cost guard blocked remote metadata check: %s", decision.Reason)
		}
		remote, remoteExists, err = s.Remote.Head(ctx, plan.ObjectKey)
		_ = s.Store.Update(func(d *state.Data) error {
			cost.RegisterClassB(d, 1)
			return nil
		})
		if err != nil {
			return "", err
		}
	} else {
		remoteExists = record.Remote.Exists
		remote = r2.Object{
			Key:    plan.ObjectKey,
			Exists: record.Remote.Exists,
			Size:   record.Remote.Size,
			SHA256: record.Remote.SHA256,
			ETag:   record.Remote.ETag,
		}
	}

	if mode == ModeInitial || firstSeen {
		return s.initialTarget(ctx, plan, local, remote, remoteExists)
	}
	return s.periodicTarget(ctx, plan, local, remote, remoteExists, record)
}

func (s *Syncer) initialTarget(ctx context.Context, plan fsutil.TargetPlan, local fsutil.Metadata, remote r2.Object, remoteExists bool) (string, error) {
	switch {
	case remoteExists && !local.Exists:
		return "restored", s.restore(ctx, plan, remote)
	case remoteExists && local.Exists:
		localHash, err := fsutil.SHA256File(plan.AbsPath)
		if err != nil {
			return "", err
		}
		if remote.SHA256 != "" && remote.SHA256 == localHash {
			local.SHA256 = localHash
			return "skipped", s.record(plan, local, remote, "verified")
		}
		if _, err := fsutil.CopyToQuarantine(plan.AbsPath, s.Config.StateDir, plan.RelPath); err != nil {
			return "", err
		}
		return "restored", s.restore(ctx, plan, remote)
	case !remoteExists && local.Exists:
		return "uploaded", s.upload(ctx, plan, local)
	default:
		if err := fsutil.EnsureParent(plan.AbsPath); err != nil {
			return "", err
		}
		return "missing", s.record(plan, local, remote, "missing")
	}
}

func (s *Syncer) periodicTarget(ctx context.Context, plan fsutil.TargetPlan, local fsutil.Metadata, remote r2.Object, remoteExists bool, record state.TargetRecord) (string, error) {
	if !local.Exists {
		if remoteExists {
			return "restored", s.restore(ctx, plan, remote)
		}
		return "missing", s.record(plan, local, remote, "missing")
	}
	if !s.Config.StrictVerify && record.Local.Exists && local.Size == record.Local.Size && local.ModTime.Equal(record.Local.ModTime) {
		return "skipped", nil
	}
	hash, err := fsutil.SHA256File(plan.AbsPath)
	if err != nil {
		return "", err
	}
	local.SHA256 = hash
	if record.Local.SHA256 == hash && !s.Config.StrictVerify {
		return "skipped", s.record(plan, local, remote, "metadata_refreshed")
	}
	if remoteExists && remote.SHA256 != "" && remote.SHA256 == hash {
		return "skipped", s.record(plan, local, remote, "verified")
	}
	return "uploaded", s.upload(ctx, plan, local)
}

func (s *Syncer) restore(ctx context.Context, plan fsutil.TargetPlan, remote r2.Object) error {
	guard := cost.Guard{Config: s.Config, State: s.Store.Snapshot()}
	if decision := guard.CheckClassB(1); !decision.Allowed {
		return fmt.Errorf("cost guard blocked restore: %s", decision.Reason)
	}
	if err := fsutil.EnsureParent(plan.AbsPath); err != nil {
		return err
	}
	tmp := plan.AbsPath + ".download"
	f, err := os.Create(tmp)
	if err != nil {
		return fmt.Errorf("create download temp file: %w", err)
	}
	obj, downloadErr := s.Remote.Download(ctx, plan.ObjectKey, f)
	closeErr := f.Close()
	if downloadErr != nil {
		_ = os.Remove(tmp)
		return downloadErr
	}
	if closeErr != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("close download temp file: %w", closeErr)
	}
	if err := os.Rename(tmp, plan.AbsPath); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("replace local file from R2: %w", err)
	}
	local, err := fsutil.Stat(plan.AbsPath)
	if err != nil {
		return err
	}
	if obj.SHA256 != "" {
		local.SHA256 = obj.SHA256
	} else {
		local.SHA256, _ = fsutil.SHA256File(plan.AbsPath)
	}
	if err := s.Store.Update(func(d *state.Data) error {
		cost.RegisterClassB(d, 1)
		cost.RegisterDownloaded(d, obj.Size)
		return nil
	}); err != nil {
		return err
	}
	return s.record(plan, local, obj, "restored")
}

func (s *Syncer) upload(ctx context.Context, plan fsutil.TargetPlan, local fsutil.Metadata) error {
	if local.SHA256 == "" {
		hash, err := fsutil.SHA256File(plan.AbsPath)
		if err != nil {
			return err
		}
		local.SHA256 = hash
	}
	snap := s.Store.Snapshot()
	oldSize := snap.Targets[plan.RelPath].Remote.Size
	decision := cost.Guard{Config: s.Config, State: snap}.CheckUpload(oldSize, local.Size)
	if !decision.Allowed {
		return fmt.Errorf("cost guard blocked upload: %s", decision.Reason)
	}
	meta := map[string]string{
		"r2sync-sha256":      local.SHA256,
		"r2sync-size":        fmt.Sprintf("%d", local.Size),
		"r2sync-source-path": plan.RelPath,
		"r2sync-updated-at":  time.Now().UTC().Format(time.RFC3339),
	}
	obj, err := s.Remote.Upload(ctx, plan.ObjectKey, plan.AbsPath, meta)
	if err != nil {
		return err
	}
	if err := s.Store.Update(func(d *state.Data) error {
		cost.RegisterClassA(d, cost.UploadClassAOps(local.Size))
		cost.RegisterUploaded(d, local.Size)
		return nil
	}); err != nil {
		return err
	}
	return s.record(plan, local, obj, "uploaded")
}

func (s *Syncer) record(plan fsutil.TargetPlan, local fsutil.Metadata, remote r2.Object, action string) error {
	return s.Store.Update(func(d *state.Data) error {
		d.Targets[plan.RelPath] = state.TargetRecord{
			Target:    plan.Target,
			AbsPath:   plan.AbsPath,
			ObjectKey: plan.ObjectKey,
			Local: state.LocalMetadata{
				Exists:  local.Exists,
				Size:    local.Size,
				ModTime: local.ModTime,
				SHA256:  local.SHA256,
			},
			Remote: state.RemoteMetadata{
				Exists:       remote.Exists,
				Size:         remote.Size,
				SHA256:       remote.SHA256,
				ETag:         remote.ETag,
				LastModified: remote.LastModified,
			},
			LastAction: action,
			UpdatedAt:  time.Now().UTC(),
		}
		return nil
	})
}

// Check is part of Runner. R2 credentials are fully validated when the
// remote store is constructed, so there is nothing left to verify here.
func (s *Syncer) Check(ctx context.Context) error {
	if s.Remote == nil {
		return config.ErrMissingCloudflareConfig
	}
	return nil
}

func (s *Syncer) setStatus(status state.Status) error {
	return applyStatus(s.Store, status)
}

// applyStatus writes a status update while preserving previously recorded
// timestamps that the update leaves unset.
func applyStatus(store *state.Store, status state.Status) error {
	return store.Update(func(d *state.Data) error {
		prev := d.Status
		if status.LastInitialSyncAt.IsZero() {
			status.LastInitialSyncAt = prev.LastInitialSyncAt
		}
		if status.LastScheduledAt.IsZero() {
			status.LastScheduledAt = prev.LastScheduledAt
		}
		if status.LastManualSyncAt.IsZero() {
			status.LastManualSyncAt = prev.LastManualSyncAt
		}
		if status.NextScheduledAt.IsZero() {
			status.NextScheduledAt = prev.NextScheduledAt
		}
		if status.LastSuccessfulSync.IsZero() {
			status.LastSuccessfulSync = prev.LastSuccessfulSync
		}
		d.Status = status
		return nil
	})
}

func (s *Syncer) nextScheduledAt(start time.Time) time.Time {
	interval := s.Config.SyncInterval
	if interval <= 0 {
		interval = config.DefaultSyncInterval
	}
	return start.Add(interval)
}
