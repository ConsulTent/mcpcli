// Package tus is a thin pure-Go tus 1.0.0 client (creation, HEAD, PATCH,
// termination) for resumable file uploads — the internal/bws
// thin-stdlib-client template applied to the tus protocol: no new
// dependencies, sentinel errors matched with errors.Is, byte-stable wrapper
// messages for goldens, bounded diagnostics (maxDiagBytes body echo,
// maxEchoChars server strings), and redirects never followed
// (http.ErrUseLastResponse — no credential re-POST, no https→http
// downgrade).
//
// Authentication honors the same transport contract as the go-sdk
// streamable transport: a nil handler sends nothing; otherwise the bearer
// comes from handler.TokenSource per request, and a 401/403 drives
// handler.Authorize followed by exactly one retry via the request factory.
package tus

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"

	sdkauth "github.com/modelcontextprotocol/go-sdk/auth"
	"golang.org/x/oauth2"
)

// tusVersion is the tus protocol version header value; every request the
// client sends carries Tus-Resumable: 1.0.0.
const tusVersion = "1.0.0"

// NormalizeEndpoint canonicalizes a tus endpoint base URL. It mirrors
// bws.NormalizeURL (absolute http/https only, no userinfo/query/fragment, a
// non-empty host — bare or port-bearing, e.g. http://:8080 — the host
// lowercased, the scheme's own default port stripped, the path kept) with
// ONE deliberate divergence: instead of trimming trailing slashes it
// ENSURES a trailing slash on a non-empty path. Tus endpoints are
// directory-style bases ("/files/"); a slashless "/files" gets ServeMux-301'd
// by tus servers, which http.ErrUseLastResponse turns into a bare redirect
// status error instead of a created upload. There is no scheme defaulting
// and no hostname rewriting.
func NormalizeEndpoint(raw string) (string, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return "", fmt.Errorf("invalid endpoint %q: %w", raw, err)
	}
	if u.Scheme == "" || u.Hostname() == "" {
		return "", fmt.Errorf("invalid endpoint %q: an absolute http(s) URL is required, e.g. https://files.example.com/", raw)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return "", fmt.Errorf("invalid endpoint %q: the scheme must be http or https, got %q", raw, u.Scheme)
	}
	if u.User != nil {
		return "", fmt.Errorf("invalid endpoint %q: userinfo is not allowed; use a plain https://host/path URL", raw)
	}
	if u.RawQuery != "" || u.ForceQuery || u.Fragment != "" {
		return "", fmt.Errorf("invalid endpoint %q: query strings and fragments are not allowed; use a plain https://host/path URL", raw)
	}
	host := strings.ToLower(u.Host)
	switch u.Scheme {
	case "https":
		host = strings.TrimSuffix(host, ":443")
	case "http":
		host = strings.TrimSuffix(host, ":80")
	}
	path := u.EscapedPath()
	if path != "" && !strings.HasSuffix(path, "/") {
		path += "/"
	}
	return u.Scheme + "://" + host + path, nil
}

// maxDiagBytes bounds response bodies kept for error diagnostics — the
// internal/bws and connection package patterns.
const maxDiagBytes = 4096

// maxEchoChars bounds how much of a server-controlled string an error
// echoes: enough prefix to identify the shape failure, never the whole
// value (the same bounded-diagnostics discipline at error scale).
const maxEchoChars = 64

// boundedEcho returns s's first maxEchoChars bytes, marked "...(truncated)"
// past that — the internal/bws marker. A server-controlled string (a
// Location header, a numeric header of unknown shape) must never reach
// errors or logs unbounded.
func boundedEcho(s string) string {
	if len(s) <= maxEchoChars {
		return s
	}
	return s[:maxEchoChars] + "...(truncated)"
}

// The four sentinels (D5), each with a named consumer: ErrUnauthorized
// (401: the auth retry boundary and the nil-handler 401 surface),
// ErrMismatchedOffset (409: the resync classification), ErrNotFound (404:
// resume/status handling — HeadOffset reports 404 as the exists bool, the
// sentinel only surfaces for other methods), ErrSizeExceeded (413: the
// chunk-cap hint). Callers match with errors.Is — never by string-matching
// the unioned messages, whose text is pinned by goldens and must stay
// verbatim.
var (
	ErrUnauthorized     = errors.New("tus: unauthorized")
	ErrMismatchedOffset = errors.New("tus: mismatched offset")
	ErrNotFound         = errors.New("tus: not found")
	ErrSizeExceeded     = errors.New("tus: size exceeds the server cap")
)

// statusError is the generic non-2xx wrapper: the wire status plus a
// maxDiagBytes-bounded body echo. One of the four sentinels rides it when
// the status has a named consumer (D5); Unwrap makes it errors.Is-matchable.
// Error() is byte-stable for goldens.
type statusError struct {
	status   string
	diag     string
	sentinel error
}

func (e *statusError) Error() string {
	if e.diag != "" {
		return fmt.Sprintf("tus: the server returned %s: %s", e.status, e.diag)
	}
	return fmt.Sprintf("tus: the server returned %s", e.status)
}

func (e *statusError) Unwrap() error { return e.sentinel }

// mismatchedOffsetError is a PATCH 409. Per D5 it embeds the client-side
// offset and the status only — the resync HEAD, not the 409's (absent or
// unreliable) Upload-Offset header, is the sole source of the server
// offset, keeping Error() byte-stable for goldens.
type mismatchedOffsetError struct {
	offset int64
	status string
	diag   string
}

func (e *mismatchedOffsetError) Error() string {
	if e.diag != "" {
		return fmt.Sprintf("tus: the server rejected the PATCH offset %d with %s: %s", e.offset, e.status, e.diag)
	}
	return fmt.Sprintf("tus: the server rejected the PATCH offset %d with %s", e.offset, e.status)
}

func (e *mismatchedOffsetError) Unwrap() error { return ErrMismatchedOffset }

// classifyStatus reads a non-2xx response into a statusError, mapping the
// sentinel-bearing statuses (D5). It consumes the response body (bounded).
func classifyStatus(resp *http.Response) error {
	diag, _ := io.ReadAll(io.LimitReader(resp.Body, maxDiagBytes))
	e := &statusError{status: resp.Status, diag: string(diag)}
	switch resp.StatusCode {
	case http.StatusUnauthorized:
		e.sentinel = ErrUnauthorized
	case http.StatusNotFound:
		e.sentinel = ErrNotFound
	case http.StatusRequestEntityTooLarge:
		e.sentinel = ErrSizeExceeded
	}
	return e
}

// Client is a thin tus 1.0.0 client for one endpoint. It is deliberately
// single-goroutine per run (the bws Client posture). The handler is the
// mcpcli auth registry's sdkauth.OAuthHandler — nil (--auth none) means no
// token injection and no Authorize calls; base carries the basic-mode
// credential round tripper (interface{ TransportHTTPClient() *http.Client
// }) and serves only with insecure=false (the CLI rejects that combo, so
// insecure wins when both would apply).
type Client struct {
	endpoint string
	handler  sdkauth.OAuthHandler
	base     http.RoundTripper
	insecure bool
	http     *http.Client

	// Warn is the functional security note channel, not decoration: when an
	// https endpoint's 201 Location redirects the upload to plain http, one
	// line lands here before any PATCH is sent (the credential would travel
	// unencrypted). Zero value nil stays silent — a nil-guarded write.
	Warn io.Writer
}

// NewClient builds a Client for one tus endpoint. Like bws.NewClient it
// normalizes defensively so a raw input still yields working request URLs —
// a normalization failure keeps the raw form and the request-time URL parse
// fails loudly. NewClient never fails. When insecure is set, the transport
// is http.DefaultTransport.(*http.Transport).Clone() with
// TLSClientConfig.InsecureSkipVerify = true — a Clone preserves the proxy
// environment and HTTP/2 that a bare &http.Transport{} would silently drop,
// and proxy + self-signed-MITM is exactly the --insecure demographic.
func NewClient(endpoint string, handler sdkauth.OAuthHandler, base http.RoundTripper, insecure bool) *Client {
	if n, err := NormalizeEndpoint(endpoint); err == nil {
		endpoint = n
	}
	c := &Client{
		endpoint: endpoint,
		handler:  handler,
		base:     base,
		insecure: insecure,
	}
	transport := base
	if insecure {
		if def, ok := http.DefaultTransport.(*http.Transport); ok {
			clone := def.Clone()
			if clone.TLSClientConfig == nil {
				clone.TLSClientConfig = &tls.Config{}
			}
			clone.TLSClientConfig.InsecureSkipVerify = true
			transport = clone
		}
	}
	c.http = &http.Client{
		Transport: transport,
		// No credential re-POST to 307/308 targets, no silent https→http
		// downgrade: a redirect surfaces as its 3xx status.
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	return c
}

// do performs one request honoring the SDK transport contract. mkReq is a
// factory: do builds the request, injects the bearer from the handler's
// TokenSource (per request — a retried request re-derives its token), sends
// it, and on a 401/403 with a non-nil handler calls Authorize and retries
// exactly once via mkReq on success. An Authorize error propagates wrapped
// with ZERO retries — the handler owns and closes the response body after
// the handoff, so do never reads or closes it on that path. With a nil
// handler nothing is injected and no Authorize runs: a 401 surfaces through
// the method's status classification as ErrUnauthorized.
func (c *Client) do(mkReq func() (*http.Request, error)) (*http.Response, error) {
	req, resp, err := c.send(mkReq)
	if err != nil {
		return nil, err
	}
	if (resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden) && c.handler != nil {
		if err := c.handler.Authorize(req.Context(), req, resp); err != nil {
			return nil, fmt.Errorf("tus: the auth handler failed to authorize the request: %w", err)
		}
		// Retry exactly once via the factory; the retried request may hit
		// 401/403 again — per the SDK contract Authorize must not run a
		// second time, so the response is returned for the caller to
		// classify.
		_, resp, err = c.send(mkReq)
		if err != nil {
			return nil, err
		}
	}
	return resp, nil
}

// send builds one request from the factory, stamps the tus protocol header,
// injects the bearer per the SDK contract and sends it. The request is
// returned with the response so do can hand the failing request to
// handler.Authorize.
func (c *Client) send(mkReq func() (*http.Request, error)) (*http.Request, *http.Response, error) {
	req, err := mkReq()
	if err != nil {
		return nil, nil, fmt.Errorf("tus: building the request: %w", err)
	}
	req.Header.Set("Tus-Resumable", tusVersion)
	if err := c.injectToken(req); err != nil {
		return nil, nil, err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return req, nil, fmt.Errorf("tus: the %s request failed: %w", req.Method, err)
	}
	return req, resp, nil
}

// injectToken mirrors the go-sdk streamable transport's token injection:
// a nil handler means no header; a TokenSource error aborts wrapped before
// any request fires; a ts.Token() error aborts wrapped UNLESS it is an
// oauth2.RetrieveError with ErrorCode "invalid_grant" — RFC 6749 §5.2, the
// token source lost its grant (e.g. an expired refresh token) — in which
// case the request proceeds headerless so the 401/403 drives
// handler.Authorize and the refresh path stays reachable.
func (c *Client) injectToken(req *http.Request) error {
	if c.handler == nil {
		return nil
	}
	ts, err := c.handler.TokenSource(req.Context())
	if err != nil {
		return fmt.Errorf("tus: the auth handler failed to provide a token source: %w", err)
	}
	if ts == nil {
		return nil
	}
	token, err := ts.Token()
	if err != nil {
		var retrieveErr *oauth2.RetrieveError
		if !errors.As(err, &retrieveErr) || retrieveErr.ErrorCode != "invalid_grant" {
			return fmt.Errorf("tus: the auth handler failed to fetch a token: %w", err)
		}
		return nil // proceed headerless; the 401/403 drives Authorize
	}
	if token != nil {
		req.Header.Set("Authorization", "Bearer "+token.AccessToken)
	}
	return nil
}

// encodeMetadata renders a metadata map into the tus Upload-Metadata wire
// form: comma-separated "key <base64.StdEncoding value>" pairs (the tus
// 1.0.0 creation extension), keys sorted so repeated calls are
// deterministic. Keys must be non-empty visible ASCII without spaces or
// commas — anything else fails closed before any network call with the
// house error form.
func encodeMetadata(metadata map[string]string) (string, error) {
	if len(metadata) == 0 {
		return "", nil
	}
	keys := make([]string, 0, len(metadata))
	for k := range metadata {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	pairs := make([]string, 0, len(keys))
	for _, k := range keys {
		if k == "" {
			return "", fmt.Errorf("tus: invalid metadata key %q: metadata keys must be non-empty printable ASCII without spaces or commas", k)
		}
		for i := 0; i < len(k); i++ {
			// Visible ASCII is 0x21..0x7E; the tus grammar additionally
			// excludes the pair separators — space (below 0x21) and comma
			// (0x2C, inside the range — rejected explicitly).
			if k[i] < 0x21 || k[i] > 0x7E || k[i] == ',' {
				return "", fmt.Errorf("tus: invalid metadata key %q: metadata keys must be non-empty printable ASCII without spaces or commas", k)
			}
		}
		pairs = append(pairs, k+" "+base64.StdEncoding.EncodeToString([]byte(metadata[k])))
	}
	return strings.Join(pairs, ","), nil
}

// Create starts an upload via the tus creation extension: POST to the
// endpoint with Upload-Length and Upload-Metadata (keys validated before
// any network call), expecting a 201 whose Location names the upload URL.
// Location is resolved through resp.Location(), so relative and absolute
// values both yield absolute URLs; a missing or unparseable one surfaces as
// an error carrying only a maxEchoChars-bounded echo.
func (c *Client) Create(ctx context.Context, length int64, metadata map[string]string) (string, error) {
	wire, err := encodeMetadata(metadata)
	if err != nil {
		return "", err
	}
	mkReq := func() (*http.Request, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint, nil)
		if err != nil {
			return nil, err
		}
		req.Header.Set("Upload-Length", strconv.FormatInt(length, 10))
		if wire != "" {
			req.Header.Set("Upload-Metadata", wire)
		}
		return req, nil
	}
	resp, err := c.do(mkReq)
	if err != nil {
		return "", err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusCreated {
		return "", classifyStatus(resp)
	}
	loc := resp.Header.Get("Location")
	if loc == "" {
		return "", errors.New("tus: the server's 201 response carries no Location header")
	}
	u, err := resp.Location()
	if err != nil {
		// The inner *url.Error's text would echo the raw Location value;
		// only its reason is diagnostic and clean of the raw URL. The value
		// itself rides bounded.
		msg := fmt.Sprintf("tus: the server's 201 Location header %q is not a usable URL", boundedEcho(loc))
		var ue *url.Error
		if errors.As(err, &ue) && ue.Err != nil {
			msg += ": " + ue.Err.Error()
		}
		return "", errors.New(msg)
	}
	c.warnLocationDowngrade(u)
	return u.String(), nil
}

// warnLocationDowngrade emits the one-line functional security note when an
// https endpoint's 201 Location redirects the upload to plain http: every
// PATCH would carry the credential unencrypted. The server-controlled URL
// rides bounded (maxEchoChars); a nil Warn stays silent.
func (c *Client) warnLocationDowngrade(u *url.URL) {
	if c.Warn == nil {
		return
	}
	eu, err := url.Parse(c.endpoint)
	if err != nil || eu.Scheme != "https" || u.Scheme != "http" {
		return
	}
	_, _ = fmt.Fprintf(c.Warn, "warning: the server redirected the upload to plain http (%s); the credential travels unencrypted\n", boundedEcho(u.String()))
}

// HeadOffset asks the server for an upload's progress. A 404 is data, not
// an error: it returns (0, 0, false, nil) so callers classify on the exists
// bool and never on sentinel shape (D5). On a 2xx response both
// Upload-Offset and Upload-Length are required and numeric; violations
// surface as errors carrying a bounded echo of the offending value.
func (c *Client) HeadOffset(ctx context.Context, url string) (offset, length int64, exists bool, err error) {
	mkReq := func() (*http.Request, error) {
		return http.NewRequestWithContext(ctx, http.MethodHead, url, nil)
	}
	resp, err := c.do(mkReq)
	if err != nil {
		return 0, 0, false, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusNotFound {
		return 0, 0, false, nil
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return 0, 0, false, classifyStatus(resp)
	}
	offStr := resp.Header.Get("Upload-Offset")
	if offStr == "" {
		return 0, 0, true, errors.New("tus: the HEAD response carries no Upload-Offset header")
	}
	offset, err = strconv.ParseInt(offStr, 10, 64)
	if err != nil {
		return 0, 0, true, fmt.Errorf("tus: the HEAD response's Upload-Offset header %q is not a number: %w", boundedEcho(offStr), err)
	}
	lenStr := resp.Header.Get("Upload-Length")
	if lenStr == "" {
		return 0, 0, true, errors.New("tus: the HEAD response carries no Upload-Length header")
	}
	length, err = strconv.ParseInt(lenStr, 10, 64)
	if err != nil {
		return 0, 0, true, fmt.Errorf("tus: the HEAD response's Upload-Length header %q is not a number: %w", boundedEcho(lenStr), err)
	}
	return offset, length, true, nil
}

// Patch sends one chunk at the given offset, expecting a 204 whose
// Upload-Offset names the new offset. Content-Type is the tus media type
// and req.ContentLength is set explicitly — a PATCH is never chunked TE.
// size must equal the body length. The body is buffered once so the
// authorize-retry path replays the WHOLE chunk from the request factory: a
// retried PATCH must never carry a consumed reader (the SDK transport
// buffers the same way); memory stays bounded by the caller's chunk size.
func (c *Client) Patch(ctx context.Context, url string, offset int64, body io.Reader, size int64) (int64, error) {
	payload, err := io.ReadAll(body)
	if err != nil {
		return 0, fmt.Errorf("tus: reading the PATCH body: %w", err)
	}
	if size != int64(len(payload)) {
		return 0, fmt.Errorf("tus: the PATCH body is %d bytes, but size is %d", len(payload), size)
	}
	mkReq := func() (*http.Request, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodPatch, url, bytes.NewReader(payload))
		if err != nil {
			return nil, err
		}
		req.Header.Set("Content-Type", "application/offset+octet-stream")
		req.Header.Set("Upload-Offset", strconv.FormatInt(offset, 10))
		req.ContentLength = size
		return req, nil
	}
	resp, err := c.do(mkReq)
	if err != nil {
		return 0, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusConflict {
		diag, _ := io.ReadAll(io.LimitReader(resp.Body, maxDiagBytes))
		return 0, &mismatchedOffsetError{offset: offset, status: resp.Status, diag: string(diag)}
	}
	if resp.StatusCode != http.StatusNoContent {
		return 0, classifyStatus(resp)
	}
	newOff := resp.Header.Get("Upload-Offset")
	if newOff == "" {
		return 0, errors.New("tus: the 204 response carries no Upload-Offset header")
	}
	n, err := strconv.ParseInt(newOff, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("tus: the 204 response's Upload-Offset header %q is not a number: %w", boundedEcho(newOff), err)
	}
	return n, nil
}

// Delete removes a staged upload via the tus termination extension,
// expecting a 204. A 404 surfaces as ErrNotFound (the CLI layer decides
// whether it is data or an error); 410 and everything else ride the generic
// wrapper.
func (c *Client) Delete(ctx context.Context, url string) error {
	mkReq := func() (*http.Request, error) {
		return http.NewRequestWithContext(ctx, http.MethodDelete, url, nil)
	}
	resp, err := c.do(mkReq)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusNoContent {
		return classifyStatus(resp)
	}
	return nil
}
