package auth

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"golang.org/x/oauth2"
)

// Each token-cache test points TokenDir at its own temporary directory.
// The tests must not run in parallel: TokenDir is a package-level var.

// fakeAccessToken derives an obviously-synthetic, per-test token value so no
// credential-shaped literal appears in the test source.
func fakeAccessToken(t *testing.T, seed string) string {
	return "not-a-real-token-" + t.Name() + "-" + seed
}

func TestTokenCacheRoundTrip(t *testing.T) {
	TokenDir = t.TempDir()

	target := "https://example.test/mcp"
	tok := &oauth2.Token{AccessToken: fakeAccessToken(t, "main"), TokenType: "Bearer"}
	if err := SaveToken(target, tok); err != nil {
		t.Fatalf("SaveToken: %v", err)
	}

	got, err := LoadToken(target)
	if err != nil {
		t.Fatalf("LoadToken: %v", err)
	}
	if got == nil {
		t.Fatal("LoadToken returned nil for a saved token")
	}
	if got.AccessToken != fakeAccessToken(t, "main") || got.Type() != "Bearer" {
		t.Errorf("round trip mismatch: got access=%q type=%q", got.AccessToken, got.Type())
	}

	// Unknown target: missing token is not an error.
	other, err := LoadToken("https://other.test")
	if err != nil {
		t.Fatalf("LoadToken(unknown): %v", err)
	}
	if other != nil {
		t.Errorf("LoadToken(unknown) = %+v, want nil", other)
	}

	// Delete removes the token; deleting again (or a missing cache file) is
	// not an error.
	if err := DeleteToken(target); err != nil {
		t.Fatalf("DeleteToken: %v", err)
	}
	gone, err := LoadToken(target)
	if err != nil {
		t.Fatalf("LoadToken after delete: %v", err)
	}
	if gone != nil {
		t.Errorf("token survived DeleteToken: %+v", gone)
	}
	if err := DeleteToken(target); err != nil {
		t.Errorf("repeated DeleteToken must be a no-op, got: %v", err)
	}
}

func TestTokenCacheKeyedByTarget(t *testing.T) {
	TokenDir = t.TempDir()

	a := "https://alpha.test/mcp"
	b := "https://beta.test/mcp"
	if err := SaveToken(a, &oauth2.Token{AccessToken: fakeAccessToken(t, "a")}); err != nil {
		t.Fatalf("SaveToken(a): %v", err)
	}
	if err := SaveToken(b, &oauth2.Token{AccessToken: fakeAccessToken(t, "b")}); err != nil {
		t.Fatalf("SaveToken(b): %v", err)
	}

	gotA, err := LoadToken(a)
	if err != nil || gotA == nil || gotA.AccessToken != fakeAccessToken(t, "a") {
		t.Errorf("LoadToken(a) = %+v, %v; want a", gotA, err)
	}
	gotB, err := LoadToken(b)
	if err != nil || gotB == nil || gotB.AccessToken != fakeAccessToken(t, "b") {
		t.Errorf("LoadToken(b) = %+v, %v; want b", gotB, err)
	}

	// Overwriting one target must not disturb the other.
	if err := SaveToken(a, &oauth2.Token{AccessToken: fakeAccessToken(t, "a2")}); err != nil {
		t.Fatalf("SaveToken(a) overwrite: %v", err)
	}
	gotA2, _ := LoadToken(a)
	if gotA2 == nil || gotA2.AccessToken != fakeAccessToken(t, "a2") {
		t.Errorf("LoadToken(a) after overwrite = %+v, want a2", gotA2)
	}
	gotB2, _ := LoadToken(b)
	if gotB2 == nil || gotB2.AccessToken != fakeAccessToken(t, "b") {
		t.Errorf("LoadToken(b) after overwriting a = %+v, want b", gotB2)
	}

	if err := DeleteToken(a); err != nil {
		t.Fatalf("DeleteToken(a): %v", err)
	}
	gotB3, _ := LoadToken(b)
	if gotB3 == nil || gotB3.AccessToken != fakeAccessToken(t, "b") {
		t.Errorf("LoadToken(b) after deleting a = %+v, want b", gotB3)
	}
}

func TestTokenCacheFilePerms(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX permission bits are not enforced on Windows")
	}
	TokenDir = t.TempDir()

	if err := SaveToken("https://example.test/mcp", &oauth2.Token{AccessToken: fakeAccessToken(t, "perms")}); err != nil {
		t.Fatalf("SaveToken: %v", err)
	}

	fi, err := os.Stat(TokenDir)
	if err != nil {
		t.Fatalf("stat TokenDir: %v", err)
	}
	if !fi.IsDir() {
		t.Fatal("TokenDir is not a directory")
	}
	if got := fi.Mode().Perm(); got != 0o700 {
		t.Errorf("TokenDir perms = %o, want 700", got)
	}

	fi, err = os.Stat(tokenCachePath())
	if err != nil {
		t.Fatalf("stat cache file: %v", err)
	}
	if got := fi.Mode().Perm(); got != 0o600 {
		t.Errorf("cache file perms = %o, want 600", got)
	}
}

// TestDefaultTokenDirName pins the default cache directory name: a dot-dir
// (.mcpcli) under the user config directory (os.TempDir() fallback), so the
// rename from the historical "mcpcli" cannot silently regress.
func TestDefaultTokenDirName(t *testing.T) {
	got := defaultTokenDir()
	if base := filepath.Base(got); base != ".mcpcli" {
		t.Errorf("defaultTokenDir() base = %q, want %q (full path %q)", base, ".mcpcli", got)
	}
}
