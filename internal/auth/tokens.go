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

func tokenCachePath() string { return filepath.Join(TokenDir, "tokens.json") }

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
// writeCache; lock files are deliberately not used (YAGNI).
var tokenMu sync.Mutex

type tokenCache map[string]*oauth2.Token

func readCache() (tokenCache, error) {
	data, err := os.ReadFile(tokenCachePath())
	if errors.Is(err, os.ErrNotExist) {
		return tokenCache{}, nil
	}
	if err != nil {
		return nil, err
	}
	cache := tokenCache{}
	if err := json.Unmarshal(data, &cache); err != nil {
		return nil, fmt.Errorf("parsing %s: %w", tokenCachePath(), err)
	}
	return cache, nil
}

func writeCache(cache tokenCache) error {
	if err := os.MkdirAll(TokenDir, 0o700); err != nil {
		return err
	}
	chmodPrivate(TokenDir, 0o700)

	data, err := json.MarshalIndent(cache, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')

	// Atomic replace: write a temp file in the same directory, then rename
	// over the cache. A concurrent reader sees either the old or the new
	// file, never a partial one.
	tmp, err := os.CreateTemp(TokenDir, "tokens-*.json.tmp")
	if err != nil {
		return err
	}
	name := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(name)
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(name)
		return err
	}
	// os.CreateTemp already uses 0600; the chmod guards against umask
	// surprises on Unix and is skipped on Windows where the bits are not
	// enforced.
	chmodPrivate(name, 0o600)
	if err := os.Rename(name, tokenCachePath()); err != nil {
		os.Remove(name)
		return err
	}
	return nil
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
	cache, err := readCache()
	if err != nil {
		return err
	}
	cache[tokenKey(target)] = tok
	return writeCache(cache)
}

// LoadToken returns the cached token for target. A missing cache file or a
// target that was never saved yields (nil, nil), not an error.
func LoadToken(target string) (*oauth2.Token, error) {
	if target == "" {
		return nil, errors.New("auth.LoadToken: empty target")
	}
	tokenMu.Lock()
	defer tokenMu.Unlock()
	cache, err := readCache()
	if err != nil {
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
	cache, err := readCache()
	if err != nil {
		return err
	}
	key := tokenKey(target)
	if _, ok := cache[key]; !ok {
		return nil
	}
	delete(cache, key)
	return writeCache(cache)
}
