package auth

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	sdkauth "github.com/modelcontextprotocol/go-sdk/auth"
	"golang.org/x/oauth2"
)

// bearerProvider implements the "bearer" mode: a static token supplied via
// --client-secret / MCPCLI_CLIENT_SECRET, sent as a Bearer Authorization
// header. No discovery, no network at build time.
type bearerProvider struct{}

func (bearerProvider) Name() string { return "bearer" }

func (bearerProvider) Build(_ context.Context, p Params) (sdkauth.OAuthHandler, func(), error) {
	if p.Creds.ClientSecret == "" {
		return nil, nil, fmt.Errorf(
			"bearer auth requires a token: pass --client-secret or set MCPCLI_CLIENT_SECRET")
	}
	// StaticTokenSource never refreshes; tokens that can expire belong to
	// the client-credentials or OAuth providers.
	src := oauth2.StaticTokenSource(&oauth2.Token{
		AccessToken: p.Creds.ClientSecret,
		TokenType:   "Bearer",
	})
	return &bearerHandler{src: src}, nil, nil
}

type bearerHandler struct {
	src oauth2.TokenSource
}

func (h *bearerHandler) TokenSource(context.Context) (oauth2.TokenSource, error) {
	return h.src, nil
}

// Authorize always fails: a static token cannot be refreshed, so a 401/403
// must surface instead of retrying with the same doomed token.
func (h *bearerHandler) Authorize(context.Context, *http.Request, *http.Response) error {
	return errors.New("bearer auth: the static token was rejected and cannot be refreshed; " +
		"supply a fresh token via --client-secret or MCPCLI_CLIENT_SECRET")
}
