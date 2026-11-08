package cli

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ConsulTent/mcpcli/internal/auth"
	"github.com/ConsulTent/mcpcli/internal/bws"
)

// Every credential-shaped value in this file is an obviously-synthetic
// dummy; the operator's real machine account, org id and secret ids must
// never appear in test source (the house credentials rule).

// The fakes ride the known-answer vector pinned in internal/bws's own test
// suite: a token whose 16-byte seed is bwsTestTokenSeed derives the key that
// decrypts bwsTestEncPayload to {"encryptionKey": bwsTestOrgKeyB64}. The
// identity fake therefore serves one constant body and needs no crypto of
// its own; only the API fake encrypts (with test-side AES/HMAC below).
const (
	bwsTestTokenSeed  = "watermelonfrog09" // 16 bytes
	bwsTestOrgKeyB64  = "oKGio6SlpqeoqaqrrK2ur7CxsrO0tba3uLm6u7y9vr/AwcLDxMXGx8jJysvMzc7P0NHS09TV1tfY2drb3N3e3w=="
	bwsTestEncPayload = "2.AAECAwQFBgcICQoLDA0ODw==|Kg10BFz6zEwuUoY+n9hgGHGioJ1GL80naJ1u33dF3LfLRJWb4XrmHPY9EYDqt/BOBP0xrcD6FChk71w6dDmdEcs5BeKECL1sZnezSJcyxaJG9k/RN3Th4Y836ZBQdnD1x1gAb2hZCwOO7aoo/z2TBQ==|X6kPVAK8pa/3wyTFkHGXfU8X4GE+X11gc/A5C3qKQfY="
)

// bwsTestToken is the obviously-synthetic machine-account token the tests
// log in with (the parts "cli-test-machine"/"cli-test-secret" are dummies).
var bwsTestToken = "0.cli-test-machine.cli-test-secret:" + base64.StdEncoding.EncodeToString([]byte(bwsTestTokenSeed))

// bwsTestOrgID is the dummy organization uuid the name-ref tests log in with.
const bwsTestOrgID = "01234567-89ab-cdef-0123-456789abcdef"

// bwsTestOrgKey decodes the pinned known-answer org key.
func bwsTestOrgKey(t *testing.T) []byte {
	t.Helper()
	key, err := base64.StdEncoding.DecodeString(bwsTestOrgKeyB64)
	if err != nil {
		t.Fatalf("decode pinned org key: %v", err)
	}
	return key
}

// bwsTestTokenDir points the credential/token cache at a fresh temp dir and
// restores it afterward (package-global, so tests stay sequential).
func bwsTestTokenDir(t *testing.T) {
	t.Helper()
	old := auth.TokenDir
	auth.TokenDir = t.TempDir()
	t.Cleanup(func() { auth.TokenDir = old })
}

// bwsTestIdentity is an httptest BWS identity server serving the constant
// known-answer exchange envelope (decryptable by any token carrying the
// pinned seed) and counting hits.
type bwsTestIdentity struct {
	mu   sync.Mutex
	url  string
	hits int
}

func newBWSTestIdentity(t *testing.T) *bwsTestIdentity {
	t.Helper()
	fi := &bwsTestIdentity{}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /connect/token", func(w http.ResponseWriter, r *http.Request) {
		fi.mu.Lock()
		fi.hits++
		fi.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"access_token":%q,"encrypted_payload":%q}`, "cli-test-jwt", bwsTestEncPayload)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	fi.url = srv.URL
	return fi
}

func (fi *bwsTestIdentity) exchangeHits() int {
	fi.mu.Lock()
	defer fi.mu.Unlock()
	return fi.hits
}

// bwsTestAPI is an httptest BWS API server serving secrets in wire form
// (fields encrypted with the org key by test-side crypto).
type bwsTestAPI struct {
	mu         sync.Mutex
	url        string
	wire       map[string]bws.Secret
	ids        []string // deterministic list order
	gets       int
	lists      int
	listStatus int
}

func newBWSTestAPI(t *testing.T, orgKey []byte, secrets map[string]bws.Secret) *bwsTestAPI {
	t.Helper()
	fa := &bwsTestAPI{wire: make(map[string]bws.Secret, len(secrets))}
	for id, s := range secrets {
		fa.wire[id] = bwsTestWireSecret(t, orgKey, s)
		fa.ids = append(fa.ids, id)
	}
	slices.Sort(fa.ids)

	mux := http.NewServeMux()
	mux.HandleFunc("GET /secrets/{id}", func(w http.ResponseWriter, r *http.Request) {
		fa.mu.Lock()
		fa.gets++
		fa.mu.Unlock()
		s, ok := fa.wire[r.PathValue("id")]
		if !ok {
			http.Error(w, "no such secret", http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(s)
	})
	mux.HandleFunc("GET /organizations/{org}/secrets", func(w http.ResponseWriter, r *http.Request) {
		fa.mu.Lock()
		fa.lists++
		status := fa.listStatus
		fa.mu.Unlock()
		if status != 0 {
			w.WriteHeader(status)
			return
		}
		items := make([]bws.Secret, 0, len(fa.ids))
		for _, id := range fa.ids {
			items = append(items, fa.wire[id])
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"object":            "secretListView",
			"projects":          []string{},
			"secrets":           items,
			"continuationToken": "",
		})
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	fa.url = srv.URL
	return fa
}

func (fa *bwsTestAPI) getHits() int {
	fa.mu.Lock()
	defer fa.mu.Unlock()
	return fa.gets
}

func (fa *bwsTestAPI) forceList(status int) {
	fa.mu.Lock()
	defer fa.mu.Unlock()
	fa.listStatus = status
}

// bwsTestEncrypt encrypts one plaintext field to its wire EncString form
// with test-side crypto (crypto/aes + crypto/hmac directly, independent of
// the code under test — the buildEncStringRaw doctrine). A fixed zero IV
// keeps fixtures deterministic.
func bwsTestEncrypt(t *testing.T, plain string, orgKey []byte) string {
	t.Helper()
	block, err := aes.NewCipher(orgKey[:32])
	if err != nil {
		t.Fatalf("aes cipher: %v", err)
	}
	padLen := aes.BlockSize - len(plain)%aes.BlockSize
	padded := make([]byte, 0, len(plain)+padLen)
	padded = append(padded, plain...)
	for i := 0; i < padLen; i++ {
		padded = append(padded, byte(padLen))
	}
	iv := make([]byte, aes.BlockSize)
	ct := make([]byte, len(padded))
	cipher.NewCBCEncrypter(block, iv).CryptBlocks(ct, padded)
	mac := hmac.New(sha256.New, orgKey[32:64])
	mac.Write(iv)
	mac.Write(ct)
	b64 := base64.StdEncoding.EncodeToString
	return "2." + b64(iv) + "|" + b64(ct) + "|" + b64(mac.Sum(nil))
}

// bwsTestWireSecret encrypts the plaintext fields with the org key.
func bwsTestWireSecret(t *testing.T, orgKey []byte, s bws.Secret) bws.Secret {
	t.Helper()
	s.Key = bwsTestEncrypt(t, s.Key, orgKey)
	s.Value = bwsTestEncrypt(t, s.Value, orgKey)
	s.Note = bwsTestEncrypt(t, s.Note, orgKey)
	return s
}

// bwsTestSecrets is the standard two-secret fixture: "first" (read-only,
// one project) and "second" (read-write, no projects).
func bwsTestSecrets(t *testing.T, orgKey []byte) map[string]bws.Secret {
	t.Helper()
	idA := "11111111-2222-3333-4444-555555555555"
	idB := "22222222-2222-3333-4444-555555555555"
	return map[string]bws.Secret{
		idA: {
			ID: idA, OrganizationID: bwsTestOrgID, Key: "first", Value: "value-a", Note: "note-a",
			CanRead: true, CanWrite: false,
			RevisionDate: time.Date(2026, 11, 8, 10, 0, 0, 0, time.UTC),
			Projects:     []string{"aaaaaaaa-0000-0000-0000-000000000000"},
		},
		idB: {
			ID: idB, OrganizationID: bwsTestOrgID, Key: "second", Value: "value-b", Note: "",
			CanRead: true, CanWrite: true,
			RevisionDate: time.Date(2026, 11, 8, 11, 0, 0, 0, time.UTC),
			Projects:     []string{},
		},
	}
}

// bwsTestLogin runs one bws login against the fakes and fails the test on
// error. The token always rides argv here (tests asserting the env path use
// t.Setenv + their own runRoot call).
func bwsTestLogin(t *testing.T, apiURL, identityURL string, extra ...string) string {
	t.Helper()
	args := []string{"bws", "login", "--api-url", apiURL, "--identity-url", identityURL, "--access-token", bwsTestToken}
	args = append(args, extra...)
	_, stderr, _, err := runRoot(t, args...)
	if err != nil {
		t.Fatalf("bws login: %v\nstderr: %s", err, stderr)
	}
	return stderr
}

// bwsTestSeedCredential plants one credential in the cache without any
// network (offline logout tests).
func bwsTestSeedCredential(t *testing.T, apiURL string) {
	t.Helper()
	err := bws.SaveCredential(bws.Credential{
		APIToken:    bwsTestToken,
		OrgID:       bwsTestOrgID,
		APIURL:      apiURL,
		IdentityURL: "https://identity.example.invalid",
	})
	if err != nil {
		t.Fatalf("seed credential: %v", err)
	}
}

// bwsTestAssertASCII fails the test when s carries a non-ASCII byte (the
// --plain contract: zero non-ASCII, zero ANSI escapes).
func bwsTestAssertASCII(t *testing.T, label, s string) {
	t.Helper()
	for i := 0; i < len(s); i++ {
		if s[i] > 127 {
			t.Fatalf("%s carries a non-ASCII byte %02x at offset %d", label, s[i], i)
		}
	}
}

// --- login ---

func TestBWSLoginE2E(t *testing.T) {
	bwsTestTokenDir(t)
	orgKey := bwsTestOrgKey(t)
	fi := newBWSTestIdentity(t)
	fa := newBWSTestAPI(t, orgKey, bwsTestSecrets(t, orgKey))

	stderr := bwsTestLogin(t, fa.url, fi.url, "--org", bwsTestOrgID)

	// The stderr report: machine account, instance URLs, org state.
	if !strings.Contains(stderr, "machine account cli-test-machine") {
		t.Errorf("login stderr %q does not report the machine account", stderr)
	}
	if !strings.Contains(stderr, fa.url) || !strings.Contains(stderr, fi.url) {
		t.Errorf("login stderr %q does not report both instance URLs", stderr)
	}
	if !strings.Contains(stderr, "org "+bwsTestOrgID) {
		t.Errorf("login stderr %q does not report the org state", stderr)
	}

	// The credential is cached with the exact stored fields.
	cred, err := bws.ResolveCredential(fa.url)
	if err != nil {
		t.Fatalf("ResolveCredential after login: %v", err)
	}
	if cred.APIToken != bwsTestToken || cred.OrgID != bwsTestOrgID || cred.APIURL != fa.url || cred.IdentityURL != fi.url {
		t.Errorf("cached credential = %+v, want the login inputs", cred)
	}

	// Org-key round trip through the CACHED credential: a separate run
	// exchanges again against the fake identity and decrypts list keys.
	stdout, _, _, err := runRoot(t, "bws", "list", "--api-url", fa.url)
	if err != nil {
		t.Fatalf("bws list after login: %v", err)
	}
	if !strings.Contains(stdout, "first") || !strings.Contains(stdout, "second") {
		t.Errorf("list stdout %q does not show the decrypted keys", stdout)
	}
}

func TestBWSLoginJSON(t *testing.T) {
	bwsTestTokenDir(t)
	fi := newBWSTestIdentity(t)
	fa := newBWSTestAPI(t, bwsTestOrgKey(t), map[string]bws.Secret{})
	t.Setenv("MCPCLI_BWS_ACCESS_TOKEN", bwsTestToken) // env-sourced: no argv warning

	// Without --org: org_id PRESENT and empty (constant key set).
	stdout, stderr, _, err := runRoot(t, "--json", "bws", "login", "--api-url", fa.url, "--identity-url", fi.url)
	if err != nil {
		t.Fatalf("login --json: %v", err)
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal([]byte(stdout), &raw); err != nil {
		t.Fatalf("login --json stdout %q is not JSON: %v", stdout, err)
	}
	for _, key := range []string{"ok", "api_url", "identity_url", "org_id", "machine_account"} {
		if _, ok := raw[key]; !ok {
			t.Errorf("login --json document lacks key %q: %s", key, stdout)
		}
	}
	var ok bool
	var orgID, apiURL string
	_ = json.Unmarshal(raw["ok"], &ok)
	_ = json.Unmarshal(raw["org_id"], &orgID)
	_ = json.Unmarshal(raw["api_url"], &apiURL)
	if !ok || orgID != "" || apiURL != fa.url {
		t.Errorf("login --json ok/org_id/api_url = %v/%q/%q, want true/\"\"/%s", ok, orgID, apiURL, fa.url)
	}
	// JSON mode silences ui decoration (warnings aside, there are none from
	// an env-sourced token).
	if strings.Contains(stderr, "logged in") || strings.Contains(stderr, "machine account") {
		t.Errorf("login --json stderr carries ui decoration: %q", stderr)
	}

	// With --org: the full cell.
	stdout, _, _, err = runRoot(t, "--json", "bws", "login", "--api-url", fa.url, "--identity-url", fi.url, "--org", bwsTestOrgID)
	if err != nil {
		t.Fatalf("login --json with org: %v", err)
	}
	raw = map[string]json.RawMessage{}
	if err := json.Unmarshal([]byte(stdout), &raw); err != nil {
		t.Fatalf("login --json stdout %q is not JSON: %v", stdout, err)
	}
	_ = json.Unmarshal(raw["org_id"], &orgID)
	if orgID != bwsTestOrgID {
		t.Errorf("login --json org_id = %q, want %q", orgID, bwsTestOrgID)
	}
}

func TestBWSLoginWarnsArgvToken(t *testing.T) {
	bwsTestTokenDir(t)
	fi := newBWSTestIdentity(t)
	fa := newBWSTestAPI(t, bwsTestOrgKey(t), map[string]bws.Secret{})

	_, stderr, _, err := runRoot(t, "bws", "login", "--api-url", fa.url, "--identity-url", fi.url, "--access-token", bwsTestToken)
	if err != nil {
		t.Fatalf("login with argv token: %v", err)
	}
	// The third warning variant tail: the env named here is the BWS one.
	if !strings.Contains(stderr, "prefer the MCPCLI_BWS_ACCESS_TOKEN environment variable") {
		t.Errorf("login stderr %q lacks the argv-token warning tail", stderr)
	}
}

func TestBWSLoginRejectsBadOrg(t *testing.T) {
	bwsTestTokenDir(t)
	fi := newBWSTestIdentity(t)
	fa := newBWSTestAPI(t, bwsTestOrgKey(t), map[string]bws.Secret{})

	_, _, _, err := runRoot(t, "bws", "login", "--api-url", fa.url, "--identity-url", fi.url, "--access-token", bwsTestToken, "--org", "not-a-uuid")
	if err == nil || !strings.Contains(err.Error(), "--org") || !strings.Contains(err.Error(), "not-a-uuid") {
		t.Fatalf("login with bad org = %v, want an error naming --org and the value", err)
	}
	// Rejected before any network: the identity fake saw no exchange.
	if n := fi.exchangeHits(); n != 0 {
		t.Errorf("bad-org login reached the identity server %d times, want 0", n)
	}
	// Nothing cached.
	if _, err := bws.ResolveCredential(""); err == nil {
		t.Error("bad-org login cached a credential")
	}
}

func TestBWSLoginDefaults(t *testing.T) {
	bwsTestTokenDir(t)
	fi := newBWSTestIdentity(t)

	// No --api-url: the default applies BEFORE normalization and lands in
	// the cache normalized. The identity side points at the fake so the
	// validation exchange stays on the loopback.
	if _, stderr, _, err := runRoot(t, "bws", "login", "--identity-url", fi.url, "--access-token", bwsTestToken); err != nil {
		t.Fatalf("login with defaults: %v\nstderr: %s", err, stderr)
	}
	cred, err := bws.ResolveCredential("https://api.bitwarden.com")
	if err != nil {
		t.Fatalf("ResolveCredential(default api url): %v", err)
	}
	if cred.APIURL != "https://api.bitwarden.com" {
		t.Errorf("cached APIURL = %q, want the applied default", cred.APIURL)
	}
	if cred.IdentityURL != fi.url {
		t.Errorf("cached IdentityURL = %q, want the fake %q", cred.IdentityURL, fi.url)
	}
}

func TestBWSLoginHTTPWarns(t *testing.T) {
	bwsTestTokenDir(t)
	fi := newBWSTestIdentity(t)
	fa := newBWSTestAPI(t, bwsTestOrgKey(t), map[string]bws.Secret{})

	// httptest serves plain http: both resolved URLs warn, and the warning
	// is not an error (the no-blocklist ruling).
	_, stderr, _, err := runRoot(t, "bws", "login", "--api-url", fa.url, "--identity-url", fi.url, "--access-token", bwsTestToken)
	if err != nil {
		t.Fatalf("login over http: %v", err)
	}
	if !strings.Contains(stderr, "--api-url is plain http://") {
		t.Errorf("login stderr %q lacks the --api-url http warning", stderr)
	}
	if !strings.Contains(stderr, "--identity-url is plain http://") {
		t.Errorf("login stderr %q lacks the --identity-url http warning", stderr)
	}
}

// --- logout ---

func TestBWSLogoutRemovesEntry(t *testing.T) {
	bwsTestTokenDir(t)
	bwsTestSeedCredential(t, "https://api.example.one")

	_, stderr, _, err := runRoot(t, "bws", "logout")
	if err != nil {
		t.Fatalf("bws logout: %v", err)
	}
	if !strings.Contains(stderr, "logged out of https://api.example.one") {
		t.Errorf("logout stderr %q does not name the removed instance", stderr)
	}
	if _, err := bws.ResolveCredential(""); err == nil || !strings.Contains(err.Error(), "no Bitwarden instance is logged in") {
		t.Errorf("cache after logout = %v, want the no-instance error", err)
	}
}

func TestBWSLogoutRequiresFlagOnN(t *testing.T) {
	bwsTestTokenDir(t)
	bwsTestSeedCredential(t, "https://api.example.one")
	bwsTestSeedCredential(t, "https://api.example.two")

	_, _, _, err := runRoot(t, "bws", "logout")
	if err == nil {
		t.Fatal("logout with two cached instances: nil error")
	}
	msg := err.Error()
	i, j := strings.Index(msg, "https://api.example.one"), strings.Index(msg, "https://api.example.two")
	if i < 0 || j < 0 {
		t.Fatalf("error %q does not list both cached instances", msg)
	}
	if i > j {
		t.Errorf("error %q lists the instances unsorted", msg)
	}
	if !strings.Contains(msg, "--api-url") {
		t.Errorf("error %q lacks the --api-url remediation", msg)
	}
	// Fail-closed: both entries survive.
	if _, err := bws.ResolveCredential("https://api.example.one"); err != nil {
		t.Errorf("instance one no longer cached after failed logout: %v", err)
	}
	if _, err := bws.ResolveCredential("https://api.example.two"); err != nil {
		t.Errorf("instance two no longer cached after failed logout: %v", err)
	}
}

func TestBWSLogoutFlagNotInCacheFailsClosed(t *testing.T) {
	bwsTestTokenDir(t)
	bwsTestSeedCredential(t, "https://api.example.one")
	bwsTestSeedCredential(t, "https://api.example.two")

	_, _, _, err := runRoot(t, "bws", "logout", "--api-url", "https://api.example.three")
	if err == nil {
		t.Fatal("logout of an uncached instance: nil error")
	}
	msg := err.Error()
	if !strings.Contains(msg, "https://api.example.one") || !strings.Contains(msg, "https://api.example.two") {
		t.Fatalf("error %q does not name the cached instances", msg)
	}
	if _, err := bws.ResolveCredential("https://api.example.one"); err != nil {
		t.Errorf("instance one no longer cached after failed logout: %v", err)
	}
	if _, err := bws.ResolveCredential("https://api.example.two"); err != nil {
		t.Errorf("instance two no longer cached after failed logout: %v", err)
	}
}

func TestBWSLogoutZeroIdempotent(t *testing.T) {
	bwsTestTokenDir(t)

	_, stderr, _, err := runRoot(t, "bws", "logout")
	if err != nil {
		t.Fatalf("logout with an empty cache: %v", err)
	}
	if !strings.Contains(stderr, "logged out") {
		t.Errorf("logout stderr %q does not report success", stderr)
	}
}

func TestBWSLogoutIgnoresEnv(t *testing.T) {
	bwsTestTokenDir(t)
	bwsTestSeedCredential(t, "https://api.example.one")
	// The env selector points elsewhere; logout must ignore it and apply the
	// one-entry rule anyway.
	t.Setenv("MCPCLI_BWS_API_URL", "https://env-selector.example")

	_, stderr, _, err := runRoot(t, "bws", "logout")
	if err != nil {
		t.Fatalf("logout with env set: %v", err)
	}
	if !strings.Contains(stderr, "logged out of https://api.example.one") {
		t.Errorf("logout stderr %q does not name the removed instance", stderr)
	}
	if _, err := bws.ResolveCredential(""); err == nil {
		t.Error("cache not empty after logout")
	}
}

func TestBWSLogoutJSONSilent(t *testing.T) {
	bwsTestTokenDir(t)
	bwsTestSeedCredential(t, "https://api.example.one")

	stdout, stderr, dec, err := runRoot(t, "--json", "bws", "logout")
	if err != nil {
		t.Fatalf("logout --json: %v", err)
	}
	if stdout != "" || stderr != "" || dec != "" {
		t.Errorf("logout --json wrote output: stdout %q, stderr %q, decoration %q", stdout, stderr, dec)
	}
	if _, err := bws.ResolveCredential(""); err == nil {
		t.Error("cache not empty after logout")
	}
}

// --- list ---

func TestBWSListE2E(t *testing.T) {
	bwsTestTokenDir(t)
	orgKey := bwsTestOrgKey(t)
	fi := newBWSTestIdentity(t)
	fa := newBWSTestAPI(t, orgKey, bwsTestSecrets(t, orgKey))
	bwsTestLogin(t, fa.url, fi.url, "--org", bwsTestOrgID)

	stdout, stderr, _, err := runRoot(t, "bws", "list", "--api-url", fa.url)
	if err != nil {
		t.Fatalf("bws list: %v", err)
	}
	// The always-on instance note names the resolved instance.
	if !strings.Contains(stderr, "bws: using instance "+fa.url) {
		t.Errorf("list stderr %q lacks the instance note", stderr)
	}
	// The metadata table: uppercase headers, decrypted keys, project ids.
	for _, want := range []string{"KEY", "READ", "WRITE", "PROJECTS", "first", "second", "aaaaaaaa-0000-0000-0000-000000000000"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("list stdout lacks %q:\n%s", want, stdout)
		}
	}

	// --plain: zero non-ASCII anywhere (ASCII borders, ASCII grant labels).
	stdout, stderr, dec, err := runRoot(t, "--plain", "bws", "list", "--api-url", fa.url)
	if err != nil {
		t.Fatalf("bws list --plain: %v", err)
	}
	bwsTestAssertASCII(t, "list --plain stdout", stdout)
	bwsTestAssertASCII(t, "list --plain stderr", stderr)
	bwsTestAssertASCII(t, "list --plain decoration", dec)

	// A 403 on the list surfaces the client's remediation pointer.
	fa.forceList(http.StatusForbidden)
	_, _, _, err = runRoot(t, "bws", "list", "--api-url", fa.url)
	fa.forceList(0)
	if err == nil || !strings.Contains(err.Error(), "machine account") || !strings.Contains(err.Error(), "Bitwarden") {
		t.Errorf("403 list error = %v, want the machine-account remediation", err)
	}
}

func TestBWSListJSON(t *testing.T) {
	bwsTestTokenDir(t)
	fi := newBWSTestIdentity(t)

	// Zero secrets: secrets present, empty, NOT nil.
	faEmpty := newBWSTestAPI(t, bwsTestOrgKey(t), map[string]bws.Secret{})
	bwsTestLogin(t, faEmpty.url, fi.url, "--org", bwsTestOrgID)
	stdout, stderr, _, err := runRoot(t, "--json", "bws", "list", "--api-url", faEmpty.url)
	if err != nil {
		t.Fatalf("list --json (zero): %v", err)
	}
	if !strings.Contains(stderr, "bws: using instance "+faEmpty.url) {
		t.Errorf("list --json stderr %q lacks the always-on instance note", stderr)
	}
	var zero struct {
		APIURL  string           `json:"api_url"`
		OrgID   string           `json:"org_id"`
		Secrets []map[string]any `json:"secrets"`
	}
	if err := json.Unmarshal([]byte(stdout), &zero); err != nil {
		t.Fatalf("list --json stdout %q is not JSON: %v", stdout, err)
	}
	if zero.Secrets == nil || len(zero.Secrets) != 0 {
		t.Errorf("zero-secrets secrets = %#v, want [] (non-nil)", zero.Secrets)
	}
	if zero.APIURL != faEmpty.url || zero.OrgID != bwsTestOrgID {
		t.Errorf("api_url/org_id = %q/%q, want %q/%q", zero.APIURL, zero.OrgID, faEmpty.url, bwsTestOrgID)
	}

	// One secret: per-item EXACT field set.
	orgKey := bwsTestOrgKey(t)
	fa := newBWSTestAPI(t, orgKey, bwsTestSecrets(t, orgKey))
	bwsTestLogin(t, fa.url, fi.url, "--org", bwsTestOrgID)
	stdout, _, _, err = runRoot(t, "--json", "bws", "list", "--api-url", fa.url)
	if err != nil {
		t.Fatalf("list --json (one): %v", err)
	}
	var one struct {
		Secrets []json.RawMessage `json:"secrets"`
	}
	if err := json.Unmarshal([]byte(stdout), &one); err != nil {
		t.Fatalf("list --json stdout %q is not JSON: %v", stdout, err)
	}
	if len(one.Secrets) != 2 {
		t.Fatalf("listed %d secrets, want 2", len(one.Secrets))
	}
	var item map[string]json.RawMessage
	if err := json.Unmarshal(one.Secrets[0], &item); err != nil {
		t.Fatalf("item decode: %v", err)
	}
	keys := make([]string, 0, len(item))
	for k := range item {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	wantKeys := []string{"id", "key", "project_ids", "read", "revision_date", "write"}
	if !slices.Equal(keys, wantKeys) {
		t.Errorf("item field set = %v, want exactly %v", keys, wantKeys)
	}
	var meta struct {
		ID           string   `json:"id"`
		Key          string   `json:"key"`
		ProjectIDs   []string `json:"project_ids"`
		Read         bool     `json:"read"`
		Write        bool     `json:"write"`
		RevisionDate string   `json:"revision_date"`
	}
	if err := json.Unmarshal(one.Secrets[0], &meta); err != nil {
		t.Fatalf("item typed decode: %v", err)
	}
	if meta.Key != "first" || !meta.Read || meta.Write || meta.RevisionDate == "" {
		t.Errorf("first item = %+v", meta)
	}
	if len(meta.ProjectIDs) != 1 || meta.ProjectIDs[0] != "aaaaaaaa-0000-0000-0000-000000000000" {
		t.Errorf("first item project_ids = %v", meta.ProjectIDs)
	}
}

func TestBWSListNoCredentialsError(t *testing.T) {
	bwsTestTokenDir(t)

	_, _, _, err := runRoot(t, "bws", "list")
	if err == nil || !strings.Contains(err.Error(), `run "mcp bws login"`) {
		t.Fatalf("list with no credentials = %v, want the run-mcp-bws-login golden", err)
	}
}

func TestBWSListNEntriesGolden(t *testing.T) {
	bwsTestTokenDir(t)
	bwsTestSeedCredential(t, "https://api.example.one")
	bwsTestSeedCredential(t, "https://api.example.two")

	_, _, _, err := runRoot(t, "bws", "list")
	if err == nil {
		t.Fatal("list with two cached instances: nil error")
	}
	msg := err.Error()
	if !strings.Contains(msg, "multiple Bitwarden instances") {
		t.Fatalf("error %q is not the multiple-instances golden", msg)
	}
	i, j := strings.Index(msg, "https://api.example.one"), strings.Index(msg, "https://api.example.two")
	if i < 0 || j < 0 || i > j {
		t.Fatalf("error %q does not list the cached instances sorted", msg)
	}
	if !strings.Contains(msg, "--api-url") {
		t.Errorf("error %q lacks the --api-url remediation", msg)
	}
}

// --- get ---

func TestBWSGetE2E(t *testing.T) {
	bwsTestTokenDir(t)
	orgKey := bwsTestOrgKey(t)
	fi := newBWSTestIdentity(t)
	fa := newBWSTestAPI(t, orgKey, bwsTestSecrets(t, orgKey))
	bwsTestLogin(t, fa.url, fi.url, "--org", bwsTestOrgID)

	// Ref order governs output order; values land on stdout, the instance
	// note on stderr.
	stdout, stderr, _, err := runRoot(t, "bws", "get", "--api-url", fa.url, "second", "first")
	if err != nil {
		t.Fatalf("bws get: %v", err)
	}
	if want := "second=value-b\nfirst=value-a\n"; stdout != want {
		t.Errorf("get stdout = %q, want %q", stdout, want)
	}
	if !strings.Contains(stderr, "bws: using instance "+fa.url) {
		t.Errorf("get stderr %q lacks the instance note", stderr)
	}

	// --plain: zero non-ASCII.
	stdout, stderr, dec, err := runRoot(t, "--plain", "bws", "get", "--api-url", fa.url, "first")
	if err != nil {
		t.Fatalf("bws get --plain: %v", err)
	}
	bwsTestAssertASCII(t, "get --plain stdout", stdout)
	bwsTestAssertASCII(t, "get --plain stderr", stderr)
	bwsTestAssertASCII(t, "get --plain decoration", dec)
}

func TestBWSGetJSON(t *testing.T) {
	bwsTestTokenDir(t)
	orgKey := bwsTestOrgKey(t)
	fi := newBWSTestIdentity(t)
	fa := newBWSTestAPI(t, orgKey, bwsTestSecrets(t, orgKey))
	bwsTestLogin(t, fa.url, fi.url, "--org", bwsTestOrgID)

	stdout, _, _, err := runRoot(t, "--json", "bws", "get", "--api-url", fa.url, "second", "first")
	if err != nil {
		t.Fatalf("bws get --json: %v", err)
	}
	var res struct {
		Secrets []json.RawMessage `json:"secrets"`
	}
	if err := json.Unmarshal([]byte(stdout), &res); err != nil {
		t.Fatalf("get --json stdout %q is not JSON: %v", stdout, err)
	}
	if len(res.Secrets) != 2 {
		t.Fatalf("got %d secrets, want 2 (ref order)", len(res.Secrets))
	}
	for i, wantKey := range []string{"second", "first"} {
		var item map[string]json.RawMessage
		if err := json.Unmarshal(res.Secrets[i], &item); err != nil {
			t.Fatalf("secret %d decode: %v", i, err)
		}
		keys := make([]string, 0, len(item))
		for k := range item {
			keys = append(keys, k)
		}
		slices.Sort(keys)
		wantKeys := []string{"id", "key", "note", "project_ids", "read", "revision_date", "value", "write"}
		if !slices.Equal(keys, wantKeys) {
			t.Errorf("secret %d field set = %v, want exactly %v", i, keys, wantKeys)
		}
		var full struct {
			Key   string `json:"key"`
			Value string `json:"value"`
			Note  string `json:"note"`
		}
		if err := json.Unmarshal(res.Secrets[i], &full); err != nil {
			t.Fatalf("secret %d typed decode: %v", i, err)
		}
		if full.Key != wantKey {
			t.Errorf("secret %d key = %q, want %q (ref order)", i, full.Key, wantKey)
		}
	}
	var second struct {
		Value string `json:"value"`
		Note  string `json:"note"`
	}
	_ = json.Unmarshal(res.Secrets[0], &second)
	if second.Value != "value-b" || second.Note != "" {
		t.Errorf("second secret value/note = %q/%q, want value-b/\"\"", second.Value, second.Note)
	}
}

func TestBWSGetZeroArgsGolden(t *testing.T) {
	bwsTestTokenDir(t)

	_, _, _, err := runRoot(t, "bws", "get")
	if err == nil || err.Error() != "requires at least 1 arg(s), only received 0" {
		t.Fatalf("bws get with no refs = %v, want the pinned MinimumNArgs golden", err)
	}
}

func TestBWSGetAbortOnFirst(t *testing.T) {
	bwsTestTokenDir(t)
	orgKey := bwsTestOrgKey(t)
	fi := newBWSTestIdentity(t)
	fa := newBWSTestAPI(t, orgKey, bwsTestSecrets(t, orgKey))
	bwsTestLogin(t, fa.url, fi.url, "--org", bwsTestOrgID)

	badRef := "99999999-2222-3333-4444-555555555555"

	// (a) The first failing ref stops the loop and is named in the error.
	_, _, _, err := runRoot(t, "bws", "get", "--api-url", fa.url, badRef, "first")
	if err == nil || !strings.Contains(err.Error(), badRef) {
		t.Fatalf("get with a failing first ref = %v, want an error naming %q", err, badRef)
	}
	if n := fa.getHits(); n != 1 {
		t.Errorf("api fake saw %d GETs, want 1 (the loop stopped at the first failure)", n)
	}

	// (b) In JSON mode the refs resolved before the failure still emit their
	// partial document before the error (the third stdout-on-failure class).
	stdout, _, _, err := runRoot(t, "--json", "bws", "get", "--api-url", fa.url, "first", badRef)
	if err == nil || !strings.Contains(err.Error(), badRef) {
		t.Fatalf("get --json with a failing second ref = %v, want an error naming %q", err, badRef)
	}
	var res struct {
		Secrets []struct {
			Key string `json:"key"`
		} `json:"secrets"`
	}
	if err := json.Unmarshal([]byte(stdout), &res); err != nil {
		t.Fatalf("partial get --json stdout %q is not JSON: %v", stdout, err)
	}
	if len(res.Secrets) != 1 || res.Secrets[0].Key != "first" {
		t.Errorf("partial secrets = %+v, want exactly the one resolved ref", res.Secrets)
	}
}

func TestBWSGetRefShapedRefGolden(t *testing.T) {
	bwsTestTokenDir(t) // no login needed: ref-shape validation precedes resolution

	_, _, _, err := runRoot(t, "bws", "get", "bws://name")
	if err == nil {
		t.Fatal("bws get with a bws://-shaped ref: nil error")
	}
	msg := err.Error()
	if !strings.Contains(msg, "bws://name") || !strings.Contains(msg, `"name"`) {
		t.Fatalf("error %q does not name the ref and its bare form", msg)
	}
	if !strings.Contains(msg, "--env") {
		t.Errorf("error %q lacks the bws://-grammar pointer", msg)
	}
}

func TestBWSGetNoCredentialsError(t *testing.T) {
	bwsTestTokenDir(t)

	_, _, _, err := runRoot(t, "bws", "get", "name")
	if err == nil || !strings.Contains(err.Error(), `run "mcp bws login"`) {
		t.Fatalf("get with no credentials = %v, want the run-mcp-bws-login golden", err)
	}
}

// --- multi-instance ---

func TestBWSMultiInstance(t *testing.T) {
	bwsTestTokenDir(t)
	orgKey := bwsTestOrgKey(t)
	fiA := newBWSTestIdentity(t)
	idA := "11111111-2222-3333-4444-555555555555"
	faA := newBWSTestAPI(t, orgKey, map[string]bws.Secret{
		idA: {ID: idA, OrganizationID: bwsTestOrgID, Key: "alpha", Value: "value-a", CanRead: true},
	})
	fiB := newBWSTestIdentity(t)
	idB := "22222222-2222-3333-4444-555555555555"
	faB := newBWSTestAPI(t, orgKey, map[string]bws.Secret{
		idB: {ID: idB, OrganizationID: bwsTestOrgID, Key: "beta", Value: "value-b", CanRead: true},
	})

	bwsTestLogin(t, faA.url, fiA.url, "--org", bwsTestOrgID)
	bwsTestLogin(t, faB.url, fiB.url, "--org", bwsTestOrgID)

	// list resolves per --api-url: each instance shows only its own secret,
	// and the note names the resolved instance.
	stdout, stderr, _, err := runRoot(t, "bws", "list", "--api-url", faA.url)
	if err != nil {
		t.Fatalf("list A: %v", err)
	}
	if !strings.Contains(stdout, "alpha") || strings.Contains(stdout, "beta") {
		t.Errorf("list A stdout %q mixes instances", stdout)
	}
	if !strings.Contains(stderr, "bws: using instance "+faA.url) {
		t.Errorf("list A stderr %q names the wrong instance", stderr)
	}
	stdout, _, _, err = runRoot(t, "bws", "list", "--api-url", faB.url)
	if err != nil {
		t.Fatalf("list B: %v", err)
	}
	if !strings.Contains(stdout, "beta") || strings.Contains(stdout, "alpha") {
		t.Errorf("list B stdout %q mixes instances", stdout)
	}

	// logout removes exactly the flagged instance.
	_, stderr, _, err = runRoot(t, "bws", "logout", "--api-url", faB.url)
	if err != nil {
		t.Fatalf("logout B: %v", err)
	}
	if !strings.Contains(stderr, "logged out of "+faB.url) {
		t.Errorf("logout B stderr %q does not name the removed instance", stderr)
	}
	if _, err := bws.ResolveCredential(faB.url); err == nil {
		t.Error("instance B still cached after logout")
	}
	if _, err := bws.ResolveCredential(faA.url); err != nil {
		t.Errorf("instance A no longer cached after logout B: %v", err)
	}
	// And B's list now fails closed naming the remaining instance.
	_, _, _, err = runRoot(t, "bws", "list", "--api-url", faB.url)
	if err == nil || !strings.Contains(err.Error(), faA.url) {
		t.Fatalf("list on the logged-out instance = %v, want a miss naming %q", err, faA.url)
	}
}
