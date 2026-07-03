package server

import (
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"log/slog"
	"os"

	"r2sync/internal/config"
	"r2sync/internal/state"

	"golang.org/x/crypto/bcrypt"
)

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
