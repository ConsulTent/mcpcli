package auth

import (
	"context"
	"strings"
	"testing"

	sdkauth "github.com/modelcontextprotocol/go-sdk/auth"
)

// testProvider is a minimal Provider used to exercise the registry without
// touching the built-ins.
type testProvider struct{ name string }

func (p testProvider) Name() string { return p.name }

func (testProvider) Build(context.Context, Params) (sdkauth.OAuthHandler, func(), error) {
	return nil, nil, nil
}

func TestMain(m *testing.M) {
	RegisterBuiltins()
	m.Run()
}

func TestProviderForUnknown(t *testing.T) {
	_, err := ProviderFor("bogus")
	if err == nil {
		t.Fatal("expected error for unknown provider, got nil")
	}
	if !strings.Contains(err.Error(), "bogus") {
		t.Errorf("error should name the unknown provider, got: %v", err)
	}
	// The error must list every registered name.
	for _, name := range Names() {
		if !strings.Contains(err.Error(), name) {
			t.Errorf("error should list registered provider %q, got: %v", name, err)
		}
	}
}

func TestProviderForKnown(t *testing.T) {
	for _, name := range []string{"none", "bearer", "client-credentials"} {
		p, err := ProviderFor(name)
		if err != nil {
			t.Fatalf("ProviderFor(%q) returned error: %v", name, err)
		}
		if p.Name() != name {
			t.Errorf("ProviderFor(%q).Name() = %q", name, p.Name())
		}
	}
}

func TestNamesSorted(t *testing.T) {
	names := Names()
	if len(names) < 3 {
		t.Fatalf("expected at least 3 registered names, got %v", names)
	}
	for i := 1; i < len(names); i++ {
		if names[i-1] >= names[i] {
			t.Errorf("Names() not sorted ascending: %v", names)
			break
		}
	}
}

func TestRegisterDuplicatePanics(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Error("expected panic on duplicate registration, got none")
		}
	}()
	Register(testProvider{name: "auth-test-duplicate"})
	Register(testProvider{name: "auth-test-duplicate"})
}

func TestRegisterNilPanics(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Error("expected panic on nil provider, got none")
		}
	}()
	Register(nil)
}

func TestNoneProviderBuildsNoHandler(t *testing.T) {
	p, err := ProviderFor("none")
	if err != nil {
		t.Fatalf("ProviderFor(none): %v", err)
	}
	h, cleanup, err := p.Build(context.Background(), Params{Target: "https://example.test"})
	if err != nil {
		t.Fatalf("none Build: %v", err)
	}
	if h != nil {
		t.Error("none provider must return a nil handler (no auth)")
	}
	if cleanup != nil {
		t.Error("none provider has nothing to clean up; cleanup should be nil")
	}
}
