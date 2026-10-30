package jev

import (
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// textResult builds a successful tool result whose payload is JSON text.
func textResult(t *testing.T, doc string) *mcp.CallToolResult {
	t.Helper()
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: doc}}}
}

// TestJkudishDetect pins the ≥2-of-4 signature rule.
func TestJkudishDetect(t *testing.T) {
	a := jkudish{}
	tests := []struct {
		name      string
		inventory []*mcp.Tool
		want      bool
	}{
		{"full", tools("jev_noul", "jev_verify", "jev_screen", "jev_decide"), true},
		{"two", tools("jev_decide", "jev_screen"), true},
		{"one is not enough", tools("jev_noul"), false},
		{"none", tools("echo"), false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := a.Detect(tt.inventory); got != tt.want {
				t.Fatalf("Detect = %v, want %v", got, tt.want)
			}
		})
	}
}

// TestJkudishBuild pins the argument mapping for every supported op,
// including the threshold pass-through and the 2-6 candidate rule.
func TestJkudishBuild(t *testing.T) {
	a := jkudish{}
	t.Run("noul maps text and optional context and threshold", func(t *testing.T) {
		tool, args, err := a.Build(OpKindNoul, Question{Text: "2+2=4?", Context: "math", Threshold: 0.9, HasThreshold: true})
		if err != nil {
			t.Fatalf("Build: %v", err)
		}
		if tool != "jev_noul" {
			t.Fatalf("tool = %s, want jev_noul", tool)
		}
		if args["context"] != "math" {
			t.Fatalf("context = %v, want math", args["context"])
		}
		if args["auto_accept"] != 0.9 {
			t.Fatalf("auto_accept = %v, want 0.9", args["auto_accept"])
		}
		props, ok := args["propositions"].([]string)
		if !ok || len(props) != 1 || props[0] != "2+2=4?" {
			t.Fatalf("propositions = %v, want [2+2=4?]", args["propositions"])
		}
	})
	t.Run("noul without threshold omits auto_accept", func(t *testing.T) {
		_, args, err := a.Build(OpKindNoul, Question{Text: "x"})
		if err != nil {
			t.Fatalf("Build: %v", err)
		}
		if _, present := args["auto_accept"]; present {
			t.Fatal("auto_accept present without HasThreshold, want omitted")
		}
	})
	t.Run("choice builds ordered candidates", func(t *testing.T) {
		_, args, err := a.Build(OpKindChoice, Question{Text: "next?", Options: []string{"a", "b"}, Evidence: []string{"e1", "e2"}, Priorities: "speed"})
		if err != nil {
			t.Fatalf("Build: %v", err)
		}
		cands, ok := args["candidates"].([]map[string]any)
		if !ok || len(cands) != 2 || cands[0]["id"] != "a" || cands[1]["id"] != "b" {
			t.Fatalf("candidates = %v, want ordered [a b]", args["candidates"])
		}
		if args["evidence"] != "e1\ne2" {
			t.Fatalf("evidence = %v, want joined", args["evidence"])
		}
	})
	t.Run("choice rejects out-of-range candidates", func(t *testing.T) {
		for _, n := range []int{1, 7} {
			opts := make([]string, n)
			for i := range opts {
				opts[i] = "x"
			}
			if _, _, err := a.Build(OpKindChoice, Question{Options: opts}); err == nil {
				t.Fatalf("Build with %d candidates succeeded, want error", n)
			}
		}
	})
	t.Run("verify builds claims and evidence array", func(t *testing.T) {
		_, args, err := a.Build(OpKindVerify, Question{Claims: []string{"c"}, Evidence: []string{"e"}})
		if err != nil {
			t.Fatalf("Build: %v", err)
		}
		if _, ok := args["claims"].([]string); !ok {
			t.Fatalf("claims = %T, want []string", args["claims"])
		}
		ev, ok := args["evidence"].([]map[string]any)
		if !ok || len(ev) != 1 || ev[0]["text"] != "e" {
			t.Fatalf("evidence = %v, want one text entry", args["evidence"])
		}
	})
	t.Run("verify without claims errors before any call", func(t *testing.T) {
		if _, _, err := a.Build(OpKindVerify, Question{Evidence: []string{"e"}}); err == nil {
			t.Fatal("Build without claims succeeded, want error")
		}
	})
	t.Run("screen maps text and purpose", func(t *testing.T) {
		tool, args, err := a.Build(OpKindScreen, Question{Text: "hello", Purpose: "support reply"})
		if err != nil {
			t.Fatalf("Build: %v", err)
		}
		if tool != "jev_screen" || args["text"] != "hello" || args["purpose"] != "support reply" {
			t.Fatalf("tool=%s args=%v", tool, args)
		}
	})
	t.Run("score and gate are unsupported with honest notes", func(t *testing.T) {
		for _, op := range []OpKind{OpKindScore, OpKindGate} {
			_, _, err := a.Build(op, Question{})
			if err == nil {
				t.Fatalf("Build(%s) succeeded, want error", op)
			}
			s := a.Supports(op)
			if s.Supported || s.Note == "" {
				t.Fatalf("Supports(%s) = %+v, want unsupported with a note", op, s)
			}
		}
	})
}

// TestJkudishParse pins the result shapes: every supported op parses its
// documented fields, and malformed payloads (missing fields, wrong types,
// empty objects) are errors — never panics.
func TestJkudishParse(t *testing.T) {
	a := jkudish{}
	t.Run("noul bare envelope", func(t *testing.T) {
		vs, err := a.Parse(OpKindNoul, textResult(t, `{"probability": 0.97, "label": "likely"}`))
		if err != nil {
			t.Fatalf("Parse: %v", err)
		}
		if len(vs) != 1 || vs[0].Answer != 0.97 || vs[0].Label != "likely" {
			t.Fatalf("verdicts = %+v", vs)
		}
		if vs[0].Type != OpKindNoul || vs[0].Confidence != nil {
			t.Fatalf("noul verdict = %+v, want no confidence (TypeSafe canon)", vs[0])
		}
	})
	t.Run("noul results array envelope", func(t *testing.T) {
		vs, err := a.Parse(OpKindNoul, textResult(t, `{"results": [{"proposition": "x", "probability": 0.1, "label": "unlikely"}]}`))
		if err != nil {
			t.Fatalf("Parse: %v", err)
		}
		if vs[0].Answer != 0.1 {
			t.Fatalf("answer = %v, want 0.1", vs[0].Answer)
		}
	})
	t.Run("noul wrong-typed probability is an error", func(t *testing.T) {
		if _, err := a.Parse(OpKindNoul, textResult(t, `{"probability": true}`)); err == nil {
			t.Fatal("Parse on boolean probability succeeded, want error")
		}
	})
	t.Run("noul null probability is an error", func(t *testing.T) {
		if _, err := a.Parse(OpKindNoul, textResult(t, `{"probability": null}`)); err == nil {
			t.Fatal("Parse on null probability succeeded, want error")
		}
	})
	t.Run("noul empty object is an error naming the field", func(t *testing.T) {
		_, err := a.Parse(OpKindNoul, textResult(t, `{}`))
		if err == nil || !strings.Contains(err.Error(), "probability") {
			t.Fatalf("err = %v, want an error naming probability", err)
		}
	})
	t.Run("decide recommendation", func(t *testing.T) {
		doc := `{"recommendation": {"selected": "thank", "escaped": false, "confidence": 0.8, "probabilities": {"thank": 0.8, "emoji": 0.2}}, "checks": []}`
		vs, err := a.Parse(OpKindChoice, textResult(t, doc))
		if err != nil {
			t.Fatalf("Parse: %v", err)
		}
		v := vs[0]
		if v.Answer != "thank" || v.Confidence == nil || *v.Confidence != 0.8 || v.Probabilities["emoji"] != 0.2 {
			t.Fatalf("verdict = %+v", v)
		}
	})
	t.Run("decide missing recommendation is an error", func(t *testing.T) {
		if _, err := a.Parse(OpKindChoice, textResult(t, `{}`)); err == nil {
			t.Fatal("Parse on empty decide result succeeded, want error")
		}
	})
	t.Run("verify per-claim verdicts", func(t *testing.T) {
		doc := `{"summary": {"verified": 1, "contradicted": 0, "unsupported": 0, "needs_review": 0}, "results": [{"claim": "c", "verdict": "verified", "confidence": 0.9, "action": "auto"}]}`
		vs, err := a.Parse(OpKindVerify, textResult(t, doc))
		if err != nil {
			t.Fatalf("Parse: %v", err)
		}
		if len(vs) != 1 || vs[0].Answer != "verified" || vs[0].Label != "auto" {
			t.Fatalf("verdicts = %+v", vs)
		}
	})
	t.Run("screen recommendation and probabilities", func(t *testing.T) {
		doc := `{"probabilities": {"injection": 0.01, "substance": 0.9, "relevance": 0.9}, "recommendation": {"action": "pass", "reason": "clean"}}`
		vs, err := a.Parse(OpKindScreen, textResult(t, doc))
		if err != nil {
			t.Fatalf("Parse: %v", err)
		}
		if vs[0].Answer != "pass" || vs[0].Probabilities["injection"] != 0.01 || vs[0].Hint != "clean" {
			t.Fatalf("verdict = %+v", vs[0])
		}
	})
	t.Run("unparseable text is an error", func(t *testing.T) {
		if _, err := a.Parse(OpKindNoul, &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "garbage"}}}); err == nil {
			t.Fatal("Parse on garbage text succeeded, want error")
		}
	})
}

// TestJkudishProbes pins the battery: exactly one probe per vocabulary tool,
// every one carrying arguments (nothing skipped), all read-only fixtures.
func TestJkudishProbes(t *testing.T) {
	probes := (jkudish{}).Probes()
	vocab := (jkudish{}).Vocabulary()
	if len(probes) != len(vocab) {
		t.Fatalf("probes = %d, vocabulary = %d, want one probe per tool", len(probes), len(vocab))
	}
	seen := map[string]bool{}
	for _, p := range probes {
		if p.Tool == "" || p.SkipReason != "" || p.Args == nil {
			t.Fatalf("probe %+v must have a tool, args, and no skip reason", p)
		}
		if seen[p.Tool] {
			t.Fatalf("duplicate probe for %s", p.Tool)
		}
		seen[p.Tool] = true
	}
	for _, name := range vocab {
		if !seen[name] {
			t.Fatalf("no probe for %s", name)
		}
	}
}
