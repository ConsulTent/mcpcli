// Package connection resolves MCP protocol versions and opens client
// sessions to MCP servers over stdio or streamable HTTP.
package connection

import (
	"fmt"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// KnownVersions lists the MCP protocol version dates this CLI can name,
// oldest first. It mirrors the versions spoken by go-sdk v1.8.0
// (mcp.SupportedProtocolVersions()).
var KnownVersions = [...]string{"2024-11-05", "2025-03-26", "2025-06-18", "2025-11-25", "2026-07-28"}

// DraftAlias selects the newest protocol version the linked SDK supports.
const DraftAlias = "draft"

// autoAlias leaves protocol version negotiation to the SDK default.
const autoAlias = "auto"

// ResolveVersion maps a --protocol value to the protocol version to pin, or
// "" for the SDK's default negotiation:
//
//	"auto"       -> "" (let the SDK negotiate)
//	"draft"      -> lexicographically greatest of mcp.SupportedProtocolVersions()
//	a known date -> itself
//	anything else -> error
//
// A known date resolves to itself even when the linked SDK cannot speak it
// (KnownVersions may lead the SDK after an upgrade, or trail it): validation
// passes, the pin is attempted at Connect time, and the SDK rejects values it
// does not support there. Compat (task 8) relies on that distinction to probe
// what a server actually accepts.
func ResolveVersion(v string) (string, error) {
	switch v {
	case autoAlias:
		return "", nil
	case DraftAlias:
		return draftVersion(), nil
	}
	for _, known := range KnownVersions {
		if v == known {
			return v, nil
		}
	}
	return "", fmt.Errorf("unknown protocol version %q: want %q, %q, or one of: %s",
		v, autoAlias, DraftAlias, strings.Join(KnownVersions[:], ", "))
}

// draftVersion returns the lexicographically greatest entry of
// mcp.SupportedProtocolVersions(). The versions are ISO dates of the same
// shape, so lexicographic order matches chronological order.
func draftVersion() string {
	max := ""
	for _, v := range mcp.SupportedProtocolVersions() {
		if v > max {
			max = v
		}
	}
	return max
}
