package syncer

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
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

func testSyncer(t *testing.T, remote *fakeRemote) (*Syncer, string) {
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
