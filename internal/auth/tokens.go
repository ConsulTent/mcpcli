package auth

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"

	"golang.org/x/oauth2"
)

// TokenDir is the directory holding the token cache file "tokens.json". It
// defaults to <os.UserConfigDir()>/.mcpcli (falling back to
// os.TempDir()/.mcpcli when the user config directory is unavailable) and
// may be reassigned; tests point it at a temporary directory.
var TokenDir = defaultTokenDir()

func defaultTokenDir() string {
	base, err := os.UserConfigDir()
	if err != nil {
		base = os.TempDir()
	}
	return filepath.Join(base, ".mcpcli")
}

// tokenCacheFile is the cache file name within TokenDir holding the OAuth2
// token map.
const tokenCacheFile = "tokens.json"

func tokenCachePath() string { return filepath.Join(TokenDir, tokenCacheFile) }

// tokenKey derives the cache key for a target: sha256(target) hex. Targets
// are URLs or stdio command strings whose raw form is unsuitable as a
// structural identifier, and hashing avoids leaking server URLs into the
// file's keys.
func tokenKey(target string) string {
	sum := sha256.Sum256([]byte(target))
	return hex.EncodeToString(sum[:])
}

// tokenMu serializes cache read-modify-write cycles within the process.
// Crash- and reader-safety across processes comes from the atomic rename in
// WriteCacheFile; lock files are deliberately not used (YAGNI).
var tokenMu sync.Mutex

type tokenCache map[string]*oauth2.Token

// ReadCacheFile reads the JSON cache file name within TokenDir into v. It is
// an unlocked primitive: callers doing a read-modify-write cycle hold their
// own lock (tokenMu for the token trio). A missing file yields
// (false, nil); a corrupt one is an error wrapping "parsing <path>" — the
// corrupt-vs-missing discipline every cache in TokenDir inherits.
func ReadCacheFile(name string, v any) (bool, error) {
	path := filepath.Join(TokenDir, name)
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if err := json.Unmarshal(data, v); err != nil {
		return false, fmt.Errorf("parsing %s: %w", path, err)
	}
	return true, nil
}

// WriteCacheFile atomically writes v as JSON to the cache file name within
// TokenDir: MarshalIndent 2-space plus a trailing newline, a temp file
// named after the cache file's stem (cacheTempPattern), then a rename over
// the target. It is an unlocked primitive, paired with ReadCacheFile.
func WriteCacheFile(name string, v any) error {
	if err := os.MkdirAll(TokenDir, 0o700); err != nil {
		return err
	}
	chmodPrivate(TokenDir, 0o700)

	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')

	// Atomic replace: write a temp file in the same directory, then rename
	// over the cache. A concurrent reader sees either the old or the new
	// file, never a partial one.
	tmp, err := os.CreateTemp(TokenDir, cacheTempPattern(name))
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return err
	}
	// os.CreateTemp already uses 0600; the chmod guards against umask
	// surprises on Unix and is skipped on Windows where the bits are not
	// enforced.
	chmodPrivate(tmpName, 0o600)
	if err := os.Rename(tmpName, filepath.Join(TokenDir, name)); err != nil {
		os.Remove(tmpName)
		return err
	}
	return nil
}

// cacheTempPattern derives the os.CreateTemp pattern for a cache file name:
// "<stem>-*.json.tmp". For tokens.json this is byte-identical to the
// pre-refactor hard-coded "tokens-*.json.tmp"; bws.json becomes
// "bws-*.json.tmp".
func cacheTempPattern(name string) string {
	return strings.TrimSuffix(name, ".json") + "-*.json.tmp"
}

// chmodPrivate tightens permissions on Unix. Windows ignores these bits, so
// the calls are skipped there.
func chmodPrivate(path string, mode os.FileMode) {
	if runtime.GOOS == "windows" {
		return
	}
	_ = os.Chmod(path, mode)
}

// SaveToken stores tok under the cache key for target.
func SaveToken(target string, tok *oauth2.Token) error {
	if target == "" {
		return errors.New("auth.SaveToken: empty target")
	}
	if tok == nil {
		return errors.New("auth.SaveToken: nil token")
	}
	tokenMu.Lock()
	defer tokenMu.Unlock()
	cache := tokenCache{}
	if _, err := ReadCacheFile(tokenCacheFile, &cache); err != nil {
		return err
	}
	cache[tokenKey(target)] = tok
	return WriteCacheFile(tokenCacheFile, cache)
}

// LoadToken returns the cached token for target. A missing cache file or a
// target that was never saved yields (nil, nil), not an error.
func LoadToken(target string) (*oauth2.Token, error) {
	if target == "" {
		return nil, errors.New("auth.LoadToken: empty target")
	}
	tokenMu.Lock()
	defer tokenMu.Unlock()
	cache := tokenCache{}
	if _, err := ReadCacheFile(tokenCacheFile, &cache); err != nil {
		return nil, err
	}
	return cache[tokenKey(target)], nil
}

// DeleteToken removes the cached token for target. A missing cache file or
// key is not an error.
func DeleteToken(target string) error {
	if target == "" {
		return errors.New("auth.DeleteToken: empty target")
	}
	tokenMu.Lock()
	defer tokenMu.Unlock()
	cache := tokenCache{}
	if _, err := ReadCacheFile(tokenCacheFile, &cache); err != nil {
		return err
	}
	key := tokenKey(target)
	if _, ok := cache[key]; !ok {
		return nil
	}
	delete(cache, key)
	return WriteCacheFile(tokenCacheFile, cache)
}
