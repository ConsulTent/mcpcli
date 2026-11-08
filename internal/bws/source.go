package bws

import (
	"context"

	"github.com/ConsulTent/mcpcli/internal/secret"
)

// Source resolves bws:// references against a Bitwarden Secrets Manager
// instance. It is the first secret.Source implementation: the Resolver
// hands it whatever follows "bws://" (or "BWS://" — the scheme is
// lowercased before dispatch) and this type owns the ref grammar.
type Source struct {
	// client is the per-run lazy Client, constructed on the first Resolve
	// and reused so the access-token exchange is paid once per run (the
	// exchange memo lives on the Client and spans Resolve calls).
	// single-goroutine per run — no sync.Once.
	client *Client
}

// Compile-time check that Source implements secret.Source.
var _ secret.Source = (*Source)(nil)

// NewSource returns a Source with no options.
func NewSource() *Source {
	return &Source{}
}

// Scheme returns the scheme this source claims.
func (s *Source) Scheme() string { return "bws" }

// Resolve resolves ref (a secret uuid or name) to its decrypted value. The
// credential is loaded lazily via ResolveCredential("") — the expansion
// path's empty-flag call form — and the Client is constructed once for the
// run. Fail-closed throughout: an unresolvable credential, a CanRead=false
// grant or an empty value is an error naming the ref.
func (s *Source) Resolve(ctx context.Context, ref string) (string, error) {
	if s.client == nil {
		cred, err := ResolveCredential("")
		if err != nil {
			return "", err
		}
		s.client = NewClient(cred.APIURL, cred.IdentityURL, cred.APIToken, cred.OrgID)
	}
	sec, err := s.client.GetSecretByRef(ctx, ref)
	if err != nil {
		return "", err
	}
	return sec.Value, nil
}
