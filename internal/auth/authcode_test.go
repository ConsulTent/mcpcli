package auth

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/ConsulTent/mcpcli/internal/ui"
	sdkauth "github.com/modelcontextprotocol/go-sdk/auth"
)

// fetchOutcome carries one AuthorizationCodeFetcher invocation's result across
// a goroutine boundary in tests.
type fetchOutcome struct {
	res *sdkauth.AuthorizationResult
	err error
}

// runFetcher invokes fetcher with the given args and a bounded context,
// failing the test if it does not settle within fetchTimeout (the manual and
// loopback flows must never hang).
func runFetcher(t *testing.T, fetcher sdkauth.AuthorizationCodeFetcher, args *sdkauth.AuthorizationArgs) fetchOutcome {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), fetchTimeout)
	defer cancel()
	ch := make(chan fetchOutcome, 1)
	go func() {
		res, err := fetcher(ctx, args)
		ch <- fetchOutcome{res: res, err: err}
	}()
	select {
	case o := <-ch:
		return o
	case <-time.After(fetchTimeout):
		t.Fatal("authorization fetcher did not return; a flow hung")
		return fetchOutcome{}
	}
}

// fetchTimeout bounds every fetcher invocation in these tests: a flow that
// waits longer than this is considered hung, not slow.
const fetchTimeout = 10 * time.Second

func TestPickRegistration(t *testing.T) {
	const redirect = "http://127.0.0.1:9/callback"

	// Client ID set: pre-registered, with the secret attached when present.
	prereg, dcr := pickRegistration(&Credentials{ClientID: "test-client-id", ClientSecret: "dummy-secret"}, redirect)
	if prereg == nil || dcr != nil {
		t.Fatalf("pickRegistration(creds) = (%v, %v), want preregistered only", prereg, dcr)
	}
	if prereg.ClientID != "test-client-id" {
		t.Errorf("PreregisteredClient.ClientID = %q, want %q", prereg.ClientID, "test-client-id")
	}
	if prereg.ClientSecretAuth == nil || prereg.ClientSecretAuth.ClientSecret != "dummy-secret" {
		t.Errorf("PreregisteredClient.ClientSecretAuth = %+v, want the provided secret", prereg.ClientSecretAuth)
	}

	// Client ID without a secret: a public client, no ClientSecretAuth.
	prereg, dcr = pickRegistration(&Credentials{ClientID: "test-client-id"}, redirect)
	if prereg == nil || dcr != nil {
		t.Fatalf("pickRegistration(public creds) = (%v, %v), want preregistered only", prereg, dcr)
	}
	if prereg.ClientSecretAuth != nil {
		t.Errorf("ClientSecretAuth = %+v, want nil when no secret is set", prereg.ClientSecretAuth)
	}

	// No client ID: dynamic client registration naming mcp and the redirect.
	prereg, dcr = pickRegistration(&Credentials{}, redirect)
	if dcr == nil || prereg != nil {
		t.Fatalf("pickRegistration(no creds) = (%v, %v), want dynamic registration only", prereg, dcr)
	}
	if dcr.Metadata == nil {
		t.Fatal("DynamicClientRegistrationConfig.Metadata is nil")
	}
	if dcr.Metadata.ClientName != "mcp" {
		t.Errorf("Metadata.ClientName = %q, want %q", dcr.Metadata.ClientName, "mcp")
	}
	if len(dcr.Metadata.RedirectURIs) != 1 || dcr.Metadata.RedirectURIs[0] != redirect {
		t.Errorf("Metadata.RedirectURIs = %v, want [%q]", dcr.Metadata.RedirectURIs, redirect)
	}
	// RequestRefreshToken is set, so DCR must advertise the refresh_token
	// grant alongside authorization_code: many servers only issue refresh
	// tokens to clients advertising it.
	want := []string{"authorization_code", "refresh_token"}
	if !slices.Equal(dcr.Metadata.GrantTypes, want) {
		t.Errorf("Metadata.GrantTypes = %v, want %v", dcr.Metadata.GrantTypes, want)
	}
}

func TestLoopbackReceiverDeliversCode(t *testing.T) {
	var stderr bytes.Buffer
	fetcher, redirectURL, closeFn, err := newLoopbackFetcher(context.Background(), &stderr)
	if err != nil {
		t.Fatalf("newLoopbackFetcher: %v", err)
	}
	defer closeFn()
	if !strings.HasPrefix(redirectURL, "http://127.0.0.1:") || !strings.HasSuffix(redirectURL, "/callback") {
		t.Fatalf("redirectURL = %q, want http://127.0.0.1:<port>/callback", redirectURL)
	}
	if closeFn == nil {
		t.Fatal("closeFn is nil, want a receiver shutdown function")
	}

	ch := make(chan fetchOutcome, 1)
	ctx, cancel := context.WithTimeout(context.Background(), fetchTimeout)
	defer cancel()
	go func() {
		res, err := fetcher(ctx, &sdkauth.AuthorizationArgs{URL: "https://idp.example/authorize?state=xyz"})
		ch <- fetchOutcome{res: res, err: err}
	}()

	// The listener is bound before newLoopbackFetcher returns, so the
	// browser's redirect can arrive at any time from here on.
	const iss = "https://idp.example"
	resp, err := http.Get(redirectURL + "?code=c&state=s&iss=" + iss)
	if err != nil {
		t.Fatalf("callback GET: %v", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("reading callback response: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Errorf("callback status = %d, want 200", resp.StatusCode)
	}
	if !strings.Contains(string(body), "Authorization complete") {
		t.Errorf("callback page does not confirm completion: %q", string(body))
	}

	select {
	case o := <-ch:
		if o.err != nil {
			t.Fatalf("fetcher error: %v", o.err)
		}
		if o.res.Code != "c" {
			t.Errorf("Code = %q, want %q", o.res.Code, "c")
		}
		if o.res.State != "s" {
			t.Errorf("State = %q, want %q", o.res.State, "s")
		}
		if o.res.Iss != iss {
			t.Errorf("Iss = %q, want %q", o.res.Iss, iss)
		}
	case <-time.After(fetchTimeout):
		t.Fatal("fetcher did not observe the callback")
	}
	if !strings.Contains(stderr.String(), "authorize at") {
		t.Errorf("stderr %q does not announce the authorization URL", stderr.String())
	}

	closeFn() // must be idempotent when the deferred call runs again
}

func TestManualFetcherReadsCode(t *testing.T) {
	var stderr bytes.Buffer
	const authURL = "https://idp.example/authorize?client_id=test-client-id&state=st9"
	fetcher := newManualFetcher(&stderr, strings.NewReader("abc\n"))
	o := runFetcher(t, fetcher, &sdkauth.AuthorizationArgs{URL: authURL})
	if o.err != nil {
		t.Fatalf("fetcher error: %v", o.err)
	}
	if o.res.Code != "abc" {
		t.Errorf("Code = %q, want %q", o.res.Code, "abc")
	}
	// The SDK validates the returned state against the one it embedded in
	// the authorization URL, so the fetcher must echo it back.
	if o.res.State != "st9" {
		t.Errorf("State = %q, want the state echoed from the authorization URL (%q)", o.res.State, "st9")
	}
	out := stderr.String()
	if !strings.Contains(out, "==> authorize at "+authURL+"\n") {
		t.Errorf("stderr %q does not announce the authorization URL verbatim", out)
	}
	if !strings.Contains(out, "Enter authorization code:") {
		t.Errorf("stderr %q does not prompt for the code", out)
	}

	// A final line without a trailing newline is still a code.
	fetcher = newManualFetcher(io.Discard, strings.NewReader("abc"))
	o = runFetcher(t, fetcher, &sdkauth.AuthorizationArgs{URL: "https://idp.example/authorize?state=x"})
	if o.err != nil || o.res.Code != "abc" {
		t.Errorf("EOF-terminated input: outcome = %+v, want code %q with no error", o, "abc")
	}
}

// TestAuthorizeAtPrintedInJSONMode pins the review fix: the authorization URL
// is functional output, so it must reach stderr even in JSON mode, where
// ui.Step-style decoration is silenced. Before the fix this test hung the
// real flow with no visible URL until --timeout.
func TestAuthorizeAtPrintedInJSONMode(t *testing.T) {
	old := ui.Current
	ui.Current = ui.JSON
	t.Cleanup(func() { ui.Current = old })

	const authURL = "https://idp.example/authorize?state=st9"

	// Manual fetcher: URL line and inline prompt must both appear.
	var stderr bytes.Buffer
	o := runFetcher(t, newManualFetcher(&stderr, strings.NewReader("abc\n")), &sdkauth.AuthorizationArgs{URL: authURL})
	if o.err != nil {
		t.Fatalf("fetcher error: %v", o.err)
	}
	for _, want := range []string{"==> authorize at " + authURL + "\n", "Enter authorization code:"} {
		if !strings.Contains(stderr.String(), want) {
			t.Errorf("JSON-mode manual stderr %q is missing %q", stderr.String(), want)
		}
	}

	// Loopback fetcher: the URL line appears as soon as the flow starts.
	var loopbackErr bytes.Buffer
	fetcher, _, closeFn, err := newLoopbackFetcher(context.Background(), &loopbackErr)
	if err != nil {
		t.Fatalf("newLoopbackFetcher: %v", err)
	}
	defer closeFn()
	fctx, fcancel := context.WithCancel(context.Background())
	defer fcancel()
	started := make(chan struct{})
	go func() {
		close(started)
		_, _ = fetcher(fctx, &sdkauth.AuthorizationArgs{URL: authURL})
	}()
	<-started // the URL is printed before the fetcher waits
	deadline := time.Now().Add(fetchTimeout)
	for time.Now().Before(deadline) && !strings.Contains(loopbackErr.String(), "==> authorize at "+authURL+"\n") {
		time.Sleep(time.Millisecond)
	}
	if !strings.Contains(loopbackErr.String(), "==> authorize at "+authURL+"\n") {
		t.Errorf("JSON-mode loopback stderr %q is missing the authorization URL line", loopbackErr.String())
	}
}

func TestManualFetcherEOF(t *testing.T) {
	for _, input := range []io.Reader{strings.NewReader(""), strings.NewReader("  \n")} {
		fetcher := newManualFetcher(io.Discard, input)
		o := runFetcher(t, fetcher, &sdkauth.AuthorizationArgs{URL: "https://idp.example/authorize?state=x"})
		if o.err == nil {
			t.Errorf("fetcher with stdin %q returned %+v, want an error", input, o.res)
		}
	}
}

func TestOAuthBuildFlows(t *testing.T) {
	prov, err := ProviderFor("oauth")
	if err != nil {
		t.Fatalf("oauth provider is not registered: %v", err)
	}

	// Loopback: live handler plus a closable receiver.
	var stderr bytes.Buffer
	h, cleanup, err := prov.Build(context.Background(), Params{
		Target: "https://example.test/mcp",
		Stderr: &stderr,
		Flow:   "loopback",
	})
	if err != nil {
		t.Fatalf("loopback Build: %v", err)
	}
	if h == nil {
		t.Error("loopback Build returned a nil handler")
	}
	if cleanup == nil {
		t.Error("loopback Build returned a nil cleanup")
	}
	cleanup()

	// Manual: handler plus a callable no-op cleanup.
	h, cleanup, err = prov.Build(context.Background(), Params{
		Target: "https://example.test/mcp",
		Stderr: &stderr,
		Stdin:  strings.NewReader(""),
		Flow:   "manual",
	})
	if err != nil {
		t.Fatalf("manual Build: %v", err)
	}
	if h == nil {
		t.Error("manual Build returned a nil handler")
	}
	if cleanup == nil {
		t.Error("manual Build returned a nil cleanup, want a no-op")
	}
	cleanup()

	// Unknown flows are rejected with both valid names in the message.
	if _, _, err := prov.Build(context.Background(), Params{Target: "https://example.test/mcp", Flow: "bogus"}); err == nil {
		t.Error("Build with Flow \"bogus\" returned nil error")
	} else if !strings.Contains(err.Error(), "loopback") || !strings.Contains(err.Error(), "manual") {
		t.Errorf("Build error %v does not name the valid flows", err)
	}

	// The manual flow reads stdin, so a missing reader is an error, not a panic.
	if _, _, err := prov.Build(context.Background(), Params{Target: "https://example.test/mcp", Flow: "manual"}); err == nil {
		t.Error("manual Build without Stdin returned nil error")
	}
}
