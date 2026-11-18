package auth

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"net/url"

	sdkauth "github.com/modelcontextprotocol/go-sdk/auth"
	"golang.org/x/oauth2"
)

// basicProvider implements the "basic" mode: HTTP Basic authentication with a
// username/email and token supplied via --client-id/--client-secret, sent as
// an Authorization: Basic header scoped to the target's scheme and host. No
// discovery, no network at build time. Like bearer, it is an HTTP-header mode:
// connection.Open rejects it for stdio servers.
type basicProvider struct{}

func (basicProvider) Name() string { return "basic" }

func (basicProvider) Build(_ context.Context, p Params) (sdkauth.OAuthHandler, func(), error) {
	if p.Creds.ClientID == "" || p.Creds.ClientSecret == "" {
		return nil, nil, fmt.Errorf(
			"basic auth requires both credentials: pass --client-id and --client-secret " +
				"or set MCPCLI_CLIENT_ID and MCPCLI_CLIENT_SECRET")
	}
	// Fail closed on a malformed --url: silently computing an empty host would
	// never send the credential and surface a misleading rejected-credentials
	// error later. On stdio runs Params.Target is the raw --command string; a
	// %-escape-bearing command parses as an invalid URL and surfaces this
	// error instead of Open's stdio rejection — both paths exit 1 fail-closed.
	u, err := url.Parse(p.Target)
	if err != nil {
		return nil, nil, fmt.Errorf("basic auth: invalid --url %q", p.Target)
	}
	return &basicHandler{
		user:         p.Creds.ClientID,
		pass:         p.Creds.ClientSecret,
		targetScheme: u.Scheme,
		targetHost:   u.Host,
	}, nil, nil
}

// basicHandler carries the Basic credential plus the scheme and host scope it
// may be sent to, computed once at Build from the parsed target URL.
type basicHandler struct {
	user, pass   string
	targetScheme string
	targetHost   string
}

// TokenSource returns nil so the SDK transport adds no Authorization header
// itself; the Basic header is injected by this handler's own
// TransportHTTPClient round tripper instead.
func (h *basicHandler) TokenSource(context.Context) (oauth2.TokenSource, error) {
	return nil, nil
}

// Authorize always fails: Basic credentials are static, so a 401/403 must
// surface instead of retrying with the same doomed credential. Per the SDK
// contract the handler owns the response and closes its body first.
func (h *basicHandler) Authorize(_ context.Context, _ *http.Request, resp *http.Response) error {
	_ = resp.Body.Close()
	return errors.New("basic auth: the credentials were rejected and cannot be refreshed")
}

// basicRoundTripper injects the Basic Authorization header onto requests
// scoped to the Build-computed target scheme AND host. Scheme equality is
// required alongside host equality: Go strips sensitive headers on domain
// change only — not scheme change — and a redirect hop's request is rebuilt
// from the initial header map, so a same-host https→http downgrade would
// otherwise re-pass this wrapper and carry the credential in plaintext.
type basicRoundTripper struct {
	base         http.RoundTripper
	user, pass   string
	targetScheme string
	targetHost   string
}

func (t *basicRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.URL.Host == t.targetHost && req.URL.Scheme == t.targetScheme &&
		req.Header.Get("Authorization") == "" {
		req.Header.Set("Authorization", "Basic "+
			base64.StdEncoding.EncodeToString([]byte(t.user+":"+t.pass)))
	}
	return t.base.RoundTrip(req)
}

// TransportHTTPClient returns an http.Client that injects the Basic credential
// onto scheme+host-scoped requests (see basicRoundTripper). The streamable
// transport honors it through StreamableClientTransport's HTTPClient field;
// discovery and metadata requests to the authorization server never receive
// the credential.
func (h *basicHandler) TransportHTTPClient() *http.Client {
	return &http.Client{
		Transport: &basicRoundTripper{
			base:         http.DefaultTransport,
			user:         h.user,
			pass:         h.pass,
			targetScheme: h.targetScheme,
			targetHost:   h.targetHost,
		},
	}
}
