package server

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"r2sync/internal/buildinfo"
	"r2sync/internal/config"
	"r2sync/internal/cost"
	"r2sync/internal/state"
	"r2sync/internal/syncer"
	"r2sync/internal/ui"

	"golang.org/x/crypto/bcrypt"
)

type RunnerFactory = syncer.RunnerFactory

const (
	sessionCookieName = "r2sync_session"
	sessionTTL        = 24 * time.Hour
	minPasswordLength = 8
)

type Server struct {
	cfgMu         sync.RWMutex
	cfg           config.Config
	store         *state.Store
	log           *slog.Logger
	runnerFactory RunnerFactory

	// OnConfigChange is invoked after a configuration or targets update has
	// been saved, so the sync manager can apply it without a restart.
	OnConfigChange func(config.Config)

	sessionMu sync.Mutex
	sessions  map[string]time.Time

	logins loginLimiter
}

func New(cfg config.Config, store *state.Store, log *slog.Logger, factory RunnerFactory) *Server {
	if log == nil {
		log = slog.Default()
	}
	return &Server{
		cfg:           cfg,
		store:         store,
		log:           log,
		runnerFactory: factory,
		sessions:      map[string]time.Time{},
	}
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/health", s.handleHealth)
	mux.HandleFunc("GET /api/ready", s.handleReady)
	mux.HandleFunc("POST /api/login", s.handleLogin)
	mux.HandleFunc("POST /api/logout", s.withAuth(s.handleLogout))
	mux.HandleFunc("POST /api/password", s.withAuth(s.handlePassword))
	mux.HandleFunc("GET /api/progress", s.withAuth(s.handleProgress))
	mux.HandleFunc("GET /api/status", s.withAuth(s.handleStatus))
	mux.HandleFunc("GET /api/config", s.withAuth(s.handleGetConfig))
	mux.HandleFunc("PUT /api/config", s.withAuth(s.handlePutConfig))
	mux.HandleFunc("GET /api/targets", s.withAuth(s.handleTargets))
	mux.HandleFunc("PUT /api/targets", s.withAuth(s.handlePutTargets))
	mux.HandleFunc("POST /api/sync", s.withAuth(s.handleSync))
	mux.HandleFunc("POST /api/verify", s.withAuth(s.handleVerify))
	mux.HandleFunc("POST /api/objects/delete", s.withAuth(s.handleDelete))
	mux.Handle("/", http.FileServerFS(ui.StaticFS()))
	return securityHeaders(mux)
}

func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("Content-Security-Policy", "default-src 'self'; script-src 'self'; style-src 'self'; img-src 'self' data:; connect-src 'self'; font-src 'self'; base-uri 'none'; frame-ancestors 'none'; form-action 'self'")
		if strings.HasPrefix(r.URL.Path, "/api/") {
			h.Set("Cache-Control", "no-store")
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) ListenAndServe(ctx context.Context) error {
	srv := &http.Server{
		Addr:              s.currentConfig().ListenAddr,
		Handler:           s.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
	}()
	s.log.Info("management server listening", "addr", srv.Addr)
	err := srv.ListenAndServe()
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "version": buildinfo.Version})
}

func (s *Server) handleReady(w http.ResponseWriter, r *http.Request) {
	snap := s.store.Snapshot()
	writeJSON(w, http.StatusOK, map[string]any{"ready": snap.Status.Ready, "status": snap.Status})
}

func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	ip := clientIP(r)
	if wait := s.logins.blockedFor(ip); wait > 0 {
		w.Header().Set("Retry-After", strconv.Itoa(int(wait.Seconds())+1))
		writeError(w, http.StatusTooManyRequests, "too_many_attempts", "too many failed logins; try again later")
		return
	}
	var body struct {
		Password string `json:"password"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "invalid JSON body")
		return
	}
	hash := s.store.Snapshot().AdminPasswordHash
	if !checkPassword(hash, body.Password) {
		s.logins.recordFailure(ip)
		writeError(w, http.StatusUnauthorized, "unauthorized", "invalid password")
		return
	}
	s.logins.recordSuccess(ip)
	token, err := randomSecret(32)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "session_error", err.Error())
		return
	}
	s.sessionMu.Lock()
	s.pruneSessionsLocked()
	s.sessions[token] = time.Now().Add(sessionTTL)
	s.sessionMu.Unlock()
	http.SetCookie(w, s.sessionCookie(r, token, sessionTTL))
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	if cookie, err := r.Cookie(sessionCookieName); err == nil {
		s.sessionMu.Lock()
		delete(s.sessions, cookie.Value)
		s.sessionMu.Unlock()
	}
	expired := s.sessionCookie(r, "", 0)
	expired.MaxAge = -1
	http.SetCookie(w, expired)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) handlePassword(w http.ResponseWriter, r *http.Request) {
	var body struct {
		CurrentPassword string `json:"current_password"`
		NewPassword     string `json:"new_password"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "invalid JSON body")
		return
	}
	if !checkPassword(s.store.Snapshot().AdminPasswordHash, body.CurrentPassword) {
		writeError(w, http.StatusUnauthorized, "unauthorized", "current password is incorrect")
		return
	}
	if len(body.NewPassword) < minPasswordLength {
		writeError(w, http.StatusBadRequest, "weak_password", "new password must be at least 8 characters")
		return
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(body.NewPassword), bcrypt.DefaultCost)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "hash_failed", err.Error())
		return
	}
	if err := s.store.Update(func(d *state.Data) error {
		d.AdminPasswordHash = string(hash)
		return nil
	}); err != nil {
		writeError(w, http.StatusInternalServerError, "save_failed", err.Error())
		return
	}
	// Revoke every other session so a leaked cookie dies with the old password.
	current := ""
	if cookie, err := r.Cookie(sessionCookieName); err == nil {
		current = cookie.Value
	}
	s.sessionMu.Lock()
	for token := range s.sessions {
		if token != current {
			delete(s.sessions, token)
		}
	}
	s.sessionMu.Unlock()
	_ = s.store.AddEvent("info", "admin password changed", "")
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) handleProgress(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.store.Snapshot().Status)
}

func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	cfg := s.currentConfig()
	snap := s.store.Snapshot()
	writeJSON(w, http.StatusOK, map[string]any{
		"version":           buildinfo.Version,
		"config":            cfg.Public(),
		"status":            snap.Status,
		"targets":           snap.Targets,
		"counters":          snap.Counters,
		"current_bytes":     cost.CurrentRemoteBytes(snap),
		"free_tier_class_a": cost.StandardFreeClassA,
		"free_tier_class_b": cost.StandardFreeClassB,
		"recent_events":     snap.RecentEvents,
	})
}

func (s *Server) handleGetConfig(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.currentConfig().Public())
}

func (s *Server) handlePutConfig(w http.ResponseWriter, r *http.Request) {
	var body struct {
		BaseDir           *string  `json:"base_dir"`
		StateDir          *string  `json:"state_dir"`
		ListenAddr        *string  `json:"listen_addr"`
		SyncMethod        *string  `json:"sync_method"`
		BucketName        *string  `json:"bucket_name"`
		AccountID         *string  `json:"account_id"`
		CloudflareToken   *string  `json:"cloudflare_token"`
		ObjectPrefix      *string  `json:"object_prefix"`
		GitHubRepo        *string  `json:"github_repo"`
		GitHubToken       *string  `json:"github_token"`
		GitHubBranch      *string  `json:"github_branch"`
		RepoDir           *string  `json:"repo_dir"`
		SyncInterval      *string  `json:"sync_interval"`
		StorageCapBytes   *int64   `json:"storage_cap_bytes"`
		ClassAWarnRatio   *float64 `json:"class_a_warn_ratio"`
		ClassABlockRatio  *float64 `json:"class_a_block_ratio"`
		ClassBWarnRatio   *float64 `json:"class_b_warn_ratio"`
		ClassBBlockRatio  *float64 `json:"class_b_block_ratio"`
		StrictVerify      *bool    `json:"strict_verify"`
		DisableCostGuards *bool    `json:"disable_cost_guards"`
		ConfirmRisk       string   `json:"confirm_risk"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "invalid JSON body")
		return
	}
	s.cfgMu.Lock()
	cfg := s.cfg
	if body.BaseDir != nil {
		cfg.BaseDir = strings.TrimSpace(*body.BaseDir)
	}
	if body.StateDir != nil {
		cfg.StateDir = strings.TrimSpace(*body.StateDir)
	}
	if body.ListenAddr != nil {
		cfg.ListenAddr = strings.TrimSpace(*body.ListenAddr)
	}
	if body.BucketName != nil {
		cfg.BucketName = strings.TrimSpace(*body.BucketName)
	}
	if body.AccountID != nil {
		cfg.AccountID = strings.TrimSpace(*body.AccountID)
	}
	if body.CloudflareToken != nil && strings.TrimSpace(*body.CloudflareToken) != "" && strings.TrimSpace(*body.CloudflareToken) != "********" {
		cfg.CloudflareToken = strings.TrimSpace(*body.CloudflareToken)
	}
	if body.ObjectPrefix != nil {
		cfg.ObjectPrefix = strings.TrimSpace(*body.ObjectPrefix)
	}
	if body.SyncMethod != nil {
		cfg.SyncMethod = strings.TrimSpace(*body.SyncMethod)
	}
	if body.GitHubRepo != nil {
		cfg.GitHubRepo = strings.TrimSpace(*body.GitHubRepo)
	}
	if body.GitHubToken != nil && strings.TrimSpace(*body.GitHubToken) != "" && strings.TrimSpace(*body.GitHubToken) != "********" {
		cfg.GitHubToken = strings.TrimSpace(*body.GitHubToken)
	}
	if body.GitHubBranch != nil {
		cfg.GitHubBranch = strings.TrimSpace(*body.GitHubBranch)
	}
	if body.RepoDir != nil {
		cfg.RepoDir = strings.TrimSpace(*body.RepoDir)
	}
	if body.SyncInterval != nil {
		cfg.SyncIntervalText = strings.TrimSpace(*body.SyncInterval)
	}
	if body.StorageCapBytes != nil {
		if *body.StorageCapBytes > cfg.StorageCapBytes && body.ConfirmRisk != "CONFIRM" {
			s.cfgMu.Unlock()
			writeError(w, http.StatusBadRequest, "confirmation_required", "increasing storage cap requires CONFIRM")
			return
		}
		cfg.StorageCapBytes = *body.StorageCapBytes
	}
	if body.ClassAWarnRatio != nil {
		cfg.ClassAWarnRatio = *body.ClassAWarnRatio
	}
	if body.ClassABlockRatio != nil {
		cfg.ClassABlockRatio = *body.ClassABlockRatio
	}
	if body.ClassBWarnRatio != nil {
		cfg.ClassBWarnRatio = *body.ClassBWarnRatio
	}
	if body.ClassBBlockRatio != nil {
		cfg.ClassBBlockRatio = *body.ClassBBlockRatio
	}
	if body.StrictVerify != nil {
		cfg.StrictVerify = *body.StrictVerify
	}
	if body.DisableCostGuards != nil {
		if *body.DisableCostGuards && body.ConfirmRisk != "CONFIRM" {
			s.cfgMu.Unlock()
			writeError(w, http.StatusBadRequest, "confirmation_required", "disabling cost guards requires CONFIRM")
			return
		}
		cfg.DisableCostGuards = *body.DisableCostGuards
	}
	if err := cfg.Normalize(); err != nil {
		s.cfgMu.Unlock()
		writeError(w, http.StatusBadRequest, "invalid_config", err.Error())
		return
	}
	if err := cfg.Save(); err != nil {
		s.cfgMu.Unlock()
		writeError(w, http.StatusInternalServerError, "save_failed", err.Error())
		return
	}
	s.cfg = cfg
	s.cfgMu.Unlock()
	s.notifyConfigChange(cfg)
	writeJSON(w, http.StatusOK, cfg.Public())
}

func (s *Server) handleTargets(w http.ResponseWriter, r *http.Request) {
	cfg := s.currentConfig()
	writeJSON(w, http.StatusOK, map[string]any{"targets": cfg.Targets, "excludes": cfg.Excludes})
}

func (s *Server) handlePutTargets(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Targets  []string `json:"targets"`
		Excludes []string `json:"excludes"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "invalid JSON body")
		return
	}
	s.cfgMu.Lock()
	cfg := s.cfg
	cfg.Targets = body.Targets
	cfg.Excludes = body.Excludes
	if err := cfg.Normalize(); err != nil {
		s.cfgMu.Unlock()
		writeError(w, http.StatusBadRequest, "invalid_config", err.Error())
		return
	}
	if err := cfg.Save(); err != nil {
		s.cfgMu.Unlock()
		writeError(w, http.StatusInternalServerError, "save_failed", err.Error())
		return
	}
	s.cfg = cfg
	s.cfgMu.Unlock()
	s.notifyConfigChange(cfg)
	writeJSON(w, http.StatusOK, map[string]any{"targets": cfg.Targets, "excludes": cfg.Excludes})
}

func (s *Server) handleSync(w http.ResponseWriter, r *http.Request) {
	result, err := s.runSync(r.Context(), syncer.ModeManual)
	if err != nil {
		writeSyncError(w, "sync_failed", err)
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func (s *Server) handleVerify(w http.ResponseWriter, r *http.Request) {
	result, err := s.runSync(r.Context(), syncer.ModeVerify)
	if err != nil {
		writeSyncError(w, "verify_failed", err)
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func writeSyncError(w http.ResponseWriter, code string, err error) {
	switch {
	case errors.Is(err, syncer.ErrSyncBusy):
		writeError(w, http.StatusConflict, "sync_busy", err.Error())
	case errors.Is(err, config.ErrMissingCloudflareConfig), errors.Is(err, config.ErrMissingGitHubConfig):
		writeError(w, http.StatusBadRequest, "not_configured", err.Error())
	default:
		writeError(w, http.StatusInternalServerError, code, err.Error())
	}
}

func (s *Server) handleDelete(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Target  string `json:"target"`
		Confirm string `json:"confirm"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "invalid JSON body")
		return
	}
	cfg := s.currentConfig()
	runner, err := s.runnerFactory(r.Context(), cfg)
	if err != nil {
		writeError(w, http.StatusBadRequest, "remote_unavailable", err.Error())
		return
	}
	if err := runner.DeleteRemote(r.Context(), body.Target, body.Confirm); err != nil {
		writeError(w, http.StatusBadRequest, "delete_failed", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) runSync(ctx context.Context, mode syncer.Mode) (syncer.Result, error) {
	cfg := s.currentConfig()
	if err := cfg.ValidateSyncConfig(); err != nil {
		return syncer.Result{}, err
	}
	runner, err := s.runnerFactory(ctx, cfg)
	if err != nil {
		return syncer.Result{}, err
	}
	if mode == syncer.ModeVerify {
		return runner.Verify(ctx)
	}
	return runner.Sync(ctx, mode)
}

func (s *Server) currentConfig() config.Config {
	s.cfgMu.RLock()
	defer s.cfgMu.RUnlock()
	return s.cfg
}

func (s *Server) notifyConfigChange(cfg config.Config) {
	if s.OnConfigChange != nil {
		s.OnConfigChange(cfg)
	}
}

func (s *Server) sessionCookie(r *http.Request, value string, ttl time.Duration) *http.Cookie {
	cookie := &http.Cookie{
		Name:     sessionCookieName,
		Value:    value,
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Secure:   r.TLS != nil,
	}
	if ttl > 0 {
		cookie.Expires = time.Now().Add(ttl)
	}
	return cookie
}

func (s *Server) pruneSessionsLocked() {
	now := time.Now()
	for token, expires := range s.sessions {
		if now.After(expires) {
			delete(s.sessions, token)
		}
	}
}

func (s *Server) withAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		cookie, err := r.Cookie(sessionCookieName)
		if err != nil || cookie.Value == "" {
			writeError(w, http.StatusUnauthorized, "unauthorized", "login required")
			return
		}
		s.sessionMu.Lock()
		expires, ok := s.sessions[cookie.Value]
		if !ok || time.Now().After(expires) {
			delete(s.sessions, cookie.Value)
			ok = false
		}
		s.sessionMu.Unlock()
		if !ok {
			writeError(w, http.StatusUnauthorized, "unauthorized", "login required")
			return
		}
		next(w, r)
	}
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func writeError(w http.ResponseWriter, status int, code, message string) {
	writeJSON(w, status, map[string]any{
		"ok":      false,
		"code":    code,
		"message": message,
	})
}
