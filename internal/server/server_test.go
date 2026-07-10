package server

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"r2sync/internal/config"
	"r2sync/internal/state"

	"golang.org/x/crypto/bcrypt"
)

const testPassword = "test-password-123"

func newTestServer(t *testing.T) (*Server, *httptest.Server) {
	t.Helper()
	base := t.TempDir()
	cfg := config.Defaults()
	cfg.BaseDir = base
	cfg.StateDir = filepath.Join(base, ".r2sync")
	if err := cfg.Normalize(); err != nil {
		t.Fatal(err)
	}
	st, err := state.Open(filepath.Join(cfg.StateDir, config.DefaultStateFileName))
	if err != nil {
		t.Fatal(err)
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(testPassword), bcrypt.MinCost)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Update(func(d *state.Data) error {
		d.AdminPasswordHash = string(hash)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	srv := New(cfg, st, slog.New(slog.NewTextHandler(io.Discard, nil)), nil)
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return srv, ts
}

func postJSON(t *testing.T, url string, body any, cookie *http.Cookie) *http.Response {
	t.Helper()
	data, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	if cookie != nil {
		req.AddCookie(cookie)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { res.Body.Close() })
	return res
}

func login(t *testing.T, ts *httptest.Server, password string) (*http.Response, *http.Cookie) {
	t.Helper()
	res := postJSON(t, ts.URL+"/api/login", map[string]string{"password": password}, nil)
	for _, c := range res.Cookies() {
		if c.Name == sessionCookieName && c.Value != "" {
			return res, c
		}
	}
	return res, nil
}

func getWithCookie(t *testing.T, url string, cookie *http.Cookie) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	if cookie != nil {
		req.AddCookie(cookie)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { res.Body.Close() })
	return res
}

func TestLoginAndAuthGate(t *testing.T) {
	_, ts := newTestServer(t)

	if res := getWithCookie(t, ts.URL+"/api/status", nil); res.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unauthenticated status = %d, want 401", res.StatusCode)
	}

	if res, _ := login(t, ts, "wrong-password"); res.StatusCode != http.StatusUnauthorized {
		t.Fatalf("wrong password login = %d, want 401", res.StatusCode)
	}

	res, cookie := login(t, ts, testPassword)
	if res.StatusCode != http.StatusOK || cookie == nil {
		t.Fatalf("login = %d, cookie = %v", res.StatusCode, cookie)
	}
	if res := getWithCookie(t, ts.URL+"/api/status", cookie); res.StatusCode != http.StatusOK {
		t.Fatalf("authenticated status = %d, want 200", res.StatusCode)
	}
}

func TestLoginRateLimit(t *testing.T) {
	_, ts := newTestServer(t)
	for i := 0; i < loginFailThreshold; i++ {
		if res, _ := login(t, ts, "wrong-password"); res.StatusCode != http.StatusUnauthorized {
			t.Fatalf("attempt %d = %d, want 401", i+1, res.StatusCode)
		}
	}
	res, _ := login(t, ts, testPassword)
	if res.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("post-threshold login = %d, want 429", res.StatusCode)
	}
	if res.Header.Get("Retry-After") == "" {
		t.Fatal("expected Retry-After header on 429")
	}
}

func TestPasswordChangeRevokesOtherSessions(t *testing.T) {
	_, ts := newTestServer(t)
	_, sessionA := login(t, ts, testPassword)
	_, sessionB := login(t, ts, testPassword)
	if sessionA == nil || sessionB == nil {
		t.Fatal("expected two sessions")
	}

	res := postJSON(t, ts.URL+"/api/password", map[string]string{
		"current_password": testPassword,
		"new_password":     "brand-new-password",
	}, sessionA)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("password change = %d, want 200", res.StatusCode)
	}

	if res := getWithCookie(t, ts.URL+"/api/status", sessionA); res.StatusCode != http.StatusOK {
		t.Fatalf("current session = %d, want 200", res.StatusCode)
	}
	if res := getWithCookie(t, ts.URL+"/api/status", sessionB); res.StatusCode != http.StatusUnauthorized {
		t.Fatalf("other session = %d, want 401", res.StatusCode)
	}

	if res, _ := login(t, ts, testPassword); res.StatusCode != http.StatusUnauthorized {
		t.Fatalf("old password = %d, want 401", res.StatusCode)
	}
	if res, _ := login(t, ts, "brand-new-password"); res.StatusCode != http.StatusOK {
		t.Fatalf("new password = %d, want 200", res.StatusCode)
	}
}

func TestPasswordChangeValidation(t *testing.T) {
	_, ts := newTestServer(t)
	_, cookie := login(t, ts, testPassword)

	res := postJSON(t, ts.URL+"/api/password", map[string]string{
		"current_password": "wrong",
		"new_password":     "brand-new-password",
	}, cookie)
	if res.StatusCode != http.StatusUnauthorized {
		t.Fatalf("wrong current password = %d, want 401", res.StatusCode)
	}

	res = postJSON(t, ts.URL+"/api/password", map[string]string{
		"current_password": testPassword,
		"new_password":     "short",
	}, cookie)
	if res.StatusCode != http.StatusBadRequest {
		t.Fatalf("weak password = %d, want 400", res.StatusCode)
	}
}

func TestSecurityHeaders(t *testing.T) {
	_, ts := newTestServer(t)
	res := getWithCookie(t, ts.URL+"/api/health", nil)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("health = %d", res.StatusCode)
	}
	for header, want := range map[string]string{
		"X-Content-Type-Options": "nosniff",
		"X-Frame-Options":        "DENY",
		"Cache-Control":          "no-store",
	} {
		if got := res.Header.Get(header); got != want {
			t.Fatalf("%s = %q, want %q", header, got, want)
		}
	}
	if res.Header.Get("Content-Security-Policy") == "" {
		t.Fatal("missing Content-Security-Policy")
	}
}
