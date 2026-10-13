package connection

import (
	"slices"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// sdkVersionsV1_8_0 is what mcp.SupportedProtocolVersions() returns for the
// pinned go-sdk v1.8.0 (newest first). The test below fails if the SDK ever
// changes the list, prompting a revisit of KnownVersions.
var sdkVersionsV1_8_0 = []string{"2026-07-28", "2025-11-25", "2025-06-18", "2025-03-26", "2024-11-05"}

func TestSDKVersionsMatchPinnedSDK(t *testing.T) {
	got := mcp.SupportedProtocolVersions()
	if !slices.Equal(got, sdkVersionsV1_8_0) {
		t.Fatalf("SupportedProtocolVersions() = %v, want %v (go-sdk v1.8.0); revisit KnownVersions and the draft rule", got, sdkVersionsV1_8_0)
	}
}

func TestResolveVersionAuto(t *testing.T) {
	v, err := ResolveVersion("auto")
	if err != nil {
		t.Fatalf("ResolveVersion(auto) error: %v", err)
	}
	if v != "" {
		t.Fatalf("ResolveVersion(auto) = %q, want empty string (SDK default negotiation)", v)
	}
}

func TestResolveVersionDraft(t *testing.T) {
	v, err := ResolveVersion("draft")
	if err != nil {
		t.Fatalf("ResolveVersion(draft) error: %v", err)
	}
	want := ""
	for _, s := range mcp.SupportedProtocolVersions() {
		if s > want {
			want = s
		}
	}
	if v != want {
		t.Fatalf("ResolveVersion(draft) = %q, want lexicographic max %q of %v", v, want, mcp.SupportedProtocolVersions())
	}
}

func TestResolveVersionKnownDates(t *testing.T) {
	for _, want := range KnownVersions {
		v, err := ResolveVersion(want)
		if err != nil {
			t.Fatalf("ResolveVersion(%q) error: %v", want, err)
		}
		if v != want {
			t.Fatalf("ResolveVersion(%q) = %q, want identity", want, v)
		}
	}
}

func TestResolveVersionUnknown(t *testing.T) {
	for _, bad := range []string{"", "bogus", "2023-01-01", "AUTO", "latest"} {
		v, err := ResolveVersion(bad)
		if err == nil {
			t.Fatalf("ResolveVersion(%q) = %q, want error", bad, v)
		}
	}
}
