package cli

import (
	"context"
	"maps"
	"slices"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// addGreetPrompt registers a prompt that echoes the arguments it received as
// "key=value" pairs sorted by key, so tests can assert exactly which string
// values reached the server.
func addGreetPrompt(s *mcp.Server) {
	s.AddPrompt(&mcp.Prompt{
		Name:        "greet",
		Description: "Greets a named subject",
		Arguments: []*mcp.PromptArgument{
			{Name: "name", Description: "who to greet", Required: true},
			{Name: "count", Description: "how many greetings"},
		},
	}, func(ctx context.Context, req *mcp.GetPromptRequest) (*mcp.GetPromptResult, error) {
		parts := make([]string, 0, len(req.Params.Arguments))
		for _, k := range slices.Sorted(maps.Keys(req.Params.Arguments)) {
			parts = append(parts, k+"="+req.Params.Arguments[k])
		}
		return &mcp.GetPromptResult{
			Messages: []*mcp.PromptMessage{{
				Role:    mcp.Role("user"),
				Content: &mcp.TextContent{Text: strings.Join(parts, " ")},
			}},
		}, nil
	})
}

// TestPromptsListE2E: a server with one prompt lists its name and description.
func TestPromptsListE2E(t *testing.T) {
	srv := newTestMCPServerWithTools(t, addGreetPrompt)

	stdout, _, _, err := runRoot(t, "--url", srv.URL, "prompts", "list")
	if err != nil {
		t.Fatalf("prompts list: %v", err)
	}
	if !strings.Contains(stdout, "greet") {
		t.Errorf("prompts list stdout %q does not name the prompt greet", stdout)
	}
	if !strings.Contains(stdout, "Greets a named subject") {
		t.Errorf("prompts list stdout %q does not include the prompt description", stdout)
	}
}

// TestPromptsGetE2E: getting a prompt with --arg pairs sends the values as raw
// strings and the server echoes them verbatim. A JSON-decoded value (count=3
// as the number 3) would fail this round trip: the SDK unmarshals prompt
// arguments into map[string]string, so a number on the wire is an error.
func TestPromptsGetE2E(t *testing.T) {
	srv := newTestMCPServerWithTools(t, addGreetPrompt)

	stdout, _, _, err := runRoot(t, "--url", srv.URL, "prompts", "get", "greet", "--arg", "name=world", "--arg", "count=3")
	if err != nil {
		t.Fatalf("prompts get: %v", err)
	}
	if !strings.Contains(stdout, "name=world") {
		t.Errorf("prompts get stdout %q does not echo name=world", stdout)
	}
	if !strings.Contains(stdout, "count=3") {
		t.Errorf("prompts get stdout %q does not echo count=3 as the raw string \"3\"", stdout)
	}
}

// TestPromptsGetBadPairFailsFast: a malformed --arg pair is rejected before
// any connection is opened.
func TestPromptsGetBadPairFailsFast(t *testing.T) {
	_, _, _, err := runRoot(t, "prompts", "get", "greet", "--arg", "noequals")
	if err == nil || !strings.Contains(err.Error(), "noequals") {
		t.Fatalf("prompts get with bad pair = %v, want an error naming the pair", err)
	}
}
