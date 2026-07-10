package syncer

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"r2sync/internal/config"
	"r2sync/internal/state"
)

// newGitFixture prepares a GitSyncer wired to a local bare repository that
// stands in for GitHub.
func newGitFixture(t *testing.T) (*GitSyncer, string, string) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git binary not available")
	}
	base := t.TempDir()
	bare := filepath.Join(t.TempDir(), "origin.git")
	if out, err := exec.Command("git", "init", "--bare", "--initial-branch=main", bare).CombinedOutput(); err != nil {
		// Older git without --initial-branch.
		if out2, err2 := exec.Command("git", "init", "--bare", bare).CombinedOutput(); err2 != nil {
			t.Fatalf("git init --bare: %v: %s / %v: %s", err, out, err2, out2)
		}
	}

	cfg := config.Defaults()
	cfg.BaseDir = base
	cfg.StateDir = filepath.Join(base, ".r2sync")
	cfg.SyncMethod = config.MethodGitHub
	cfg.GitHubRepo = "example/state"
	cfg.GitHubToken = "test-token"
	cfg.GitHubBranch = "main"
	cfg.Targets = []string{"data/app.db"}
	cfg.StorageCapBytes = 1024 * 1024
	if err := cfg.Normalize(); err != nil {
		t.Fatal(err)
	}
	st, err := state.Open(filepath.Join(cfg.StateDir, config.DefaultStateFileName))
	if err != nil {
		t.Fatal(err)
	}
	g := NewGitSyncer(cfg, st)
	g.remoteOverride = bare
	return g, base, bare
}

func bareFileContent(t *testing.T, bare, branch, rel string) (string, bool) {
	t.Helper()
	out, err := exec.Command("git", "-C", bare, "show", branch+":"+rel).CombinedOutput()
	if err != nil {
		return "", false
	}
	return string(out), true
}

func TestGitSyncMigratesLinksAndPushes(t *testing.T) {
	g, base, bare := newGitFixture(t)
	path := filepath.Join(base, "data", "app.db")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("v1"), 0o644); err != nil {
		t.Fatal(err)
	}

	res, err := g.Sync(context.Background(), ModeInitial)
	if err != nil {
		t.Fatalf("initial git sync: %v", err)
	}
	if res.Uploaded != 1 {
		t.Fatalf("Uploaded = %d, want 1", res.Uploaded)
	}

	// The original path must now be a symlink into the repository.
	info, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("%s is not a symlink", path)
	}
	linkDst, err := os.Readlink(path)
	if err != nil {
		t.Fatal(err)
	}
	wantDst := filepath.Join(g.Config.RepoDir, "data", "app.db")
	if linkDst != wantDst {
		t.Fatalf("symlink -> %s, want %s", linkDst, wantDst)
	}
	if got, _ := os.ReadFile(path); string(got) != "v1" {
		t.Fatalf("content through symlink = %q", got)
	}
	if content, ok := bareFileContent(t, bare, "main", "data/app.db"); !ok || content != "v1" {
		t.Fatalf("remote content = %q ok=%v", content, ok)
	}
	if !g.Store.Snapshot().Status.Ready {
		t.Fatal("expected ready after initial git sync")
	}

	// Writing through the symlink and running a scheduled sync must land on
	// the remote.
	if err := os.WriteFile(path, []byte("v2 updated"), 0o644); err != nil {
		t.Fatal(err)
	}
	res, err = g.Sync(context.Background(), ModeScheduled)
	if err != nil {
		t.Fatalf("scheduled git sync: %v", err)
	}
	if res.Uploaded != 1 {
		t.Fatalf("scheduled Uploaded = %d, want 1", res.Uploaded)
	}
	if content, ok := bareFileContent(t, bare, "main", "data/app.db"); !ok || content != "v2 updated" {
		t.Fatalf("remote content after update = %q ok=%v", content, ok)
	}
}

func TestGitSyncRestoresFromExistingRemote(t *testing.T) {
	seed, seedBase, bare := newGitFixture(t)
	seedPath := filepath.Join(seedBase, "data", "app.db")
	if err := os.MkdirAll(filepath.Dir(seedPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(seedPath, []byte("remote data"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := seed.Sync(context.Background(), ModeInitial); err != nil {
		t.Fatalf("seed sync: %v", err)
	}

	// A fresh deployment (empty base dir) pointing at the same remote must
	// restore the file and expose it at the original path.
	freshBase := t.TempDir()
	cfg := seed.Config
	cfg.BaseDir = freshBase
	cfg.StateDir = filepath.Join(freshBase, ".r2sync")
	cfg.RepoDir = filepath.Join(cfg.StateDir, "repo")
	if err := cfg.Normalize(); err != nil {
		t.Fatal(err)
	}
	st, err := state.Open(filepath.Join(cfg.StateDir, config.DefaultStateFileName))
	if err != nil {
		t.Fatal(err)
	}
	fresh := NewGitSyncer(cfg, st)
	fresh.remoteOverride = bare

	res, err := fresh.Sync(context.Background(), ModeInitial)
	if err != nil {
		t.Fatalf("fresh initial sync: %v", err)
	}
	if res.Restored != 1 {
		t.Fatalf("Restored = %d, want 1 (result: %+v)", res.Restored, res)
	}
	got, err := os.ReadFile(filepath.Join(freshBase, "data", "app.db"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "remote data" {
		t.Fatalf("restored content = %q", got)
	}
}

func TestGitSyncQuarantinesDifferingLocalFile(t *testing.T) {
	seed, seedBase, bare := newGitFixture(t)
	seedPath := filepath.Join(seedBase, "data", "app.db")
	if err := os.MkdirAll(filepath.Dir(seedPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(seedPath, []byte("remote wins"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := seed.Sync(context.Background(), ModeInitial); err != nil {
		t.Fatal(err)
	}

	freshBase := t.TempDir()
	cfg := seed.Config
	cfg.BaseDir = freshBase
	cfg.StateDir = filepath.Join(freshBase, ".r2sync")
	cfg.RepoDir = filepath.Join(cfg.StateDir, "repo")
	if err := cfg.Normalize(); err != nil {
		t.Fatal(err)
	}
	localPath := filepath.Join(freshBase, "data", "app.db")
	if err := os.MkdirAll(filepath.Dir(localPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(localPath, []byte("stale local"), 0o644); err != nil {
		t.Fatal(err)
	}
	st, err := state.Open(filepath.Join(cfg.StateDir, config.DefaultStateFileName))
	if err != nil {
		t.Fatal(err)
	}
	fresh := NewGitSyncer(cfg, st)
	fresh.remoteOverride = bare

	if _, err := fresh.Sync(context.Background(), ModeInitial); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(localPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "remote wins" {
		t.Fatalf("content = %q, want remote version", got)
	}
	matches, err := filepath.Glob(filepath.Join(cfg.StateDir, "quarantine", "*", "data", "app.db"))
	if err != nil || len(matches) != 1 {
		t.Fatalf("expected quarantined local copy, matches=%v err=%v", matches, err)
	}
	q, _ := os.ReadFile(matches[0])
	if string(q) != "stale local" {
		t.Fatalf("quarantine content = %q", q)
	}
}

func TestGitDeleteRemoteMaterializesFile(t *testing.T) {
	g, base, bare := newGitFixture(t)
	path := filepath.Join(base, "data", "app.db")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("keep me"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := g.Sync(context.Background(), ModeInitial); err != nil {
		t.Fatal(err)
	}

	if err := g.DeleteRemote(context.Background(), "data/app.db", "WRONG"); err == nil {
		t.Fatal("expected confirmation error")
	}
	if err := g.DeleteRemote(context.Background(), "data/app.db", "DELETE"); err != nil {
		t.Fatalf("DeleteRemote: %v", err)
	}

	// Local path must be a real file again with the same content.
	info, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		t.Fatal("path is still a symlink after remote delete")
	}
	got, _ := os.ReadFile(path)
	if string(got) != "keep me" {
		t.Fatalf("materialized content = %q", got)
	}
	if _, ok := bareFileContent(t, bare, "main", "data/app.db"); ok {
		t.Fatal("remote still has the file after delete")
	}
	rec := g.Store.Snapshot().Targets["data/app.db"]
	if rec.LastAction != "remote_deleted" {
		t.Fatalf("LastAction = %q", rec.LastAction)
	}
}

func TestGitSyncDirectoryTarget(t *testing.T) {
	g, base, bare := newGitFixture(t)
	g.Config.Targets = []string{"data/store/"}
	dir := filepath.Join(base, "data", "store")
	if err := os.MkdirAll(filepath.Join(dir, "nested"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("A"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "nested", "b.txt"), []byte("B"), 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := g.Sync(context.Background(), ModeInitial); err != nil {
		t.Fatalf("initial dir sync: %v", err)
	}
	info, err := os.Lstat(dir)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode()&os.ModeSymlink == 0 {
		t.Fatal("directory target was not replaced by a symlink")
	}
	if content, ok := bareFileContent(t, bare, "main", "data/store/nested/b.txt"); !ok || content != "B" {
		t.Fatalf("remote nested content = %q ok=%v", content, ok)
	}
	// Files must remain reachable through the symlinked directory.
	got, err := os.ReadFile(filepath.Join(dir, "a.txt"))
	if err != nil || string(got) != "A" {
		t.Fatalf("read through dir symlink = %q err=%v", got, err)
	}
}

func TestGitSyncerImplementsRunner(t *testing.T) {
	var _ Runner = (*GitSyncer)(nil)
	var _ Runner = (*Syncer)(nil)
}

func TestChangedPathsParsing(t *testing.T) {
	g, base, _ := newGitFixture(t)
	path := filepath.Join(base, "data", "app.db")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("v1"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := g.Sync(context.Background(), ModeInitial); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("changed"), 0o644); err != nil {
		t.Fatal(err)
	}
	changed, err := g.changedPaths(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for rel := range changed {
		if strings.HasSuffix(rel, "data/app.db") {
			found = true
		}
	}
	if !found {
		t.Fatalf("changedPaths = %v, want data/app.db", changed)
	}
}
