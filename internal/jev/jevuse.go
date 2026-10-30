package jev

import (
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// jevUse adapts github.com/shitianfang/jev-use: one jev_judge tool batching
// the three TypeSafe question types (with confidence and escalate semantics
// on every verdict), plus jev_gate — a PreToolUse permission check that can
// deny or ask but never grant. Wire shapes follow the flavor's reference doc.
type jevUse struct{}

func (jevUse) Name() string         { return "jev-use" }
func (jevUse) Vocabulary() []string { return []string{"jev_judge", "jev_gate"} }

// Detect matches the flavor's unambiguous signature: the jev_judge tool.
func (jevUse) Detect(tools []*mcp.Tool) bool {
	for _, t := range tools {
		if t.Name == "jev_judge" {
			return true
		}
	}
	return false
}

func (jevUse) Supports(op OpKind) Support {
	switch op {
	case OpKindNoul, OpKindChoice, OpKindScore:
		return Support{Supported: true, Via: "jev_judge"}
	case OpKindGate:
		return Support{Supported: true, Via: "jev_gate"}
	case OpKindVerify:
		return Support{Note: "no claims-vs-evidence tool; nearest: jev_judge with a noul question per claim"}
	case OpKindScreen:
		return Support{Note: "no injection screener; nearest: jev_judge with a noul question about the text"}
	default:
		return Support{Note: "unknown op"}
	}
}

func (jevUse) Build(op OpKind, q Question) (string, map[string]any, error) {
	question := map[string]any{"id": "q", "type": op.String(), "question": q.Text}
	switch op {
	case OpKindNoul:
		// nothing extra: the question text is the proposition
	case OpKindChoice:
		if len(q.Options) < 2 {
			return "", nil, opError(op, "a choice needs at least 2 options, got %d", len(q.Options))
		}
		options := make(map[string]string, len(q.Options))
		for _, label := range q.Options {
			options[label] = label
		}
		question["options"] = options
	case OpKindScore:
		if len(q.Levels) < 2 {
			return "", nil, opError(op, "a score needs at least 2 levels, got %d", len(q.Levels))
		}
		question["levels"] = q.Levels
	case OpKindGate:
		if q.Tool == "" {
			return "", nil, opError(op, "a tool name is required")
		}
		args := map[string]any{"state": q.State, "tool": q.Tool, "input": q.Input}
		if q.HasThreshold {
			args["confidence_threshold"] = q.Threshold
		}
		return "jev_gate", args, nil
	default:
		s := (jevUse{}).Supports(op)
		return "", nil, opError(op, "unsupported on the jev-use flavor: %s", s.Note)
	}
	args := map[string]any{
		"state":     q.Context,
		"questions": []map[string]any{question},
	}
	if q.HasThreshold {
		args["confidence_threshold"] = q.Threshold
	}
	return "jev_judge", args, nil
}

// Parse converts one canonical op's result, enforcing the per-op answer
// typing: noul and score answers must be numbers, choice and gate decisions
// strings — a null or wrong-typed answer is a parse error, never a panic.
func (a jevUse) Parse(op OpKind, res *mcp.CallToolResult) ([]Verdict, error) {
	payload, err := resultPayload(res)
	if err != nil {
		return nil, opError(op, "%s", err)
	}
	if op == OpKindGate {
		return a.parseGate(payload)
	}
	var doc struct {
		Verdicts []struct {
			ID             string             `json:"id"`
			Type           string             `json:"type"`
			Answer         any                `json:"answer"`
			Confidence     *float64           `json:"confidence"`
			ConfidenceFrom string             `json:"confidenceFrom"`
			Escalate       bool               `json:"escalate"`
			Reason         string             `json:"reason"`
			Hint           string             `json:"hint"`
			Distribution   map[string]float64 `json:"distribution"`
			Legend         map[string]string  `json:"legend"`
		} `json:"verdicts"`
	}
	if err := decodePayload(payload, &doc); err != nil {
		return nil, opError(op, "%s", err)
	}
	if len(doc.Verdicts) == 0 {
		return nil, opError(op, "missing field: verdicts")
	}
	v := doc.Verdicts[0]
	verdict := Verdict{
		Type:          op,
		Confidence:    v.Confidence,
		Reason:        v.Reason,
		Hint:          v.Hint,
		Probabilities: v.Distribution,
		Legend:        v.Legend,
		Escalate:      v.Escalate,
	}
	switch op {
	case OpKindNoul, OpKindScore:
		f, ok := v.Answer.(float64)
		if !ok {
			return nil, opError(op, "%s", &fieldError{field: "answer", got: v.Answer, want: "a number"})
		}
		verdict.Answer = f
	case OpKindChoice:
		s, ok := v.Answer.(string)
		if !ok {
			return nil, opError(op, "%s", &fieldError{field: "answer", got: v.Answer, want: "a string"})
		}
		verdict.Answer = s
	default:
		s := a.Supports(op)
		return nil, opError(op, "unsupported on the jev-use flavor: %s", s.Note)
	}
	return []Verdict{verdict}, nil
}

// parseGate converts a jev_gate decision. The gate is advisory — it can deny
// or ask, never grant — so the verdict reports the decision without any
// grant wording.
func (jevUse) parseGate(payload map[string]any) ([]Verdict, error) {
	var doc struct {
		Decision       string   `json:"decision"`
		Confidence     *float64 `json:"confidence"`
		ConfidenceFrom string   `json:"confidenceFrom"`
		Hint           string   `json:"hint"`
	}
	if err := decodePayload(payload, &doc); err != nil {
		return nil, opError(OpKindGate, "%s", err)
	}
	if doc.Decision == "" {
		return nil, opError(OpKindGate, "missing field: decision")
	}
	return []Verdict{{
		Type:       OpKindGate,
		Answer:     doc.Decision,
		Confidence: doc.Confidence,
		Hint:       doc.Hint,
		Escalate:   doc.Decision == "escalate",
	}}, nil
}

// Probes exercises both tools: all three question types share one jev_judge
// call (latency is flat in question count), and the gate checks a benign
// write it never performs — jev_gate only advises.
func (jevUse) Probes() []Probe {
	return []Probe{
		{Tool: "jev_judge", Title: "noul, choice and score in one call", Args: map[string]any{
			"state": "battery self-test",
			"questions": []map[string]any{
				{"id": "noul", "type": "noul", "question": "2+2=4?"},
				{"id": "choice", "type": "choice", "question": "next action?",
					"options": map[string]string{"proceed": "keep going", "stop": "stop here"}},
				{"id": "score", "type": "score", "question": "how routine is this self-test?",
					"levels": []string{"routine", "notable"}},
			},
		}},
		{Tool: "jev_gate", Title: "a benign write, advised on only", Args: map[string]any{
			"state": "battery self-test",
			"tool":  "write_file",
			"input": map[string]any{"path": "battery.txt", "content": "hello"},
		}},
	}
}
