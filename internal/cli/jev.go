package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"slices"
	"strings"

	"charm.land/lipgloss/v2"
	"charm.land/lipgloss/v2/table"
	"github.com/ConsulTent/mcpcli/internal/auth"
	"github.com/ConsulTent/mcpcli/internal/connection"
	"github.com/ConsulTent/mcpcli/internal/format"
	"github.com/ConsulTent/mcpcli/internal/jev"
	"github.com/ConsulTent/mcpcli/internal/ui"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/spf13/cobra"
)

// newJevCommand builds the "jev" group: typed judgment questions against a
// Jev MCP server, flavor-agnostically. The canonical primitives are TypeSafe's
// (noul/choice/score); adapters map them onto whichever of the known flavors
// the server speaks.
func newJevCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "jev",
		Short: "Ask Jev judgment questions (noul, choice, score, verify, screen, gate)",
		// Runnable (printing help, exactly what cobra does for a bare group)
		// so the root's PersistentPreRunE runs here too — notably its
		// --interactive check, which must reject "-i jev" instead of
		// silently printing help.
		RunE: func(cmd *cobra.Command, args []string) error { return cmd.Help() },
	}
	cmd.AddCommand(newJevAskCommand(), newJevScreenCommand(), newJevVerifyCommand(), newJevGateCommand(), newJevCheckCommand())
	return cmd
}

// newJevAskCommand builds "jev ask QUESTION": one typed question whose
// primitive is inferred from the flags — --opt asks a choice, --levels a
// score, anything else a noul. Prefix the question with "--" when it begins
// with a dash (jev ask -- -why?).
func newJevAskCommand() *cobra.Command {
	var (
		opts      []string
		levelsRaw string
		contextS  string
		evidence  []string
		prioriti  string
		threshold float64
	)
	cmd := &cobra.Command{
		Use:   "ask QUESTION",
		Short: "Ask one typed judgment question",
		Long: `Ask one typed judgment question; the primitive is inferred from the flags:
--opt asks a choice (repeatable, ordered), --levels a score (comma-separated
ordered levels), and neither asks a noul (the probability the question is
true). Prefix the question with "--" when it begins with a dash.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			flags := cmd.Flags()
			// Flag-shape checks first, before anything that reads input.
			if flags.Changed("opt") && flags.Changed("levels") {
				return errors.New("--levels and --opt are mutually exclusive")
			}
			question := strings.TrimSpace(args[0])
			if question == "" {
				return errors.New("ask requires a non-empty QUESTION")
			}
			opts, err := cleanLabels(opts)
			if err != nil {
				return err
			}
			var levels []string
			if levelsRaw != "" {
				levels, err = splitLevels(levelsRaw)
				if err != nil {
					return err
				}
			}
			if flags.Changed("threshold") && math.IsNaN(threshold) {
				return fmt.Errorf("invalid --threshold %v: expected a number between 0 and 1", threshold)
			}

			op := jev.OpKindNoul
			if len(opts) > 0 {
				op = jev.OpKindChoice
			} else if len(levels) > 0 {
				op = jev.OpKindScore
			}
			q := jev.Question{
				Text:       question,
				Context:    contextS,
				Evidence:   evidence,
				Priorities: prioriti,
				Options:    opts,
				Levels:     levels,
			}
			if flags.Changed("threshold") {
				if threshold < 0 || threshold > 1 {
					return fmt.Errorf("invalid --threshold %v: expected a number between 0 and 1", threshold)
				}
				q.Threshold, q.HasThreshold = threshold, true
			}
			return runWithConnection(cmd.Context(), FlagsFrom(cmd), func(ctx context.Context, session *mcp.ClientSession) error {
				return runJevOp(ctx, session, cmd, op, q, askSetFlags(cmd))
			})
		},
	}
	cmd.Flags().StringArrayVar(&opts, "opt", nil, "choice option label (repeatable, ordered; at least 2)")
	cmd.Flags().StringVar(&levelsRaw, "levels", "", "score levels as comma-separated ordered descriptions (e.g. \"routine,incident\")")
	cmd.Flags().StringVar(&contextS, "context", "", "shared context or state for the question")
	cmd.Flags().StringArrayVar(&evidence, "evidence", nil, "evidence text (repeatable)")
	cmd.Flags().StringVar(&prioriti, "priorities", "", "priorities for a choice (jkudish flavor)")
	cmd.Flags().Float64Var(&threshold, "threshold", 0, "confidence/auto-accept threshold between 0 and 1")
	return cmd
}

// newJevScreenCommand builds "jev screen TEXT": prompt-injection screening of
// one text (jkudish flavor). Advisory only — the recommendation never grants
// anything.
func newJevScreenCommand() *cobra.Command {
	var purpose string
	cmd := &cobra.Command{
		Use:   "screen TEXT",
		Short: "Screen one text for prompt injection",
		Long: `Screen one text for prompt injection (jkudish flavor only). The result is
advisory: an action of pass, review, block or skip, never a permission.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			text := strings.TrimSpace(args[0])
			if text == "" {
				return errors.New("screen requires a non-empty TEXT")
			}
			return runWithConnection(cmd.Context(), FlagsFrom(cmd), func(ctx context.Context, session *mcp.ClientSession) error {
				return runJevOp(ctx, session, cmd, jev.OpKindScreen, jev.Question{Text: text, Purpose: purpose}, nil)
			})
		},
	}
	cmd.Flags().StringVar(&purpose, "purpose", "", "what the text will be used for")
	return cmd
}

// newJevVerifyCommand builds "jev verify": claims checked against evidence
// (jkudish flavor), one verdict per claim.
func newJevVerifyCommand() *cobra.Command {
	var (
		claims    []string
		evidence  []string
		threshold float64
	)
	cmd := &cobra.Command{
		Use:   "verify",
		Short: "Verify claims against evidence",
		Long: `Verify claims against evidence (jkudish flavor only). Each --claim is
checked against every --evidence text; the result carries one verdict per
claim (verified, contradicted, unsupported or unknown) with its confidence.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(claims) == 0 {
				return errors.New("verify requires at least one --claim")
			}
			if len(evidence) == 0 {
				return errors.New("verify requires at least one --evidence")
			}
			q := jev.Question{Claims: claims, Evidence: evidence}
			if cmd.Flags().Changed("threshold") {
				if threshold < 0 || threshold > 1 || math.IsNaN(threshold) {
					return fmt.Errorf("invalid --threshold %v: expected a number between 0 and 1", threshold)
				}
				q.Threshold, q.HasThreshold = threshold, true
			}
			return runWithConnection(cmd.Context(), FlagsFrom(cmd), func(ctx context.Context, session *mcp.ClientSession) error {
				return runJevOp(ctx, session, cmd, jev.OpKindVerify, q, nil)
			})
		},
	}
	cmd.Flags().StringArrayVar(&claims, "claim", nil, "claim text (repeatable, at least one)")
	cmd.Flags().StringArrayVar(&evidence, "evidence", nil, "evidence text (repeatable, at least one)")
	cmd.Flags().Float64Var(&threshold, "threshold", 0, "auto-accept threshold between 0 and 1")
	return cmd
}

// newJevGateCommand builds "jev gate NAME": the jev-use PreToolUse check on
// one tool call. --input is inline JSON or "-" for stdin; the gate can deny
// or ask, never grant — its decision is advisory output.
func newJevGateCommand() *cobra.Command {
	var (
		input string
		state string
	)
	cmd := &cobra.Command{
		Use:   "gate NAME",
		Short: "Run the PreToolUse risk check on one tool call",
		Long: `Run the jev-use PreToolUse risk check on one tool call: NAME is the tool,
--input its arguments as inline JSON ("-" reads stdin), --state optional
surrounding state. The gate can deny or escalate (ask), never grant: treat
the decision as advice in your own permission flow.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			name := strings.TrimSpace(args[0])
			if name == "" {
				return errors.New("gate requires a non-empty NAME")
			}
			if strings.TrimSpace(input) == "" {
				return errors.New("gate requires --input")
			}
			// Parse before connecting, so malformed input fails fast — and
			// only this flag may read stdin.
			gateInput, err := parseGateInput(input)
			if err != nil {
				return err
			}
			return runWithConnection(cmd.Context(), FlagsFrom(cmd), func(ctx context.Context, session *mcp.ClientSession) error {
				return runJevOp(ctx, session, cmd, jev.OpKindGate, jev.Question{Tool: name, Input: gateInput, State: state}, nil)
			})
		},
	}
	cmd.Flags().StringVar(&input, "input", "", `tool arguments as inline JSON ("-" for stdin)`)
	cmd.Flags().StringVar(&state, "state", "", "surrounding state for the check")
	return cmd
}

// Battery row statuses (the --json wire contract; the summary counts share
// these names, with "errors" as the count noun). authSkipMode/authFailMode
// are the --probe-on-auth-error values.
const (
	probeOK       = "ok"
	probeEscalate = "escalate"
	probeSkipped  = "skipped"
	probeAuthSkip = "auth_skipped"
	probeError    = "error"
	authSkipMode  = "skip"
	authFailMode  = "fail"
)

// newJevCheckCommand builds "jev check": detect the flavor from the tool
// inventory and, with --probe, run the self-test battery — one minimal
// read-only call per available tool. Auth errors from a dead provider key
// fail the run by default; --probe-on-auth-error skip aborts the battery,
// marks the rest skipped, and exits 0 (the CI posture: an unpaid bill is not
// a red build). Unlike compat, check honors --auth for its connection.
func newJevCheckCommand() *cobra.Command {
	var (
		probe  bool
		onAuth string
	)
	cmd := &cobra.Command{
		Use:   "check",
		Short: "Detect the Jev flavor and self-test it",
		Long: `Detect which Jev flavor the server speaks from its tool inventory and
print the support matrix (which question primitives map to which tools).
With --probe, run the self-test battery: one minimal read-only call per
available tool, each under its own --timeout window. Escalated and
fail-closed verdicts are data (exit 0); a tool error fails (exit 1) unless
--probe-on-auth-error skip is set, in which case the first provider
auth/billing error aborts the battery, marks the remaining probes skipped,
warns on stderr, and exits 0. The auth classification is a text heuristic:
only recognized auth-class errors are skipped, anything else still fails.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			f := FlagsFrom(cmd)
			if f == nil {
				return errors.New("global flags unavailable: command must be built by NewRootCommand")
			}
			if onAuth != authSkipMode && onAuth != authFailMode {
				return fmt.Errorf("invalid --probe-on-auth-error %q: expected skip or fail", onAuth)
			}
			if cmd.Flags().Changed("probe-on-auth-error") && !probe {
				return errors.New("--probe-on-auth-error only applies to jev check --probe")
			}
			if err := (&connection.Options{Command: f.Command, URL: f.URL}).Validate(); err != nil {
				return err
			}
			target := f.URL
			if target == "" {
				target = f.Command
			}

			var res checkResult
			err := ui.RunWithSpinner(cmd.Context(), "checking Jev on "+target, func(ctx context.Context) error {
				var workErr error
				res, workErr = runJevCheck(ctx, f, probe, onAuth == authSkipMode, cmd)
				return workErr
			})
			if err != nil && res.Flavor == "" && res.Error == "" {
				// Connect or list failure: nothing to report beyond the error.
				return err
			}
			// Doc-before-exit: the report always reaches stdout, whatever the
			// battery found — the --json consumer's `tee | jq` never sees an
			// empty document.
			out := cmd.OutOrStdout()
			if ui.Current == ui.JSON {
				if err := format.EmitJSON(out, res); err != nil {
					return err
				}
			} else if err := writeJevCheckReport(out, res); err != nil {
				return err
			}
			if res.errAfterReport != "" {
				return errors.New(res.errAfterReport)
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&probe, "probe", false, "run the self-test battery (one minimal call per available tool)")
	cmd.Flags().StringVar(&onAuth, "probe-on-auth-error", authFailMode, `on a provider auth/billing error: "fail" (default) or "skip"`)
	return cmd
}

// jevResultJSON is the one-document --json shape of ask/verify/screen/gate:
// the canonical verdicts regardless of flavor, with an error field only when
// the tool itself failed (the document still prints, then the command exits
// on format.ErrToolError).
type jevResultJSON struct {
	Flavor    string        `json:"flavor"`
	Tool      string        `json:"tool"`
	Verdicts  []jev.Verdict `json:"verdicts"`
	Escalated bool          `json:"escalated,omitempty"`
	Error     string        `json:"error,omitempty"`
}

// opRowJSON is one support-matrix row: how (or how not) a primitive maps.
type opRowJSON struct {
	Op   string `json:"op"`
	Via  string `json:"via,omitempty"`
	OK   bool   `json:"ok"`
	Note string `json:"note,omitempty"`
}

// probeRowJSON is one battery row. Status is one of
// ok|escalate|skipped|auth_skipped|error.
type probeRowJSON struct {
	Tool   string `json:"tool"`
	Status string `json:"status"`
	Detail string `json:"detail,omitempty"`
}

// checkSummary counts the battery rows; the five counts always sum to the
// number of rows (asserted in tests and by the CI lane's jq).
type checkSummary struct {
	OK          int `json:"ok"`
	Escalate    int `json:"escalate"`
	Skipped     int `json:"skipped"`
	AuthSkipped int `json:"auth_skipped"`
	Errors      int `json:"errors"`
}

// checkResult is the --json shape of jev check. Flavor is empty when no
// known flavor was detected; Error then carries the reason and the unmatched
// jev_* tools are still listed.
type checkResult struct {
	Flavor         string         `json:"flavor"`
	Matched        []string       `json:"matched,omitempty"`
	Unmatched      []string       `json:"unmatched,omitempty"`
	Ops            []opRowJSON    `json:"ops"`
	Probes         []probeRowJSON `json:"probes,omitempty"`
	Summary        *checkSummary  `json:"summary,omitempty"`
	Error          string         `json:"error,omitempty"`
	errAfterReport string         // returned as the command error after the report prints
}

// runJevOp runs one canonical op end to end inside an open session: detect
// the flavor, map and build the call, warn about flags the mapping drops,
// execute, and render. An isError result still prints the document first,
// then fails with format.ErrToolError — the tools call semantics.
func runJevOp(ctx context.Context, session *mcp.ClientSession, cmd *cobra.Command, op jev.OpKind, q jev.Question, setFlags map[string]bool) error {
	out, errOut := cmd.OutOrStdout(), cmd.ErrOrStderr()
	adapter, toolNames, err := detectJev(ctx, session)
	if err != nil {
		return err
	}
	if adapter == nil {
		return fmt.Errorf("no known Jev flavor detected (jev tools present: %s)", strings.Join(toolNames, ", "))
	}
	for _, warning := range droppedFlagWarnings(adapter.Name(), op, setFlags) {
		_, _ = fmt.Fprintln(errOut, warning)
	}
	sup := adapter.Supports(op)
	if !sup.Supported {
		return fmt.Errorf("unsupported op %q on the %s flavor: %s", op, adapter.Name(), sanitizeNote(sup.Note))
	}
	tool, args, err := adapter.Build(op, q)
	if err != nil {
		return err
	}
	if !slices.Contains(toolNames, tool) {
		return fmt.Errorf("tool %s is not exposed by this server", tool)
	}
	res, err := session.CallTool(ctx, &mcp.CallToolParams{Name: tool, Arguments: args})
	if err != nil {
		return fmt.Errorf("call tool %s: %w", tool, err)
	}
	doc := jevResultJSON{Flavor: adapter.Name(), Tool: tool, Verdicts: []jev.Verdict{}}
	if res.IsError {
		doc.Error = resultErrorText(res)
		if ui.Current == ui.JSON {
			if err := format.EmitJSON(out, doc); err != nil {
				return err
			}
		} else {
			_, _ = fmt.Fprintln(errOut, doc.Error)
		}
		return format.ErrToolError
	}
	verdicts, err := adapter.Parse(op, res)
	if err != nil {
		return err
	}
	doc.Verdicts = verdicts
	for _, v := range verdicts {
		if v.Escalate {
			doc.Escalated = true
		}
	}
	if ui.Current == ui.JSON {
		return format.EmitJSON(out, doc)
	}
	return format.JevVerdicts(out, verdicts, ui.Current == ui.Plain)
}

// detectJev lists the server's tools and resolves the Jev adapter. It
// returns every jev_* tool name present — the caller's no-flavor error names
// them so the user can see what the server does expose.
func detectJev(ctx context.Context, session *mcp.ClientSession) (jev.Adapter, []string, error) {
	res, err := session.ListTools(ctx, nil)
	if err != nil {
		return nil, nil, fmt.Errorf("list tools: %w", err)
	}
	adapter, _, _ := jev.AdapterFor(res.Tools)
	var names []string
	for _, t := range res.Tools {
		if t != nil && strings.HasPrefix(t.Name, "jev_") {
			names = append(names, t.Name)
		}
	}
	return adapter, names, nil
}

// askSetFlags records which ask flags the user actually set, so the mapping
// can warn about the ones it drops — one stderr line per flag, in every
// output mode (the --client-secret precedent).
func askSetFlags(cmd *cobra.Command) map[string]bool {
	set := map[string]bool{}
	for _, name := range []string{"evidence", "priorities", "context", "threshold"} {
		if cmd.Flags().Changed(name) {
			set["--"+name] = true
		}
	}
	return set
}

// droppedByFlavorOp lists the ask flags each mapping cannot carry.
var droppedByFlavorOp = map[string]map[jev.OpKind][]string{
	"jkudish": {
		jev.OpKindNoul:   {"--evidence", "--priorities"},
		jev.OpKindChoice: {"--context", "--threshold"},
	},
	"jev-use": {
		jev.OpKindNoul:   {"--evidence", "--priorities"},
		jev.OpKindChoice: {"--evidence", "--priorities"},
		jev.OpKindScore:  {"--evidence", "--priorities"},
	},
}

// droppedFlagWarnings names every user-set flag the mapping drops.
func droppedFlagWarnings(flavor string, op jev.OpKind, set map[string]bool) []string {
	var warnings []string
	for _, flag := range droppedByFlavorOp[flavor][op] {
		if set[flag] {
			warnings = append(warnings, fmt.Sprintf("warning: %s is not carried by the %s %s mapping; ignoring", flag, flavor, op))
		}
	}
	return warnings
}

// parseGateInput decodes gate --input: inline JSON, or stdin via "-".
// encoding/json accepts null into a map without error, so the nil check is
// load-bearing — a null input is a usage error, not an empty argument set.
func parseGateInput(input string) (map[string]any, error) {
	data := []byte(input)
	if input == "-" {
		d, err := readArgsInput(input)
		if err != nil {
			return nil, err
		}
		data = d
	}
	var args map[string]any
	if err := json.Unmarshal(data, &args); err != nil {
		return nil, fmt.Errorf("gate --input: expected a JSON object: %w", err)
	}
	if args == nil {
		return nil, errors.New("gate --input: expected a JSON object")
	}
	return args, nil
}

// cleanLabels validates --opt entries: trimmed, non-empty, no duplicates.
func cleanLabels(opts []string) ([]string, error) {
	seen := make(map[string]bool, len(opts))
	out := make([]string, 0, len(opts))
	for _, raw := range opts {
		label := strings.TrimSpace(raw)
		if label == "" {
			return nil, fmt.Errorf("invalid --opt %q: expected a non-empty label", raw)
		}
		if seen[label] {
			return nil, fmt.Errorf("invalid --opt %q: duplicate label", raw)
		}
		seen[label] = true
		out = append(out, label)
	}
	return out, nil
}

// splitLevels splits --levels on commas, trimming each segment; any empty
// segment (including a trailing comma) is an error.
func splitLevels(raw string) ([]string, error) {
	parts := strings.Split(raw, ",")
	for _, p := range parts {
		if strings.TrimSpace(p) == "" {
			return nil, fmt.Errorf("invalid --levels %q: segments must be non-empty", raw)
		}
	}
	for i := range parts {
		parts[i] = strings.TrimSpace(parts[i])
	}
	return parts, nil
}

// resultErrorText extracts an isError result's text (its first text block),
// bounded to one line.
func resultErrorText(res *mcp.CallToolResult) string {
	for _, c := range res.Content {
		if tc, ok := c.(*mcp.TextContent); ok {
			return shortError(errors.New(tc.Text))
		}
	}
	return "tool returned an error"
}

// sanitizeNote renders a server-supplied note for an error message:
// printable ASCII only, whitespace collapsed, capped like shortError —
// terminal escapes and oversized payloads cannot reach the message.
func sanitizeNote(note string) string {
	var b strings.Builder
	space := false
	for _, r := range note {
		switch {
		case r >= 0x20 && r <= 0x7e:
			b.WriteByte(byte(r))
			space = false
		default:
			if !space && b.Len() > 0 {
				b.WriteByte(' ')
				space = true
			}
		}
		if b.Len() >= 160 {
			return strings.TrimSpace(b.String()[:160]) + "..."
		}
	}
	return strings.TrimSpace(b.String())
}

// runJevCheck performs the whole check: open one session (honoring --auth,
// unlike compat), detect the flavor, build the report, and with probe run
// the battery. Each probe owns its own --timeout window carved from ctx; the
// connect/list phase shares one window.
func runJevCheck(ctx context.Context, f *GlobalFlags, probe, skipOnAuth bool, cmd *cobra.Command) (checkResult, error) {
	res := checkResult{Ops: []opRowJSON{}}

	// The connect phase gets one --timeout window, like runWithConnection.
	cctx, cancel := context.WithTimeout(ctx, f.Timeout)
	defer cancel()
	provider, err := auth.ProviderFor(f.Auth)
	if err != nil {
		return res, err
	}
	target := f.URL
	if target == "" {
		target = f.Command
	}
	handler, cleanup, err := provider.Build(cctx, auth.Params{
		Target: target,
		Creds:  auth.ResolveCredentials(f.ClientID, f.ClientSecret),
		Stderr: os.Stderr,
		Stdin:  os.Stdin,
		Flow:   f.AuthFlow,
	})
	if err != nil {
		return res, err
	}
	if cleanup != nil {
		defer cleanup()
	}
	session, err := connection.Open(cctx, &connection.Options{
		Command:         f.Command,
		URL:             f.URL,
		Auth:            f.Auth,
		ProtocolVersion: f.Protocol,
		AuthHandler:     handler,
	})
	if err != nil {
		return res, err
	}
	defer func() { _ = session.Close() }()

	tools, err := session.ListTools(cctx, nil)
	if err != nil {
		return res, fmt.Errorf("list tools: %w", err)
	}
	adapter, matched, unmatched := jev.AdapterFor(tools.Tools)
	res.Matched, res.Unmatched = matched, unmatched
	if adapter == nil {
		res.Error = "no known Jev flavor detected"
		res.errAfterReport = res.Error
		return res, errors.New(res.Error)
	}
	res.Flavor = adapter.Name()

	for _, op := range []jev.OpKind{jev.OpKindNoul, jev.OpKindChoice, jev.OpKindScore, jev.OpKindVerify, jev.OpKindScreen, jev.OpKindGate} {
		row := opRowJSON{Op: op.String()}
		sup := adapter.Supports(op)
		if sup.Supported {
			row.OK = true
			row.Via = sup.Via
			if note := schemaDriftNote(tools.Tools, adapter, op); note != "" {
				row.Note = note
			}
		} else {
			row.Note = sanitizeNote(sup.Note)
		}
		res.Ops = append(res.Ops, row)
	}

	if !probe {
		return res, nil
	}

	// The battery: the flavor's fixtures plus one skip row per unmatched
	// jev_* tool (unknown to the adapter — reachable via tools call).
	probes := adapter.Probes()
	for _, name := range unmatched {
		probes = append(probes, jev.Probe{Tool: name, SkipReason: "not a canonical Jev tool; call it directly via tools call"})
	}
	rows := make([]probeRowJSON, 0, len(probes))
	summary := checkSummary{}
	aborted, abortDetail := false, ""
	for _, p := range probes {
		switch {
		case aborted:
			rows = append(rows, probeRowJSON{Tool: p.Tool, Status: probeSkipped, Detail: "not run: battery aborted on " + abortDetail})
			summary.Skipped++
		case p.SkipReason != "":
			rows = append(rows, probeRowJSON{Tool: p.Tool, Status: probeSkipped, Detail: p.SkipReason})
			summary.Skipped++
		default:
			row, abort, authAbort := runJevProbe(ctx, session, f, adapter, p, skipOnAuth)
			rows = append(rows, row)
			countRow(&summary, row)
			if abort {
				aborted = true
				if authAbort {
					abortDetail = "auth-class error"
					if skipOnAuth {
						_, _ = fmt.Fprintln(cmd.ErrOrStderr(), "warning: provider key rejected (auth/billing) — probes skipped")
					}
				} else {
					abortDetail = "error"
				}
			}
		}
	}
	res.Probes = rows
	res.Summary = &summary
	if summary.Errors > 0 {
		res.errAfterReport = fmt.Sprintf("jev check: %d of %d probes failed; see the report", summary.Errors, len(rows))
	}
	return res, nil
}

// countRow folds one row into the summary. The five counts must always sum
// to the number of rows — the invariant tests and the CI lane's jq assert.
func countRow(s *checkSummary, row probeRowJSON) {
	switch row.Status {
	case probeOK:
		s.OK++
	case probeEscalate:
		s.Escalate++
	case probeSkipped:
		s.Skipped++
	case probeAuthSkip:
		s.AuthSkipped++
	default:
		s.Errors++
	}
}

// runJevProbe executes one battery probe under its own --timeout window and
// returns its row plus whether the battery aborts. An auth-class error is
// auth_skipped in skip mode (data, exit 0 downstream) and a plain error in
// fail mode; every other error aborts with exit 1 either way.
func runJevProbe(ctx context.Context, session *mcp.ClientSession, f *GlobalFlags, adapter jev.Adapter, p jev.Probe, skipOnAuth bool) (row probeRowJSON, abort, authAbort bool) {
	row = probeRowJSON{Tool: p.Tool}
	pctx, cancel := context.WithTimeout(ctx, f.Timeout)
	defer cancel()
	res, err := session.CallTool(pctx, &mcp.CallToolParams{Name: p.Tool, Arguments: p.Args})
	authStatus := func(detail string) (probeRowJSON, bool, bool) {
		if skipOnAuth {
			return probeRowJSON{Tool: p.Tool, Status: probeAuthSkip, Detail: "auth-class (heuristic): " + detail}, true, true
		}
		return probeRowJSON{Tool: p.Tool, Status: probeError, Detail: detail}, true, true
	}
	switch {
	case err != nil && errors.Is(err, context.DeadlineExceeded):
		row.Status = probeError
		row.Detail = "probe timed out after " + f.Timeout.String()
		return row, true, false
	case err != nil:
		if jev.IsAuthError(err) {
			return authStatus(shortError(err))
		}
		row.Status = probeError
		row.Detail = shortError(err)
		return row, true, false
	case res.IsError:
		text := resultErrorText(res)
		if jev.IsAuthError(errors.New(text)) {
			return authStatus(text)
		}
		row.Status = probeError
		row.Detail = text
		return row, true, false
	}
	row.Status = probeOK
	// Escalation is data: surface it when the probe maps to a canonical op
	// whose verdicts escalated. Parse failures do not fail the probe — the
	// tool answered; the diagnostics in the ops matrix cover shape drift.
	if op := probeOp(adapter, p.Tool); op != jev.OpKindUnknown {
		if verdicts, err := adapter.Parse(op, res); err == nil {
			for _, v := range verdicts {
				if v.Escalate {
					row.Status = probeEscalate
				}
			}
		}
	}
	return row, false, false
}

// probeOp finds the canonical op a probed tool maps onto, so the battery can
// surface escalation from tools with canonical shapes.
func probeOp(adapter jev.Adapter, tool string) jev.OpKind {
	for _, op := range []jev.OpKind{jev.OpKindNoul, jev.OpKindChoice, jev.OpKindScore, jev.OpKindVerify, jev.OpKindScreen, jev.OpKindGate} {
		if s := adapter.Supports(op); s.Supported && s.Via == tool {
			return op
		}
	}
	return jev.OpKindUnknown
}

// schemaDriftNote compares a sample built call's argument keys against the
// tool's declared input schema, as a data note on the support matrix — never
// a failure (schemas drift without the wire breaking).
func schemaDriftNote(tools []*mcp.Tool, adapter jev.Adapter, op jev.OpKind) string {
	sup := adapter.Supports(op)
	sample, args, err := adapter.Build(op, jev.Question{
		Text:     "schema sample",
		Claims:   []string{"schema sample"},
		Evidence: []string{"schema sample"},
		Options:  []string{"a", "b"},
		Levels:   []string{"a", "b"},
		Tool:     "schema sample",
		Input:    map[string]any{"k": "v"},
	})
	if err != nil || sample != sup.Via {
		return ""
	}
	for _, t := range tools {
		if t == nil || t.Name != sample {
			continue
		}
		schema, ok := t.InputSchema.(map[string]any)
		if !ok {
			return ""
		}
		props, ok := schema["properties"].(map[string]any)
		if !ok {
			return ""
		}
		var missing []string
		for key := range args {
			if _, declared := props[key]; !declared {
				missing = append(missing, key)
			}
		}
		if len(missing) == 0 {
			return ""
		}
		slices.Sort(missing)
		return "schema drift: " + sample + " does not declare " + strings.Join(missing, ", ")
	}
	return ""
}

// writeJevCheckReport renders the support matrix, the battery table (when
// probed), and the summary line. Rich uses the Unicode border; Plain the
// ASCII border.
func writeJevCheckReport(w io.Writer, res checkResult) error {
	border := lipgloss.NormalBorder()
	if ui.Current == ui.Plain {
		border = lipgloss.ASCIIBorder()
	}
	t := table.New().Border(border).Headers("OP", "SUPPORTED", "VIA / NOTE")
	for _, row := range res.Ops {
		supported := ui.IconFail + " no"
		via := row.Note
		if row.OK {
			supported = ui.IconSuccess + " yes"
			via = row.Via
			if row.Note != "" {
				via += " (" + row.Note + ")"
			}
		}
		if ui.Current == ui.Plain {
			supported = map[bool]string{true: "OK: yes", false: "ERROR: no"}[row.OK]
		}
		t.Row(row.Op, supported, via)
	}
	if err := format.WriteTable(w, t); err != nil {
		return err
	}
	if res.Summary == nil {
		return nil
	}
	bt := table.New().Border(border).Headers("TOOL", "STATUS", "DETAIL")
	for _, row := range res.Probes {
		bt.Row(row.Tool, probeStatusCell(row.Status), row.Detail)
	}
	if err := format.WriteTable(w, bt); err != nil {
		return err
	}
	s := res.Summary
	_, err := fmt.Fprintf(w, "probes: %d ok, %d escalated, %d skipped, %d auth_skipped, %d errors\n",
		s.OK, s.Escalate, s.Skipped, s.AuthSkipped, s.Errors)
	return err
}

// probeStatusCell renders a battery status for the table: icons in Rich,
// ASCII labels in Plain.
func probeStatusCell(status string) string {
	if ui.Current == ui.Plain {
		switch status {
		case probeOK:
			return "OK"
		case probeEscalate:
			return "WARN: escalate"
		case probeSkipped:
			return "SKIP"
		case probeAuthSkip:
			return "SKIP: auth"
		default:
			return "ERROR"
		}
	}
	switch status {
	case probeOK:
		return ui.IconSuccess + " ok"
	case probeEscalate:
		return ui.IconWarn + " escalate"
	case probeSkipped:
		return "skip"
	case probeAuthSkip:
		return ui.IconWarn + " auth_skipped"
	default:
		return ui.IconFail + " error"
	}
}
