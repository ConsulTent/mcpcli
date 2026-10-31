// Package version holds mcp's version. Version is the baseline constant;
// Current prefers the module stamp recorded by the go tool when one exists
// (release builds report their tag) and falls back to Version, so local
// builds print the release baseline instead of "(devel)".
package version

import "runtime/debug"

// Version is the current release of mcp. Every substantive change bumps the
// minor segment (v1.0.0 -> v1.1.0); see CONTRIBUTING.md.
const Version = "v1.3.3"

// Current returns the module version recorded by the go tool, falling back
// to Version when the stamp is missing or "(devel)".
func Current() string {
	if info, ok := debug.ReadBuildInfo(); ok {
		if v := info.Main.Version; v != "" && v != "(devel)" {
			return v
		}
	}
	return Version
}
