// Package auth provides the mcp auth provider registry and the built-in
// providers: none, bearer, client-credentials, and oauth.
//
// A provider turns flag/environment input into an SDK OAuthHandler. The
// streamable HTTP transport calls the handler's TokenSource to set the
// Authorization header (a nil source means no header), and on a 401/403 it
// calls Authorize and retries the request once when Authorize succeeds.
package auth

import (
	"context"
	"fmt"
	"io"
	"sort"
	"strings"
	"sync"

	sdkauth "github.com/modelcontextprotocol/go-sdk/auth"
)

// Params carries everything a provider may need to build its handler.
//
// Target is the server URL for streamable HTTP (or the stdio command string);
// it doubles as the token-cache key. Creds holds client credentials resolved
// from flags and environment. Stderr/Stdin are the command's streams for
// providers that interact with the user (the OAuth provider added later uses
// them). Flow is the OAuth flow flavor: "loopback" or "manual".
type Params struct {
	Target string
	Creds  Credentials
	Stderr io.Writer
	Stdin  io.Reader
	Flow   string
}

// Provider supplies auth handlers for one --auth mode.
type Provider interface {
	// Name returns the provider's --auth value.
	Name() string

	// Build returns the handler for the transport. A (nil, nil, nil) result
	// means the provider needs no auth handler, so the transport sends no
	// Authorization header. cleanup, when non-nil, releases provider
	// resources when the command finishes; providers with nothing to clean
	// up return nil.
	Build(ctx context.Context, p Params) (handler sdkauth.OAuthHandler, cleanup func(), err error)
}

var (
	registryMu sync.RWMutex
	registry   = map[string]Provider{}

	builtinsOnce sync.Once
)

// Register adds p to the registry. It panics on a nil provider or a duplicate
// name: both are programming errors visible at startup, not runtime states.
func Register(p Provider) {
	if p == nil {
		panic("auth.Register: nil provider")
	}
	registryMu.Lock()
	defer registryMu.Unlock()
	name := p.Name()
	if name == "" {
		panic("auth.Register: provider with empty name")
	}
	if _, dup := registry[name]; dup {
		panic(fmt.Sprintf("auth.Register: duplicate provider %q", name))
	}
	registry[name] = p
}

// ProviderFor looks up a registered provider by name. An unknown name returns
// an error listing the registered provider names.
func ProviderFor(name string) (Provider, error) {
	registryMu.RLock()
	defer registryMu.RUnlock()
	p, ok := registry[name]
	if !ok {
		return nil, fmt.Errorf("unknown auth provider %q (registered: %s)", name, strings.Join(namesLocked(), ", "))
	}
	return p, nil
}

// Names returns all registered provider names, sorted ascending, for help
// text and validation messages.
func Names() []string {
	registryMu.RLock()
	defer registryMu.RUnlock()
	return namesLocked()
}

// namesLocked returns the sorted names; callers must hold registryMu.
func namesLocked() []string {
	names := make([]string, 0, len(registry))
	for name := range registry {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// RegisterBuiltins registers the built-in providers: none, bearer,
// client-credentials and oauth. It is invoked from the cli package's init
// (root.go) so the registry is populated before any flag validation runs; it
// is safe to call repeatedly.
func RegisterBuiltins() {
	builtinsOnce.Do(func() {
		Register(noneProvider{})
		Register(bearerProvider{})
		Register(clientCredentialsProvider{})
		Register(oauthProvider{})
	})
}

// noneProvider implements the "none" mode: no auth handler at all.
type noneProvider struct{}

func (noneProvider) Name() string { return "none" }

func (noneProvider) Build(context.Context, Params) (sdkauth.OAuthHandler, func(), error) {
	return nil, nil, nil
}
