package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// newTestMCPServerWithTools hosts an in-memory MCP server with the given tools
// registered before any session connects, behind the streamable HTTP handler —
// the same topology newTestMCPServer uses.
func newTestMCPServerWithTools(t *testing.T, add func(*mcp.Server)) *httptest.Server {
	t.Helper()
	srv := mcp.NewServer(&mcp.Implementation{Name: "testserver", Version: "1.0.0"}, nil)
	add(srv)
	httpSrv := httptest.NewServer(mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return srv }, nil))
	t.Cleanup(httpSrv.Close)
	return httpSrv
}

// addEchoTool registers a tool that returns its whole arguments object
// JSON-encoded as text, so tests can assert exactly what reached the server.
func addEchoTool(s *mcp.Server) {
	s.AddTool(&mcp.Tool{
		Name:        "echo",
		Description: "Echoes its arguments as JSON text",
		InputSchema: json.RawMessage(`{"type":"object"}`),
	}, func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		var args map[string]any
		if len(req.Params.Arguments) > 0 {
			if err := json.Unmarshal(req.Params.Arguments, &args); err != nil {
				return nil, err
			}
		}
		if args == nil {
			args = map[string]any{}
		}
		b, err := json.Marshal(args)
		if err != nil {
			return nil, err
		}
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: string(b)}}}, nil
	})
}

// newDefaultsToolAdder builds an Add function registering one tool for the
// smart-defaults fixtures. Its raw AddTool handler echoes req.Params.Arguments
// verbatim and returns isError when one of the required parameters is ABSENT
// from the raw args — an absence-only check (`_, ok := m[k]`), so a zero value
// passes but a missing key never does, which is exactly what makes any
// truthiness-based client fill flip the outcome and fail these cells. The raw
// AddTool handler path performs no server-side schema processing (go-sdk
// dispatches the handler unwrapped; input validation is the caller's job), so
// the client's schema-defaults fill is the only thing that can turn the fill
// cells green.
func newDefaultsToolAdder(name, schema string, required ...string) func(*mcp.Server) {
	return func(s *mcp.Server) {
		s.AddTool(&mcp.Tool{
			Name:        name,
			Description: "Defaults fixture: echoes raw arguments, rejects absent required ones",
			InputSchema: json.RawMessage(schema),
		}, func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			var m map[string]any
			if len(req.Params.Arguments) > 0 {
				if err := json.Unmarshal(req.Params.Arguments, &m); err != nil {
					m = nil
				}
			}
			for _, r := range required {
				if _, ok := m[r]; !ok {
					return &mcp.CallToolResult{
						IsError: true,
						Content: []mcp.Content{&mcp.TextContent{Text: "missing " + r}},
					}, nil
				}
			}
			return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: string(req.Params.Arguments)}}}, nil
		})
	}
}

// defaultsFillSchema is the fill cell's schema: required query (string, no
// default) and required k (number, default 5), plus the OPTIONAL lang (string,
// default "en") — the optional-with-default property exists so the
// optional-never-fabricated ruling has its own assertion in the fill cell.
const defaultsFillSchema = `{"type":"object","properties":{"query":{"type":"string"},` +
	`"k":{"type":"number","default":5},"lang":{"type":"string","default":"en"}},"required":["query","k"]}`

// TestToolsListE2E: a server with one tool lists its name and description.
func TestToolsListE2E(t *testing.T) {
	srv := newTestMCPServerWithTools(t, addEchoTool)

	stdout, _, _, err := runRoot(t, "--url", srv.URL, "tools", "list")
	if err != nil {
		t.Fatalf("tools list: %v", err)
	}
	if !strings.Contains(stdout, "echo") {
		t.Errorf("tools list stdout %q does not name the tool echo", stdout)
	}
	if !strings.Contains(stdout, "Echoes its arguments as JSON text") {
		t.Errorf("tools list stdout %q does not include the tool description", stdout)
	}
}

// TestToolsListPlainASCII: the list table must honor the --plain contract on
// stdout too — every byte ASCII, no escape sequences (the table border is
// ASCII in plain mode, not the Unicode box-drawing default).
func TestToolsListPlainASCII(t *testing.T) {
	srv := newTestMCPServerWithTools(t, addEchoTool)

	stdout, stderr, dec, err := runRoot(t, "--url", srv.URL, "--plain", "tools", "list")
	if err != nil {
		t.Fatalf("tools list --plain: %v", err)
	}
	for name, s := range map[string]string{"stdout": stdout, "stderr": stderr, "ui output": dec} {
		for i := 0; i < len(s); i++ {
			if b := s[i]; b >= 0x80 {
				t.Fatalf("tools list --plain: %s has non-ASCII byte 0x%02x at offset %d: %q", name, b, i, s)
			}
			if b := s[i]; b == 0x1b {
				t.Fatalf("tools list --plain: %s has an ESC byte at offset %d: %q", name, i, s)
			}
		}
	}
	if !strings.Contains(stdout, "echo") {
		t.Errorf("tools list --plain stdout %q does not name the tool echo", stdout)
	}
}

// TestToolsListJSONEmpty: a server with no tools still lists successfully in
// --json mode: exit 0 and exactly one valid JSON document (the SDK's
// ListToolsResult as-is; "tools":null for a zero-value result is fine).
func TestToolsListJSONEmpty(t *testing.T) {
	srv := newTestMCPServer(t)

	stdout, stderr, dec, err := runRoot(t, "--url", srv.URL, "--json", "tools", "list")
	if err != nil {
		t.Fatalf("tools list --json: %v", err)
	}
	var res map[string]any
	if err := json.Unmarshal([]byte(stdout), &res); err != nil {
		t.Fatalf("tools list --json stdout %q is not JSON: %v", stdout, err)
	}
	if _, ok := res["tools"]; !ok {
		t.Errorf("tools list --json output %q lacks the tools key", stdout)
	}
	if stderr != "" || dec != "" {
		t.Errorf("tools list --json wrote decoration: stderr %q, ui output %q", stderr, dec)
	}
}

// TestToolsCallE2E: calling a tool with --arg pairs sends the decoded
// arguments and prints the tool's content to stdout.
func TestToolsCallE2E(t *testing.T) {
	srv := newTestMCPServerWithTools(t, addEchoTool)

	stdout, _, _, err := runRoot(t, "--url", srv.URL, "tools", "call", "echo", "--arg", "msg=hello", "--arg", "count=3")
	if err != nil {
		t.Fatalf("tools call: %v", err)
	}
	if !strings.Contains(stdout, `"msg":"hello"`) {
		t.Errorf("tools call stdout %q does not echo msg=hello", stdout)
	}
	if !strings.Contains(stdout, `"count":3`) {
		t.Errorf("tools call stdout %q does not show count decoded as the number 3", stdout)
	}
}

// TestToolsCallFileArgsE2E pins the merge order: file arguments load first and
// --arg pairs override them key by key.
func TestToolsCallFileArgsE2E(t *testing.T) {
	srv := newTestMCPServerWithTools(t, addEchoTool)
	path := filepath.Join(t.TempDir(), "args.json")
	if err := os.WriteFile(path, []byte(`{"msg":"fromfile","extra":1}`), 0o600); err != nil {
		t.Fatal(err)
	}

	stdout, _, _, err := runRoot(t, "--url", srv.URL, "tools", "call", "echo", "-A", path, "--arg", "msg=fromarg")
	if err != nil {
		t.Fatalf("tools call with args file: %v", err)
	}
	if !strings.Contains(stdout, `"msg":"fromarg"`) {
		t.Errorf("stdout %q: --arg did not override the file's msg", stdout)
	}
	if !strings.Contains(stdout, `"extra":1`) {
		t.Errorf("stdout %q: file-only key extra is missing", stdout)
	}
	if strings.Contains(stdout, "fromfile") {
		t.Errorf("stdout %q: the file value leaked through the override", stdout)
	}
}

// TestToolsCallIsErrorExitCode pins Review Focus 1: an isError result exits 1
// in both output modes. Human mode routes the error content to stderr; --json
// still prints the full CallToolResult JSON to stdout.
func TestToolsCallIsErrorExitCode(t *testing.T) {
	srv := newTestMCPServerWithTools(t, func(s *mcp.Server) {
		s.AddTool(&mcp.Tool{
			Name:        "fail",
			Description: "Always fails",
			InputSchema: json.RawMessage(`{"type":"object"}`),
		}, func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			return &mcp.CallToolResult{
				IsError: true,
				Content: []mcp.Content{&mcp.TextContent{Text: "boom"}},
			}, nil
		})
	})

	// Human mode: the error content lands on stderr and the command fails.
	stdout, stderr, _, err := runRoot(t, "--url", srv.URL, "tools", "call", "fail")
	if err == nil || !strings.Contains(err.Error(), "tool returned an error") {
		t.Fatalf("tools call fail = %v, want ErrToolError", err)
	}
	if !strings.Contains(stderr, "boom") {
		t.Errorf("human stderr %q does not carry the tool's error content", stderr)
	}
	if strings.Contains(stdout, "boom") {
		t.Errorf("human stdout %q carries the error content; it belongs on stderr", stdout)
	}

	// JSON mode: the full result still reaches stdout as JSON, and the
	// command still fails.
	stdout, _, _, err = runRoot(t, "--url", srv.URL, "--json", "tools", "call", "fail")
	if err == nil || !strings.Contains(err.Error(), "tool returned an error") {
		t.Fatalf("tools call fail --json = %v, want ErrToolError", err)
	}
	var res map[string]any
	if err := json.Unmarshal([]byte(stdout), &res); err != nil {
		t.Fatalf("tools call fail --json stdout %q is not JSON: %v", stdout, err)
	}
	if res["isError"] != true {
		t.Errorf("isError = %v, want true in the JSON output %q", res["isError"], stdout)
	}
	content, ok := res["content"].([]any)
	if !ok || len(content) == 0 {
		t.Fatalf("content = %v in %q, want one text block", res["content"], stdout)
	}
	block, ok := content[0].(map[string]any)
	if !ok || block["text"] != "boom" {
		t.Errorf("content[0] = %v in %q, want text \"boom\"", content[0], stdout)
	}
}

// TestToolsCallJSONE2E: a successful call in --json mode emits exactly the
// CallToolResult document, with no decoration anywhere.
func TestToolsCallJSONE2E(t *testing.T) {
	srv := newTestMCPServerWithTools(t, addEchoTool)

	stdout, stderr, dec, err := runRoot(t, "--url", srv.URL, "--json", "tools", "call", "echo", "--arg", "msg=hi")
	if err != nil {
		t.Fatalf("tools call --json: %v", err)
	}
	var res map[string]any
	if err := json.Unmarshal([]byte(stdout), &res); err != nil {
		t.Fatalf("tools call --json stdout %q is not JSON: %v", stdout, err)
	}
	if _, isErr := res["isError"]; isErr && res["isError"] == true {
		t.Errorf("echo --json reported isError in %q", stdout)
	}
	content, ok := res["content"].([]any)
	if !ok || len(content) == 0 {
		t.Fatalf("content = %v in %q, want one text block", res["content"], stdout)
	}
	block, ok := content[0].(map[string]any)
	if !ok || !strings.Contains(fmt.Sprint(block["text"]), `"msg":"hi"`) {
		t.Errorf("content[0] = %v in %q, want the echoed args", content[0], stdout)
	}
	if stderr != "" || dec != "" {
		t.Errorf("tools call --json wrote decoration: stderr %q, ui output %q", stderr, dec)
	}
}

// TestToolsCallBadPairFailsFast: a malformed --arg pair is rejected before any
// connection is opened.
func TestToolsCallBadPairFailsFast(t *testing.T) {
	_, _, _, err := runRoot(t, "tools", "call", "echo", "--arg", "noequals")
	if err == nil || !strings.Contains(err.Error(), "noequals") {
		t.Fatalf("tools call with bad pair = %v, want an error naming the pair", err)
	}
}

// toolsCallGuidance is the golden guidance string for a wrong positional
// count: the tool name is the only positional, and the ways to pass real
// arguments are named so the next invocation can be correct.
const toolsCallGuidance = "tools call takes the tool name as its only positional argument; " +
	"pass tool arguments with --arg key=value (values JSON-decode: --arg count=3) or -A file.json"

// TestToolsCallArgGuidance pins the custom positional validator on BOTH
// bounds: two positionals and zero positionals get the same pre-connection
// guidance (cobra's bare ExactArgs text names no --arg), and a >1-only check
// would let the zero-arg form through to a post-connect panic on args[0].
func TestToolsCallArgGuidance(t *testing.T) {
	srv := newTestMCPServerWithTools(t, addEchoTool)

	_, _, _, err := runRoot(t, "--url", srv.URL, "--auth", "none", "tools", "call", "a", "b")
	if err == nil || !strings.Contains(err.Error(), toolsCallGuidance) {
		t.Fatalf("tools call with two positionals = %v, want the argument guidance", err)
	}

	_, _, _, err = runRoot(t, "--url", srv.URL, "--auth", "none", "tools", "call")
	if err == nil || !strings.Contains(err.Error(), toolsCallGuidance) {
		t.Fatalf("tools call with no positional = %v, want the same guidance (a >1-only check panics on args[0] after connecting)", err)
	}
}

// TestToolsCallUnknownToolListsAvailable: an unknown tool name fails with the
// server's error plus an appended "available tools:" line built from one
// ListTools fetch. The hostileESC-named registration proves the enrichment
// sanitizes this second server-supplied names site: the echoed line carries
// the sanitized names and no escape byte anywhere.
func TestToolsCallUnknownToolListsAvailable(t *testing.T) {
	srv := newTestMCPServerWithTools(t, func(s *mcp.Server) {
		addEchoTool(s)
		s.AddTool(&mcp.Tool{
			Name:        hostileESC,
			Description: "Hostile name",
			InputSchema: json.RawMessage(`{"type":"object"}`),
		}, func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "never called"}}}, nil
		})
	})

	_, stderr, _, err := runRoot(t, "--url", srv.URL, "tools", "call", "nonexistent")
	if err == nil || !strings.Contains(err.Error(), "available tools: ") {
		t.Fatalf("unknown tool call = %v, want the appended available-tools line", err)
	}
	if !strings.Contains(err.Error(), "echo") {
		t.Errorf("error %q does not list the echo tool", err)
	}
	assertNoESC(t, "stderr", stderr)
	assertNoESC(t, "error", err.Error())
}

// TestToolsCallIsErrorHintsParameters: a rejected call (isError result) adds
// one stderr note naming the tool's parameters with the required ones marked,
// sorted for a deterministic golden. In --json mode stdout stays exactly the
// one result document and the ErrToolError failure is unchanged — the note
// rides stderr in both modes.
func TestToolsCallIsErrorHintsParameters(t *testing.T) {
	srv := newTestMCPServerWithTools(t, func(s *mcp.Server) {
		s.AddTool(&mcp.Tool{
			Name:        "fail",
			Description: "Always fails",
			InputSchema: json.RawMessage(`{"type":"object","properties":{"text":{},"count":{}},"required":["count"]}`),
		}, func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			return &mcp.CallToolResult{
				IsError: true,
				Content: []mcp.Content{&mcp.TextContent{Text: "missing count"}},
			}, nil
		})
	})

	// Human mode: the note lands on stderr beside the tool's error content.
	_, stderr, _, err := runRoot(t, "--url", srv.URL, "tools", "call", "fail")
	if err == nil || !strings.Contains(err.Error(), "tool returned an error") {
		t.Fatalf("tools call fail = %v, want ErrToolError", err)
	}
	if !strings.Contains(stderr, "fail parameters: count (required), text") {
		t.Errorf("human stderr %q lacks the sorted required-marked parameter note", stderr)
	}

	// JSON mode: stdout stays the one result document, the command still
	// fails with ErrToolError, and the note still rides stderr.
	stdout, stderr, _, err := runRoot(t, "--url", srv.URL, "--json", "tools", "call", "fail")
	if err == nil || !strings.Contains(err.Error(), "tool returned an error") {
		t.Fatalf("tools call fail --json = %v, want ErrToolError", err)
	}
	var res map[string]any
	if err := json.Unmarshal([]byte(stdout), &res); err != nil {
		t.Fatalf("tools call fail --json stdout %q is not one JSON document: %v", stdout, err)
	}
	if res["isError"] != true {
		t.Errorf("isError = %v in %q, want true", res["isError"], stdout)
	}
	if !strings.Contains(stderr, "fail parameters: count (required), text") {
		t.Errorf("--json stderr %q lacks the parameter note", stderr)
	}
}

// TestToolsCallFillSchemaDefaults pins the smart-defaults fill: a required
// parameter ABSENT from the arguments but carrying a schema `default` is
// filled client-side before the call, and the fill is named on stderr in both
// modes. The fixture's raw handler echoes the RAW arguments, so the echo
// proves what actually went over the wire: k arrives as its default 5, the
// user's query survives verbatim, and the optional lang — which also declares
// a default — stays absent (absence is not zero: optional parameters are
// never fabricated).
func TestToolsCallFillSchemaDefaults(t *testing.T) {
	srv := newTestMCPServerWithTools(t, newDefaultsToolAdder("defaults", defaultsFillSchema, "query", "k"))

	// Human mode: the call succeeds, the echo carries the fill, the note
	// names it with its value.
	stdout, stderr, _, err := runRoot(t, "--url", srv.URL, "tools", "call", "defaults",
		"--arg", "query=What is in my documents?")
	if err != nil {
		t.Fatalf("tools call defaults with missing k = %v, want the schema-defaults fill to succeed (stderr %q)", err, stderr)
	}
	if !strings.Contains(stdout, `"query":"What is in my documents?"`) {
		t.Errorf("stdout %q does not carry the user's query verbatim", stdout)
	}
	if !strings.Contains(stdout, `"k":5`) {
		t.Errorf("stdout %q does not carry k filled from its schema default 5", stdout)
	}
	if strings.Contains(stdout, `"lang"`) {
		t.Errorf("stdout %q carries lang; an optional parameter must never be fabricated", stdout)
	}
	if !strings.Contains(stderr, "k=5") {
		t.Errorf("stderr %q does not name the k fill with its value", stderr)
	}

	// JSON mode: stdout parses as exactly ONE CallToolResult document (a
	// concatenated second document makes Unmarshal fail) carrying the echoed
	// k=5, and the fill note is the first — and only — stderr emission on the
	// success path.
	stdout, stderr, dec, err := runRoot(t, "--url", srv.URL, "--json", "tools", "call", "defaults",
		"--arg", "query=What is in my documents?")
	if err != nil {
		t.Fatalf("tools call defaults --json = %v, want success (stderr %q)", err, stderr)
	}
	var res map[string]any
	if err := json.Unmarshal([]byte(stdout), &res); err != nil {
		t.Fatalf("tools call defaults --json stdout %q is not exactly one JSON document: %v", stdout, err)
	}
	content, ok := res["content"].([]any)
	if !ok || len(content) == 0 {
		t.Fatalf("content = %v in %q, want one text block", res["content"], stdout)
	}
	block, ok := content[0].(map[string]any)
	if !ok {
		t.Fatalf("content[0] = %v in %q, want a text block", content[0], stdout)
	}
	text, _ := block["text"].(string)
	if !strings.Contains(text, `"k":5`) || !strings.Contains(text, `"query":"What is in my documents?"`) {
		t.Errorf("--json echoed args %q lack the k fill or the user's query", text)
	}
	if strings.Contains(text, `"lang"`) {
		t.Errorf("--json echoed args %q carry lang; an optional parameter must never be fabricated", text)
	}
	if want := "filled missing arguments from schema defaults: k=5\n"; stderr != want {
		t.Errorf("--json stderr = %q, want exactly the one fill note %q", stderr, want)
	}
	if dec != "" {
		t.Errorf("--json wrote ui decoration: %q", dec)
	}
}

// TestToolsCallStrictSkipsFill: --strict restores the pass-through shape — no
// fill, no fill note, and the rejected call keeps the v1.5.1 parameter hint
// (the failure path unchanged).
func TestToolsCallStrictSkipsFill(t *testing.T) {
	srv := newTestMCPServerWithTools(t, newDefaultsToolAdder("defaults", defaultsFillSchema, "query", "k"))

	_, stderr, _, err := runRoot(t, "--url", srv.URL, "tools", "call", "defaults",
		"--arg", "query=What is in my documents?", "--strict")
	if err == nil || !strings.Contains(err.Error(), "tool returned an error") {
		t.Fatalf("strict call = %v, want ErrToolError (the fill must not run under --strict)", err)
	}
	if !strings.Contains(stderr, "defaults parameters: k (required), lang, query (required)") {
		t.Errorf("strict stderr %q lacks the v1.5.1 parameter hint", stderr)
	}
	if strings.Contains(stderr, "filled") {
		t.Errorf("strict stderr %q carries a fill note; --strict must skip the fill", stderr)
	}
}

// TestToolsCallRequiredWithoutDefault: a required parameter without a declared
// default is never fabricated — the call still fails with the v1.5.1 hint in
// BOTH modes, pre- and post-fix (a GREEN-both-ways guard for the no-fabrication
// ruling).
func TestToolsCallRequiredWithoutDefault(t *testing.T) {
	srv := newTestMCPServerWithTools(t, newDefaultsToolAdder("nodefault",
		`{"type":"object","properties":{"query":{"type":"string"},"k":{"type":"number"}},"required":["query","k"]}`,
		"query", "k"))

	for _, mode := range []struct{ name, flag string }{{"lenient", ""}, {"strict", "--strict"}} {
		args := []string{"--url", srv.URL, "tools", "call", "nodefault", "--arg", "query=What is in my documents?"}
		if mode.flag != "" {
			args = append(args, mode.flag)
		}
		_, stderr, _, err := runRoot(t, args...)
		if err == nil || !strings.Contains(err.Error(), "tool returned an error") {
			t.Fatalf("%s call without a defaultable k = %v, want ErrToolError (nothing to fill from)", mode.name, err)
		}
		if !strings.Contains(stderr, "nodefault parameters: k (required), query") {
			t.Errorf("%s stderr %q lacks the parameter hint", mode.name, stderr)
		}
		if strings.Contains(stderr, "filled") {
			t.Errorf("%s stderr %q carries a fill note; a required param without a default must not be fabricated", mode.name, stderr)
		}
	}
}

// TestToolsCallNullDefaultIsNoDefault: an explicit JSON "default": null on a
// required param is NO default (safety fold F-R2-2) — the nil-read guard must
// skip it, so the call fails honestly instead of succeeding with k:null plus a
// fill note (the exit-0-null class). GREEN-both-ways in both modes.
func TestToolsCallNullDefaultIsNoDefault(t *testing.T) {
	srv := newTestMCPServerWithTools(t, newDefaultsToolAdder("nulldefault",
		`{"type":"object","properties":{"k":{"default":null}},"required":["k"]}`,
		"k"))

	for _, mode := range []struct{ name, flag string }{{"lenient", ""}, {"strict", "--strict"}} {
		args := []string{"--url", srv.URL, "tools", "call", "nulldefault"}
		if mode.flag != "" {
			args = append(args, mode.flag)
		}
		_, stderr, _, err := runRoot(t, args...)
		if err == nil || !strings.Contains(err.Error(), "tool returned an error") {
			t.Fatalf("%s call with default:null k = %v, want ErrToolError (null is no default)", mode.name, err)
		}
		if strings.Contains(stderr, "filled") {
			t.Errorf("%s stderr %q carries a fill note; a null default must not be injected", mode.name, stderr)
		}
	}
}

// TestToolsCallUserSuppliedWins: user sources always win — presence, not
// truthiness, is the precedence test (safety fold F-R2-3). The query property
// declares a "fallback" default in this fixture precisely so the empty-string
// arm is live: a truthiness-based fill would treat the passed empty string as
// absent and overwrite it with "fallback", failing the cell.
func TestToolsCallUserSuppliedWins(t *testing.T) {
	srv := newTestMCPServerWithTools(t, newDefaultsToolAdder("userwins",
		`{"type":"object","properties":{"query":{"type":"string","default":"fallback"},"k":{"type":"number","default":5}},"required":["query","k"]}`,
		"query", "k"))

	// A user-supplied k=3 beats the default: the echo carries 3 and no fill
	// note is written (GREEN both ways — the call also succeeded pre-fix).
	stdout, stderr, _, err := runRoot(t, "--url", srv.URL, "tools", "call", "userwins",
		"--arg", "query=What is in my documents?", "--arg", "k=3")
	if err != nil {
		t.Fatalf("user k=3 call = %v, want success (stderr %q)", err, stderr)
	}
	if !strings.Contains(stdout, `"k":3`) {
		t.Errorf("stdout %q does not carry the user's k=3", stdout)
	}
	if strings.Contains(stderr, "k=5") || strings.Contains(stderr, "filled") {
		t.Errorf("stderr %q carries a fill note for a user-supplied argument", stderr)
	}

	// Presence, not truthiness: an empty-string query is PRESENT with a zero
	// value — no query fill, no note for it — while k (absent) is still filled
	// from its default; a zero value in one argument must not block the fill
	// of another.
	stdout, stderr, _, err = runRoot(t, "--url", srv.URL, "tools", "call", "userwins",
		"--arg", "query=")
	if err != nil {
		t.Fatalf("empty-string query call = %v, want k filled from its default (stderr %q)", err, stderr)
	}
	if !strings.Contains(stdout, `"query":""`) {
		t.Errorf("stdout %q does not keep the user's empty-string query verbatim", stdout)
	}
	if !strings.Contains(stdout, `"k":5`) {
		t.Errorf("stdout %q does not carry k filled from its default despite the zero-value query", stdout)
	}
	if strings.Contains(stderr, "query=") || strings.Contains(stderr, "fallback") {
		t.Errorf("stderr %q names a query fill; a present zero value must not be overwritten", stderr)
	}

	// --arg k=null decodes to a PRESENT nil value: no fill, no note, and the
	// raw null rides to the handler whose absence-only check passes it (the
	// fold's second zero-value arm; a truthiness fill would send k=5).
	stdout, stderr, _, err = runRoot(t, "--url", srv.URL, "tools", "call", "userwins",
		"--arg", "query=What is in my documents?", "--arg", "k=null")
	if err != nil {
		t.Fatalf("k=null call = %v, want success with the null kept (stderr %q)", err, stderr)
	}
	if !strings.Contains(stdout, `"k":null`) {
		t.Errorf("stdout %q does not keep the user's k=null verbatim", stdout)
	}
	if strings.Contains(stdout, `"k":5`) {
		t.Errorf("stdout %q carries k=5; a present null must not be overwritten", stdout)
	}
	if strings.Contains(stderr, "filled") {
		t.Errorf("stderr %q carries a fill note for a present k=null", stderr)
	}
}

// TestToolsCallHostileShapesFailOpen: hostile schema shapes fail open to the
// original args (GREEN both ways — no fill ever happened, and no panic), and
// the hostile-value arm carries the note's per-value cap: a 300-rune default
// IS filled, but the stderr note must not contain the full string —
// sanitizeNote's 160-rune cap bounds every server-supplied note value.
func TestToolsCallHostileShapesFailOpen(t *testing.T) {
	bigDefault := strings.Repeat("A", 300)
	srv := newTestMCPServerWithTools(t, func(s *mcp.Server) {
		newDefaultsToolAdder("noprops", `{"type":"object"}`, "k")(s)
		newDefaultsToolAdder("orphanreq", `{"type":"object","properties":{"query":{"type":"string"}},"required":["k"]}`, "k")(s)
		newDefaultsToolAdder("bigdefault",
			fmt.Sprintf(`{"type":"object","properties":{"blob":{"type":"string","default":%q}},"required":["blob"]}`, bigDefault),
			"blob")(s)
	})

	// Hostile shape: the schema omits properties entirely — the applier's
	// comma-ok read fails and the original args go out unchanged.
	_, stderr, _, err := runRoot(t, "--url", srv.URL, "tools", "call", "noprops")
	if err == nil || !strings.Contains(err.Error(), "tool returned an error") {
		t.Fatalf("noprops call = %v, want ErrToolError (fail open to the original args)", err)
	}
	if strings.Contains(stderr, "filled") {
		t.Errorf("noprops stderr %q carries a fill note; a properties-less schema must fail open", stderr)
	}

	// Hostile shape: required names a key properties lacks — the nil property
	// read yields no default, so the same nil-default guard skips the fill.
	_, stderr, _, err = runRoot(t, "--url", srv.URL, "tools", "call", "orphanreq")
	if err == nil || !strings.Contains(err.Error(), "tool returned an error") {
		t.Fatalf("orphanreq call = %v, want ErrToolError (fail open to the original args)", err)
	}
	if strings.Contains(stderr, "filled") {
		t.Errorf("orphanreq stderr %q carries a fill note; a required-not-in-properties schema must fail open", stderr)
	}

	// Hostile VALUE: the 300-rune default is honored over the wire (the call
	// succeeds and the echo carries all of it) but the note shows only the
	// 160-rune-capped head.
	stdout, stderr, _, err := runRoot(t, "--url", srv.URL, "tools", "call", "bigdefault")
	if err != nil {
		t.Fatalf("bigdefault call = %v, want the 300-rune default filled (stderr %q)", err, stderr)
	}
	if !strings.Contains(stdout, bigDefault) {
		t.Errorf("stdout %q does not carry the full 300-rune default over the wire", stdout)
	}
	if !strings.Contains(stderr, strings.Repeat("A", 100)) {
		t.Errorf("stderr %q does not carry the capped head of the big default", stderr)
	}
	if strings.Contains(stderr, bigDefault) {
		t.Errorf("stderr %q carries the full 300-rune default; the note's value cap must bound it", stderr)
	}
}

// addImageTool registers a tool whose result is one image content block with
// fixed decoded bytes, so --outfile tests can assert the exact file content.
func addImageTool(s *mcp.Server) {
	s.AddTool(&mcp.Tool{
		Name:        "gen",
		Description: "Returns one image",
		InputSchema: json.RawMessage(`{"type":"object"}`),
	}, func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		return &mcp.CallToolResult{Content: []mcp.Content{
			&mcp.ImageContent{Data: []byte("PNGDATA"), MIMEType: "image/png"},
		}}, nil
	})
}

// addTwoImageTool registers a tool whose result is two image content blocks,
// so --outfile must number the saved files.
func addTwoImageTool(s *mcp.Server) {
	s.AddTool(&mcp.Tool{
		Name:        "twogen",
		Description: "Returns two images",
		InputSchema: json.RawMessage(`{"type":"object"}`),
	}, func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		return &mcp.CallToolResult{Content: []mcp.Content{
			&mcp.ImageContent{Data: []byte("PNGDATA1"), MIMEType: "image/png"},
			&mcp.ImageContent{Data: []byte("PNGDATA2"), MIMEType: "image/png"},
		}}, nil
	})
}

// TestToolsCallOutfile: --outfile saves the image block's decoded bytes to a
// file with the MIME-derived extension, prints the summary line on stdout and
// the wrote notice on stderr, and a --plain run stays byte-ASCII.
func TestToolsCallOutfile(t *testing.T) {
	srv := newTestMCPServerWithTools(t, addImageTool)
	tmp := t.TempDir()
	base := filepath.Join(tmp, "img")

	stdout, stderr, _, err := runRoot(t, "--url", srv.URL, "tools", "call", "gen", "--outfile", base)
	if err != nil {
		t.Fatalf("tools call --outfile: %v", err)
	}
	got, err := os.ReadFile(base + ".png")
	if err != nil {
		t.Fatalf("read img.png: %v", err)
	}
	if string(got) != "PNGDATA" {
		t.Errorf("img.png = %q, want PNGDATA", got)
	}
	if !strings.Contains(stdout, "[image/png content, 7 bytes]") {
		t.Errorf("stdout %q lacks the image summary line", stdout)
	}
	if !strings.Contains(stderr, "wrote") || !strings.Contains(stderr, "(7 bytes)") {
		t.Errorf("stderr %q lacks the wrote notice with the byte count", stderr)
	}

	// Plain-mode purity: the same run under --plain writes no non-ASCII or
	// escape byte to stdout.
	stdout, _, _, err = runRoot(t, "--url", srv.URL, "--plain", "tools", "call", "gen", "--outfile", filepath.Join(tmp, "plain"))
	if err != nil {
		t.Fatalf("tools call --outfile --plain: %v", err)
	}
	for i := 0; i < len(stdout); i++ {
		if b := stdout[i]; b >= 0x80 || b == 0x1b {
			t.Fatalf("plain stdout has byte 0x%02x at offset %d: %q", b, i, stdout)
		}
	}
}

// TestToolsCallOutfileMultiple: two image blocks land in numbered files, each
// with its own exact bytes.
func TestToolsCallOutfileMultiple(t *testing.T) {
	srv := newTestMCPServerWithTools(t, addTwoImageTool)
	tmp := t.TempDir()
	base := filepath.Join(tmp, "img")

	_, _, _, err := runRoot(t, "--url", srv.URL, "tools", "call", "twogen", "--outfile", base)
	if err != nil {
		t.Fatalf("tools call twogen --outfile: %v", err)
	}
	for name, want := range map[string]string{
		"img.1.png": "PNGDATA1",
		"img.2.png": "PNGDATA2",
	} {
		got, err := os.ReadFile(filepath.Join(tmp, name))
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		if string(got) != want {
			t.Errorf("%s = %q, want %q", name, got, want)
		}
	}
}

// TestToolsCallOutfileNoMedia: --outfile on a text-only tool result is an
// error (exit 1) and no file is created.
func TestToolsCallOutfileNoMedia(t *testing.T) {
	srv := newTestMCPServerWithTools(t, addEchoTool)
	tmp := t.TempDir()

	_, _, _, err := runRoot(t, "--url", srv.URL, "tools", "call", "echo", "--outfile", filepath.Join(tmp, "img"))
	if err == nil || !strings.Contains(err.Error(), "no image or audio content") {
		t.Fatalf("text-only call with --outfile = %v, want the no-media error", err)
	}
	entries, err := os.ReadDir(tmp)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Errorf("no-media call created files: %v", entries)
	}
}

// TestToolsCallOutfileJSON: in --json mode the full CallToolResult document
// still reaches stdout (base64 data included) and the file is written with the
// decoded bytes.
func TestToolsCallOutfileJSON(t *testing.T) {
	srv := newTestMCPServerWithTools(t, addImageTool)
	tmp := t.TempDir()
	base := filepath.Join(tmp, "img")

	stdout, _, _, err := runRoot(t, "--url", srv.URL, "--json", "tools", "call", "gen", "--outfile", base)
	if err != nil {
		t.Fatalf("tools call --json --outfile: %v", err)
	}
	var res map[string]any
	if err := json.Unmarshal([]byte(stdout), &res); err != nil {
		t.Fatalf("tools call --json --outfile stdout %q is not one JSON document: %v", stdout, err)
	}
	if !strings.Contains(stdout, "UE5HREFUQQ==") { // base64("PNGDATA")
		t.Errorf("json stdout %q lacks the base64 image data", stdout)
	}
	got, err := os.ReadFile(base + ".png")
	if err != nil {
		t.Fatalf("read img.png: %v", err)
	}
	if string(got) != "PNGDATA" {
		t.Errorf("img.png = %q, want the decoded PNGDATA", got)
	}
}
