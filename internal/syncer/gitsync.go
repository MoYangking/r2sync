package syncer

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"r2sync/internal/config"
	"r2sync/internal/cost"
	"r2sync/internal/fsutil"
	"r2sync/internal/state"
)

// githubFileLimit is GitHub's hard per-file size limit; pushes containing
// larger blobs are rejected by the server.
const githubFileLimit = int64(100 * 1024 * 1024)

// GitSyncer implements the "github" sync method: targets are migrated into a
// dedicated git repository directory and replaced with symlinks, so programs
// keep reading/writing their original paths while the repository is pulled,
// committed and pushed as a whole.
//
// Compared to the reference implementation this variant:
//   - quarantines conflicting local files instead of deleting them,
//   - commits local changes before pull --rebase (a dirty worktree makes
//     rebase fail silently otherwise),
//   - never stores the token on disk (per-command HTTP header instead of a
//     token-embedded origin URL),
//   - initializes empty remotes with an empty commit instead of a stub file,
//   - propagates remote errors instead of treating them as "empty remote".
type GitSyncer struct {
	Config config.Config
	Store  *state.Store

	// remoteOverride replaces the GitHub URL in tests (e.g. a local bare
	// repository path).
	remoteOverride string
}

func NewGitSyncer(cfg config.Config, st *state.Store) *GitSyncer {
	return &GitSyncer{Config: cfg, Store: st}
}

type gitTarget struct {
	target  string // as configured (may end with "/" to mean directory)
	rel     string // clean slash-separated path relative to base dir
	src     string // original absolute path (becomes the symlink)
	dst     string // absolute path inside the repository
	wantDir bool
}

/* ---------------------------------------------------------------- runner */

func (g *GitSyncer) Verify(ctx context.Context) (Result, error) {
	return g.Sync(ctx, ModeVerify)
}

func (g *GitSyncer) Check(ctx context.Context) error {
	if err := g.precheck(); err != nil {
		return err
	}
	if err := os.MkdirAll(g.Config.RepoDir, 0o755); err != nil {
		return fmt.Errorf("create repo dir: %w", err)
	}
	_, err := g.gitNet(ctx, "ls-remote", g.remoteURL(), "HEAD")
	if err != nil {
		return fmt.Errorf("GitHub repository %s is not accessible: %w", g.Config.GitHubRepo, err)
	}
	return nil
}

func (g *GitSyncer) Sync(ctx context.Context, mode Mode) (Result, error) {
	if err := g.precheck(); err != nil {
		return Result{}, err
	}
	release, err := acquireSyncGate()
	if err != nil {
		return Result{}, err
	}
	defer release()

	start := time.Now().UTC()
	wasReady := g.Store.Snapshot().Status.Ready
	runningReady := wasReady && mode != ModeInitial
	setProgress := func(progress int, target string) {
		_ = applyStatus(g.Store, state.Status{Stage: string(mode), Progress: progress, CurrentTarget: target, Ready: runningReady})
	}
	setProgress(0, "")

	result, runErr := g.run(ctx, mode, setProgress)

	ready := runErr == nil
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
		status.NextScheduledAt = start.Add(g.interval())
	case ModeScheduled:
		status.LastScheduledAt = start
		status.NextScheduledAt = time.Now().UTC().Add(g.interval())
	case ModeManual:
		status.LastManualSyncAt = start
	}
	if runErr != nil {
		status.Stage = "error"
		status.Ready = ready
		status.LastError = runErr.Error()
		status.LastSuccessfulSync = time.Time{}
		_ = applyStatus(g.Store, status)
		_ = g.Store.AddEvent("error", runErr.Error(), "")
		return result, runErr
	}
	_ = applyStatus(g.Store, status)
	_ = g.Store.AddEvent("info", fmt.Sprintf("git sync complete: committed=%d restored=%d skipped=%d missing=%d",
		result.Uploaded, result.Restored, result.Skipped, result.Missing), "")
	return result, nil
}

func (g *GitSyncer) run(ctx context.Context, mode Mode, setProgress func(int, string)) (Result, error) {
	var result Result
	plans, err := g.planTargets()
	if err != nil {
		return result, err
	}
	if err := g.ensureRepo(ctx); err != nil {
		return result, err
	}
	setProgress(10, "")

	initial := mode == ModeInitial || !g.bootstrapped(ctx)
	if initial {
		if err := g.alignRemote(ctx); err != nil {
			return result, err
		}
		setProgress(30, "")
		for i, plan := range plans {
			if ctx.Err() != nil {
				return result, ctx.Err()
			}
			setProgress(30+int(float64(i)/float64(len(plans))*30), plan.target)
			action, err := g.migrateAndLink(plan)
			if err != nil {
				result.Errors++
				_ = g.Store.AddEvent("error", err.Error(), plan.target)
				return result, fmt.Errorf("link %s: %w", plan.target, err)
			}
			countAction(&result, action)
		}
	} else {
		setProgress(20, "")
		for _, plan := range plans {
			if ctx.Err() != nil {
				return result, ctx.Err()
			}
			if err := g.ensureLinked(plan); err != nil {
				result.Errors++
				_ = g.Store.AddEvent("error", err.Error(), plan.target)
				return result, fmt.Errorf("relink %s: %w", plan.target, err)
			}
		}
	}

	g.keepEmptyDirs(plans)
	setProgress(65, "")

	changedRels, err := g.changedPaths(ctx)
	if err != nil {
		return result, err
	}
	if len(changedRels) > 0 {
		if err := g.checkGuards(plans); err != nil {
			return result, err
		}
		if _, err := g.git(ctx, "add", "-A"); err != nil {
			return result, err
		}
		if g.hasStagedChanges(ctx) {
			if _, err := g.git(ctx, "commit", "-m", commitMessage(mode)); err != nil {
				return result, err
			}
		}
	}
	setProgress(80, "")

	pushed, err := g.pushWithReconcile(ctx, mode)
	if err != nil {
		return result, err
	}
	setProgress(95, "")

	changedTargets := matchTargets(plans, changedRels)
	if !initial {
		for _, plan := range plans {
			if changedTargets[plan.rel] {
				result.Uploaded++
			} else {
				result.Skipped++
			}
		}
	}
	g.recordTargets(plans, changedTargets, pushed)
	return result, nil
}

func countAction(result *Result, action string) {
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

func commitMessage(mode Mode) string {
	switch mode {
	case ModeInitial:
		return "r2sync: link targets"
	case ModeManual:
		return "r2sync: manual sync"
	case ModeVerify:
		return "r2sync: verify sync"
	default:
		return "r2sync: scheduled sync"
	}
}

func (g *GitSyncer) precheck() error {
	if err := g.Config.ValidateSyncConfig(); err != nil {
		return err
	}
	if len(g.Config.Targets) == 0 {
		return fmt.Errorf("no sync targets configured")
	}
	if _, err := exec.LookPath("git"); err != nil {
		return fmt.Errorf("git binary not found in PATH; the GitHub sync method requires git")
	}
	return nil
}

func (g *GitSyncer) interval() time.Duration {
	if g.Config.SyncInterval > 0 {
		return g.Config.SyncInterval
	}
	return config.DefaultSyncInterval
}

/* ------------------------------------------------------------ repository */

func (g *GitSyncer) remoteURL() string {
	if g.remoteOverride != "" {
		return g.remoteOverride
	}
	return "https://github.com/" + g.Config.GitHubRepo + ".git"
}

func (g *GitSyncer) git(ctx context.Context, args ...string) (string, error) {
	return g.runGit(ctx, false, args...)
}

// gitNet runs git commands that talk to the remote; the token travels as a
// per-invocation HTTP header so it is never written to .git/config.
func (g *GitSyncer) gitNet(ctx context.Context, args ...string) (string, error) {
	return g.runGit(ctx, true, args...)
}

func (g *GitSyncer) runGit(ctx context.Context, withAuth bool, args ...string) (string, error) {
	full := []string{
		"-C", g.Config.RepoDir,
		"-c", "commit.gpgsign=false",
		"-c", "core.quotepath=false",
		"-c", "user.name=r2sync",
		"-c", "user.email=r2sync@localhost",
	}
	if withAuth && g.Config.GitHubToken != "" {
		header := "Authorization: Basic " +
			base64.StdEncoding.EncodeToString([]byte("x-access-token:"+g.Config.GitHubToken))
		full = append(full, "-c", "http.https://github.com/.extraheader="+header)
	}
	full = append(full, args...)
	cmd := exec.CommandContext(ctx, "git", full...)
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0", "LC_ALL=C")
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	err := cmd.Run()
	// Return raw output: porcelain formats are column-sensitive, so callers
	// trim only where safe.
	text := out.String()
	if err != nil {
		detail := strings.TrimSpace(text)
		if len(detail) > 400 {
			detail = detail[len(detail)-400:]
		}
		return text, fmt.Errorf("git %s failed: %s", args[0], detail)
	}
	return text, nil
}

func (g *GitSyncer) ensureRepo(ctx context.Context) error {
	if err := os.MkdirAll(g.Config.RepoDir, 0o755); err != nil {
		return fmt.Errorf("create repo dir: %w", err)
	}
	if _, err := os.Stat(filepath.Join(g.Config.RepoDir, ".git")); err != nil {
		if _, err := g.git(ctx, "init"); err != nil {
			return err
		}
		if _, err := g.git(ctx, "symbolic-ref", "HEAD", "refs/heads/"+g.Config.GitHubBranch); err != nil {
			return err
		}
	}
	remotes, _ := g.git(ctx, "remote")
	hasOrigin := false
	for _, name := range strings.Fields(remotes) {
		if name == "origin" {
			hasOrigin = true
			break
		}
	}
	if hasOrigin {
		if _, err := g.git(ctx, "remote", "set-url", "origin", g.remoteURL()); err != nil {
			return err
		}
	} else {
		if _, err := g.git(ctx, "remote", "add", "origin", g.remoteURL()); err != nil {
			return err
		}
	}
	return g.writeGitExclude()
}

// writeGitExclude keeps configured excludes out of the repository without
// committing a .gitignore into the user's data.
func (g *GitSyncer) writeGitExclude() error {
	dir := filepath.Join(g.Config.RepoDir, ".git", "info")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("create git info dir: %w", err)
	}
	var b strings.Builder
	b.WriteString("# managed by r2sync\n")
	for _, ex := range g.Config.Excludes {
		if ex = strings.TrimSpace(ex); ex != "" {
			b.WriteString(ex + "\n")
		}
	}
	return os.WriteFile(filepath.Join(dir, "exclude"), []byte(b.String()), 0o644)
}

func (g *GitSyncer) bootstrapped(ctx context.Context) bool {
	if _, err := os.Stat(filepath.Join(g.Config.RepoDir, ".git")); err != nil {
		return false
	}
	if _, err := g.git(ctx, "rev-parse", "--verify", "HEAD"); err != nil {
		return false
	}
	url, err := g.git(ctx, "remote", "get-url", "origin")
	return err == nil && strings.TrimSpace(url) == g.remoteURL()
}

// alignRemote makes the local repository match origin/<branch>, initializing
// the remote when it is empty. Local changes are committed first and rebased
// on top; on conflict the local side is quarantined and the remote wins,
// matching r2sync's initial-sync semantics.
func (g *GitSyncer) alignRemote(ctx context.Context) error {
	out, err := g.gitNet(ctx, "ls-remote", "origin")
	if err != nil {
		return err
	}
	branch := g.Config.GitHubBranch
	remoteHasBranch := false
	for _, line := range strings.Split(out, "\n") {
		if strings.HasSuffix(strings.TrimSpace(line), "refs/heads/"+branch) {
			remoteHasBranch = true
			break
		}
	}

	if _, err := g.git(ctx, "rev-parse", "--verify", "HEAD"); err != nil {
		if _, err := g.git(ctx, "commit", "--allow-empty", "-m", "r2sync: initialize repository"); err != nil {
			return err
		}
	}
	if !remoteHasBranch {
		_, err := g.gitNet(ctx, "push", "-u", "origin", branch)
		return err
	}

	if _, err := g.gitNet(ctx, "fetch", "--depth=1", "origin", branch); err != nil {
		return err
	}
	// Preserve anything written through the symlinks while the daemon was
	// down; a hard reset here would silently destroy newer local data.
	if _, err := g.git(ctx, "add", "-A"); err != nil {
		return err
	}
	if g.hasStagedChanges(ctx) {
		if _, err := g.git(ctx, "commit", "-m", "r2sync: preserve local changes before align"); err != nil {
			return err
		}
	}
	if _, err := g.git(ctx, "rebase", "origin/"+branch); err != nil {
		_, _ = g.git(ctx, "rebase", "--abort")
		if qerr := g.quarantineDivergence(ctx, branch); qerr != nil {
			_ = g.Store.AddEvent("warn", "quarantine before remote-wins reset failed: "+qerr.Error(), "")
		}
		if _, err := g.git(ctx, "reset", "--hard", "origin/"+branch); err != nil {
			return err
		}
		_ = g.Store.AddEvent("warn", "local history diverged from origin; remote version restored, local copies quarantined", "")
	}
	if _, err := g.git(ctx, "checkout", "-B", branch); err != nil {
		return err
	}
	return nil
}

// quarantineDivergence copies files that differ from origin/<branch> into the
// state quarantine directory before a remote-wins reset.
func (g *GitSyncer) quarantineDivergence(ctx context.Context, branch string) error {
	out, err := g.git(ctx, "diff", "--name-only", "origin/"+branch)
	if err != nil {
		return err
	}
	for _, rel := range strings.Split(out, "\n") {
		rel = strings.TrimSpace(rel)
		if rel == "" {
			continue
		}
		abs := filepath.Join(g.Config.RepoDir, filepath.FromSlash(rel))
		if info, err := os.Lstat(abs); err == nil && info.Mode().IsRegular() {
			if _, err := fsutil.CopyToQuarantine(abs, g.Config.StateDir, rel); err != nil {
				return err
			}
		}
	}
	return nil
}

func (g *GitSyncer) hasStagedChanges(ctx context.Context) bool {
	cmd := exec.CommandContext(ctx, "git", "-C", g.Config.RepoDir, "diff", "--cached", "--quiet")
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
	err := cmd.Run()
	if err == nil {
		return false
	}
	var exitErr *exec.ExitError
	return errors.As(err, &exitErr) && exitErr.ExitCode() == 1
}

// changedPaths returns worktree paths (relative, slash-separated) that differ
// from HEAD, before staging.
func (g *GitSyncer) changedPaths(ctx context.Context) (map[string]bool, error) {
	out, err := g.git(ctx, "status", "--porcelain")
	if err != nil {
		return nil, err
	}
	changed := map[string]bool{}
	for _, line := range strings.Split(out, "\n") {
		if len(line) < 4 {
			continue
		}
		path := strings.TrimSpace(line[3:])
		if idx := strings.Index(path, " -> "); idx >= 0 {
			path = path[idx+4:]
		}
		path = strings.Trim(path, "\"")
		if path != "" {
			changed[strings.TrimSuffix(path, "/")] = true
		}
	}
	return changed, nil
}

// pushWithReconcile pushes the branch; when the remote moved it commits
// nothing extra, rebases local commits on top and retries once.
func (g *GitSyncer) pushWithReconcile(ctx context.Context, mode Mode) (bool, error) {
	branch := g.Config.GitHubBranch
	if mode == ModeVerify || g.Config.StrictVerify {
		if _, err := g.gitNet(ctx, "fetch", "--depth=1", "origin", branch); err != nil {
			return false, err
		}
	}
	if _, err := g.gitNet(ctx, "push", "origin", branch); err == nil {
		return true, nil
	}
	if _, err := g.gitNet(ctx, "pull", "--rebase", "origin", branch); err != nil {
		_, _ = g.git(ctx, "rebase", "--abort")
		return false, fmt.Errorf("remote diverged and rebase failed: %w", err)
	}
	if _, err := g.gitNet(ctx, "push", "origin", branch); err != nil {
		return false, err
	}
	return true, nil
}

/* ------------------------------------------------------- migrate & link */

func (g *GitSyncer) planTargets() ([]gitTarget, error) {
	plans := make([]gitTarget, 0, len(g.Config.Targets))
	for _, target := range g.Config.Targets {
		plan, err := fsutil.PlanTarget(g.Config.BaseDir, "", target)
		if err != nil {
			return nil, err
		}
		plans = append(plans, gitTarget{
			target:  target,
			rel:     plan.RelPath,
			src:     plan.AbsPath,
			dst:     filepath.Join(g.Config.RepoDir, filepath.FromSlash(plan.RelPath)),
			wantDir: strings.HasSuffix(strings.TrimSpace(target), "/"),
		})
	}
	return plans, nil
}

// migrateAndLink moves a target into the repository and leaves a symlink at
// the original path. The repository copy wins when both sides exist and
// differ; the losing local file is quarantined first.
func (g *GitSyncer) migrateAndLink(plan gitTarget) (string, error) {
	info, lerr := os.Lstat(plan.src)
	switch {
	case lerr == nil && info.Mode()&os.ModeSymlink != 0:
		if err := ensureDstPlaceholder(plan); err != nil {
			return "", err
		}
		return "skipped", g.ensureSymlink(plan.src, plan.dst)

	case lerr == nil && info.IsDir():
		restored, err := g.mergeDirIntoRepo(plan.src, plan.dst, plan.rel)
		if err != nil {
			return "", err
		}
		if err := os.RemoveAll(plan.src); err != nil {
			return "", fmt.Errorf("remove migrated directory %s: %w", plan.src, err)
		}
		if err := g.ensureSymlink(plan.src, plan.dst); err != nil {
			return "", err
		}
		if restored {
			return "restored", nil
		}
		return "uploaded", nil

	case lerr == nil:
		dstExists := pathExists(plan.dst)
		if !dstExists {
			if err := movePath(plan.src, plan.dst); err != nil {
				return "", err
			}
			if err := g.ensureSymlink(plan.src, plan.dst); err != nil {
				return "", err
			}
			return "uploaded", nil
		}
		same, err := filesEqual(plan.src, plan.dst)
		if err != nil {
			return "", err
		}
		if !same {
			if _, err := fsutil.CopyToQuarantine(plan.src, g.Config.StateDir, plan.rel); err != nil {
				return "", err
			}
			_ = g.Store.AddEvent("warn", "local file differed from repository copy; repository wins, local copy quarantined", plan.target)
		}
		if err := os.Remove(plan.src); err != nil {
			return "", fmt.Errorf("remove migrated file %s: %w", plan.src, err)
		}
		if err := g.ensureSymlink(plan.src, plan.dst); err != nil {
			return "", err
		}
		if same {
			return "skipped", nil
		}
		return "restored", nil

	default:
		if pathExists(plan.dst) {
			if err := g.ensureSymlink(plan.src, plan.dst); err != nil {
				return "", err
			}
			return "restored", nil
		}
		if err := ensureDstPlaceholder(plan); err != nil {
			return "", err
		}
		if err := g.ensureSymlink(plan.src, plan.dst); err != nil {
			return "", err
		}
		return "missing", nil
	}
}

// ensureLinked repairs link drift during periodic syncs. If a program
// replaced the symlink with a real file, that file is newer local state:
// local wins and it is moved back into the repository.
func (g *GitSyncer) ensureLinked(plan gitTarget) error {
	info, err := os.Lstat(plan.src)
	if err != nil {
		if os.IsNotExist(err) {
			if err := ensureDstPlaceholder(plan); err != nil {
				return err
			}
			return g.ensureSymlink(plan.src, plan.dst)
		}
		return fmt.Errorf("lstat %s: %w", plan.src, err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return g.ensureSymlink(plan.src, plan.dst)
	}
	_ = g.Store.AddEvent("warn", "symlink was replaced by a real path; migrating newer local data into repository", plan.target)
	if info.IsDir() {
		if err := overwriteDirIntoRepo(plan.src, plan.dst); err != nil {
			return err
		}
		if err := os.RemoveAll(plan.src); err != nil {
			return fmt.Errorf("remove re-migrated directory %s: %w", plan.src, err)
		}
	} else {
		if err := os.RemoveAll(plan.dst); err != nil {
			return fmt.Errorf("replace repository copy %s: %w", plan.dst, err)
		}
		if err := movePath(plan.src, plan.dst); err != nil {
			return err
		}
	}
	return g.ensureSymlink(plan.src, plan.dst)
}

func (g *GitSyncer) ensureSymlink(src, dst string) error {
	if err := fsutil.EnsureParent(src); err != nil {
		return err
	}
	if info, err := os.Lstat(src); err == nil {
		if info.Mode()&os.ModeSymlink != 0 {
			if current, err := os.Readlink(src); err == nil && current == dst {
				return nil
			}
		}
		if err := os.RemoveAll(src); err != nil {
			return fmt.Errorf("remove %s before linking: %w", src, err)
		}
	}
	if err := os.Symlink(dst, src); err != nil {
		return fmt.Errorf("symlink %s -> %s: %w", src, dst, err)
	}
	return nil
}

// mergeDirIntoRepo copies local files that the repository does not have yet;
// files present on both sides keep the repository version (differing local
// copies are quarantined). Reports whether any repository file won.
func (g *GitSyncer) mergeDirIntoRepo(src, dst, rel string) (bool, error) {
	restored := false
	err := filepath.Walk(src, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		sub, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		targetPath := filepath.Join(dst, sub)
		if info.IsDir() {
			return os.MkdirAll(targetPath, 0o755)
		}
		if !info.Mode().IsRegular() {
			return nil
		}
		if !pathExists(targetPath) {
			return copyFile(path, targetPath, info.Mode())
		}
		same, err := filesEqual(path, targetPath)
		if err != nil {
			return err
		}
		if !same {
			relUnder := rel + "/" + filepath.ToSlash(sub)
			if _, err := fsutil.CopyToQuarantine(path, g.Config.StateDir, relUnder); err != nil {
				return err
			}
			restored = true
		}
		return nil
	})
	if err != nil {
		return restored, fmt.Errorf("merge directory %s into repository: %w", src, err)
	}
	return restored, nil
}

// overwriteDirIntoRepo copies everything from src over dst (local wins).
func overwriteDirIntoRepo(src, dst string) error {
	return filepath.Walk(src, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		sub, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		targetPath := filepath.Join(dst, sub)
		if info.IsDir() {
			return os.MkdirAll(targetPath, 0o755)
		}
		if !info.Mode().IsRegular() {
			return nil
		}
		return copyFile(path, targetPath, info.Mode())
	})
}

func ensureDstPlaceholder(plan gitTarget) error {
	if pathExists(plan.dst) {
		return nil
	}
	if plan.wantDir {
		return os.MkdirAll(plan.dst, 0o755)
	}
	if err := fsutil.EnsureParent(plan.dst); err != nil {
		return err
	}
	f, err := os.OpenFile(plan.dst, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
	if err != nil {
		if os.IsExist(err) {
			return nil
		}
		return fmt.Errorf("create placeholder %s: %w", plan.dst, err)
	}
	return f.Close()
}

// keepEmptyDirs drops .gitkeep files into empty directories under directory
// targets so git tracks them.
func (g *GitSyncer) keepEmptyDirs(plans []gitTarget) {
	for _, plan := range plans {
		info, err := os.Stat(plan.dst)
		if err != nil || !info.IsDir() {
			continue
		}
		_ = filepath.Walk(plan.dst, func(path string, fi os.FileInfo, err error) error {
			if err != nil || !fi.IsDir() {
				return nil
			}
			if fi.Name() == ".git" {
				return filepath.SkipDir
			}
			entries, err := os.ReadDir(path)
			if err == nil && len(entries) == 0 {
				_ = os.WriteFile(filepath.Join(path, ".gitkeep"), nil, 0o644)
			}
			return nil
		})
	}
}

/* ------------------------------------------------------ guards & records */

func (g *GitSyncer) checkGuards(plans []gitTarget) error {
	var total int64
	for _, plan := range plans {
		size, largest := pathSize(plan.dst)
		total += size
		if largest.size > githubFileLimit {
			_ = g.Store.AddEvent("warn",
				fmt.Sprintf("%s is %d bytes; GitHub rejects files above 100 MiB — consider the R2 method for large files", largest.path, largest.size),
				plan.target)
		}
	}
	if g.Config.DisableCostGuards {
		return nil
	}
	if g.Config.StorageCapBytes > 0 && total > g.Config.StorageCapBytes {
		return fmt.Errorf("storage cap exceeded: repository targets use %d bytes > cap %d bytes", total, g.Config.StorageCapBytes)
	}
	return nil
}

func (g *GitSyncer) recordTargets(plans []gitTarget, changed map[string]bool, pushed bool) {
	now := time.Now().UTC()
	for _, plan := range plans {
		rec := g.Store.Snapshot().Targets[plan.rel]
		info, err := os.Stat(plan.dst)
		local := state.LocalMetadata{}
		if err == nil {
			if info.IsDir() {
				size, _ := pathSize(plan.dst)
				local = state.LocalMetadata{Exists: true, Size: size, ModTime: info.ModTime().UTC()}
			} else {
				local = state.LocalMetadata{Exists: true, Size: info.Size(), ModTime: info.ModTime().UTC()}
				if changed[plan.rel] || rec.Local.SHA256 == "" {
					if sum, herr := fsutil.SHA256File(plan.dst); herr == nil {
						local.SHA256 = sum
					}
				} else {
					local.SHA256 = rec.Local.SHA256
				}
			}
		}
		remote := rec.Remote
		if pushed && local.Exists {
			remote = state.RemoteMetadata{
				Exists:       true,
				Size:         local.Size,
				SHA256:       local.SHA256,
				LastModified: now,
			}
		}
		action := rec.LastAction
		switch {
		case changed[plan.rel] && pushed:
			action = "uploaded"
		case action == "":
			action = "verified"
		}
		_ = g.Store.Update(func(d *state.Data) error {
			d.Targets[plan.rel] = state.TargetRecord{
				Target:    plan.target,
				AbsPath:   plan.src,
				ObjectKey: g.Config.GitHubRepo + "@" + g.Config.GitHubBranch + ":" + plan.rel,
				Local:     local,
				Remote:    remote,
				LastAction: action,
				UpdatedAt: now,
			}
			return nil
		})
	}
}

/* ------------------------------------------------------------- deletion */

// DeleteRemote removes a target from the repository (and pushes the removal)
// after materializing the current content back at the original path, so the
// local program keeps its data as a plain file.
func (g *GitSyncer) DeleteRemote(ctx context.Context, target string, confirm string) error {
	if confirm != "DELETE" {
		return fmt.Errorf("remote delete requires confirmation")
	}
	if err := g.precheck(); err != nil {
		return err
	}
	plan, err := fsutil.PlanTarget(g.Config.BaseDir, "", target)
	if err != nil {
		return err
	}
	dst := filepath.Join(g.Config.RepoDir, filepath.FromSlash(plan.RelPath))

	if info, err := os.Lstat(plan.AbsPath); err == nil && info.Mode()&os.ModeSymlink != 0 {
		if err := os.Remove(plan.AbsPath); err != nil {
			return fmt.Errorf("remove symlink %s: %w", plan.AbsPath, err)
		}
		if dstInfo, err := os.Stat(dst); err == nil {
			if dstInfo.IsDir() {
				if err := overwriteDirIntoRepo(dst, plan.AbsPath); err != nil {
					return fmt.Errorf("materialize directory back to %s: %w", plan.AbsPath, err)
				}
			} else {
				if err := copyFile(dst, plan.AbsPath, dstInfo.Mode()); err != nil {
					return err
				}
			}
		}
	}
	if err := os.RemoveAll(dst); err != nil {
		return fmt.Errorf("remove repository copy %s: %w", dst, err)
	}
	if _, err := g.git(ctx, "add", "-A"); err != nil {
		return err
	}
	if g.hasStagedChanges(ctx) {
		if _, err := g.git(ctx, "commit", "-m", "r2sync: remove "+plan.RelPath+" from repository"); err != nil {
			return err
		}
	}
	if _, err := g.pushWithReconcile(ctx, ModeManual); err != nil {
		return err
	}
	return g.Store.Update(func(d *state.Data) error {
		cost.RegisterFree(d, 1)
		rec := d.Targets[plan.RelPath]
		rec.Remote = state.RemoteMetadata{}
		rec.LastAction = "remote_deleted"
		rec.UpdatedAt = time.Now().UTC()
		d.Targets[plan.RelPath] = rec
		return nil
	})
}

/* -------------------------------------------------------------- helpers */

func matchTargets(plans []gitTarget, changed map[string]bool) map[string]bool {
	out := map[string]bool{}
	for path := range changed {
		for _, plan := range plans {
			if path == plan.rel || strings.HasPrefix(path, plan.rel+"/") {
				out[plan.rel] = true
			}
		}
	}
	return out
}

func pathExists(path string) bool {
	_, err := os.Lstat(path)
	return err == nil
}

type largestFile struct {
	path string
	size int64
}

// pathSize returns the total size of a file or directory tree (excluding
// .git) and the largest regular file found.
func pathSize(path string) (int64, largestFile) {
	info, err := os.Stat(path)
	if err != nil {
		return 0, largestFile{}
	}
	if !info.IsDir() {
		return info.Size(), largestFile{path: path, size: info.Size()}
	}
	var total int64
	var largest largestFile
	_ = filepath.Walk(path, func(p string, fi os.FileInfo, err error) error {
		if err != nil {
			return nil
		}
		if fi.IsDir() {
			if fi.Name() == ".git" {
				return filepath.SkipDir
			}
			return nil
		}
		if fi.Mode().IsRegular() {
			total += fi.Size()
			if fi.Size() > largest.size {
				largest = largestFile{path: p, size: fi.Size()}
			}
		}
		return nil
	})
	return total, largest
}

func filesEqual(a, b string) (bool, error) {
	ia, err := os.Stat(a)
	if err != nil {
		return false, err
	}
	ib, err := os.Stat(b)
	if err != nil {
		return false, err
	}
	if ia.Size() != ib.Size() {
		return false, nil
	}
	ha, err := fsutil.SHA256File(a)
	if err != nil {
		return false, err
	}
	hb, err := fsutil.SHA256File(b)
	if err != nil {
		return false, err
	}
	return ha == hb, nil
}

func copyFile(src, dst string, mode os.FileMode) error {
	if err := fsutil.EnsureParent(dst); err != nil {
		return err
	}
	in, err := os.Open(src)
	if err != nil {
		return fmt.Errorf("open %s: %w", src, err)
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, mode.Perm())
	if err != nil {
		return fmt.Errorf("create %s: %w", dst, err)
	}
	_, copyErr := io.Copy(out, in)
	closeErr := out.Close()
	if copyErr != nil {
		return fmt.Errorf("copy %s -> %s: %w", src, dst, copyErr)
	}
	if closeErr != nil {
		return fmt.Errorf("close %s: %w", dst, closeErr)
	}
	return nil
}

// movePath renames src to dst, falling back to copy+remove across devices.
func movePath(src, dst string) error {
	if err := fsutil.EnsureParent(dst); err != nil {
		return err
	}
	if err := os.Rename(src, dst); err == nil {
		return nil
	}
	info, err := os.Stat(src)
	if err != nil {
		return err
	}
	if err := copyFile(src, dst, info.Mode()); err != nil {
		return err
	}
	return os.Remove(src)
}
