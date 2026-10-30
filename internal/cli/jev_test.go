package cli

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/ConsulTent/mcpcli/internal/format"
	"github.com/ConsulTent/mcpcli/internal/jev"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// Fixture servers for the three Jev flavors, plus auth-failing and
// error-failing variants. Every tool returns the canned result its flavor's
// adapter is documented to parse, so the E2E tests exercise the full
// detect → build → call → parse → render path over a real MCP session.

// addJkudishFixture registers the jkudish flavor's twelve tools with canned
// results and realistic input schemas, and records every call by tool name.
func addJkudishFixture(s *mcp.Server, calls *[]string) {
	text := func(doc string) *mcp.CallToolResult {
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: doc}}}
	}
	record := func(name string) {
		if calls != nil {
			*calls = append(*calls, name)
		}
	}
	propSchema := func(keys ...string) json.RawMessage {
		props := map[string]any{}
		for _, k := range keys {
			props[k] = map[string]any{"type": []string{"string", "array"}}
		}
		b, _ := json.Marshal(map[string]any{"type": "object", "properties": props})
		return b
	}

	s.AddTool(&mcp.Tool{Name: "jev_noul", InputSchema: propSchema("propositions", "context", "auto_accept")},
		func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			record("jev_noul")
			return text(`{"probability": 0.97, "label": "likely"}`), nil
		})
	s.AddTool(&mcp.Tool{Name: "jev_verify", InputSchema: propSchema("claims", "evidence", "auto_accept", "subject_at")},
		func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			record("jev_verify")
			return text(`{"summary": {"verified": 1}, "results": [{"claim": "c", "verdict": "verified", "confidence": 0.9, "action": "auto"}]}`), nil
		})
	s.AddTool(&mcp.Tool{Name: "jev_screen", InputSchema: propSchema("text", "purpose", "block_at", "review_at")},
		func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			record("jev_screen")
			return text(`{"probabilities": {"injection": 0.01, "substance": 0.97, "relevance": 0.97}, "recommendation": {"action": "pass", "reason": "clean"}}`), nil
		})
	s.AddTool(&mcp.Tool{Name: "jev_decide", InputSchema: propSchema("decision", "evidence", "priorities", "candidates", "requirements")},
		func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			record("jev_decide")
			return text(`{"recommendation": {"selected": "thank", "escaped": false, "confidence": 0.8, "probabilities": {"thank": 0.8, "emoji": 0.2}}}`), nil
		})
	for name, doc := range map[string]string{
		"jev_classify": `{"summary": {"items": 1, "auto": 1}, "results": [{"id": "m1", "classification": "positive", "margin": 1, "confidence": 1, "decision": "auto"}]}`,
		"jev_compare":  `{"overall": {"relation": "same_fact", "confidence": 1, "decision": "auto"}}`,
		"jev_find":     `{"exists": true, "top": [{"id": "c1", "relevance": 0.9}]}`,
		"jev_rerank":   `{"ranked": [{"rank": 1, "id": "c1", "relevance": 0.9}]}`,
		"jev_extract":  `{"fields": [{"id": "total", "value": "$42.00"}]}`,
		"jev_audit":    `{"records": [{"id": "r1", "p_wrong": 0.01, "action": "ok"}]}`,
		"jev_review":   `{"action": "auto", "composite": 1.9, "safe_to_apply": 0.95, "reason_codes": []}`,
		"jev_gate":     `{"action": "review", "reason_codes": ["claims_unverified"], "safe_to_apply": 0.5}`,
	} {
		s.AddTool(&mcp.Tool{Name: name, InputSchema: json.RawMessage(`{"type":"object"}`)},
			func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
				record(req.Params.Name)
				return text(doc), nil
			})
	}
}

// addJevUseFixture registers the jev-use flavor's two tools. The judge
// handler answers the question type the request actually carried, so choice
// and score asks parse; escalate flips the verdict to an escalated one.
func addJevUseFixture(s *mcp.Server, calls *[]string, escalate bool) {
	text := func(doc string) *mcp.CallToolResult {
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: doc}}}
	}
	s.AddTool(&mcp.Tool{Name: "jev_judge", InputSchema: json.RawMessage(`{"type":"object","properties":{"state":{},"questions":{},"confidence_threshold":{}}}`)},
		func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			if calls != nil {
				*calls = append(*calls, "jev_judge")
			}
			var args struct {
				Questions []struct {
					Type string `json:"type"`
				} `json:"questions"`
			}
			if err := json.Unmarshal(req.Params.Arguments, &args); err != nil {
				return nil, err
			}
			answer := any(0.97)
			qtype := "noul"
			if len(args.Questions) > 0 {
				qtype = args.Questions[0].Type
			}
			switch qtype {
			case "choice":
				answer = "merge"
			case "score":
				answer = 0.8
			}
			var doc string
			if escalate {
				doc = `{"verdicts": [{"id": "q", "type": "` + qtype + `", "answer": ` + jsonAnswer(answer) + `, "confidence": 0.34, "confidenceFrom": "reported", "escalate": true, "reason": "unsure", "hint": "treat the answer as a prior"}], "escalated": true}`
			} else {
				doc = `{"verdicts": [{"id": "q", "type": "` + qtype + `", "answer": ` + jsonAnswer(answer) + `, "confidence": 0.94, "confidenceFrom": "estimated", "escalate": false}], "escalated": false}`
			}
			return text(doc), nil
		})
	s.AddTool(&mcp.Tool{Name: "jev_gate", InputSchema: json.RawMessage(`{"type":"object","properties":{"state":{},"tool":{},"input":{},"confidence_threshold":{}}}`)},
		func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			if calls != nil {
				*calls = append(*calls, "jev_gate")
			}
			return text(`{"decision": "deny", "confidence": 0.91, "confidenceFrom": "reported", "hint": "writes outside the workspace"}`), nil
		})
}

// jsonAnswer renders a canned answer for the fixture doc.
func jsonAnswer(a any) string {
	switch v := a.(type) {
	case string:
		b, _ := json.Marshal(v)
		return string(b)
	case float64:
		return strconv.FormatFloat(v, 'g', -1, 64)
	default:
		return "0.97"
	}
}

// addLegostinFixture registers the legostin flavor's signature plus one
// extra browser tool, so detection and the doctor-only battery both have
// something to chew on.
func addLegostinFixture(s *mcp.Server, calls *[]string) {
	for _, name := range []string{"jev_task", "jev_ask", "jev_tabs"} {
		s.AddTool(&mcp.Tool{Name: name, InputSchema: json.RawMessage(`{"type":"object"}`)},
			func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
				if calls != nil {
					*calls = append(*calls, req.Params.Name)
				}
				return nil, errors.New("drives Chrome; fixture refuses")
			})
	}
	s.AddTool(&mcp.Tool{Name: "jev_doctor", InputSchema: json.RawMessage(`{"type":"object"}`)},
		func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			if calls != nil {
				*calls = append(*calls, "jev_doctor")
			}
			return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: `{"status": "ok"}`}}}, nil
		})
}

// jevAskDoc mirrors the canonical --json document of ask/verify/screen/gate.
type jevAskDoc struct {
	Flavor    string        `json:"flavor"`
	Tool      string        `json:"tool"`
	Verdicts  []jev.Verdict `json:"verdicts"`
	Escalated bool          `json:"escalated"`
	Error     string        `json:"error"`
}

// jevCheckDoc mirrors the --json document of check.
type jevCheckDoc struct {
	Flavor    string   `json:"flavor"`
	Matched   []string `json:"matched"`
	Unmatched []string `json:"unmatched"`
	Ops       []struct {
		Op   string `json:"op"`
		Via  string `json:"via"`
		OK   bool   `json:"ok"`
		Note string `json:"note"`
	} `json:"ops"`
	Probes []struct {
		Tool   string `json:"tool"`
		Status string `json:"status"`
		Detail string `json:"detail"`
	} `json:"probes"`
	Summary *struct {
		OK          int `json:"ok"`
		Escalate    int `json:"escalate"`
		Skipped     int `json:"skipped"`
		AuthSkipped int `json:"auth_skipped"`
		Errors      int `json:"errors"`
	} `json:"summary"`
	Error string `json:"error"`
}

// decodeJevAsk unmarshals a runRoot stdout document.
func decodeJevAsk(t *testing.T, stdout string) jevAskDoc {
	t.Helper()
	var doc jevAskDoc
	if err := json.Unmarshal([]byte(stdout), &doc); err != nil {
		t.Fatalf("decode ask doc: %v\nstdout: %s", err, stdout)
	}
	return doc
}

// decodeJevCheck unmarshals a runRoot stdout document.
func decodeJevCheck(t *testing.T, stdout string) jevCheckDoc {
	t.Helper()
	var doc jevCheckDoc
	if err := json.Unmarshal([]byte(stdout), &doc); err != nil {
		t.Fatalf("decode check doc: %v\nstdout: %s", err, stdout)
	}
	return doc
}

// assertCountsInvariant pins the battery contract: the five summary counts
// always sum to the number of probe rows.
func assertCountsInvariant(t *testing.T, doc jevCheckDoc) {
	t.Helper()
	if doc.Summary == nil {
		t.Fatal("battery doc has no summary")
	}
	sum := doc.Summary.OK + doc.Summary.Escalate + doc.Summary.Skipped + doc.Summary.AuthSkipped + doc.Summary.Errors
	if sum != len(doc.Probes) {
		t.Fatalf("summary counts sum to %d, probe rows = %d — invariant broken", sum, len(doc.Probes))
	}
}

// assertASCII pins the plain-mode byte contract: no non-ASCII, no ESC.
func assertASCII(t *testing.T, name, s string) {
	t.Helper()
	for i := 0; i < len(s); i++ {
		if b := s[i]; b >= 0x80 {
			t.Fatalf("%s has non-ASCII byte 0x%02x at offset %d: %q", name, b, i, s)
		}
		if b := s[i]; b == 0x1b {
			t.Fatalf("%s has an ESC byte at offset %d: %q", name, i, s)
		}
	}
}

// TestJevAskNoulE2E pins the noul path on both judgment flavors: the same
// canonical document regardless of flavor (the merge the feature exists for).
func TestJevAskNoulE2E(t *testing.T) {
	servers := map[string]*struct {
		build func(*mcp.Server)
		tool  string
	}{
		"jkudish": {func(s *mcp.Server) { addJkudishFixture(s, nil) }, "jev_noul"},
		"jev-use": {func(s *mcp.Server) { addJevUseFixture(s, nil, false) }, "jev_judge"},
	}
	for flavor, tc := range servers {
		t.Run(flavor, func(t *testing.T) {
			srv := newTestMCPServerWithTools(t, tc.build)
			stdout, _, _, err := runRoot(t, "--url", srv.URL, "--json", "jev", "ask", "2+2=4?")
			if err != nil {
				t.Fatalf("ask: %v", err)
			}
			doc := decodeJevAsk(t, stdout)
			if doc.Flavor != flavor || doc.Tool != tc.tool {
				t.Fatalf("doc = %+v, want flavor %s tool %s", doc, flavor, tc.tool)
			}
			if len(doc.Verdicts) != 1 || doc.Verdicts[0].Type != jev.OpKindNoul || doc.Verdicts[0].Answer != 0.97 {
				t.Fatalf("verdicts = %+v, want one noul with answer 0.97", doc.Verdicts)
			}
			if doc.Escalated {
				t.Fatal("escalated = true, want false")
			}
		})
	}
}

// TestJevAskChoiceE2E pins the choice inference (--opt) on both flavors.
// The jkudish row carries --evidence/--priorities: the live server validates
// both as non-empty (captured 2026-10-30), and the Build-time error without
// them is pinned by the case after the table.
func TestJevAskChoiceE2E(t *testing.T) {
	tests := []struct {
		flavor string
		build  func(*mcp.Server)
		tool   string
		want   string
		extra  []string
	}{
		{"jkudish", func(s *mcp.Server) { addJkudishFixture(s, nil) }, "jev_decide", "thank", []string{"--evidence", "the comment says thanks", "--priorities", "be concise"}},
		{"jev-use", func(s *mcp.Server) { addJevUseFixture(s, nil, false) }, "jev_judge", "merge", nil},
	}
	for _, tt := range tests {
		t.Run(tt.flavor, func(t *testing.T) {
			srv := newTestMCPServerWithTools(t, tt.build)
			args := slices.Concat([]string{"--url", srv.URL, "--json", "jev", "ask", "next?", "--opt", "a", "--opt", "b"}, tt.extra)
			stdout, _, _, err := runRoot(t, args...)
			if err != nil {
				t.Fatalf("ask: %v", err)
			}
			doc := decodeJevAsk(t, stdout)
			if doc.Tool != tt.tool || len(doc.Verdicts) != 1 || doc.Verdicts[0].Answer != tt.want {
				t.Fatalf("doc = %+v, want tool %s answer %s", doc, tt.tool, tt.want)
			}
			if doc.Verdicts[0].Type != jev.OpKindChoice {
				t.Fatalf("type = %v, want choice", doc.Verdicts[0].Type)
			}
		})
	}
	t.Run("jkudish choice without evidence and priorities errors", func(t *testing.T) {
		srv := newTestMCPServerWithTools(t, func(s *mcp.Server) { addJkudishFixture(s, nil) })
		_, _, _, err := runRoot(t, "--url", srv.URL, "jev", "ask", "next?", "--opt", "a", "--opt", "b")
		if err == nil || !strings.Contains(err.Error(), "requires --evidence and --priorities") {
			t.Fatalf("err = %v, want the missing-flag error", err)
		}
	})
}

// TestJevAskScoreE2E pins the score inference (--levels) on jev-use, and the
// plain-mode byte contract on a full verdict line.
func TestJevAskScoreE2E(t *testing.T) {
	srv := newTestMCPServerWithTools(t, func(s *mcp.Server) { addJevUseFixture(s, nil, false) })
	t.Run("json", func(t *testing.T) {
		stdout, _, _, err := runRoot(t, "--url", srv.URL, "--json", "jev", "ask", "risk?", "--levels", "routine, incident")
		if err != nil {
			t.Fatalf("ask: %v", err)
		}
		doc := decodeJevAsk(t, stdout)
		if len(doc.Verdicts) != 1 || doc.Verdicts[0].Type != jev.OpKindScore || doc.Verdicts[0].Answer != 0.8 {
			t.Fatalf("verdicts = %+v, want one score with answer 0.8", doc.Verdicts)
		}
	})
	t.Run("plain is byte-ascii", func(t *testing.T) {
		stdout, stderr, _, err := runRoot(t, "--url", srv.URL, "--plain", "jev", "ask", "risk?", "--levels", "routine,incident")
		if err != nil {
			t.Fatalf("ask: %v", err)
		}
		assertASCII(t, "stdout", stdout)
		assertASCII(t, "stderr", stderr)
	})
}

// TestJevAskEscalateExitsZero pins the data-not-failure semantics: an
// escalated verdict renders (with its hint) and exits 0, in both modes.
func TestJevAskEscalateExitsZero(t *testing.T) {
	srv := newTestMCPServerWithTools(t, func(s *mcp.Server) { addJevUseFixture(s, nil, true) })
	t.Run("json", func(t *testing.T) {
		stdout, _, _, err := runRoot(t, "--url", srv.URL, "--json", "jev", "ask", "next?", "--opt", "a", "--opt", "b")
		if err != nil {
			t.Fatalf("ask: %v (escalate must exit 0)", err)
		}
		doc := decodeJevAsk(t, stdout)
		if !doc.Escalated || !doc.Verdicts[0].Escalate || doc.Verdicts[0].Hint == "" {
			t.Fatalf("doc = %+v, want escalated verdict with hint", doc)
		}
	})
	t.Run("human", func(t *testing.T) {
		stdout, _, _, err := runRoot(t, "--url", srv.URL, "jev", "ask", "next?", "--opt", "a", "--opt", "b")
		if err != nil {
			t.Fatalf("ask: %v (escalate must exit 0)", err)
		}
		if !strings.Contains(stdout, "ESCALATE") || !strings.Contains(stdout, "treat the answer as a prior") {
			t.Fatalf("stdout = %q, want ESCALATE with the hint", stdout)
		}
	})
}

// TestJevAskUnsupported pins the honest refusal: score on the jkudish flavor
// errors with the nearest alternatives named.
func TestJevAskUnsupported(t *testing.T) {
	srv := newTestMCPServerWithTools(t, func(s *mcp.Server) { addJkudishFixture(s, nil) })
	_, _, _, err := runRoot(t, "--url", srv.URL, "jev", "ask", "risk?", "--levels", "a,b")
	if err == nil || !strings.Contains(err.Error(), `unsupported op "score"`) {
		t.Fatalf("err = %v, want unsupported op \"score\"", err)
	}
}

// TestJevAskDroppedFlagWarning pins the never-silent rule: a flag the
// mapping cannot carry warns on stderr even in --json mode, and the document
// on stdout stays valid.
func TestJevAskDroppedFlagWarning(t *testing.T) {
	srv := newTestMCPServerWithTools(t, func(s *mcp.Server) { addJevUseFixture(s, nil, false) })
	stdout, stderr, _, err := runRoot(t, "--url", srv.URL, "--json", "jev", "ask", "2+2=4?", "--evidence", "math facts")
	if err != nil {
		t.Fatalf("ask: %v", err)
	}
	if !strings.Contains(stderr, "warning: --evidence is not carried by the jev-use noul mapping") {
		t.Fatalf("stderr = %q, want the dropped-flag warning", stderr)
	}
	decodeJevAsk(t, stdout) // must still be one valid document
}

// TestJevArgumentErrors pins the golden pre-connect error table: every
// malformed flag state fails before any connection, naming the flag.
func TestJevArgumentErrors(t *testing.T) {
	tests := []struct {
		name string
		args []string
		want string
	}{
		{"opt and levels", []string{"jev", "ask", "--opt", "a", "--levels", "x,y", "q"}, "--levels and --opt are mutually exclusive"},
		{"blank question", []string{"jev", "ask", "   "}, "ask requires a non-empty QUESTION"},
		{"blank opt", []string{"jev", "ask", "--opt", "  ", "q"}, `invalid --opt`},
		{"duplicate opt", []string{"jev", "ask", "--opt", "a", "--opt", "a", "q"}, "duplicate label"},
		{"empty levels segment", []string{"jev", "ask", "--levels", "a,,b", "q"}, "segments must be non-empty"},
		{"trailing comma levels", []string{"jev", "ask", "--levels", "a,b,", "q"}, "segments must be non-empty"},
		{"threshold nan", []string{"jev", "ask", "--threshold", "nan", "q"}, "invalid --threshold"},
		{"threshold out of range", []string{"jev", "ask", "--threshold", "2", "q"}, "invalid --threshold"},
		{"verify no claims", []string{"jev", "verify"}, "verify requires at least one --claim"},
		{"verify no evidence", []string{"jev", "verify", "--claim", "c"}, "verify requires at least one --evidence"},
		{"screen blank text", []string{"jev", "screen", "  "}, "screen requires a non-empty TEXT"},
		{"gate blank name", []string{"jev", "gate", "  ", "--input", "{}"}, "gate requires a non-empty NAME"},
		{"gate missing input", []string{"jev", "gate", "write_file"}, "gate requires --input"},
		{"gate null input", []string{"jev", "gate", "write_file", "--input", "null"}, "gate --input: expected a JSON object"},
		{"gate array input", []string{"jev", "gate", "write_file", "--input", "[1]"}, "gate --input: expected a JSON object"},
		{"bad auth mode", []string{"jev", "check", "--probe", "--probe-on-auth-error", "bogus"}, "invalid --probe-on-auth-error"},
		{"auth mode without probe", []string{"jev", "check", "--probe-on-auth-error", "skip"}, "--probe-on-auth-error only applies to jev check --probe"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// No target flags: a pre-connect error must fire without any
			// network attempt.
			_, _, _, err := runRoot(t, tt.args...)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("err = %v, want it to contain %q", err, tt.want)
			}
		})
	}
}

// TestJevVerifyScreenGateE2E pins the three single-op commands against their
// flavors, including the gate's stdin path for --input.
func TestJevVerifyScreenGateE2E(t *testing.T) {
	t.Run("verify", func(t *testing.T) {
		srv := newTestMCPServerWithTools(t, func(s *mcp.Server) { addJkudishFixture(s, nil) })
		stdout, _, _, err := runRoot(t, "--url", srv.URL, "--json", "jev", "verify", "--claim", "the build passed", "--evidence", "214 tests green")
		if err != nil {
			t.Fatalf("verify: %v", err)
		}
		doc := decodeJevAsk(t, stdout)
		if doc.Tool != "jev_verify" || len(doc.Verdicts) != 1 || doc.Verdicts[0].Answer != "verified" {
			t.Fatalf("doc = %+v, want one verified verdict", doc)
		}
	})
	t.Run("screen", func(t *testing.T) {
		srv := newTestMCPServerWithTools(t, func(s *mcp.Server) { addJkudishFixture(s, nil) })
		stdout, _, _, err := runRoot(t, "--url", srv.URL, "--json", "jev", "screen", "hello there", "--purpose", "support reply")
		if err != nil {
			t.Fatalf("screen: %v", err)
		}
		doc := decodeJevAsk(t, stdout)
		if doc.Verdicts[0].Answer != "pass" || doc.Verdicts[0].Probabilities["injection"] != 0.01 {
			t.Fatalf("doc = %+v, want pass with injection probability", doc)
		}
	})
	t.Run("gate inline input", func(t *testing.T) {
		srv := newTestMCPServerWithTools(t, func(s *mcp.Server) { addJevUseFixture(s, nil, false) })
		stdout, _, _, err := runRoot(t, "--url", srv.URL, "--json", "jev", "gate", "write_file", "--input", `{"path":"x.txt"}`)
		if err != nil {
			t.Fatalf("gate: %v", err)
		}
		doc := decodeJevAsk(t, stdout)
		if doc.Tool != "jev_gate" || doc.Verdicts[0].Answer != "deny" || !strings.Contains(doc.Verdicts[0].Hint, "workspace") {
			t.Fatalf("doc = %+v, want deny with hint", doc)
		}
	})
	t.Run("gate stdin input", func(t *testing.T) {
		srv := newTestMCPServerWithTools(t, func(s *mcp.Server) { addJevUseFixture(s, nil, false) })
		r, w, err := os.Pipe()
		if err != nil {
			t.Fatalf("pipe: %v", err)
		}
		if _, err := w.WriteString(`{"path":"x.txt"}`); err != nil {
			t.Fatalf("write pipe: %v", err)
		}
		_ = w.Close()
		oldStdin := os.Stdin
		os.Stdin = r
		defer func() { os.Stdin = oldStdin }()
		stdout, _, _, err := runRoot(t, "--url", srv.URL, "--json", "jev", "gate", "write_file", "--input", "-")
		if err != nil {
			t.Fatalf("gate stdin: %v", err)
		}
		if doc := decodeJevAsk(t, stdout); doc.Verdicts[0].Answer != "deny" {
			t.Fatalf("doc = %+v, want deny", doc)
		}
	})
}

// TestJevIsErrorDocFirst pins the isError contract: the canonical document
// still prints (carrying the error text), then the command fails with
// format.ErrToolError.
func TestJevIsErrorDocFirst(t *testing.T) {
	srv := newTestMCPServerWithTools(t, func(s *mcp.Server) {
		s.AddTool(&mcp.Tool{Name: "jev_judge", InputSchema: json.RawMessage(`{"type":"object"}`)},
			func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
				return &mcp.CallToolResult{
					IsError: true,
					Content: []mcp.Content{&mcp.TextContent{Text: "judge exploded"}},
				}, nil
			})
	})
	stdout, _, _, err := runRoot(t, "--url", srv.URL, "--json", "jev", "ask", "2+2=4?")
	if !errors.Is(err, format.ErrToolError) {
		t.Fatalf("err = %v, want format.ErrToolError", err)
	}
	doc := decodeJevAsk(t, stdout)
	if doc.Error == "" || !strings.Contains(doc.Error, "judge exploded") {
		t.Fatalf("doc = %+v, want the error text carried in the document", doc)
	}
}

// TestJevCheckE2E pins detection and the support matrix for all three
// flavors plus the no-flavor refusal (which still prints a document).
func TestJevCheckE2E(t *testing.T) {
	tests := []struct {
		name    string
		build   func(*mcp.Server)
		flavor  string
		matched int
		wantVia map[string]string
	}{
		{
			"jkudish", func(s *mcp.Server) { addJkudishFixture(s, nil) }, "jkudish", 12,
			map[string]string{"noul": "jev_noul", "choice": "jev_decide", "verify": "jev_verify", "screen": "jev_screen"},
		},
		{
			"jev-use", func(s *mcp.Server) { addJevUseFixture(s, nil, false) }, "jev-use", 2,
			map[string]string{"noul": "jev_judge", "choice": "jev_judge", "score": "jev_judge", "gate": "jev_gate"},
		},
		{
			"legostin", func(s *mcp.Server) { addLegostinFixture(s, nil) }, "legostin", 1,
			nil, // every op unsupported
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := newTestMCPServerWithTools(t, tt.build)
			stdout, _, _, err := runRoot(t, "--url", srv.URL, "--json", "jev", "check")
			if err != nil {
				t.Fatalf("check: %v", err)
			}
			doc := decodeJevCheck(t, stdout)
			if doc.Flavor != tt.flavor {
				t.Fatalf("flavor = %q, want %q", doc.Flavor, tt.flavor)
			}
			if len(doc.Matched) != tt.matched {
				t.Fatalf("matched = %v (%d), want %d", doc.Matched, len(doc.Matched), tt.matched)
			}
			if len(doc.Ops) != 6 {
				t.Fatalf("ops = %d rows, want 6", len(doc.Ops))
			}
			for _, row := range doc.Ops {
				if via, ok := tt.wantVia[row.Op]; ok && (!row.OK || row.Via != via) {
					t.Fatalf("op %s = %+v, want supported via %s", row.Op, row, via)
				}
				if _, ok := tt.wantVia[row.Op]; !ok && row.OK {
					t.Fatalf("op %s supported, want unsupported", row.Op)
				}
			}
		})
	}
	t.Run("no flavor", func(t *testing.T) {
		srv := newTestMCPServerWithTools(t, addEchoTool)
		stdout, _, _, err := runRoot(t, "--url", srv.URL, "--json", "jev", "check")
		if err == nil {
			t.Fatal("check on a non-Jev server succeeded, want error")
		}
		doc := decodeJevCheck(t, stdout)
		if doc.Flavor != "" || !strings.Contains(doc.Error, "no known Jev flavor") {
			t.Fatalf("doc = %+v, want empty flavor with the reason", doc)
		}
	})
	t.Run("human report renders", func(t *testing.T) {
		srv := newTestMCPServerWithTools(t, func(s *mcp.Server) { addJevUseFixture(s, nil, false) })
		stdout, _, _, err := runRoot(t, "--url", srv.URL, "--plain", "jev", "check")
		if err != nil {
			t.Fatalf("check: %v", err)
		}
		assertASCII(t, "stdout", stdout)
		if !strings.Contains(stdout, "jev_judge") {
			t.Fatalf("stdout = %q, want the matrix to name jev_judge", stdout)
		}
	})
	t.Run("schema drift lands as a data note", func(t *testing.T) {
		// jev_noul without "propositions" declared: the support matrix must
		// carry the drift note instead of failing or hiding it.
		srv := newTestMCPServerWithTools(t, func(s *mcp.Server) {
			s.AddTool(&mcp.Tool{Name: "jev_noul", InputSchema: json.RawMessage(`{"type":"object","properties":{"context":{}}}`)},
				func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
					return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: `{"probability": 0.5}`}}}, nil
				})
			s.AddTool(&mcp.Tool{Name: "jev_verify", InputSchema: json.RawMessage(`{"type":"object"}`)},
				func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
					return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: `{"results": [{"verdict": "unknown"}]}`}}}, nil
				})
		})
		stdout, _, _, err := runRoot(t, "--url", srv.URL, "--json", "jev", "check")
		if err != nil {
			t.Fatalf("check: %v", err)
		}
		doc := decodeJevCheck(t, stdout)
		if doc.Flavor != "jkudish" {
			t.Fatalf("flavor = %q, want jkudish (two-tool signature)", doc.Flavor)
		}
		found := false
		for _, row := range doc.Ops {
			if row.Op == "noul" {
				found = true
				if !row.OK || !strings.Contains(row.Note, "schema drift") || !strings.Contains(row.Note, "propositions") {
					t.Fatalf("noul row = %+v, want drift note naming propositions", row)
				}
			}
		}
		if !found {
			t.Fatal("no noul row in the support matrix")
		}
	})
}

// TestJevCheckProbeE2E pins the battery on all three flavors: one row per
// available tool, the counts invariant, and exit 0 with escalated verdicts.
func TestJevCheckProbeE2E(t *testing.T) {
	t.Run("jkudish twelve tools", func(t *testing.T) {
		var calls []string
		srv := newTestMCPServerWithTools(t, func(s *mcp.Server) { addJkudishFixture(s, &calls) })
		stdout, _, _, err := runRoot(t, "--url", srv.URL, "--json", "jev", "check", "--probe")
		if err != nil {
			t.Fatalf("check --probe: %v", err)
		}
		doc := decodeJevCheck(t, stdout)
		if len(doc.Probes) != 12 || doc.Summary.OK != 12 {
			t.Fatalf("probes = %d rows, summary %+v, want 12 ok", len(doc.Probes), doc.Summary)
		}
		assertCountsInvariant(t, doc)
		if len(calls) != 12 {
			t.Fatalf("server saw %d calls, want 12 (one per tool)", len(calls))
		}
	})
	t.Run("jev-use two probes", func(t *testing.T) {
		srv := newTestMCPServerWithTools(t, func(s *mcp.Server) { addJevUseFixture(s, nil, false) })
		stdout, _, _, err := runRoot(t, "--url", srv.URL, "--json", "jev", "check", "--probe")
		if err != nil {
			t.Fatalf("check --probe: %v", err)
		}
		doc := decodeJevCheck(t, stdout)
		if len(doc.Probes) != 2 || doc.Summary.OK != 2 {
			t.Fatalf("probes = %+v, want judge + gate both ok", doc.Probes)
		}
		assertCountsInvariant(t, doc)
	})
	t.Run("legostin doctor plus skips", func(t *testing.T) {
		var calls []string
		srv := newTestMCPServerWithTools(t, func(s *mcp.Server) { addLegostinFixture(s, &calls) })
		stdout, _, _, err := runRoot(t, "--url", srv.URL, "--json", "jev", "check", "--probe")
		if err != nil {
			t.Fatalf("check --probe: %v", err)
		}
		doc := decodeJevCheck(t, stdout)
		if len(doc.Probes) != 4 || doc.Summary.OK != 1 || doc.Summary.Skipped != 3 {
			t.Fatalf("probes = %+v, want doctor ok plus three skips", doc.Probes)
		}
		assertCountsInvariant(t, doc)
		for _, name := range []string{"jev_task", "jev_ask", "jev_tabs"} {
			if slices.Contains(calls, name) {
				t.Fatalf("browser tool %s was called; battery must not drive Chrome", name)
			}
		}
		if !slices.Contains(calls, "jev_doctor") {
			t.Fatal("jev_doctor was not called")
		}
	})
	t.Run("escalated probe exits zero", func(t *testing.T) {
		srv := newTestMCPServerWithTools(t, func(s *mcp.Server) { addJevUseFixture(s, nil, true) })
		stdout, _, _, err := runRoot(t, "--url", srv.URL, "--json", "jev", "check", "--probe")
		if err != nil {
			t.Fatalf("check --probe: %v (escalate must exit 0)", err)
		}
		doc := decodeJevCheck(t, stdout)
		if doc.Summary.Escalate != 1 || doc.Summary.OK != 1 || doc.Summary.Errors != 0 {
			t.Fatalf("summary = %+v, want one escalate, one ok, no errors", doc.Summary)
		}
		assertCountsInvariant(t, doc)
	})
}

// TestJevCheckProbeAuthSkip pins the CI posture: a provider auth rejection
// aborts the battery after the first probe (the gate is never called), and —
// in skip mode — exits 0 with the warning and honest skip rows. Fail mode
// turns the same rejection into exit 1. Both error shapes are covered: a
// CallTool error and an isError result.
func TestJevCheckProbeAuthSkip(t *testing.T) {
	fixtures := map[string]func(*mcp.Server, *[]string){
		"call-tool error": func(s *mcp.Server, calls *[]string) {
			addJevUseAuthErrorFixture(s, calls, false)
		},
		"is-error result": func(s *mcp.Server, calls *[]string) {
			addJevUseAuthErrorFixture(s, calls, true)
		},
	}
	for shape, build := range fixtures {
		t.Run(shape+" skip mode", func(t *testing.T) {
			var calls []string
			srv := newTestMCPServerWithTools(t, func(s *mcp.Server) { build(s, &calls) })
			stdout, stderr, _, err := runRoot(t, "--url", srv.URL, "--json", "jev", "check", "--probe", "--probe-on-auth-error", "skip")
			if err != nil {
				t.Fatalf("skip mode err = %v, want nil (dead key must not fail the run)", err)
			}
			doc := decodeJevCheck(t, stdout)
			if doc.Summary.AuthSkipped != 1 || doc.Summary.Skipped != 1 || doc.Summary.Errors != 0 {
				t.Fatalf("summary = %+v, want one auth_skipped, one skipped, no errors", doc.Summary)
			}
			assertCountsInvariant(t, doc)
			if len(doc.Probes) == 0 || doc.Probes[0].Status != "auth_skipped" || !strings.Contains(doc.Probes[0].Detail, "auth-class (heuristic)") {
				t.Fatalf("first probe = %+v, want auth_skipped with the heuristic detail", doc.Probes[0])
			}
			if !strings.Contains(doc.Probes[1].Detail, "not run: battery aborted on auth-class error") {
				t.Fatalf("second probe = %+v, want the not-run skip detail", doc.Probes[1])
			}
			if !strings.Contains(stderr, "warning: provider key rejected") {
				t.Fatalf("stderr = %q, want the skip warning", stderr)
			}
			if slices.Contains(calls, "jev_gate") {
				t.Fatal("jev_gate was called after the abort; later probes must not run")
			}
		})
		t.Run(shape+" fail mode", func(t *testing.T) {
			var calls []string
			srv := newTestMCPServerWithTools(t, func(s *mcp.Server) { build(s, &calls) })
			stdout, _, _, err := runRoot(t, "--url", srv.URL, "--json", "jev", "check", "--probe")
			if err == nil {
				t.Fatal("fail mode err = nil, want the battery failure")
			}
			doc := decodeJevCheck(t, stdout) // doc-before-exit
			if doc.Summary.Errors != 1 || doc.Summary.Skipped != 1 || doc.Summary.AuthSkipped != 0 {
				t.Fatalf("summary = %+v, want one error, one skipped", doc.Summary)
			}
			assertCountsInvariant(t, doc)
			if !strings.Contains(err.Error(), "1 of 2 probes failed") {
				t.Fatalf("err = %v, want the low-cardinality summary", err)
			}
		})
	}
}

// addJevUseAuthErrorFixture registers a jev-use server whose judge tool
// rejects like a dead provider key, in either error shape.
func addJevUseAuthErrorFixture(s *mcp.Server, calls *[]string, isErrorShape bool) {
	s.AddTool(&mcp.Tool{Name: "jev_judge", InputSchema: json.RawMessage(`{"type":"object"}`)},
		func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			*calls = append(*calls, "jev_judge")
			if isErrorShape {
				return &mcp.CallToolResult{
					IsError: true,
					Content: []mcp.Content{&mcp.TextContent{Text: "typesafe rejected the request: HTTP 401 Unauthorized (invalid API key)"}},
				}, nil
			}
			return nil, errors.New("provider request failed: 402 Payment Required (billing on hold)")
		})
	s.AddTool(&mcp.Tool{Name: "jev_gate", InputSchema: json.RawMessage(`{"type":"object"}`)},
		func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			*calls = append(*calls, "jev_gate")
			return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: `{"decision": "allow"}`}}}, nil
		})
}

// TestJevCheckProbeNonAuthError pins the other abort path: a non-auth tool
// error fails the run in both modes, immediately, with the remaining probes
// skipped and the call count proving the battery stopped.
func TestJevCheckProbeNonAuthError(t *testing.T) {
	var calls []string
	srv := newTestMCPServerWithTools(t, func(s *mcp.Server) {
		s.AddTool(&mcp.Tool{Name: "jev_judge", InputSchema: json.RawMessage(`{"type":"object"}`)},
			func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
				calls = append(calls, "jev_judge")
				return &mcp.CallToolResult{
					IsError: true,
					Content: []mcp.Content{&mcp.TextContent{Text: "internal judge failure"}},
				}, nil
			})
		s.AddTool(&mcp.Tool{Name: "jev_gate", InputSchema: json.RawMessage(`{"type":"object"}`)},
			func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
				calls = append(calls, "jev_gate")
				return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: `{"decision": "allow"}`}}}, nil
			})
	})
	stdout, _, _, err := runRoot(t, "--url", srv.URL, "--json", "jev", "check", "--probe", "--probe-on-auth-error", "skip")
	if err == nil {
		t.Fatal("skip mode on a non-auth error succeeded, want failure (skip covers auth-class only)")
	}
	doc := decodeJevCheck(t, stdout)
	if doc.Summary.Errors != 1 || doc.Summary.Skipped != 1 {
		t.Fatalf("summary = %+v, want one error one skipped", doc.Summary)
	}
	assertCountsInvariant(t, doc)
	if slices.Contains(calls, "jev_gate") {
		t.Fatal("jev_gate ran after a non-auth abort")
	}
}

// TestJevInteractiveRejection pins that "-i jev" hits the root's interactive
// rejection — the runnable-group pattern's whole reason to exist.
func TestJevInteractiveRejection(t *testing.T) {
	srv := newTestMCPServerWithTools(t, func(s *mcp.Server) { addJevUseFixture(s, nil, false) })
	_, _, _, err := runRoot(t, "-i", "--url", srv.URL, "jev")
	if err == nil {
		t.Fatal("-i jev accepted, want the interactive rejection")
	}
	if _, _, _, err := runRoot(t, "-i", "--url", srv.URL, "jev", "ask", "q"); err == nil {
		t.Fatal("-i jev ask accepted, want the interactive rejection")
	}
}
