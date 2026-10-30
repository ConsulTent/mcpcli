// Package jev adapts one canonical Jev question model onto the tool surfaces
// of the known Jev MCP servers ("flavors"), which share no tool naming.
//
// Jev is TypeSafe's small-judgment model: it answers typed questions rather
// than generating text. The canonical primitives follow TypeSafe's own
// vocabulary (docs.typesafe.ai): a noul is the probability that a statement
// is true (near 0.5 means uncertain; noul answers carry no confidence), a
// choice picks one option from a set, and a score is a position on ordered
// levels. The adapter layer exists because the three servers expose these
// (and more) under different names and shapes:
//
//   - jkudish (github.com/jkudish/jev-mcp): twelve judgment tools — jev_noul,
//     jev_verify, jev_screen, jev_decide, and nine more
//   - jev-use (github.com/shitianfang/jev-use): jev_judge batches the three
//     question types in one call; jev_gate is a PreToolUse permission gate
//   - legostin (github.com/legostin/jev-mcp): browser automation driven by
//     Jev — detectable, but its tools are not judgment primitives, so only
//     jev_doctor is probed
//
// The jev_gate name collides across flavors (jkudish: a PR/diff gate; jev-use:
// a PreToolUse permission gate), so ops map by semantics, never by name.
package jev

import (
	"encoding/json"
	"fmt"
	"strings"
)

// OpKind identifies one canonical operation. The zero value is not a valid
// op: parsing an unknown op is an error, never a silent default.
type OpKind int

// The canonical operations. Noul, Choice and Score exist on every judgment
// flavor (the TypeSafe primitives); Verify and Screen are jkudish-only; Gate
// is the jev-use PreToolUse check.
const (
	OpKindUnknown OpKind = iota
	OpKindNoul
	OpKindChoice
	OpKindScore
	OpKindVerify
	OpKindScreen
	OpKindGate
)

// String returns the canonical (TypeSafe) primitive name, used in errors,
// check output and the JSON document.
func (o OpKind) String() string {
	switch o {
	case OpKindNoul:
		return "noul"
	case OpKindChoice:
		return "choice"
	case OpKindScore:
		return "score"
	case OpKindVerify:
		return "verify"
	case OpKindScreen:
		return "screen"
	case OpKindGate:
		return "gate"
	default:
		return "unknown"
	}
}

// MarshalJSON renders the primitive name, mirroring String, so verdicts carry
// "noul"/"choice"/… instead of ints.
func (o OpKind) MarshalJSON() ([]byte, error) {
	return json.Marshal(o.String())
}

// Question is one canonical ask. Build maps it onto a flavor's tool call;
// fields the target op or flavor does not carry are the caller's business
// (the CLI warns before dropping them).
type Question struct {
	Text         string   // the question, proposition, or text to judge
	Purpose      string   // screen: what the text will be used for
	Context      string   // optional shared context/state
	Claims       []string // verify: claim texts
	Evidence     []string // verify: evidence texts; choice: context evidence
	Options      []string // choice: ordered option labels (2–6 on jev_decide)
	Levels       []string // score: ordered level descriptions
	Priorities   string   // choice on jkudish: priorities for jev_decide
	Threshold    float64  // confidence/auto-accept threshold when HasThreshold
	HasThreshold bool

	// Gate (jev-use PreToolUse): the action being gated, verbatim.
	Tool  string
	Input map[string]any
	State string
}

// Verdict is one typed answer. TypeSafe's fields (Answer, Confidence,
// Probabilities, Legend) are the base; flavor extensions (Label, Reason,
// Hint, ReasonCodes, SafeToApply) appear only when the server supplied them.
// A noul verdict has no Confidence (TypeSafe canon: the probability itself
// carries the uncertainty); Parse never returns a nil Answer.
type Verdict struct {
	Type          OpKind             `json:"type"`
	Answer        any                `json:"answer"` // per Type: float64 (noul, score), string (choice, gate decision, verify verdict, screen action)
	Confidence    *float64           `json:"confidence,omitempty"`
	Probabilities map[string]float64 `json:"probabilities,omitempty"`
	Legend        map[string]string  `json:"legend,omitempty"`
	Label         string             `json:"label,omitempty"`        // jkudish noul classification: likely/unlikely/uncertain
	Reason        string             `json:"reason,omitempty"`       // jev-use escalate reason
	Hint          string             `json:"hint,omitempty"`         // escalate guidance, or a screen recommendation reason
	ReasonCodes   []string           `json:"reason_codes,omitempty"` // jkudish gate/review codes
	SafeToApply   *float64           `json:"safe_to_apply,omitempty"`
	Escalate      bool               `json:"escalate,omitempty"`
}

// authPhrases are the non-numeric markers of a provider rejecting its key.
// The list is intentionally SEPARATE from connection's connect-time
// unauthorized heuristic (internal/connection): 402/billing wording is right
// here (provider billing) but would wrongly widen the connect retry hint.
var authPhrases = []string{"unauthorized", "invalid api key", "payment required", "billing"}

// authCodes are the HTTP status codes a provider rejection surfaces as.
var authCodes = []string{"401", "402", "403"}

// IsAuthError reports whether err looks like a Jev provider rejecting its API
// key — an unpaid bill, a rotated key, a 401/402/403 — as opposed to a tool
// bug or transport failure. It is deliberately a text heuristic (providers
// surface rejection through free-form tool error text): case-insensitive,
// numeric codes matched token-bounded (non-alphanumeric delimiters on both
// sides), phrases matched as substrings. Rate limits are NOT auth-class — a
// throttled key is still valid — and an empty error never matches.
func IsAuthError(err error) bool {
	if err == nil {
		return false
	}
	s := strings.ToLower(err.Error())
	for _, code := range authCodes {
		if tokenBounded(s, code) {
			return true
		}
	}
	for _, p := range authPhrases {
		if strings.Contains(s, p) {
			return true
		}
	}
	return false
}

// tokenBounded reports whether the numeric code occurs in s delimited by
// non-alphanumerics on both sides, so "401" matches "http 401" but not
// "took 14012ms", "id 4012", or "https://x/401abc".
func tokenBounded(s, code string) bool {
	for i := 0; i < len(s); {
		j := strings.Index(s[i:], code)
		if j < 0 {
			return false
		}
		start, end := i+j, i+j+len(code)
		if (start == 0 || !isAlnum(s[start-1])) && (end == len(s) || !isAlnum(s[end])) {
			return true
		}
		i = end
	}
	return false
}

func isAlnum(c byte) bool {
	return c >= '0' && c <= '9' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z'
}

// Errorf is the package's error constructor: every error names the op (and,
// where relevant, the flavor and field) so callers can report exactly what
// was rejected without re-wrapping.
func opError(op OpKind, format string, args ...any) error {
	return fmt.Errorf("jev %s: %s", op, fmt.Sprintf(format, args...))
}
