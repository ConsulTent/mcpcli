package bws

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// Every credential-shaped value in this file is derived per test from
// crypto/rand or an obviously-synthetic constant; the operator's real
// machine account, org id and secret ids must never appear in test source.

// baseForms pins the two instance forms the live contract verified: a
// dedicated base (https://api.bitwarden.com) and a path-prefixed one
// (https://vault.bitwarden.eu/api). The fakes mount under the given path
// prefixes so both forms must pass over arbitrary loopback bases.
var baseForms = []struct {
	name       string
	apiMount   string
	identMount string
}{
	{"dedicated", "", ""},
	{"path-prefixed", "/api", "/identity"},
}

// testOrgKey returns a random 64-byte org key (the decrypted
// encrypted_payload contents).
func testOrgKey(t *testing.T) []byte {
	t.Helper()
	key := make([]byte, 64)
	if _, err := rand.Read(key); err != nil {
		t.Fatalf("rand org key: %v", err)
	}
	return key
}

// dummyAccessToken builds an obviously-synthetic machine-account access
// token "0.<id>.<secret>:<b64 seed>" with a random 16-byte seed and returns
// its parts.
func dummyAccessToken(t *testing.T) (token, clientID, clientSecret string, seed []byte) {
	t.Helper()
	seed = make([]byte, 16)
	if _, err := rand.Read(seed); err != nil {
		t.Fatalf("rand seed: %v", err)
	}
	clientID = randomHex(t, 8)
	clientSecret = randomHex(t, 16)
	token = fmt.Sprintf("0.%s.%s:%s", clientID, clientSecret, base64.StdEncoding.EncodeToString(seed))
	return token, clientID, clientSecret, seed
}

func randomHex(t *testing.T, n int) string {
	t.Helper()
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		t.Fatalf("rand: %v", err)
	}
	return hex.EncodeToString(b)
}

// fakeIdentity is an httptest BWS identity server. It counts exchanges,
// records the observed exchange request, and serves an encrypted_payload
// that decrypts (under the seed-derived key) to the org-key payload.
type fakeIdentity struct {
	mu        sync.Mutex
	url       string
	exchanges int
	lastPath  string
	lastForm  map[string][]string
	lastType  string
	// knobs for failure variants
	status int
	body   string
}

func newFakeIdentity(t *testing.T, mount, token string, orgKey []byte) *fakeIdentity {
	t.Helper()
	fi := &fakeIdentity{}
	mux := http.NewServeMux()
	mux.HandleFunc("POST "+mount+"/connect/token", func(w http.ResponseWriter, r *http.Request) {
		fi.mu.Lock()
		fi.exchanges++
		fi.lastPath = r.URL.Path
		fi.lastType = r.Header.Get("Content-Type")
		_ = r.ParseForm()
		fi.lastForm = r.PostForm
		status, body := fi.status, fi.body
		fi.mu.Unlock()

		if status != 0 {
			w.WriteHeader(status)
			_, _ = w.Write([]byte(body))
			return
		}
		_, _, seed, err := ParseAccessToken(token)
		if err != nil {
			http.Error(w, "fake cannot parse its own token: "+err.Error(), http.StatusInternalServerError)
			return
		}
		encKey, macKey, err := deriveSeedKey(seed)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		payload := fmt.Sprintf(`{"encryptionKey":%q}`, base64.StdEncoding.EncodeToString(orgKey))
		enc, err := encryptEncString(payload, encKey, macKey)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		// The JWT is opaque to the client; the fake emits a dummy value.
		_, _ = fmt.Fprintf(w, `{"access_token":%q,"encrypted_payload":%q}`, "dummy.jwt.value", string(enc))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	fi.url = srv.URL + mount
	return fi
}

func (fi *fakeIdentity) exchangeCount() int {
	fi.mu.Lock()
	defer fi.mu.Unlock()
	return fi.exchanges
}

func (fi *fakeIdentity) forceResponse(status int, body string) {
	fi.mu.Lock()
	defer fi.mu.Unlock()
	fi.status, fi.body = status, body
}

// observed returns the recorded exchange request details.
func (fi *fakeIdentity) observed() (path string, form map[string][]string, contentType string) {
	fi.mu.Lock()
	defer fi.mu.Unlock()
	return fi.lastPath, fi.lastForm, fi.lastType
}

// fakeAPI is an httptest BWS API server serving secrets in wire form
// (EncString fields encrypted with the org key).
type fakeAPI struct {
	mu      sync.Mutex
	url     string
	orgKey  []byte
	secrets map[string]Secret // wire form
	names   []string          // ids in list order
	// knobs
	forceListStatus   int
	forceGetStatus    int
	forcePutStatus    int
	continuation      string
	putBodies         []string
	gets, lists, puts int
}

func newFakeAPI(t *testing.T, mount string, orgKey []byte, secrets map[string]Secret) *fakeAPI {
	t.Helper()
	fa := &fakeAPI{orgKey: orgKey, secrets: secrets}
	for id := range secrets {
		fa.names = append(fa.names, id)
	}
	// Deterministic list order for golden-ish assertions.
	for i := 0; i < len(fa.names); i++ {
		for j := i + 1; j < len(fa.names); j++ {
			if fa.names[j] < fa.names[i] {
				fa.names[i], fa.names[j] = fa.names[j], fa.names[i]
			}
		}
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET "+mount+"/secrets/{id}", func(w http.ResponseWriter, r *http.Request) {
		fa.mu.Lock()
		fa.gets++
		status := fa.forceGetStatus
		fa.mu.Unlock()
		if status != 0 {
			w.WriteHeader(status)
			return
		}
		s, ok := fa.secrets[r.PathValue("id")]
		if !ok {
			http.Error(w, "no such secret", http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(s)
	})
	mux.HandleFunc("GET "+mount+"/organizations/{org}/secrets", func(w http.ResponseWriter, r *http.Request) {
		fa.mu.Lock()
		fa.lists++
		status, cont := fa.forceListStatus, fa.continuation
		fa.mu.Unlock()
		if status != 0 {
			w.WriteHeader(status)
			return
		}
		items := make([]Secret, 0, len(fa.secrets))
		for _, id := range fa.names {
			items = append(items, fa.secrets[id])
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"object":            "secretListView",
			"projects":          []string{},
			"secrets":           items,
			"continuationToken": cont,
		})
	})
	mux.HandleFunc("PUT "+mount+"/secrets/{id}", func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		fa.mu.Lock()
		fa.puts++
		fa.putBodies = append(fa.putBodies, string(raw))
		status := fa.forcePutStatus
		fa.mu.Unlock()
		if status != 0 {
			w.WriteHeader(status)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte("{}"))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	fa.url = srv.URL + mount
	return fa
}

func (fa *fakeAPI) forceList(status int) {
	fa.mu.Lock()
	defer fa.mu.Unlock()
	fa.forceListStatus = status
}

func (fa *fakeAPI) forcePut(status int) {
	fa.mu.Lock()
	defer fa.mu.Unlock()
	fa.forcePutStatus = status
}

func (fa *fakeAPI) recordedPuts() []string {
	fa.mu.Lock()
	defer fa.mu.Unlock()
	out := make([]string, len(fa.putBodies))
	copy(out, fa.putBodies)
	return out
}

func (fa *fakeAPI) setContinuation(tok string) {
	fa.mu.Lock()
	defer fa.mu.Unlock()
	fa.continuation = tok
}

// wireSecret encrypts the plaintext fields with the org key, producing the
// wire-form Secret the fake serves. Project names ride the same org-key
// EncString form as the live wire's per-item projects objects.
func wireSecret(t *testing.T, orgKey []byte, s Secret) Secret {
	t.Helper()
	enc, mac := orgKey[:32], orgKey[32:64]
	var err error
	if s.Key, err = encField(s.Key, enc, mac); err != nil {
		t.Fatalf("encrypt key: %v", err)
	}
	if s.Value, err = encField(s.Value, enc, mac); err != nil {
		t.Fatalf("encrypt value: %v", err)
	}
	if s.Note, err = encField(s.Note, enc, mac); err != nil {
		t.Fatalf("encrypt note: %v", err)
	}
	for i := range s.Projects {
		if s.Projects[i].Name, err = encField(s.Projects[i].Name, enc, mac); err != nil {
			t.Fatalf("encrypt project %s name: %v", s.Projects[i].ID, err)
		}
	}
	return s
}

// encField encrypts one plaintext field to its wire EncString form.
func encField(plain string, enc, mac []byte) (string, error) {
	wire, err := encryptEncString(plain, enc, mac)
	if err != nil {
		return "", err
	}
	return string(wire), nil
}

// buildEncStringRaw assembles an EncString from raw parts with test-side
// crypto (crypto/aes + crypto/hmac directly, no package helpers) so
// shape-error fixtures are independent of the code under test. plaintext
// must already be padded.
func buildEncStringRaw(t *testing.T, encKey, macKey, iv, plaintext []byte) EncString {
	t.Helper()
	block, err := aes.NewCipher(encKey)
	if err != nil {
		t.Fatalf("aes cipher: %v", err)
	}
	if len(plaintext)%aes.BlockSize != 0 {
		t.Fatalf("test plaintext must be padded, len %d", len(plaintext))
	}
	ct := make([]byte, len(plaintext))
	cipher.NewCBCEncrypter(block, iv).CryptBlocks(ct, plaintext)
	mac := hmac.New(sha256.New, macKey)
	mac.Write(iv)
	mac.Write(ct)
	b64 := base64.StdEncoding.EncodeToString
	return EncString("2." + b64(iv) + "|" + b64(ct) + "|" + b64(mac.Sum(nil)))
}

// known-answer vectors, computed once with an independent manual RFC 5869
// implementation (cross-checked against Python's hmac/hashlib) and hard-
// coded here — never derived from the code under test at test time.
const (
	kaSeedHex    = "77617465726d656c6f6e66726f673039" // "watermelonfrog09", 16 bytes
	kaOKHex      = "82058be8d27c6e7a54e2c1ecfe4991a0216c9cd574fa04c4413b7cb9308186d091a15f3c637b09d93f1a0be3f74ee3099e403db45f5db39a905759304b8e81b0"
	kaOrgKeyB64  = "oKGio6SlpqeoqaqrrK2ur7CxsrO0tba3uLm6u7y9vr/AwcLDxMXGx8jJysvMzc7P0NHS09TV1tfY2drb3N3e3w=="
	kaEncPayload = "2.AAECAwQFBgcICQoLDA0ODw==|Kg10BFz6zEwuUoY+n9hgGHGioJ1GL80naJ1u33dF3LfLRJWb4XrmHPY9EYDqt/BOBP0xrcD6FChk71w6dDmdEcs5BeKECL1sZnezSJcyxaJG9k/RN3Th4Y836ZBQdnD1x1gAb2hZCwOO7aoo/z2TBQ==|X6kPVAK8pa/3wyTFkHGXfU8X4GE+X11gc/A5C3qKQfY="
	kaPlaintext  = `{"encryptionKey":"` + kaOrgKeyB64 + `"}`
)

func unhex(t *testing.T, s string) []byte {
	t.Helper()
	b, err := hex.DecodeString(s)
	if err != nil {
		t.Fatalf("unhex: %v", err)
	}
	return b
}

func TestDeriveSeedKeyKnownAnswer(t *testing.T) {
	seed := unhex(t, kaSeedHex)
	if len(seed) != 16 {
		t.Fatalf("known-answer seed is %d bytes, want 16", len(seed))
	}
	ok := unhex(t, kaOKHex)
	encKey, macKey, err := deriveSeedKey(seed)
	if err != nil {
		t.Fatalf("deriveSeedKey: %v", err)
	}
	if want := ok[:32]; hex.EncodeToString(encKey) != hex.EncodeToString(want) {
		t.Errorf("encKey = %s, want %s", hex.EncodeToString(encKey), hex.EncodeToString(want))
	}
	if want := ok[32:]; hex.EncodeToString(macKey) != hex.EncodeToString(want) {
		t.Errorf("macKey = %s, want %s", hex.EncodeToString(macKey), hex.EncodeToString(want))
	}
}

func TestDecryptEncStringKnownAnswer(t *testing.T) {
	seed := unhex(t, kaSeedHex)
	encKey, macKey, err := deriveSeedKey(seed)
	if err != nil {
		t.Fatalf("deriveSeedKey: %v", err)
	}
	got, err := decryptEncString(kaEncPayload, encKey, macKey)
	if err != nil {
		t.Fatalf("decryptEncString: %v", err)
	}
	if got != kaPlaintext {
		t.Errorf("decrypt = %q, want %q", got, kaPlaintext)
	}
	// And the decrypted payload carries a well-formed 64-byte org key.
	var payload struct {
		EncryptionKey string `json:"encryptionKey"`
	}
	if err := json.Unmarshal([]byte(got), &payload); err != nil {
		t.Fatalf("payload unmarshal: %v", err)
	}
	orgKey, err := base64.StdEncoding.DecodeString(payload.EncryptionKey)
	if err != nil {
		t.Fatalf("org key b64: %v", err)
	}
	if len(orgKey) != 64 {
		t.Errorf("org key length = %d, want 64", len(orgKey))
	}
}

func TestEncStringRoundTrip(t *testing.T) {
	encKey := unhex(t, kaOKHex)[:32]
	macKey := unhex(t, kaOKHex)[32:]
	cases := []struct {
		name      string
		plaintext string
	}{
		{"short", "s3cret-value"},
		{"exactly one block", "0123456789abcdef"},
		{"multi-block", "a much longer secret value that spans several AES blocks for padding coverage"},
		{"empty", ""},
		{"binary-ish", "\x01\x02\xff trailing"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			enc, err := encryptEncString(tc.plaintext, encKey, macKey)
			if err != nil {
				t.Fatalf("encryptEncString: %v", err)
			}
			got, err := decryptEncString(enc, encKey, macKey)
			if err != nil {
				t.Fatalf("decryptEncString: %v", err)
			}
			if got != tc.plaintext {
				t.Errorf("round trip = %q, want %q", got, tc.plaintext)
			}
		})
	}
}

func TestEncryptFreshIV(t *testing.T) {
	encKey := unhex(t, kaOKHex)[:32]
	macKey := unhex(t, kaOKHex)[32:]
	a, err := encryptEncString("same plaintext", encKey, macKey)
	if err != nil {
		t.Fatalf("first encrypt: %v", err)
	}
	b, err := encryptEncString("same plaintext", encKey, macKey)
	if err != nil {
		t.Fatalf("second encrypt: %v", err)
	}
	if a == b {
		t.Error("two encrypts of the same plaintext produced identical EncStrings; IV is not fresh")
	}
}

func TestDecryptEncStringShapeErrors(t *testing.T) {
	encKey := unhex(t, kaOKHex)[:32]
	macKey := unhex(t, kaOKHex)[32:]
	validIV := make([]byte, aes.BlockSize)
	for i := range validIV {
		validIV[i] = byte(i)
	}
	validMAC := make([]byte, sha256.Size) // structurally valid mac length

	cases := []struct {
		name    string
		enc     EncString
		wantSub []string
	}{
		{"wrong type prefix", "1.AAA|BBB|CCC", []string{"1"}},
		{"no dot", "justgarbage", []string{"justgarbage"}},
		{"two parts", "2.AAA|BBB", []string{"2."}},
		{"four parts", "2.AAA|BBB|CCC|DDD", []string{"2."}},
		{"invalid base64", "2.!!!|!!!|!!!", []string{"base64"}},
		{"short iv", EncString("2." + base64.StdEncoding.EncodeToString(make([]byte, 8)) + "|AAA|AAA"), []string{"iv"}},
		{"ct not block aligned", EncString("2." + base64.StdEncoding.EncodeToString(validIV) + "|" + base64.StdEncoding.EncodeToString(make([]byte, 6)) + "|" + base64.StdEncoding.EncodeToString(validMAC)), []string{"block"}},
		{"empty ct", EncString("2." + base64.StdEncoding.EncodeToString(validIV) + "||" + base64.StdEncoding.EncodeToString(validMAC)), []string{"block"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := decryptEncString(tc.enc, encKey, macKey)
			if err == nil {
				t.Fatalf("decryptEncString(%q) = nil error, want shape error", tc.enc)
			}
			for _, sub := range tc.wantSub {
				if !strings.Contains(err.Error(), sub) {
					t.Errorf("error %q does not contain %q", err, sub)
				}
			}
		})
	}

	t.Run("tampered ct fails HMAC before decrypt", func(t *testing.T) {
		// Build a valid EncString, then flip a ct byte while keeping the
		// original MAC: verify-then-decrypt must reject with the
		// authentication error, never attempt CBC on unauthenticated data.
		enc := buildEncStringRaw(t, encKey, macKey, validIV, make([]byte, 16))
		parts := strings.SplitN(strings.TrimPrefix(string(enc), "2."), "|", 3)
		raw, err := base64.StdEncoding.DecodeString(parts[1])
		if err != nil {
			t.Fatalf("decode ct: %v", err)
		}
		raw[0] ^= 0xFF
		tampered := EncString("2." + parts[0] + "|" + base64.StdEncoding.EncodeToString(raw) + "|" + parts[2])
		_, err = decryptEncString(tampered, encKey, macKey)
		if err == nil {
			t.Fatal("tampered ct accepted")
		}
		if !strings.Contains(err.Error(), "HMAC") {
			t.Errorf("error %q: want the HMAC authentication failure, not a decrypt-side error", err)
		}
		if strings.Contains(err.Error(), "padding") {
			t.Errorf("error %q is a decrypt-side error: CBC ran before the HMAC check", err)
		}
	})

	t.Run("invalid padding rejected", func(t *testing.T) {
		// MAC-consistent ciphertexts whose decrypted padding violates the
		// padLen ∈ [1, blocksize] guard.
		for name, last := range map[string]byte{"zero pad": 0x00, "pad 17": 0x11, "pad 255": 0xFF} {
			pt := make([]byte, 16)
			pt[15] = last
			enc := buildEncStringRaw(t, encKey, macKey, validIV, pt)
			_, err := decryptEncString(enc, encKey, macKey)
			if err == nil {
				t.Errorf("padding %s: nil error, want padding shape error", name)
			} else if !strings.Contains(err.Error(), "padding") {
				t.Errorf("padding %s: error %q does not mention padding", name, err)
			}
		}
	})
}

// TestDecryptEncStringBoundedEcho pins that shape errors never echo a
// server-controlled EncString unbounded: a malformed string of arbitrary
// length surfaces only as a bounded prefix carrying the ...(truncated)
// marker, so the long tail never reaches errors or logs (the siblings'
// discipline — they echo only the observed type prefix).
func TestDecryptEncStringBoundedEcho(t *testing.T) {
	encKey := unhex(t, kaOKHex)[:32]
	macKey := unhex(t, kaOKHex)[32:]
	head := strings.Repeat("A", 500)
	tail := "TAIL-MARKER-NEVER-IN-ERRORS"

	t.Run("no type prefix", func(t *testing.T) {
		_, err := decryptEncString(EncString(head+tail), encKey, macKey)
		if err == nil {
			t.Fatal("long dot-less string: nil error")
		}
		assertBoundedEcho(t, err, tail)
	})

	t.Run("unsupported type prefix", func(t *testing.T) {
		_, err := decryptEncString(EncString(head+"."+tail+"|x|y"), encKey, macKey)
		if err == nil {
			t.Fatal("long wrong-type string: nil error")
		}
		assertBoundedEcho(t, err, tail)
	})
}

// assertBoundedEcho fails the test when err echoes the long tail, lacks the
// truncation marker, or is itself unbounded.
func assertBoundedEcho(t *testing.T, err error, tail string) {
	t.Helper()
	msg := err.Error()
	if strings.Contains(msg, tail) {
		t.Errorf("error echoes the long tail: %d bytes, ends %q", len(msg), msg[min(len(msg), 40):])
	}
	if !strings.Contains(msg, "...(truncated)") {
		t.Errorf("error %q lacks the truncation marker", msg)
	}
	if len(msg) > 512 {
		t.Errorf("error is %d bytes; the echoed string is not bounded", len(msg))
	}
}

func TestNormalizeURL(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want string // empty means want error
	}{
		{"bare https", "https://api.bitwarden.com", "https://api.bitwarden.com"},
		{"trailing slash", "https://api.bitwarden.com/", "https://api.bitwarden.com"},
		{"trailing slashes", "https://api.bitwarden.com///", "https://api.bitwarden.com"},
		{"path prefix kept", "https://vault.bitwarden.eu/api", "https://vault.bitwarden.eu/api"},
		{"path prefix trailing slash", "https://vault.bitwarden.eu/api/", "https://vault.bitwarden.eu/api"},
		{"deep path", "https://vault.bitwarden.eu/api/extra/", "https://vault.bitwarden.eu/api/extra"},
		{"uppercase host and scheme", "HTTPS://API.Example.COM", "https://api.example.com"},
		{"uppercase host with path", "https://Vault.Example.COM/API", "https://vault.example.com/API"},
		{"https default port stripped", "https://api.example.com:443", "https://api.example.com"},
		{"http default port stripped", "http://api.example.com:80", "http://api.example.com"},
		{"non-default https port kept", "https://api.example.com:8443", "https://api.example.com:8443"},
		{"non-default http port kept", "http://api.example.com:8080", "http://api.example.com:8080"},
		{"443 kept on http scheme", "http://api.example.com:443", "http://api.example.com:443"},
		{"80 kept on https scheme", "https://api.example.com:80", "https://api.example.com:80"},
		{"wrong scheme", "ftp://api.example.com", ""},
		{"javascript scheme", "javascript:alert(1)", ""},
		{"bare host", "api.example.com", ""},
		{"protocol-relative", "//api.example.com", ""},
		{"relative", "/api", ""},
		{"empty", "", ""},
		{"empty host", "https://", ""},
		{"empty host with http port", "http://:8080", ""},
		{"empty host with https port", "https://:0", ""},
		{"userinfo", "https://user:secret@api.example.com", ""},
		{"query", "https://api.example.com/path?x=1", ""},
		{"fragment", "https://api.example.com/path#frag", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := NormalizeURL(tc.raw)
			if tc.want == "" {
				if err == nil {
					t.Fatalf("NormalizeURL(%q) = %q, want error", tc.raw, got)
				}
				if !strings.Contains(err.Error(), "https://") {
					t.Errorf("error %q does not name an example form (https://...)", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("NormalizeURL(%q): %v", tc.raw, err)
			}
			if got != tc.want {
				t.Errorf("NormalizeURL(%q) = %q, want %q", tc.raw, got, tc.want)
			}
		})
	}
}

func TestParseAccessToken(t *testing.T) {
	seedB64 := base64.StdEncoding.EncodeToString(unhex(t, kaSeedHex))

	t.Run("valid with padded seed", func(t *testing.T) {
		id, secret, seed, err := ParseAccessToken("0.machine-account-id.machine-account-secret:" + seedB64)
		if err != nil {
			t.Fatalf("ParseAccessToken: %v", err)
		}
		if id != "machine-account-id" || secret != "machine-account-secret" {
			t.Errorf("id/secret = %q/%q, want the embedded pair", id, secret)
		}
		if string(seed) != string(unhex(t, kaSeedHex)) {
			t.Errorf("seed = %x, want %x", seed, kaSeedHex)
		}
	})

	t.Run("valid without padding", func(t *testing.T) {
		_, _, seed, err := ParseAccessToken("0.a.b:" + base64.RawStdEncoding.EncodeToString(unhex(t, kaSeedHex)))
		if err != nil {
			t.Fatalf("ParseAccessToken: %v", err)
		}
		if len(seed) != 16 {
			t.Errorf("seed length = %d, want 16", len(seed))
		}
	})

	t.Run("empty token names both inputs", func(t *testing.T) {
		_, _, _, err := ParseAccessToken("")
		if err == nil {
			t.Fatal("empty token: nil error")
		}
		for _, sub := range []string{"--access-token", "MCPCLI_BWS_ACCESS_TOKEN"} {
			if !strings.Contains(err.Error(), sub) {
				t.Errorf("error %q does not name input %q", err, sub)
			}
		}
	})

	t.Run("wrong version carries observed version", func(t *testing.T) {
		_, _, _, err := ParseAccessToken("1.a.b:" + seedB64)
		if err == nil {
			t.Fatal("version 1 token: nil error")
		}
		if !strings.Contains(err.Error(), "1") || !strings.Contains(err.Error(), "0") {
			t.Errorf("error %q does not carry observed version 1 and wanted 0", err)
		}
	})

	t.Run("short seed carries observed length", func(t *testing.T) {
		short := base64.StdEncoding.EncodeToString(make([]byte, 15))
		_, _, _, err := ParseAccessToken("0.a.b:" + short)
		if err == nil {
			t.Fatal("15-byte seed: nil error")
		}
		if !strings.Contains(err.Error(), "15") {
			t.Errorf("error %q does not carry the observed length 15", err)
		}
	})

	t.Run("long seed carries observed length", func(t *testing.T) {
		long := base64.StdEncoding.EncodeToString(make([]byte, 17))
		_, _, _, err := ParseAccessToken("0.a.b:" + long)
		if err == nil {
			t.Fatal("17-byte seed: nil error")
		}
		if !strings.Contains(err.Error(), "17") {
			t.Errorf("error %q does not carry the observed length 17", err)
		}
	})

	t.Run("malformed forms fail", func(t *testing.T) {
		for _, token := range []string{"no-separator", "0.nodots:" + seedB64, "nodot:" + seedB64, "0.:" + seedB64} {
			if _, _, _, err := ParseAccessToken(token); err == nil {
				t.Errorf("ParseAccessToken(%q) = nil error, want malformed-token error", token)
			}
		}
	})

	t.Run("seed not base64", func(t *testing.T) {
		if _, _, _, err := ParseAccessToken("0.a.b:not-base64!!"); err == nil {
			t.Fatal("non-base64 seed: nil error")
		}
	})
}

func TestExchangeFormAndEnvelope(t *testing.T) {
	for _, form := range baseForms {
		t.Run(form.name, func(t *testing.T) {
			tok, clientID, clientSecret, _ := dummyAccessToken(t)
			orgKey := testOrgKey(t)
			fi := newFakeIdentity(t, form.identMount, tok, orgKey)
			c := NewClient("https://api.invalid"+form.apiMount, fi.url, tok, "")

			tr, err := c.Exchange(t.Context())
			if err != nil {
				t.Fatalf("Exchange: %v", err)
			}
			if tr.AccessToken == "" {
				t.Error("exchange response has an empty access_token")
			}
			if tr.EncryptedPayload == "" {
				t.Error("exchange response has an empty encrypted_payload")
			}

			path, formVals, contentType := fi.observed()
			if path != form.identMount+"/connect/token" {
				t.Errorf("exchange path = %q, want %q", path, form.identMount+"/connect/token")
			}
			if contentType != "application/x-www-form-urlencoded" {
				t.Errorf("Content-Type = %q, want application/x-www-form-urlencoded", contentType)
			}
			want := map[string]string{
				"grant_type":    "client_credentials",
				"client_id":     clientID,
				"client_secret": clientSecret,
				"scope":         "api.secrets",
			}
			for k, v := range want {
				if got := formVals[k]; len(got) != 1 || got[0] != v {
					t.Errorf("form[%q] = %v, want [%q]", k, got, v)
				}
			}
		})
	}
}

func TestOrgKeyDecryptsPayload(t *testing.T) {
	tok, _, _, _ := dummyAccessToken(t)
	orgKey := testOrgKey(t)
	fi := newFakeIdentity(t, "", tok, orgKey)
	c := NewClient("https://api.invalid", fi.url, tok, "")

	got, err := c.orgKey(t.Context())
	if err != nil {
		t.Fatalf("orgKey: %v", err)
	}
	if string(got) != string(orgKey) {
		t.Error("orgKey does not round-trip the encrypted payload's key")
	}
	// Memoized: a second call must not re-exchange.
	if _, err := c.orgKey(t.Context()); err != nil {
		t.Fatalf("second orgKey: %v", err)
	}
	if n := fi.exchangeCount(); n != 1 {
		t.Errorf("exchange count after two orgKey calls = %d, want 1", n)
	}
}

func TestTruncatedOrgKeyFailsClosed(t *testing.T) {
	tok, _, _, _ := dummyAccessToken(t)
	// The payload decrypts fine but carries only 32 bytes of key material:
	// the length check must fire before any [:32]/[32:] slicing.
	fi := newFakeIdentity(t, "", tok, make([]byte, 32))
	c := NewClient("https://api.invalid", fi.url, tok, "")
	_, err := c.orgKey(t.Context())
	if err == nil {
		t.Fatal("32-byte org key accepted")
	}
	if !strings.Contains(err.Error(), "64") {
		t.Errorf("error %q does not name the required 64-byte length", err)
	}
}

func TestExchange401UnionedMessage(t *testing.T) {
	tok, _, _, _ := dummyAccessToken(t)
	orgKey := testOrgKey(t)
	fi := newFakeIdentity(t, "", tok, orgKey)
	fi.forceResponse(http.StatusUnauthorized, `{"error":"invalid_grant"}`)
	c := NewClient("https://api.invalid", fi.url, tok, "")

	_, err := c.Exchange(t.Context())
	if err == nil {
		t.Fatal("401 exchange: nil error")
	}
	for _, sub := range []string{"401", "stale or rotated", "identity URL", "mcp bws login"} {
		if !strings.Contains(err.Error(), sub) {
			t.Errorf("401 error %q does not contain %q", err, sub)
		}
	}
	if strings.Contains(err.Error(), tok) {
		t.Error("401 error contains the access token")
	}
	// The sentinel: callers match 401s with errors.Is, never by string.
	if !errors.Is(err, ErrUnauthorized) {
		t.Error("401 error does not wrap ErrUnauthorized")
	}
	// The unioned message stays byte-identical (goldens and docs quote it).
	want := fmt.Sprintf(`bws: the access token was rejected with 401 by the identity server %s: the token may be stale or rotated, or the identity URL may point at a different instance; re-run "mcp bws login"`, fi.url)
	if err.Error() != want {
		t.Errorf("401 message =\n%q\nwant\n%q", err.Error(), want)
	}
}

func TestExchangeNon2xxBoundedBody(t *testing.T) {
	tok, _, _, _ := dummyAccessToken(t)
	orgKey := testOrgKey(t)
	fi := newFakeIdentity(t, "", tok, orgKey)
	huge := strings.Repeat("x", 10_000)
	fi.forceResponse(http.StatusInternalServerError, huge)
	c := NewClient("https://api.invalid", fi.url, tok, "")

	_, err := c.Exchange(t.Context())
	if err == nil {
		t.Fatal("500 exchange: nil error")
	}
	if !strings.Contains(err.Error(), "500") {
		t.Errorf("error %q does not name the status", err)
	}
	if len(err.Error()) > 4200 { // status text + up to 4096 body bytes + wrapper
		t.Errorf("error is %d bytes; the body is not bounded at 4 KiB", len(err.Error()))
	}
	// The sentinel is 401-only: a 500 must not match it.
	if errors.Is(err, ErrUnauthorized) {
		t.Error("500 error wraps ErrUnauthorized; the sentinel is 401-only")
	}
}

func TestExchange2xxShapeErrorWithoutBody(t *testing.T) {
	tok, _, _, _ := dummyAccessToken(t)
	orgKey := testOrgKey(t)
	fi := newFakeIdentity(t, "", tok, orgKey)
	fi.forceResponse(http.StatusOK, `oops SECRET-BODY-MARKER not json`)
	c := NewClient("https://api.invalid", fi.url, tok, "")

	_, err := c.Exchange(t.Context())
	if err == nil {
		t.Fatal("malformed 2xx body: nil error")
	}
	if strings.Contains(err.Error(), "SECRET-BODY-MARKER") {
		t.Errorf("2xx parse-failure error leaks the body: %q", err)
	}
}

func TestExchangeMissingEncryptedPayload(t *testing.T) {
	tok, _, _, _ := dummyAccessToken(t)
	orgKey := testOrgKey(t)
	fi := newFakeIdentity(t, "", tok, orgKey)
	// A 2xx envelope that decodes but carries only the access_token leg: an
	// absent or renamed encrypted_payload must be named as the missing field.
	fi.forceResponse(http.StatusOK, `{"access_token":"x"}`)

	t.Run("exchange names the missing field", func(t *testing.T) {
		c := NewClient("https://api.invalid", fi.url, tok, "")
		_, err := c.Exchange(t.Context())
		if err == nil {
			t.Fatal("2xx envelope without encrypted_payload: nil error")
		}
		if !strings.Contains(err.Error(), "carries no encrypted_payload") {
			t.Errorf("error %q does not name the missing encrypted_payload", err)
		}
	})

	t.Run("orgKey does not blame the ciphertext", func(t *testing.T) {
		// The zero EncString must never reach decryptEncString, whose "has
		// no type prefix" shape error misattributes a missing field to
		// corruption.
		c := NewClient("https://api.invalid", fi.url, tok, "")
		_, err := c.orgKey(t.Context())
		if err == nil {
			t.Fatal("orgKey over an envelope without encrypted_payload: nil error")
		}
		if !strings.Contains(err.Error(), "carries no encrypted_payload") {
			t.Errorf("error %q does not name the missing encrypted_payload", err)
		}
		if strings.Contains(err.Error(), "type prefix") {
			t.Errorf("error %q misattributes the missing field to ciphertext corruption", err)
		}
	})
}

func TestGetSecretByRef(t *testing.T) {
	for _, form := range baseForms {
		t.Run(form.name, func(t *testing.T) {
			tok, _, _, _ := dummyAccessToken(t)
			orgKey := testOrgKey(t)
			orgID := "01234567-89ab-cdef-0123-456789abcdef"
			uuidRef := "11111111-2222-3333-4444-555555555555"
			secrets := map[string]Secret{
				uuidRef: wireSecret(t, orgKey, Secret{
					ID: uuidRef, OrganizationID: orgID,
					Key: "jev", Value: "the-api-key-value", Note: "a note",
					CanRead: true, CanWrite: false,
					RevisionDate: time.Date(2026, 11, 8, 10, 0, 0, 0, time.UTC),
					Projects:     []Project{{ID: "aaaaaaaa-0000-0000-0000-000000000000", Name: "proj-a"}},
				}),
			}
			fi := newFakeIdentity(t, form.identMount, tok, orgKey)
			fa := newFakeAPI(t, form.apiMount, orgKey, secrets)
			c := NewClient(fa.url, fi.url, tok, orgID)

			s, err := c.GetSecretByRef(t.Context(), uuidRef)
			if err != nil {
				t.Fatalf("GetSecretByRef(uuid): %v", err)
			}
			if s.Key != "jev" || s.Value != "the-api-key-value" || s.Note != "a note" {
				t.Errorf("decrypted secret = %+v", s)
			}
			if len(s.Projects) != 1 || s.Projects[0].ID != "aaaaaaaa-0000-0000-0000-000000000000" || s.Projects[0].Name != "proj-a" {
				t.Errorf("decrypted projects = %+v, want the one project with its name decrypted", s.Projects)
			}
			if !s.CanRead || s.CanWrite {
				t.Errorf("read/write flags = %v/%v, want true/false", s.CanRead, s.CanWrite)
			}

			// The org key is memoized: a second ref costs no new exchange.
			if _, err := c.GetSecretByRef(t.Context(), uuidRef); err != nil {
				t.Fatalf("second GetSecretByRef: %v", err)
			}
			if n := fi.exchangeCount(); n != 1 {
				t.Errorf("exchange count after two gets = %d, want 1", n)
			}

			t.Run("CanRead false names the grant cause", func(t *testing.T) {
				denied := "22222222-2222-3333-4444-555555555555"
				fa2 := newFakeAPI(t, form.apiMount, orgKey, map[string]Secret{
					denied: wireSecret(t, orgKey, Secret{ID: denied, Key: "locked", Value: "x", CanRead: false}),
				})
				c2 := NewClient(fa2.url, fi.url, tok, orgID)
				_, err := c2.GetSecretByRef(t.Context(), denied)
				if err == nil {
					t.Fatal("CanRead=false accepted")
				}
				if !strings.Contains(err.Error(), denied) || !strings.Contains(err.Error(), "read") {
					t.Errorf("error %q does not name the ref and the grant-read cause", err)
				}
			})

			t.Run("empty value names the content cause", func(t *testing.T) {
				empty := "33333333-2222-3333-4444-555555555555"
				fa2 := newFakeAPI(t, form.apiMount, orgKey, map[string]Secret{
					empty: wireSecret(t, orgKey, Secret{ID: empty, Key: "hollow", Value: "", CanRead: true}),
				})
				c2 := NewClient(fa2.url, fi.url, tok, orgID)
				_, err := c2.GetSecretByRef(t.Context(), empty)
				if err == nil {
					t.Fatal("empty value accepted")
				}
				if !strings.Contains(err.Error(), empty) || !strings.Contains(err.Error(), "empty") {
					t.Errorf("error %q does not name the ref and the empty-content cause", err)
				}
			})

			t.Run("NUL-bearing value rejected naming the key", func(t *testing.T) {
				nulID := "44444444-2222-3333-4444-555555555555"
				fa2 := newFakeAPI(t, form.apiMount, orgKey, map[string]Secret{
					nulID: wireSecret(t, orgKey, Secret{ID: nulID, Key: "nul-key", Value: "before\x00after", CanRead: true}),
				})
				c2 := NewClient(fa2.url, fi.url, tok, orgID)
				_, err := c2.GetSecretByRef(t.Context(), nulID)
				if err == nil {
					t.Fatal("NUL-bearing value accepted")
				}
				if !strings.Contains(err.Error(), "nul-key") {
					t.Errorf("error %q does not name the decrypted KEY", err)
				}
				if strings.Contains(err.Error(), "before\x00after") {
					t.Errorf("error %q echoes the NUL-bearing value", err)
				}
			})
		})
	}
}

func TestGetSecretByName(t *testing.T) {
	tok, _, _, _ := dummyAccessToken(t)
	orgKey := testOrgKey(t)
	orgID := "01234567-89ab-cdef-0123-456789abcdef"
	idA := "11111111-2222-3333-4444-555555555555"
	idB := "22222222-2222-3333-4444-555555555555"
	fa := newFakeAPI(t, "", orgKey, map[string]Secret{
		idA: wireSecret(t, orgKey, Secret{ID: idA, Key: "jev", Value: "value-a", CanRead: true}),
		idB: wireSecret(t, orgKey, Secret{ID: idB, Key: "other", Value: "value-b", CanRead: true}),
	})
	fi := newFakeIdentity(t, "", tok, orgKey)
	c := NewClient(fa.url, fi.url, tok, orgID)

	t.Run("single match", func(t *testing.T) {
		s, err := c.GetSecretByRef(t.Context(), "jev")
		if err != nil {
			t.Fatalf("GetSecretByRef(name): %v", err)
		}
		if s.ID != idA || s.Value != "value-a" {
			t.Errorf("resolved = %+v, want idA/value-a", s)
		}
	})

	t.Run("zero matches", func(t *testing.T) {
		_, err := c.GetSecretByRef(t.Context(), "missing")
		if err == nil {
			t.Fatal("zero matches: nil error")
		}
		if !strings.Contains(err.Error(), "missing") {
			t.Errorf("error %q does not name the ref", err)
		}
	})

	t.Run("multiple matches", func(t *testing.T) {
		fa2 := newFakeAPI(t, "", orgKey, map[string]Secret{
			idA: wireSecret(t, orgKey, Secret{ID: idA, Key: "dupe", Value: "value-a", CanRead: true}),
			idB: wireSecret(t, orgKey, Secret{ID: idB, Key: "dupe", Value: "value-b", CanRead: true}),
		})
		c2 := NewClient(fa2.url, fi.url, tok, orgID)
		_, err := c2.GetSecretByRef(t.Context(), "dupe")
		if err == nil {
			t.Fatal("multiple matches: nil error")
		}
		if !strings.Contains(err.Error(), "dupe") || !strings.Contains(err.Error(), idA) || !strings.Contains(err.Error(), idB) {
			t.Errorf("error %q does not name the ref and the competing ids", err)
		}
	})

	t.Run("missing org id fails closed pre-URL", func(t *testing.T) {
		c2 := NewClient(fa.url, fi.url, tok, "")
		_, err := c2.GetSecretByRef(t.Context(), "jev")
		if err == nil {
			t.Fatal("name ref without org id: nil error")
		}
		if !strings.Contains(err.Error(), "--org") {
			t.Errorf("error %q does not carry the re-login-with---org remediation", err)
		}
	})
}

func TestListSecrets(t *testing.T) {
	tok, _, _, _ := dummyAccessToken(t)
	orgKey := testOrgKey(t)
	orgID := "01234567-89ab-cdef-0123-456789abcdef"
	idA := "11111111-2222-3333-4444-555555555555"
	idB := "22222222-2222-3333-4444-555555555555"
	fa := newFakeAPI(t, "", orgKey, map[string]Secret{
		idA: wireSecret(t, orgKey, Secret{ID: idA, OrganizationID: orgID, Key: "jev", Value: "v", CanRead: true, CanWrite: true}),
		idB: wireSecret(t, orgKey, Secret{ID: idB, OrganizationID: orgID, Key: "testsecret", Value: "v", CanRead: true, CanWrite: true}),
	})
	fi := newFakeIdentity(t, "", tok, orgKey)
	c := NewClient(fa.url, fi.url, tok, orgID)

	items, err := c.ListSecrets(t.Context())
	if err != nil {
		t.Fatalf("ListSecrets: %v", err)
	}
	if len(items) != 2 {
		t.Fatalf("listed %d secrets, want 2", len(items))
	}
	byID := map[string]Secret{items[0].ID: items[0], items[1].ID: items[1]}
	if byID[idA].Key != "jev" || byID[idB].Key != "testsecret" {
		t.Errorf("listed keys not decrypted: %+v", items)
	}

	t.Run("continuation token fails loud naming it", func(t *testing.T) {
		fa.setContinuation("page-2-marker")
		defer fa.setContinuation("")
		_, err := c.ListSecrets(t.Context())
		if err == nil {
			t.Fatal("continuationToken ignored")
		}
		if !strings.Contains(err.Error(), "page-2-marker") {
			t.Errorf("error %q does not name the continuation token", err)
		}
	})

	t.Run("403 carries remediation", func(t *testing.T) {
		fa.forceList(http.StatusForbidden)
		defer fa.forceList(0)
		_, err := c.ListSecrets(t.Context())
		if err == nil {
			t.Fatal("403 list: nil error")
		}
		if !strings.Contains(err.Error(), "machine account") || !strings.Contains(err.Error(), "Bitwarden") {
			t.Errorf("403 error %q lacks the remediation pointer", err)
		}
	})
}

// TestSecretProjectsWireShape pins the LIVE per-secret projects wire shape
// (re-proved against the real EU instance, 2026-11-08): `projects` is an
// array of OBJECTS `{"id": ..., "name": <EncString>}` — the live list run
// failed with "json: cannot unmarshal object into .secrets.0.projects.0 of
// type string" against the old []string decode, and the live GET probe
// showed "projects": [{"id": ..., "name": "2.AyFL…"}]. The bodies below are
// the live shapes verbatim (synthetic uuids; names encrypted with the fake
// org key) and are served RAW — through no struct-based fake — so the test
// stays independent of the decode under change.
func TestSecretProjectsWireShape(t *testing.T) {
	tok, _, _, _ := dummyAccessToken(t)
	orgKey := testOrgKey(t)
	orgID := "01234567-89ab-cdef-0123-456789abcdef"
	id := "11111111-2222-3333-4444-555555555555"
	projID := "1077cafe-0000-4000-8000-000000000000"
	enc, mac := orgKey[:32], orgKey[32:64]
	encKeyField, err := encField("live-shape-key", enc, mac)
	if err != nil {
		t.Fatalf("encrypt key field: %v", err)
	}
	encValueField, err := encField("live-shape-value", enc, mac)
	if err != nil {
		t.Fatalf("encrypt value field: %v", err)
	}
	encNote, err := encField("", enc, mac)
	if err != nil {
		t.Fatalf("encrypt note field: %v", err)
	}
	encName, err := encField("Projekt Eins", enc, mac)
	if err != nil {
		t.Fatalf("encrypt project name: %v", err)
	}

	secret := fmt.Sprintf(`{"id":%q,"organizationId":%q,"key":%q,"value":%q,"note":%q,"read":true,"write":false,"revisionDate":"2026-11-08T10:00:00.000000Z","projects":[{"id":%q,"name":%q}]}`,
		id, orgID, encKeyField, encValueField, encNote, projID, encName)
	listBody := `{"object":"secretListView","projects":[],"secrets":[` + secret + `],"continuationToken":""}`

	newRawAPI := func() *Client {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			switch {
			case r.URL.Path == "/organizations/"+orgID+"/secrets":
				_, _ = w.Write([]byte(listBody))
			case strings.HasPrefix(r.URL.Path, "/secrets/"):
				_, _ = w.Write([]byte(secret))
			default:
				http.NotFound(w, r)
			}
		}))
		t.Cleanup(srv.Close)
		fi := newFakeIdentity(t, "", tok, orgKey)
		return NewClient(srv.URL, fi.url, tok, orgID)
	}

	t.Run("list decodes object projects", func(t *testing.T) {
		items, err := newRawAPI().ListSecrets(t.Context())
		if err != nil {
			t.Fatalf("ListSecrets against the live object-shape wire: %v", err)
		}
		if len(items) != 1 || len(items[0].Projects) != 1 {
			t.Fatalf("listed projects = %+v, want the one object project", items)
		}
		if got := items[0].Projects[0]; got.ID != projID || got.Name != "Projekt Eins" {
			t.Errorf("listed project = {%s %q}, want {%s %q} (id carried, name decrypted)", got.ID, got.Name, projID, "Projekt Eins")
		}
	})

	t.Run("get decodes object projects", func(t *testing.T) {
		s, err := newRawAPI().GetSecretByRef(t.Context(), id)
		if err != nil {
			t.Fatalf("GetSecretByRef against the live object-shape wire: %v", err)
		}
		if len(s.Projects) != 1 {
			t.Fatalf("got projects = %+v, want the one object project", s.Projects)
		}
		if got := s.Projects[0]; got.ID != projID || got.Name != "Projekt Eins" {
			t.Errorf("got project = {%s %q}, want {%s %q} (id carried, name decrypted)", got.ID, got.Name, projID, "Projekt Eins")
		}
	})

	t.Run("absent projects decode fine", func(t *testing.T) {
		secretNoProjects := fmt.Sprintf(`{"id":%q,"organizationId":%q,"key":%q,"value":%q,"note":%q,"read":true,"write":false,"revisionDate":"2026-11-08T10:00:00.000000Z"}`,
			id, orgID, encKeyField, encValueField, encNote)
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			switch {
			case r.URL.Path == "/organizations/"+orgID+"/secrets":
				_, _ = w.Write([]byte(`{"object":"secretListView","projects":[],"secrets":[` + secretNoProjects + `],"continuationToken":""}`))
			case strings.HasPrefix(r.URL.Path, "/secrets/"):
				_, _ = w.Write([]byte(secretNoProjects))
			default:
				http.NotFound(w, r)
			}
		}))
		t.Cleanup(srv.Close)
		fi := newFakeIdentity(t, "", tok, orgKey)
		c := NewClient(srv.URL, fi.url, tok, orgID)
		if _, err := c.ListSecrets(t.Context()); err != nil {
			t.Fatalf("ListSecrets without a projects key: %v", err)
		}
		if _, err := c.GetSecretByRef(t.Context(), id); err != nil {
			t.Fatalf("GetSecretByRef without a projects key: %v", err)
		}
	})

	t.Run("empty projects decode fine", func(t *testing.T) {
		secretEmptyProjects := fmt.Sprintf(`{"id":%q,"organizationId":%q,"key":%q,"value":%q,"note":%q,"read":true,"write":false,"revisionDate":"2026-11-08T10:00:00.000000Z","projects":[]}`,
			id, orgID, encKeyField, encValueField, encNote)
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			switch {
			case r.URL.Path == "/organizations/"+orgID+"/secrets":
				_, _ = w.Write([]byte(`{"object":"secretListView","projects":[],"secrets":[` + secretEmptyProjects + `],"continuationToken":""}`))
			case strings.HasPrefix(r.URL.Path, "/secrets/"):
				_, _ = w.Write([]byte(secretEmptyProjects))
			default:
				http.NotFound(w, r)
			}
		}))
		t.Cleanup(srv.Close)
		fi := newFakeIdentity(t, "", tok, orgKey)
		c := NewClient(srv.URL, fi.url, tok, orgID)
		if _, err := c.ListSecrets(t.Context()); err != nil {
			t.Fatalf("ListSecrets with empty projects: %v", err)
		}
		if _, err := c.GetSecretByRef(t.Context(), id); err != nil {
			t.Fatalf("GetSecretByRef with empty projects: %v", err)
		}
	})
}

func TestUpdateSecret(t *testing.T) {
	tok, _, _, _ := dummyAccessToken(t)
	orgKey := testOrgKey(t)
	orgID := "01234567-89ab-cdef-0123-456789abcdef"
	id := "11111111-2222-3333-4444-555555555555"
	fa := newFakeAPI(t, "", orgKey, map[string]Secret{
		id: wireSecret(t, orgKey, Secret{ID: id, OrganizationID: orgID, Key: "testsecret", Value: "old", CanRead: true, CanWrite: true}),
	})
	fi := newFakeIdentity(t, "", tok, orgKey)
	c := NewClient(fa.url, fi.url, tok, orgID)

	err := c.UpdateSecret(t.Context(), SecretUpdateRequest{
		ID:             id,
		OrganizationID: orgID,
		Key:            "testsecret",
		Value:          "new-plain-value",
		Note:           "updated note",
		ProjectIDs:     []string{"aaaaaaaa-0000-0000-0000-000000000000"},
		ValueChanged:   true,
	})
	if err != nil {
		t.Fatalf("UpdateSecret: %v", err)
	}
	puts := fa.recordedPuts()
	if len(puts) != 1 {
		t.Fatalf("recorded %d PUTs, want 1", len(puts))
	}
	var body struct {
		ID             string   `json:"id"`
		OrganizationID string   `json:"organizationId"`
		Key            string   `json:"key"`
		Value          string   `json:"value"`
		Note           string   `json:"note"`
		ProjectIDs     []string `json:"projectIds"`
		ValueChanged   bool     `json:"valueChanged"`
	}
	if err := json.Unmarshal([]byte(puts[0]), &body); err != nil {
		t.Fatalf("PUT body is not JSON: %v\nbody: %s", err, puts[0])
	}
	if body.ID != id || body.OrganizationID != orgID || !body.ValueChanged {
		t.Errorf("PUT body fields = %+v", body)
	}
	if len(body.ProjectIDs) != 1 {
		t.Errorf("projectIds = %v, want the one project", body.ProjectIDs)
	}
	// The wire key/value/note must arrive as EncStrings under the org key.
	dec, err := decryptEncString(EncString(body.Value), orgKey[:32], orgKey[32:64])
	if err != nil {
		t.Fatalf("PUT value is not a decryptable EncString: %v", err)
	}
	if dec != "new-plain-value" {
		t.Errorf("decrypted PUT value = %q, want the plaintext", dec)
	}
	if decKey, err := decryptEncString(EncString(body.Key), orgKey[:32], orgKey[32:64]); err != nil || decKey != "testsecret" {
		t.Errorf("decrypted PUT key = %q, %v", decKey, err)
	}

	t.Run("403 carries remediation", func(t *testing.T) {
		fa.forcePut(http.StatusForbidden)
		defer fa.forcePut(0)
		err := c.UpdateSecret(t.Context(), SecretUpdateRequest{ID: id, OrganizationID: orgID, Key: "k", Value: "v", Note: "n"})
		if err == nil {
			t.Fatal("403 put: nil error")
		}
		if !strings.Contains(err.Error(), "machine account") || !strings.Contains(err.Error(), "Bitwarden") {
			t.Errorf("403 error %q lacks the remediation pointer", err)
		}
	})
}

func TestClientErrorsNeverLeakSecrets(t *testing.T) {
	tok, _, _, seed := dummyAccessToken(t)
	orgKey := testOrgKey(t)
	fi := newFakeIdentity(t, "", tok, orgKey)
	fi.forceResponse(http.StatusInternalServerError, "boom")
	c := NewClient("https://api.invalid", fi.url, tok, "01234567-89ab-cdef-0123-456789abcdef")

	_, exchangeErr := c.Exchange(t.Context())
	c2 := NewClient("https://api.invalid", fi.url, tok, "01234567-89ab-cdef-0123-456789abcdef")
	_, keyErr := c2.orgKey(t.Context())

	for _, err := range []error{exchangeErr, keyErr} {
		if err == nil {
			t.Fatal("expected an error")
		}
		msg := err.Error()
		for _, forbidden := range []string{tok, hex.EncodeToString(seed), base64.StdEncoding.EncodeToString(seed), hex.EncodeToString(orgKey)} {
			if forbidden != "" && strings.Contains(msg, forbidden) {
				t.Errorf("error %q contains secret material", msg)
			}
		}
	}
}
