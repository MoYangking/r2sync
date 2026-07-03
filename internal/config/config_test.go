package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadEnvAndPublicRedactsToken(t *testing.T) {
	t.Setenv("R2SYNC_CONFIG", filepath.Join(t.TempDir(), "missing.json"))
	t.Setenv("R2SYNC_BUCKET", "test-bucket")
	t.Setenv("R2SYNC_TOKEN", "abcdefghijklmnopqrstuvwxyz")
	t.Setenv("R2SYNC_TARGETS", "data/a.db,data/b.db")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if cfg.BucketName != "test-bucket" {
		t.Fatalf("bucket = %q", cfg.BucketName)
	}
	if len(cfg.Targets) != 2 {
		t.Fatalf("targets = %#v", cfg.Targets)
	}
	pub := cfg.Public()
	if !pub.CloudflareConfigured {
		t.Fatal("expected CloudflareConfigured")
	}
	if pub.CloudflareToken == "" || pub.CloudflareToken == cfg.CloudflareToken {
		t.Fatalf("token was not redacted: %q", pub.CloudflareToken)
	}
}

func TestSaveAndReload(t *testing.T) {
	dir := t.TempDir()
	cfg := Defaults()
	cfg.ConfigPath = filepath.Join(dir, "config.json")
	cfg.StateDir = dir
	cfg.BucketName = "bucket"
	cfg.CloudflareToken = "secret"
	if err := cfg.Save(); err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	t.Setenv("R2SYNC_CONFIG", cfg.ConfigPath)
	got, err := Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if got.BucketName != "bucket" || got.CloudflareToken != "secret" {
		t.Fatalf("loaded config mismatch: %#v", got.Public())
	}
	if _, err := os.Stat(cfg.ConfigPath); err != nil {
		t.Fatalf("config not saved: %v", err)
	}
}

func TestStateDirEnvControlsDefaultConfigPath(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("R2SYNC_STATE_DIR", dir)
	cfg := Defaults()
	cfg.StateDir = dir
	cfg.ConfigPath = filepath.Join(dir, DefaultConfigFileName)
	cfg.BucketName = "from-state-dir"
	if err := cfg.Save(); err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	got, err := Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if got.BucketName != "from-state-dir" {
		t.Fatalf("bucket = %q", got.BucketName)
	}
}
