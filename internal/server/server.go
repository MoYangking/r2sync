package server

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"r2sync/internal/config"
	"r2sync/internal/cost"
	"r2sync/internal/r2"
	"r2sync/internal/state"
	"r2sync/internal/syncer"
	"r2sync/internal/ui"
)

type RemoteFactory func(context.Context, config.Config) (r2.ObjectStore, error)

type Server struct {
	cfgMu         sync.RWMutex
	cfg           config.Config
	store         *state.Store
	log           *slog.Logger
	remoteFactory RemoteFactory

	sessionMu sync.Mutex
	sessions  map[string]time.Time
}

func New(cfg config.Config, store *state.Store, log *slog.Logger, factory RemoteFactory) *Server {
	if log == nil {
		log = slog.Default()
	}
	return &Server{
		cfg:           cfg,
		store:         store,
		log:           log,
		remoteFactory: factory,
		sessions:      map[string]time.Time{},
	}
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/health", s.handleHealth)
	mux.HandleFunc("GET /api/ready", s.handleReady)
	mux.HandleFunc("POST /api/login", s.handleLogin)
	mux.HandleFunc("POST /api/logout", s.withAuth(s.handleLogout))
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
	return mux
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
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) handleReady(w http.ResponseWriter, r *http.Request) {
	snap := s.store.Snapshot()
	writeJSON(w, http.StatusOK, map[string]any{"ready": snap.Status.Ready, "status": snap.Status})
}

func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Password string `json:"password"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "invalid JSON body")
		return
	}
	hash := s.store.Snapshot().AdminPasswordHash
	if !checkPassword(hash, body.Password) {
		writeError(w, http.StatusUnauthorized, "unauthorized", "invalid password")
		return
	}
	token, err := randomSecret(32)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "session_error", err.Error())
		return
	}
	s.sessionMu.Lock()
	s.sessions[token] = time.Now().Add(24 * time.Hour)
	s.sessionMu.Unlock()
	http.SetCookie(w, &http.Cookie{
		Name:     "r2sync_session",
		Value:    token,
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Expires:  time.Now().Add(24 * time.Hour),
	})
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	if cookie, err := r.Cookie("r2sync_session"); err == nil {
		s.sessionMu.Lock()
		delete(s.sessions, cookie.Value)
		s.sessionMu.Unlock()
	}
	http.SetCookie(w, &http.Cookie{Name: "r2sync_session", Value: "", Path: "/", MaxAge: -1, HttpOnly: true})
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) handleProgress(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.store.Snapshot().Status)
}

func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	cfg := s.currentConfig()
	snap := s.store.Snapshot()
	writeJSON(w, http.StatusOK, map[string]any{
		"config":        cfg.Public(),
		"status":        snap.Status,
		"targets":       snap.Targets,
		"counters":      snap.Counters,
		"current_bytes": cost.CurrentRemoteBytes(snap),
		"recent_events": snap.RecentEvents,
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
		BucketName        *string  `json:"bucket_name"`
		AccountID         *string  `json:"account_id"`
		CloudflareToken   *string  `json:"cloudflare_token"`
		ObjectPrefix      *string  `json:"object_prefix"`
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
	writeJSON(w, http.StatusOK, map[string]any{"targets": cfg.Targets, "excludes": cfg.Excludes})
}

func (s *Server) handleSync(w http.ResponseWriter, r *http.Request) {
	result, err := s.runSync(r.Context(), syncer.ModeManual)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "sync_failed", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func (s *Server) handleVerify(w http.ResponseWriter, r *http.Request) {
	result, err := s.runSync(r.Context(), syncer.ModeVerify)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "verify_failed", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, result)
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
	remote, err := s.remoteFactory(r.Context(), cfg)
	if err != nil {
		writeError(w, http.StatusBadRequest, "remote_unavailable", err.Error())
		return
	}
	if err := syncer.New(cfg, s.store, remote).DeleteRemote(r.Context(), body.Target, body.Confirm); err != nil {
		writeError(w, http.StatusBadRequest, "delete_failed", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) runSync(ctx context.Context, mode syncer.Mode) (syncer.Result, error) {
	cfg := s.currentConfig()
	remote, err := s.remoteFactory(ctx, cfg)
	if err != nil {
		return syncer.Result{}, err
	}
	sync := syncer.New(cfg, s.store, remote)
	if mode == syncer.ModeVerify {
		return sync.Verify(ctx)
	}
	return sync.Sync(ctx, mode)
}

func (s *Server) currentConfig() config.Config {
	s.cfgMu.RLock()
	defer s.cfgMu.RUnlock()
	return s.cfg
}

func (s *Server) withAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		cookie, err := r.Cookie("r2sync_session")
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
