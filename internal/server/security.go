package server

import (
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"sync"
	"time"

	"r2sync/internal/config"
	"r2sync/internal/state"

	"golang.org/x/crypto/bcrypt"
)

const (
	loginFailWindow    = 15 * time.Minute
	loginFailThreshold = 5
	loginBlockBase     = 30 * time.Second
	loginBlockMax      = 16 * time.Minute
)

// loginLimiter throttles password guessing per client IP: after
// loginFailThreshold failures inside loginFailWindow, further attempts are
// blocked for an exponentially growing duration.
type loginLimiter struct {
	mu    sync.Mutex
	fails map[string]*failRecord
}

type failRecord struct {
	count        int
	lastFailure  time.Time
	blockedUntil time.Time
}

func (l *loginLimiter) blockedFor(ip string) time.Duration {
	l.mu.Lock()
	defer l.mu.Unlock()
	rec, ok := l.fails[ip]
	if !ok {
		return 0
	}
	if wait := time.Until(rec.blockedUntil); wait > 0 {
		return wait
	}
	return 0
}

func (l *loginLimiter) recordFailure(ip string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.fails == nil {
		l.fails = map[string]*failRecord{}
	}
	l.pruneLocked()
	rec := l.fails[ip]
	if rec == nil || time.Since(rec.lastFailure) > loginFailWindow {
		rec = &failRecord{}
		l.fails[ip] = rec
	}
	rec.count++
	rec.lastFailure = time.Now()
	if rec.count >= loginFailThreshold {
		block := loginBlockBase << uint(min(rec.count-loginFailThreshold, 5))
		if block > loginBlockMax {
			block = loginBlockMax
		}
		rec.blockedUntil = time.Now().Add(block)
	}
}

func (l *loginLimiter) recordSuccess(ip string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.fails, ip)
}

func (l *loginLimiter) pruneLocked() {
	cutoff := time.Now().Add(-loginFailWindow)
	for ip, rec := range l.fails {
		if rec.lastFailure.Before(cutoff) && time.Now().After(rec.blockedUntil) {
			delete(l.fails, ip)
		}
	}
}

func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

func EnsureSecurity(cfg *config.Config, store *state.Store, log *slog.Logger) error {
	snap := store.Snapshot()
	changed := false
	password := os.Getenv("R2SYNC_ADMIN_PASSWORD")
	hash := snap.AdminPasswordHash
	if hash == "" && cfg.AdminPasswordHash != "" {
		hash = cfg.AdminPasswordHash
	}
	if hash == "" {
		if password == "" {
			var err error
			password, err = randomSecret(18)
			if err != nil {
				return err
			}
			if log != nil {
				log.Warn("generated initial admin password; save it now", "password", password)
			}
		}
		b, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
		if err != nil {
			return fmt.Errorf("hash admin password: %w", err)
		}
		hash = string(b)
		changed = true
	}
	sessionKey := snap.SessionSigningKey
	if sessionKey == "" {
		var err error
		sessionKey, err = randomSecret(32)
		if err != nil {
			return err
		}
		changed = true
	}
	if changed {
		return store.Update(func(d *state.Data) error {
			d.AdminPasswordHash = hash
			d.SessionSigningKey = sessionKey
			return nil
		})
	}
	return nil
}

func checkPassword(hash, password string) bool {
	if hash == "" || password == "" {
		return false
	}
	return bcrypt.CompareHashAndPassword([]byte(hash), []byte(password)) == nil
}

func randomSecret(bytesLen int) (string, error) {
	b := make([]byte, bytesLen)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("generate random secret: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}
