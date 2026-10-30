package jev

import (
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// TestJevUseDetect pins the jev_judge signature.
func TestJevUseDetect(t *testing.T) {
	a := jevUse{}
	if !a.Detect(tools("jev_judge")) {
		t.Fatal("Detect(jev_judge) = false, want true")
	}
	if a.Detect(tools("jev_judge_missing", "jev_gate")) {
		t.Fatal("Detect without jev_judge = true, want false")
	}
}

// TestJevUseBuild pins the judge request mapping for the three question
// types plus the gate, including the threshold pass-through.
func TestJevUseBuild(t *testing.T) {
	a := jevUse{}
	t.Run("noul question with state", func(t *testing.T) {
		tool, args, err := a.Build(OpKindNoul, Question{Text: "2+2=4?", Context: "math class", Threshold: 0.6, HasThreshold: true})
		if err != nil {
			t.Fatalf("Build: %v", err)
		}
		if tool != "jev_judge" {
			t.Fatalf("tool = %s, want jev_judge", tool)
		}
		if args["state"] != "math class" || args["confidence_threshold"] != 0.6 {
			t.Fatalf("args = %v", args)
		}
		qs, ok := args["questions"].([]map[string]any)
		if !ok || len(qs) != 1 || qs[0]["type"] != "noul" || qs[0]["question"] != "2+2=4?" {
			t.Fatalf("questions = %v", args["questions"])
		}
	})
	t.Run("choice options as label map", func(t *testing.T) {
		_, args, err := a.Build(OpKindChoice, Question{Text: "next?", Options: []string{"go", "stop"}})
		if err != nil {
			t.Fatalf("Build: %v", err)
		}
		q := args["questions"].([]map[string]any)[0]
		opts, ok := q["options"].(map[string]string)
		if !ok || len(opts) != 2 || opts["go"] != "go" {
			t.Fatalf("options = %v, want label map", q["options"])
		}
	})
	t.Run("choice with one option errors", func(t *testing.T) {
		if _, _, err := a.Build(OpKindChoice, Question{Options: []string{"only"}}); err == nil {
			t.Fatal("Build with one option succeeded, want error")
		}
	})
	t.Run("score levels pass through", func(t *testing.T) {
		_, args, err := a.Build(OpKindScore, Question{Text: "risk?", Levels: []string{"low", "high"}})
		if err != nil {
			t.Fatalf("Build: %v", err)
		}
		q := args["questions"].([]map[string]any)[0]
		if q["type"] != "score" {
			t.Fatalf("type = %v, want score", q["type"])
		}
		levels, ok := q["levels"].([]string)
		if !ok || len(levels) != 2 {
			t.Fatalf("levels = %v", q["levels"])
		}
	})
	t.Run("score with one level errors", func(t *testing.T) {
		if _, _, err := a.Build(OpKindScore, Question{Levels: []string{"only"}}); err == nil {
			t.Fatal("Build with one level succeeded, want error")
		}
	})
	t.Run("gate maps state tool input", func(t *testing.T) {
		tool, args, err := a.Build(OpKindGate, Question{State: "s", Tool: "write_file", Input: map[string]any{"path": "x"}, Threshold: 0.5, HasThreshold: true})
		if err != nil {
			t.Fatalf("Build: %v", err)
		}
		if tool != "jev_gate" || args["tool"] != "write_file" || args["confidence_threshold"] != 0.5 {
			t.Fatalf("tool=%s args=%v", tool, args)
		}
		if _, ok := args["input"].(map[string]any); !ok {
			t.Fatalf("input = %T, want map", args["input"])
		}
	})
	t.Run("gate without a tool name errors", func(t *testing.T) {
		if _, _, err := a.Build(OpKindGate, Question{}); err == nil {
			t.Fatal("Build without tool name succeeded, want error")
		}
	})
	t.Run("verify and screen are unsupported with nearest-alternative notes", func(t *testing.T) {
		for _, op := range []OpKind{OpKindVerify, OpKindScreen} {
			if _, _, err := a.Build(op, Question{}); err == nil {
				t.Fatalf("Build(%s) succeeded, want error", op)
			}
			s := a.Supports(op)
			if s.Supported || s.Note == "" {
				t.Fatalf("Supports(%s) = %+v, want unsupported with a note", op, s)
			}
		}
	})
}

// TestJevUseParse pins the verdict shape for all four supported ops and the
// type enforcement: numbers where numbers belong, strings where strings
// belong, missing fields named.
func TestJevUseParse(t *testing.T) {
	a := jevUse{}
	t.Run("judge noul verdict", func(t *testing.T) {
		doc := `{"verdicts": [{"id": "q", "type": "noul", "answer": 0.97, "confidence": 0.94, "confidenceFrom": "estimated", "escalate": false}], "escalated": false, "backend": "typesafe", "latencyMs": 187}`
		vs, err := a.Parse(OpKindNoul, textResult(t, doc))
		if err != nil {
			t.Fatalf("Parse: %v", err)
		}
		v := vs[0]
		if v.Answer != 0.97 || v.Confidence == nil || *v.Confidence != 0.94 || v.Escalate {
			t.Fatalf("verdict = %+v", v)
		}
	})
	t.Run("judge choice verdict with distribution and legend", func(t *testing.T) {
		doc := `{"verdicts": [{"id": "q", "type": "choice", "answer": "merge", "confidence": 0.34, "confidenceFrom": "reported", "escalate": true, "reason": "unsure", "distribution": {"merge": 0.55, "rerun": 0.41, "hold": 0.04}, "hint": "treat the answer as a prior"}], "escalated": true}`
		vs, err := a.Parse(OpKindChoice, textResult(t, doc))
		if err != nil {
			t.Fatalf("Parse: %v", err)
		}
		v := vs[0]
		if v.Answer != "merge" || !v.Escalate || v.Reason != "unsure" || v.Hint != "treat the answer as a prior" {
			t.Fatalf("verdict = %+v", v)
		}
		if v.Probabilities["rerun"] != 0.41 {
			t.Fatalf("probabilities = %v", v.Probabilities)
		}
	})
	t.Run("judge score verdict with legend", func(t *testing.T) {
		doc := `{"verdicts": [{"id": "q", "type": "score", "answer": 0.8, "confidence": 0.81, "confidenceFrom": "reported", "escalate": false, "distribution": {"0": 0.35, "1": 0.5, "2": 0.15}, "legend": {"0": "routine", "1": "worth a look", "2": "incident"}}], "escalated": false}`
		vs, err := a.Parse(OpKindScore, textResult(t, doc))
		if err != nil {
			t.Fatalf("Parse: %v", err)
		}
		v := vs[0]
		if v.Answer != 0.8 || v.Legend["2"] != "incident" {
			t.Fatalf("verdict = %+v", v)
		}
	})
	t.Run("string answer for a noul is an error", func(t *testing.T) {
		doc := `{"verdicts": [{"id": "q", "type": "noul", "answer": "yes"}]}`
		if _, err := a.Parse(OpKindNoul, textResult(t, doc)); err == nil {
			t.Fatal("Parse on string noul answer succeeded, want error")
		}
	})
	t.Run("null answer is an error", func(t *testing.T) {
		doc := `{"verdicts": [{"id": "q", "type": "noul", "answer": null}]}`
		if _, err := a.Parse(OpKindNoul, textResult(t, doc)); err == nil {
			t.Fatal("Parse on null answer succeeded, want error")
		}
	})
	t.Run("empty verdicts array is an error naming the field", func(t *testing.T) {
		if _, err := a.Parse(OpKindNoul, textResult(t, `{"verdicts": []}`)); err == nil {
			t.Fatal("Parse on empty verdicts succeeded, want error")
		}
	})
	t.Run("gate decision with escalate flag", func(t *testing.T) {
		doc := `{"decision": "deny", "confidence": 0.91, "confidenceFrom": "reported", "hint": "writes outside the workspace"}`
		vs, err := a.Parse(OpKindGate, textResult(t, doc))
		if err != nil {
			t.Fatalf("Parse: %v", err)
		}
		v := vs[0]
		if v.Answer != "deny" || v.Escalate || v.Hint != "writes outside the workspace" {
			t.Fatalf("verdict = %+v", v)
		}
	})
	t.Run("gate escalate decision sets the flag", func(t *testing.T) {
		vs, err := a.Parse(OpKindGate, textResult(t, `{"decision": "escalate", "confidence": 0.4}`))
		if err != nil {
			t.Fatalf("Parse: %v", err)
		}
		if !vs[0].Escalate {
			t.Fatalf("verdict = %+v, want escalate", vs[0])
		}
	})
	t.Run("gate missing decision is an error", func(t *testing.T) {
		if _, err := a.Parse(OpKindGate, textResult(t, `{}`)); err == nil {
			t.Fatal("Parse on empty gate result succeeded, want error")
		}
	})
	t.Run("structured content path", func(t *testing.T) {
		res := &mcp.CallToolResult{StructuredContent: map[string]any{
			"decision": "allow", "confidence": 0.9,
		}}
		vs, err := a.Parse(OpKindGate, res)
		if err != nil {
			t.Fatalf("Parse: %v", err)
		}
		if vs[0].Answer != "allow" || vs[0].Escalate {
			t.Fatalf("verdict = %+v", vs[0])
		}
	})
}

// TestJevUseProbes pins the two-entry battery: one judge call carrying all
// three question types, one advisory gate check.
func TestJevUseProbes(t *testing.T) {
	probes := (jevUse{}).Probes()
	if len(probes) != 2 {
		t.Fatalf("probes = %d, want 2 (judge + gate)", len(probes))
	}
	judge := probes[0]
	if judge.Tool != "jev_judge" || judge.SkipReason != "" {
		t.Fatalf("judge probe = %+v", judge)
	}
	qs, ok := judge.Args["questions"].([]map[string]any)
	if !ok || len(qs) != 3 {
		t.Fatalf("questions = %v, want 3 (noul, choice, score)", judge.Args["questions"])
	}
	if probes[1].Tool != "jev_gate" {
		t.Fatalf("second probe = %+v, want jev_gate", probes[1])
	}
}
