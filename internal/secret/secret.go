// Package secret resolves secret references of the form <scheme>://<ref>
// against pluggable sources — the seam that lets mcpcli pull credentials
// from Bitwarden Secrets Manager (bws://) today and other secret managers
// later. A Resolver is built per run with its sources handed in explicitly
// (no init, no global registry): adding a manager means one new package
// implementing Source plus one registration line at the cli wiring site.
package secret

import (
	"context"
	"fmt"
	"strings"
)

// Source resolves references for one URL scheme. Scheme returns the scheme
// it claims (compared case-insensitively: "BWS" and "bws" are the same
// registration). Resolve receives ref — whatever follows the "<scheme>://"
// prefix — and owns its grammar entirely (uuid, name, path, ...).
type Source interface {
	Scheme() string
	Resolve(ctx context.Context, ref string) (string, error)
}

// Resolver dispatches secret references to registered sources. Build one per
// run with New; a Resolver is immutable after construction and holds nothing
// but its source map.
type Resolver struct {
	sources map[string]Source
}

// New returns a Resolver dispatching to the given sources. Schemes are
// lowercased at insert and deduped on the lowered form, so two sources
// claiming the same scheme in any case spelling fail construction. An empty
// Scheme is not special-cased and simply dedupes like any other value.
func New(sources ...Source) (*Resolver, error) {
	r := &Resolver{sources: make(map[string]Source, len(sources))}
	for _, s := range sources {
		scheme := strings.ToLower(s.Scheme())
		if prev, dup := r.sources[scheme]; dup {
			return nil, fmt.Errorf("duplicate secret scheme %q (registered by %T and %T)", scheme, prev, s)
		}
		r.sources[scheme] = s
	}
	return r, nil
}

// Expand resolves value when its scheme matches a registered source — the
// scheme is lowercased before lookup (RFC 3986: schemes are
// case-insensitive, so "BWS://name" reaches the "bws" source) and the source
// receives whatever follows the first "://". Anything else passes through
// unchanged. Expansion is single-pass: a resolved value is never re-scanned,
// so a decrypted secret whose VALUE is itself a registered ref comes back
// as-is instead of looping (self or mutual refs).
func (r *Resolver) Expand(ctx context.Context, value string) (string, error) {
	if scheme, ref, ok := splitRef(value); ok {
		if src, found := r.sources[strings.ToLower(scheme)]; found {
			return src.Resolve(ctx, ref)
		}
	}
	// shortcut: misspelled scheme stays literal; upgrade when an operator
	// misdiagnoses through bws get.
	return value, nil
}

// Recognizes reports whether value looks like a reference this Resolver
// would expand: its scheme — lowercased, same rule as Expand — matches a
// registered source. It performs no resolution (no network, no side
// effects), so callers can classify values without paying for a fetch.
func (r *Resolver) Recognizes(value string) bool {
	scheme, _, ok := splitRef(value)
	if !ok {
		return false
	}
	_, found := r.sources[strings.ToLower(scheme)]
	return found
}

// splitRef splits value at its first "://" into scheme and the remainder.
func splitRef(value string) (scheme, ref string, ok bool) {
	i := strings.Index(value, "://")
	if i < 0 {
		return "", "", false
	}
	return value[:i], value[i+3:], true
}
