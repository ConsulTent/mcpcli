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
// PersistentPreRunE prints when --client-secret is set from the command line.
// It is functional output written straight to os.Stderr, so capturing it
// means swapping os.Stderr (runRoot's cobra-level buffers do not see it).
const clientSecretWarning = "warning: --client-secret was passed on the command line; prefer the MCPCLI_CLIENT_SECRET environment variable"

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
