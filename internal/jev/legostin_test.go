package jev

import "testing"

// TestLegostinDetect pins the task+doctor signature.
func TestLegostinDetect(t *testing.T) {
	a := legostin{}
	if !a.Detect(tools("jev_task", "jev_doctor")) {
		t.Fatal("Detect(task+doctor) = false, want true")
	}
	if a.Detect(tools("jev_task")) {
		t.Fatal("Detect(task only) = true, want false")
	}
	if a.Detect(tools("jev_doctor")) {
		t.Fatal("Detect(doctor only) = true, want false")
	}
}

// TestLegostinUnsupported pins that every op is unsupported with the
// honest call-it-directly note, and Build/Parse refuse rather than guess.
func TestLegostinUnsupported(t *testing.T) {
	a := legostin{}
	for _, op := range []OpKind{OpKindNoul, OpKindChoice, OpKindScore, OpKindVerify, OpKindScreen, OpKindGate} {
		s := a.Supports(op)
		if s.Supported {
			t.Fatalf("Supports(%s) supported, want unsupported", op)
		}
		if _, _, err := a.Build(op, Question{Text: "x"}); err == nil {
			t.Fatalf("Build(%s) succeeded, want error", op)
		}
		if _, err := a.Parse(op, textResult(t, `{}`)); err == nil {
			t.Fatalf("Parse(%s) succeeded, want error", op)
		}
	}
}

// TestLegostinProbes pins the doctor-only battery: one probe, no skips, no
// browser-driving tools.
func TestLegostinProbes(t *testing.T) {
	probes := (legostin{}).Probes()
	if len(probes) != 1 || probes[0].Tool != "jev_doctor" || probes[0].SkipReason != "" {
		t.Fatalf("probes = %+v, want a single jev_doctor probe", probes)
	}
}
