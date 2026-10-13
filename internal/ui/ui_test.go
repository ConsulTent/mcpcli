package ui

import (
	"bytes"
	"context"
	"errors"
	"os"
	"strings"
	"testing"
)

type ctxKey struct{}

// setMode pins Current for the duration of the test and redirects the
// spinner's destination to a buffer so no decoration leaks into the test log.
// Both are restored automatically.
func setMode(t *testing.T, m Mode) *bytes.Buffer {
	t.Helper()
	oldMode, oldOut := Current, spinnerOut
	var buf bytes.Buffer
	Current = m
	spinnerOut = &buf
	t.Cleanup(func() { Current, spinnerOut = oldMode, oldOut })
	return &buf
}

// TestPlainModeASCIIOnly verifies that Plain mode output is pure ASCII with no
// ANSI escape sequences, and uses the OK:/ERROR:/WARN: prefixes.
func TestPlainModeASCIIOnly(t *testing.T) {
	w := setMode(t, Plain)

	Step(w, "connecting to %s:%d", "localhost", 8080)
	Success(w, "connected after %d ms", 12)
	Fail(w, "connection refused (%d)", 61)
	warn(w, "server did not advertise %s", "logging")
	if err := RunWithSpinner(context.Background(), "probe", func(context.Context) error {
		return nil
	}); err != nil {
		t.Fatalf("RunWithSpinner success: %v", err)
	}
	sentinel := errors.New("refused")
	if err := RunWithSpinner(context.Background(), "probe", func(context.Context) error {
		return sentinel
	}); !errors.Is(err, sentinel) {
		t.Fatalf("RunWithSpinner error = %v, want %v", err, sentinel)
	}

	b := w.Bytes()
	for i, c := range b {
		if c >= 0x80 {
			t.Fatalf("non-ASCII byte 0x%02x at offset %d: %q", c, i, w.String())
		}
		if c == 0x1b {
			t.Fatalf("ANSI escape byte (ESC) at offset %d: %q", i, w.String())
		}
	}
	out := w.String()
	for _, want := range []string{
		"==> connecting to localhost:8080",
		"OK: connected after 12 ms",
		"ERROR: connection refused (61)",
		"WARN: server did not advertise logging",
		"==> probe...",
		"OK: probe done in",
		"ERROR: probe failed in",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("plain output missing %q\ngot: %q", want, out)
		}
	}
}

// TestJSONQuiet verifies that JSON mode writes nothing while still running fn
// with the caller's context and propagating its error.
func TestJSONQuiet(t *testing.T) {
	w := setMode(t, JSON)

	Step(w, "step %d", 1)
	Success(w, "ok %d", 2)
	Fail(w, "bad %d", 3)
	warn(w, "warn %d", 4)
	if w.Len() != 0 {
		t.Fatalf("JSON mode wrote %q, want nothing", w.String())
	}

	ctx := context.WithValue(context.Background(), ctxKey{}, "marker")
	sentinel := errors.New("boom")
	ran := 0
	err := RunWithSpinner(ctx, "probe", func(c context.Context) error {
		ran++
		if got := c.Value(ctxKey{}); got != "marker" {
			t.Errorf("fn ctx value = %v, want marker", got)
		}
		return sentinel
	})
	if !errors.Is(err, sentinel) {
		t.Errorf("RunWithSpinner error = %v, want %v", err, sentinel)
	}
	if ran != 1 {
		t.Errorf("fn ran %d times, want 1", ran)
	}
	if w.Len() != 0 {
		t.Errorf("JSON RunWithSpinner wrote %q, want nothing", w.String())
	}
}

// TestRunWithSpinnerRunsFn verifies that fn runs exactly once, receives the
// caller's context, and its error propagates in every mode.
func TestRunWithSpinnerRunsFn(t *testing.T) {
	sentinel := errors.New("kaboom")
	for _, tc := range []struct {
		name string
		mode Mode
		done string
	}{
		{"rich", Rich, "✅ work done in"},
		{"plain", Plain, "OK: work done in"},
		{"json", JSON, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := setMode(t, tc.mode)
			called := 0
			if err := RunWithSpinner(context.Background(), "work", func(context.Context) error {
				called++
				return nil
			}); err != nil {
				t.Fatalf("success run: %v", err)
			}
			err := RunWithSpinner(context.Background(), "work", func(context.Context) error {
				called++
				return sentinel
			})
			if !errors.Is(err, sentinel) {
				t.Fatalf("failing run error = %v, want %v", err, sentinel)
			}
			if called != 2 {
				t.Fatalf("fn called %d times, want 2", called)
			}
			if tc.mode == JSON {
				if w.Len() != 0 {
					t.Errorf("JSON mode wrote %q, want nothing", w.String())
				}
				return
			}
			out := w.String()
			if !strings.Contains(out, tc.done) {
				t.Errorf("output missing %q\ngot: %q", tc.done, out)
			}
			if !strings.Contains(out, "kaboom") {
				t.Errorf("output missing fn error\ngot: %q", out)
			}
		})
	}
}

// TestCombineSpinnerOutcome pins runRich's result-precedence rule: the wrapped
// fn's outcome is authoritative. A bubbletea program failure never turns a
// successful fn into a caller-visible error, and fn's error always wins even
// when the program also failed.
func TestCombineSpinnerOutcome(t *testing.T) {
	fnErr := errors.New("op failed")
	perr := errors.New("program failed")

	cases := []struct {
		name  string
		fnErr error
		perr  error
		want  error
	}{
		{"both succeeded", nil, nil, nil},
		{"fn succeeded, program failed", nil, perr, nil},
		{"fn failed, program failed", fnErr, perr, fnErr},
		{"fn failed, program succeeded", fnErr, nil, fnErr},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := combineSpinnerOutcome(tc.fnErr, tc.perr)
			if !errors.Is(got, tc.want) {
				t.Errorf("combineSpinnerOutcome(%v, %v) = %v, want %v", tc.fnErr, tc.perr, got, tc.want)
			}
		})
	}
}

// TestRichUsesEmoji verifies that Rich mode decorates with the emoji icons.
func TestRichUsesEmoji(t *testing.T) {
	w := setMode(t, Rich)

	Success(w, "uploaded %d files", 3)
	Fail(w, "upload rejected")
	warn(w, "slow server")

	out := w.String()
	for _, tc := range []struct{ icon, fn string }{
		{"✅", "Success"},
		{"❌", "Fail"},
		{"⚠️", "warn"},
	} {
		if !strings.Contains(out, tc.icon) {
			t.Errorf("rich output missing %s icon %q\ngot: %q", tc.fn, tc.icon, out)
		}
	}

	if err := RunWithSpinner(context.Background(), "probe", func(context.Context) error {
		return nil
	}); err != nil {
		t.Fatalf("RunWithSpinner: %v", err)
	}
	if !strings.Contains(w.String(), "✅ probe done in") {
		t.Errorf("rich RunWithSpinner output missing success line\ngot: %q", w.String())
	}
}

// TestRichRunWithSpinnerRegularFile verifies the Rich fallback treats a
// non-terminal os.File (the isTerminal *os.File branch) like any other
// non-terminal: fn runs, no animation frames, final result line present.
func TestRichRunWithSpinnerRegularFile(t *testing.T) {
	setMode(t, Rich)
	f, err := os.CreateTemp(t.TempDir(), "ui-*.txt")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	oldOut := spinnerOut
	spinnerOut = f
	t.Cleanup(func() { spinnerOut = oldOut })

	ran := false
	if err := RunWithSpinner(context.Background(), "work", func(context.Context) error {
		ran = true
		return nil
	}); err != nil {
		t.Fatalf("RunWithSpinner: %v", err)
	}
	if !ran {
		t.Fatal("fn did not run")
	}
	b, err := os.ReadFile(f.Name())
	if err != nil {
		t.Fatal(err)
	}
	out := string(b)
	if strings.Contains(out, "\x1b") {
		t.Errorf("ANSI escape written to non-terminal file: %q", out)
	}
	if !strings.Contains(out, "==> work...") || !strings.Contains(out, "✅ work done in") {
		t.Errorf("unexpected output: %q", out)
	}
}
