package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ConsulTent/mcpcli/internal/ui"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// jevReplSession opens a session against a minimal jkudish-flavor pair whose
// underTest handler records the raw call arguments, so the REPL tests can
// assert exactly what reached the server.
func jevReplSession(t *testing.T, underTest string, captured *map[string]any) *mcp.ClientSession {
	t.Helper()
	srv := newTestMCPServerWithTools(t, addJkudishCapture(t, underTest, captured))
	return openTestSession(t, srv)
}

func TestReplJevAsk(t *testing.T) {
	var captured map[string]any
	session := jevReplSession(t, "jev_noul", &captured)

	stdout, _, err := driveRepl(t, session, "jev ask is 2+2=4?\nexit\n", ui.Rich)
	if err != nil {
		t.Fatal(err)
	}
	if props, _ := captured["propositions"].([]any); len(props) != 1 || props[0] != "is 2+2=4?" {
		t.Fatalf("propositions = %v, want the joined question", captured["propositions"])
	}
	if !strings.Contains(stdout, "likely") {
		t.Fatalf("stdout missing the verdict: %q", stdout)
	}
}

func TestReplJevAskInFiles(t *testing.T) {
	dir := t.TempDir()
	questionFile := filepath.Join(dir, "q.txt")
	contextFile := filepath.Join(dir, "ctx.txt")
	if err := os.WriteFile(questionFile, []byte("is the build green?"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(contextFile, []byte("CI log context"), 0o644); err != nil {
		t.Fatal(err)
	}

	var captured map[string]any
	session := jevReplSession(t, "jev_noul", &captured)
	if _, stderr, err := driveRepl(t, session,
		"jev ask --in-file "+questionFile+" --in-file context="+contextFile+"\nexit\n", ui.Rich); err != nil {
		t.Fatal(err)
	} else if !strings.Contains(stderr, "read "+questionFile) {
		t.Fatalf("stderr missing the read notice: %q", stderr)
	}
	if props, _ := captured["propositions"].([]any); len(props) != 1 || props[0] != "is the build green?" {
		t.Fatalf("propositions = %v, want the file content", captured["propositions"])
	}
	if captured["context"] != "CI log context" {
		t.Fatalf("context = %v", captured["context"])
	}
}

func TestReplJevScreen(t *testing.T) {
	fixture := filepath.Join("testdata", "gutenberg-sample.txt")
	purposeFile := filepath.Join(t.TempDir(), "purpose.txt")
	if err := os.WriteFile(purposeFile, []byte("summarization"), 0o644); err != nil {
		t.Fatal(err)
	}

	var captured map[string]any
	session := jevReplSession(t, "jev_screen", &captured)
	if _, _, err := driveRepl(t, session,
		"jev screen --in-file "+fixture+" --in-file purpose="+purposeFile+"\nexit\n", ui.Rich); err != nil {
		t.Fatal(err)
	}
	if captured["text"] != fixtureText(t) {
		t.Fatalf("text = %v…, want the exact fixture content", captured["text"])
	}
	if captured["purpose"] != "summarization" {
		t.Fatalf("purpose = %v", captured["purpose"])
	}
}

func TestReplJevVerifyInFiles(t *testing.T) {
	dir := t.TempDir()
	claimFile := filepath.Join(dir, "claim.txt")
	e1 := filepath.Join(dir, "e1.log")
	e2 := filepath.Join(dir, "e2.log")
	for path, content := range map[string]string{claimFile: "tests pass", e1: "first run green", e2: "second run green"} {
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	var captured map[string]any
	session := jevReplSession(t, "jev_verify", &captured)
	if _, _, err := driveRepl(t, session,
		"jev verify --in-file claim="+claimFile+" --in-file evidence="+e1+" --in-file evidence="+e2+"\nexit\n", ui.Rich); err != nil {
		t.Fatal(err)
	}
	if claims, _ := captured["claims"].([]any); len(claims) != 1 || claims[0] != "tests pass" {
		t.Fatalf("claims = %v", captured["claims"])
	}
	if ev, _ := captured["evidence"].([]any); len(ev) != 2 {
		t.Fatalf("evidence = %v, want both files in spec order", captured["evidence"])
	}
}

func TestReplJevErrors(t *testing.T) {
	var captured map[string]any
	session := jevReplSession(t, "jev_noul", &captured)
	dir := t.TempDir()
	qFile := filepath.Join(dir, "q.txt")
	if err := os.WriteFile(qFile, []byte("from file"), 0o644); err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name string
		line string
		want string
	}{
		{"text and bare in-file are exclusive", "jev ask both --in-file " + qFile, "mutually exclusive"},
		{"ask requires a question", "jev ask", "non-empty"},
		{"stdin is the repl's own input", "jev ask --in-file -", `not available in the REPL`},
		{"verify rejects free text", "jev verify some free text", "no free text"},
		{"verify requires claims", "jev verify", "claim"},
		{"unknown subcommand", "jev ponder", "usage:"},
		{"missing subcommand", "jev", "usage:"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, stderr, err := driveRepl(t, session, tc.line+"\nexit\n", ui.Rich)
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(stderr, tc.want) {
				t.Fatalf("stderr = %q, want containing %q", stderr, tc.want)
			}
		})
	}
}

func TestReplJevJSONMode(t *testing.T) {
	var captured map[string]any
	session := jevReplSession(t, "jev_noul", &captured)

	stdout, _, err := driveRepl(t, session, "jev ask is 2+2=4?\nexit\n", ui.JSON)
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Flavor   string `json:"flavor"`
		Tool     string `json:"tool"`
		Verdicts []any  `json:"verdicts"`
	}
	if err := json.Unmarshal([]byte(stdout), &doc); err != nil {
		t.Fatalf("stdout is not one JSON document: %v\n%s", err, stdout)
	}
	if doc.Flavor != "jkudish" || doc.Tool != "jev_noul" || len(doc.Verdicts) != 1 {
		t.Fatalf("doc = %+v, want one jkudish jev_noul verdict", doc)
	}
}
