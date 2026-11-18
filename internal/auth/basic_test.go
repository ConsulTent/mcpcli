package auth

import (
	"context"
	"encoding/base64"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
)

// basicProvider must satisfy the Provider interface; its Build signature
// additionally pins *basicHandler to the sdkauth.OAuthHandler contract.
var _ Provider = basicProvider{}

// recordingBody is a response body that flags when Close is called, pinning
// the SDK contract that Authorize owns and closes the 401 response.
type recordingBody struct {
	onClose func()
}

func (b *recordingBody) Read([]byte) (int, error) { return 0, io.EOF }

func (b *recordingBody) Close() error {
	b.onClose()
	return nil
}

// recordingTransport is a stub base RoundTripper: it records the outbound
// request and returns a bare 200 response, so the scoping predicate of
// TransportHTTPClient is observed without a real server on the wire.
type recordingTransport struct {
	got *http.Request
}

func (rt *recordingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	rt.got = req
	return &http.Response{StatusCode: http.StatusOK, Body: http.NoBody}, nil
}

// basicTestCreds returns deliberately-dummy test credentials, constructed
// from inert fragments (the house dummy-fixture pattern — no credential
// literal appears in this file).
func basicTestCreds() Credentials {
	return Credentials{
		ClientID:     strings.Join([]string{"basic", "user", "test", "invalid"}, "."),
		ClientSecret: strings.Join([]string{"dummy", "secret", "value"}, "-"),
	}
}

func TestBasicProviderBuildRequiresCredentials(t *testing.T) {
	const wantCreds = "basic auth requires both credentials: " +
		"pass --client-id and --client-secret or set MCPCLI_CLIENT_ID and MCPCLI_CLIENT_SECRET"

	cases := []struct {
		name  string
		creds Credentials
	}{
		{"missing client id", Credentials{ClientSecret: basicTestCreds().ClientSecret}},
		{"missing client secret", Credentials{ClientID: basicTestCreds().ClientID}},
	}
	for _, tc := range cases {
		h, cleanup, err := basicProvider{}.Build(context.Background(), Params{
			Target: "https://mcp.example.com/mcp",
			Creds:  tc.creds,
		})
		if err == nil {
			if cleanup != nil {
				cleanup()
			}
			t.Errorf("%s: Build = nil error, want the credentials error", tc.name)
			continue
		}
		if h != nil {
			t.Errorf("%s: handler must be nil on error", tc.name)
		}
		if !strings.Contains(err.Error(), wantCreds) {
			t.Errorf("%s: error %q does not contain %q", tc.name, err.Error(), wantCreds)
		}
	}

	// Both credentials present: the positive cell also pins the registry
	// registration (ProviderFor must find the mode).
	p, err := ProviderFor("basic")
	if err != nil {
		t.Fatalf("ProviderFor(basic): %v", err)
	}
	h, cleanup, err := p.Build(context.Background(), Params{
		Target: "https://mcp.example.com/mcp",
		Creds:  basicTestCreds(),
	})
	if err != nil {
		t.Fatalf("basic Build with both credentials: %v", err)
	}
	if cleanup != nil {
		defer cleanup()
	}
	if h == nil {
		t.Fatal("basic Build with both credentials = nil handler, want non-nil")
	}
}

// TestBasicHandlerAuthorizeIsStatic: basic credentials cannot be refreshed, so
// Authorize must return a fixed error — the SDK transport retries only when
// Authorize returns nil, so the error is the no-retry contract — and must
// close the response body it was handed.
func TestBasicHandlerAuthorizeIsStatic(t *testing.T) {
	h, cleanup, err := basicProvider{}.Build(context.Background(), Params{
		Target: "https://mcp.example.com/mcp",
		Creds:  basicTestCreds(),
	})
	if err != nil {
		t.Fatalf("basic Build: %v", err)
	}
	if cleanup != nil {
		defer cleanup()
	}

	closed := false
	resp := &http.Response{
		StatusCode: http.StatusUnauthorized,
		Header:     http.Header{},
		Body:       &recordingBody{onClose: func() { closed = true }},
	}
	err = h.Authorize(context.Background(),
		&http.Request{URL: &url.URL{Scheme: "https", Host: "mcp.example.com"}}, resp)
	if err == nil {
		t.Fatal("basic Authorize must return an error (static credentials cannot be refreshed)")
	}
	const wantErr = "basic auth: the credentials were rejected and cannot be refreshed"
	if err.Error() != wantErr {
		t.Errorf("Authorize error = %q, want %q", err.Error(), wantErr)
	}
	if !closed {
		t.Error("Authorize must close the response body before returning")
	}
}

// TestTransportHTTPClientScopesHeader fires the handler's TransportHTTPClient
// through a recording stub base RoundTripper (no real server): the Basic
// header rides only a request to the Build target's host AND scheme, and a
// pre-existing Authorization header is never clobbered.
func TestTransportHTTPClientScopesHeader(t *testing.T) {
	creds := basicTestCreds()
	user, pass := creds.ClientID, creds.ClientSecret
	wantBasic := "Basic " + base64.StdEncoding.EncodeToString([]byte(user+":"+pass))

	h, cleanup, err := basicProvider{}.Build(context.Background(), Params{
		Target: "https://svc.example.com/mcp",
		Creds:  Credentials{ClientID: user, ClientSecret: pass},
	})
	if err != nil {
		t.Fatalf("basic Build: %v", err)
	}
	if cleanup != nil {
		defer cleanup()
	}
	bh, ok := h.(*basicHandler)
	if !ok {
		t.Fatalf("Build returned %T, want *basicHandler", h)
	}

	cases := []struct {
		name     string
		hop      string
		preAuth  string // Authorization header on the outgoing request, "" for none
		wantAuth string // header the stub must record, "" for none
	}{
		{
			name:     "target host and scheme carries the basic header",
			hop:      "https://svc.example.com/mcp",
			wantAuth: wantBasic,
		},
		{
			name: "foreign host carries no header",
			hop:  "https://other.example.com/path",
		},
		{
			name: "same host with downgraded scheme carries no header",
			hop:  "http://svc.example.com/mcp",
		},
		{
			name:     "pre-set authorization header is not clobbered",
			hop:      "https://svc.example.com/mcp",
			preAuth:  "Bearer something",
			wantAuth: "Bearer something",
		},
	}
	for _, tc := range cases {
		rt := &recordingTransport{}
		client := bh.TransportHTTPClient()
		// Swap the stub in as the wrapper's base RoundTripper — replacing
		// client.Transport wholesale would bypass the credential injection.
		client.Transport.(*basicRoundTripper).base = rt

		req, err := http.NewRequest(http.MethodPost, tc.hop, nil)
		if err != nil {
			t.Fatalf("%s: NewRequest: %v", tc.name, err)
		}
		if tc.preAuth != "" {
			req.Header.Set("Authorization", tc.preAuth)
		}
		resp, err := client.Do(req)
		if err != nil {
			t.Fatalf("%s: client.Do: %v", tc.name, err)
		}
		_ = resp.Body.Close()
		if rt.got == nil {
			t.Fatalf("%s: stub recorded no request", tc.name)
		}
		got := rt.got.Header.Get("Authorization")
		if got != tc.wantAuth {
			t.Errorf("%s: recorded Authorization = %q, want %q", tc.name, got, tc.wantAuth)
		}
	}
}
