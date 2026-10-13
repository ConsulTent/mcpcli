package cli

import (
	"errors"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// TestFallbackMethod pins the fallback precedence: the first declared
// capability in tools/list -> resources/list -> prompts/list order wins, and
// a server declaring none has no fallback.
func TestFallbackMethod(t *testing.T) {
	tests := []struct {
		name string
		caps *mcp.ServerCapabilities
		want string
	}{
		{"none", nil, ""},
		{"tools_only", &mcp.ServerCapabilities{Tools: &mcp.ToolCapabilities{}}, "tools/list"},
		{"resources_only", &mcp.ServerCapabilities{Resources: &mcp.ResourceCapabilities{}}, "resources/list"},
		{"prompts_only", &mcp.ServerCapabilities{Prompts: &mcp.PromptCapabilities{}}, "prompts/list"},
		{"precedence", &mcp.ServerCapabilities{
			Prompts:   &mcp.PromptCapabilities{},
			Resources: &mcp.ResourceCapabilities{},
			Tools:     &mcp.ToolCapabilities{},
		}, "tools/list"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := fallbackMethod(tt.caps); got != tt.want {
				t.Errorf("fallbackMethod(%v) = %q, want %q", tt.caps, got, tt.want)
			}
		})
	}
}

// TestIsMethodNotFound: the detector matches the real wire error shape for a
// missing ping method (as observed against mcp.svelte.dev) and nothing
// broader.
func TestIsMethodNotFound(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{
			name: "ping_method_missing",
			err:  errors.New(`calling "ping": sending "ping": Method ping not found: rejected by transport: Not Found`),
			want: true,
		},
		{name: "nil", err: nil, want: false},
		{name: "other_method_missing", err: errors.New(`calling "tools/list": Method tools/list not found`), want: false},
		{name: "header_mismatch", err: errors.New("MCP error -32020: Header mismatch"), want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isMethodNotFound(tt.err); got != tt.want {
				t.Errorf("isMethodNotFound(%v) = %v, want %v", tt.err, got, tt.want)
			}
		})
	}
}
