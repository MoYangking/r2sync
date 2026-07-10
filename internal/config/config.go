package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

const (
	DefaultListenAddr       = "0.0.0.0:5321"
	DefaultSyncInterval     = 5 * time.Hour
	DefaultStorageCapBytes  = int64(4 * 1024 * 1024 * 1024)
	DefaultClassAWarnRatio  = 0.80
	DefaultClassABlockRatio = 0.95
	DefaultClassBWarnRatio  = 0.80
	DefaultClassBBlockRatio = 0.95
	DefaultObjectPrefix     = ""
	DefaultConfigFileName   = "config.json"
	DefaultStateFileName    = "state.json"
	DefaultTarget           = "data/sophnet.db"
	DefaultGitHubBranch     = "main"
	envPrefix               = "R2SYNC_"
	redactedSecret          = "********"
)

// Sync methods selectable via sync_method / R2SYNC_SYNC_METHOD.
const (
	MethodR2     = "r2"
	MethodGitHub = "github"
)

var ErrMissingCloudflareConfig = errors.New("missing Cloudflare R2 configuration")
var ErrMissingGitHubConfig = errors.New("missing GitHub repository configuration")

type Config struct {
	BaseDir           string        `json:"base_dir"`
	StateDir          string        `json:"state_dir"`
	ListenAddr        string        `json:"listen_addr"`
	SyncMethod        string        `json:"sync_method,omitempty"`
	BucketName        string        `json:"bucket_name"`
	AccountID         string        `json:"account_id,omitempty"`
	CloudflareToken   string        `json:"cloudflare_token,omitempty"`
	ObjectPrefix      string        `json:"object_prefix,omitempty"`
	GitHubRepo        string        `json:"github_repo,omitempty"`
	GitHubToken       string        `json:"github_token,omitempty"`
	GitHubBranch      string        `json:"github_branch,omitempty"`
	RepoDir           string        `json:"repo_dir,omitempty"`
	SyncInterval      time.Duration `json:"-"`
	SyncIntervalText  string        `json:"sync_interval"`
	Targets           []string      `json:"targets"`
	Excludes          []string      `json:"excludes"`
	StorageCapBytes   int64         `json:"storage_cap_bytes"`
	ClassAWarnRatio   float64       `json:"class_a_warn_ratio"`
	ClassABlockRatio  float64       `json:"class_a_block_ratio"`
	ClassBWarnRatio   float64       `json:"class_b_warn_ratio"`
	ClassBBlockRatio  float64       `json:"class_b_block_ratio"`
	StrictVerify      bool          `json:"strict_verify"`
	DisableCostGuards bool          `json:"disable_cost_guards"`
	AdminPasswordHash string        `json:"admin_password_hash,omitempty"`
	SessionSigningKey string        `json:"session_signing_key,omitempty"`
	ConfigPath        string        `json:"-"`
}

type PublicConfig struct {
	BaseDir              string   `json:"base_dir"`
	StateDir             string   `json:"state_dir"`
	ListenAddr           string   `json:"listen_addr"`
	SyncMethod           string   `json:"sync_method"`
	BucketName           string   `json:"bucket_name"`
	AccountID            string   `json:"account_id,omitempty"`
	CloudflareConfigured bool     `json:"cloudflare_configured"`
	CloudflareToken      string   `json:"cloudflare_token,omitempty"`
	ObjectPrefix         string   `json:"object_prefix,omitempty"`
	GitHubRepo           string   `json:"github_repo,omitempty"`
	GitHubConfigured     bool     `json:"github_configured"`
	GitHubToken          string   `json:"github_token,omitempty"`
	GitHubBranch         string   `json:"github_branch"`
	RepoDir              string   `json:"repo_dir"`
	SyncInterval         string   `json:"sync_interval"`
	Targets              []string `json:"targets"`
	Excludes             []string `json:"excludes"`
	StorageCapBytes      int64    `json:"storage_cap_bytes"`
	ClassAWarnRatio      float64  `json:"class_a_warn_ratio"`
	ClassABlockRatio     float64  `json:"class_a_block_ratio"`
	ClassBWarnRatio      float64  `json:"class_b_warn_ratio"`
	ClassBBlockRatio     float64  `json:"class_b_block_ratio"`
	StrictVerify         bool     `json:"strict_verify"`
	DisableCostGuards    bool     `json:"disable_cost_guards"`
}

func Defaults() Config {
	wd, err := os.Getwd()
	if err != nil || wd == "" {
		wd = "."
	}
	stateDir := filepath.Join(wd, ".r2sync")
	return Config{
		BaseDir:           wd,
		StateDir:          stateDir,
		ListenAddr:        DefaultListenAddr,
		SyncMethod:        MethodR2,
		ObjectPrefix:      DefaultObjectPrefix,
		GitHubBranch:      DefaultGitHubBranch,
		SyncInterval:      DefaultSyncInterval,
		SyncIntervalText:  DefaultSyncInterval.String(),
		Targets:           []string{DefaultTarget},
		Excludes:          []string{".r2sync", ".sync-complete", ".sync-progress.json", ".sync.ready"},
		StorageCapBytes:   DefaultStorageCapBytes,
		ClassAWarnRatio:   DefaultClassAWarnRatio,
		ClassABlockRatio:  DefaultClassABlockRatio,
		ClassBWarnRatio:   DefaultClassBWarnRatio,
		ClassBBlockRatio:  DefaultClassBBlockRatio,
		StrictVerify:      false,
		DisableCostGuards: false,
	}
}

func Load() (Config, error) {
	cfg := Defaults()
	if v := strings.TrimSpace(os.Getenv(envPrefix + "BASE_DIR")); v != "" {
		cfg.BaseDir = v
		// The default state dir follows the base dir unless explicitly set.
		cfg.StateDir = filepath.Join(v, ".r2sync")
	}
	if v := strings.TrimSpace(os.Getenv(envPrefix + "STATE_DIR")); v != "" {
		cfg.StateDir = v
	}
	cfgPath := strings.TrimSpace(os.Getenv(envPrefix + "CONFIG"))
	if cfgPath == "" {
		cfgPath = filepath.Join(cfg.StateDir, DefaultConfigFileName)
	}
	cfg.ConfigPath = cfgPath

	if data, err := os.ReadFile(cfgPath); err == nil {
		if err := json.Unmarshal(data, &cfg); err != nil {
			return Config{}, fmt.Errorf("read config file %s: %w", cfgPath, err)
		}
		cfg.ConfigPath = cfgPath
	} else if !errors.Is(err, os.ErrNotExist) {
		return Config{}, fmt.Errorf("read config file %s: %w", cfgPath, err)
	}

	applyEnv(&cfg)
	if err := cfg.Normalize(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

func (c *Config) Normalize() error {
	if c.BaseDir == "" {
		c.BaseDir = Defaults().BaseDir
	}
	if c.StateDir == "" {
		c.StateDir = filepath.Join(c.BaseDir, ".r2sync")
	}
	if c.ListenAddr == "" {
		c.ListenAddr = DefaultListenAddr
	}
	if c.ConfigPath == "" {
		c.ConfigPath = filepath.Join(c.StateDir, DefaultConfigFileName)
	}
	switch strings.ToLower(strings.TrimSpace(c.SyncMethod)) {
	case "", MethodR2:
		c.SyncMethod = MethodR2
	case MethodGitHub:
		c.SyncMethod = MethodGitHub
	default:
		return fmt.Errorf("invalid sync_method %q: must be %q or %q", c.SyncMethod, MethodR2, MethodGitHub)
	}
	c.GitHubRepo = strings.Trim(strings.TrimSpace(c.GitHubRepo), "/")
	if c.GitHubRepo != "" && strings.Count(c.GitHubRepo, "/") != 1 {
		return fmt.Errorf("invalid github_repo %q: expected owner/name", c.GitHubRepo)
	}
	if c.GitHubBranch == "" {
		c.GitHubBranch = DefaultGitHubBranch
	}
	if c.RepoDir == "" {
		c.RepoDir = filepath.Join(c.StateDir, "repo")
	}
	if c.SyncIntervalText == "" {
		c.SyncIntervalText = DefaultSyncInterval.String()
	}
	d, err := time.ParseDuration(c.SyncIntervalText)
	if err != nil {
		return fmt.Errorf("invalid sync_interval %q: %w", c.SyncIntervalText, err)
	}
	if d <= 0 {
		return fmt.Errorf("sync_interval must be positive")
	}
	c.SyncInterval = d
	c.Targets = normalizeList(c.Targets)
	if len(c.Targets) == 0 {
		c.Targets = []string{DefaultTarget}
	}
	c.Excludes = normalizeList(c.Excludes)
	c.Excludes = appendMissing(c.Excludes, ".r2sync", ".sync-complete", ".sync-progress.json", ".sync.ready")
	if c.StorageCapBytes <= 0 {
		c.StorageCapBytes = DefaultStorageCapBytes
	}
	if c.ClassAWarnRatio <= 0 {
		c.ClassAWarnRatio = DefaultClassAWarnRatio
	}
	if c.ClassABlockRatio <= 0 {
		c.ClassABlockRatio = DefaultClassABlockRatio
	}
	if c.ClassBWarnRatio <= 0 {
		c.ClassBWarnRatio = DefaultClassBWarnRatio
	}
	if c.ClassBBlockRatio <= 0 {
		c.ClassBBlockRatio = DefaultClassBBlockRatio
	}
	return nil
}

func (c Config) Save() error {
	if c.ConfigPath == "" {
		c.ConfigPath = filepath.Join(c.StateDir, DefaultConfigFileName)
	}
	if err := os.MkdirAll(filepath.Dir(c.ConfigPath), 0o700); err != nil {
		return fmt.Errorf("create config directory: %w", err)
	}
	c.SyncIntervalText = c.SyncIntervalTextOrDefault()
	data, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return fmt.Errorf("encode config: %w", err)
	}
	tmp := c.ConfigPath + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return fmt.Errorf("write config temp file: %w", err)
	}
	if err := os.Rename(tmp, c.ConfigPath); err != nil {
		return fmt.Errorf("replace config file: %w", err)
	}
	return nil
}

func (c Config) SyncIntervalTextOrDefault() string {
	if c.SyncIntervalText != "" {
		return c.SyncIntervalText
	}
	if c.SyncInterval > 0 {
		return c.SyncInterval.String()
	}
	return DefaultSyncInterval.String()
}

func (c Config) Public() PublicConfig {
	token := ""
	if c.CloudflareToken != "" {
		token = redactedSecret
	}
	ghToken := ""
	if c.GitHubToken != "" {
		ghToken = redactedSecret
	}
	return PublicConfig{
		BaseDir:              c.BaseDir,
		StateDir:             c.StateDir,
		ListenAddr:           c.ListenAddr,
		SyncMethod:           c.SyncMethod,
		BucketName:           c.BucketName,
		AccountID:            c.AccountID,
		CloudflareConfigured: c.CloudflareToken != "",
		CloudflareToken:      token,
		ObjectPrefix:         c.ObjectPrefix,
		GitHubRepo:           c.GitHubRepo,
		GitHubConfigured:     c.GitHubToken != "",
		GitHubToken:          ghToken,
		GitHubBranch:         c.GitHubBranch,
		RepoDir:              c.RepoDir,
		SyncInterval:         c.SyncIntervalTextOrDefault(),
		Targets:              append([]string(nil), c.Targets...),
		Excludes:             append([]string(nil), c.Excludes...),
		StorageCapBytes:      c.StorageCapBytes,
		ClassAWarnRatio:      c.ClassAWarnRatio,
		ClassABlockRatio:     c.ClassABlockRatio,
		ClassBWarnRatio:      c.ClassBWarnRatio,
		ClassBBlockRatio:     c.ClassBBlockRatio,
		StrictVerify:         c.StrictVerify,
		DisableCostGuards:    c.DisableCostGuards,
	}
}

func (c Config) HasCloudflareConfig() bool {
	return strings.TrimSpace(c.BucketName) != "" && strings.TrimSpace(c.CloudflareToken) != ""
}

func (c Config) HasGitHubConfig() bool {
	return strings.TrimSpace(c.GitHubRepo) != "" && strings.TrimSpace(c.GitHubToken) != ""
}

// HasSyncConfig reports whether the selected sync method has enough
// configuration to run.
func (c Config) HasSyncConfig() bool {
	if c.SyncMethod == MethodGitHub {
		return c.HasGitHubConfig()
	}
	return c.HasCloudflareConfig()
}

// ValidateSyncConfig returns the method-specific missing-configuration error.
func (c Config) ValidateSyncConfig() error {
	if c.SyncMethod == MethodGitHub {
		if !c.HasGitHubConfig() {
			return ErrMissingGitHubConfig
		}
		return nil
	}
	return c.ValidateCloudflareConfig()
}

func (c Config) ValidateCloudflareConfig() error {
	if !c.HasCloudflareConfig() {
		return ErrMissingCloudflareConfig
	}
	return nil
}

func MaskSecret(s string) string {
	if s == "" {
		return ""
	}
	if len(s) <= 8 {
		return redactedSecret
	}
	return s[:4] + strings.Repeat("*", 8) + s[len(s)-4:]
}

func applyEnv(c *Config) {
	setString := func(key string, dst *string) {
		if v := strings.TrimSpace(os.Getenv(envPrefix + key)); v != "" {
			*dst = v
		}
	}
	setString("BASE_DIR", &c.BaseDir)
	setString("STATE_DIR", &c.StateDir)
	setString("LISTEN_ADDR", &c.ListenAddr)
	setString("SYNC_METHOD", &c.SyncMethod)
	setString("BUCKET", &c.BucketName)
	setString("BUCKET_NAME", &c.BucketName)
	setString("ACCOUNT_ID", &c.AccountID)
	setString("TOKEN", &c.CloudflareToken)
	setString("CLOUDFLARE_TOKEN", &c.CloudflareToken)
	setString("OBJECT_PREFIX", &c.ObjectPrefix)
	setString("GITHUB_REPO", &c.GitHubRepo)
	setString("GITHUB_TOKEN", &c.GitHubToken)
	setString("GITHUB_PAT", &c.GitHubToken)
	setString("GITHUB_BRANCH", &c.GitHubBranch)
	setString("REPO_DIR", &c.RepoDir)
	setString("SYNC_INTERVAL", &c.SyncIntervalText)
	setString("ADMIN_PASSWORD_HASH", &c.AdminPasswordHash)

	if v := strings.TrimSpace(os.Getenv(envPrefix + "TARGETS")); v != "" {
		c.Targets = splitList(v)
	}
	if v := strings.TrimSpace(os.Getenv(envPrefix + "EXCLUDES")); v != "" {
		c.Excludes = splitList(v)
	}
	if v := strings.TrimSpace(os.Getenv(envPrefix + "STORAGE_CAP_BYTES")); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil {
			c.StorageCapBytes = n
		}
	}
	if v := strings.TrimSpace(os.Getenv(envPrefix + "STRICT_VERIFY")); v != "" {
		c.StrictVerify = parseBool(v)
	}
	if v := strings.TrimSpace(os.Getenv(envPrefix + "DISABLE_COST_GUARDS")); v != "" {
		c.DisableCostGuards = parseBool(v)
	}
}

func splitList(v string) []string {
	return normalizeList(strings.FieldsFunc(v, func(r rune) bool {
		return r == ',' || r == ';' || r == '\n' || r == '\r' || r == '\t' || r == ' '
	}))
}

func normalizeList(values []string) []string {
	out := make([]string, 0, len(values))
	seen := map[string]struct{}{}
	for _, value := range values {
		v := strings.TrimSpace(value)
		v = strings.Trim(v, "\"'")
		v = strings.TrimLeft(v, "/\\")
		if v == "" {
			continue
		}
		if _, ok := seen[v]; ok {
			continue
		}
		seen[v] = struct{}{}
		out = append(out, v)
	}
	return out
}

func appendMissing(values []string, extras ...string) []string {
	seen := map[string]struct{}{}
	for _, value := range values {
		seen[value] = struct{}{}
	}
	for _, extra := range extras {
		if _, ok := seen[extra]; !ok {
			values = append(values, extra)
		}
	}
	return values
}

func parseBool(v string) bool {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "1", "true", "yes", "y", "on":
		return true
	default:
		return false
	}
}
