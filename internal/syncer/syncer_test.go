package syncer

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"r2sync/internal/config"
	"r2sync/internal/fsutil"
	"r2sync/internal/r2"
	"r2sync/internal/state"
)

type fakeRemote struct {
	objects map[string][]byte
	meta    map[string]map[string]string
}

func newFakeRemote() *fakeRemote {
	return &fakeRemote{objects: map[string][]byte{}, meta: map[string]map[string]string{}}
}

func (f *fakeRemote) Head(ctx context.Context, key string) (r2.Object, bool, error) {
	data, ok := f.objects[key]
	if !ok {
		return r2.Object{Key: key}, false, nil
	}
	return r2.Object{Key: key, Exists: true, Size: int64(len(data)), SHA256: f.meta[key]["r2sync-sha256"], Metadata: f.meta[key]}, true, nil
}

func (f *fakeRemote) Download(ctx context.Context, key string, w io.Writer) (r2.Object, error) {
	data := f.objects[key]
	_, err := w.Write(data)
	return r2.Object{Key: key, Exists: true, Size: int64(len(data)), SHA256: f.meta[key]["r2sync-sha256"], Metadata: f.meta[key]}, err
}

func (f *fakeRemote) Upload(ctx context.Context, key string, filePath string, metadata map[string]string) (r2.Object, error) {
	data, err := os.ReadFile(filePath)
	if err != nil {
		return r2.Object{}, err
	}
	f.objects[key] = append([]byte(nil), data...)
	f.meta[key] = metadata
	return r2.Object{Key: key, Exists: true, Size: int64(len(data)), SHA256: metadata["r2sync-sha256"], Metadata: metadata}, nil
}

func (f *fakeRemote) Delete(ctx context.Context, key string) error {
	delete(f.objects, key)
	delete(f.meta, key)
	return nil
}

func testSyncer(t *testing.T, remote r2.ObjectStore) (*Syncer, string) {
	t.Helper()
	base := t.TempDir()
	cfg := config.Defaults()
	cfg.BaseDir = base
	cfg.StateDir = filepath.Join(base, ".r2sync")
	cfg.Targets = []string{"data/sophnet.db"}
	cfg.StorageCapBytes = 1024 * 1024
	if err := cfg.Normalize(); err != nil {
		t.Fatal(err)
	}
	st, err := state.Open(filepath.Join(cfg.StateDir, config.DefaultStateFileName))
	if err != nil {
		t.Fatal(err)
	}
	return New(cfg, st, remote), filepath.Join(base, "data", "sophnet.db")
}

func TestInitialSyncRemoteWinsAndQuarantinesLocal(t *testing.T) {
	remote := newFakeRemote()
	hashRemote := hashBytes(t, []byte("remote"))
	remote.objects["data/sophnet.db"] = []byte("remote")
	remote.meta["data/sophnet.db"] = map[string]string{"r2sync-sha256": hashRemote}
	s, path := testSyncer(t, remote)
	if err := fsutil.EnsureParent(path); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("local"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := s.InitialSync(context.Background()); err != nil {
		t.Fatalf("InitialSync() error = %v", err)
	}
	got, _ := os.ReadFile(path)
	if string(got) != "remote" {
		t.Fatalf("local file = %q", got)
	}
	matches, err := filepath.Glob(filepath.Join(s.Config.StateDir, "quarantine", "*", "data", "sophnet.db"))
	if err != nil || len(matches) != 1 {
		t.Fatalf("expected quarantine copy, matches=%v err=%v", matches, err)
	}
}

func TestInitialSyncLocalWinsWhenRemoteMissing(t *testing.T) {
	remote := newFakeRemote()
	s, path := testSyncer(t, remote)
	if err := fsutil.EnsureParent(path); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("local"), 0o644); err != nil {
		t.Fatal(err)
	}
	res, err := s.InitialSync(context.Background())
	if err != nil {
		t.Fatalf("InitialSync() error = %v", err)
	}
	if res.Uploaded != 1 {
		t.Fatalf("Uploaded = %d", res.Uploaded)
	}
	if string(remote.objects["data/sophnet.db"]) != "local" {
		t.Fatalf("remote object = %q", remote.objects["data/sophnet.db"])
	}
	status := s.Store.Snapshot().Status
	if status.NextScheduledAt.IsZero() {
		t.Fatal("NextScheduledAt was not set after initial sync")
	}
	if status.LastInitialSyncAt.IsZero() {
		t.Fatal("LastInitialSyncAt was not set after initial sync")
	}
}

func TestScheduledSyncSkipsUnchangedLocalMetadata(t *testing.T) {
	remote := newFakeRemote()
	s, path := testSyncer(t, remote)
	if err := fsutil.EnsureParent(path); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("local"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := s.InitialSync(context.Background()); err != nil {
		t.Fatal(err)
	}
	time.Sleep(10 * time.Millisecond)
	res, err := s.ScheduledSync(context.Background())
	if err != nil {
		t.Fatalf("ScheduledSync() error = %v", err)
	}
	if res.Skipped != 1 {
		t.Fatalf("Skipped = %d", res.Skipped)
	}
}

type failUploadRemote struct {
	*fakeRemote
}

func (f *failUploadRemote) Upload(ctx context.Context, key string, filePath string, metadata map[string]string) (r2.Object, error) {
	return r2.Object{}, fmt.Errorf("simulated upload failure")
}

func TestScheduledSyncFailureKeepsReady(t *testing.T) {
	remote := newFakeRemote()
	s, path := testSyncer(t, remote)
	if err := fsutil.EnsureParent(path); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("local"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := s.InitialSync(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !s.Store.Snapshot().Status.Ready {
		t.Fatal("expected ready after initial sync")
	}
	if err := os.WriteFile(path, []byte("changed content"), 0o644); err != nil {
		t.Fatal(err)
	}
	failing := New(s.Config, s.Store, &failUploadRemote{fakeRemote: remote})
	if _, err := failing.ScheduledSync(context.Background()); err == nil {
		t.Fatal("expected scheduled sync to fail")
	}
	status := s.Store.Snapshot().Status
	if !status.Ready {
		t.Fatal("scheduled sync failure must not flip the startup gate to not-ready")
	}
	if status.Stage != "error" {
		t.Fatalf("Stage = %q, want error", status.Stage)
	}
}

func TestScheduledSyncRestoresNewTargetFromRemote(t *testing.T) {
	remote := newFakeRemote()
	s, path := testSyncer(t, remote)
	if err := fsutil.EnsureParent(path); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("local"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := s.InitialSync(context.Background()); err != nil {
		t.Fatal(err)
	}

	// data/extra.db already exists remotely but was never synced by this
	// instance; adding it as a target must restore it instead of marking it
	// missing forever (or blindly uploading over it).
	hash := hashBytes(t, []byte("remote extra"))
	remote.objects["data/extra.db"] = []byte("remote extra")
	remote.meta["data/extra.db"] = map[string]string{"r2sync-sha256": hash}

	cfg := s.Config
	cfg.Targets = append([]string(nil), cfg.Targets...)
	cfg.Targets = append(cfg.Targets, "data/extra.db")
	res, err := New(cfg, s.Store, remote).ScheduledSync(context.Background())
	if err != nil {
		t.Fatalf("ScheduledSync() error = %v", err)
	}
	if res.Restored != 1 {
		t.Fatalf("Restored = %d, want 1", res.Restored)
	}
	got, err := os.ReadFile(filepath.Join(cfg.BaseDir, "data", "extra.db"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "remote extra" {
		t.Fatalf("restored file = %q", got)
	}
}

type blockingRemote struct {
	*fakeRemote
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func (b *blockingRemote) Head(ctx context.Context, key string) (r2.Object, bool, error) {
	b.once.Do(func() { close(b.entered) })
	<-b.release
	return b.fakeRemote.Head(ctx, key)
}

func TestConcurrentSyncReturnsBusy(t *testing.T) {
	remote := &blockingRemote{
		fakeRemote: newFakeRemote(),
		entered:    make(chan struct{}),
		release:    make(chan struct{}),
	}
	s, path := testSyncer(t, remote)
	if err := fsutil.EnsureParent(path); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("local"), 0o644); err != nil {
		t.Fatal(err)
	}

	done := make(chan error, 1)
	go func() {
		_, err := s.ManualSync(context.Background())
		done <- err
	}()
	<-remote.entered

	if _, err := New(s.Config, s.Store, remote).ManualSync(context.Background()); !errors.Is(err, ErrSyncBusy) {
		t.Fatalf("second sync error = %v, want ErrSyncBusy", err)
	}
	close(remote.release)
	if err := <-done; err != nil {
		t.Fatalf("first sync error = %v", err)
	}
}

func TestManagerReconfigureRunsInitialAndScheduledSync(t *testing.T) {
	remote := newFakeRemote()
	base := t.TempDir()
	cfg := config.Defaults()
	cfg.BaseDir = base
	cfg.StateDir = filepath.Join(base, ".r2sync")
	cfg.Targets = []string{"data/app.db"}
	cfg.SyncIntervalText = "60ms"
	cfg.StorageCapBytes = 1024 * 1024
	if err := cfg.Normalize(); err != nil {
		t.Fatal(err)
	}
	st, err := state.Open(filepath.Join(cfg.StateDir, config.DefaultStateFileName))
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(base, "data", "app.db")
	if err := fsutil.EnsureParent(path); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("v1"), 0o644); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	m := NewManager(st, nil, func(_ context.Context, c config.Config) (Runner, error) {
		return New(c, st, remote), nil
	})

	incomplete := cfg
	incomplete.BucketName = ""
	incomplete.CloudflareToken = ""
	m.Start(ctx, incomplete)

	complete := cfg
	complete.BucketName = "bucket"
	complete.CloudflareToken = "token"
	m.Reconfigure(complete)

	waitFor(t, 3*time.Second, "initial sync via manager", func() bool {
		return st.Snapshot().Status.Ready
	})
	if string(remote.objects["data/app.db"]) != "v1" {
		t.Fatalf("remote object = %q", remote.objects["data/app.db"])
	}

	// A scheduled run must pick up local changes without any reconfigure.
	if err := os.WriteFile(path, []byte("v2 with longer content"), 0o644); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 3*time.Second, "scheduled sync upload", func() bool {
		return string(remote.objects["data/app.db"]) == "v2 with longer content"
	})
}

func waitFor(t *testing.T, timeout time.Duration, what string, fn func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if fn() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func hashBytes(t *testing.T, data []byte) string {
	t.Helper()
	tmp := filepath.Join(t.TempDir(), "hash")
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		t.Fatal(err)
	}
	h, err := fsutil.SHA256File(tmp)
	if err != nil {
		t.Fatal(err)
	}
	if h == "" || bytes.Equal(data, []byte(h)) {
		t.Fatalf("bad hash %q", h)
	}
	return h
}
