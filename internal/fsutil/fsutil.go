package fsutil

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

type TargetPlan struct {
	Target    string
	AbsPath   string
	RelPath   string
	ObjectKey string
}

type Metadata struct {
	Exists  bool
	Size    int64
	ModTime time.Time
	SHA256  string
}

var ErrUnsafePath = errors.New("unsafe path")

func PlanTarget(baseDir, objectPrefix, target string) (TargetPlan, error) {
	target = strings.TrimSpace(target)
	if target == "" {
		return TargetPlan{}, fmt.Errorf("%w: empty target", ErrUnsafePath)
	}
	cleanTarget := filepath.Clean(target)
	if strings.Contains(cleanTarget, "..") {
		parts := strings.FieldsFunc(cleanTarget, func(r rune) bool { return r == '/' || r == '\\' })
		for _, part := range parts {
			if part == ".." {
				return TargetPlan{}, fmt.Errorf("%w: %s", ErrUnsafePath, target)
			}
		}
	}

	var abs string
	if filepath.IsAbs(cleanTarget) {
		abs = cleanTarget
	} else {
		abs = filepath.Join(baseDir, cleanTarget)
	}
	abs, err := filepath.Abs(abs)
	if err != nil {
		return TargetPlan{}, fmt.Errorf("resolve target path: %w", err)
	}

	rel := cleanTarget
	if filepath.IsAbs(rel) {
		rel = strings.TrimPrefix(filepath.ToSlash(rel), filepath.ToSlash(filepath.VolumeName(rel)))
		rel = strings.TrimLeft(rel, "/")
	}
	rel = strings.TrimLeft(filepath.ToSlash(filepath.Clean(rel)), "/")
	if rel == "." || rel == "" || strings.HasPrefix(rel, "../") || strings.Contains(rel, "/../") {
		return TargetPlan{}, fmt.Errorf("%w: %s", ErrUnsafePath, target)
	}

	keyPrefix := strings.Trim(strings.ReplaceAll(objectPrefix, "\\", "/"), "/")
	key := rel
	if keyPrefix != "" {
		key = keyPrefix + "/" + rel
	}
	key = strings.TrimLeft(filepath.ToSlash(filepath.Clean(key)), "/")
	if strings.Contains(key, "..") || strings.HasPrefix(key, "/") || key == "." || key == "" {
		return TargetPlan{}, fmt.Errorf("%w: object key %s", ErrUnsafePath, key)
	}
	if runtime.GOOS == "windows" {
		key = strings.ReplaceAll(key, ":", "_")
	}

	return TargetPlan{Target: target, AbsPath: abs, RelPath: rel, ObjectKey: key}, nil
}

func Stat(path string) (Metadata, error) {
	info, err := os.Stat(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return Metadata{}, nil
		}
		return Metadata{}, fmt.Errorf("stat %s: %w", path, err)
	}
	if info.IsDir() {
		return Metadata{}, fmt.Errorf("target is a directory, not a file: %s", path)
	}
	return Metadata{Exists: true, Size: info.Size(), ModTime: info.ModTime().UTC()}, nil
}

func SHA256File(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", fmt.Errorf("open for hash %s: %w", path, err)
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", fmt.Errorf("hash %s: %w", path, err)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func EnsureParent(path string) error {
	parent := filepath.Dir(path)
	if parent == "" || parent == "." {
		return nil
	}
	if err := os.MkdirAll(parent, 0o755); err != nil {
		return fmt.Errorf("create parent directory %s: %w", parent, err)
	}
	return nil
}

func WriteFileAtomic(path string, r io.Reader) error {
	if err := EnsureParent(path); err != nil {
		return err
	}
	tmp := path + ".r2sync.tmp"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o644)
	if err != nil {
		return fmt.Errorf("create temp file %s: %w", tmp, err)
	}
	_, copyErr := io.Copy(f, r)
	closeErr := f.Close()
	if copyErr != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("write temp file %s: %w", tmp, copyErr)
	}
	if closeErr != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("close temp file %s: %w", tmp, closeErr)
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("replace %s: %w", path, err)
	}
	return nil
}

func CopyToQuarantine(src, stateDir, relPath string) (string, error) {
	if _, err := os.Stat(src); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", nil
		}
		return "", fmt.Errorf("stat quarantine source: %w", err)
	}
	stamp := time.Now().UTC().Format("20060102T150405Z")
	dst := filepath.Join(stateDir, "quarantine", stamp, filepath.FromSlash(relPath))
	if err := EnsureParent(dst); err != nil {
		return "", err
	}
	in, err := os.Open(src)
	if err != nil {
		return "", fmt.Errorf("open quarantine source: %w", err)
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return "", fmt.Errorf("create quarantine destination: %w", err)
	}
	_, copyErr := io.Copy(out, in)
	closeErr := out.Close()
	if copyErr != nil {
		return "", fmt.Errorf("copy quarantine file: %w", copyErr)
	}
	if closeErr != nil {
		return "", fmt.Errorf("close quarantine file: %w", closeErr)
	}
	return dst, nil
}

func MetadataChanged(local Metadata, previousSize int64, previousModTime time.Time) bool {
	if !local.Exists {
		return previousSize != 0 || !previousModTime.IsZero()
	}
	return local.Size != previousSize || !local.ModTime.Equal(previousModTime)
}
