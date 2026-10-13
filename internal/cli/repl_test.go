package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/ConsulTent/mcpcli/internal/connection"
	"github.com/ConsulTent/mcpcli/internal/ui"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// replTestFlags is the GlobalFlags the direct runRepl tests run with: only the
// per-command timeout matters there, the ui mode is driven by driveRepl.
func replTestFlags() *GlobalFlags {
	return &GlobalFlags{Timeout: 5 * time.Second}
}

// openTestSession connects a real client session to srv over streamable HTTP,
// the same topology the one-shot command tests use, and closes it at cleanup.
func openTestSession(t *testing.T, srv *httptest.Server) *mcp.ClientSession {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	session, err := connection.Open(ctx, &connection.Options{URL: srv.URL, Auth: "none", ProtocolVersion: "auto"})
	if err != nil {
		t.Fatalf("open session: %v", err)
	}
	t.Cleanup(func() { _ = session.Close() })
	return session
}

// driveRepl feeds input to runRepl against session in the given ui mode
// (restored afterwards) and returns what the loop wrote to each stream.
func driveRepl(t *testing.T, session *mcp.ClientSession, input string, mode ui.Mode) (stdout, stderr string, err error) {
	t.Helper()
	old := ui.Current
	ui.Current = mode
	defer func() { ui.Current = old }()

	var out, errBuf bytes.Buffer
	err = runRepl(context.Background(), strings.NewReader(input), &out, &errBuf, session, replTestFlags())
	return out.String(), errBuf.String(), err
}

// addGreetPromptAndNotesResource registers one prompt (one string argument)
// and one text resource, so the REPL's prompt and resource commands have
// something to show.
func addGreetPromptAndNotesResource(s *mcp.Server) {
	s.AddPrompt(&mcp.Prompt{
		Name:        "greet",
		Description: "Greets someone by name",
		Arguments:   []*mcp.PromptArgument{{Name: "name", Description: "who to greet"}},
	}, func(ctx context.Context, req *mcp.GetPromptRequest) (*mcp.GetPromptResult, error) {
		return &mcp.GetPromptResult{
			Messages: []*mcp.PromptMessage{{
				Role:    "user",
				Content: &mcp.TextContent{Text: "Hello, " + req.Params.Arguments["name"] + "!"},
			}},
		}, nil
	})
	s.AddResource(&mcp.Resource{
		URI:         "file:///notes.txt",
		Name:        "notes",
		Description: "Some notes",
	}, func(ctx context.Context, req *mcp.ReadResourceRequest) (*mcp.ReadResourceResult, error) {
		return &mcp.ReadResourceResult{
			Contents: []*mcp.ResourceContents{{URI: req.Params.URI, Text: "note body"}},
		}, nil
	})
}

// TestReplEOF: an empty stdin is a clean exit — nil error, no result output,
// and exactly the one prompt printed before the first (and only) read.
func TestReplEOF(t *testing.T) {
	session := openTestSession(t, newTestMCPServer(t))

	stdout, stderr, err := driveRepl(t, session, "", ui.Rich)
	if err != nil {
		t.Fatalf("repl on EOF = %v, want nil", err)
	}
	if stdout != "" {
		t.Errorf("EOF stdout = %q, want empty", stdout)
	}
	if stderr != "mcp> " {
		t.Errorf("EOF stderr = %q, want exactly one %q prompt", stderr, replPrompt)
	}
}

// TestReplToolsCommand: "tools" lists the server's tools on stdout and the
// prompt precedes every read, including the final "exit".
func TestReplToolsCommand(t *testing.T) {
	session := openTestSession(t, newTestMCPServerWithTools(t, addEchoTool))

	stdout, stderr, err := driveRepl(t, session, "tools\nexit\n", ui.Rich)
	if err != nil {
		t.Fatalf("repl tools+exit: %v", err)
	}
	if !strings.Contains(stdout, "echo") {
		t.Errorf("tools stdout %q does not name the tool echo", stdout)
	}
	if !strings.Contains(stdout, "Echoes its arguments as JSON text") {
		t.Errorf("tools stdout %q does not include the tool description", stdout)
	}
	if strings.Contains(stderr, "❌") {
		t.Errorf("tools stderr %q reports an error", stderr)
	}
	if got := strings.Count(stderr, replPrompt); got != 2 {
		t.Errorf("stderr %q has %d prompts, want one before each of the 2 reads", stderr, got)
	}
}

// TestReplUnknownCommand: an unrecognized line reports the hint on stderr and
// the loop keeps serving commands.
func TestReplUnknownCommand(t *testing.T) {
	session := openTestSession(t, newTestMCPServerWithTools(t, addEchoTool))

	stdout, stderr, err := driveRepl(t, session, "bogus\ntools\nexit\n", ui.Rich)
	if err != nil {
		t.Fatalf("repl bogus+tools+exit: %v", err)
	}
	if !strings.Contains(stderr, "unknown command, try help") {
		t.Errorf("stderr %q does not carry the unknown-command hint", stderr)
	}
	if !strings.Contains(stdout, "echo") {
		t.Errorf("stdout %q does not list the tools after the unknown command; the loop did not continue", stdout)
	}
}

// TestReplHelpListsCommands: help prints the command list to stderr (the
// results stream stays clean) and names every implemented command.
func TestReplHelpListsCommands(t *testing.T) {
	session := openTestSession(t, newTestMCPServer(t))

	stdout, stderr, err := driveRepl(t, session, "help\nexit\n", ui.Rich)
	if err != nil {
		t.Fatalf("repl help+exit: %v", err)
	}
	if stdout != "" {
		t.Errorf("help stdout = %q, want empty (help goes to stderr)", stdout)
	}
	for _, want := range []string{"tools", "call NAME", "resources", "read URI", "prompts", "get NAME", "info", "ping", "exit", "quit", "quoting"} {
		if !strings.Contains(stderr, want) {
			t.Errorf("help output %q does not mention %q", stderr, want)
		}
	}
}

// TestReplCallWithJSONObject: "call NAME {json}" decodes the third token as
// the tool's argument object.
func TestReplCallWithJSONObject(t *testing.T) {
	session := openTestSession(t, newTestMCPServerWithTools(t, addEchoTool))

	stdout, _, err := driveRepl(t, session, "call echo {\"msg\":\"hi\",\"count\":3}\nexit\n", ui.Rich)
	if err != nil {
		t.Fatalf("repl call+exit: %v", err)
	}
	if !strings.Contains(stdout, `"msg":"hi"`) {
		t.Errorf("call stdout %q does not echo msg=hi", stdout)
	}
	if !strings.Contains(stdout, `"count":3`) {
		t.Errorf("call stdout %q does not carry count as the number 3", stdout)
	}
}

// TestReplBadLinesKeepLoopAlive: malformed input and failing operations each
// report one error line on stderr and never kill the loop.
func TestReplBadLinesKeepLoopAlive(t *testing.T) {
	session := openTestSession(t, newTestMCPServerWithTools(t, addEchoTool))

	input := "call\n" + // missing NAME
		"call echo notjson\n" + // arguments token is not JSON
		"call echo [1,2]\n" + // arguments token is not an object
		"call nosuchtool {\"a\":1}\n" + // the server rejects the call
		"get\n" + // missing NAME
		"get greet {\"name\":{\"deep\":true}}\n" + // prompt argument is not a scalar
		"read\n" + // missing URI
		"tools\nexit\n"
	stdout, stderr, err := driveRepl(t, session, input, ui.Rich)
	if err != nil {
		t.Fatalf("repl with bad lines: %v", err)
	}
	if !strings.Contains(stdout, "echo") {
		t.Errorf("stdout %q does not list the tools after the bad lines; the loop did not survive", stdout)
	}
	for _, want := range []string{
		"usage: call NAME [JSON-OBJ]",
		"JSON object",
		"nosuchtool",
		"usage: get NAME [JSON-OBJ]",
		"scalar",
		"usage: read URI",
	} {
		if !strings.Contains(stderr, want) {
			t.Errorf("stderr %q does not report %q", stderr, want)
		}
	}
}

// TestReplResourcesPromptsReadGet: the resource and prompt commands work over
// a real session, and "get" stringifies scalar JSON arguments.
func TestReplResourcesPromptsReadGet(t *testing.T) {
	session := openTestSession(t, newTestMCPServerWithTools(t, addGreetPromptAndNotesResource))

	stdout, _, err := driveRepl(t, session,
		"resources\nread file:///notes.txt\nprompts\nget greet {\"name\":\"world\"}\nexit\n", ui.Rich)
	if err != nil {
		t.Fatalf("repl resources/read/prompts/get: %v", err)
	}
	if !strings.Contains(stdout, "file:///notes.txt") {
		t.Errorf("stdout %q does not list the notes resource", stdout)
	}
	if !strings.Contains(stdout, "note body") {
		t.Errorf("stdout %q does not carry the read resource body", stdout)
	}
	if !strings.Contains(stdout, "greet") {
		t.Errorf("stdout %q does not list the greet prompt", stdout)
	}
	if !strings.Contains(stdout, "Hello, world!") {
		t.Errorf("stdout %q does not carry the prompted message; scalar args were not applied", stdout)
	}
}

// TestReplPingAndInfo: ping reports the timed round trip and info reports the
// server's identity, mirroring the one-shot commands.
func TestReplPingAndInfo(t *testing.T) {
	session := openTestSession(t, newTestMCPServer(t))

	stdout, _, err := driveRepl(t, session, "ping\ninfo\nexit\n", ui.Rich)
	if err != nil {
		t.Fatalf("repl ping/info/exit: %v", err)
	}
	if !strings.Contains(stdout, "pong") {
		t.Errorf("ping stdout %q does not report pong", stdout)
	}
	if !strings.Contains(stdout, "testserver") {
		t.Errorf("info stdout %q does not name the server", stdout)
	}
}

// TestReplPlainMode: plain mode output stays ASCII, mirroring the one-shot
// ping line.
func TestReplPlainMode(t *testing.T) {
	session := openTestSession(t, newTestMCPServer(t))

	stdout, _, err := driveRepl(t, session, "ping\nexit\n", ui.Plain)
	if err != nil {
		t.Fatalf("repl plain ping/exit: %v", err)
	}
	if !strings.Contains(stdout, "pong:") {
		t.Errorf("plain ping stdout %q does not use the plain pong line", stdout)
	}
	for i := 0; i < len(stdout); i++ {
		if stdout[i] >= 0x80 || stdout[i] == 0x1b {
			t.Fatalf("plain stdout %q has a non-ASCII or escape byte at %d", stdout, i)
		}
	}
}

// TestReplJSONMode: no prompt anywhere, every result is one JSON document on
// stdout, and errors are JSON error documents on stderr.
func TestReplJSONMode(t *testing.T) {
	session := openTestSession(t, newTestMCPServerWithTools(t, addEchoTool))

	stdout, stderr, err := driveRepl(t, session, "tools\ncall echo {\"msg\":\"hi\"}\nbogus\nexit\n", ui.JSON)
	if err != nil {
		t.Fatalf("repl json mode: %v", err)
	}
	if strings.Contains(stderr, replPrompt) {
		t.Errorf("json mode stderr %q still carries the prompt", stderr)
	}

	// stdout is a stream of JSON documents, one per result-producing command.
	dec := json.NewDecoder(strings.NewReader(stdout))
	var docs []map[string]any
	for dec.More() {
		var doc map[string]any
		if err := dec.Decode(&doc); err != nil {
			t.Fatalf("json mode stdout %q is not a JSON document stream: %v", stdout, err)
		}
		docs = append(docs, doc)
	}
	if len(docs) != 2 {
		t.Fatalf("json mode stdout produced %d documents, want 2 (tools, call): %q", len(docs), stdout)
	}
	if _, ok := docs[0]["tools"]; !ok {
		t.Errorf("first document %q lacks the tools key", stdout)
	}
	content, ok := docs[1]["content"].([]any)
	if !ok || len(content) == 0 {
		t.Fatalf("second document in %q has no content blocks", stdout)
	}
	if block, ok := content[0].(map[string]any); !ok || !strings.Contains(block["text"].(string), `"msg":"hi"`) {
		t.Errorf("second document content[0] = %v in %q, want the echoed args", content[0], stdout)
	}

	// The unknown command became a JSON error document on stderr.
	var errDoc map[string]any
	if err := json.Unmarshal([]byte(stderr), &errDoc); err != nil {
		t.Fatalf("json mode stderr %q is not one JSON document: %v", stderr, err)
	}
	if errDoc["error"] != "unknown command, try help" {
		t.Errorf("json mode error document = %v, want the unknown-command hint", errDoc["error"])
	}
}

// TestInteractiveFlagRequiresNoSubcommand: -i is a whole-run mode — combining
// it with a subcommand is a usage error.
func TestInteractiveFlagRequiresNoSubcommand(t *testing.T) {
	_, _, _, err := runRoot(t, "-i", "tools")
	if err == nil || !strings.Contains(err.Error(), "--interactive") || !strings.Contains(err.Error(), "subcommand") {
		t.Fatalf("-i with a subcommand = %v, want a usage error naming --interactive", err)
	}

	_, _, _, err = runRoot(t, "-i", "auth", "login")
	if err == nil || !strings.Contains(err.Error(), "--interactive") {
		t.Fatalf("-i auth login = %v, want the same usage error", err)
	}
}

// TestInteractiveE2E: the full -i wiring connects through runWithConnection
// and runs the REPL against the session over the standard streams cobra sees.
func TestInteractiveE2E(t *testing.T) {
	srv := newTestMCPServerWithTools(t, addEchoTool)

	var out, errBuf bytes.Buffer
	restore := ui.SetSpinnerOutput(&bytes.Buffer{})
	defer restore()
	root := NewRootCommand()
	root.SetOut(&out)
	root.SetErr(&errBuf)
	root.SetIn(strings.NewReader("tools\nexit\n"))
	root.SetArgs([]string{"--url", srv.URL, "-i"})
	if err := root.Execute(); err != nil {
		t.Fatalf("-i repl: %v", err)
	}
	if !strings.Contains(out.String(), "echo") {
		t.Errorf("-i stdout %q does not list the tools", out.String())
	}
	if !strings.Contains(errBuf.String(), replPrompt) {
		t.Errorf("-i stderr %q does not carry the prompt", errBuf.String())
	}
}
