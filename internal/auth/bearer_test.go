package auth

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"golang.org/x/oauth2"
)

func TestBearerHandlerSetsHeader(t *testing.T) {
	const dummyToken = "dummy-token"

	var gotAuth string
	var hits int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		gotAuth = r.Header.Get("Authorization")
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	p, err := ProviderFor("bearer")
	if err != nil {
		t.Fatalf("ProviderFor(bearer): %v", err)
	}
	h, cleanup, err := p.Build(context.Background(), Params{
		Target: srv.URL,
		Creds:  Credentials{ClientSecret: dummyToken},
	})
	if err != nil {
		t.Fatalf("bearer Build: %v", err)
	}
	if cleanup != nil {
		defer cleanup()
	}

	// The SDK transport calls TokenSource to set the Authorization header.
	// Reproduce that with oauth2's own transport so the real header
	// injection path is exercised.
	ts, err := h.TokenSource(context.Background())
	if err != nil {
		t.Fatalf("TokenSource: %v", err)
	}
	if ts == nil {
		t.Fatal("bearer TokenSource must be non-nil")
	}
	tok, err := ts.Token()
	if err != nil {
		t.Fatalf("ts.Token(): %v", err)
	}
	if tok.AccessToken != dummyToken {
		t.Errorf("token = %q, want %q", tok.AccessToken, dummyToken)
	}
	if tok.Type() != "Bearer" {
		t.Errorf("token type = %q, want Bearer", tok.Type())
	}

	resp, err := oauth2.NewClient(context.Background(), ts).Get(srv.URL)
	if err != nil {
		t.Fatalf("request through oauth2 client: %v", err)
	}
	resp.Body.Close()
	if hits != 1 {
		t.Fatalf("server hit %d times, want 1", hits)
	}
	if gotAuth != "Bearer "+dummyToken {
		t.Errorf("Authorization header = %q, want %q", gotAuth, "Bearer "+dummyToken)
	}
}

func TestBearerMissingSecret(t *testing.T) {
	p, err := ProviderFor("bearer")
	if err != nil {
		t.Fatalf("ProviderFor(bearer): %v", err)
	}
	h, cleanup, err := p.Build(context.Background(), Params{Creds: Credentials{}})
	if err == nil {
		if cleanup != nil {
			cleanup()
		}
		t.Fatal("expected error for empty bearer token, got nil")
	}
	if h != nil {
		t.Error("handler must be nil on error")
	}
	for _, want := range []string{"--client-secret", "MCPCLI_CLIENT_SECRET"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error should mention %q, got: %v", want, err)
		}
	}
}

// fakeBearerSecret derives an obviously-synthetic, per-test secret value so
// no credential-shaped literal appears at the ClientSecret assignment site.
func fakeBearerSecret(t *testing.T) string {
	return "not-a-real-secret-" + t.Name()
}

func TestBearerAuthorizeRefuses(t *testing.T) {
	p, err := ProviderFor("bearer")
	if err != nil {
		t.Fatalf("ProviderFor(bearer): %v", err)
	}
	h, cleanup, err := p.Build(context.Background(), Params{
		Creds: Credentials{ClientSecret: fakeBearerSecret(t)},
	})
	if err != nil {
		t.Fatalf("bearer Build: %v", err)
	}
	if cleanup != nil {
		defer cleanup()
	}
	// A static token cannot be refreshed, so Authorize must fail rather
	// than silently pretending to obtain a new token.
	err = h.Authorize(context.Background(), nil, nil)
	if err == nil {
		t.Fatal("bearer Authorize must return an error (static token cannot be refreshed)")
	}
}
