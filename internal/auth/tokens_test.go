package auth

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
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

// TestReadCacheCorruptCacheIsError pins the corrupt-vs-missing cache
// discipline: a missing cache file is (nil, nil), but a corrupt one is an
// error wrapping "parsing <path>" — never a silently empty cache. The cache
// is not wedged by corruption: once the file is removed, normal
// save/load round-trips resume.
func TestReadCacheCorruptCacheIsError(t *testing.T) {
	TokenDir = t.TempDir()

	corruptPath := tokenCachePath()
	if err := os.WriteFile(corruptPath, []byte("{invalid"), 0o600); err != nil {
		t.Fatalf("write corrupt cache file: %v", err)
	}

	tok, err := LoadToken("any-target")
	if err == nil {
		t.Fatalf("LoadToken on corrupt cache = (%+v, nil error), want parse error", tok)
	}
	if tok != nil {
		t.Errorf("LoadToken on corrupt cache returned a token: %+v", tok)
	}
	if !strings.Contains(err.Error(), "parsing ") {
		t.Errorf("corrupt-cache error %q does not wrap %q", err, "parsing ")
	}
	if !strings.Contains(err.Error(), corruptPath) {
		t.Errorf("corrupt-cache error %q does not name the path %s", err, corruptPath)
	}

	// Recovery: removing the corrupt file un-wedges the cache.
	if err := os.Remove(corruptPath); err != nil {
		t.Fatalf("remove corrupt cache file: %v", err)
	}
	target := "https://example.test/mcp"
	want := &oauth2.Token{AccessToken: fakeAccessToken(t, "recover"), TokenType: "Bearer"}
	if err := SaveToken(target, want); err != nil {
		t.Fatalf("SaveToken after recovery: %v", err)
	}
	got, err := LoadToken(target)
	if err != nil {
		t.Fatalf("LoadToken after recovery: %v", err)
	}
	if got == nil || got.AccessToken != want.AccessToken {
		t.Errorf("LoadToken after recovery = %+v, want access=%q", got, want.AccessToken)
	}
}

// TestReadCacheFileWriteCacheFile pins the exported generic cache-file
// primitives on TokenDir: missing file is (found=false, nil), a corrupt one
// is an error naming the path, write-then-read round-trips, the
// serialization shape is MarshalIndent 2-space + trailing newline, the write
// is atomic (no temp litter) and private (0700 dir / 0600 file), and the
// temp-file pattern is derived from the cache-file name's stem.
func TestReadCacheFileWriteCacheFile(t *testing.T) {
	TokenDir = t.TempDir()

	type payload struct {
		Value string
	}

	// Missing file: found=false, nil error.
	var got payload
	found, err := ReadCacheFile("probe.json", &got)
	if err != nil {
		t.Fatalf("ReadCacheFile(missing): %v", err)
	}
	if found {
		t.Fatal("ReadCacheFile(missing) found = true, want false")
	}

	// Corrupt file: error wrapping "parsing " and naming the path.
	corruptPath := filepath.Join(TokenDir, "probe.json")
	if err := os.WriteFile(corruptPath, []byte("{invalid"), 0o600); err != nil {
		t.Fatalf("write corrupt probe.json: %v", err)
	}
	found, err = ReadCacheFile("probe.json", &got)
	if err == nil {
		t.Fatal("ReadCacheFile(corrupt) = nil error, want parse error")
	}
	if found {
		t.Error("ReadCacheFile(corrupt) found = true, want false")
	}
	if !strings.Contains(err.Error(), "parsing ") || !strings.Contains(err.Error(), corruptPath) {
		t.Errorf("ReadCacheFile(corrupt) error = %q, want wrapping %q and naming %s", err, "parsing ", corruptPath)
	}

	// Write-then-read round trip.
	want := payload{Value: fakeAccessToken(t, "cache")}
	if err := WriteCacheFile("probe.json", want); err != nil {
		t.Fatalf("WriteCacheFile: %v", err)
	}
	found, err = ReadCacheFile("probe.json", &got)
	if err != nil {
		t.Fatalf("ReadCacheFile(after write): %v", err)
	}
	if !found {
		t.Fatal("ReadCacheFile(after write) found = false, want true")
	}
	if got != want {
		t.Errorf("round trip mismatch: got %+v, want %+v", got, want)
	}

	// Serialization shape: MarshalIndent 2-space + trailing newline.
	raw, err := os.ReadFile(filepath.Join(TokenDir, "probe.json"))
	if err != nil {
		t.Fatalf("read probe.json: %v", err)
	}
	wantRaw, err := json.MarshalIndent(want, "", "  ")
	if err != nil {
		t.Fatalf("marshal expected shape: %v", err)
	}
	if want, got := append(wantRaw, '\n'), raw; !bytes.Equal(got, want) {
		t.Errorf("serialized shape = %q, want %q", got, want)
	}

	// Atomic rename: no temp litter survives the write.
	entries, err := os.ReadDir(TokenDir)
	if err != nil {
		t.Fatalf("read TokenDir: %v", err)
	}
	if len(entries) != 1 || entries[0].Name() != "probe.json" {
		var names []string
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Errorf("TokenDir entries after write = %v, want [probe.json]", names)
	}

	// Private write: 0700 dir / 0600 file where POSIX bits are enforced.
	if runtime.GOOS != "windows" {
		fi, err := os.Stat(TokenDir)
		if err != nil {
			t.Fatalf("stat TokenDir: %v", err)
		}
		if got := fi.Mode().Perm(); got != 0o700 {
			t.Errorf("TokenDir perms = %o, want 700", got)
		}
		fi, err = os.Stat(filepath.Join(TokenDir, "probe.json"))
		if err != nil {
			t.Fatalf("stat probe.json: %v", err)
		}
		if got := fi.Mode().Perm(); got != 0o600 {
			t.Errorf("cache file perms = %o, want 600", got)
		}
	}

	// Stem-derived temp pattern: byte-identical to the pre-refactor pattern
	// for the existing caller, and stem-derived for future ones.
	if p := cacheTempPattern("tokens.json"); p != "tokens-*.json.tmp" {
		t.Errorf("cacheTempPattern(tokens.json) = %q, want %q", p, "tokens-*.json.tmp")
	}
	if p := cacheTempPattern("bws.json"); p != "bws-*.json.tmp" {
		t.Errorf("cacheTempPattern(bws.json) = %q, want %q", p, "bws-*.json.tmp")
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
