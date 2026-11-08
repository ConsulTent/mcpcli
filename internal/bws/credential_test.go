package bws

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ConsulTent/mcpcli/internal/auth"
)

// useTempTokenDir points auth.TokenDir at a per-test temporary directory and
// restores the previous value afterwards. Tests must not run in parallel:
// TokenDir is a package global.
func useTempTokenDir(t *testing.T) {
	t.Helper()
	prev := auth.TokenDir
	auth.TokenDir = t.TempDir()
	t.Cleanup(func() { auth.TokenDir = prev })
}

// dummyCredential builds an obviously-synthetic credential for the given
// host; no usable-looking literal appears in test source.
func dummyCredential(t *testing.T, host string) Credential {
	t.Helper()
	token := make([]byte, 12)
	if _, err := rand.Read(token); err != nil {
		t.Fatalf("rand: %v", err)
	}
	return Credential{
		APIToken:    "bws-dummy-" + t.Name() + "-" + hex.EncodeToString(token),
		OrgID:       "01234567-89ab-cdef-0123-456789abcdef",
		APIURL:      "https://" + host,
		IdentityURL: "https://identity." + host,
	}
}

func credentialCachePath() string {
	return filepath.Join(auth.TokenDir, credentialCacheFile)
}

func TestSaveResolveRoundTrip(t *testing.T) {
	useTempTokenDir(t)
	want := dummyCredential(t, "round-trip.test")
	if err := SaveCredential(want); err != nil {
		t.Fatalf("SaveCredential: %v", err)
	}
	got, err := ResolveCredential("https://round-trip.test")
	if err != nil {
		t.Fatalf("ResolveCredential: %v", err)
	}
	if got != want {
		t.Errorf("ResolveCredential = %+v, want %+v", got, want)
	}

	// The empty flag slot is UNSET (the expansion path's call form): with
	// exactly one cached instance it resolves from the cache.
	got, err = ResolveCredential("")
	if err != nil {
		t.Fatalf("ResolveCredential(empty flag): %v", err)
	}
	if got != want {
		t.Errorf("ResolveCredential(empty flag) = %+v, want %+v", got, want)
	}
}

func TestSaveNormalizesAPIURL(t *testing.T) {
	useTempTokenDir(t)
	cred := dummyCredential(t, "normalized.test")
	cred.APIURL = "https://Normalized.Test:443/"
	if err := SaveCredential(cred); err != nil {
		t.Fatalf("SaveCredential: %v", err)
	}
	// Any spelling of the same normalized URL must hit the cache.
	got, err := ResolveCredential("https://normalized.test")
	if err != nil {
		t.Fatalf("ResolveCredential: %v", err)
	}
	if got.APIURL != "https://normalized.test" {
		t.Errorf("stored APIURL = %q, want the normalized form", got.APIURL)
	}
}

func TestResolveCredentialPrecedence(t *testing.T) {
	useTempTokenDir(t)
	alpha := dummyCredential(t, "alpha.test")
	beta := dummyCredential(t, "beta.test")
	if err := SaveCredential(alpha); err != nil {
		t.Fatalf("SaveCredential(alpha): %v", err)
	}
	if err := SaveCredential(beta); err != nil {
		t.Fatalf("SaveCredential(beta): %v", err)
	}

	t.Run("flag beats env and the env is unexamined", func(t *testing.T) {
		// A garbage env proves it is never consulted when the flag wins.
		t.Setenv("MCPCLI_BWS_API_URL", "::::not-a-url::::")
		got, err := ResolveCredential("https://alpha.test")
		if err != nil {
			t.Fatalf("ResolveCredential(flag): %v", err)
		}
		if got.APIURL != "https://alpha.test" {
			t.Errorf("flag did not win: %+v", got)
		}
	})

	t.Run("env beats cache when the flag is unset", func(t *testing.T) {
		t.Setenv("MCPCLI_BWS_API_URL", "https://beta.test")
		got, err := ResolveCredential("")
		if err != nil {
			t.Fatalf("ResolveCredential(env): %v", err)
		}
		if got.APIURL != "https://beta.test" {
			t.Errorf("env did not win: %+v", got)
		}
	})
}

func TestResolveCredentialSetButEmptyEnvFailsClosed(t *testing.T) {
	useTempTokenDir(t)
	t.Setenv("MCPCLI_BWS_API_URL", "")
	_, err := ResolveCredential("")
	if err == nil {
		t.Fatal("set-but-empty env: nil error")
	}
	if !strings.Contains(err.Error(), "MCPCLI_BWS_API_URL") || !strings.Contains(err.Error(), "empty") {
		t.Errorf("error %q does not name the env input and its emptiness", err)
	}
}

func TestResolveCredentialInvalidInputsNameTheInput(t *testing.T) {
	useTempTokenDir(t)

	t.Run("bad flag", func(t *testing.T) {
		_, err := ResolveCredential("not a url")
		if err == nil {
			t.Fatal("bad flag: nil error")
		}
		if !strings.Contains(err.Error(), "--api-url") || !strings.Contains(err.Error(), "not a url") {
			t.Errorf("error %q does not name the flag input", err)
		}
	})

	t.Run("non-http flag", func(t *testing.T) {
		_, err := ResolveCredential("ftp://api.test")
		if err == nil {
			t.Fatal("non-http flag: nil error")
		}
		if !strings.Contains(err.Error(), "--api-url") {
			t.Errorf("error %q does not name the flag input", err)
		}
	})

	t.Run("bad env", func(t *testing.T) {
		t.Setenv("MCPCLI_BWS_API_URL", "ftp://api.test")
		_, err := ResolveCredential("")
		if err == nil {
			t.Fatal("bad env: nil error")
		}
		if !strings.Contains(err.Error(), "MCPCLI_BWS_API_URL") {
			t.Errorf("error %q does not name the env input", err)
		}
	})
}

func TestResolveCredentialCacheTruthTable(t *testing.T) {
	t.Run("zero cached entries", func(t *testing.T) {
		useTempTokenDir(t)
		_, err := ResolveCredential("")
		if err == nil {
			t.Fatal("empty cache: nil error")
		}
		if !strings.Contains(err.Error(), "mcp bws login") {
			t.Errorf("error %q does not point at mcp bws login", err)
		}
	})

	t.Run("two cached entries list sorted URLs", func(t *testing.T) {
		useTempTokenDir(t)
		if err := SaveCredential(dummyCredential(t, "zeta.test")); err != nil {
			t.Fatalf("SaveCredential(zeta): %v", err)
		}
		if err := SaveCredential(dummyCredential(t, "alpha.test")); err != nil {
			t.Fatalf("SaveCredential(alpha): %v", err)
		}
		_, err := ResolveCredential("")
		if err == nil {
			t.Fatal("two cached entries: nil error")
		}
		// Sorted, not map order.
		if !strings.Contains(err.Error(), "https://alpha.test") || !strings.Contains(err.Error(), "https://zeta.test") {
			t.Errorf("error %q does not list the cached instances", err)
		}
		if strings.Index(err.Error(), "https://alpha.test") > strings.Index(err.Error(), "https://zeta.test") {
			t.Errorf("error %q lists instances unsorted", err)
		}
	})
}

func TestResolveCredentialValidButUncachedFailsClosed(t *testing.T) {
	useTempTokenDir(t)
	cached := dummyCredential(t, "cached.test")
	if err := SaveCredential(cached); err != nil {
		t.Fatalf("SaveCredential: %v", err)
	}

	t.Run("flag selector", func(t *testing.T) {
		_, err := ResolveCredential("https://uncached.test")
		if err == nil {
			t.Fatal("valid-but-uncached flag: nil error")
		}
		for _, sub := range []string{"https://uncached.test", "https://cached.test", "mcp bws login", "--api-url"} {
			if !strings.Contains(err.Error(), sub) {
				t.Errorf("error %q does not contain %q", err, sub)
			}
		}
	})

	t.Run("env selector", func(t *testing.T) {
		t.Setenv("MCPCLI_BWS_API_URL", "https://uncached.test")
		_, err := ResolveCredential("")
		if err == nil {
			t.Fatal("valid-but-uncached env: nil error")
		}
		for _, sub := range []string{"https://uncached.test", "https://cached.test", "mcp bws login"} {
			if !strings.Contains(err.Error(), sub) {
				t.Errorf("error %q does not contain %q", err, sub)
			}
		}
	})
}

func TestDeleteCredential(t *testing.T) {
	useTempTokenDir(t)
	cred := dummyCredential(t, "delete-me.test")
	if err := SaveCredential(cred); err != nil {
		t.Fatalf("SaveCredential: %v", err)
	}

	// Deleting an unknown instance is not an error and reports false.
	deleted, err := DeleteCredential("https://unknown.test")
	if err != nil {
		t.Fatalf("DeleteCredential(unknown): %v", err)
	}
	if deleted {
		t.Error("DeleteCredential(unknown) = true, want false")
	}

	deleted, err = DeleteCredential("https://delete-me.test")
	if err != nil {
		t.Fatalf("DeleteCredential: %v", err)
	}
	if !deleted {
		t.Error("DeleteCredential(existing) = false, want true")
	}
	if _, err := ResolveCredential(""); err == nil {
		t.Error("cache not empty after delete")
	}

	// Deleting again is still false-and-nil.
	deleted, err = DeleteCredential("https://delete-me.test")
	if err != nil || deleted {
		t.Errorf("repeated DeleteCredential = (%v, %v), want (false, nil)", deleted, err)
	}
}

// TestNullContentCachePinsReInit pins the nil-map re-init: a bws.json
// holding the literal `null` (json.Unmarshal nils a pre-initialized map
// error-free) must not wedge the cache — SaveCredential upserts through it.
func TestNullContentCachePinsReInit(t *testing.T) {
	useTempTokenDir(t)
	if err := os.WriteFile(credentialCachePath(), []byte("null"), 0o600); err != nil {
		t.Fatalf("write null bws.json: %v", err)
	}
	cred := dummyCredential(t, "after-null.test")
	if err := SaveCredential(cred); err != nil {
		t.Fatalf("SaveCredential over null content: %v", err)
	}
	got, err := ResolveCredential("https://after-null.test")
	if err != nil {
		t.Fatalf("ResolveCredential after null content: %v", err)
	}
	if got != cred {
		t.Errorf("ResolveCredential = %+v, want %+v", got, cred)
	}
}

// TestCredentialCacheByteShape pins the serialization shape of bws.json:
// map keyed sha256(normalized APIURL), the URL also in the value,
// MarshalIndent 2-space plus trailing newline (via auth.WriteCacheFile).
func TestCredentialCacheByteShape(t *testing.T) {
	useTempTokenDir(t)
	cred := dummyCredential(t, "byte-shape.test")
	if err := SaveCredential(cred); err != nil {
		t.Fatalf("SaveCredential: %v", err)
	}
	raw, err := os.ReadFile(credentialCachePath())
	if err != nil {
		t.Fatalf("read bws.json: %v", err)
	}

	want := map[string]Credential{credentialKey("https://byte-shape.test"): cred}
	wantRaw, err := json.MarshalIndent(want, "", "  ")
	if err != nil {
		t.Fatalf("marshal expected shape: %v", err)
	}
	if got, want := string(raw), string(wantRaw)+"\n"; got != want {
		t.Errorf("bws.json =\n%s\nwant\n%s", got, want)
	}
}

// TestTwoInstanceUpsert pins that a second SaveCredential preserves the
// first instance and that the file on disk byte-matches the expected
// two-key map.
func TestTwoInstanceUpsert(t *testing.T) {
	useTempTokenDir(t)
	a := dummyCredential(t, "one.test")
	b := dummyCredential(t, "two.test")
	if err := SaveCredential(a); err != nil {
		t.Fatalf("SaveCredential(a): %v", err)
	}
	if err := SaveCredential(b); err != nil {
		t.Fatalf("SaveCredential(b): %v", err)
	}

	gotA, err := ResolveCredential("https://one.test")
	if err != nil || gotA != a {
		t.Errorf("ResolveCredential(a) = %+v, %v; want %+v", gotA, err, a)
	}
	gotB, err := ResolveCredential("https://two.test")
	if err != nil || gotB != b {
		t.Errorf("ResolveCredential(b) = %+v, %v; want %+v", gotB, err, b)
	}

	raw, err := os.ReadFile(credentialCachePath())
	if err != nil {
		t.Fatalf("read bws.json: %v", err)
	}
	want := map[string]Credential{
		credentialKey("https://one.test"): a,
		credentialKey("https://two.test"): b,
	}
	wantRaw, err := json.MarshalIndent(want, "", "  ")
	if err != nil {
		t.Fatalf("marshal expected shape: %v", err)
	}
	if got, want := string(raw), string(wantRaw)+"\n"; got != want {
		t.Errorf("bws.json =\n%s\nwant\n%s", got, want)
	}

	// Upserting an existing instance overwrites only that instance.
	b2 := b
	b2.OrgID = "99999999-9999-9999-9999-999999999999"
	if err := SaveCredential(b2); err != nil {
		t.Fatalf("SaveCredential(b2): %v", err)
	}
	gotB2, _ := ResolveCredential("https://two.test")
	if gotB2.OrgID != b2.OrgID {
		t.Errorf("upsert did not overwrite: %+v", gotB2)
	}
	gotA2, _ := ResolveCredential("https://one.test")
	if gotA2 != a {
		t.Errorf("upserting b disturbed a: %+v", gotA2)
	}
}

// TestCredentialCacheCorruptIsError inherits the corrupt-vs-missing cache
// discipline from auth.ReadCacheFile.
func TestCredentialCacheCorruptIsError(t *testing.T) {
	useTempTokenDir(t)
	if err := os.WriteFile(credentialCachePath(), []byte("{invalid"), 0o600); err != nil {
		t.Fatalf("write corrupt bws.json: %v", err)
	}
	if _, err := ResolveCredential(""); err == nil {
		t.Fatal("corrupt bws.json: nil error")
	} else if !strings.Contains(err.Error(), "parsing ") {
		t.Errorf("error %q does not wrap the parsing path", err)
	}
}
