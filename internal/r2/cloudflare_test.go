package r2

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestVerifyTokenUsesAccountScopedEndpoint(t *testing.T) {
	t.Parallel()
	var paths []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		switch r.URL.Path {
		case "/accounts/acct/tokens/verify":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"success":true,"result":{"id":"token-id"}}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	client := NewCloudflareClient("token")
	client.baseURL = server.URL

	info, err := client.VerifyToken(context.Background(), "acct")
	if err != nil {
		t.Fatalf("VerifyToken returned error: %v", err)
	}
	if info.ID != "token-id" {
		t.Fatalf("token id = %q", info.ID)
	}
	if len(paths) != 1 || paths[0] != "/accounts/acct/tokens/verify" {
		t.Fatalf("requested paths = %#v", paths)
	}
}

func TestVerifyTokenRequiresAccountID(t *testing.T) {
	t.Parallel()
	client := NewCloudflareClient("token")
	if _, err := client.VerifyToken(context.Background(), ""); err == nil {
		t.Fatalf("VerifyToken returned nil error without account id")
	}
}
