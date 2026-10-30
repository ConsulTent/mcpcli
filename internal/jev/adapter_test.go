package jev

import (
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// tools builds a tool inventory by name, the shape ListTools returns.
func tools(names ...string) []*mcp.Tool {
	out := make([]*mcp.Tool, len(names))
	for i, n := range names {
		out[i] = &mcp.Tool{Name: n}
	}
	return out
}

// TestAdapterFor pins detection order and the matched/unmatched split: the
// first signature hit wins, the winner's vocabulary partitions the server's
// jev_* tools, and a server no flavor recognizes yields a nil adapter with
// every jev_* tool unmatched.
func TestAdapterFor(t *testing.T) {
	tests := []struct {
		name       string
		inventory  []*mcp.Tool
		wantAdapt  string // "" means none
		wantMatch  []string
		wantUnmatc []string
	}{
		{
			name:      "jkudish full inventory",
			inventory: tools("jev_noul", "jev_verify", "jev_screen", "jev_decide", "jev_classify", "jev_compare", "jev_find", "jev_rerank", "jev_extract", "jev_audit", "jev_review", "jev_gate"),
			wantAdapt: "jkudish",
			wantMatch: []string{"jev_noul", "jev_verify", "jev_screen", "jev_decide", "jev_classify", "jev_compare", "jev_find", "jev_rerank", "jev_extract", "jev_audit", "jev_review", "jev_gate"},
		},
		{
			name:      "jkudish two-tool signature is enough",
			inventory: tools("jev_noul", "jev_verify"),
			wantAdapt: "jkudish",
			wantMatch: []string{"jev_noul", "jev_verify"},
		},
		{
			name:       "single shared name does not detect jkudish",
			inventory:  tools("jev_noul", "unrelated_tool"),
			wantAdapt:  "",
			wantUnmatc: []string{"jev_noul"},
		},
		{
			name:      "jev-use",
			inventory: tools("jev_judge", "jev_gate", "echo"),
			wantAdapt: "jev-use",
			// jev_gate is known to the flavor (its PreToolUse gate) but is
			// not part of judge, so both its tools are matched.
			wantMatch: []string{"jev_judge", "jev_gate"},
		},
		{
			name:      "legostin",
			inventory: tools("jev_task", "jev_doctor", "jev_ask", "jev_tabs"),
			wantAdapt: "legostin",
			wantMatch: []string{"jev_doctor"},
			// Browser-driving tools are known unknowns: reported, not probed.
			wantUnmatc: []string{"jev_task", "jev_ask", "jev_tabs"},
		},
		{
			name:       "no jev tools at all",
			inventory:  tools("echo", "add"),
			wantAdapt:  "",
			wantUnmatc: nil,
		},
		{
			name:       "unknown jev tool only",
			inventory:  tools("jev_mystery"),
			wantAdapt:  "",
			wantUnmatc: []string{"jev_mystery"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			a, matched, unmatched := AdapterFor(tt.inventory)
			if tt.wantAdapt == "" {
				if a != nil {
					t.Fatalf("adapter = %q, want none", a.Name())
				}
			} else if a == nil || a.Name() != tt.wantAdapt {
				got := "none"
				if a != nil {
					got = a.Name()
				}
				t.Fatalf("adapter = %q, want %q", got, tt.wantAdapt)
			}
			if !equalStrings(matched, tt.wantMatch) {
				t.Fatalf("matched = %v, want %v", matched, tt.wantMatch)
			}
			if !equalStrings(unmatched, tt.wantUnmatc) {
				t.Fatalf("unmatched = %v, want %v", unmatched, tt.wantUnmatc)
			}
		})
	}
}

// TestAdapterForAmbiguousBothSignatures pins the tie-break: a server
// exposing both flavors' signatures resolves to the earlier flavor in
// detection order (jkudish), with the jev-use tool reported unmatched.
func TestAdapterForAmbiguousBothSignatures(t *testing.T) {
	a, _, unmatched := AdapterFor(tools("jev_noul", "jev_verify", "jev_judge"))
	if a == nil || a.Name() != "jkudish" {
		t.Fatalf("adapter = %v, want jkudish (first in detection order)", a)
	}
	if !equalStrings(unmatched, []string{"jev_judge"}) {
		t.Fatalf("unmatched = %v, want [jev_judge]", unmatched)
	}
}

func equalStrings(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}

// TestResultPayload pins the content precedence: StructuredContent wins over
// text, text must be JSON, and the payload must be an object — Parse never
// inspects content blocks unchecked.
func TestResultPayload(t *testing.T) {
	t.Run("structured content wins", func(t *testing.T) {
		res := &mcp.CallToolResult{
			StructuredContent: map[string]any{"decision": "deny"},
			Content:           []mcp.Content{&mcp.TextContent{Text: `{"decision":"allow"}`}},
		}
		p, err := resultPayload(res)
		if err != nil {
			t.Fatalf("resultPayload: %v", err)
		}
		if p["decision"] != "deny" {
			t.Fatalf("decision = %v, want deny (structured content must win)", p["decision"])
		}
	})
	t.Run("text fallback", func(t *testing.T) {
		res := &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: `{"answer": 0.9}`}}}
		p, err := resultPayload(res)
		if err != nil {
			t.Fatalf("resultPayload: %v", err)
		}
		if p["answer"] != 0.9 {
			t.Fatalf("answer = %v, want 0.9", p["answer"])
		}
	})
	t.Run("non-json text is an error", func(t *testing.T) {
		res := &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "not json"}}}
		if _, err := resultPayload(res); err == nil {
			t.Fatal("resultPayload succeeded on non-JSON text, want error")
		}
	})
	t.Run("empty result is an error", func(t *testing.T) {
		if _, err := resultPayload(&mcp.CallToolResult{}); err == nil {
			t.Fatal("resultPayload succeeded on empty result, want error")
		}
	})
	t.Run("array payload is an error", func(t *testing.T) {
		res := &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: `[1,2]`}}}
		if _, err := resultPayload(res); err == nil {
			t.Fatal("resultPayload succeeded on a JSON array, want error")
		}
	})
}
