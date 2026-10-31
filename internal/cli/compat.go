package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"

	"charm.land/lipgloss/v2"
	"charm.land/lipgloss/v2/table"
	"github.com/ConsulTent/mcpcli/internal/connection"
	"github.com/ConsulTent/mcpcli/internal/format"
	"github.com/ConsulTent/mcpcli/internal/trace"
	"github.com/ConsulTent/mcpcli/internal/ui"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/spf13/cobra"
)

// Status values of a compat probe row (the --json wire contract).
const (
	statusAccepted     = "accepted"
	statusRejected     = "rejected"
	statusNotSpeakable = "not-speakeable"
)

// compatRow is one protocol version's probe outcome.
type compatRow struct {
	Version string `json:"version"` // a KnownVersions entry or the draft alias
	Status  string `json:"status"`  // accepted | rejected | not-speakeable
	Detail  string `json:"detail"`  // what the server echoed, or the short error
}

// compatResult is the machine-readable shape of a compat run. Native is the
// version reported by an unpinned connect: what this transport negotiated,
// not necessarily the server's newest supported version — hence the note.
type compatResult struct {
	Native     string      `json:"native"`
	NativeNote string      `json:"native_note"`
	Results    []compatRow `json:"results"`
}

// newCompatCommand builds "compat": probe every protocol version mcp can
// name against the target server, then report the native (negotiated)
// version. Unlike the other session commands it does its own pinned connects
// and ignores auth entirely.
func newCompatCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "compat",
		Short: "Probe which MCP protocol versions the server accepts",
		Long: `Compat probes each MCP protocol version mcp can name (plus "draft") by
opening a minimal connection pinned to that version; rejections are reported
as data, not failures. Notes: auth is ignored, probes are unauthenticated; a
discovery-negotiated version is accepted on advertisement (its ping is
skipped: the SDK Ping omits the _meta such servers validate); legacy sessions
report the negotiated version when the server downgrades. The final line is
the native version from an unpinned connect: what the current transport
negotiated, which may be older than the server's newest supported version.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			f := FlagsFrom(cmd)
			if f == nil {
				return errors.New("global flags unavailable: command must be built by NewRootCommand")
			}
			// Exactly one target, with connection's own messages. Auth is
			// ignored, so no provider is built and no handler is set.
			if err := (&connection.Options{Command: f.Command, URL: f.URL}).Validate(); err != nil {
				return err
			}
			target := f.URL
			if target == "" {
				target = f.Command
			}

			var tracer *trace.Tracer
			if f.Trace || f.Verbose {
				tracer = trace.New(f.Verbose)
			}

			res := compatResult{NativeNote: "negotiated on this transport", Results: []compatRow{}}
			err := ui.RunWithSpinner(cmd.Context(), "probing protocol versions on "+target, func(ctx context.Context) error {
				res.Results = probeVersions(ctx, f, mcp.SupportedProtocolVersions(), tracer)
				native, err := fetchNative(ctx, f, tracer)
				if err != nil {
					return err
				}
				res.Native = native
				return nil
			})
			if err != nil {
				return err
			}
			if tracer != nil {
				if ui.Current == ui.JSON {
					_ = tracer.EmitJSON(cmd.ErrOrStderr())
				} else {
					_ = tracer.Render(cmd.ErrOrStderr(), ui.Current == ui.Plain, f.Verbose)
				}
			}

			out := cmd.OutOrStdout()
			if ui.Current == ui.JSON {
				return format.EmitJSON(out, res)
			}
			return writeCompatReport(out, res)
		},
	}
}

// probeVersions probes every protocol version mcp can name —
// connection.KnownVersions plus the draft alias — against the target: one
// minimal pinned connect plus a ping per version, one report row each, in
// probe order. supported is the linked SDK's version list (parameterized so
// tests can drive the not-speakeable branch): a version outside it is
// reported without any connection attempt.
func probeVersions(ctx context.Context, f *GlobalFlags, supported []string, tracer *trace.Tracer) []compatRow {
	versions := slices.Concat(connection.KnownVersions[:], []string{connection.DraftAlias})
	rows := make([]compatRow, 0, len(versions))
	for _, v := range versions {
		rows = append(rows, probeVersion(ctx, f, v, supported, tracer))
	}
	return rows
}

// probeVersion reports one version's compatibility with the target. A row is
// accepted when the pinned connect and a ping both succeed — or, for a
// discovery-negotiated 2026-07-28 session, when the connect itself succeeded,
// because the server advertised the version and the SDK's Ping cannot
// complete on such sessions (it omits the SEP-2575 _meta they validate).
// Anything else — connect failure, ping failure, or a version the linked SDK
// cannot speak — is data about the endpoint, so only it is recorded, never
// returned as an error.
func probeVersion(ctx context.Context, f *GlobalFlags, v string, supported []string, tracer *trace.Tracer) compatRow {
	row := compatRow{Version: v}
	resolved, err := connection.ResolveVersion(v)
	if err != nil {
		// Unreachable for the values probeVersions feeds in; reported
		// rather than crashed on.
		row.Status = statusRejected
		row.Detail = shortError(err)
		return row
	}
	if !slices.Contains(supported, resolved) {
		row.Status = statusNotSpeakable
		row.Detail = "the linked SDK does not support " + resolved
		return row
	}

	// Each probe gets its own --timeout window, so a slow server delays the
	// whole run by at most one timeout per version.
	pctx, cancel := context.WithTimeout(ctx, f.Timeout)
	defer cancel()

	var detail []string
	if v == connection.DraftAlias {
		detail = append(detail, "resolved to "+resolved)
	}

	session, err := connection.Open(pctx, &connection.Options{
		Command:         f.Command,
		URL:             f.URL,
		ProtocolVersion: v, // Open resolves the alias itself
		Tracer:          tracer,
		Label:           "probe " + v,
	})
	if err != nil {
		row.Status = statusRejected
		row.Detail = strings.Join(append(detail, shortError(err)), "; ")
		return row
	}
	negotiated := ""
	if init := session.InitializeResult(); init != nil {
		negotiated = init.ProtocolVersion
	}

	// The lexicographic date compare is sound: negotiated is SDK-validated —
	// discovery intersects the server's advertised versions with client
	// support (go-sdk client.go:437-450), and initialize results are checked
	// against supportedProtocolVersions (go-sdk client.go:395-398) — so no
	// arbitrary string reaches this comparison.
	if negotiated >= "2026-07-28" {
		// Discovery succeeded: the server explicitly advertised this version.
		// Ping is skipped — the SDK's Ping never injects the SEP-2575
		// _meta.protocolVersion, and servers validating header/meta
		// consistency (like mcp.svelte.dev) reject it with -32020.
		_ = session.Close()
		row.Status = statusAccepted
		_, d := classifyProbe(v, negotiated, nil)
		detail = append(detail, d)
		row.Detail = strings.Join(detail, "; ")
		return row
	}

	pingErr := session.Ping(pctx, nil)
	_ = session.Close() // the verdict is already fixed; a close error cannot change it
	status, d := classifyProbe(v, negotiated, pingErr)
	detail = append(detail, d)
	row.Status = status
	row.Detail = strings.Join(detail, "; ")
	return row
}

// classifyProbe decides one probe row's status and detail from what the
// pinned connect actually negotiated. A discovery-negotiated 2026-07-28
// session is accepted without a ping (the SDK's Ping omits the SEP-2575 _meta
// such servers validate); legacy sessions ping and report honestly when the
// server downgrades the requested version. safeVersion guards every
// server-supplied version before it reaches output.
func classifyProbe(pinned, negotiated string, pingErr error) (status, detail string) {
	if negotiated >= "2026-07-28" {
		return statusAccepted, "server advertises " + safeVersion(negotiated) + " via discovery; ping skipped (SDK Ping omits the SEP-2575 _meta such servers validate)"
	}
	if pingErr != nil {
		return statusRejected, shortError(pingErr)
	}
	if negotiated == "" {
		return statusAccepted, "requested " + safeVersion(pinned) + "; server negotiated (unknown)"
	}
	if negotiated == pinned {
		return statusAccepted, "server echoed " + safeVersion(negotiated)
	}
	return statusAccepted, "requested " + safeVersion(pinned) + "; server negotiated " + safeVersion(negotiated)
}

// safeVersion sanitizes a server-supplied version string for output: only
// [0-9a-zA-Z.-] survives, capped at 32 characters, so terminal escape
// sequences and oversized payloads cannot reach the report.
func safeVersion(s string) string {
	const max = 32
	var b []byte
	for i := 0; i < len(s) && len(b) < max; i++ {
		c := s[i]
		switch {
		case c >= '0' && c <= '9', c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c == '.', c == '-':
			b = append(b, c)
		}
	}
	return string(b)
}

// fetchNative performs the unpinned (auto) connect and returns the protocol
// version the server reported for it. An error here means the endpoint is
// unreachable: the probe could not be completed.
func fetchNative(ctx context.Context, f *GlobalFlags, tracer *trace.Tracer) (string, error) {
	nctx, cancel := context.WithTimeout(ctx, f.Timeout)
	defer cancel()
	session, err := connection.Open(nctx, &connection.Options{
		Command:         f.Command,
		URL:             f.URL,
		ProtocolVersion: "auto", // explicitly unpinned; Open resolves the alias
		Tracer:          tracer,
		Label:           "native",
	})
	if err != nil {
		return "", err
	}
	defer session.Close()
	if init := session.InitializeResult(); init != nil {
		return init.ProtocolVersion, nil
	}
	return "", nil
}

// writeCompatReport renders the probe rows as a table followed by the native
// line. Rich uses the Unicode border; Plain switches to the ASCII border so
// the output stays byte-ASCII.
func writeCompatReport(w io.Writer, res compatResult) error {
	border := lipgloss.NormalBorder()
	if ui.Current == ui.Plain {
		border = lipgloss.ASCIIBorder()
	}
	t := table.New().Border(border).Headers("VERSION", "STATUS", "DETAIL")
	for _, row := range res.Results {
		t.Row(row.Version, statusCell(row.Status), row.Detail)
	}
	if err := format.WriteTable(w, t); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(w, "native (negotiated): %s\n", res.Native); err != nil {
		return fmt.Errorf("write native line: %w", err)
	}
	return nil
}

// statusCell renders a row's status for the table: icons in Rich, ASCII
// labels in Plain (SKIP marks a version this build cannot speak).
func statusCell(status string) string {
	if ui.Current == ui.Plain {
		switch status {
		case statusAccepted:
			return "OK: accepted"
		case statusRejected:
			return "ERROR: rejected"
		default:
			return "SKIP"
		}
	}
	switch status {
	case statusAccepted:
		return ui.IconSuccess + " accepted"
	case statusRejected:
		return ui.IconFail + " rejected"
	default:
		return ui.IconWarn + " not speakeable"
	}
}

// shortError collapses an error to a single bounded line for the detail
// column: its first line, cut to 160 characters.
func shortError(err error) string {
	msg := strings.SplitN(strings.TrimSpace(err.Error()), "\n", 2)[0]
	if r := []rune(msg); len(r) > 160 {
		msg = string(r[:160]) + "..."
	}
	return msg
}
