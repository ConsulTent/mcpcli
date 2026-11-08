package secret

import (
	"context"
	"strings"
	"testing"
)

// fakeSource is a counting fake: it records every Resolve call and returns a
// canned value. Scheme is a field so one type can register under any
// spelling, including duplicate and mixed-case pairs.
type fakeSource struct {
	scheme string
	value  string
	calls  int
	refs   []string
}

func (f *fakeSource) Scheme() string { return f.scheme }

func (f *fakeSource) Resolve(ctx context.Context, ref string) (string, error) {
	f.calls++
	f.refs = append(f.refs, ref)
	return f.value, nil
}

func TestResolverNewDuplicateScheme(t *testing.T) {
	t.Run("equal lowered schemes", func(t *testing.T) {
		_, err := New(&fakeSource{scheme: "bws"}, &fakeSource{scheme: "bws"})
		if err == nil {
			t.Fatal("New with duplicate scheme: want error, got nil")
		}
		if !strings.Contains(err.Error(), `"bws"`) {
			t.Errorf("error %q: want it to name the scheme", err)
		}
	})
	t.Run("mixed-case pair collides", func(t *testing.T) {
		_, err := New(&fakeSource{scheme: "BWS"}, &fakeSource{scheme: "bws"})
		if err == nil {
			t.Fatal("New with mixed-case duplicate scheme: want error, got nil")
		}
		if !strings.Contains(err.Error(), `"bws"`) {
			t.Errorf("error %q: want it to name the lowered scheme", err)
		}
	})
	t.Run("empty schemes collide naturally", func(t *testing.T) {
		// No special-casing: two sources with an empty Scheme() dedupe on the
		// lowered form like any other pair.
		if _, err := New(&fakeSource{}, &fakeSource{}); err == nil {
			t.Fatal("New with two empty schemes: want error, got nil")
		}
	})
}

func TestResolverExpandDispatch(t *testing.T) {
	src := &fakeSource{scheme: "bws", value: "dummy-value"}
	r, err := New(src)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	got, err := r.Expand(t.Context(), "bws://name")
	if err != nil {
		t.Fatalf("Expand: %v", err)
	}
	if got != "dummy-value" {
		t.Errorf("Expand = %q, want %q", got, "dummy-value")
	}
	if src.calls != 1 {
		t.Errorf("source Resolve calls = %d, want 1", src.calls)
	}
	if len(src.refs) != 1 || src.refs[0] != "name" {
		t.Errorf("source received refs %v, want [name] (the post-:// remainder)", src.refs)
	}
}

func TestResolverExpandPassthrough(t *testing.T) {
	src := &fakeSource{scheme: "bws", value: "dummy-value"}
	r, err := New(src)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	// "bwss://misspelled" pins the documented limitation: an unregistered
	// scheme (e.g. a typo) stays literal instead of erroring.
	for _, value := range []string{"https://example.com/api", "plain value", "bwss://misspelled"} {
		got, err := r.Expand(t.Context(), value)
		if err != nil {
			t.Fatalf("Expand(%q): %v", value, err)
		}
		if got != value {
			t.Errorf("Expand(%q) = %q, want unchanged", value, got)
		}
	}
	if src.calls != 0 {
		t.Errorf("source Resolve calls = %d, want 0 (passthrough never resolves)", src.calls)
	}
}

func TestResolverExpandUppercaseValue(t *testing.T) {
	src := &fakeSource{scheme: "bws", value: "dummy-value"}
	r, err := New(src)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	// RFC 3986: schemes are case-insensitive, so BWS:// reaches the bws
	// source.
	got, err := r.Expand(t.Context(), "BWS://name")
	if err != nil {
		t.Fatalf("Expand: %v", err)
	}
	if got != "dummy-value" {
		t.Errorf("Expand = %q, want %q", got, "dummy-value")
	}
	if src.calls != 1 || len(src.refs) != 1 || src.refs[0] != "name" {
		t.Errorf("source calls = %d refs %v, want 1 call with ref [name]", src.calls, src.refs)
	}
}

func TestResolverMixedCaseRegistration(t *testing.T) {
	src := &fakeSource{scheme: "BwS", value: "dummy-value"}
	r, err := New(src)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	got, err := r.Expand(t.Context(), "bws://name")
	if err != nil {
		t.Fatalf("Expand: %v", err)
	}
	if got != "dummy-value" {
		t.Errorf("Expand = %q, want %q", got, "dummy-value")
	}
	if src.calls != 1 || len(src.refs) != 1 || src.refs[0] != "name" {
		t.Errorf("source calls = %d refs %v, want 1 call with ref [name]", src.calls, src.refs)
	}
}

func TestResolverSinglePass(t *testing.T) {
	inner := &fakeSource{scheme: "bws", value: "inner-value"}
	outer := &fakeSource{scheme: "vault", value: "bws://name"}
	r, err := New(outer, inner)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	got, err := r.Expand(t.Context(), "vault://one")
	if err != nil {
		t.Fatalf("Expand: %v", err)
	}
	if got != "bws://name" {
		t.Errorf("Expand = %q, want the source value unchanged", got)
	}
	if outer.calls != 1 {
		t.Errorf("outer source Resolve calls = %d, want 1", outer.calls)
	}
	if inner.calls != 0 {
		t.Errorf("inner source Resolve calls = %d, want 0 (single-pass: a resolved value is never re-scanned)", inner.calls)
	}
}

func TestRecognizes(t *testing.T) {
	src := &fakeSource{scheme: "bws", value: "dummy-value"}
	r, err := New(src)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	for _, value := range []string{"bws://name", "BWS://name"} {
		if !r.Recognizes(value) {
			t.Errorf("Recognizes(%q) = false, want true", value)
		}
	}
	for _, value := range []string{"https://example.com/api", "plain value", ""} {
		if r.Recognizes(value) {
			t.Errorf("Recognizes(%q) = true, want false", value)
		}
	}
	if src.calls != 0 {
		t.Errorf("source Resolve calls = %d, want 0 (Recognizes is pure classification)", src.calls)
	}
}
