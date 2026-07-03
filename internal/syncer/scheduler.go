package syncer

import (
	"context"
	"log/slog"
	"sync"
	"time"
)

type Scheduler struct {
	Syncer *Syncer
	Log    *slog.Logger

	mu      sync.Mutex
	running bool
}

func (s *Scheduler) Run(ctx context.Context) {
	if s.Syncer == nil {
		return
	}
	interval := s.Syncer.Config.SyncInterval
	if interval <= 0 {
		interval = 5 * time.Hour
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.runOnce(ctx)
		}
	}
}

func (s *Scheduler) runOnce(ctx context.Context) {
	s.mu.Lock()
	if s.running {
		s.mu.Unlock()
		return
	}
	s.running = true
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		s.running = false
		s.mu.Unlock()
	}()
	if _, err := s.Syncer.ScheduledSync(ctx); err != nil && s.Log != nil {
		s.Log.Error("scheduled sync failed", "error", err)
	}
}
