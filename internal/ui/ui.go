// Package ui is mcp's light output layer: three decoration modes (Rich,
// Plain, JSON) over the go-gitmoji-cli UI stack (bubbletea, bubbles, lipgloss).
//
// The wrapper contains no rendering logic of its own beyond mode switches and
// icon constants. go-gitmoji-cli/pkg/ui itself exports only interactive
// full-screen flows (CommitPrompt, ListRun, TextInputsRun, ConfirmationRun)
// and a Spinner hard-wired to os.Stdout and os.Exit; its style helpers are
// unexported. The wrapper therefore calls through to the same components
// pkg/ui is built on: RunWithSpinner drives bubbles/spinner + bubbletea with
// pkg/ui's exact spinner configuration (Dot glyphs, pink 205) but aimed at
// stderr, and the step prefix reuses pkg/ui's pink accent via lipgloss.
//
// Every Step/Success/Fail line goes to the writer the caller
// passes (stderr in production), RunWithSpinner decorates stderr, and JSON
// mode writes nothing at all.
package ui

import (
	"context"
	"fmt"
	"io"
	"os"
	"sync"
	"time"

	"github.com/charmbracelet/bubbles/spinner"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

// Mode selects the decoration level for every function in this package.
type Mode int

const (
	// Rich is the default: emoji icons, ANSI styles, animated spinner.
	Rich Mode = iota
	// Plain restricts output to ASCII: no emoji, no ANSI escape sequences.
	Plain
	// JSON writes nothing; the caller owns all output.
	JSON
)

// Rich-mode icons.
const (
	IconSuccess = "✅"
	IconFail    = "❌"
	IconWarn    = "⚠️"
)

// Plain-mode ASCII prefixes, one per icon.
const (
	prefixOK    = "OK:"
	prefixError = "ERROR:"
	prefixWarn  = "WARN:"
)

// Current selects how the package decorates output. The cli layer sets it
// from the global flags; tests set it directly. The zero value is Rich.
var Current = Rich

// spinnerOut is where RunWithSpinner decoration goes; stderr by contract.
// Tests redirect it to a buffer.
var spinnerOut io.Writer = os.Stderr

// SetSpinnerOutput redirects spinner decoration to w and returns a restore
// function that puts stderr back. Production code never calls it; tests in
// other packages use it to capture the decoration that normally goes to
// stderr.
func SetSpinnerOutput(w io.Writer) (restore func()) {
	old := spinnerOut
	spinnerOut = w
	return func() { spinnerOut = old }
}

// stepStyle mirrors the pink go-gitmoji-cli/pkg/ui applies to its interactive
// elements (spinner glyphs, focused inputs).
var stepStyle = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("205"))

// Step announces the beginning of a unit of work: a styled "==> msg" line in
// Rich, a plain "==> msg" line in Plain, nothing in JSON.
func Step(w io.Writer, format string, args ...any) {
	switch Current {
	case JSON:
	case Plain:
		fmt.Fprintln(w, "==> "+fmt.Sprintf(format, args...))
	default:
		fmt.Fprintln(w, stepStyle.Render("==> "+fmt.Sprintf(format, args...)))
	}
}

// Success reports a completed unit of work: "✅ msg" in Rich, "OK: msg" in
// Plain, nothing in JSON.
func Success(w io.Writer, format string, args ...any) {
	status(w, IconSuccess, prefixOK, format, args...)
}

// Fail reports a failed unit of work: "❌ msg" in Rich, "ERROR: msg" in Plain,
// nothing in JSON.
func Fail(w io.Writer, format string, args ...any) {
	status(w, IconFail, prefixError, format, args...)
}

// warn reports a problem the run can continue past: "⚠️ msg" in Rich,
// "WARN: msg" in Plain, nothing in JSON. Unexported: runRich's
// spinner-failure note is its only caller.
func warn(w io.Writer, format string, args ...any) {
	status(w, IconWarn, prefixWarn, format, args...)
}

// status writes one status line: icon + message in Rich, ASCII prefix +
// message in Plain, nothing in JSON.
func status(w io.Writer, icon, prefix, format string, args ...any) {
	switch Current {
	case JSON:
	case Plain:
		fmt.Fprintln(w, prefix+" "+fmt.Sprintf(format, args...))
	default:
		fmt.Fprintln(w, icon+" "+fmt.Sprintf(format, args...))
	}
}

// RunWithSpinner runs fn, passing ctx through unchanged, decorating the wait
// according to Current. Rich animates "<spinner> label…" on stderr and
// finishes with "✅ label done in <dur>" or "❌ label failed: <err>"; Plain
// prints a start line and an end line with duration/error; JSON only runs fn.
// fn's error propagates in every mode.
func RunWithSpinner(ctx context.Context, label string, fn func(ctx context.Context) error) error {
	switch Current {
	case JSON:
		return fn(ctx)
	case Plain:
		return runPlain(ctx, label, fn)
	default:
		return runRich(ctx, label, fn)
	}
}

// runPlain is the Plain-mode spinner: start line, fn, end line, no animation.
func runPlain(ctx context.Context, label string, fn func(ctx context.Context) error) error {
	fmt.Fprintln(spinnerOut, "==> "+label+"...")
	start := time.Now()
	err := fn(ctx)
	dur := time.Since(start).Round(time.Millisecond)
	if err != nil {
		fmt.Fprintf(spinnerOut, "%s %s failed in %s: %v\n", prefixError, label, dur, err)
		return err
	}
	fmt.Fprintf(spinnerOut, "%s %s done in %s\n", prefixOK, label, dur)
	return nil
}

// runRich is the Rich-mode spinner. On a terminal it drives bubbles/spinner
// through bubbletea (input disabled, output on stderr) exactly the way
// go-gitmoji-cli/pkg/ui configures its spinner. Without a terminal the
// animation is pointless noise, so it degrades to a start line plus the final
// result line. fn runs in both cases and its error propagates; fn's outcome is
// authoritative, so a bubbletea program failure never turns a successful fn
// into a caller-visible error (see combineSpinnerOutcome).
func runRich(ctx context.Context, label string, fn func(ctx context.Context) error) error {
	start := time.Now()
	if !isTerminal(spinnerOut) {
		fmt.Fprintln(spinnerOut, "==> "+label+"...")
		err := fn(ctx)
		return finishRich(label, time.Since(start).Round(time.Millisecond), err)
	}

	res := struct {
		sync.WaitGroup
		err error
	}{}
	res.Add(1)
	p := tea.NewProgram(newSpinnerModel(label),
		tea.WithOutput(spinnerOut),
		tea.WithInput(nil), // no keyboard input; the spinner is not interactive
	)
	go func() {
		defer res.Done()
		res.err = fn(ctx)
		p.Send(doneMsg{})
	}()
	_, perr := p.Run()
	res.Wait() // fn completed regardless of the program's fate; no leaks
	dur := time.Since(start).Round(time.Millisecond)
	if perr != nil && res.err == nil {
		// fn succeeded; its outcome is authoritative. The animation itself
		// failed (rare: the terminal went away), which is worth one line on
		// stderr but must not fail the caller.
		warn(spinnerOut, "spinner animation failed: %v", perr)
	}
	return finishRich(label, dur, combineSpinnerOutcome(res.err, perr))
}

// combineSpinnerOutcome decides what RunWithSpinner returns when both the
// wrapped fn and the bubbletea program have finished: fn's outcome is
// authoritative. A program failure never turns a successful fn into a
// caller-visible error, and fn's error always wins even when the program also
// failed.
func combineSpinnerOutcome(fnErr, perr error) error {
	_ = perr // deliberately ignored: the spinner is decoration, not the result
	if fnErr != nil {
		return fnErr
	}
	return nil
}

// finishRich writes the final result line and returns err unchanged.
func finishRich(label string, dur time.Duration, err error) error {
	if err != nil {
		fmt.Fprintf(spinnerOut, "%s %s failed: %v\n", IconFail, label, err)
		return err
	}
	fmt.Fprintf(spinnerOut, "%s %s done in %s\n", IconSuccess, label, dur)
	return nil
}

// doneMsg tells the spinner model the operation finished.
type doneMsg struct{}

// spinnerModel animates "label…" with the same look go-gitmoji-cli/pkg/ui
// gives its spinner: bubbles' Dot glyphs in pink 205.
type spinnerModel struct {
	spinner spinner.Model
	label   string
	done    bool
}

func newSpinnerModel(label string) spinnerModel {
	s := spinner.New()
	s.Spinner = spinner.Dot
	s.Style = lipgloss.NewStyle().Foreground(lipgloss.Color("205"))
	return spinnerModel{spinner: s, label: label}
}

func (m spinnerModel) Init() tea.Cmd { return m.spinner.Tick }

func (m spinnerModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	if _, ok := msg.(doneMsg); ok {
		m.done = true
		return m, tea.Quit
	}
	var cmd tea.Cmd
	m.spinner, cmd = m.spinner.Update(msg)
	return m, cmd
}

func (m spinnerModel) View() string {
	if m.done {
		return ""
	}
	return m.spinner.View() + " " + m.label + "…"
}

// isTerminal reports whether w is (likely) an interactive terminal, via the
// stdlib character-device heuristic, keeping the package free of extra
// dependencies. A bytes.Buffer or io.Discard is never a terminal.
func isTerminal(w io.Writer) bool {
	f, ok := w.(*os.File)
	if !ok {
		return false
	}
	fi, err := f.Stat()
	return err == nil && fi.Mode()&os.ModeCharDevice != 0
}
