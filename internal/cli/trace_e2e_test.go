package cli

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/ConsulTent/mcpcli/internal/ui"
)

// TestTraceE2E pins the --trace surface end to end: the table reaches stderr
// (never stdout), rows name every request the session made, stdout stays
// exactly the one result document, and --plain byte-scans ASCII.
func TestTraceE2E(t *testing.T) {
	srv := newTestMCPServerWithTools(t, addEchoTool)

	// Rich human mode first: the table with its rows reaches stderr.
	stdout, stderr, _, err := runRoot(t, "--trace", "--url", srv.URL, "tools", "list")
	if err != nil {
		t.Fatalf("tools list --trace: %v\nstderr: %s", err, stderr)
	}
	for _, want := range []string{
		"connect", "CALL", "TIME", "SENT", "RECV",
		"boot (initialize)", // the handshake row
		"tools/list",        // the command's own call
	} {
		if !strings.Contains(stderr, want) {
			t.Fatalf("trace stderr missing %q:\n%s", want, stderr)
		}
	}
	if strings.Contains(stdout, "CALL") || strings.Contains(stdout, "boot (") {
		t.Fatalf("trace leaked into stdout: %s", stdout)
	}

	// stdout purity under --json: exactly one JSON document, no trace bytes.
	stdout, stderr, _, err = runRoot(t, "--trace", "--json", "--url", srv.URL, "tools", "list")
	if err != nil {
		t.Fatalf("tools list --trace --json: %v\n%s", err, stderr)
	}
	var doc map[string]any
	if err := json.Unmarshal([]byte(stdout), &doc); err != nil {
		t.Fatalf("stdout not one JSON doc under --trace --json: %v\n%s", err, stdout)
	}
	if strings.Contains(stdout, "CALL") || strings.Contains(stdout, "boot (") {
		t.Fatalf("trace leaked into stdout: %s", stdout)
	}
	if !strings.Contains(stderr, "tools/list") {
		t.Fatalf("trace doc missing from stderr under --json:\n%s", stderr)
	}

	// Plain mode: byte-ASCII, no bars, no emoji.
	_, stderr, _, err = runRoot(t, "--trace", "--plain", "--url", srv.URL, "tools", "list")
	if err != nil {
		t.Fatalf("tools list --trace --plain: %v", err)
	}
	if strings.Contains(stderr, "BAR") || strings.Contains(stderr, "▓") || strings.Contains(stderr, "❌") {
		t.Fatalf("plain trace must have no bars or emoji:\n%s", stderr)
	}
	for b := range stderr {
		if stderr[b] > 0x7E {
			t.Fatalf("plain trace non-ASCII byte %q:\n%s", stderr[b], stderr)
		}
	}
}

// TestTraceJSONDoc: in --json mode the trace is one structured document on
// stderr with the snake_case fields.
func TestTraceJSONDoc(t *testing.T) {
	srv := newTestMCPServerWithTools(t, addEchoTool)
	_, stderr, _, err := runRoot(t, "--trace", "--json", "--url", srv.URL, "tools", "call", "echo", "--arg", "msg=hi")
	if err != nil {
		t.Fatalf("tools call --trace --json: %v\n%s", err, stderr)
	}
	var doc struct {
		Sections []struct {
			Calls []struct {
				Method   string  `json:"method"`
				RTTms    float64 `json:"rtt_ms"`
				ReqBytes int     `json:"req_bytes"`
			} `json:"calls"`
		} `json:"sections"`
	}
	if err := json.Unmarshal([]byte(stderr), &doc); err != nil {
		t.Fatalf("stderr not one trace JSON doc: %v\n%s", err, stderr)
	}
	var sawCallTool bool
	for _, s := range doc.Sections {
		for _, c := range s.Calls {
			if c.Method == "tools/call" {
				sawCallTool = true
				if c.ReqBytes == 0 {
					t.Fatalf("tools/call row has no req_bytes:\n%s", stderr)
				}
			}
		}
	}
	if !sawCallTool {
		t.Fatalf("no tools/call row in trace doc:\n%s", stderr)
	}
}

// TestTraceVerboseTotals: --verbose implies tracing and adds the totals line.
func TestTraceVerboseTotals(t *testing.T) {
	srv := newTestMCPServerWithTools(t, addEchoTool)
	_, stderr, _, err := runRoot(t, "--verbose", "--url", srv.URL, "tools", "list")
	if err != nil {
		t.Fatalf("tools list --verbose: %v", err)
	}
	if !strings.Contains(stderr, "totals:") || !strings.Contains(stderr, "internal ") {
		t.Fatalf("verbose trace missing totals/internal:\n%s", stderr)
	}
}

// TestTraceFailedCall: a tool error still gets its row, marked.
func TestTraceFailedCall(t *testing.T) {
	srv := newTestMCPServerWithTools(t, addEchoTool)
	_, stderr, _, _ := runRoot(t, "--trace", "--url", srv.URL, "tools", "call", "nope")
	if !strings.Contains(stderr, "❌ tools/call") {
		t.Fatalf("failed call row missing or unmarked:\n%s", stderr)
	}
}

// TestTraceSpinnerSuppressed: under --trace the connect spinner's done-line
// is not printed (the report carries the same numbers once).
func TestTraceSpinnerSuppressed(t *testing.T) {
	srv := newTestMCPServerWithTools(t, addEchoTool)
	_, stderr, dec, err := runRoot(t, "--trace", "--url", srv.URL, "tools", "list")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(stderr, "connecting to") || strings.Contains(dec, "connecting to") {
		t.Fatalf("spinner line printed under --trace (stderr=%q dec=%q)", stderr, dec)
	}
}

// TestTraceREPLAccumulates: the -i path prints one report at session end
// with every line's calls (the report is runWithConnection's, after the REPL
// loop returns).
func TestTraceREPLAccumulates(t *testing.T) {
	srv := newTestMCPServerWithTools(t, addEchoTool)

	var out, errBuf bytes.Buffer
	restore := ui.SetSpinnerOutput(&bytes.Buffer{})
	defer restore()
	root := NewRootCommand()
	root.SetOut(&out)
	root.SetErr(&errBuf)
	root.SetIn(strings.NewReader("tools\nping\nexit\n"))
	root.SetArgs([]string{"--trace", "--url", srv.URL, "-i"})
	if err := root.Execute(); err != nil {
		t.Fatalf("-i --trace repl: %v", err)
	}
	if !strings.Contains(errBuf.String(), "tools/list") || !strings.Contains(errBuf.String(), "ping") {
		t.Fatalf("REPL trace report missing accumulated calls:\n%s", errBuf.String())
	}
}
