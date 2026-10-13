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
