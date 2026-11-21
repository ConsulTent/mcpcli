package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"time"

	"github.com/ConsulTent/mcpcli/internal/format"
	"github.com/ConsulTent/mcpcli/internal/ui"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// Session operations shared by the one-shot commands and the REPL. Each op
// runs one ClientSession call, wraps its error, and renders the result by the
// global output mode — the skeleton both callers previously duplicated.
// Callers own everything around the op: connection/timeout scope, argument
// parsing (which stays per-path), and error rendering (cobra text + exit 1
// for commands, replError for the REPL loop).

// opListTools mirrors "tools list".
func opListTools(ctx context.Context, s *mcp.ClientSession, out io.Writer) error {
	res, err := s.ListTools(ctx, nil)
	if err != nil {
		return fmt.Errorf("list tools: %w", err)
	}
	if ui.Current == ui.JSON {
		return format.EmitJSON(out, res)
	}
	return format.ToolsList(out, res, ui.Current == ui.Plain)
}

// opCallTool mirrors "tools call NAME". In --json mode an isError result is
// still data: the full result reaches stdout first, then ErrToolError is
// returned so the caller can fail with the right exit semantics.
//
// A non-empty outfile diverts image/audio blocks to files (write first, so a
// write failure fails fast) before the result renders in either mode. An
// isError result never writes, whatever outfile says.
//
// Two smart-error enrichments guide the next invocation, both fail-open (a
// failed guidance fetch never masks the original outcome): an unknown tool
// name appends the available tools to the returned error, and a rejected call
// (isError result) prints one stderr note naming the tool's parameters —
// before the mode branch, so both render modes get it on errOut while
// stdout's one-document contract and ErrToolError's pinned string stay
// untouched.
func opCallTool(ctx context.Context, s *mcp.ClientSession, name string, args map[string]any, out, errOut io.Writer, outfile string) error {
	res, err := s.CallTool(ctx, &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		callErr := fmt.Errorf("call tool %s: %w", name, err)
		if !isUnknownTool(err, name) {
			return callErr
		}
		names, ok := availableToolNames(ctx, s)
		if !ok {
			return callErr
		}
		return fmt.Errorf("call tool %s: %w\navailable tools: %s", name, err, strings.Join(names, ", "))
	}
	if outfile != "" && !res.IsError {
		medias := toolMedia(res)
		if len(medias) == 0 {
			return fmt.Errorf("--outfile: no image or audio content in tool result")
		}
		if err := writeMediaFiles(outfile, medias, errOut); err != nil {
			return err
		}
	}
	if res.IsError {
		hintToolParams(ctx, s, name, errOut)
	}
	if ui.Current == ui.JSON {
		if err := format.EmitJSON(out, res); err != nil {
			return err
		}
		if res.IsError {
			return format.ErrToolError
		}
		return nil
	}
	return format.ToolCall(out, errOut, res)
}

// isUnknownTool reports whether a CallTool error means the server has no tool
// with that name (as opposed to rejecting its arguments). Matches the wire
// shape observed from SDK servers — `calling "tools/call": unknown tool
// "NAME"` — the isMethodNotFound precedent's two-contains form, with the
// called name quoted so a merely tool-adjacent error message cannot trip it.
// A false positive only ever appends a truthful tool list.
func isUnknownTool(err error, name string) bool {
	if err == nil {
		return false
	}
	s := strings.ToLower(err.Error())
	return strings.Contains(s, "unknown tool") && strings.Contains(s, `"`+strings.ToLower(name)+`"`)
}

// availableToolNames lists the server's tool names for the unknown-tool
// guidance: one ListTools round-trip, single-page exactly like "tools list".
// Each server-supplied name is filtered through sanitizeNote and nil entries
// are skipped (a JSON null in the server's []*mcp.Tool must not panic — the
// adapter.go guard's shape). ok is false only when the listing itself fails,
// so callers can fail open to the original error; an empty list is still
// returned (the empty tail of the guidance line is truthful).
func availableToolNames(ctx context.Context, s *mcp.ClientSession) (names []string, ok bool) {
	res, err := s.ListTools(ctx, nil)
	if err != nil {
		return nil, false
	}
	for _, t := range res.Tools {
		if t == nil {
			continue
		}
		names = append(names, sanitizeNote(t.Name))
	}
	return names, true
}

// hintToolParams writes one stderr note naming the called tool's parameters
// with the required ones marked, when a rejected call suggests bad arguments:
// `fail parameters: count (required), text`. The names come from the tool's
// InputSchema via its own ListTools fetch (a CallToolResult carries no
// schema), sorted for deterministic output. Every read is comma-ok and
// fail-open — a hostile schema shape (non-map schema, non-list required,
// non-string entries) means no note, never a panic. Zero names means no note
// (schemaDriftNote's len==0 form).
func hintToolParams(ctx context.Context, s *mcp.ClientSession, name string, errOut io.Writer) {
	res, err := s.ListTools(ctx, nil)
	if err != nil {
		return
	}
	var tool *mcp.Tool
	for _, t := range res.Tools {
		if t != nil && t.Name == name {
			tool = t
			break
		}
	}
	if tool == nil {
		return
	}
	schema, ok := tool.InputSchema.(map[string]any)
	if !ok {
		return
	}
	props, ok := schema["properties"].(map[string]any)
	if !ok || len(props) == 0 {
		return
	}
	required := make(map[string]bool)
	if reqList, ok := schema["required"].([]any); ok {
		for _, r := range reqList {
			if str, ok := r.(string); ok {
				required[str] = true
			}
		}
	}
	names := make([]string, 0, len(props))
	for key := range props {
		names = append(names, key)
	}
	slices.Sort(names)
	parts := make([]string, 0, len(names))
	for _, n := range names {
		if required[n] {
			parts = append(parts, sanitizeNote(n)+" (required)")
		} else {
			parts = append(parts, sanitizeNote(n))
		}
	}
	_, _ = fmt.Fprintln(errOut, sanitizeNote(name)+" parameters: "+strings.Join(parts, ", "))
}

// applySchemaDefaults fills required parameters that are ABSENT from args with
// the JSON `default` their property declares in the tool's input schema, and
// returns the (possibly newly seeded) args map. The one-shot "tools call" gates
// it on !strict; the REPL's call always applies it (no --strict exists there).
//
// Fill policy, per the converged battery's folds:
//   - NIL ARGS IS THE EMPTY SET: the REPL passes a nil map when the line
//     carries no JSON object, so the map is seeded before the first injection
//     (assignment to a nil map would panic).
//   - Presence, not truthiness, is the precedence test: a user-supplied value
//     — an empty string, a JSON null, a zero number — is PRESENT and wins;
//     defaults cover absences only.
//   - A JSON `default` of null is NO default: one nil read covers the absent
//     key, an explicit `default: null`, and a required name missing from
//     properties (reading a nil map yields zero, never a panic). A declared
//     0/""/false is injected as-is — the zero-value rejection bars FABRICATION
//     (no declared default), never honoring declared defaults.
//   - Optional parameters are never fabricated: absent means absent, and a
//     fabricated `limit: 0` changes semantics.
//   - Fail-open everywhere: a listing failure, a tool not found, or a hostile
//     schema shape (non-map schema, no properties, non-list required,
//     non-string entries) returns the original args unchanged — a data note,
//     never a failure (the schemaDriftNote posture).
//
// When anything was filled, ONE stderr note names every fill in every output
// mode; each value renders as sanitizeNote of its compact JSON so the 160-rune
// cap bounds server-supplied VALUES on stderr (a hostile five-megabyte default
// would otherwise dump megabytes per lenient call), and the name=value parts
// sort by name for deterministic output (the hintToolParams discipline).
func applySchemaDefaults(ctx context.Context, session *mcp.ClientSession, name string, args map[string]any, errOut io.Writer) map[string]any {
	res, err := session.ListTools(ctx, nil)
	if err != nil {
		return args
	}
	var tool *mcp.Tool
	for _, t := range res.Tools {
		if t != nil && t.Name == name {
			tool = t
			break
		}
	}
	if tool == nil {
		return args
	}
	schema, ok := tool.InputSchema.(map[string]any)
	if !ok {
		return args
	}
	props, ok := schema["properties"].(map[string]any)
	if !ok {
		return args
	}
	reqList, ok := schema["required"].([]any)
	if !ok {
		return args
	}
	var filled []string
	for _, r := range reqList {
		key, ok := r.(string)
		if !ok {
			continue
		}
		if _, ok := args[key]; ok {
			continue // user sources win: presence, not truthiness
		}
		prop, _ := props[key].(map[string]any)
		def := prop["default"] // nil-map read covers absent key, default:null, required-not-in-properties
		if def == nil {
			continue // JSON null is NO default
		}
		b, err := json.Marshal(def)
		if err != nil {
			continue // unreachable for JSON-decoded values; fail open without the fill
		}
		if args == nil {
			args = map[string]any{} // nil args is the empty set; a nil map panics on assignment
		}
		args[key] = def
		filled = append(filled, sanitizeNote(key)+"="+sanitizeNote(string(b)))
	}
	if len(filled) > 0 {
		slices.Sort(filled) // unique names, so sorting the name=value parts sorts by name
		_, _ = fmt.Fprintln(errOut, "filled missing arguments from schema defaults: "+strings.Join(filled, ", "))
	}
	return args
}

// resourcesListJSON is the --json shape of "resources list": one document
// holding both list results, since the command performs both requests.
type resourcesListJSON struct {
	Resources *mcp.ListResourcesResult         `json:"resources"`
	Templates *mcp.ListResourceTemplatesResult `json:"templates"`
}

// opListResources mirrors "resources list".
func opListResources(ctx context.Context, s *mcp.ClientSession, out io.Writer) error {
	res, err := s.ListResources(ctx, nil)
	if err != nil {
		return fmt.Errorf("list resources: %w", err)
	}
	tpls, err := s.ListResourceTemplates(ctx, nil)
	if err != nil {
		return fmt.Errorf("list resource templates: %w", err)
	}
	if ui.Current == ui.JSON {
		return format.EmitJSON(out, resourcesListJSON{Resources: res, Templates: tpls})
	}
	return format.ResourcesList(out, res, tpls, ui.Current == ui.Plain)
}

// opReadResource mirrors "resources read URI". A non-empty outfile diverts
// blob entries to files; text entries still print on stdout. Media
// extraction and every failure happen before a single content byte reaches
// stdout, so a text-only resource with --outfile errors with nothing printed.
// In --json mode the full document still reaches stdout after the write.
func opReadResource(ctx context.Context, s *mcp.ClientSession, uri string, out, errOut io.Writer, outfile string) error {
	res, err := s.ReadResource(ctx, &mcp.ReadResourceParams{URI: uri})
	if err != nil {
		return fmt.Errorf("read resource %s: %w", uri, err)
	}
	if outfile == "" {
		if ui.Current == ui.JSON {
			return format.EmitJSON(out, res)
		}
		return format.ResourceRead(out, res)
	}
	medias := resourceMedia(res)
	if len(medias) == 0 {
		return fmt.Errorf("--outfile: no binary content in resource")
	}
	if err := writeMediaFiles(outfile, medias, errOut); err != nil {
		return err
	}
	if ui.Current == ui.JSON {
		return format.EmitJSON(out, res)
	}
	// Blob entries diverted to files; render only the non-blob (text) ones
	// through the shared formatter.
	textOnly := &mcp.ReadResourceResult{}
	for _, rc := range res.Contents {
		if rc != nil && len(rc.Blob) == 0 {
			textOnly.Contents = append(textOnly.Contents, rc)
		}
	}
	return format.ResourceRead(out, textOnly)
}

// opListPrompts mirrors "prompts list".
func opListPrompts(ctx context.Context, s *mcp.ClientSession, out io.Writer) error {
	res, err := s.ListPrompts(ctx, nil)
	if err != nil {
		return fmt.Errorf("list prompts: %w", err)
	}
	if ui.Current == ui.JSON {
		return format.EmitJSON(out, res)
	}
	return format.PromptsList(out, res, ui.Current == ui.Plain)
}

// opGetPrompt mirrors "prompts get NAME" with string-typed arguments.
func opGetPrompt(ctx context.Context, s *mcp.ClientSession, name string, args map[string]string, out io.Writer) error {
	res, err := s.GetPrompt(ctx, &mcp.GetPromptParams{Name: name, Arguments: args})
	if err != nil {
		return fmt.Errorf("get prompt %s: %w", name, err)
	}
	if ui.Current == ui.JSON {
		return format.EmitJSON(out, res)
	}
	return format.PromptGet(out, res)
}

// opInfo mirrors "info": the initialize result the session negotiated, so no
// request is sent and no context is needed.
func opInfo(s *mcp.ClientSession, out io.Writer) error {
	init := s.InitializeResult()
	if ui.Current == ui.JSON {
		return format.EmitJSON(out, init)
	}
	return format.ServerInfo(out, init)
}

// pingResult is the machine-readable shape of a successful ping. Via is set
// only when the capability fallback verified the session.
type pingResult struct {
	OK        bool   `json:"ok"`
	LatencyMS int64  `json:"latency_ms"`
	Via       string `json:"via,omitempty"`
}

// pingSession performs one meta-aware Ping round trip: on a 2026-07-28
// (discovery-negotiated) session it sets the SEP-2575 _meta.protocolVersion
// explicitly, because the SDK's Ping never injects it and servers validating
// header/meta consistency reject a meta-less request with -32020.
func pingSession(ctx context.Context, s *mcp.ClientSession) (time.Duration, error) {
	negotiated := ""
	if init := s.InitializeResult(); init != nil {
		negotiated = init.ProtocolVersion
	}
	params := &mcp.PingParams{}
	if negotiated >= "2026-07-28" {
		params.SetMeta(map[string]any{mcp.MetaKeyProtocolVersion: negotiated})
	}
	start := time.Now()
	err := s.Ping(ctx, params)
	return time.Since(start), err
}

// errNoVerification is returned by verifySession when the server implements
// neither ping nor any listable capability, so no in-session verification is
// possible. Callers decide what that means (auth login verifies by connect).
var errNoVerification = errors.New("server implements neither ping nor any listable capability")

// isMethodNotFound reports whether a session-call error means the server has
// no handler for the method (as opposed to rejecting its contents). Matches
// the wire shape observed against discovery-native servers that omit ping:
// `calling "ping": ... not found`.
func isMethodNotFound(err error) bool {
	if err == nil {
		return false
	}
	s := strings.ToLower(err.Error())
	return strings.Contains(s, `calling "ping"`) && strings.Contains(s, "not found")
}

// fallbackMethod picks the verification alternative for a server without
// ping: the first listable capability it declared, in tools/list ->
// resources/list -> prompts/list order. Empty when nothing is listable.
func fallbackMethod(caps *mcp.ServerCapabilities) string {
	switch {
	case caps == nil:
		return ""
	case caps.Tools != nil:
		return "tools/list"
	case caps.Resources != nil:
		return "resources/list"
	case caps.Prompts != nil:
		return "prompts/list"
	default:
		return ""
	}
}

// verifySession verifies a live session with the best available method: the
// meta-aware ping first; when the server does not implement ping, the first
// declared-capability list request instead. via names what verified ("ping"
// or the fallback method) so callers can report it honestly.
func verifySession(ctx context.Context, s *mcp.ClientSession) (via string, latency time.Duration, err error) {
	latency, err = pingSession(ctx, s)
	if err == nil {
		return "ping", latency, nil
	}
	if !isMethodNotFound(err) {
		return "", 0, err
	}
	method := fallbackMethod(s.InitializeResult().Capabilities)
	if method == "" {
		return "", 0, errNoVerification
	}
	start := time.Now()
	switch method {
	case "tools/list":
		if _, err = s.ListTools(ctx, nil); err != nil {
			return "", 0, fmt.Errorf("tools/list: %w", err)
		}
	case "resources/list":
		if _, err = s.ListResources(ctx, nil); err != nil {
			return "", 0, fmt.Errorf("resources/list: %w", err)
		}
	default: // "prompts/list"
		if _, err = s.ListPrompts(ctx, nil); err != nil {
			return "", 0, fmt.Errorf("prompts/list: %w", err)
		}
	}
	return method, time.Since(start), nil
}

// renderPing writes one verification result line in the active output mode.
// The ping path keeps the classic "pong" wording; a capability fallback says
// "alive via <method>" so the report names what actually ran. JSON gains the
// "via" field only when the fallback was used.
func renderPing(out io.Writer, via string, latency time.Duration) {
	ms := latency.Milliseconds()
	if ui.Current == ui.JSON {
		res := pingResult{OK: true, LatencyMS: ms}
		if via != "ping" {
			res.Via = via
		}
		_ = format.EmitJSON(out, res)
		return
	}
	if via == "ping" {
		if ui.Current == ui.Plain {
			fmt.Fprintf(out, "pong: %dms\n", ms)
			return
		}
		fmt.Fprintf(out, "%s pong in %dms\n", ui.IconSuccess, ms)
		return
	}
	if ui.Current == ui.Plain {
		fmt.Fprintf(out, "alive via %s: %dms\n", via, ms)
		return
	}
	fmt.Fprintf(out, "%s alive via %s in %dms\n", ui.IconSuccess, via, ms)
}

// opPing mirrors "ping": one timed round trip, or the capability fallback
// when the server does not implement ping. The result line is command output
// and goes to stdout (not through ui.Success) so JSON stays a single document
// and Plain stays byte-ASCII.
func opPing(ctx context.Context, s *mcp.ClientSession, out io.Writer) error {
	via, latency, err := verifySession(ctx, s)
	if err != nil {
		return fmt.Errorf("ping: %w", err)
	}
	renderPing(out, via, latency)
	return nil
}
