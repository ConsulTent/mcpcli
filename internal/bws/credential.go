package bws

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"
	"sync"

	"github.com/ConsulTent/mcpcli/internal/auth"
)

// Credential is one stored Bitwarden machine-account login: the
// machine-account access token (NOT the short-lived exchange JWT — that is
// per-run and never persisted), the org id for name refs, and the two
// instance URLs. The field name APIToken (vs NewClient's accessToken
// parameter) is deliberate: it names what is stored — the long-lived
// machine-account token — as opposed to the per-run exchange JWT.
type Credential struct {
	APIToken    string `json:"api_token"`
	OrgID       string `json:"org_id"`
	APIURL      string `json:"api_url"`
	IdentityURL string `json:"identity_url"`
}

// credentialCacheFile is the cache file name within auth.TokenDir holding
// the credential map, keyed sha256(normalized APIURL).
const credentialCacheFile = "bws.json"

// credentialKey derives the cache key for an instance: sha256(apiURL) hex —
// the same discipline as the OAuth2 token cache.
func credentialKey(apiURL string) string {
	sum := sha256.Sum256([]byte(apiURL))
	return hex.EncodeToString(sum[:])
}

// credentialMu serializes cache read-modify-write cycles within the
// process, mirroring auth's tokenMu (the exported auth.ReadCacheFile/
// auth.WriteCacheFile primitives are deliberately unlocked).
var credentialMu sync.Mutex

// normalizeCredentialURL normalizes an API URL and fails closed with the
// input named.
func normalizeCredentialURL(raw string) (string, error) {
	normalized, err := NormalizeURL(raw)
	if err != nil {
		return "", fmt.Errorf("bws: %w", err)
	}
	if normalized == "" {
		return "", errors.New("bws: the API URL is empty")
	}
	return normalized, nil
}

// loadCredentials reads the credential map from bws.json via
// auth.ReadCacheFile. A `null` file unmarshals error-free into a nil map
// (json null), so the map is re-initialized here — a nil map would silently
// drop every later upsert.
func loadCredentials() (map[string]Credential, error) {
	creds := map[string]Credential{}
	if _, err := auth.ReadCacheFile(credentialCacheFile, &creds); err != nil {
		return nil, err
	}
	if creds == nil {
		creds = map[string]Credential{}
	}
	return creds, nil
}

// SaveCredential upserts the credential under sha256 of its normalized
// APIURL. The URL is also normalized into the stored value: hash keys are
// not reversible, and every error listing renders the stored form.
func SaveCredential(cred Credential) error {
	normalized, err := normalizeCredentialURL(cred.APIURL)
	if err != nil {
		return err
	}
	cred.APIURL = normalized
	credentialMu.Lock()
	defer credentialMu.Unlock()
	creds, err := loadCredentials()
	if err != nil {
		return err
	}
	creds[credentialKey(normalized)] = cred
	return auth.WriteCacheFile(credentialCacheFile, creds)
}

// DeleteCredential removes the credential for apiURL (normalized). A
// missing instance reports (false, nil) — but unlike a silently-nil delete,
// callers can distinguish it from a real removal.
func DeleteCredential(apiURL string) (bool, error) {
	normalized, err := normalizeCredentialURL(apiURL)
	if err != nil {
		return false, err
	}
	credentialMu.Lock()
	defer credentialMu.Unlock()
	creds, err := loadCredentials()
	if err != nil {
		return false, err
	}
	key := credentialKey(normalized)
	if _, ok := creds[key]; !ok {
		return false, nil
	}
	delete(creds, key)
	if err := auth.WriteCacheFile(credentialCacheFile, creds); err != nil {
		return false, err
	}
	return true, nil
}

// CachedInstances returns the normalized API URLs of every cached instance
// login, sorted — the offline enumeration logout's truth table rides (zero,
// one or many cached instances). Deliberately env-free: unlike
// ResolveCredential it never consults MCPCLI_BWS_API_URL, because the env
// selects credentials for the reading verbs, it is not a cache target.
func CachedInstances() ([]string, error) {
	credentialMu.Lock()
	defer credentialMu.Unlock()
	creds, err := loadCredentials()
	if err != nil {
		return nil, err
	}
	return sortedCachedURLs(creds), nil
}

// ResolveCredential resolves the Bitwarden instance to use, consult-in-order:
// the --api-url flag beats the MCPCLI_BWS_API_URL environment variable beats
// the cache. An empty flag slot is UNSET, not invalid (the expansion path
// calls ResolveCredential("")). The env is never examined when the flag
// wins. Every failure names its input; a valid-but-uncached selector fails
// closed naming the cached instances and the remediation. Resolution never
// validates the credential against the network — a token rejected later at
// Exchange is the caller's 401 message.
func ResolveCredential(apiURLFlag string) (Credential, error) {
	if apiURLFlag != "" {
		return resolveBySelector("flag", "--api-url", apiURLFlag)
	}
	if env, ok := os.LookupEnv("MCPCLI_BWS_API_URL"); ok {
		if env == "" {
			return Credential{}, errors.New("bws: MCPCLI_BWS_API_URL is set but empty; unset it or set it to the instance URL, e.g. https://api.bitwarden.com")
		}
		return resolveBySelector("MCPCLI_BWS_API_URL", "MCPCLI_BWS_API_URL", env)
	}

	credentialMu.Lock()
	defer credentialMu.Unlock()
	creds, err := loadCredentials()
	if err != nil {
		return Credential{}, err
	}
	switch len(creds) {
	case 0:
		return Credential{}, errors.New("bws: no Bitwarden instance is logged in; run \"mcp bws login\"")
	case 1:
		for _, cred := range creds {
			return cred, nil
		}
	}
	return Credential{}, fmt.Errorf("bws: multiple Bitwarden instances are logged in (%s); pass --api-url or set MCPCLI_BWS_API_URL",
		strings.Join(sortedCachedURLs(creds), ", "))
}

// resolveBySelector resolves a non-empty selector (flag or env) against the
// cache. The selector kind and spelling name the input in every error.
func resolveBySelector(kind, inputName, raw string) (Credential, error) {
	normalized, err := normalizeCredentialURL(raw)
	if err != nil {
		return Credential{}, fmt.Errorf("bws: invalid %s %q: %w", inputName, raw, err)
	}
	credentialMu.Lock()
	defer credentialMu.Unlock()
	creds, err := loadCredentials()
	if err != nil {
		return Credential{}, err
	}
	if cred, ok := creds[credentialKey(normalized)]; ok {
		return cred, nil
	}
	return Credential{}, fmt.Errorf("bws: no Bitwarden credential is cached for %s (%s); logged-in instances: %s; run \"mcp bws login --api-url %s\" first",
		inputName, normalized, strings.Join(sortedCachedURLs(creds), ", "), normalized)
}

// sortedCachedURLs renders the cached instances' API URLs in sorted order —
// the one listing every multi-instance fail-closed error uses (map
// iteration is randomized; these errors are golden-pinned).
func sortedCachedURLs(creds map[string]Credential) []string {
	urls := make([]string, 0, len(creds))
	for _, cred := range creds {
		urls = append(urls, cred.APIURL)
	}
	slices.Sort(urls)
	return urls
}
