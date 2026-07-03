package fsutil

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPlanTargetRejectsTraversal(t *testing.T) {
	_, err := PlanTarget(t.TempDir(), "", "../secret.txt")
	if err == nil {
		t.Fatal("expected traversal error")
	}
}

func TestPlanTargetBuildsObjectKey(t *testing.T) {
	base := t.TempDir()
	plan, err := PlanTarget(base, "prefix", "data/sophnet.db")
	if err != nil {
		t.Fatalf("PlanTarget() error = %v", err)
	}
	if plan.AbsPath != filepath.Join(base, "data", "sophnet.db") {
		t.Fatalf("AbsPath = %q", plan.AbsPath)
	}
	if plan.ObjectKey != "prefix/data/sophnet.db" {
		t.Fatalf("ObjectKey = %q", plan.ObjectKey)
	}
}

func TestSHA256File(t *testing.T) {
	path := filepath.Join(t.TempDir(), "a.txt")
	if err := os.WriteFile(path, []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}
	hash, err := SHA256File(path)
	if err != nil {
		t.Fatalf("SHA256File() error = %v", err)
	}
	if len(hash) != 64 || strings.Contains(hash, "sha256:") {
		t.Fatalf("unexpected hash %q", hash)
	}
}
