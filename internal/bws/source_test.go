package bws

import (
	"strings"
	"testing"

	"github.com/ConsulTent/mcpcli/internal/secret"
)

// TestSourceScheme pins the Source's registration contract.
func TestSourceScheme(t *testing.T) {
	if got := (NewSource()).Scheme(); got != "bws" {
		t.Errorf("Scheme() = %q, want %q", got, "bws")
	}
}

// TestSourceResolveThroughResolver runs the real bws Source behind the real
// secret.Resolver: dispatch (including the uppercase BWS:// spelling),
// uuid and name refs, once-per-run exchange, and the NUL guard exercised
// through the expansion path — over both instance base forms.
func TestSourceResolveThroughResolver(t *testing.T) {
	for _, form := range baseForms {
		t.Run(form.name, func(t *testing.T) {
			useTempTokenDir(t)
			tok, _, _, _ := dummyAccessToken(t)
			orgKey := testOrgKey(t)
			orgID := "01234567-89ab-cdef-0123-456789abcdef"
			uuidRef := "11111111-2222-3333-4444-555555555555"
			nulRef := "22222222-2222-3333-4444-555555555555"
			fa := newFakeAPI(t, form.apiMount, orgKey, map[string]Secret{
				uuidRef: wireSecret(t, orgKey, Secret{ID: uuidRef, OrganizationID: orgID, Key: "jev", Value: "the-jev-provider-key", CanRead: true, CanWrite: true}),
				nulRef:  wireSecret(t, orgKey, Secret{ID: nulRef, OrganizationID: orgID, Key: "nul-key", Value: "has\x00nul", CanRead: true}),
			})
			fi := newFakeIdentity(t, form.identMount, tok, orgKey)
			if err := SaveCredential(Credential{
				APIToken:    tok,
				OrgID:       orgID,
				APIURL:      fa.url,
				IdentityURL: fi.url,
			}); err != nil {
				t.Fatalf("SaveCredential: %v", err)
			}

			res, err := secret.New(NewSource())
			if err != nil {
				t.Fatalf("secret.New: %v", err)
			}

			// Uppercase scheme reaches the bws source (RFC 3986).
			got, err := res.Expand(t.Context(), "BWS://"+uuidRef)
			if err != nil {
				t.Fatalf("Expand(BWS://uuid): %v", err)
			}
			if got != "the-jev-provider-key" {
				t.Errorf("Expand = %q, want the decrypted value", got)
			}

			// Lowercase spelling, same source instance.
			if got, err := res.Expand(t.Context(), "bws://"+uuidRef); err != nil || got != "the-jev-provider-key" {
				t.Errorf("Expand(bws://uuid) = %q, %v", got, err)
			}

			// Name ref rides the org list + decrypted-key match.
			got, err = res.Expand(t.Context(), "bws://jev")
			if err != nil {
				t.Fatalf("Expand(bws://name): %v", err)
			}
			if got != "the-jev-provider-key" {
				t.Errorf("Expand by name = %q, want the decrypted value", got)
			}

			// Once per run: three Expands, one identity exchange.
			if n := fi.exchangeCount(); n != 1 {
				t.Errorf("exchange count after 3 Expands = %d, want 1", n)
			}

			// NUL-bearing value rejected naming the KEY.
			_, err = res.Expand(t.Context(), "bws://"+nulRef)
			if err == nil {
				t.Fatal("NUL-bearing value accepted through Expand")
			}
			if !strings.Contains(err.Error(), "nul-key") {
				t.Errorf("error %q does not name the decrypted KEY", err)
			}
		})
	}
}

// TestSourceResolveExchange401 pins the runtime 401 unioned message
// surfacing through the expansion path.
func TestSourceResolveExchange401(t *testing.T) {
	useTempTokenDir(t)
	tok, _, _, _ := dummyAccessToken(t)
	orgKey := testOrgKey(t)
	fi := newFakeIdentity(t, "", tok, orgKey)
	fi.forceResponse(401, `{"error":"invalid_grant"}`)
	fa := newFakeAPI(t, "", orgKey, nil)
	if err := SaveCredential(Credential{
		APIToken:    tok,
		OrgID:       "01234567-89ab-cdef-0123-456789abcdef",
		APIURL:      fa.url,
		IdentityURL: fi.url,
	}); err != nil {
		t.Fatalf("SaveCredential: %v", err)
	}

	res, err := secret.New(NewSource())
	if err != nil {
		t.Fatalf("secret.New: %v", err)
	}
	_, err = res.Expand(t.Context(), "bws://11111111-2222-3333-4444-555555555555")
	if err == nil {
		t.Fatal("401 exchange: nil error through Expand")
	}
	for _, sub := range []string{"401", "mcp bws login"} {
		if !strings.Contains(err.Error(), sub) {
			t.Errorf("error %q does not contain %q", err, sub)
		}
	}
}

// TestSourceResolveUncachedFailsClosed pins that an empty cache fails the
// expansion before any network call.
func TestSourceResolveUncachedFailsClosed(t *testing.T) {
	useTempTokenDir(t)
	res, err := secret.New(NewSource())
	if err != nil {
		t.Fatalf("secret.New: %v", err)
	}
	_, err = res.Expand(t.Context(), "bws://jev")
	if err == nil {
		t.Fatal("uncached credential: nil error")
	}
	if !strings.Contains(err.Error(), "mcp bws login") {
		t.Errorf("error %q does not point at mcp bws login", err)
	}
}

// TestSourceResolveMissingOrgID pins the name-ref fail-closed on a cached
// credential without OrgID.
func TestSourceResolveMissingOrgID(t *testing.T) {
	useTempTokenDir(t)
	tok, _, _, _ := dummyAccessToken(t)
	orgKey := testOrgKey(t)
	fa := newFakeAPI(t, "", orgKey, map[string]Secret{})
	fi := newFakeIdentity(t, "", tok, orgKey)
	if err := SaveCredential(Credential{APIToken: tok, APIURL: fa.url, IdentityURL: fi.url}); err != nil {
		t.Fatalf("SaveCredential: %v", err)
	}
	res, err := secret.New(NewSource())
	if err != nil {
		t.Fatalf("secret.New: %v", err)
	}
	_, err = res.Expand(t.Context(), "bws://jev")
	if err == nil {
		t.Fatal("name ref without org id: nil error")
	}
	if !strings.Contains(err.Error(), "--org") {
		t.Errorf("error %q does not carry the re-login remediation", err)
	}
}
