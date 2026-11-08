// Package bws is a thin pure-Go REST client for Bitwarden Secrets Manager
// (BWS) machine accounts — the first secret.Source implementation. It
// speaks the live wire contract directly (no SDK: the official Go SDK is
// CGO and would break the CGO_ENABLED=0 cross-compile), with arbitrary
// instance bases (dedicated api.<host> and path-prefixed vault.<host>/api)
// and stdlib-only crypto.
package bws

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/hkdf"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// NormalizeURL canonicalizes an instance base URL for both the API and the
// identity endpoints. An absolute URL with an http/https scheme is
// required; trailing slashes are trimmed, the host is lowercased and the
// scheme's own default port is stripped (443 for https, 80 for http —
// scheme-matched, so :443 on http survives); a path prefix is kept. URLs
// carrying userinfo or a query/fragment are rejected (fail-closed-minimal).
// There is no scheme defaulting and no hostname rewriting.
func NormalizeURL(raw string) (string, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return "", fmt.Errorf("invalid URL %q: %w", raw, err)
	}
	if u.Scheme == "" || u.Host == "" {
		return "", fmt.Errorf("invalid URL %q: an absolute URL is required, e.g. https://api.bitwarden.com or https://vault.bitwarden.eu/api", raw)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return "", fmt.Errorf("invalid URL %q: the scheme must be http or https (e.g. https://api.bitwarden.com), got %q", raw, u.Scheme)
	}
	if u.User != nil {
		return "", fmt.Errorf("invalid URL %q: userinfo is not allowed; use a plain https://host/path URL", raw)
	}
	if u.RawQuery != "" || u.ForceQuery || u.Fragment != "" {
		return "", fmt.Errorf("invalid URL %q: query strings and fragments are not allowed; use a plain https://host/path URL", raw)
	}
	host := strings.ToLower(u.Host)
	switch u.Scheme {
	case "https":
		host = strings.TrimSuffix(host, ":443")
	case "http":
		host = strings.TrimSuffix(host, ":80")
	}
	path := strings.TrimRight(u.EscapedPath(), "/")
	return u.Scheme + "://" + host + path, nil
}

// ParseAccessToken splits a machine-account access token
// "0.<api-key-id>.<client-secret>:<b64 16-byte seed>" into its parts. The
// seed never leaves the process (it derives the key that decrypts the
// exchange response's encrypted_payload). Version and seed errors carry the
// OBSERVED version/length: Bitwarden has demonstrably rotated wire formats.
func ParseAccessToken(token string) (clientID, clientSecret string, seed []byte, err error) {
	if token == "" {
		return "", "", nil, errors.New("invalid access token: it is empty; pass --access-token or set MCPCLI_BWS_ACCESS_TOKEN")
	}
	head, seedB64, ok := strings.CutLast(token, ":")
	if !ok {
		return "", "", nil, errors.New("invalid access token: missing the \":<seed>\" part; the format is 0.<id>.<secret>:<seed>")
	}
	version, rest, ok := strings.Cut(head, ".")
	if !ok {
		return "", "", nil, fmt.Errorf("invalid access token: no client id after the version %q; the format is 0.<id>.<secret>:<seed>", version)
	}
	if version != "0" {
		return "", "", nil, fmt.Errorf("unsupported access token version %q (want 0); the token format may have rotated", version)
	}
	clientID, clientSecret, ok = strings.Cut(rest, ".")
	if !ok || clientID == "" || clientSecret == "" {
		return "", "", nil, errors.New("invalid access token: the \"0.<id>.<secret>\" head is incomplete; the format is 0.<id>.<secret>:<seed>")
	}
	// The live seeds are standard base64; the unpadded form is tolerated
	// because a 16-byte value's padding is exactly "==" and stripping it is
	// unambiguous.
	seed, err = base64.StdEncoding.DecodeString(seedB64)
	if err != nil {
		seed, err = base64.RawStdEncoding.DecodeString(seedB64)
		if err != nil {
			return "", "", nil, fmt.Errorf("invalid access token: the seed is not valid base64: %w", err)
		}
	}
	if len(seed) != 16 {
		return "", "", nil, fmt.Errorf("invalid access token: the seed decodes to %d bytes, want 16", len(seed))
	}
	return clientID, clientSecret, seed, nil
}

// EncString is a Bitwarden encrypted string: "2.<iv b64>|<ct b64>|<mac b64>"
// — AES-256-CBC with HMAC-SHA256 over iv‖ct.
type EncString string

// TokenResponse is the decoded POST {identity}/connect/token envelope. The
// explicit json tags are load-bearing: underscores break
// encoding/json's case-insensitive fallback and untagged fields would
// silently decode empty.
type TokenResponse struct {
	AccessToken      string    `json:"access_token"`
	EncryptedPayload EncString `json:"encrypted_payload"`
}

// Secret is one BWS secret. It doubles as the wire-decode shape (json tags
// keep the wire's lowercase read/write; there is deliberately no
// CreationDate field — nothing pins it) and the decrypted result: Key,
// Value and Note hold EncStrings after decode and are decrypted in place.
type Secret struct {
	ID             string    `json:"id"`
	OrganizationID string    `json:"organizationId"`
	Key            string    `json:"key"`
	Value          string    `json:"value"`
	Note           string    `json:"note"`
	CanRead        bool      `json:"read"`
	CanWrite       bool      `json:"write"`
	RevisionDate   time.Time `json:"revisionDate"`
	Projects       []string  `json:"projects"`
}

// SecretUpdateRequest is the body for PUT /secrets/{id}. ID, OrganizationID,
// Key, Value and Note carry plaintext; UpdateSecret encrypts key/value/note
// with the org key so the wire form is EncStrings (the org key is
// unexported, so no caller could pre-encrypt). The explicit json tags on
// every field are load-bearing: the server rejects unknown fields and
// untagged fields would silently drop out of the request.
type SecretUpdateRequest struct {
	ID             string   `json:"id"`
	OrganizationID string   `json:"organizationId"`
	Key            string   `json:"key"`
	Value          string   `json:"value"`
	Note           string   `json:"note"`
	ProjectIDs     []string `json:"projectIds"`
	ValueChanged   bool     `json:"valueChanged"`
}

// maxDiagBytes bounds response bodies kept for error diagnostics — the
// connection package's maxStderrBytes pattern.
const maxDiagBytes = 4096

// maxEchoChars bounds how much of a server-controlled string an error
// echoes: enough prefix to identify the shape failure, never the whole
// value (the same bounded-diagnostics discipline at error scale).
const maxEchoChars = 64

// boundedEcho returns s's first maxEchoChars bytes, marked "...(truncated)"
// past that — the connection package's marker. A server-controlled string
// (an EncString of unknown shape) must never reach errors or logs unbounded.
func boundedEcho(s string) string {
	if len(s) <= maxEchoChars {
		return s
	}
	return s[:maxEchoChars] + "...(truncated)"
}

// Client is a thin BWS REST client for one instance. It is deliberately
// single-goroutine per run: the exchange memo below is a plain field, not a
// sync.Once, and a Client (or its wrapping Source) must not be shared
// across goroutines.
type Client struct {
	apiURL      string
	identityURL string
	accessToken string
	orgID       string
	http        *http.Client

	// Parsed access-token parts; tokenErr surfaces on first use.
	clientID     string
	clientSecret string
	seed         []byte
	tokenErr     error

	// Per-run exchange memo, populated together by orgKey: the JWT bearer
	// for API calls and the org key for field decryption. Never persisted.
	jwt     string
	orgKeyC []byte
}

// NewClient builds a Client for one instance. Both URLs are caller-supplied
// (zero domain knowledge: dedicated and path-prefixed bases are equally
// valid); they are assumed pre-normalized — SaveCredential and the Source
// normalize — but NewClient normalizes defensively so a raw input still
// yields working request URLs. A normalization failure keeps the raw form
// and the request-time URL parse fails loudly. NewClient never fails: a
// malformed access token surfaces on first use.
func NewClient(apiURL, identityURL, accessToken, orgID string) *Client {
	if n, err := NormalizeURL(apiURL); err == nil {
		apiURL = n
	}
	if n, err := NormalizeURL(identityURL); err == nil {
		identityURL = n
	}
	c := &Client{
		apiURL:      apiURL,
		identityURL: identityURL,
		accessToken: accessToken,
		orgID:       orgID,
		http: &http.Client{
			// No credential re-POST to 307/308 targets, no silent
			// https→http downgrade: a redirect surfaces as its 3xx status.
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
	}
	c.clientID, c.clientSecret, c.seed, c.tokenErr = ParseAccessToken(accessToken)
	return c
}

// tokenGate returns the deferred parse error, if any.
func (c *Client) tokenGate() error {
	if c.tokenErr != nil {
		return fmt.Errorf("bws: %w", c.tokenErr)
	}
	return nil
}

// ErrUnauthorized is the sentinel wrapped by every Exchange 401 error: the
// identity server rejected the machine-account token. Callers match it with
// errors.Is — never by string-matching the unioned message, whose text is
// pinned by goldens and docs and must stay verbatim.
var ErrUnauthorized = errors.New("bws: unauthorized")

// unauthorizedError is Exchange's 401: the runtime unioned message (naming
// the identity server and both possible causes) wrapping ErrUnauthorized.
// Error() is byte-identical to the message the sentinel replaced, so goldens
// and docs quoting it stay green.
type unauthorizedError struct{ identityURL string }

func (e *unauthorizedError) Error() string {
	return fmt.Sprintf("bws: the access token was rejected with 401 by the identity server %s: the token may be stale or rotated, or the identity URL may point at a different instance; re-run \"mcp bws login\"", e.identityURL)
}

func (e *unauthorizedError) Unwrap() error { return ErrUnauthorized }

// Exchange performs the client-credentials exchange against the identity
// server: POST {identity}/connect/token with form-encoded
// grant_type/client_id/client_secret/scope. A 401 carries the runtime
// unioned message naming both causes (stale/rotated token or identity-URL
// mismatch); other non-2xx statuses carry the status and a bounded body; a
// 2xx parse failure is a shape error WITHOUT the body.
func (c *Client) Exchange(ctx context.Context) (*TokenResponse, error) {
	if err := c.tokenGate(); err != nil {
		return nil, err
	}
	form := url.Values{
		"grant_type":    {"client_credentials"},
		"client_id":     {c.clientID},
		"client_secret": {c.clientSecret},
		"scope":         {"api.secrets"},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.identityURL+"/connect/token", strings.NewReader(form.Encode()))
	if err != nil {
		return nil, fmt.Errorf("bws: building the exchange request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("bws: exchanging the access token at %s: %w", c.identityURL, err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode == http.StatusUnauthorized {
		return nil, &unauthorizedError{identityURL: c.identityURL}
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		diag, _ := io.ReadAll(io.LimitReader(resp.Body, maxDiagBytes))
		if len(diag) > 0 {
			return nil, fmt.Errorf("bws: the identity server returned %s: %s", resp.Status, diag)
		}
		return nil, fmt.Errorf("bws: the identity server returned %s", resp.Status)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("bws: reading the exchange response: %w", err)
	}
	var tr TokenResponse
	if err := json.Unmarshal(body, &tr); err != nil {
		// Shape error without the body: the body of a 2xx response may hold
		// anything and never belongs in an error.
		return nil, fmt.Errorf("bws: the exchange response has an unexpected shape (status %s): %w", resp.Status, err)
	}
	if tr.AccessToken == "" {
		return nil, fmt.Errorf("bws: the exchange response carries no access_token (status %s)", resp.Status)
	}
	return &tr, nil
}

// deriveSeedKey expands the 16-byte access-token seed into the AES key
// (first 32 bytes) and HMAC key (last 32 bytes) that decrypt the exchange
// response's encrypted_payload: HKDF-SHA256 with extract key
// "bitwarden-accesstoken" and expand info "sm-access-token".
func deriveSeedKey(seed []byte) (encKey, macKey []byte, err error) {
	prk, err := hkdf.Extract(sha256.New, seed, []byte("bitwarden-accesstoken"))
	if err != nil {
		return nil, nil, fmt.Errorf("bws: deriving the seed key: %w", err)
	}
	ok, err := hkdf.Expand(sha256.New, prk, "sm-access-token", 64)
	if err != nil {
		return nil, nil, fmt.Errorf("bws: deriving the seed key: %w", err)
	}
	return ok[:32], ok[32:], nil
}

// decryptEncString decrypts an EncString with the given AES-256 key and
// HMAC-SHA256 key. It is verify-then-decrypt: the HMAC over iv‖ct is
// checked (constant-time) BEFORE any CBC work; iv and ciphertext length
// shape errors fire before the cipher; the PKCS#7 padding strip is guarded
// (padLen within [1, blocksize] and not past the plaintext).
func decryptEncString(enc EncString, encKey, macKey []byte) (string, error) {
	s := string(enc)
	typePrefix, rest, found := strings.Cut(s, ".")
	if !found {
		return "", fmt.Errorf("bws: encrypted string %q has no type prefix", boundedEcho(s))
	}
	if typePrefix != "2" {
		return "", fmt.Errorf("bws: encrypted string has unsupported type prefix %q (want 2)", boundedEcho(typePrefix))
	}
	parts := strings.Split(rest, "|")
	if len(parts) != 3 {
		return "", fmt.Errorf("bws: encrypted string with type prefix %q has %d \"|\"-separated parts, want 3", typePrefix+".", len(parts))
	}
	iv, err := base64.StdEncoding.DecodeString(parts[0])
	if err != nil {
		return "", fmt.Errorf("bws: encrypted string %q has an invalid base64 iv: %w", typePrefix, err)
	}
	if len(iv) != aes.BlockSize {
		return "", fmt.Errorf("bws: encrypted string %q has a %d-byte iv, want %d", typePrefix, len(iv), aes.BlockSize)
	}
	ct, err := base64.StdEncoding.DecodeString(parts[1])
	if err != nil {
		return "", fmt.Errorf("bws: encrypted string %q has an invalid base64 ciphertext: %w", typePrefix, err)
	}
	if len(ct) == 0 || len(ct)%aes.BlockSize != 0 {
		return "", fmt.Errorf("bws: encrypted string %q has a %d-byte ciphertext, want a non-zero multiple of the %d-byte AES block", typePrefix, len(ct), aes.BlockSize)
	}
	mac, err := base64.StdEncoding.DecodeString(parts[2])
	if err != nil {
		return "", fmt.Errorf("bws: encrypted string %q has an invalid base64 mac: %w", typePrefix, err)
	}
	// Verify-then-decrypt: authenticate iv‖ct before touching the cipher.
	sum := hmac.New(sha256.New, macKey)
	sum.Write(iv)
	sum.Write(ct)
	if !hmac.Equal(sum.Sum(nil), mac) {
		return "", errors.New("bws: encrypted string failed HMAC authentication; the ciphertext or mac was altered or the key is wrong")
	}
	block, err := aes.NewCipher(encKey)
	if err != nil {
		return "", fmt.Errorf("bws: encrypted string decryption: %w", err)
	}
	pt := make([]byte, len(ct))
	cipher.NewCBCDecrypter(block, iv).CryptBlocks(pt, ct)
	padLen := int(pt[len(pt)-1])
	if padLen < 1 || padLen > aes.BlockSize || padLen > len(pt) {
		return "", fmt.Errorf("bws: encrypted string has invalid padding (pad byte %d)", padLen)
	}
	return string(pt[:len(pt)-padLen]), nil
}

// encryptEncString encrypts plaintext into an EncString with a fresh
// crypto/rand IV per write.
func encryptEncString(plaintext string, encKey, macKey []byte) (EncString, error) {
	block, err := aes.NewCipher(encKey)
	if err != nil {
		return "", fmt.Errorf("bws: encrypted string encryption: %w", err)
	}
	padLen := aes.BlockSize - len(plaintext)%aes.BlockSize
	padded := make([]byte, 0, len(plaintext)+padLen)
	padded = append(padded, plaintext...)
	for i := 0; i < padLen; i++ {
		padded = append(padded, byte(padLen))
	}
	iv := make([]byte, aes.BlockSize)
	// Checked, never ignored: ignoring a rand failure would encrypt with an
	// all-zero IV (CWE-329).
	if _, err := rand.Read(iv); err != nil {
		return "", fmt.Errorf("bws: generating the encryption IV: %w", err)
	}
	ct := make([]byte, len(padded))
	cipher.NewCBCEncrypter(block, iv).CryptBlocks(ct, padded)
	sum := hmac.New(sha256.New, macKey)
	sum.Write(iv)
	sum.Write(ct)
	b64 := base64.StdEncoding.EncodeToString
	return EncString("2." + b64(iv) + "|" + b64(ct) + "|" + b64(sum.Sum(nil))), nil
}

// orgKey recovers the 64-byte organization key: it exchanges (memoized —
// one identity POST per Client), decrypts the encrypted_payload with the
// seed-derived key and decodes the {"encryptionKey": "<b64>"} payload. The
// decoded key is length-checked ≥ 64 BEFORE any [:32]/[32:] slicing. The
// memo also stashes the exchange JWT for the API calls that follow.
func (c *Client) orgKey(ctx context.Context) ([]byte, error) {
	if c.orgKeyC != nil {
		return c.orgKeyC, nil
	}
	if err := c.tokenGate(); err != nil {
		return nil, err
	}
	tr, err := c.Exchange(ctx)
	if err != nil {
		return nil, err
	}
	encKey, macKey, err := deriveSeedKey(c.seed)
	if err != nil {
		return nil, err
	}
	payload, err := decryptEncString(tr.EncryptedPayload, encKey, macKey)
	if err != nil {
		return nil, fmt.Errorf("bws: decrypting the exchange payload: %w", err)
	}
	var decoded struct {
		EncryptionKey string `json:"encryptionKey"`
	}
	if err := json.Unmarshal([]byte(payload), &decoded); err != nil {
		return nil, fmt.Errorf("bws: the decrypted exchange payload has an unexpected shape: %w", err)
	}
	key, err := base64.StdEncoding.DecodeString(decoded.EncryptionKey)
	if err != nil {
		return nil, fmt.Errorf("bws: the exchange payload's encryptionKey is not valid base64: %w", err)
	}
	if len(key) < 64 {
		return nil, fmt.Errorf("bws: the exchange payload's encryption key is %d bytes, want at least 64", len(key))
	}
	c.jwt = tr.AccessToken
	c.orgKeyC = key
	return key, nil
}

// apiCall performs one authenticated API call (bearer JWT from the memoized
// exchange) and returns the 2xx body. Non-2xx errors carry the status and a
// bounded body; a 403 adds the machine-account remediation pointer.
func (c *Client) apiCall(ctx context.Context, method, target string, body []byte) ([]byte, error) {
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, target, reader)
	if err != nil {
		return nil, fmt.Errorf("bws: building the %s request: %w", method, err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Authorization", "Bearer "+c.jwt)
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("bws: %s %s: %w", method, target, err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		diag, _ := io.ReadAll(io.LimitReader(resp.Body, maxDiagBytes))
		msg := fmt.Sprintf("bws: %s %s returned %s", method, target, resp.Status)
		if len(diag) > 0 {
			msg += ": " + string(diag)
		}
		if resp.StatusCode == http.StatusForbidden {
			msg += "; check the machine account's access in Bitwarden"
		}
		return nil, errors.New(msg)
	}
	return io.ReadAll(resp.Body)
}

// getSecret fetches and decrypts one secret by id.
func (c *Client) getSecret(ctx context.Context, id string) (*Secret, error) {
	key, err := c.orgKey(ctx)
	if err != nil {
		return nil, err
	}
	body, err := c.apiCall(ctx, http.MethodGet, c.apiURL+"/secrets/"+id, nil)
	if err != nil {
		return nil, err
	}
	var s Secret
	if err := json.Unmarshal(body, &s); err != nil {
		return nil, fmt.Errorf("bws: the secret response for %s has an unexpected shape: %w", id, err)
	}
	if err := decryptSecretFields(&s, key); err != nil {
		return nil, fmt.Errorf("bws: secret %s: %w", id, err)
	}
	return &s, nil
}

// decryptSecretFields decrypts key, value and note in place with the org
// key. One failure aborts everything: a half-decoded secret must never
// surface.
func decryptSecretFields(s *Secret, orgKey []byte) error {
	enc, mac := orgKey[:32], orgKey[32:64]
	for _, field := range []struct {
		name string
		val  *string
	}{
		{"key", &s.Key},
		{"value", &s.Value},
		{"note", &s.Note},
	} {
		plain, err := decryptEncString(EncString(*field.val), enc, mac)
		if err != nil {
			return fmt.Errorf("decrypting the %s field: %w", field.name, err)
		}
		*field.val = plain
	}
	return nil
}

// GetSecretByRef resolves one ref to a secret — the shared leaf used by
// both Source.Resolve and the CLI. A uuid ref is fetched by id; anything
// else is a name resolved against the org's secret list (decrypted-key
// match, exactly one). Fail-closed: CanRead=false (grant-read cause) or an
// empty decrypted value (content cause) is an error naming the ref, the two
// causes distinguished. A NUL-bearing value is rejected naming the
// decrypted KEY.
func (c *Client) GetSecretByRef(ctx context.Context, ref string) (*Secret, error) {
	var s *Secret
	if IsUUIDRef(ref) {
		got, err := c.getSecret(ctx, ref)
		if err != nil {
			return nil, err
		}
		s = got
	} else {
		if c.orgID == "" {
			return nil, errors.New("bws: secret name refs need an organization id; re-login with \"mcp bws login --org\"")
		}
		items, err := c.ListSecrets(ctx)
		if err != nil {
			return nil, err
		}
		var matches []Secret
		for _, item := range items {
			if item.Key == ref {
				matches = append(matches, item)
			}
		}
		switch len(matches) {
		case 0:
			return nil, fmt.Errorf("bws: no secret named %q in organization %s", ref, c.orgID)
		case 1:
			got, err := c.getSecret(ctx, matches[0].ID)
			if err != nil {
				return nil, err
			}
			s = got
		default:
			ids := make([]string, 0, len(matches))
			for _, m := range matches {
				ids = append(ids, m.ID)
			}
			return nil, fmt.Errorf("bws: %d secrets are named %q (ids %s); use a secret's uuid instead", len(matches), ref, strings.Join(ids, ", "))
		}
	}
	if !s.CanRead {
		return nil, fmt.Errorf("bws: access denied to secret %q: the machine account's grant does not include read; check the machine account's access in Bitwarden", ref)
	}
	if s.Value == "" {
		return nil, fmt.Errorf("bws: secret %q resolves to an empty value; check the secret's content in Bitwarden", ref)
	}
	if strings.ContainsRune(s.Value, '\x00') {
		return nil, fmt.Errorf("bws: secret %q contains a NUL byte and cannot be used as a resolved value", s.Key)
	}
	return s, nil
}

// ListSecrets lists the org's secrets (metadata only, values absent from
// the wire) and decrypts each listed key. A non-empty continuationToken
// fails loud with its value: pagination is unsupported this round.
func (c *Client) ListSecrets(ctx context.Context) ([]Secret, error) {
	// Org guards fire before any network call: name resolution is the only
	// consumer needing the org id, and a missing/broken id must fail
	// pre-URL.
	if c.orgID == "" {
		return nil, errors.New("bws: listing secrets needs an organization id; re-login with \"mcp bws login --org\"")
	}
	if !IsUUIDRef(c.orgID) {
		return nil, fmt.Errorf("bws: organization id %q is not a valid uuid", c.orgID)
	}
	key, err := c.orgKey(ctx)
	if err != nil {
		return nil, err
	}
	body, err := c.apiCall(ctx, http.MethodGet, c.apiURL+"/organizations/"+c.orgID+"/secrets", nil)
	if err != nil {
		return nil, err
	}
	var envelope struct {
		ContinuationToken string   `json:"continuationToken"`
		Secrets           []Secret `json:"secrets"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil {
		return nil, fmt.Errorf("bws: the secret list response has an unexpected shape: %w", err)
	}
	if envelope.ContinuationToken != "" {
		return nil, fmt.Errorf("bws: the secret list returned a continuation token (%q); pagination is not supported this round", envelope.ContinuationToken)
	}
	enc, mac := key[:32], key[32:64]
	for i := range envelope.Secrets {
		plain, err := decryptEncString(EncString(envelope.Secrets[i].Key), enc, mac)
		if err != nil {
			return nil, fmt.Errorf("bws: secret %s: decrypting the listed key: %w", envelope.Secrets[i].ID, err)
		}
		envelope.Secrets[i].Key = plain
	}
	return envelope.Secrets, nil
}

// UpdateSecret PUTs the updated secret: key/value/note are encrypted with
// the org key, the body carries exactly the wire fields
// {id, organizationId, key, value, note, projectIds, valueChanged}.
func (c *Client) UpdateSecret(ctx context.Context, req SecretUpdateRequest) error {
	key, err := c.orgKey(ctx)
	if err != nil {
		return err
	}
	if !IsUUIDRef(req.ID) {
		return fmt.Errorf("bws: secret id %q is not a valid uuid", req.ID)
	}
	enc, mac := key[:32], key[32:64]
	var wire struct {
		ID             string    `json:"id"`
		OrganizationID string    `json:"organizationId"`
		Key            EncString `json:"key"`
		Value          EncString `json:"value"`
		Note           EncString `json:"note"`
		ProjectIDs     []string  `json:"projectIds"`
		ValueChanged   bool      `json:"valueChanged"`
	}
	wire.ID = req.ID
	wire.OrganizationID = req.OrganizationID
	if wire.Key, err = encryptEncString(req.Key, enc, mac); err != nil {
		return err
	}
	if wire.Value, err = encryptEncString(req.Value, enc, mac); err != nil {
		return err
	}
	if wire.Note, err = encryptEncString(req.Note, enc, mac); err != nil {
		return err
	}
	wire.ProjectIDs = req.ProjectIDs
	if wire.ProjectIDs == nil {
		wire.ProjectIDs = []string{}
	}
	wire.ValueChanged = req.ValueChanged
	body, err := json.Marshal(wire)
	if err != nil {
		return fmt.Errorf("bws: encoding the secret update: %w", err)
	}
	_, err = c.apiCall(ctx, http.MethodPut, c.apiURL+"/secrets/"+req.ID, body)
	return err
}
