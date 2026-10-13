package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
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
func opCallTool(ctx context.Context, s *mcp.ClientSession, name string, args map[string]any, out, errOut io.Writer) error {
	res, err := s.CallTool(ctx, &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		return fmt.Errorf("call tool %s: %w", name, err)
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

// opReadResource mirrors "resources read URI".
func opReadResource(ctx context.Context, s *mcp.ClientSession, uri string, out io.Writer) error {
	res, err := s.ReadResource(ctx, &mcp.ReadResourceParams{URI: uri})
	if err != nil {
		return fmt.Errorf("read resource %s: %w", uri, err)
	}
	if ui.Current == ui.JSON {
		return format.EmitJSON(out, res)
	}
	return format.ResourceRead(out, res)
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
