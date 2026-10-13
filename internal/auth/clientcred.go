package auth

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"

	sdkauth "github.com/modelcontextprotocol/go-sdk/auth"
	"github.com/modelcontextprotocol/go-sdk/oauthex"
	"golang.org/x/oauth2"
	"golang.org/x/oauth2/clientcredentials"
)

// clientCredentialsProvider implements the "client-credentials" mode: a 2-legged
// OAuth2 grant against a token endpoint discovered from the server's own
// 401 response (RFC 9728 protected-resource metadata, then RFC 8414
// authorization-server metadata).
//
// SDK contract honored here: TokenSource returns nil until the endpoint is
// known, so the transport sends no Authorization header and the server's
// 401/403 drives discovery; Authorize performs discovery + the token grant
// and returns nil, after which the transport retries with the header.
type clientCredentialsProvider struct{}

func (clientCredentialsProvider) Name() string { return "client-credentials" }

func (clientCredentialsProvider) Build(_ context.Context, p Params) (sdkauth.OAuthHandler, func(), error) {
	if p.Creds.ClientID == "" || p.Creds.ClientSecret == "" {
		return nil, nil, fmt.Errorf(
			"client-credentials auth requires both credentials: pass --client-id and --client-secret " +
				"or set MCPCLI_CLIENT_ID and MCPCLI_CLIENT_SECRET")
	}
	// Build performs no network I/O; discovery happens lazily in Authorize.
	return &ccHandler{creds: p.Creds, target: p.Target}, nil, nil
}

type ccHandler struct {
	creds  Credentials
	target string

	mu  sync.Mutex
	src oauth2.TokenSource // nil until Authorize discovers the token endpoint
}

// TokenSource returns the discovered source, or nil before discovery so the
// transport sends the request without an Authorization header.
func (h *ccHandler) TokenSource(context.Context) (oauth2.TokenSource, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.src, nil
}

// Authorize discovers the authorization server from the 401/403 response,
// fetches a client-credentials token source, and stores it for TokenSource.
// Per the SDK contract it takes ownership of and closes the response body.
func (h *ccHandler) Authorize(ctx context.Context, req *http.Request, resp *http.Response) error {
	defer resp.Body.Close()

	issuer, err := h.discoverIssuer(ctx, req, resp)
	if err != nil {
		return err
	}

	meta, err := sdkauth.GetAuthServerMetadata(ctx, issuer, http.DefaultClient)
	if err != nil {
		return fmt.Errorf("fetching authorization server metadata for %s: %w", issuer, err)
	}
	if meta == nil || meta.TokenEndpoint == "" {
		return fmt.Errorf("authorization server %s advertises no token endpoint; "+
			"cannot use client-credentials auth", issuer)
	}

	// Scope choice: use the scopes the authorization server advertises
	// (ScopesSupported); when it advertises none, request none rather than
	// inventing a default.
	cfg := clientcredentials.Config{
		ClientID:     h.creds.ClientID,
		ClientSecret: h.creds.ClientSecret,
		TokenURL:     meta.TokenEndpoint,
		Scopes:       meta.ScopesSupported,
	}
	src := cfg.TokenSource(ctx)

	h.mu.Lock()
	h.src = src
	h.mu.Unlock()
	return nil
}

// discoverIssuer determines which authorization server to query.
//
//  1. A WWW-Authenticate challenge pointing at resource metadata (the
//     resource_metadata parameter) wins; a failure to fetch it is fatal.
//  2. Otherwise the default RFC 9728 location on the request's origin
//     (with the path inserted) is tried; failures are ignored because the
//     document is optional.
//  3. Otherwise the request's own origin is assumed to be the issuer, letting
//     GetAuthServerMetadata try the RFC 8414 well-known endpoints.
func (h *ccHandler) discoverIssuer(ctx context.Context, req *http.Request, resp *http.Response) (string, error) {
	if u := resourceMetadataURL(resp); u != "" {
		prm, err := oauthex.GetProtectedResourceMetadata(ctx, u, req.URL.String(), http.DefaultClient)
		if err != nil {
			return "", fmt.Errorf("fetching protected resource metadata: %w", err)
		}
		if len(prm.AuthorizationServers) > 0 {
			// The first advertised authorization server is used; multiple
			// servers are rare and no selection policy exists yet.
			return prm.AuthorizationServers[0], nil
		}
		// The doc named no authorization server; fall through to defaults.
	}

	if issuer, err := h.issuerFromDefaultPRM(ctx, req.URL); err == nil {
		return issuer, nil
	}
	return issuerOrigin(req.URL), nil
}

// resourceMetadataURL extracts the resource_metadata parameter from the
// WWW-Authenticate challenge headers, preferring Bearer challenges.
func resourceMetadataURL(resp *http.Response) string {
	challenges, err := oauthex.ParseWWWAuthenticate(resp.Header.Values("WWW-Authenticate"))
	if err != nil {
		return ""
	}
	// Two passes: a Bearer challenge first, then any other scheme.
	for _, prefer := range []string{"bearer", ""} {
		for _, ch := range challenges {
			if prefer != "" && ch.Scheme != prefer {
				continue
			}
			if u := ch.Params["resource_metadata"]; u != "" {
				return u
			}
		}
	}
	return ""
}

// issuerFromDefaultPRM tries the default protected-resource metadata location
// for the request URL and returns the first advertised authorization server.
// It returns an error whenever the document cannot be used.
func (h *ccHandler) issuerFromDefaultPRM(ctx context.Context, u *url.URL) (string, error) {
	prmURL := *u
	prmURL.Path = defaultPRMPath(u.Path)
	prmURL.RawQuery = ""
	prmURL.Fragment = ""

	prm, err := oauthex.GetProtectedResourceMetadata(ctx, prmURL.String(), u.String(), http.DefaultClient)
	if err != nil {
		return "", err
	}
	if len(prm.AuthorizationServers) == 0 {
		return "", fmt.Errorf("protected resource metadata names no authorization servers")
	}
	return prm.AuthorizationServers[0], nil
}

// defaultPRMPath renders the RFC 9728 default metadata path for a resource
// path: "/.well-known/oauth-protected-resource" with the resource path
// inserted after it.
func defaultPRMPath(path string) string {
	path = strings.TrimSuffix(path, "/")
	return "/.well-known/oauth-protected-resource" + path
}

// issuerOrigin reduces a URL to scheme://host[:port], the fallback issuer
// used when no protected-resource metadata is available.
func issuerOrigin(u *url.URL) string {
	origin := *u
	origin.Path = ""
	origin.RawQuery = ""
	origin.Fragment = ""
	return origin.String()
}
