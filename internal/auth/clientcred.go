package auth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
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

	issuer, resource := h.discoverIssuer(ctx, req, resp)

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
	//
	// Resource choice (RFC 8707): the resource the protected-resource
	// metadata advertises — the RFC-correct audience — falling back to the
	// connect URL. The indicator rides EndpointParams under the "resource"
	// key, never "scope": a collision there returns an "oauth2: cannot
	// overwrite parameter" error.
	cfg := clientcredentials.Config{
		ClientID:       h.creds.ClientID,
		ClientSecret:   h.creds.ClientSecret,
		TokenURL:       meta.TokenEndpoint,
		Scopes:         meta.ScopesSupported,
		EndpointParams: url.Values{"resource": {resource}},
	}
	src := cfg.TokenSource(ctx)

	h.mu.Lock()
	h.src = src
	h.mu.Unlock()
	return nil
}

// discoverIssuer determines which authorization server to query and which
// resource indicator (RFC 8707) to request alongside the grant.
//
//  1. A WWW-Authenticate challenge pointing at resource metadata (the
//     resource_metadata parameter) wins. The fetch is tolerant by design: a
//     failure to fetch, a resource that does not match the connect URL, or a
//     document naming no authorization server falls through to the defaults
//     rather than aborting.
//  2. Otherwise the default RFC 9728 location on the request's origin
//     (with the path inserted) is tried; failures are ignored because the
//     document is optional.
//  3. Otherwise the request's own origin is assumed to be the issuer, letting
//     GetAuthServerMetadata try the RFC 8414 well-known endpoints.
//
// The returned resource is the advertised prm.Resource when the document
// carries one — requests for the advertised resource are the RFC-correct
// audience — with the connect URL (h.target) as the fallback.
func (h *ccHandler) discoverIssuer(ctx context.Context, req *http.Request, resp *http.Response) (issuer, resource string) {
	if u := resourceMetadataURL(resp); u != "" {
		if prm, err := fetchPRM(ctx, u); err == nil && len(prm.AuthorizationServers) > 0 {
			// The first advertised authorization server is used; multiple
			// servers are rare and no selection policy exists yet.
			return prm.AuthorizationServers[0], h.advertisedResource(prm.Resource)
		}
		// The doc could not be fetched or named no authorization server;
		// fall through to the default location.
	}

	if issuer, resource, ok := h.issuerFromDefaultPRM(ctx, req.URL); ok {
		return issuer, resource
	}
	return issuerOrigin(req.URL), h.target
}

// advertisedResource prefers the resource the metadata advertises, falling
// back to the connect URL when the document omits it.
func (h *ccHandler) advertisedResource(advertised string) string {
	if advertised != "" {
		return advertised
	}
	return h.target
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
// for the request URL and reports the first advertised authorization server
// together with the resource it advertises. ok is false whenever the document
// cannot be used.
func (h *ccHandler) issuerFromDefaultPRM(ctx context.Context, u *url.URL) (issuer, resource string, ok bool) {
	prmURL := *u
	prmURL.Path = defaultPRMPath(u.Path)
	prmURL.RawQuery = ""
	prmURL.Fragment = ""

	prm, err := fetchPRM(ctx, prmURL.String())
	if err != nil || len(prm.AuthorizationServers) == 0 {
		return "", "", false
	}
	return prm.AuthorizationServers[0], h.advertisedResource(prm.Resource), true
}

// fetchPRM fetches and decodes an RFC 9728 protected-resource metadata
// document from metadataURL. It mirrors the SDK discovery fetch's guards on
// this GET — the URL check below (https always; http only on loopback hosts;
// and non-loopback private/reserved literal IPs rejected), a no-redirect
// policy, and the same 1 MiB read bound — while deliberately NOT enforcing
// the SDK's resource equality: the RFC 9728 §3.3 mismatch is the layer-1
// failure this helper exists to tolerate, and the authorization_servers
// entries are validated downstream by GetAuthServerMetadata before the
// credential POST. A failure here is never fatal by itself; callers fall
// through the candidate chain.
//
// shortcut: the hostname→private-IP dial axis stays open without a custom
// Control dialer (~15 lines); accepted for an unauthenticated GET whose body
// is parsed-and-discarded while the credential-bearing leg stays guarded
// downstream.
func fetchPRM(ctx context.Context, metadataURL string) (*oauthex.ProtectedResourceMetadata, error) {
	if err := checkPRMURL(metadataURL); err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, metadataURL, nil)
	if err != nil {
		return nil, err
	}
	// Redirects are never followed: a server-controlled 302 must not carry
	// this fetch to an internal host past the initial URL check. A
	// redirecting PRM endpoint becomes a fallthrough of the candidate chain.
	client := &http.Client{
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return errors.New("fetchPRM: redirect not followed")
		},
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("bad status %d", resp.StatusCode)
	}
	prm := new(oauthex.ProtectedResourceMetadata)
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(prm); err != nil {
		return nil, err
	}
	return prm, nil
}

// checkPRMURL reimplements both clauses of the SDK's checkHTTPSOrLoopback
// for the PRM fetch URL: the scheme must be https, or http on a loopback
// host; and a literal private/reserved IP (RFC 1918/4193, link-local —
// including the 169.254.169.254 cloud metadata endpoint —, multicast, the
// unspecified address, and RFC 6598 CGNAT 100.64.0.0/10) is rejected. The
// Unmap keeps IPv4-mapped IPv6 literals from slipping past the
// BitLen-strict checks. The challenge URL is server-controlled, so relaxing
// either clause would be an SSRF-guard relaxation on that GET;
// loopback/localhost/httptest hosts pass exactly as before.
func checkPRMURL(addr string) error {
	if addr == "" {
		return nil
	}
	u, err := url.Parse(addr)
	if err != nil {
		return err
	}
	if !isLoopbackHost(u.Host) && u.Scheme != "https" {
		return fmt.Errorf("URL %q does not use HTTPS or is not a loopback address", addr)
	}
	if ip, err := netip.ParseAddr(u.Hostname()); err == nil && isPrivateOrReservedIP(ip) {
		return fmt.Errorf("URL %q refers to a non-public address", addr)
	}
	return nil
}

// isLoopbackHost reports whether a URL host (host or host:port) is the
// literal "localhost" or a loopback IP.
func isLoopbackHost(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		// SplitHostPort fails on a bare host without a port.
		host = strings.Trim(addr, "[]")
	}
	if host == "localhost" {
		return true
	}
	ip, err := netip.ParseAddr(host)
	if err != nil {
		return false
	}
	return ip.IsLoopback()
}

// cgnatPrefix is the RFC 6598 carrier-grade NAT range (100.64.0.0/10);
// netip's IsPrivate does not report it, but it is shared ISP space that is
// not publicly routable and could point at another tenant behind the same NAT.
var cgnatPrefix = netip.MustParsePrefix("100.64.0.0/10")

// isPrivateOrReservedIP reports whether ip belongs to a range that must not
// be reached through a server-controlled URL, as a defense against SSRF.
func isPrivateOrReservedIP(ip netip.Addr) bool {
	ip = ip.Unmap()
	if !ip.IsValid() {
		return true
	}
	return ip.IsPrivate() ||
		ip.IsLinkLocalUnicast() ||
		ip.IsMulticast() ||
		ip.IsUnspecified() ||
		cgnatPrefix.Contains(ip)
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
