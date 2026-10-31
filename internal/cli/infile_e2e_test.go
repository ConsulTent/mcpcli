package cli

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// fixtureText returns the committed Gutenberg sample's exact content — the
// same file the README example points at, so these tests pin what users run.
func fixtureText(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", "gutenberg-sample.txt"))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// decodeEchoResult decodes the --json CallToolResult then the echo tool's
// inner JSON, returning the arguments the server received. Assertions decode
// rather than substring-match raw bytes: JSON escaping of quotes and dashes
// would make byte matching fragile.
func decodeEchoResult(t *testing.T, stdout string) map[string]any {
	t.Helper()
	var res struct {
		Content []struct {
			Text string `json:"text"`
		} `json:"content"`
	}
	if err := json.Unmarshal([]byte(stdout), &res); err != nil {
		t.Fatalf("decode CallToolResult: %v\nstdout: %s", err, stdout)
	}
	if len(res.Content) == 0 {
		t.Fatalf("no content in result: %s", stdout)
	}
	var args map[string]any
	if err := json.Unmarshal([]byte(res.Content[0].Text), &args); err != nil {
		t.Fatalf("decode echoed args: %v\ntext: %s", err, res.Content[0].Text)
	}
	return args
}

// TestToolsCallInFileE2E: the committed fixture's full content reaches the
// server as a raw-string argument, byte-identical after the JSON round trip,
// with a read notice on stderr and no decoration leakage into stdout.
func TestToolsCallInFileE2E(t *testing.T) {
	srv := newTestMCPServerWithTools(t, addEchoTool)
	fixture := filepath.Join("testdata", "gutenberg-sample.txt")

	stdout, stderr, _, err := runRoot(t, "--json", "--url", srv.URL, "tools", "call", "echo", "--in-file", "msg="+fixture)
	if err != nil {
		t.Fatalf("tools call: %v\nstderr: %s", err, stderr)
	}
	if got := decodeEchoResult(t, stdout)["msg"]; got != fixtureText(t) {
		t.Fatalf("msg = %q…, want the exact fixture content", truncateAny(got, 60))
	}
	if !strings.Contains(stderr, "read "+fixture+" (") || !strings.Contains(stderr, " bytes)") {
		t.Fatalf("stderr missing read notice: %q", stderr)
	}
	if strings.Contains(stdout, "read ") {
		t.Fatalf("notice leaked into --json stdout: %q", stdout)
	}
}

// TestToolsCallInFilePrecedenceE2E pins the source order: -A loads first,
// --in-file second, --arg last (overriding everything per key).
func TestToolsCallInFilePrecedenceE2E(t *testing.T) {
	srv := newTestMCPServerWithTools(t, addEchoTool)
	dir := t.TempDir()
	argsFile := filepath.Join(dir, "a.json")
	if err := os.WriteFile(argsFile, []byte(`{"k": "fromA"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	inFile := filepath.Join(dir, "f.txt")
	if err := os.WriteFile(inFile, []byte("fromFile"), 0o644); err != nil {
		t.Fatal(err)
	}

	run := func(extra ...string) map[string]any {
		t.Helper()
		args := append([]string{"--json", "--url", srv.URL, "tools", "call", "echo", "-A", argsFile, "--in-file", "k=" + inFile}, extra...)
		stdout, _, _, err := runRoot(t, args...)
		if err != nil {
			t.Fatalf("tools call: %v", err)
		}
		return decodeEchoResult(t, stdout)
	}
	if got := run()["k"]; got != "fromFile" {
		t.Fatalf("--in-file should beat -A: got %v", got)
	}
	if got := run("--arg", "k=fromArg")["k"]; got != "fromArg" {
		t.Fatalf("--arg should beat --in-file: got %v", got)
	}
}

// TestToolsCallInFileBareRejected: tools call has no primary text input.
func TestToolsCallInFileBareRejected(t *testing.T) {
	srv := newTestMCPServerWithTools(t, addEchoTool)
	_, _, _, err := runRoot(t, "--url", srv.URL, "tools", "call", "echo", "--in-file", "plain.txt")
	if err == nil || !strings.Contains(err.Error(), "no primary text input") {
		t.Fatalf("err = %v, want bare rejection", err)
	}
}

// TestPromptsGetInFileE2E: the prompt receives the file's content as a raw
// string argument; --arg still overrides per key.
func TestPromptsGetInFileE2E(t *testing.T) {
	var got map[string]string
	srv := newTestMCPServerWithTools(t, func(s *mcp.Server) {
		s.AddPrompt(&mcp.Prompt{Name: "echo", Description: "Echoes its arguments"}, func(ctx context.Context, req *mcp.GetPromptRequest) (*mcp.GetPromptResult, error) {
			got = map[string]string{}
			for k, v := range req.Params.Arguments {
				got[k] = v
			}
			return &mcp.GetPromptResult{Description: "echoed"}, nil
		})
	})
	inFile := filepath.Join(t.TempDir(), "task.txt")
	if err := os.WriteFile(inFile, []byte(fixtureText(t)), 0o644); err != nil {
		t.Fatal(err)
	}

	if _, _, _, err := runRoot(t, "--url", srv.URL, "prompts", "get", "echo", "--in-file", "task="+inFile); err != nil {
		t.Fatal(err)
	}
	if got["task"] != fixtureText(t) {
		t.Fatalf("task = %q…, want the exact fixture content", truncateAny(got["task"], 60))
	}
	if _, _, _, err := runRoot(t, "--url", srv.URL, "prompts", "get", "echo", "--in-file", "task="+inFile, "--arg", "task=flag"); err != nil {
		t.Fatal(err)
	}
	if got["task"] != "flag" {
		t.Fatalf("--arg should override --in-file: got %q", got["task"])
	}
}

// addJkudishCapture registers a minimal jkudish-flavor pair (detection needs
// two of the four core tools): the tool under test, whose handler records
// the raw arguments of the call, plus a canned bystander. Each answers with
// the doc its adapter parses.
func addJkudishCapture(t *testing.T, underTest string, captured *map[string]any) func(*mcp.Server) {
	t.Helper()
	docs := map[string]string{
		"jev_noul":   `{"probability": 0.97, "label": "likely"}`,
		"jev_screen": `{"probabilities": {"injection": 0.01, "substance": 0.97, "relevance": 0.97}, "recommendation": {"action": "pass", "reason": "clean"}}`,
		"jev_verify": `{"summary": {"verified": 1}, "results": [{"claim": "c", "verdict": "verified", "confidence": 0.9, "action": "auto"}]}`,
	}
	return func(s *mcp.Server) {
		text := func(doc string) *mcp.CallToolResult {
			return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: doc}}}
		}
		bystander := "jev_noul"
		if underTest == bystander {
			bystander = "jev_screen"
		}
		s.AddTool(&mcp.Tool{Name: bystander, InputSchema: json.RawMessage(`{"type":"object"}`)},
			func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
				return text(docs[bystander]), nil
			})
		s.AddTool(&mcp.Tool{Name: underTest, InputSchema: json.RawMessage(`{"type":"object"}`)},
			func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
				var args map[string]any
				if len(req.Params.Arguments) > 0 {
					if err := json.Unmarshal(req.Params.Arguments, &args); err != nil {
						return nil, err
					}
				}
				*captured = args
				return text(docs[underTest]), nil
			})
	}
}

// TestJevScreenInFileE2E: the bare --in-file replaces TEXT and purpose=
// fills the purpose (explicit flag wins); the file's full content reaches
// jev_screen verbatim.
func TestJevScreenInFileE2E(t *testing.T) {
	fixture := filepath.Join("testdata", "gutenberg-sample.txt")
	purposeFile := filepath.Join(t.TempDir(), "purpose.txt")
	if err := os.WriteFile(purposeFile, []byte("summarization"), 0o644); err != nil {
		t.Fatal(err)
	}

	var captured map[string]any
	srv := newTestMCPServerWithTools(t, addJkudishCapture(t, "jev_screen", &captured))
	if _, _, _, err := runRoot(t, "--url", srv.URL, "jev", "screen", "--in-file", fixture, "--in-file", "purpose="+purposeFile); err != nil {
		t.Fatal(err)
	}
	if captured["text"] != fixtureText(t) {
		t.Fatalf("jev_screen text = %q…, want the exact fixture content", truncateAny(captured["text"], 60))
	}
	if captured["purpose"] != "summarization" {
		t.Fatalf("purpose = %v", captured["purpose"])
	}

	// An explicit --purpose beats the purpose= file.
	if _, _, _, err := runRoot(t, "--url", srv.URL, "jev", "screen", "--in-file", fixture, "--in-file", "purpose="+purposeFile, "--purpose", "code review"); err != nil {
		t.Fatal(err)
	}
	if captured["purpose"] != "code review" {
		t.Fatalf("explicit --purpose should win: got %v", captured["purpose"])
	}
}

// TestJevAskInFileE2E: bare file question + context= file; positional and
// bare --in-file are mutually exclusive; the 0-arg error keeps cobra's
// wording without --in-file.
func TestJevAskInFileE2E(t *testing.T) {
	fixture := filepath.Join("testdata", "gutenberg-sample.txt")
	contextFile := filepath.Join(t.TempDir(), "ctx.txt")
	if err := os.WriteFile(contextFile, []byte("CI log context"), 0o644); err != nil {
		t.Fatal(err)
	}

	var captured map[string]any
	srv := newTestMCPServerWithTools(t, addJkudishCapture(t, "jev_noul", &captured))
	if _, _, _, err := runRoot(t, "--url", srv.URL, "jev", "ask", "--in-file", fixture, "--in-file", "context="+contextFile); err != nil {
		t.Fatal(err)
	}
	props, _ := captured["propositions"].([]any)
	if len(props) != 1 || props[0] != fixtureText(t) {
		t.Fatalf("propositions = %v, want the fixture as the sole proposition", captured["propositions"])
	}
	if captured["context"] != "CI log context" {
		t.Fatalf("context = %v", captured["context"])
	}

	_, _, _, err := runRoot(t, "--url", srv.URL, "jev", "ask", "both?", "--in-file", fixture)
	if err == nil || !strings.Contains(err.Error(), "mutually exclusive") {
		t.Fatalf("err = %v, want positional/--in-file XOR", err)
	}
	_, _, _, err = runRoot(t, "--url", srv.URL, "jev", "ask")
	if err == nil || !strings.Contains(err.Error(), "accepts 1 arg(s)") {
		t.Fatalf("err = %v, want cobra's exact-args message preserved", err)
	}
}

// TestJevVerifyInFileE2E: claim=/evidence= files satisfy the required slots
// on their own and append after flag values.
func TestJevVerifyInFileE2E(t *testing.T) {
	dir := t.TempDir()
	claimFile := filepath.Join(dir, "claim.txt")
	evidenceFile := filepath.Join(dir, "evidence.txt")
	if err := os.WriteFile(claimFile, []byte("tests pass"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(evidenceFile, []byte("go test: ok"), 0o644); err != nil {
		t.Fatal(err)
	}

	var captured map[string]any
	srv := newTestMCPServerWithTools(t, addJkudishCapture(t, "jev_verify", &captured))
	if _, _, _, err := runRoot(t, "--url", srv.URL, "jev", "verify", "--in-file", "claim="+claimFile, "--in-file", "evidence="+evidenceFile); err != nil {
		t.Fatal(err)
	}
	if claims, _ := captured["claims"].([]any); len(claims) != 1 || claims[0] != "tests pass" {
		t.Fatalf("claims = %v", captured["claims"])
	}
	if ev, _ := captured["evidence"].([]any); len(ev) != 1 {
		t.Fatalf("evidence = %v", captured["evidence"])
	} else if m, _ := ev[0].(map[string]any); m["text"] != "go test: ok" {
		t.Fatalf("evidence[0] = %v", ev[0])
	}

	// Flag values come first, file values append after.
	if _, _, _, err := runRoot(t, "--url", srv.URL, "jev", "verify", "--claim", "flag claim", "--in-file", "claim="+claimFile, "--in-file", "evidence="+evidenceFile); err != nil {
		t.Fatal(err)
	}
	if claims, _ := captured["claims"].([]any); len(claims) != 2 || claims[0] != "flag claim" || claims[1] != "tests pass" {
		t.Fatalf("claims = %v, want flag value then file value", captured["claims"])
	}
}

// TestInFileMissingFailsFast: a missing --in-file fails before any
// connection is attempted (the URL points nowhere).
func TestInFileMissingFailsFast(t *testing.T) {
	_, _, _, err := runRoot(t, "--url", "http://127.0.0.1:1/mcp", "tools", "call", "echo", "--in-file", "k=definitely-missing.txt")
	if err == nil || !strings.Contains(err.Error(), `--in-file "definitely-missing.txt":`) {
		t.Fatalf("err = %v, want the named path in a pre-connect failure", err)
	}
}

func truncateAny(v any, n int) string {
	s, ok := v.(string)
	if !ok {
		s = "<non-string>"
	}
	if len(s) > n {
		return s[:n]
	}
	return s
}
