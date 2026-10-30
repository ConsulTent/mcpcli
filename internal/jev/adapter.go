package jev

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// Adapter maps the canonical question model onto one flavor's tools. Adapters
// are stateless values registered in flavors; a new flavor is one file plus
// one slice entry.
type Adapter interface {
	// Name is the flavor identifier used in output ("jkudish", "jev-use").
	Name() string

	// Detect reports whether the server's tool inventory matches this
	// flavor's signature.
	Detect(tools []*mcp.Tool) bool

	// Vocabulary lists the flavor's known tool names, so check can report
	// which of the server's jev_* tools the flavor recognizes.
	Vocabulary() []string

	// Supports describes how (or how not) one op maps onto this flavor.
	// When Supported is false, Note names the nearest alternative instead
	// of fabricating one.
	Supports(op OpKind) Support

	// Build produces the tool call for one op. An unsupported op is an
	// error carrying the Supports note.
	Build(op OpKind, q Question) (tool string, args map[string]any, err error)

	// Parse converts a successful (non-isError) tool result into verdicts,
	// enforcing the per-op answer typing: it is the single point where
	// untrusted result JSON becomes typed values, so a nil Answer or a
	// wrong-typed answer is an error here, never a downstream panic.
	Parse(op OpKind, res *mcp.CallToolResult) ([]Verdict, error)

	// Probes lists the flavor's self-test battery: one minimal read-only
	// call per supported tool (or a skip row for a tool that cannot be
	// exercised safely).
	Probes() []Probe
}

// Support is one op's availability on a flavor.
type Support struct {
	Supported bool
	Via       string // the tool the op maps onto, when supported
	Note      string // when unsupported: the nearest alternative, honestly named
}

// Probe is one battery entry: either a minimal tool call (Args set) or a
// reported skip (SkipReason set, Args nil).
type Probe struct {
	Tool       string
	Title      string
	Args       map[string]any
	SkipReason string
}

// flavors holds the built-in adapters in detection order: the first Detect
// hit wins; AdapterFor also reports which jev_* tools the winner recognizes.
var flavors = []Adapter{jkudish{}, jevUse{}, legostin{}}

// AdapterFor picks the adapter for a server's tool inventory: the first
// flavor whose signature matches (also-matched flavors are detectable but
// unreachable by ordering). matched lists the server's jev_* tools the winner
// recognizes (its Vocabulary ∩ the inventory); unmatched lists every other
// jev_* tool, so check can suggest calling them directly via tools call.
// A nil adapter means no known flavor: unmatched then holds every jev_* tool.
func AdapterFor(tools []*mcp.Tool) (adapter Adapter, matched, unmatched []string) {
	var jevTools []string
	for _, t := range tools {
		if t != nil && strings.HasPrefix(t.Name, "jev_") {
			jevTools = append(jevTools, t.Name)
		}
	}
	for _, a := range flavors {
		if !a.Detect(tools) {
			continue
		}
		vocab := make(map[string]bool, len(jevTools))
		for _, name := range a.Vocabulary() {
			vocab[name] = true
		}
		for _, name := range jevTools {
			if vocab[name] {
				matched = append(matched, name)
			} else {
				unmatched = append(unmatched, name)
			}
		}
		return a, matched, unmatched
	}
	return nil, nil, jevTools
}

// resultPayload extracts the JSON object a tool result carries: the
// StructuredContent when present, else the first text content block parsed as
// JSON. An empty result, a non-JSON text, or a payload that is not a JSON
// object is an error — Parse never inspects content blocks unchecked.
func resultPayload(res *mcp.CallToolResult) (map[string]any, error) {
	var raw any
	switch {
	case res.StructuredContent != nil:
		raw = res.StructuredContent
	default:
		var text *mcp.TextContent
		for _, c := range res.Content {
			if tc, ok := c.(*mcp.TextContent); ok {
				text = tc
				break
			}
		}
		if text == nil {
			return nil, fmt.Errorf("result carries no text or structured content")
		}
		if err := json.Unmarshal([]byte(text.Text), &raw); err != nil {
			return nil, fmt.Errorf("result text is not JSON: %w", err)
		}
	}
	obj, ok := raw.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("result is not a JSON object")
	}
	return obj, nil
}

// decodePayload re-marshals the generic payload into a typed document, so
// per-flavor parsing works with typed fields (and json's own number and
// depth limits) instead of unchecked assertions.
func decodePayload(payload map[string]any, doc any) error {
	b, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("re-encode result: %w", err)
	}
	if err := json.Unmarshal(b, doc); err != nil {
		return fmt.Errorf("decode result: %w", err)
	}
	return nil
}

// fieldError reports a malformed verdict field: missing (want empty), or
// present with the wrong type. It is the single shape-problem error Parse
// produces, so untrusted-result messages stay uniform.
type fieldError struct {
	field string
	got   any
	want  string
}

func (e *fieldError) Error() string {
	if e.want == "" {
		return "missing field: " + e.field
	}
	return fmt.Sprintf("field %s: expected %s, got %T", e.field, e.want, e.got)
}
