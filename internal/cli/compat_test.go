package cli

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/ConsulTent/mcpcli/internal/connection"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// TestClassifyProbe pins the post-connect verdict logic: a discovery-
// negotiated 2026-07-28 session is accepted without a ping (the SDK's Ping
// omits the SEP-2575 _meta such servers validate), legacy sessions ping and
// report honestly when the server downgrades the requested version.
func TestClassifyProbe(t *testing.T) {
	tests := []struct {
		name       string
		pinned     string
		negotiated string
		pingErr    error
		wantStatus string
		wantDetail string
	}{
		{
			name:       "discovery_session",
			pinned:     "2026-07-28",
			negotiated: "2026-07-28",
			pingErr:    nil,
			wantStatus: statusAccepted,
			wantDetail: "server advertises 2026-07-28 via discovery; ping skipped (SDK Ping omits the SEP-2575 _meta such servers validate)",
		},
		{
			name:       "legacy_echo",
			pinned:     "2025-06-18",
			negotiated: "2025-06-18",
			pingErr:    nil,
			wantStatus: statusAccepted,
			wantDetail: "server echoed 2025-06-18",
		},
		{
			name:       "legacy_downgrade",
			pinned:     "2025-11-25",
			negotiated: "2025-06-18",
			pingErr:    nil,
			wantStatus: statusAccepted,
			wantDetail: "requested 2025-11-25; server negotiated 2025-06-18",
		},
		{
			name:       "ping_failure",
			pinned:     "2025-06-18",
			negotiated: "2025-06-18",
			pingErr:    errors.New("boom"),
			wantStatus: statusRejected,
			wantDetail: "boom",
		},
		{
			name:       "unknown_negotiated",
			pinned:     "2025-06-18",
			negotiated: "",
			pingErr:    nil,
			wantStatus: statusAccepted,
			wantDetail: "requested 2025-06-18; server negotiated (unknown)",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotStatus, gotDetail := classifyProbe(tt.pinned, tt.negotiated, tt.pingErr)
			if gotStatus != tt.wantStatus {
				t.Errorf("classifyProbe(%q, %q, %v) status = %q, want %q", tt.pinned, tt.negotiated, tt.pingErr, gotStatus, tt.wantStatus)
			}
			if gotDetail != tt.wantDetail {
				t.Errorf("classifyProbe(%q, %q, %v) detail = %q, want %q", tt.pinned, tt.negotiated, tt.pingErr, gotDetail, tt.wantDetail)
			}
		})
	}
}

// TestSafeVersion: server-supplied version strings reach output only through
// safeVersion — control characters and non-[0-9a-zA-Z.-] bytes stripped,
// length capped at 32.
func TestSafeVersion(t *testing.T) {
	got := safeVersion("\x1b]0;pwned\x07 2026-07-28; rm -rf / and a very long tail")
	const want = "0pwned2026-07-28rm-rfandaverylon"
	if got != want {
		t.Fatalf("safeVersion() = %q, want %q", got, want)
	}
	if len(got) > 32 {
		t.Fatalf("safeVersion() length %d exceeds 32", len(got))
	}
	if got := safeVersion("2025-06-18"); got != "2025-06-18" {
		t.Fatalf("safeVersion(plain) = %q, want unchanged", got)
	}
}

// compatJSONRow and compatJSON pin compat's --json wire contract in the tests,
// independent of the implementation's Go types.
type compatJSONRow struct {
	Version string `json:"version"`
	Status  string `json:"status"`
	Detail  string `json:"detail"`
}

type compatJSON struct {
	Native     string          `json:"native"`
	NativeNote string          `json:"native_note"`
	Results    []compatJSONRow `json:"results"`
}

func parseCompatJSON(t *testing.T, stdout string) compatJSON {
	t.Helper()
	var got compatJSON
	if err := json.Unmarshal([]byte(stdout), &got); err != nil {
		t.Fatalf("compat --json stdout %q is not JSON: %v", stdout, err)
	}
	return got
}

// draftRow returns the probe row for the draft alias.
func draftRow(t *testing.T, rows []compatJSONRow) compatJSONRow {
	t.Helper()
	i := slices.IndexFunc(rows, func(r compatJSONRow) bool { return r.Version == connection.DraftAlias })
	if i < 0 {
		t.Fatalf("no %q row in compat results %+v", connection.DraftAlias, rows)
	}
	return rows[i]
}

// TestCompatE2E runs compat against a real MCP server: the human table ends
// with the native (negotiated) line and reports at least one accepted
// version; --json emits the full document on stdout with no decoration.
func TestCompatE2E(t *testing.T) {
	srv := newTestMCPServer(t)

	stdout, _, _, err := runRoot(t, "--url", srv.URL, "compat")
	if err != nil {
		t.Fatalf("compat: %v", err)
	}
	if !strings.Contains(stdout, "native (negotiated): ") {
		t.Errorf("compat stdout %q lacks the native (negotiated) line", stdout)
	}
	if !strings.Contains(stdout, "accepted") {
		t.Errorf("compat stdout %q reports no accepted version", stdout)
	}

	stdout, stderr, dec, err := runRoot(t, "--url", srv.URL, "--json", "compat")
	if err != nil {
		t.Fatalf("compat --json: %v", err)
	}
	if stderr != "" || dec != "" {
		t.Errorf("compat --json wrote decoration: stderr %q, ui output %q", stderr, dec)
	}
	got := parseCompatJSON(t, stdout)
	if got.Native == "" {
		t.Errorf("compat --json native is empty in %q", stdout)
	}
	if got.NativeNote == "" {
		t.Errorf("compat --json native_note is empty in %q", stdout)
	}
	if want := len(connection.KnownVersions) + 1; len(got.Results) != want {
		t.Errorf("compat --json has %d result rows, want %d (%v + draft)", len(got.Results), want, connection.KnownVersions)
	}
	accepted := 0
	for _, row := range got.Results {
		switch row.Status {
		case "accepted":
			accepted++
		case "rejected", "not-speakeable":
		default:
			t.Errorf("row %+v has an unknown status", row)
		}
	}
	if accepted < 1 {
		t.Errorf("compat --json reports %d accepted rows, want at least 1: %+v", accepted, got.Results)
	}
}

// TestCompatPlainASCII runs compat in Plain mode and asserts every captured
// byte is ASCII with no escape sequences.
func TestCompatPlainASCII(t *testing.T) {
	srv := newTestMCPServer(t)

	stdout, stderr, dec, err := runRoot(t, "--url", srv.URL, "--plain", "compat")
	if err != nil {
		t.Fatalf("compat --plain: %v", err)
	}
	for name, s := range map[string]string{"stdout": stdout, "stderr": stderr, "ui output": dec} {
		for i := 0; i < len(s); i++ {
			if b := s[i]; b >= 0x80 {
				t.Fatalf("compat --plain: %s has non-ASCII byte 0x%02x at offset %d: %q", name, b, i, s)
			}
			if b := s[i]; b == 0x1b {
				t.Fatalf("compat --plain: %s has an ESC byte at offset %d: %q", name, i, s)
			}
		}
	}
	if !strings.Contains(stdout, "OK: accepted") {
		t.Errorf("compat --plain stdout %q lacks an ASCII accepted row", stdout)
	}
	if !strings.Contains(stdout, "native (negotiated): ") {
		t.Errorf("compat --plain stdout %q lacks the native line", stdout)
	}
}

// TestCompatUnreachable pins the exit contract: an unreachable endpoint is
// the only failure mode (the auto/native connect fails), even though every
// pinned probe was already reported as rejected.
func TestCompatUnreachable(t *testing.T) {
	_, _, _, err := runRoot(t, "--url", "http://127.0.0.1:1/mcp", "compat")
	if err == nil {
		t.Fatal("compat against an unreachable endpoint succeeded, want an error (exit 1)")
	}
}

// TestCompatDraftResolves asserts the draft row is present and resolves to
// the newest protocol version the linked SDK supports, whatever the server
// then does with it.
func TestCompatDraftResolves(t *testing.T) {
	srv := newTestMCPServer(t)

	stdout, _, _, err := runRoot(t, "--url", srv.URL, "--json", "compat")
	if err != nil {
		t.Fatalf("compat --json: %v", err)
	}
	got := parseCompatJSON(t, stdout)

	newest := ""
	for _, v := range mcp.SupportedProtocolVersions() {
		if v > newest {
			newest = v
		}
	}
	row := draftRow(t, got.Results)
	if row.Status == "not-speakeable" {
		t.Errorf("draft row %+v is not-speakeable, but draft resolves to a version the SDK supports", row)
	}
	if !strings.HasPrefix(row.Detail, "resolved to "+newest) {
		t.Errorf("draft row detail %q does not record resolution to the newest SDK version %q", row.Detail, newest)
	}
}

// TestCompatNotSpeakable table-drives the branch that fires when the linked
// SDK cannot speak a version it can name: those rows are reported without any
// connection attempt. The target binary does not exist, so every speakable
// row rejects instantly and the test never touches the network.
func TestCompatNotSpeakable(t *testing.T) {
	f := &GlobalFlags{Command: "mcp-compat-no-such-binary", Timeout: 5 * time.Second}

	all := connection.KnownVersions[:]
	tests := []struct {
		name      string
		supported []string
		skipped   []string // versions the trimmed SDK cannot speak
	}{
		{"sdk supports nothing", nil, slices.Concat(all, []string{connection.DraftAlias})},
		{"sdk supports only the oldest", []string{"2024-11-05"}, []string{"2025-03-26", "2025-06-18", "2025-11-25", "2026-07-28", connection.DraftAlias}},
		{"sdk trails the newest", []string{"2024-11-05", "2025-03-26", "2025-06-18", "2025-11-25"}, []string{"2026-07-28", connection.DraftAlias}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rows := probeVersions(context.Background(), f, tt.supported)
			if want := len(connection.KnownVersions) + 1; len(rows) != want {
				t.Fatalf("probeVersions returned %d rows, want %d", len(rows), want)
			}
			for _, row := range rows {
				want := "rejected"
				if slices.Contains(tt.skipped, row.Version) {
					want = "not-speakeable"
				}
				if row.Status != want {
					t.Errorf("version %q status = %q, want %q (detail %q)", row.Version, row.Status, want, row.Detail)
				}
			}
		})
	}
}
