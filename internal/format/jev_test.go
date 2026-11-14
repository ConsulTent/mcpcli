package format

import (
	"bytes"
	"strings"
	"testing"

	"github.com/ConsulTent/mcpcli/internal/jev"
)

// hostileESC is the planted terminal-escape payload every render sink must
// strip: an OSC title-setter plus a color-introducer, the two shapes a
// server-controlled string could use to hijack the operator's terminal.
const hostileESC = "\x1b]0;pwned\x07\x1b[31m"

// assertNoESC fails when s carries the ANSI escape byte (0x1b) — the hostile
// byte every human-mode render sink must strip.
func assertNoESC(t *testing.T, name, s string) {
	t.Helper()
	if i := strings.IndexByte(s, 0x1b); i >= 0 {
		t.Fatalf("%s has an ESC byte at offset %d: %q", name, i, s)
	}
}

// TestJevVerdictsPreservesNonASCII pins the IsPrint contract's other half:
// the operator's own non-ASCII text (choice options, score levels and their
// legends echo back through the flavors' Build/Parse round trip) must render
// intact — the blanking defect an ASCII-only filter would introduce at
// exit 0, with no other test failing.
func TestJevVerdictsPreservesNonASCII(t *testing.T) {
	conf := 0.9
	verdicts := []jev.Verdict{{
		Type:          jev.OpKindChoice,
		Answer:        "是",
		Confidence:    &conf,
		Label:         "déployer",
		Probabilities: map[string]float64{"是": 0.6, "否": 0.4},
		Legend:        map[string]string{"高": " café"},
	}}
	var buf bytes.Buffer
	if err := JevVerdicts(&buf, verdicts, false); err != nil {
		t.Fatalf("JevVerdicts() error = %v", err)
	}
	got := buf.String()
	for _, want := range []string{"是", "否", " café", "déployer"} {
		if !strings.Contains(got, want) {
			t.Errorf("verdict line %q does not preserve %q", got, want)
		}
	}
}

// TestJevVerdictsFiltersControlRunes plants ESC in EVERY server-supplied
// string JevVerdicts renders — the audit plan's single-fixture enumeration:
// the answer-string arm, label, reason, hint, one reason code, one
// probability key, and one legend key plus its value. A later refactor
// dropping the filter from any one sink cannot keep this cell green.
func TestJevVerdictsFiltersControlRunes(t *testing.T) {
	verdicts := []jev.Verdict{{
		Type:          jev.OpKindChoice,
		Answer:        hostileESC + "ship",
		Label:         hostileESC + "likely",
		Probabilities: map[string]float64{hostileESC + "ship": 0.5},
		Legend:        map[string]string{hostileESC + "lvl": hostileESC + "high"},
		ReasonCodes:   []string{hostileESC + "claims_unverified"},
		Reason:        hostileESC + "unsure",
		Hint:          hostileESC + "review",
		Escalate:      true,
	}}
	var buf bytes.Buffer
	if err := JevVerdicts(&buf, verdicts, false); err != nil {
		t.Fatalf("JevVerdicts() error = %v", err)
	}
	assertNoESC(t, "verdict line", buf.String())
}

// TestSafePrint pins the shared printable-character filter beside its
// safeMIME sibling: printable Unicode survives (the operator's own text),
// control runes collapse to single spaces, and the cap counts runes.
func TestSafePrint(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"passthrough incl non-ascii", "café 是", "café 是"},
		{"collapses escapes to spaces", "bad" + hostileESC + "key", "bad ]0;pwned [31mkey"},
		{"caps at 160 runes", strings.Repeat("é", 200), strings.Repeat("é", 160) + "..."},
		{"leading controls stay trimmed", hostileESC + "tail", "]0;pwned [31mtail"},
		{"trailing collapse artifact trimmed", "boom\x07", "boom"},
		{"legitimate leading space survives", " café", " café"},
		{"empty", "", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := safePrint(tt.in); got != tt.want {
				t.Errorf("safePrint(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}
