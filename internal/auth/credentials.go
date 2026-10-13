package auth

import "os"

// Credentials holds OAuth client credentials resolved from flags and
// environment. For the bearer provider ClientSecret carries the static
// bearer token (mcp reuses --client-secret for it).
type Credentials struct {
	ClientID     string
	ClientSecret string
}

// ResolveCredentials resolves client credentials from flag values with
// environment fallback: a non-empty flag wins over the corresponding
// MCPCLI_CLIENT_ID / MCPCLI_CLIENT_SECRET variable. Fields resolve
// independently, and missing values are left empty so that each provider can
// report a specific, actionable error instead of a generic one.
func ResolveCredentials(idFlag, secretFlag string) Credentials {
	id := idFlag
	if id == "" {
		id = os.Getenv("MCPCLI_CLIENT_ID")
	}
	secret := secretFlag
	if secret == "" {
		secret = os.Getenv("MCPCLI_CLIENT_SECRET")
	}
	return Credentials{ClientID: id, ClientSecret: secret}
}
