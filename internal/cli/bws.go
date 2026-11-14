package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"charm.land/lipgloss/v2"
	"charm.land/lipgloss/v2/table"
	"github.com/ConsulTent/mcpcli/internal/bws"
	"github.com/ConsulTent/mcpcli/internal/format"
	"github.com/ConsulTent/mcpcli/internal/ui"
	"github.com/spf13/cobra"
)

// bwsFlags holds the bws group's flags: --api-url is group-persistent (one
// instance selector shared by every verb), the rest are login-local — the
// runtime instance and org id ride the cached credential.
type bwsFlags struct {
	apiURL      string
	accessToken string
	orgID       string
	identityURL string
}

// newBWSCommand assembles the "bws" group: login, logout, list and get —
// cache management and secret reads against one Bitwarden Secrets Manager
// instance. None of them is an MCP server session, so no connection flags
// apply.
func newBWSCommand(g *GlobalFlags) *cobra.Command {
	f := &bwsFlags{}
	cmd := &cobra.Command{
		Use:   "bws",
		Short: "Manage Bitwarden Secrets Manager credentials and secrets",
		// Runnable (printing help, exactly what cobra does for a bare group)
		// so the root's PersistentPreRunE runs here too — notably its
		// --interactive check, which must reject "-i bws login".
		//
		// The parent deliberately declares NO PersistentPreRunE: cobra runs
		// only the closest PersistentPreRunE up the chain, so declaring one
		// here would silence the root's hook — output-mode setup, --auth
		// validation and the argv-secret warning — for every bws verb.
		RunE: func(cmd *cobra.Command, args []string) error { return cmd.Help() },
		Long: `Manage Bitwarden Secrets Manager (BWS) credentials and secrets.

login stores a machine-account access token for one Bitwarden instance in
mcp's cache, validating it with a live exchange against the identity
server. logout deletes a cached instance login without touching the
network. list shows the organization's secrets as decrypted-name metadata
(never values). get prints secret values, one KEY=value line per ref —
values go to stdout by design, so pipe or redirect them deliberately.

Every verb targets one Bitwarden instance: pass --api-url, set
MCPCLI_BWS_API_URL, or (for list/get) rely on the single cached login.
Refs are bare secret uuids or names; the bws:// form belongs to --env
values and --client-secret, not to these verbs.`,
	}
	cmd.PersistentFlags().StringVar(&f.apiURL, "api-url", "", "Bitwarden Secrets Manager API base URL, e.g. https://api.bitwarden.com")
	cmd.AddCommand(newBWSLoginCommand(g, f), newBWSLogoutCommand(f), newBWSListCommand(g, f), newBWSGetCommand(g, f))
	return cmd
}

// bwsLoginResult is the machine-readable shape of a login. Explicit snake
// tags; org_id stays PRESENT-but-empty when no org was given (constant key
// set).
type bwsLoginResult struct {
	OK             bool   `json:"ok"`
	APIURL         string `json:"api_url"`
	IdentityURL    string `json:"identity_url"`
	OrgID          string `json:"org_id"`
	MachineAccount string `json:"machine_account"`
}

// newBWSLoginCommand builds "bws login": validate a machine-account token by
// performing the live exchange against the identity server, then upsert the
// credential (token, org id, both instance URLs) into the cache keyed by the
// normalized API URL — the same discipline as the OAuth2 token cache.
func newBWSLoginCommand(g *GlobalFlags, f *bwsFlags) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "login",
		Short: "Store a Bitwarden machine-account login in the cache",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			errOut := cmd.ErrOrStderr()

			// The token comes from the flag or the environment — the flag
			// wins. A flag-sourced token warns (see below); an env-sourced
			// one is silent.
			token := f.accessToken
			if token != "" {
				// Functional security note, not mode-gated decoration (the
				// root --client-secret precedent): a secret on the command
				// line is visible in shell history and process listings, so
				// the flag says so on stderr in every mode — JSON included.
				// Leaf-local flags warn at the leaf; this is the third
				// warning variant (its tail names the BWS env).
				_, _ = fmt.Fprintln(errOut, "warning: --access-token was passed on the command line; prefer the MCPCLI_BWS_ACCESS_TOKEN environment variable")
			} else if env, ok := os.LookupEnv("MCPCLI_BWS_ACCESS_TOKEN"); ok {
				token = env
			}
			if token == "" {
				return errors.New("bws login needs an access token: pass --access-token or set MCPCLI_BWS_ACCESS_TOKEN")
			}

			// The two defaults are the feature's only URL defaults, applied
			// BEFORE normalization: an empty slot means default here, never
			// invalid.
			apiURL := f.apiURL
			if apiURL == "" {
				apiURL = "https://api.bitwarden.com"
			}
			identityURL := f.identityURL
			if identityURL == "" {
				identityURL = "https://identity.bitwarden.com"
			}
			normAPI, err := bws.NormalizeURL(apiURL)
			if err != nil {
				return fmt.Errorf("invalid --api-url: %w", err)
			}
			normIdentity, err := bws.NormalizeURL(identityURL)
			if err != nil {
				return fmt.Errorf("invalid --identity-url: %w", err)
			}
			bwsWarnPlainHTTP(errOut, "--api-url", normAPI)
			bwsWarnPlainHTTP(errOut, "--identity-url", normIdentity)

			orgID := f.orgID
			if orgID != "" && !bws.IsUUIDRef(orgID) {
				return fmt.Errorf("invalid --org %q: not a uuid", orgID)
			}
			// shortcut: the org id is stored unvalidated against the API —
			// there is no well-defined probe; a wrong id surfaces at the
			// first org-scoped call (twin of the --api-url shortcut below).

			// The per-RunE window: bws verbs bypass runWithConnection, so
			// the --timeout applies here (the auth-login pattern).
			ctx, cancel := context.WithTimeout(cmd.Context(), g.Timeout)
			defer cancel()

			// Validation = one live exchange: it proves the token works and
			// its response carries the org key material. It touches the
			// identity server only.
			client := bws.NewClient(normAPI, normIdentity, token, orgID)
			if _, err := client.Exchange(ctx); err != nil {
				// The 401 sentinel (errors.Is, not a string match on the
				// unioned message — its text is golden-pinned). At login the
				// actionable knob is the exchange target being validated, so
				// the runtime re-login pointer gets the --identity-url tail.
				if errors.Is(err, bws.ErrUnauthorized) {
					return fmt.Errorf("%w\nbws login: check --identity-url (%s): it must be the identity server of the same instance as --api-url", err, normIdentity)
				}
				return fmt.Errorf("bws login: validating the token: %w", err)
			}

			// shortcut: --api-url is stored unvalidated against the API — no
			// well-defined probe exists; a wrong base surfaces at the first
			// org-scoped call.
			if err := bws.SaveCredential(bws.Credential{
				APIToken:    token,
				OrgID:       orgID,
				APIURL:      normAPI,
				IdentityURL: normIdentity,
			}); err != nil {
				return err
			}

			// Display only: the parsed api-key-id names the machine account.
			id, _, _, err := bws.ParseAccessToken(token)
			if err != nil {
				// Unreachable: Exchange above already gated on a well-formed
				// token. Reported rather than crashed on.
				return err
			}
			orgState := "org " + orgID
			if orgID == "" {
				// The line enumerates the WHOLE degraded set: an empty OrgID
				// disables both name refs and bws list (both fail closed
				// pre-URL), and the at-login report must say so or the
				// degraded credential stops being visible at login.
				if ui.Current == ui.Plain {
					orgState = "no org - name refs and bws list disabled"
				} else {
					orgState = "no org — name refs and bws list disabled"
				}
			}
			ui.Step(errOut, "machine account %s", id)
			ui.Step(errOut, "api %s", normAPI)
			ui.Step(errOut, "identity %s", normIdentity)
			ui.Step(errOut, "%s", orgState)
			ui.Success(errOut, "logged in to %s", normAPI)

			if ui.Current == ui.JSON {
				return format.EmitJSON(cmd.OutOrStdout(), bwsLoginResult{
					OK:             true,
					APIURL:         normAPI,
					IdentityURL:    normIdentity,
					OrgID:          orgID,
					MachineAccount: id,
				})
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&f.accessToken, "access-token", "", "machine-account access token (0.<id>.<secret>:<seed>); prefer MCPCLI_BWS_ACCESS_TOKEN")
	cmd.Flags().StringVar(&f.orgID, "org", "", "organization id (uuid) enabling secret-name refs")
	cmd.Flags().StringVar(&f.identityURL, "identity-url", "", "Bitwarden identity base URL (default: https://identity.bitwarden.com)")
	return cmd
}

// bwsWarnPlainHTTP emits the one-line plain-http warning for a resolved
// instance URL. A warning only: the no-blocklist ruling keeps self-hosted
// and loopback http instances working.
func bwsWarnPlainHTTP(w io.Writer, flag, rawURL string) {
	if strings.HasPrefix(rawURL, "http://") {
		_, _ = fmt.Fprintln(w, "warning: "+flag+" is plain http:// ("+rawURL+"); the access token and secrets travel unencrypted")
	}
}

// newBWSLogoutCommand builds "bws logout": delete one cached instance login.
// Strictly offline — the cache is keyed by API URL, so no connection is made
// and none is needed. The truth table rides internal/bws's exported surface
// only: CachedInstances enumerates (env-free, sorted) and DeleteCredential
// removes; no cache-file detail reaches this package. MCPCLI_BWS_API_URL is
// ignored here: the env selects credentials for the reading verbs, it is not
// a cache target.
func newBWSLogoutCommand(f *bwsFlags) *cobra.Command {
	return &cobra.Command{
		Use:   "logout",
		Short: "Delete a cached Bitwarden instance login",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			errOut := cmd.ErrOrStderr()

			urls, err := bws.CachedInstances()
			if err != nil {
				return err
			}
			if len(urls) == 0 {
				// Idempotent: forgetting nothing is a successful logout.
				ui.Success(errOut, "logged out (no Bitwarden instance was cached)")
				return nil
			}
			if flag := f.apiURL; flag != "" {
				// Membership probe through the exported resolver: the flag
				// beats MCPCLI_BWS_API_URL there, and its miss error names
				// the cached instances — fail-closed, never a silent no-op
				// delete.
				cred, err := bws.ResolveCredential(flag)
				if err != nil {
					return err
				}
				if _, err := bws.DeleteCredential(cred.APIURL); err != nil {
					return err
				}
				ui.Success(errOut, "logged out of %s", cred.APIURL)
				return nil
			}
			if len(urls) > 1 {
				return fmt.Errorf("bws logout: %d Bitwarden instances are cached (%s); pass --api-url to choose the instance to forget", len(urls), strings.Join(urls, ", "))
			}
			if _, err := bws.DeleteCredential(urls[0]); err != nil {
				return err
			}
			ui.Success(errOut, "logged out of %s", urls[0])
			return nil
		},
	}
}

// bwsSecretMeta is one list row's machine-readable shape: the EXACT field
// set pinned by the design (no extras), explicit snake tags.
type bwsSecretMeta struct {
	ID           string    `json:"id"`
	Key          string    `json:"key"`
	ProjectIDs   []string  `json:"project_ids"`
	Read         bool      `json:"read"`
	Write        bool      `json:"write"`
	RevisionDate time.Time `json:"revision_date"`
}

// bwsListResult is the machine-readable shape of a list.
type bwsListResult struct {
	APIURL  string          `json:"api_url"`
	OrgID   string          `json:"org_id"`
	Secrets []bwsSecretMeta `json:"secrets"`
}

// newBWSListCommand builds "bws list": the org's secrets as decrypted-name
// metadata. Values never reach this output — a read of the secret CONTENT is
// bws get's job.
func newBWSListCommand(g *GlobalFlags, f *bwsFlags) *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List the organization's secrets (metadata only)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			errOut := cmd.ErrOrStderr()

			cred, err := bws.ResolveCredential(f.apiURL)
			if err != nil {
				return err
			}
			bwsInstanceNote(errOut, cred.APIURL)

			ctx, cancel := context.WithTimeout(cmd.Context(), g.Timeout)
			defer cancel()

			client := bws.NewClient(cred.APIURL, cred.IdentityURL, cred.APIToken, cred.OrgID)
			secrets, err := client.ListSecrets(ctx)
			if err != nil {
				return err
			}

			if ui.Current == ui.JSON {
				res := bwsListResult{
					APIURL:  cred.APIURL,
					OrgID:   cred.OrgID,
					Secrets: make([]bwsSecretMeta, 0, len(secrets)),
				}
				for _, s := range secrets {
					res.Secrets = append(res.Secrets, bwsSecretMeta{
						ID:           s.ID,
						Key:          s.Key,
						ProjectIDs:   bwsNotEmpty(bwsProjectIDs(s.Projects)),
						Read:         s.CanRead,
						Write:        s.CanWrite,
						RevisionDate: s.RevisionDate,
					})
				}
				return format.EmitJSON(cmd.OutOrStdout(), res)
			}
			return bwsWriteSecretTable(cmd.OutOrStdout(), secrets)
		},
	}
}

// bwsWriteSecretTable renders the metadata table: the house compat.go table
// conventions — uppercase headers, the Unicode border in Rich and the ASCII
// border in Plain, grant columns as icons in Rich and ASCII labels in Plain
// (the statusCell pattern). The project column shows ids (the decoded
// projects also carry decrypted names; the pinned table stays id-only).
func bwsWriteSecretTable(w io.Writer, secrets []bws.Secret) error {
	border := lipgloss.NormalBorder()
	if ui.Current == ui.Plain {
		border = lipgloss.ASCIIBorder()
	}
	t := table.New().Border(border).Headers("KEY", "READ", "WRITE", "PROJECTS")
	for _, s := range secrets {
		// The decrypted name is server-supplied: sanitize at the render call
		// (the --json twin keeps the raw name — data delivery).
		t.Row(sanitizeNote(s.Key), bwsGrantCell(s.CanRead), bwsGrantCell(s.CanWrite), strings.Join(bwsProjectIDs(s.Projects), ", "))
	}
	return format.WriteTable(w, t)
}

// bwsGrantCell renders a read/write grant for the table: icons in Rich,
// ASCII labels in Plain — the statusCell pattern.
func bwsGrantCell(ok bool) string {
	if ui.Current == ui.Plain {
		if ok {
			return "yes"
		}
		return "no"
	}
	if ok {
		return ui.IconSuccess
	}
	return ui.IconWarn
}

// bwsProjectIDs extracts the project ids from a decoded secret's projects
// (the live wire carries {id, name} objects; outputs pin ids only).
func bwsProjectIDs(projects []bws.Project) []string {
	ids := make([]string, 0, len(projects))
	for _, p := range projects {
		ids = append(ids, p.ID)
	}
	return ids
}

// bwsNotEmpty normalizes a nil project list to the empty slice so the JSON
// document always carries a real array.
func bwsNotEmpty(ids []string) []string {
	if ids == nil {
		return []string{}
	}
	return ids
}

// bwsInstanceNote is the always-on instance note for the verbs that read
// secrets: a raw Fprintln so it survives --json (ui.* would be silenced) —
// the root.go security-note precedent. Without it a stale --api-url or env
// selection would silently read another instance's data.
func bwsInstanceNote(w io.Writer, apiURL string) {
	_, _ = fmt.Fprintln(w, "bws: using instance "+apiURL)
}

// bwsSecretFull is one fetched secret's machine-readable shape: the EXACT
// field set pinned by the design (no extras), explicit snake tags. The value
// and note are decrypted plaintext — this struct exists only on the bws get
// stdout path.
type bwsSecretFull struct {
	ID           string    `json:"id"`
	Key          string    `json:"key"`
	Value        string    `json:"value"`
	Note         string    `json:"note"`
	ProjectIDs   []string  `json:"project_ids"`
	Read         bool      `json:"read"`
	Write        bool      `json:"write"`
	RevisionDate time.Time `json:"revision_date"`
}

// bwsGetResult is the machine-readable shape of a get.
type bwsGetResult struct {
	Secrets []bwsSecretFull `json:"secrets"`
}

// newBWSGetCommand builds "bws get": fetch one or more secrets by bare ref
// (uuid or name) and print them. Human mode writes one KEY=value line per
// ref (KEY is the decrypted secret name, directly consumable as an --env
// LHS); --json writes one {secrets:[...]} document in ref order.
func newBWSGetCommand(g *GlobalFlags, f *bwsFlags) *cobra.Command {
	return &cobra.Command{
		Use:   "get <ref>...",
		Short: "Print secret values for bare refs (KEY=value)",
		Args:  cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			errOut := cmd.ErrOrStderr()
			out := cmd.OutOrStdout()

			// Ref-shape validation precedes credential resolution: a
			// bws://-shaped ref is the --env/--client-secret grammar and
			// fails closed before any cache lookup or network. The scheme
			// segment matches in ANY letter case (EqualFold, the Resolver's
			// dispatch rule): "BWS://x" expands on the --env side, so it
			// must get the same bare-ref remediation.
			for _, ref := range args {
				if scheme, bare, found := strings.Cut(ref, "://"); found && strings.EqualFold(scheme, "bws") {
					return fmt.Errorf("bws: secret %q: pass the bare ref %q; the bws:// form belongs to --env values and --client-secret, not to the bws verbs", ref, bare)
				}
			}

			cred, err := bws.ResolveCredential(f.apiURL)
			if err != nil {
				return err
			}
			bwsInstanceNote(errOut, cred.APIURL)

			ctx, cancel := context.WithTimeout(cmd.Context(), g.Timeout)
			defer cancel()

			client := bws.NewClient(cred.APIURL, cred.IdentityURL, cred.APIToken, cred.OrgID)

			// shortcut: each NAME ref re-lists the org (GetSecretByRef's
			// name branch), so the loop is pinned at 1-2 refs per run and
			// aborts on the first failure — continuing past a failure would
			// multiply the per-ref cost for work nobody asked for.
			resolved := make([]bwsSecretFull, 0, len(args))
			for _, ref := range args {
				sec, err := client.GetSecretByRef(ctx, ref)
				if err != nil {
					// The third stdout-on-failure class (the jev-check
					// report precedent): refs resolved before the failure
					// still emit their partial document before the error.
					if ui.Current == ui.JSON && len(resolved) > 0 {
						_ = format.EmitJSON(out, bwsGetResult{Secrets: resolved})
					}
					return err
				}
				if ui.Current == ui.JSON {
					resolved = append(resolved, bwsSecretFull{
						ID:           sec.ID,
						Key:          sec.Key,
						Value:        sec.Value,
						Note:         sec.Note,
						ProjectIDs:   bwsNotEmpty(bwsProjectIDs(sec.Projects)),
						Read:         sec.CanRead,
						Write:        sec.CanWrite,
						RevisionDate: sec.RevisionDate,
					})
				} else if _, err := fmt.Fprintf(out, "%s=%s\n", sec.Key, sec.Value); err != nil {
					return err
				}
			}
			if ui.Current == ui.JSON {
				return format.EmitJSON(out, bwsGetResult{Secrets: resolved})
			}
			return nil
		},
	}
}
