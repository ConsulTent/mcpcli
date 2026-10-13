package auth

import "testing"

func TestResolveCredentials(t *testing.T) {
	t.Run("flag beats env", func(t *testing.T) {
		t.Setenv("MCPCLI_CLIENT_ID", "env-client-id")
		t.Setenv("MCPCLI_CLIENT_SECRET", "env-client-secret")
		got := ResolveCredentials("flag-client-id", "flag-client-secret")
		if got.ClientID != "flag-client-id" || got.ClientSecret != "flag-client-secret" {
			t.Errorf("flags must win over env, got %+v", got)
		}
	})

	t.Run("env only", func(t *testing.T) {
		t.Setenv("MCPCLI_CLIENT_ID", "env-client-id")
		t.Setenv("MCPCLI_CLIENT_SECRET", "env-client-secret")
		got := ResolveCredentials("", "")
		if got.ClientID != "env-client-id" || got.ClientSecret != "env-client-secret" {
			t.Errorf("empty flags must fall back to env, got %+v", got)
		}
	})

	t.Run("fields resolve independently", func(t *testing.T) {
		t.Setenv("MCPCLI_CLIENT_SECRET", "env-client-secret")
		got := ResolveCredentials("flag-client-id", "")
		if got.ClientID != "flag-client-id" || got.ClientSecret != "env-client-secret" {
			t.Errorf("flag id + env secret expected, got %+v", got)
		}
	})

	t.Run("neither set", func(t *testing.T) {
		t.Setenv("MCPCLI_CLIENT_ID", "")
		t.Setenv("MCPCLI_CLIENT_SECRET", "")
		got := ResolveCredentials("", "")
		if got.ClientID != "" || got.ClientSecret != "" {
			t.Errorf("expected empty credentials, got %+v", got)
		}
	})
}
