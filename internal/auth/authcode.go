package auth

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"

	sdkauth "github.com/modelcontextprotocol/go-sdk/auth"
	"github.com/modelcontextprotocol/go-sdk/oauthex"
	"golang.org/x/oauth2"
)

// oauthProvider implements the "oauth" mode: the SDK's authorization-code
// flow (RFC 8414 discovery from the server's 401, PKCE, RFC 9207 iss
// checking) driven either by a local loopback receiver or by a pasted code.
//
// No browser is ever launched: the loopback flow prints the authorization
// URL and waits for the user's own browser to hit the local receiver; the
// manual flow prints the URL and reads the code from stdin.
type oauthProvider struct{}

func (oauthProvider) Name() string { return "oauth" }

// manualRedirectURL is the redirect URI used by the manual flow: a loopback
// address nothing listens on. The browser lands on a connection-error page
// and the user copies the code out of its address bar. With dynamic client
// registration it is registered per login run; with a pre-registered client
// the server must have this redirect registered for the client.
const manualRedirectURL = "http://127.0.0.1:1/callback"

func (oauthProvider) Build(ctx context.Context, p Params) (sdkauth.OAuthHandler, func(), error) {
	stderr := p.Stderr
	if stderr == nil {
		stderr = io.Discard
	}

	var (
		fetcher     sdkauth.AuthorizationCodeFetcher
		redirectURL string
		closeFn     = func() {} // manual flow cleans up nothing
	)
	switch p.Flow {
	case "loopback":
		var err error
		fetcher, redirectURL, closeFn, err = newLoopbackFetcher(ctx, stderr)
		if err != nil {
			return nil, nil, err
		}
	case "manual":
		if p.Stdin == nil {
			return nil, nil, errors.New("oauth manual flow needs a reader to read the code from (no stdin)")
		}
		fetcher = newManualFetcher(stderr, p.Stdin)
		redirectURL = manualRedirectURL
	default:
		return nil, nil, fmt.Errorf("unknown auth flow %q (want \"loopback\" or \"manual\")", p.Flow)
	}

	cfg := &sdkauth.AuthorizationCodeHandlerConfig{
		RedirectURL:              redirectURL,
		AuthorizationCodeFetcher: fetcher,
		RequestRefreshToken:      true,
	}
	if prereg, dcr := pickRegistration(&p.Creds, redirectURL); prereg != nil {
		cfg.PreregisteredClient = prereg
	} else {
		cfg.DynamicClientRegistrationConfig = dcr
	}

	// Reuse a cached token when one is usable, so repeat logins (and any
	// later authenticated run) skip the browser round-trip. An unreadable
	// cache is ignored: the flow re-authorizes and overwrites it.
	if tok, err := LoadToken(p.Target); err == nil && tok != nil && tok.AccessToken != "" {
		cfg.InitialTokenSource = oauth2.StaticTokenSource(tok)
	}

	// After the code is exchanged, persist the fresh token; the wrapper then
	// persists every refresh so the cache always holds a restorable session.
	cfg.NewTokenSource = func(tctx context.Context, oc *oauth2.Config, tok *oauth2.Token) (oauth2.TokenSource, error) {
		if err := SaveToken(p.Target, tok); err != nil {
			return nil, fmt.Errorf("saving token: %w", err)
		}
		return &savingTokenSource{
			wrapped: oc.TokenSource(tctx, tok),
			target:  p.Target,
			last:    tok.AccessToken,
		}, nil
	}

	h, err := sdkauth.NewAuthorizationCodeHandler(cfg)
	if err != nil {
		closeFn() // release the loopback receiver on config errors
		return nil, nil, err
	}
	return h, closeFn, nil
}

// pickRegistration chooses how mcp identifies itself to the authorization
// server: a pre-registered client when a client ID is available (confidential
// when a secret is also present, public otherwise), dynamic client
// registration otherwise.
func pickRegistration(creds *Credentials, redirectURL string) (*oauthex.ClientCredentials, *sdkauth.DynamicClientRegistrationConfig) {
	if creds.ClientID != "" {
		prereg := &oauthex.ClientCredentials{ClientID: creds.ClientID}
		if creds.ClientSecret != "" {
			prereg.ClientSecretAuth = &oauthex.ClientSecretAuth{ClientSecret: creds.ClientSecret}
		}
		return prereg, nil
	}
	return nil, &sdkauth.DynamicClientRegistrationConfig{
		Metadata: &oauthex.ClientRegistrationMetadata{
			ClientName:   "mcp",
			RedirectURIs: []string{redirectURL},
			// RequestRefreshToken wants refresh tokens, and many servers
			// only issue them to clients advertising the grant type.
			GrantTypes: []string{"authorization_code", "refresh_token"},
		},
	}
}

// callbackPage is the small confirmation page the loopback receiver serves
// after capturing the authorization response.
const callbackPage = `<!DOCTYPE html>
<html>
<head><title>mcp authorization</title></head>
<body style="font-family: sans-serif; text-align: center; padding-top: 3em;">
<h1>&#9989; Authorization complete</h1>
<p>mcp is authorized. You can close this window and return to the terminal.</p>
</body>
</html>
`

// newLoopbackFetcher starts a one-shot HTTP receiver on 127.0.0.1:0 and
// returns an AuthorizationCodeFetcher that waits for the browser redirect on
// it. The redirect URL is http://127.0.0.1:<port>/callback; closeFn shuts the
// receiver down (idempotent, safe to call from a defer).
func newLoopbackFetcher(ctx context.Context, stderr io.Writer) (sdkauth.AuthorizationCodeFetcher, string, func(), error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, "", nil, fmt.Errorf("binding loopback receiver: %w", err)
	}
	redirectURL := fmt.Sprintf("http://127.0.0.1:%d/callback", ln.Addr().(*net.TCPAddr).Port)

	type outcome struct {
		res *sdkauth.AuthorizationResult
		err error
	}
	delivered := make(chan outcome, 1)
	var once sync.Once
	deliver := func(o outcome) { once.Do(func() { delivered <- o }) }

	srv := &http.Server{
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/callback" {
				http.NotFound(w, r)
				return
			}
			q := r.URL.Query()
			if e := q.Get("error"); e != "" {
				deliver(outcome{err: fmt.Errorf("authorization server returned %s: %s", e, q.Get("error_description"))})
				http.Error(w, "authorization failed: "+e, http.StatusBadRequest)
				return
			}
			code := q.Get("code")
			if code == "" {
				deliver(outcome{err: errors.New("authorization callback carried no code")})
				http.Error(w, "authorization failed: no code parameter", http.StatusBadRequest)
				return
			}
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			w.WriteHeader(http.StatusOK)
			fmt.Fprint(w, callbackPage)
			deliver(outcome{res: &sdkauth.AuthorizationResult{
				Code:  code,
				State: q.Get("state"),
				Iss:   q.Get("iss"),
			}})
		}),
		// Handlers observe ctx cancellation (e.g. the command's --timeout).
		BaseContext: func(net.Listener) context.Context { return ctx },
	}
	go srv.Serve(ln) // ErrServerClosed after closeFn is expected and ignored
	closeFn := sync.OnceFunc(func() { _ = srv.Close() })

	return func(fctx context.Context, args *sdkauth.AuthorizationArgs) (*sdkauth.AuthorizationResult, error) {
		authorizeAt(stderr, args.URL)
		select {
		case o := <-delivered:
			return o.res, o.err
		case <-fctx.Done():
			return nil, fmt.Errorf("waiting for authorization: %w", fctx.Err())
		}
	}, redirectURL, closeFn, nil
}

// authorizeAt announces the authorization URL. This is functional output, not
// decoration: the user cannot complete the flow without it, so it is printed
// unconditionally to stderr regardless of the UI mode (JSON mode silences
// decoration only).
func authorizeAt(stderr io.Writer, url string) {
	fmt.Fprintf(stderr, "==> authorize at %s\n", url)
}

// newManualFetcher returns an AuthorizationCodeFetcher for headless use: it
// prints the authorization URL, prompts on stderr, and reads one line (the
// pasted code) from stdin. A closed or empty stdin errors out instead of
// hanging, and so does a context cancellation while waiting for input.
func newManualFetcher(stderr io.Writer, stdin io.Reader) sdkauth.AuthorizationCodeFetcher {
	// One reader for the fetcher's lifetime so buffered input is not lost
	// across invocations.
	br := bufio.NewReader(stdin)
	return func(ctx context.Context, args *sdkauth.AuthorizationArgs) (*sdkauth.AuthorizationResult, error) {
		authorizeAt(stderr, args.URL)
		// The prompt is functional output too: it must sit inline on stderr
		// in every UI mode, or a headless user has nothing to answer.
		fmt.Fprint(stderr, "Enter authorization code: ")

		type line struct {
			text string
			err  error
		}
		lines := make(chan line, 1)
		go func() {
			s, err := br.ReadString('\n')
			lines <- line{text: strings.TrimSpace(s), err: err}
		}()
		var l line
		select {
		case l = <-lines:
		case <-ctx.Done():
			return nil, fmt.Errorf("waiting for authorization code: %w", ctx.Err())
		}
		if l.text == "" {
			if l.err != nil && !errors.Is(l.err, io.EOF) {
				return nil, fmt.Errorf("reading authorization code: %w", l.err)
			}
			return nil, errors.New("no authorization code entered")
		}

		// The SDK rejects a result whose state differs from the one it
		// embedded in the authorization URL, so echo the URL's state back
		// the same way the SDK's own example fetcher does.
		state := ""
		if u, err := url.Parse(args.URL); err == nil {
			state = u.Query().Get("state")
		}
		return &sdkauth.AuthorizationResult{Code: l.text, State: state}, nil
	}
}

// savingTokenSource wraps a token source and persists the token whenever its
// access token changes (i.e. on refresh), keeping mcp's token cache
// restorable for the lifetime of the session.
type savingTokenSource struct {
	wrapped oauth2.TokenSource
	target  string

	mu   sync.Mutex
	last string // access token already persisted
}

func (s *savingTokenSource) Token() (*oauth2.Token, error) {
	tok, err := s.wrapped.Token()
	if err != nil {
		return nil, err
	}
	// The SDK can call Token concurrently (e.g. updateGrantedScopes right
	// after the exchange), so the persisted-token check-and-set is guarded.
	s.mu.Lock()
	defer s.mu.Unlock()
	if tok.AccessToken != s.last {
		if err := SaveToken(s.target, tok); err != nil {
			return nil, fmt.Errorf("persisting refreshed token for %s: %w", s.target, err)
		}
		s.last = tok.AccessToken
	}
	return tok, nil
}
