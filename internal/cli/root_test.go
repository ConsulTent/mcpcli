package cli

import (
	"bytes"
	"io"
	"os"
	"strings"
	"testing"

	"github.com/ConsulTent/mcpcli/internal/version"
)

func TestRootRejectsBadAuth(t *testing.T) {
	cmd := NewRootCommand()
	var buf bytes.Buffer
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{"--auth", "bogus"})

	err := cmd.Execute()
	if err == nil {
		t.Fatal("expected error for --auth bogus, got nil")
	}
	// The error must name the offending value and list the registered
	// providers. Task 6 adds "oauth"; assert the built-ins registered here.
	if !strings.Contains(err.Error(), "bogus") {
		t.Errorf("error should mention the bad value, got: %v", err)
	}
	for _, want := range []string{"bearer", "client-credentials", "none"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error should list registered provider %q, got: %v", want, err)
		}
	}
}

func TestVersionFlag(t *testing.T) {
	cmd := NewRootCommand()
	var buf bytes.Buffer
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{"--version"})

	if err := cmd.Execute(); err != nil {
		t.Fatalf("--version returned error: %v", err)
	}
	// In a test binary version.Current() falls back to the baseline
	// constant, so the output is deterministic: "mcp version vX.Y.Z".
	if want := "mcp version " + version.Version; !strings.Contains(buf.String(), want) {
		t.Fatalf("--version output %q does not contain %q", buf.String(), want)
	}
}

// TestRootRejectsAuthFlowWithoutOauth: --auth-flow is silently inert without
// --auth oauth, so an explicit combination is rejected with guidance instead.
func TestRootRejectsAuthFlowWithoutOauth(t *testing.T) {
	cmd := NewRootCommand()
	var buf bytes.Buffer
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{"--auth", "none", "--auth-flow", "manual"})

	err := cmd.Execute()
	if err == nil {
		t.Fatal("--auth-flow without --auth oauth = nil error, want rejection")
	}
	if !strings.Contains(err.Error(), "--auth-flow only applies to --auth oauth") {
		t.Errorf("error %q does not explain the --auth-flow restriction", err.Error())
	}
}

func TestRootHelpShowsCopyright(t *testing.T) {
	cmd := NewRootCommand()
	var buf bytes.Buffer
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{"--help"})

	if err := cmd.Execute(); err != nil {
		t.Fatalf("--help returned error: %v", err)
	}
	for _, want := range []string{"https://consultent.ltd", "Copyright ConsulTent Pte. Ltd., all Rights Reserved."} {
		if !strings.Contains(buf.String(), want) {
			t.Errorf("--help output missing %q\ngot:\n%s", want, buf.String())
		}
	}
}

// clientSecretWarning is the one-line argv-visibility warning the root's
// PersistentPreRunE prints when --client-secret carries a literal (a value
// that is not a registered secret reference). It is functional output
// written straight to os.Stderr, so capturing it means swapping os.Stderr
// (runRoot's cobra-level buffers do not see it).
const clientSecretWarning = "warning: --client-secret was passed on the command line; prefer bws:// references or MCPCLI_CLIENT_SECRET"

// envLiteralWarning is the --env variant of the same warning: it attributes
// the leaking flag with the offending KEY (the value itself is never echoed
// — the client-secret precedent).
func envLiteralWarning(key string) string {
	return "warning: --env " + key + " was passed on the command line; prefer a bws:// reference"
}

// captureOSStderr redirects os.Stderr into a pipe and returns a function that
// closes the write end and returns everything written. mcp's warning and
// its stdout/stderr contract keep the captured volume far below the pipe
// buffer, so reading after Execute cannot deadlock.
func captureOSStderr(t *testing.T) func() string {
	t.Helper()
	old := os.Stderr
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	os.Stderr = w
	t.Cleanup(func() { os.Stderr = old })
	return func() string {
		w.Close()
		data, _ := io.ReadAll(r)
		r.Close()
		return string(data)
	}
}

// TestClientSecretFlagWarnsOnArgv pins the plan's binding constraint: setting
// --client-secret on the command line prints exactly one stderr warning
// pointing at the environment variable. info without a target is the cheapest
// failing path; PersistentPreRunE has already run when the runner rejects it.
func TestClientSecretFlagWarnsOnArgv(t *testing.T) {
	readStderr := captureOSStderr(t)
	_, _, _, err := runRoot(t, "--client-secret", "dummy", "info")
	if err == nil || !strings.Contains(err.Error(), "--url") {
		t.Fatalf("info without a target = %v, want the missing-target error", err)
	}
	if got := strings.Count(readStderr(), clientSecretWarning); got != 1 {
		t.Fatalf("argv warning count = %d, want exactly 1", got)
	}
}

// TestClientSecretEnvNoWarning verifies the warning fires for the flag only:
// credentials arriving through MCPCLI_CLIENT_SECRET are not visible in argv
// and must stay silent.
func TestClientSecretEnvNoWarning(t *testing.T) {
	t.Setenv("MCPCLI_CLIENT_SECRET", "dummy")
	readStderr := captureOSStderr(t)
	_, _, _, err := runRoot(t, "info")
	if err == nil || !strings.Contains(err.Error(), "--url") {
		t.Fatalf("info without a target = %v, want the missing-target error", err)
	}
	captured := readStderr()
	if got := strings.Count(captured, clientSecretWarning); got != 0 {
		t.Fatalf("env-only run printed the argv warning %d time(s)\ncaptured stderr:\n%s", got, captured)
	}
}

// TestRootEnvSyntaxErrors pins the golden --env parse errors in the exact
// args.go house form, byte-exact, on any command (the root hook is the one
// parse site for the flag).
func TestRootEnvSyntaxErrors(t *testing.T) {
	cases := []struct {
		name string
		args []string
		want string
	}{
		{"missing equals", []string{"--env", "garbage"}, `invalid argument "garbage": expected KEY=VALUE`},
		{"empty key", []string{"--env", "=value"}, `invalid argument "=value": expected KEY=VALUE`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, _, _, err := runRoot(t, append(tc.args, "info")...)
			if err == nil {
				t.Fatalf("%s = nil error, want %q", tc.name, tc.want)
			}
			if err.Error() != tc.want {
				t.Fatalf("error = %q, want byte-exact %q", err.Error(), tc.want)
			}
		})
	}
}

// TestParseEnvPairsLastWins pins the dedup rule of the one parse site:
// duplicate KEYs collapse last-wins, surviving pairs keep first-appearance
// order (deterministic expansion and error attribution), and both malformed
// shapes carry the args.go form.
func TestParseEnvPairsLastWins(t *testing.T) {
	bindings, err := parseEnvPairs([]string{"A=1", "B=x", "A=2", "B="})
	if err != nil {
		t.Fatalf("parseEnvPairs: %v", err)
	}
	want := []envBinding{{Key: "A", Value: "2"}, {Key: "B", Value: ""}}
	if len(bindings) != len(want) {
		t.Fatalf("bindings = %+v, want %+v", bindings, want)
	}
	for i := range want {
		if bindings[i] != want[i] {
			t.Fatalf("bindings[%d] = %+v, want %+v", i, bindings[i], want[i])
		}
	}
	for _, pair := range []string{"garbage", "=value"} {
		_, err := parseEnvPairs([]string{pair})
		if err == nil {
			t.Fatalf("parseEnvPairs(%q) = nil error, want the args.go form", pair)
		}
		if want := `invalid argument "` + pair + `": expected KEY=VALUE`; err.Error() != want {
			t.Fatalf("parseEnvPairs(%q) error = %q, want %q", pair, err.Error(), want)
		}
	}
}

// TestRootEnvGarbageFailsOnAnyCommand: the root hook validates --env for
// every command, so malformed input fails offline under the bws subtree too
// — before any bws logic (and any network) runs.
func TestRootEnvGarbageFailsOnAnyCommand(t *testing.T) {
	_, _, _, err := runRoot(t, "--env", "garbage", "bws", "list")
	if err == nil {
		t.Fatal("--env garbage under bws list = nil error, want the root-hook rejection")
	}
	if want := `invalid argument "garbage": expected KEY=VALUE`; err.Error() != want {
		t.Fatalf("error = %q, want %q", err.Error(), want)
	}
}

// TestRootEnvEmptyValueSilent: an empty --env VALUE is a deliberate empty
// injection, not a leaked literal — the argv warning stays silent (and the
// old client-secret tail must not resurface on it).
func TestRootEnvEmptyValueSilent(t *testing.T) {
	readStderr := captureOSStderr(t)
	_, _, _, err := runRoot(t, "--env", "FOO=", "info")
	if err == nil || !strings.Contains(err.Error(), "--url") {
		t.Fatalf("info without a target = %v, want the missing-target error", err)
	}
	captured := readStderr()
	if strings.Contains(captured, "warning:") {
		t.Fatalf("empty --env VALUE fired a warning\ncaptured stderr:\n%s", captured)
	}
}

// TestRootEnvWarningVariants pins the argv-warning matrix: literal values
// warn naming the leaking flag, secret references stay silent — including
// the upper-case BWS:// spelling (Recognizes lowercases the scheme) — and
// the --client-secret message carries its updated tail.
func TestRootEnvWarningVariants(t *testing.T) {
	cases := []struct {
		name       string
		args       []string
		want       string // a warning line containing this, exactly once
		wantSilent bool
	}{
		{"client-secret literal", []string{"--client-secret", "literal-token", "info"}, clientSecretWarning, false},
		{"client-secret ref silent", []string{"--client-secret", "bws://11111111-2222-3333-4444-555555555555", "info"}, "", true},
		{"env literal attributes --env", []string{"--env", "GITHUB_TOKEN=ghp_literal", "info"}, envLiteralWarning("GITHUB_TOKEN"), false},
		{"env ref silent", []string{"--env", "K=bws://name", "info"}, "", true},
		{"env uppercase ref silent", []string{"--env", "K=BWS://name", "info"}, "", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			readStderr := captureOSStderr(t)
			_, _, _, err := runRoot(t, tc.args...)
			if err == nil || !strings.Contains(err.Error(), "--url") {
				t.Fatalf("info without a target = %v, want the missing-target error", err)
			}
			captured := readStderr()
			if tc.wantSilent {
				if strings.Contains(captured, "warning:") {
					t.Fatalf("%s: run printed a warning\ncaptured stderr:\n%s", tc.name, captured)
				}
				return
			}
			if got := strings.Count(captured, tc.want); got != 1 {
				t.Fatalf("%s: warning count = %d, want exactly 1\ncaptured stderr:\n%s", tc.name, got, captured)
			}
		})
	}
	t.Run("client-secret literal carries the new tail only", func(t *testing.T) {
		readStderr := captureOSStderr(t)
		_, _, _, _ = runRoot(t, "--client-secret", "literal-token", "info")
		captured := readStderr()
		if strings.Contains(captured, "prefer the MCPCLI_CLIENT_SECRET environment variable") {
			t.Fatalf("run printed the OLD warning tail\ncaptured stderr:\n%s", captured)
		}
	})
}

// TestRootAuthFlowBWSLoginFailsLoud: the --auth-flow exemption is the auth
// group's login only — bws login must not slip through, or its --auth-flow
// would be silently inert (cobra pass-2).
func TestRootAuthFlowBWSLoginFailsLoud(t *testing.T) {
	_, _, _, err := runRoot(t, "bws", "login", "--auth-flow", "manual")
	if err == nil {
		t.Fatal("bws login --auth-flow manual = nil error, want the loud rejection")
	}
	if !strings.Contains(err.Error(), "--auth-flow only applies to --auth oauth") {
		t.Fatalf("error %q does not explain the --auth-flow restriction", err.Error())
	}
}
