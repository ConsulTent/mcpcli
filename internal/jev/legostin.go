package jev

import (
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// legostin adapts github.com/legostin/jev-mcp: browser automation driven by
// Jev per-element decisions. The flavor publishes no argument schemas and its
// tools are not judgment primitives, so the adapter detects it, probes its
// self-test (jev_doctor), and points everything else at tools call rather
// than guessing at shapes it cannot verify.
type legostin struct{}

func (legostin) Name() string         { return "legostin" }
func (legostin) Vocabulary() []string { return []string{"jev_doctor"} }

// Detect matches the flavor's signature: its task driver and its self-test.
func (legostin) Detect(tools []*mcp.Tool) bool {
	var task, doctor bool
	for _, t := range tools {
		switch t.Name {
		case "jev_task":
			task = true
		case "jev_doctor":
			doctor = true
		}
	}
	return task && doctor
}

func (legostin) Supports(OpKind) Support {
	return Support{Note: "browser-automation flavor: its tools drive Chrome rather than answer judgment primitives; call them directly via tools call (e.g. tools call jev_ask)"}
}

func (a legostin) Build(op OpKind, _ Question) (string, map[string]any, error) {
	return "", nil, opError(op, "unsupported on the legostin flavor: %s", a.Supports(op).Note)
}

func (a legostin) Parse(op OpKind, _ *mcp.CallToolResult) ([]Verdict, error) {
	return nil, opError(op, "unsupported on the legostin flavor: %s", a.Supports(op).Note)
}

// Probes is the flavor's doctor-only battery. Every other tool the server
// exposes is reported as a skip row by check itself (unknown to the
// vocabulary), so nothing here drives a browser.
func (legostin) Probes() []Probe {
	return []Probe{
		{Tool: "jev_doctor", Title: "self-test", Args: map[string]any{}},
	}
}
