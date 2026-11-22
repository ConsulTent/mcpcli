package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/ConsulTent/mcpcli/internal/auth"
	"github.com/ConsulTent/mcpcli/internal/format"
	"github.com/ConsulTent/mcpcli/internal/tus"
	"github.com/ConsulTent/mcpcli/internal/ui"
	sdkauth "github.com/modelcontextprotocol/go-sdk/auth"
	"github.com/spf13/cobra"
	"golang.org/x/oauth2"
)

// tusFlags holds the tus group's flags: --api-url is group-persistent (the
// bws twin) — the creation endpoint every upload targets. status and delete
// take the upload's URL as their positional argument instead, so they never
// read it.
type tusFlags struct {
	apiURL string
}

// newTusCommand assembles the "tus" group: upload, status and delete —
// resumable file uploads against one tus 1.0.0 endpoint. None of them opens
// an MCP server session, so no connection flags apply.
func newTusCommand(g *GlobalFlags) *cobra.Command {
	f := &tusFlags{}
	cmd := &cobra.Command{
		Use:   "tus",
		Short: "Upload files and manage staged uploads via tus 1.0.0",
		// Runnable (printing help, exactly what cobra does for a bare group)
		// so the root's PersistentPreRunE runs here too — notably its
		// --interactive check, which must reject "-i tus upload".
		//
		// The parent deliberately declares NO PersistentPreRunE: cobra runs
		// only the closest PersistentPreRunE up the chain, so declaring one
		// here would silence the root's hook — output-mode setup, --auth
		// validation and the argv-secret warning — for every tus verb.
		RunE: func(cmd *cobra.Command, args []string) error { return cmd.Help() },
		Long: `Upload files to a tus 1.0.0 resumable-upload endpoint and manage the
staged uploads: upload streams a file in PATCH chunks (client-side
chunking, resumable), status reports a staged upload's offset (a 404 is
data: nothing is staged there), delete removes one via the termination
extension.

upload targets the creation endpoint named by --api-url; status and
delete take the staged upload's URL as their positional argument. Auth
rides the same global --auth surface as MCP connections — bws://
references in --client-secret expand before anything connects.`,
	}
	cmd.PersistentFlags().StringVar(&f.apiURL, "api-url", "", "tus creation endpoint, e.g. http://localhost:8095/files/")
	cmd.AddCommand(newTusUploadCommand(g, f), newTusStatusCommand(g, f), newTusDeleteCommand(g, f))
	return cmd
}

// tusAuthHandler resolves the auth handler for one tus verb — the
// runWithConnection build sequence minus the connection. Secret references
// expand FIRST (the resolved secret replaces the flag value on the shared
// GlobalFlags), then the auth registry builds the handler for the mode.
// Params.Target is the normalized endpoint: it keys the oauth token cache,
// falls back as the RFC 8707 resource, and scopes basic's credentials — it
// must be the URL the credential actually guards. --auth none builds a nil
// handler (no token injection, no Authorize calls). The caller defers
// cleanup when non-nil.
func tusAuthHandler(cmd *cobra.Command, normEndpoint string) (sdkauth.OAuthHandler, func(), error) {
	g := FlagsFrom(cmd)
	if g == nil {
		return nil, nil, errors.New("global flags unavailable: command must be built by NewRootCommand")
	}
	if _, err := expandSecretRefs(cmd.Context(), g, cmd.ErrOrStderr()); err != nil {
		return nil, nil, err
	}
	provider, err := auth.ProviderFor(g.Auth)
	if err != nil {
		return nil, nil, err
	}
	handler, cleanup, err := provider.Build(cmd.Context(), auth.Params{
		Target:   normEndpoint,
		Creds:    auth.ResolveCredentials(g.ClientID, g.ClientSecret),
		Stderr:   cmd.ErrOrStderr(),
		Stdin:    cmd.InOrStdin(),
		Flow:     g.AuthFlow,
		Resource: g.Resource,
	})
	if err != nil {
		return nil, nil, err
	}
	return handler, cleanup, nil
}

// basicBase extracts basic mode's credential round tripper — the same
// interface assertion connection.go makes on the streamable transport's
// behalf. Only basicHandler implements TransportHTTPClient, and its client
// always carries a non-nil Transport by construction; every other mode
// (and --auth none's nil handler) yields nil, leaving the tus client's
// default transport in place.
func basicBase(handler sdkauth.OAuthHandler) http.RoundTripper {
	if bc, ok := handler.(interface{ TransportHTTPClient() *http.Client }); ok {
		return bc.TransportHTTPClient().Transport
	}
	return nil
}

// tusWarnPlainHTTP emits the one-line plain-http warning for the resolved
// endpoint. Under the plain-http credential gate it covers ONLY the allowed
// case — a literal loopback host with --insecure opted in; every other
// plain-http endpoint with a credential is refused pre-wire by the tus
// client's send() guard (the warning printing before that client error on
// the forbidden path is the accepted double report). Names the concrete URL
// so the actionable knob stays visible.
func tusWarnPlainHTTP(w io.Writer, flag, rawURL string) {
	if strings.HasPrefix(rawURL, "http://") {
		_, _ = fmt.Fprintln(w, "warning: "+flag+" is plain http:// ("+rawURL+"); credentials sent to it travel unencrypted")
	}
}

// parseChunkSize parses a --chunk-size value: bare digits are bytes and an
// optional k/m/g suffix (case-insensitive) multiplies. Unset ("") returns 0
// and non-positive values pass through — tus.Upload's <= 0 rule applies its
// 64 MiB default, one rule with no special case at this layer.
func parseChunkSize(s string) (int64, error) {
	if s == "" {
		return 0, nil
	}
	mult := int64(1)
	body := s
	switch last := s[len(s)-1:]; last {
	case "k", "K":
		mult = 1 << 10
		body = s[:len(s)-1]
	case "m", "M":
		mult = 1 << 20
		body = s[:len(s)-1]
	case "g", "G":
		mult = 1 << 30
		body = s[:len(s)-1]
	}
	n, err := strconv.ParseInt(body, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("invalid argument %q: expected a byte count with an optional k/m/g suffix, e.g. 64m", s)
	}
	if mult > 1 && n > 0 && n > math.MaxInt64/mult {
		return 0, fmt.Errorf("invalid argument %q: chunk size overflows a 64-bit byte count", s)
	}
	return n * mult, nil
}

// parseMetadataPairs parses --metadata KEY=VALUE entries: split on the first
// "=", empty KEY rejected with the args.go house error form. Values are RAW
// strings (the --in-file semantics, never --arg's JSON decoding) — they
// reach the wire base64-encoded by tus's encodeMetadata, and tus validates
// the key charset before any request fires.
func parseMetadataPairs(entries []string) ([][2]string, error) {
	pairs := make([][2]string, 0, len(entries))
	for _, e := range entries {
		k, v, ok := strings.Cut(e, "=")
		if !ok || k == "" {
			return nil, fmt.Errorf("invalid argument %q: expected KEY=VALUE", e)
		}
		pairs = append(pairs, [2]string{k, v})
	}
	return pairs, nil
}

// tusSameHostGuard refuses a --resume URL whose host differs from the
// endpoint's: credentials built for one host must never ride a request to
// another. Both hosts compare case-insensitively with their ports.
func tusSameHostGuard(resumeURL, normEndpoint string) error {
	ru, err := url.Parse(resumeURL)
	if err != nil {
		return fmt.Errorf("tus: invalid --resume %q: %w", resumeURL, err)
	}
	if ru.Scheme == "" || ru.Host == "" {
		return fmt.Errorf("tus: invalid --resume %q: an absolute http(s) URL is required", resumeURL)
	}
	eu, err := url.Parse(normEndpoint)
	if err != nil {
		return err
	}
	if !strings.EqualFold(ru.Host, eu.Host) {
		return fmt.Errorf("tus: --resume host %q does not match --api-url host %q; refusing to send credentials to a different host", ru.Host, eu.Host)
	}
	return nil
}

// tusUploadFlags holds the upload verb's flags.
type tusUploadFlags struct {
	client    string
	filename  string
	metadata  []string
	chunkSize string
	resume    string
	retry     int
}

// tusUploadResult is the machine-readable shape of an upload. Explicit
// snake-free tags; offset equals size on every success path — the server
// holds every byte when Upload returns without an error.
type tusUploadResult struct {
	URL    string `json:"url"`
	Offset int64  `json:"offset"`
	Size   int64  `json:"size"`
}

// newTusUploadCommand builds "tus upload": create (or HEAD-resume), then
// chunked PATCHes, honoring the full global --auth surface.
func newTusUploadCommand(g *GlobalFlags, f *tusFlags) *cobra.Command {
	uf := &tusUploadFlags{}
	cmd := &cobra.Command{
		Use:   "upload <file>",
		Short: "Upload a file to the tus endpoint in resumable chunks",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			errOut := cmd.ErrOrStderr()
			out := cmd.OutOrStdout()

			// Pre-flight (1): the endpoint must normalize before anything
			// else runs — a missing one is its own error, a garbage one
			// surfaces NormalizeEndpoint's message verbatim.
			if f.apiURL == "" {
				return errors.New("tus: --api-url is required for tus upload")
			}
			normEndpoint, err := tus.NormalizeEndpoint(f.apiURL)
			if err != nil {
				return err
			}
			// Pure flag syntax: chunk size and metadata entries fail here,
			// before any auth or file work.
			chunkSize, err := parseChunkSize(uf.chunkSize)
			if err != nil {
				return err
			}
			pairs, err := parseMetadataPairs(uf.metadata)
			if err != nil {
				return err
			}

			// The per-RunE window: tus verbs bypass runWithConnection, so
			// the --timeout applies here (the bws pattern).
			ctx, cancel := context.WithTimeout(cmd.Context(), g.Timeout)
			defer cancel()

			// (2) auth: secret refs expand, then the registry builds the
			// handler for the mode.
			handler, cleanup, err := tusAuthHandler(cmd, normEndpoint)
			if err != nil {
				return err
			}
			if cleanup != nil {
				defer cleanup()
			}

			// (3) the payload: open + stat. The os error propagates bare —
			// the --in-file house form.
			file, err := os.Open(args[0])
			if err != nil {
				return err
			}
			defer func() { _ = file.Close() }()
			info, err := file.Stat()
			if err != nil {
				return err
			}
			size := info.Size()

			// (4) metadata: the sugar fills the initial map, --metadata
			// pairs apply after — same-key override, duplicate keys
			// last-wins.
			metadata := map[string]string{
				"client":   uf.client,
				"filename": uf.filename,
			}
			if uf.filename == "" {
				metadata["filename"] = filepath.Base(args[0])
			}
			for _, p := range pairs {
				metadata[p[0]] = p[1]
			}

			// (5) same-host resume guard: pre-network, naming both hosts.
			if uf.resume != "" {
				if err := tusSameHostGuard(uf.resume, normEndpoint); err != nil {
					return err
				}
			}

			// (6) the client: basic's credential round tripper rides as the
			// base transport (--auth none passes a nil handler and nil
			// base). Warn carries the functional security notes to stderr.
			client := tus.NewClient(normEndpoint, handler, basicBase(handler), g.Insecure)
			client.Warn = errOut

			// (7) Functional security note, not mode-gated decoration: a
			// credential sent to a plain-http endpoint travels unencrypted
			// — the bws twin.
			if g.Auth != "none" {
				tusWarnPlainHTTP(errOut, "--api-url", normEndpoint)
			}

			// (8) --insecure: the discovery fetches inside the SDK auth
			// contract (client-credentials RFC 9728/8414) read the TLS-skip
			// client from the context.
			if g.Insecure {
				ctx = context.WithValue(ctx, oauth2.HTTPClient, insecureHTTPClient())
			}

			// Step lines are success-path only: buffered while the run
			// proceeds, flushed after Upload returns — nothing prints when
			// the error returns, so stdout stays empty on failure in every
			// mode and stderr never carries a step line for a failed run.
			var steps []string
			var lastOffset int64
			if uf.resume != "" {
				// The resume offset for the step line: one HEAD before the
				// orchestration. Best-effort by design — a transient failure
				// here only omits the step line; Upload's own resume HEAD
				// owns the outcome and reports its canonical abort.
				if off, _, exists, herr := client.HeadOffset(ctx, uf.resume); herr == nil && exists {
					steps = append(steps, fmt.Sprintf("resuming at offset %d", off))
				}
			}
			onProgress := func(sent, total int64) {
				lastOffset = sent
				steps = append(steps, fmt.Sprintf("uploaded %d/%d", sent, total))
			}

			// (9) the orchestration: chunkSize <= 0 applies the client's
			// default (one rule); retry <= 0 clamps to no retries.
			url, err := client.Upload(ctx, file, size, metadata, chunkSize, uf.resume, max(uf.retry, 0), onProgress)
			if err != nil {
				// The CLI failure line is the one site that adds context
				// (D3): the file, the wire target once Upload knows it, and
				// the last successfully-sent offset ride the error text in
				// every mode; stdout stays empty.
				if url != "" {
					return fmt.Errorf("tus: uploading %s to %s at offset %d: %w", args[0], url, lastOffset, err)
				}
				return fmt.Errorf("tus: uploading %s at offset %d: %w", args[0], lastOffset, err)
			}
			// Flush the success-path step lines in event order: created (or
			// resuming) first, then one line per PATCH.
			if uf.resume == "" {
				ui.Step(errOut, "created %s", url)
			}
			for _, line := range steps {
				ui.Step(errOut, "%s", line)
			}

			// (10) outputs: --json emits exactly one document; human mode
			// one stdout line.
			if ui.Current == ui.JSON {
				return format.EmitJSON(out, tusUploadResult{URL: url, Offset: size, Size: size})
			}
			_, err = fmt.Fprintf(out, "uploaded %d bytes to %s\n", size, url)
			return err
		},
	}
	cmd.Flags().StringVar(&uf.client, "client", "mcpcli", "value of the `client` upload metadata key")
	cmd.Flags().StringVar(&uf.filename, "filename", "", "override the `filename` upload metadata key (default: the file's base name)")
	// StringArrayVar, not StringSliceVar: the slice type CSV-splits values
	// and would corrupt a comma-bearing literal (the house repeatable-flag
	// precedent).
	cmd.Flags().StringArrayVar(&uf.metadata, "metadata", nil, "extra upload metadata as KEY=VALUE (repeatable; values are raw strings; overrides --client/--filename)")
	cmd.Flags().StringVar(&uf.chunkSize, "chunk-size", "", "PATCH chunk size with optional k/m/g suffix, e.g. 64m (default: 64m; <= 0 keeps the default)")
	cmd.Flags().StringVar(&uf.resume, "resume", "", "resume a staged upload from its URL (must be on the --api-url host)")
	cmd.Flags().IntVar(&uf.retry, "retry", 3, "retry budget for retry-classified PATCH failures")
	return cmd
}

// tusStatusResult is the machine-readable shape of a status. The url field
// echoes the positional AS GIVEN.
type tusStatusResult struct {
	URL    string `json:"url"`
	Exists bool   `json:"exists"`
	Offset int64  `json:"offset"`
	Length int64  `json:"length"`
}

// newTusStatusCommand builds "tus status": one HEAD, with a 404 answered as
// data (exists:false, exit 0) — the compat "rejections are data" precedent.
func newTusStatusCommand(g *GlobalFlags, f *tusFlags) *cobra.Command {
	return &cobra.Command{
		Use:   "status <url>",
		Short: "Report the staged offset of a tus upload",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			errOut := cmd.ErrOrStderr()
			out := cmd.OutOrStdout()
			uploadURL := args[0]

			// Pre-flight: the positional must be an absolute http(s) URL.
			// The upload URL goes to the wire AS GIVEN — NormalizeEndpoint
			// validates the shape; its trailing-slash canonicalization is
			// for directory-style bases, not upload URLs.
			if _, err := tus.NormalizeEndpoint(uploadURL); err != nil {
				return err
			}

			// The per-RunE window (the bws pattern).
			ctx, cancel := context.WithTimeout(cmd.Context(), g.Timeout)
			defer cancel()

			handler, cleanup, err := tusAuthHandler(cmd, uploadURL)
			if err != nil {
				return err
			}
			if cleanup != nil {
				defer cleanup()
			}

			client := tus.NewClient(uploadURL, handler, basicBase(handler), g.Insecure)
			client.Warn = errOut
			if g.Auth != "none" {
				tusWarnPlainHTTP(errOut, "url", uploadURL)
			}
			if g.Insecure {
				ctx = context.WithValue(ctx, oauth2.HTTPClient, insecureHTTPClient())
			}

			offset, length, exists, err := client.HeadOffset(ctx, uploadURL)
			if err != nil {
				return fmt.Errorf("tus: status %s: %w", uploadURL, err)
			}
			if !exists {
				// Data, not failure: a landed, expired or swept upload
				// answers 404 — exit 0 with exists:false.
				if ui.Current == ui.JSON {
					return format.EmitJSON(out, tusStatusResult{URL: uploadURL, Exists: false})
				}
				_, err = fmt.Fprintf(out, "no upload in staging at %s (completed, expired, or deleted)\n", uploadURL)
				return err
			}
			if ui.Current == ui.JSON {
				return format.EmitJSON(out, tusStatusResult{URL: uploadURL, Exists: true, Offset: offset, Length: length})
			}
			_, err = fmt.Fprintf(out, "offset %d of %d (in staging)\n", offset, length)
			return err
		},
	}
}

// tusDeleteResult is the machine-readable shape of a delete.
type tusDeleteResult struct {
	URL     string `json:"url"`
	Deleted bool   `json:"deleted"`
}

// newTusDeleteCommand builds "tus delete": the termination extension, 204 →
// exit 0, 404/410 → exit 1 with the URL in the error.
func newTusDeleteCommand(g *GlobalFlags, f *tusFlags) *cobra.Command {
	return &cobra.Command{
		Use:   "delete <url>",
		Short: "Remove a staged tus upload (termination extension)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			errOut := cmd.ErrOrStderr()
			out := cmd.OutOrStdout()
			uploadURL := args[0]

			// Pre-flight: the positional must be an absolute http(s) URL;
			// the URL goes to the wire AS GIVEN (the status twin).
			if _, err := tus.NormalizeEndpoint(uploadURL); err != nil {
				return err
			}

			// The per-RunE window (the bws pattern).
			ctx, cancel := context.WithTimeout(cmd.Context(), g.Timeout)
			defer cancel()

			handler, cleanup, err := tusAuthHandler(cmd, uploadURL)
			if err != nil {
				return err
			}
			if cleanup != nil {
				defer cleanup()
			}

			client := tus.NewClient(uploadURL, handler, basicBase(handler), g.Insecure)
			client.Warn = errOut
			if g.Auth != "none" {
				tusWarnPlainHTTP(errOut, "url", uploadURL)
			}
			if g.Insecure {
				ctx = context.WithValue(ctx, oauth2.HTTPClient, insecureHTTPClient())
			}

			if err := client.Delete(ctx, uploadURL); err != nil {
				// URL context rides the error text in every mode (the D3
				// single CLI site); errors.Is keeps the sentinel matchable.
				return fmt.Errorf("tus: delete %s: %w", uploadURL, err)
			}
			if ui.Current == ui.JSON {
				return format.EmitJSON(out, tusDeleteResult{URL: uploadURL, Deleted: true})
			}
			_, err = fmt.Fprintf(out, "deleted %s\n", uploadURL)
			return err
		},
	}
}
