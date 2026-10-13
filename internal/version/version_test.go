package version

import (
	"strings"
	"testing"
)

// TestVersionShape pins the baseline constant's format: a leading v, three
// dot-separated numeric segments. Bumping the version keeps this shape.
func TestVersionShape(t *testing.T) {
	if !strings.HasPrefix(Version, "v") {
		t.Fatalf("Version = %q, want a leading v", Version)
	}
	parts := strings.Split(strings.TrimPrefix(Version, "v"), ".")
	if len(parts) != 3 {
		t.Fatalf("Version = %q, want three dot-separated segments", Version)
	}
	for _, p := range parts {
		if p == "" {
			t.Fatalf("Version = %q, has an empty segment", Version)
		}
		for _, r := range p {
			if r < '0' || r > '9' {
				t.Fatalf("Version = %q, segment %q is not numeric", Version, p)
			}
		}
	}
}

// TestCurrentFallsBackToConstant runs in a test binary, whose build info has
// no usable module stamp ("" or "(devel)"), so Current must report the
// baseline constant — never "(devel)" or empty.
func TestCurrentFallsBackToConstant(t *testing.T) {
	got := Current()
	if got == "" || got == "(devel)" {
		t.Fatalf("Current() = %q, want the fallback constant", got)
	}
	if got != Version {
		// A stamp can only win in a release build; in a test binary the
		// constant is the only possible answer.
		t.Fatalf("Current() = %q, want %q", got, Version)
	}
}
