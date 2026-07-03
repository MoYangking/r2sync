package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"

	"r2sync/internal/config"
	"r2sync/internal/r2"
	"r2sync/internal/server"
	"r2sync/internal/state"
	"r2sync/internal/syncer"
)

func main() {
	os.Exit(run(os.Args[1:]))
}

func run(args []string) int {
	if len(args) == 0 || args[0] == "--help" || args[0] == "-h" {
		printHelp()
		return 0
	}
	log := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	switch args[0] {
	case "serve":
		if err := serve(ctx, log); err != nil {
			log.Error("serve failed", "error", err)
			return 1
		}
	case "sync":
		if err := syncOnce(ctx, log); err != nil {
			log.Error("sync failed", "error", err)
			return 1
		}
	case "run":
		code, err := runCommand(ctx, log, args[1:])
		if err != nil {
			log.Error("run failed", "error", err)
			return 1
		}
		return code
	case "config":
		if len(args) >= 2 && args[1] == "check" {
			if err := configCheck(ctx, log); err != nil {
				log.Error("config check failed", "error", err)
				return 1
			}
			return 0
		}
		printHelp()
		return 1
	default:
		printHelp()
		return 1
	}
	return 0
}

func printHelp() {
	fmt.Println(`r2sync - Cloudflare R2 file sync

Usage:
  r2sync serve
  r2sync sync
  r2sync run -- <command> [args...]
  r2sync config check

Environment:
  R2SYNC_BUCKET, R2SYNC_TOKEN, R2SYNC_ACCOUNT_ID, R2SYNC_TARGETS,
  R2SYNC_STATE_DIR, R2SYNC_BASE_DIR, R2SYNC_ADMIN_PASSWORD`)
}

func loadRuntime(log *slog.Logger) (config.Config, *state.Store, error) {
	cfg, err := config.Load()
	if err != nil {
		return config.Config{}, nil, err
	}
	statePath := filepath.Join(cfg.StateDir, config.DefaultStateFileName)
	st, err := state.Open(statePath)
	if err != nil {
		return config.Config{}, nil, err
	}
	if err := server.EnsureSecurity(&cfg, st, log); err != nil {
		return config.Config{}, nil, err
	}
	return cfg, st, nil
}

func remoteFactory(ctx context.Context, cfg config.Config) (r2.ObjectStore, error) {
	setup, err := r2.Setup(ctx, cfg)
	if err != nil {
		return nil, err
	}
	return setup.Store, nil
}

func setupRemote(ctx context.Context, cfg config.Config) (r2.ObjectStore, error) {
	setup, err := r2.Setup(ctx, cfg)
	if err != nil {
		return nil, err
	}
	return setup.Store, nil
}

func serve(ctx context.Context, log *slog.Logger) error {
	cfg, st, err := loadRuntime(log)
	if err != nil {
		return err
	}
	srv := server.New(cfg, st, log, remoteFactory)
	if cfg.HasCloudflareConfig() {
		remote, err := setupRemote(ctx, cfg)
		if err != nil {
			log.Error("R2 setup failed; management UI remains available", "error", err)
		} else {
			s := syncer.New(cfg, st, remote)
			if _, err := s.InitialSync(ctx); err != nil {
				log.Error("initial sync failed; management UI remains available", "error", err)
			} else {
				go (&syncer.Scheduler{Syncer: s, Log: log}).Run(ctx)
			}
		}
	} else {
		log.Warn("R2 config is incomplete; open management UI to configure bucket and token")
	}
	return srv.ListenAndServe(ctx)
}

func syncOnce(ctx context.Context, log *slog.Logger) error {
	cfg, st, err := loadRuntime(log)
	if err != nil {
		return err
	}
	remote, err := setupRemote(ctx, cfg)
	if err != nil {
		return err
	}
	_, err = syncer.New(cfg, st, remote).ManualSync(ctx)
	return err
}

func configCheck(ctx context.Context, log *slog.Logger) error {
	cfg, _, err := loadRuntime(log)
	if err != nil {
		return err
	}
	setup, err := r2.Setup(ctx, cfg)
	if err != nil {
		return err
	}
	log.Info("configuration is valid", "bucket", cfg.BucketName, "account_id", setup.AccountID)
	return nil
}

func runCommand(ctx context.Context, log *slog.Logger, args []string) (int, error) {
	if len(args) > 0 && args[0] == "--" {
		args = args[1:]
	}
	if len(args) == 0 {
		return 1, fmt.Errorf("missing command after --")
	}
	cfg, st, err := loadRuntime(log)
	if err != nil {
		return 1, err
	}
	remote, err := setupRemote(ctx, cfg)
	if err != nil {
		return 1, err
	}
	s := syncer.New(cfg, st, remote)
	if _, err := s.InitialSync(ctx); err != nil {
		return 1, err
	}
	childCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	go (&syncer.Scheduler{Syncer: s, Log: log}).Run(childCtx)
	go func() {
		srv := server.New(cfg, st, log, remoteFactory)
		if err := srv.ListenAndServe(childCtx); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Error("management server failed", "error", err)
		}
	}()
	cmd := exec.CommandContext(ctx, args[0], args[1:]...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Stdin = os.Stdin
	cmd.Env = os.Environ()
	log.Info("starting command after initial sync", "command", strings.Join(args, " "))
	err = cmd.Run()
	cancel()
	if err == nil {
		return 0, nil
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return exitErr.ExitCode(), nil
	}
	return 1, err
}
