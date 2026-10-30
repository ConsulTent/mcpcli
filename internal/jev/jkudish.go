package jev

import (
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// jkudish adapts github.com/jkudish/jev-mcp: twelve judgment tools, TypeSafe
// provider by default. Wire shapes follow the flavor's README; the noul
// result envelope is not documented there and is read tolerantly (see
// parseNoulResult).
type jkudish struct{}

// jkudishTools is the full vocabulary, in battery order.
var jkudishTools = []string{
	"jev_noul", "jev_verify", "jev_screen", "jev_decide", "jev_classify",
	"jev_compare", "jev_find", "jev_rerank", "jev_extract", "jev_audit",
	"jev_review", "jev_gate",
}

func (jkudish) Name() string         { return "jkudish" }
func (jkudish) Vocabulary() []string { return jkudishTools }

// Detect matches the flavor's signature: at least two of its four core
// judgment tools. The threshold keeps a single shared name (say, a foreign
// server that also happens to expose jev_noul) from being misread.
func (jkudish) Detect(tools []*mcp.Tool) bool {
	n := 0
	for _, t := range tools {
		switch t.Name {
		case "jev_noul", "jev_verify", "jev_screen", "jev_decide":
			n++
		}
	}
	return n >= 2
}

func (jkudish) Supports(op OpKind) Support {
	switch op {
	case OpKindNoul:
		return Support{Supported: true, Via: "jev_noul"}
	case OpKindChoice:
		return Support{Supported: true, Via: "jev_decide"}
	case OpKindVerify:
		return Support{Supported: true, Via: "jev_verify"}
	case OpKindScreen:
		return Support{Supported: true, Via: "jev_screen"}
	case OpKindScore:
		return Support{Note: "no ordered-levels tool; nearest: jev_classify (unordered classes) or jev_compare (two passages)"}
	case OpKindGate:
		return Support{Note: "jev_gate here is a PR/diff completion gate, not a PreToolUse permission gate; call it directly via tools call jev_gate"}
	default:
		return Support{Note: "unknown op"}
	}
}

// Probes is the flavor's self-test battery: one minimal read-only call per
// tool, every fixture advisory (fabricated one-line inputs — nothing here
// mutates state or spends more than a fraction of a cent). Args follow the
// flavor README's documented shapes.
func (jkudish) Probes() []Probe {
	return []Probe{
		{Tool: "jev_noul", Title: "one trivially true proposition", Args: map[string]any{
			"propositions": []string{"2+2=4"},
		}},
		{Tool: "jev_verify", Title: "one claim with matching evidence", Args: map[string]any{
			"claims":   []string{"the build passed"},
			"evidence": []map[string]any{{"text": "214 tests passed, 0 failed"}},
		}},
		{Tool: "jev_screen", Title: "one benign text", Args: map[string]any{
			"text": "Please review this patch when you have time.",
		}},
		{Tool: "jev_decide", Title: "a two-candidate decision", Args: map[string]any{
			"decision":   "how to acknowledge a teammate's PR comment",
			"evidence":   "the comment says: nice fix, thanks",
			"priorities": "be concise and kind",
			"candidates": []map[string]any{
				{"id": "thank", "description": "reply with a short thanks"},
				{"id": "emoji", "description": "react with an emoji"},
			},
		}},
		{Tool: "jev_classify", Title: "one item into two classes", Args: map[string]any{
			"purpose": "sentiment",
			"items":   []map[string]any{{"id": "m1", "text": "works great, thanks"}},
			"classes": []map[string]any{
				{"id": "positive", "description": "a happy message"},
				{"id": "neutral", "description": "a matter-of-fact message"},
			},
		}},
		{Tool: "jev_compare", Title: "two identical passages", Args: map[string]any{
			"passage_a": "the sky is blue",
			"passage_b": "the sky is blue",
		}},
		{Tool: "jev_find", Title: "a query over two candidates", Args: map[string]any{
			"query": "a mention of the deploy pipeline",
			"candidates": []map[string]any{
				{"id": "c1", "text": "the deploy pipeline ran"},
				{"id": "c2", "text": "the lunch menu"},
			},
		}},
		{Tool: "jev_rerank", Title: "a query over two candidates", Args: map[string]any{
			"query": "the deploy pipeline",
			"candidates": []map[string]any{
				{"id": "c1", "text": "the deploy pipeline ran"},
				{"id": "c2", "text": "the lunch menu"},
			},
		}},
		{Tool: "jev_extract", Title: "one regex field from one line", Args: map[string]any{
			"document": "invoice total: $42.00",
			"fields":   []map[string]any{{"id": "total", "pattern": `total: \$[0-9.]+`, "description": "the total amount"}},
		}},
		{Tool: "jev_audit", Title: "one record against a source", Args: map[string]any{
			"source":  "a hardcoded ordered list",
			"records": []map[string]any{{"id": "r1", "request": "is the list ordered?", "value": "yes"}},
		}},
		{Tool: "jev_review", Title: "a one-word diff review", Args: map[string]any{
			"request": "fix a README typo",
			"diff":    "- speeling\n+ spelling\n",
			"tests":   "none; documentation only",
		}},
		{Tool: "jev_gate", Title: "a one-word diff against one claim", Args: map[string]any{
			"request": "fix a README typo",
			"diff":    "- speeling\n+ spelling\n",
			"claims":  []string{"the change is only a spelling fix"},
			"evidence": []map[string]any{
				{"text": "the diff changes exactly one word"},
			},
			"tests": "none; documentation only",
		}},
	}
}

func (a jkudish) Build(op OpKind, q Question) (string, map[string]any, error) {
	switch op {
	case OpKindNoul:
		args := map[string]any{"propositions": []string{q.Text}}
		if q.Context != "" {
			args["context"] = q.Context
		}
		if q.HasThreshold {
			args["auto_accept"] = q.Threshold
		}
		return "jev_noul", args, nil
	case OpKindChoice:
		if len(q.Options) < 2 || len(q.Options) > 6 {
			return "", nil, opError(op, "jev_decide takes 2-6 candidates, got %d", len(q.Options))
		}
		candidates := make([]map[string]any, len(q.Options))
		for i, label := range q.Options {
			candidates[i] = map[string]any{"id": label, "description": label}
		}
		args := map[string]any{
			"decision":   q.Text,
			"evidence":   strings.Join(q.Evidence, "\n"),
			"priorities": q.Priorities,
			"candidates": candidates,
		}
		return "jev_decide", args, nil
	case OpKindVerify:
		if len(q.Claims) == 0 {
			return "", nil, opError(op, "at least one claim is required")
		}
		evidence := make([]map[string]any, len(q.Evidence))
		for i, e := range q.Evidence {
			evidence[i] = map[string]any{"text": e}
		}
		args := map[string]any{"claims": q.Claims, "evidence": evidence}
		if q.HasThreshold {
			args["auto_accept"] = q.Threshold
		}
		return "jev_verify", args, nil
	case OpKindScreen:
		args := map[string]any{"text": q.Text}
		if q.Purpose != "" {
			args["purpose"] = q.Purpose
		}
		return "jev_screen", args, nil
	default:
		s := a.Supports(op)
		return "", nil, opError(op, "unsupported on the jkudish flavor: %s", s.Note)
	}
}

// Parse converts one canonical op's result. Unsupported ops are errors; the
// other nine tools are reachable via tools call and are only exercised by
// the battery, which records status without canonical parsing.
func (a jkudish) Parse(op OpKind, res *mcp.CallToolResult) ([]Verdict, error) {
	payload, err := resultPayload(res)
	if err != nil {
		return nil, opError(op, "%s", err)
	}
	switch op {
	case OpKindNoul:
		v, err := a.parseNoulResult(payload)
		if err != nil {
			return nil, opError(op, "%s", err)
		}
		return []Verdict{v}, nil
	case OpKindChoice:
		var doc struct {
			Recommendation *struct {
				Selected      string             `json:"selected"`
				Escaped       bool               `json:"escaped"`
				Confidence    *float64           `json:"confidence"`
				Probabilities map[string]float64 `json:"probabilities"`
			} `json:"recommendation"`
		}
		if err := decodePayload(payload, &doc); err != nil {
			return nil, opError(op, "%s", err)
		}
		if doc.Recommendation == nil || doc.Recommendation.Selected == "" {
			return nil, opError(op, "missing field: recommendation.selected")
		}
		return []Verdict{{
			Type:          op,
			Answer:        doc.Recommendation.Selected,
			Confidence:    doc.Recommendation.Confidence,
			Probabilities: doc.Recommendation.Probabilities,
			Escalate:      doc.Recommendation.Escaped,
		}}, nil
	case OpKindVerify:
		var doc struct {
			Results []struct {
				Claim      string   `json:"claim"`
				Verdict    string   `json:"verdict"`
				Confidence *float64 `json:"confidence"`
				Action     string   `json:"action"`
			} `json:"results"`
		}
		if err := decodePayload(payload, &doc); err != nil {
			return nil, opError(op, "%s", err)
		}
		if len(doc.Results) == 0 {
			return nil, opError(op, "missing field: results")
		}
		verdicts := make([]Verdict, 0, len(doc.Results))
		for _, r := range doc.Results {
			if r.Verdict == "" {
				return nil, opError(op, "missing field: results[].verdict")
			}
			verdicts = append(verdicts, Verdict{
				Type:       op,
				Answer:     r.Verdict,
				Confidence: r.Confidence,
				Label:      r.Action,
			})
		}
		return verdicts, nil
	case OpKindScreen:
		var doc struct {
			Probabilities  map[string]float64 `json:"probabilities"`
			Recommendation *struct {
				Action string `json:"action"`
				Reason string `json:"reason"`
			} `json:"recommendation"`
		}
		if err := decodePayload(payload, &doc); err != nil {
			return nil, opError(op, "%s", err)
		}
		if doc.Recommendation == nil || doc.Recommendation.Action == "" {
			return nil, opError(op, "missing field: recommendation.action")
		}
		return []Verdict{{
			Type:          op,
			Answer:        doc.Recommendation.Action,
			Probabilities: doc.Probabilities,
			Hint:          doc.Recommendation.Reason,
		}}, nil
	default:
		s := a.Supports(op)
		return nil, opError(op, "unsupported on the jkudish flavor: %s", s.Note)
	}
}

// parseNoulResult reads a jev_noul result. The flavor's README documents the
// semantics — one probability plus a label (likely/unlikely/uncertain) per
// proposition — but not the envelope, so this reader accepts the shapes in
// the wild: a bare {"probability":…,"label":…} object, or a
// results/propositions/answers array whose first entry carries them. The
// number is enforced as a number (float64) here, so a wrong-typed answer is
// a parse error, never a downstream panic.
func (jkudish) parseNoulResult(payload map[string]any) (Verdict, error) {
	if p, ok := payload["probability"]; ok {
		return noulVerdict(p, payload["label"])
	}
	for _, key := range []string{"results", "propositions", "answers", "noul"} {
		arr, ok := payload[key].([]any)
		if !ok || len(arr) == 0 {
			continue
		}
		item, ok := arr[0].(map[string]any)
		if !ok {
			continue
		}
		if p, ok := item["probability"]; ok {
			return noulVerdict(p, item["label"])
		}
	}
	return Verdict{}, &fieldError{field: "probability"}
}

func noulVerdict(p any, label any) (Verdict, error) {
	f, ok := p.(float64)
	if !ok {
		return Verdict{}, &fieldError{field: "probability", got: p, want: "a number"}
	}
	v := Verdict{Type: OpKindNoul, Answer: f}
	if l, ok := label.(string); ok {
		v.Label = l
	}
	return v, nil
}
