package cli

import (
	"context"
	"fmt"
	"io"
	"strings"

	"github.com/ConsulTent/mcpcli/internal/auth"
	"github.com/ConsulTent/mcpcli/internal/bws"
	"github.com/ConsulTent/mcpcli/internal/secret"
)

// newSecretResolver wires the builtin secret sources — the bws source today;
// adding a manager means one new package implementing secret.Source plus one
// registration line here.
func newSecretResolver() (*secret.Resolver, error) {
	return secret.New(bws.NewSource())
}

// expandSecretRefs resolves every secret reference the run consumes BEFORE
// any connection opens, so auth stays secret-free (it never sees a bws://
// ref) and stdio children receive resolved values. One helper, three
// consumers: runWithConnection, runJevCheck's own session open, and
// newAuthLoginCommand.
//
// It expands a reference in the resolved --client-secret (the HTTP bearer
// consumes it, so this happens on every run) and — only when the run spawns
// a stdio child (f.Command != "") — every --env value; on HTTP runs the env
// map would be provably dead, and each ref would cost a live identity POST
// for work nobody consumes. The returned map feeds connection.Options.Env.
//
// The expanded secret replaces the resolved one on the flags: consumers keep
// building auth.Params from auth.ResolveCredentials(f.ClientID,
// f.ClientSecret), which then carries plaintext. Refs resolve once at
// session open — a rotated secret needs a restart.
func expandSecretRefs(ctx context.Context, f *GlobalFlags, errOut io.Writer) (map[string]string, error) {
	resolver, err := newSecretResolver()
	if err != nil {
		return nil, err
	}
	creds := auth.ResolveCredentials(f.ClientID, f.ClientSecret)
	envMap := make(map[string]string, len(f.envBindings))

	// One run = one instance: when any reference is pending, name the
	// instance the values come from BEFORE any of them resolve. A stale
	// MCPCLI_BWS_API_URL would otherwise inject another instance's secret
	// values silently — strictly worse than bws list's metadata — so the
	// note is always-on raw stderr (ui.* would die in --json). Emitted via
	// the cli's bwsInstanceNote (the note does not live in internal/bws).
	clientSecretRef := creds.ClientSecret != "" && resolver.Recognizes(creds.ClientSecret)
	firstEnvRef := ""
	for _, b := range f.envBindings {
		if f.Command != "" && resolver.Recognizes(b.Value) {
			firstEnvRef = "--env " + b.Key
			break
		}
	}
	if clientSecretRef || firstEnvRef != "" {
		cred, err := bws.ResolveCredential("")
		if err != nil {
			// Attribute the failure to the first pending expansion (the
			// client secret, else the first --env binding in order).
			attr := "--client-secret"
			if !clientSecretRef {
				attr = firstEnvRef
			}
			return nil, fmt.Errorf("%s: %w", attr, err)
		}
		bwsInstanceNote(errOut, cred.APIURL)
	}

	// The client secret expands on every run — the HTTP bearer consumes it.
	if creds.ClientSecret != "" {
		value, err := resolver.Expand(ctx, creds.ClientSecret)
		if err != nil {
			return nil, fmt.Errorf("--client-secret: %w", err)
		}
		if err := rejectNUL("--client-secret", value); err != nil {
			return nil, err
		}
		creds.ClientSecret = value
	}

	// The --env map is built for stdio children only.
	if f.Command != "" {
		for _, b := range f.envBindings {
			value, err := resolver.Expand(ctx, b.Value)
			if err != nil {
				return nil, fmt.Errorf("--env %s: %w", b.Key, err)
			}
			if err := rejectNUL("--env "+b.Key, value); err != nil {
				return nil, err
			}
			envMap[b.Key] = value
		}
	}

	// The expanded secret replaces the resolved one on the flags, so each
	// consumer's auth.Params carries plaintext (flag beats env on
	// re-resolution, with the same effective value either way).
	f.ClientSecret = creds.ClientSecret
	return envMap, nil
}

// rejectNUL refuses a value carrying a NUL byte, naming the attribute it
// came from. The guard lives in shared code covering BOTH the --env
// expansions and the ClientSecret expansion: exec environment entries and
// HTTP header values cannot carry NUL, and without it such a secret would
// die later as an unattributable net/http or exec error.
func rejectNUL(attribute, value string) error {
	if strings.IndexByte(value, 0) >= 0 {
		return fmt.Errorf("%s: value contains a NUL byte and cannot be used as a resolved value", attribute)
	}
	return nil
}
