package tus

// Every credential-shaped value in this file is an obviously-synthetic
// constant assembled from inert fragments (see dummyToken/dummyBasicCreds);
// no real credential ever appears in test source.

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ConsulTent/mcpcli/internal/auth"
	"golang.org/x/oauth2"
)

// dummyToken assembles a synthetic bearer token from inert fragments — no
// credential-shaped literal for scanners, an obvious dummy by construction.
func dummyToken() string { return "dummy-token-" + "xyz" }

// dummyBasicCreds assembles synthetic Basic credentials from inert fragments.
func dummyBasicCreds() (string, string) { return "dummy-" + "user", "dummy-" + "pass" }

// ---------------------------------------------------------------------------
// fake: one stateful tus 1.0.0 server with a forceable fault script
// (the internal/bws fake-server pattern: hit counters, captured requests,
// queued faults consumed in order per method).
// ---------------------------------------------------------------------------

// fakeFault scripts one deviant response for the next request of its method.
type fakeFault struct {
	status     int               // HTTP status to serve (success-path faults use 201/204)
	body       string            // response body carried by the fault status
	location   string            // override the Location header (POST 201 faults)
	noLocation bool              // omit the Location header on a 201 fault
	hijack     bool              // hijack the connection and close it abruptly (mid-PATCH drop)
	hang       bool              // hang until the request's context is done (client-side cancel)
	advance    int64             // bytes the fake persists while failing (tusd partial-append quirk)
	extra      map[string]string // extra response headers (e.g. a garbage Upload-Offset)
}

// captured is one observed wire request.
type captured struct {
	method           string
	path             string
	header           http.Header
	contentLength    int64
	transferEncoding []string
	body             []byte
}

type fakeTus struct {
	mu  sync.Mutex
	srv *httptest.Server

	// upload state
	exists   bool
	offset   int64
	length   int64
	metadata string

	// knobs
	relativeLocation bool // Location is relative (/files/abc123) instead of absolute

	// scripted faults, consumed in order per method
	postFaults, headFaults, patchFaults, deleteFaults []fakeFault

	// captures
	posts, heads, patches, deletes int
	postCaptures                   []captured
	headCaptures                   []captured
	patchCaptures                  []captured
	deleteCaptures                 []captured
}

func newFakeTus(t *testing.T, useTLS bool) *fakeTus {
	t.Helper()
	ft := &fakeTus{}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /files/", ft.handlePost)
	mux.HandleFunc("HEAD /files/abc123", ft.handleHead)
	mux.HandleFunc("PATCH /files/abc123", ft.handlePatch)
	mux.HandleFunc("DELETE /files/abc123", ft.handleDelete)
	var srv *httptest.Server
	if useTLS {
		srv = httptest.NewTLSServer(mux)
	} else {
		srv = httptest.NewServer(mux)
	}
	// Keep test output pristine: the TLS-rejection row's aborted handshake
	// logs from the fake's server goroutine are expected noise, not signal.
	srv.Config.ErrorLog = log.New(io.Discard, "", 0)
	t.Cleanup(srv.Close)
	ft.srv = srv
	return ft
}

func (ft *fakeTus) endpoint() string  { return ft.srv.URL + "/files/" }
func (ft *fakeTus) uploadURL() string { return ft.srv.URL + "/files/abc123" }

func (ft *fakeTus) seed(exists bool, offset, length int64) {
	ft.mu.Lock()
	defer ft.mu.Unlock()
	ft.exists, ft.offset, ft.length = exists, offset, length
}

func (ft *fakeTus) queuePost(f fakeFault) {
	ft.mu.Lock()
	ft.postFaults = append(ft.postFaults, f)
	ft.mu.Unlock()
}
func (ft *fakeTus) queueHead(f fakeFault) {
	ft.mu.Lock()
	ft.headFaults = append(ft.headFaults, f)
	ft.mu.Unlock()
}
func (ft *fakeTus) queuePatch(f fakeFault) {
	ft.mu.Lock()
	ft.patchFaults = append(ft.patchFaults, f)
	ft.mu.Unlock()
}
func (ft *fakeTus) queueDelete(f fakeFault) {
	ft.mu.Lock()
	ft.deleteFaults = append(ft.deleteFaults, f)
	ft.mu.Unlock()
}

func popFaultLocked(q *[]fakeFault) (fakeFault, bool) {
	if len(*q) == 0 {
		return fakeFault{}, false
	}
	f := (*q)[0]
	*q = (*q)[1:]
	return f, true
}

func captureRequest(r *http.Request, body []byte) captured {
	return captured{
		method:           r.Method,
		path:             r.URL.Path,
		header:           r.Header.Clone(),
		contentLength:    r.ContentLength,
		transferEncoding: r.TransferEncoding,
		body:             body,
	}
}

func (ft *fakeTus) applyExtra(h http.Header, extra map[string]string) {
	for k, v := range extra {
		h.Set(k, v)
	}
}

func (ft *fakeTus) handlePost(w http.ResponseWriter, r *http.Request) {
	ft.mu.Lock()
	ft.posts++
	ft.postCaptures = append(ft.postCaptures, captureRequest(r, nil))
	fault, hasFault := popFaultLocked(&ft.postFaults)
	ft.mu.Unlock()

	if hasFault {
		if fault.hang {
			<-r.Context().Done()
			return
		}
		if fault.status == http.StatusCreated {
			switch {
			case fault.noLocation:
			case fault.location != "":
				w.Header().Set("Location", fault.location)
			default:
				w.Header().Set("Location", ft.srv.URL+"/files/abc123")
			}
		}
		ft.applyExtra(w.Header(), fault.extra)
		w.WriteHeader(fault.status)
		_, _ = io.WriteString(w, fault.body)
		return
	}

	ft.mu.Lock()
	ft.length, _ = strconv.ParseInt(r.Header.Get("Upload-Length"), 10, 64)
	ft.metadata = r.Header.Get("Upload-Metadata")
	ft.exists = true
	ft.offset = 0
	relative := ft.relativeLocation
	ft.mu.Unlock()
	loc := "/files/abc123"
	if !relative {
		loc = ft.srv.URL + loc
	}
	w.Header().Set("Tus-Resumable", "1.0.0")
	w.Header().Set("Location", loc)
	w.WriteHeader(http.StatusCreated)
}

func (ft *fakeTus) handleHead(w http.ResponseWriter, r *http.Request) {
	ft.mu.Lock()
	ft.heads++
	ft.headCaptures = append(ft.headCaptures, captureRequest(r, nil))
	fault, hasFault := popFaultLocked(&ft.headFaults)
	exists, offset, length := ft.exists, ft.offset, ft.length
	ft.mu.Unlock()

	if hasFault {
		ft.applyExtra(w.Header(), fault.extra)
		w.WriteHeader(fault.status)
		_, _ = io.WriteString(w, fault.body)
		return
	}
	if !exists {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	w.Header().Set("Tus-Resumable", "1.0.0")
	w.Header().Set("Upload-Offset", strconv.FormatInt(offset, 10))
	w.Header().Set("Upload-Length", strconv.FormatInt(length, 10))
	w.WriteHeader(http.StatusOK)
}

func (ft *fakeTus) handlePatch(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)

	ft.mu.Lock()
	ft.patches++
	ft.patchCaptures = append(ft.patchCaptures, captureRequest(r, body))
	fault, hasFault := popFaultLocked(&ft.patchFaults)
	sent := r.Header.Get("Upload-Offset")
	stored := ft.offset
	ft.mu.Unlock()

	if hasFault {
		ft.mu.Lock()
		ft.offset += fault.advance
		ft.mu.Unlock()
		if fault.hang {
			<-r.Context().Done()
			return
		}
		if fault.hijack {
			hj, ok := w.(http.Hijacker)
			if !ok {
				http.Error(w, "fake cannot hijack", http.StatusInternalServerError)
				return
			}
			conn, _, err := hj.Hijack()
			if err == nil {
				_ = conn.Close() // abrupt drop: the client sees a network error
			}
			return
		}
		ft.applyExtra(w.Header(), fault.extra)
		w.WriteHeader(fault.status)
		_, _ = io.WriteString(w, fault.body)
		return
	}

	// tusd quirk: an offset mismatch is a 409 WITHOUT an Upload-Offset
	// header — HEAD is the only recovery primitive.
	if sent != strconv.FormatInt(stored, 10) {
		w.WriteHeader(http.StatusConflict)
		_, _ = io.WriteString(w, "offset mismatch")
		return
	}
	ft.mu.Lock()
	ft.offset += int64(len(body))
	newOffset := ft.offset
	ft.mu.Unlock()
	w.Header().Set("Tus-Resumable", "1.0.0")
	w.Header().Set("Upload-Offset", strconv.FormatInt(newOffset, 10))
	w.WriteHeader(http.StatusNoContent)
}

func (ft *fakeTus) handleDelete(w http.ResponseWriter, r *http.Request) {
	ft.mu.Lock()
	ft.deletes++
	ft.deleteCaptures = append(ft.deleteCaptures, captureRequest(r, nil))
	fault, hasFault := popFaultLocked(&ft.deleteFaults)
	exists := ft.exists
	ft.mu.Unlock()

	if hasFault {
		ft.applyExtra(w.Header(), fault.extra)
		w.WriteHeader(fault.status)
		_, _ = io.WriteString(w, fault.body)
		return
	}
	if !exists {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	ft.mu.Lock()
	ft.exists = false
	ft.mu.Unlock()
	w.Header().Set("Tus-Resumable", "1.0.0")
	w.WriteHeader(http.StatusNoContent)
}

func (ft *fakeTus) counts() (posts, heads, patches, deletes int) {
	ft.mu.Lock()
	defer ft.mu.Unlock()
	return ft.posts, ft.heads, ft.patches, ft.deletes
}

func (ft *fakeTus) lastPost() captured {
	ft.mu.Lock()
	defer ft.mu.Unlock()
	return ft.postCaptures[len(ft.postCaptures)-1]
}

func (ft *fakeTus) patchAt(i int) captured {
	ft.mu.Lock()
	defer ft.mu.Unlock()
	return ft.patchCaptures[i]
}

func (ft *fakeTus) headAt(i int) captured {
	ft.mu.Lock()
	defer ft.mu.Unlock()
	return ft.headCaptures[i]
}

// ---------------------------------------------------------------------------
// fake auth handlers (sdkauth.OAuthHandler implementations with counters)
// ---------------------------------------------------------------------------

type fakeHandler struct {
	// TokenSource behavior: ts wins when set; tsErr aborts; otherwise a
	// source returning (nil token, tokenErr).
	ts       oauth2.TokenSource
	tsErr    error
	tokenErr error

	authorizeErr error

	tokenSourceCalls int
	authorizeCalls   int
}

func (h *fakeHandler) TokenSource(context.Context) (oauth2.TokenSource, error) {
	h.tokenSourceCalls++
	if h.tsErr != nil {
		return nil, h.tsErr
	}
	if h.ts != nil {
		return h.ts, nil
	}
	return &fakeTokenSource{err: h.tokenErr}, nil
}

func (h *fakeHandler) Authorize(_ context.Context, _ *http.Request, resp *http.Response) error {
	h.authorizeCalls++
	_ = resp.Body.Close() // the SDK contract: the handler owns the body
	return h.authorizeErr
}

type fakeTokenSource struct{ err error }

func (s *fakeTokenSource) Token() (*oauth2.Token, error) { return nil, s.err }

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

func tempFile(t *testing.T, content []byte) *os.File {
	t.Helper()
	f, err := os.CreateTemp(t.TempDir(), "tus-upload-*.bin")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = f.Close() })
	if _, err := f.Write(content); err != nil {
		t.Fatal(err)
	}
	return f
}

func patternBytes(n int) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = byte(i%200 + 1) // non-zero pattern; never NUL
	}
	return b
}

func waitFor(t *testing.T, cond func() bool, what string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// standard metadata fixture: {"filename":"a b.txt","client":"mcpcli"} —
// "a b.txt" → YSBiLnR4dA== and "mcpcli" → bWNwY2xp per the tus grammar.
var stdMetadata = map[string]string{"filename": "a b.txt", "client": "mcpcli"}

func wantMetadataPairs() map[string]string {
	return map[string]string{
		"filename": "YSBiLnR4dA==",
		"client":   "bWNwY2xp",
	}
}

func parseMetadataHeader(wire string) map[string]string {
	pairs := map[string]string{}
	for _, pair := range strings.Split(wire, ",") {
		key, value, _ := strings.Cut(pair, " ")
		pairs[key] = value
	}
	return pairs
}

// ---------------------------------------------------------------------------
// NormalizeEndpoint (D2)
// ---------------------------------------------------------------------------

func TestNormalizeEndpoint(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		want string
	}{
		{"path gains trailing slash", "https://example.com/files", "https://example.com/files/"},
		{"existing trailing slash kept", "https://example.com/files/", "https://example.com/files/"},
		{"host lowercased, path case kept", "http://Example.COM/Files", "http://example.com/Files/"},
		{"empty path stays empty", "https://example.com", "https://example.com"},
		{"root slash kept", "https://example.com/", "https://example.com/"},
		{"https default port stripped", "https://example.com:443/files", "https://example.com/files/"},
		{"http default port stripped", "http://example.com:80/files", "http://example.com/files/"},
		{"non-default port kept", "http://example.com:8080/files", "http://example.com:8080/files/"},
		{"https default port stripped bare host", "https://example.com:443", "https://example.com"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := NormalizeEndpoint(tt.raw)
			if err != nil {
				t.Fatalf("NormalizeEndpoint(%q) returned error: %v", tt.raw, err)
			}
			if got != tt.want {
				t.Fatalf("NormalizeEndpoint(%q) = %q, want %q", tt.raw, got, tt.want)
			}
		})
	}

	rejects := []struct {
		name string
		raw  string
	}{
		{"empty string", ""},
		{"relative path", "files"},
		{"ftp scheme", "ftp://example.com/files"},
		{"userinfo", "https://user:pass@example.com/files"},
		{"query", "https://example.com/files?x=1"},
		{"fragment", "https://example.com/files#frag"},
		{"empty host with port", "http://:8080/files"},
	}
	for _, tt := range rejects {
		t.Run("reject "+tt.name, func(t *testing.T) {
			got, err := NormalizeEndpoint(tt.raw)
			if err == nil {
				t.Fatalf("NormalizeEndpoint(%q) = %q, want an error", tt.raw, got)
			}
			if !strings.HasPrefix(err.Error(), "invalid endpoint ") {
				t.Fatalf("NormalizeEndpoint(%q) error %q lacks the house prefix", tt.raw, err)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// metadata grammar (D1)
// ---------------------------------------------------------------------------

func TestEncodeMetadata(t *testing.T) {
	t.Run("single pair wire-exact", func(t *testing.T) {
		got, err := encodeMetadata(map[string]string{"filename": "a b.txt"})
		if err != nil {
			t.Fatalf("encodeMetadata: %v", err)
		}
		if want := "filename YSBiLnR4dA=="; got != want {
			t.Fatalf("encodeMetadata = %q, want %q", got, want)
		}
	})

	t.Run("multi pair parses to exact base64 values", func(t *testing.T) {
		wire, err := encodeMetadata(stdMetadata)
		if err != nil {
			t.Fatalf("encodeMetadata: %v", err)
		}
		got := parseMetadataHeader(wire)
		want := wantMetadataPairs()
		for k, v := range want {
			if got[k] != v {
				t.Fatalf("metadata pair %q = %q, want %q (wire %q)", k, got[k], v, wire)
			}
			delete(got, k)
		}
		if len(got) != 0 {
			t.Fatalf("metadata wire carries unexpected extra pairs: %v", got)
		}
	})

	t.Run("empty metadata is empty wire", func(t *testing.T) {
		got, err := encodeMetadata(nil)
		if err != nil {
			t.Fatalf("encodeMetadata(nil): %v", err)
		}
		if got != "" {
			t.Fatalf("encodeMetadata(nil) = %q, want empty", got)
		}
	})

	t.Run("invalid keys", func(t *testing.T) {
		const wantSuffix = `: metadata keys must be non-empty printable ASCII without spaces or commas`
		for _, key := range []string{"", "bad key", "bad,key", "kéy", "tab\tkey"} {
			got, err := encodeMetadata(map[string]string{key: "value"})
			if err == nil {
				t.Fatalf("encodeMetadata(key %q) = %q, want an error", key, got)
			}
			want := fmt.Sprintf("tus: invalid metadata key %q", key) + wantSuffix
			if err.Error() != want {
				t.Fatalf("encodeMetadata(key %q) error = %q, want %q", key, err.Error(), want)
			}
		}
	})
}

// ---------------------------------------------------------------------------
// Create: wire contract and Location handling (D1)
// ---------------------------------------------------------------------------

func TestCreateWireContract(t *testing.T) {
	for _, mode := range []struct {
		name     string
		relative bool
	}{{"relative location", true}, {"absolute location", false}} {
		t.Run(mode.name, func(t *testing.T) {
			ft := newFakeTus(t, false)
			ft.relativeLocation = mode.relative
			c := NewClient(ft.endpoint(), nil, nil, false)

			got, err := c.Create(context.Background(), 42, stdMetadata)
			if err != nil {
				t.Fatalf("Create: %v", err)
			}
			if got != ft.uploadURL() {
				t.Fatalf("Create returned %q, want %q", got, ft.uploadURL())
			}

			posts, _, _, _ := ft.counts()
			if posts != 1 {
				t.Fatalf("posts = %d, want 1", posts)
			}
			cap := ft.lastPost()
			if v := cap.header.Get("Tus-Resumable"); v != "1.0.0" {
				t.Fatalf("POST Tus-Resumable = %q, want 1.0.0", v)
			}
			if v := cap.header.Get("Upload-Length"); v != "42" {
				t.Fatalf("POST Upload-Length = %q, want 42", v)
			}
			wire := cap.header.Get("Upload-Metadata")
			parsed := parseMetadataHeader(wire)
			want := wantMetadataPairs()
			for k, v := range want {
				if parsed[k] != v {
					t.Fatalf("wire metadata %q = %q, want %q (wire %q)", k, parsed[k], v, wire)
				}
				delete(parsed, k)
			}
			if len(parsed) != 0 {
				t.Fatalf("wire metadata carries unexpected extra pairs: %v", parsed)
			}
		})
	}
}

func TestCreateFaultStatuses(t *testing.T) {
	t.Run("401 with nil handler surfaces ErrUnauthorized", func(t *testing.T) {
		ft := newFakeTus(t, false)
		ft.queuePost(fakeFault{status: http.StatusUnauthorized, body: "who are you"})
		c := NewClient(ft.endpoint(), nil, nil, false)

		_, err := c.Create(context.Background(), 1, nil)
		if !errors.Is(err, ErrUnauthorized) {
			t.Fatalf("Create error = %v, want errors.Is ErrUnauthorized", err)
		}
		want := "tus: the server returned 401 Unauthorized: who are you"
		if err.Error() != want {
			t.Fatalf("Create error = %q, want %q", err.Error(), want)
		}
	})

	t.Run("500 carries status and bounded body", func(t *testing.T) {
		ft := newFakeTus(t, false)
		long := strings.Repeat("A", 5000)
		ft.queuePost(fakeFault{status: http.StatusInternalServerError, body: long})
		c := NewClient(ft.endpoint(), nil, nil, false)

		_, err := c.Create(context.Background(), 1, nil)
		if err == nil {
			t.Fatal("Create succeeded, want an error")
		}
		want := "tus: the server returned 500 Internal Server Error: " + strings.Repeat("A", 4096)
		if err.Error() != want {
			t.Fatalf("Create error = %q, want body bounded at maxDiagBytes", err.Error())
		}
		if strings.Contains(err.Error(), strings.Repeat("A", 4097)) {
			t.Fatal("Create error echoes past maxDiagBytes")
		}
	})
}

func TestCreateLocationFaults(t *testing.T) {
	t.Run("missing Location", func(t *testing.T) {
		ft := newFakeTus(t, false)
		ft.queuePost(fakeFault{status: http.StatusCreated, noLocation: true})
		c := NewClient(ft.endpoint(), nil, nil, false)

		_, err := c.Create(context.Background(), 1, nil)
		want := "tus: the server's 201 response carries no Location header"
		if err == nil || err.Error() != want {
			t.Fatalf("Create error = %v, want %q", err, want)
		}
	})

	t.Run("unparseable Location bounded echo", func(t *testing.T) {
		ft := newFakeTus(t, false)
		bad := "http://[" + strings.Repeat("x", 80)
		ft.queuePost(fakeFault{status: http.StatusCreated, location: bad})
		c := NewClient(ft.endpoint(), nil, nil, false)

		_, err := c.Create(context.Background(), 1, nil)
		if err == nil {
			t.Fatal("Create succeeded, want an error")
		}
		if !strings.Contains(err.Error(), "...(truncated)") {
			t.Fatalf("Create error %q lacks the bounded-echo marker", err.Error())
		}
		if strings.Contains(err.Error(), strings.Repeat("x", 65)) {
			t.Fatalf("Create error %q echoes the Location past maxEchoChars", err.Error())
		}
	})
}

// ---------------------------------------------------------------------------
// HeadOffset (D1)
// ---------------------------------------------------------------------------

func TestHeadOffset(t *testing.T) {
	t.Run("parses offset and length", func(t *testing.T) {
		ft := newFakeTus(t, false)
		ft.seed(true, 42, 100)
		c := NewClient(ft.endpoint(), nil, nil, false)

		offset, length, exists, err := c.HeadOffset(context.Background(), ft.uploadURL())
		if err != nil {
			t.Fatalf("HeadOffset: %v", err)
		}
		if !exists || offset != 42 || length != 100 {
			t.Fatalf("HeadOffset = (%d, %d, %v), want (42, 100, true)", offset, length, exists)
		}
		_, heads, _, _ := ft.counts()
		if heads != 1 {
			t.Fatalf("heads = %d, want 1", heads)
		}
		cap := ft.headAt(0)
		if v := cap.header.Get("Tus-Resumable"); v != "1.0.0" {
			t.Fatalf("HEAD Tus-Resumable = %q, want 1.0.0", v)
		}
	})

	t.Run("404 is data, not an error", func(t *testing.T) {
		ft := newFakeTus(t, false)
		ft.seed(false, 0, 0)
		c := NewClient(ft.endpoint(), nil, nil, false)

		offset, length, exists, err := c.HeadOffset(context.Background(), ft.uploadURL())
		if err != nil {
			t.Fatalf("HeadOffset: %v", err)
		}
		if exists || offset != 0 || length != 0 {
			t.Fatalf("HeadOffset = (%d, %d, %v), want (0, 0, false)", offset, length, exists)
		}
	})

	t.Run("200 without Upload-Offset is an error", func(t *testing.T) {
		ft := newFakeTus(t, false)
		ft.queueHead(fakeFault{status: http.StatusOK})
		c := NewClient(ft.endpoint(), nil, nil, false)

		_, _, _, err := c.HeadOffset(context.Background(), ft.uploadURL())
		want := "tus: the HEAD response carries no Upload-Offset header"
		if err == nil || err.Error() != want {
			t.Fatalf("HeadOffset error = %v, want %q", err, want)
		}
	})

	t.Run("200 without Upload-Length is an error", func(t *testing.T) {
		ft := newFakeTus(t, false)
		ft.queueHead(fakeFault{status: http.StatusOK, extra: map[string]string{"Upload-Offset": "7"}})
		c := NewClient(ft.endpoint(), nil, nil, false)

		_, _, _, err := c.HeadOffset(context.Background(), ft.uploadURL())
		want := "tus: the HEAD response carries no Upload-Length header"
		if err == nil || err.Error() != want {
			t.Fatalf("HeadOffset error = %v, want %q", err, want)
		}
	})

	t.Run("unparseable Upload-Offset bounded echo", func(t *testing.T) {
		ft := newFakeTus(t, false)
		ft.queueHead(fakeFault{status: http.StatusOK, extra: map[string]string{
			"Upload-Offset": strings.Repeat("9", 100),
			"Upload-Length": "100",
		}})
		c := NewClient(ft.endpoint(), nil, nil, false)

		_, _, _, err := c.HeadOffset(context.Background(), ft.uploadURL())
		if err == nil {
			t.Fatal("HeadOffset succeeded, want an error")
		}
		if !strings.Contains(err.Error(), "...(truncated)") {
			t.Fatalf("HeadOffset error %q lacks the bounded-echo marker", err.Error())
		}
	})
}

// ---------------------------------------------------------------------------
// Patch: wire contract, offset tracking, sentinel statuses (D1, D5)
// ---------------------------------------------------------------------------

func TestPatchOffsetTracking(t *testing.T) {
	ft := newFakeTus(t, false)
	ft.seed(true, 42, 100)
	c := NewClient(ft.endpoint(), nil, nil, false)

	chunk := []byte("hello")
	got, err := c.Patch(context.Background(), ft.uploadURL(), 42, bytes.NewReader(chunk), int64(len(chunk)))
	if err != nil {
		t.Fatalf("Patch: %v", err)
	}
	if got != 47 {
		t.Fatalf("Patch returned offset %d, want 47", got)
	}

	_, _, patches, _ := ft.counts()
	if patches != 1 {
		t.Fatalf("patches = %d, want 1", patches)
	}
	cap := ft.patchAt(0)
	if v := cap.header.Get("Tus-Resumable"); v != "1.0.0" {
		t.Fatalf("PATCH Tus-Resumable = %q, want 1.0.0", v)
	}
	if v := cap.header.Get("Upload-Offset"); v != "42" {
		t.Fatalf("PATCH wire Upload-Offset = %q, want 42", v)
	}
	if v := cap.header.Get("Content-Type"); v != "application/offset+octet-stream" {
		t.Fatalf("PATCH Content-Type = %q, want application/offset+octet-stream", v)
	}
	if cap.contentLength != int64(len(chunk)) {
		t.Fatalf("PATCH ContentLength = %d, want %d (explicit, never chunked TE)", cap.contentLength, len(chunk))
	}
	if len(cap.transferEncoding) != 0 {
		t.Fatalf("PATCH used chunked transfer encoding: %v", cap.transferEncoding)
	}
	if !bytes.Equal(cap.body, chunk) {
		t.Fatalf("PATCH body = %q, want %q", cap.body, chunk)
	}
}

func TestPatchSentinelStatuses(t *testing.T) {
	tests := []struct {
		name     string
		fault    fakeFault
		sentinel error
		want     string // byte-exact error text
	}{
		{
			name:     "409 without Upload-Offset header maps to ErrMismatchedOffset",
			fault:    fakeFault{status: http.StatusConflict, body: "offset mismatch"},
			sentinel: ErrMismatchedOffset,
			want:     "tus: the server rejected the PATCH offset 42 with 409 Conflict: offset mismatch",
		},
		{
			name:     "413 maps to ErrSizeExceeded",
			fault:    fakeFault{status: http.StatusRequestEntityTooLarge, body: "cap"},
			sentinel: ErrSizeExceeded,
			want:     "tus: the server returned 413 Request Entity Too Large: cap",
		},
		{
			name:     "404 maps to ErrNotFound",
			fault:    fakeFault{status: http.StatusNotFound, body: "gone"},
			sentinel: ErrNotFound,
			want:     "tus: the server returned 404 Not Found: gone",
		},
		{
			name:     "401 with nil handler maps to ErrUnauthorized",
			fault:    fakeFault{status: http.StatusUnauthorized, body: "who"},
			sentinel: ErrUnauthorized,
			want:     "tus: the server returned 401 Unauthorized: who",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ft := newFakeTus(t, false)
			ft.seed(true, 42, 100)
			ft.queuePatch(tt.fault)
			c := NewClient(ft.endpoint(), nil, nil, false)

			_, err := c.Patch(context.Background(), ft.uploadURL(), 42, bytes.NewReader(patternBytes(8)), 8)
			if !errors.Is(err, tt.sentinel) {
				t.Fatalf("Patch error = %v, want errors.Is %v", err, tt.sentinel)
			}
			if err.Error() != tt.want {
				t.Fatalf("Patch error = %q, want %q", err.Error(), tt.want)
			}
		})
	}

	t.Run("204 without Upload-Offset is an error", func(t *testing.T) {
		ft := newFakeTus(t, false)
		ft.seed(true, 0, 100)
		ft.queuePatch(fakeFault{status: http.StatusNoContent})
		c := NewClient(ft.endpoint(), nil, nil, false)

		_, err := c.Patch(context.Background(), ft.uploadURL(), 0, bytes.NewReader(patternBytes(8)), 8)
		want := "tus: the 204 response carries no Upload-Offset header"
		if err == nil || err.Error() != want {
			t.Fatalf("Patch error = %v, want %q", err, want)
		}
	})
}

// ---------------------------------------------------------------------------
// do(): the SDK transport contract (D1)
// ---------------------------------------------------------------------------

func TestDoAuthorizeRetryOnce(t *testing.T) {
	t.Run("POST retried after successful Authorize", func(t *testing.T) {
		for _, status := range []int{http.StatusUnauthorized, http.StatusForbidden} {
			ft := newFakeTus(t, false)
			ft.queuePost(fakeFault{status: status})
			h := &fakeHandler{ts: oauth2.StaticTokenSource(&oauth2.Token{AccessToken: dummyToken(), TokenType: "Bearer"})}
			c := NewClient(ft.endpoint(), h, nil, false)

			got, err := c.Create(context.Background(), 1, nil)
			if err != nil {
				t.Fatalf("Create with %d then retry: %v", status, err)
			}
			if got != ft.uploadURL() {
				t.Fatalf("Create returned %q, want %q", got, ft.uploadURL())
			}
			posts, _, _, _ := ft.counts()
			if posts != 2 {
				t.Fatalf("posts = %d, want 2 (initial + retry)", posts)
			}
			if h.authorizeCalls != 1 {
				t.Fatalf("authorizeCalls = %d, want 1", h.authorizeCalls)
			}
			if h.tokenSourceCalls != 2 {
				t.Fatalf("tokenSourceCalls = %d, want 2 (bearer per request)", h.tokenSourceCalls)
			}
		}
	})

	t.Run("PATCH retried with the whole body replayed", func(t *testing.T) {
		ft := newFakeTus(t, false)
		ft.seed(true, 0, 100)
		ft.queuePatch(fakeFault{status: http.StatusUnauthorized})
		h := &fakeHandler{ts: oauth2.StaticTokenSource(&oauth2.Token{AccessToken: dummyToken(), TokenType: "Bearer"})}
		c := NewClient(ft.endpoint(), h, nil, false)

		chunk := patternBytes(8)
		got, err := c.Patch(context.Background(), ft.uploadURL(), 0, bytes.NewReader(chunk), int64(len(chunk)))
		if err != nil {
			t.Fatalf("Patch with 401 then retry: %v", err)
		}
		if got != 8 {
			t.Fatalf("Patch returned offset %d, want 8", got)
		}
		_, _, patches, _ := ft.counts()
		if patches != 2 {
			t.Fatalf("patches = %d, want 2", patches)
		}
		for i := 0; i < 2; i++ {
			cap := ft.patchAt(i)
			if !bytes.Equal(cap.body, chunk) {
				t.Fatalf("PATCH #%d body = %q, want the full chunk replayed", i+1, cap.body)
			}
			if v := cap.header.Get("Authorization"); v != "Bearer "+dummyToken() {
				t.Fatalf("PATCH #%d Authorization = %q, want the bearer on every request", i+1, v)
			}
		}
		if h.authorizeCalls != 1 {
			t.Fatalf("authorizeCalls = %d, want 1", h.authorizeCalls)
		}
	})
}

func TestDoAuthorizeErrorNoRetry(t *testing.T) {
	ft := newFakeTus(t, false)
	ft.queuePost(fakeFault{status: http.StatusUnauthorized})
	h := &fakeHandler{authorizeErr: errors.New("deny")}
	c := NewClient(ft.endpoint(), h, nil, false)

	_, err := c.Create(context.Background(), 1, nil)
	if err == nil {
		t.Fatal("Create succeeded, want an error")
	}
	if !errors.Is(err, h.authorizeErr) {
		t.Fatalf("Create error = %v, want it to wrap the Authorize error", err)
	}
	want := "tus: the auth handler failed to authorize the request: deny"
	if err.Error() != want {
		t.Fatalf("Create error = %q, want %q", err.Error(), want)
	}
	posts, _, _, _ := ft.counts()
	if posts != 1 {
		t.Fatalf("posts = %d, want exactly 1 (zero retries after an Authorize error)", posts)
	}
}

func TestDoTokenSourceErrorZeroRequests(t *testing.T) {
	ft := newFakeTus(t, false)
	h := &fakeHandler{tsErr: errors.New("token source failed")}
	c := NewClient(ft.endpoint(), h, nil, false)

	_, err := c.Create(context.Background(), 1, nil)
	if err == nil {
		t.Fatal("Create succeeded, want an error")
	}
	if !errors.Is(err, h.tsErr) {
		t.Fatalf("Create error = %v, want it to wrap the TokenSource error", err)
	}
	want := "tus: the auth handler failed to provide a token source: token source failed"
	if err.Error() != want {
		t.Fatalf("Create error = %q, want %q", err.Error(), want)
	}
	posts, _, _, _ := ft.counts()
	if posts != 0 {
		t.Fatalf("posts = %d, want 0 (aborted before any request)", posts)
	}
}

func TestDoTokenFetchErrorAborts(t *testing.T) {
	ft := newFakeTus(t, false)
	h := &fakeHandler{tokenErr: errors.New("token fetch failed")}
	c := NewClient(ft.endpoint(), h, nil, false)

	_, err := c.Create(context.Background(), 1, nil)
	if err == nil {
		t.Fatal("Create succeeded, want an error")
	}
	if !errors.Is(err, h.tokenErr) {
		t.Fatalf("Create error = %v, want it to wrap the Token error", err)
	}
	want := "tus: the auth handler failed to fetch a token: token fetch failed"
	if err.Error() != want {
		t.Fatalf("Create error = %q, want %q", err.Error(), want)
	}
	posts, _, _, _ := ft.counts()
	if posts != 0 {
		t.Fatalf("posts = %d, want 0 (aborted before any request)", posts)
	}
}

func TestInvalidGrantProceedsHeaderless(t *testing.T) {
	ft := newFakeTus(t, false)
	ft.queuePost(fakeFault{status: http.StatusUnauthorized})
	h := &fakeHandler{tokenErr: &oauth2.RetrieveError{ErrorCode: "invalid_grant"}}
	c := NewClient(ft.endpoint(), h, nil, false)

	got, err := c.Create(context.Background(), 1, nil)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if got != ft.uploadURL() {
		t.Fatalf("Create returned %q, want %q", got, ft.uploadURL())
	}
	posts, _, _, _ := ft.counts()
	if posts != 2 {
		t.Fatalf("posts = %d, want 2 (headerless attempt + Authorize retry)", posts)
	}
	if h.authorizeCalls != 1 {
		t.Fatalf("authorizeCalls = %d, want 1 (the 401 drove Authorize)", h.authorizeCalls)
	}
	cap := ft.lastPost()
	if v := cap.header.Get("Authorization"); v != "" {
		t.Fatalf("POST Authorization = %q, want no header (invalid_grant proceeds headerless)", v)
	}
}

func TestBearerInjection(t *testing.T) {
	ft := newFakeTus(t, false)
	h := &fakeHandler{ts: oauth2.StaticTokenSource(&oauth2.Token{AccessToken: dummyToken(), TokenType: "Bearer"})}
	c := NewClient(ft.endpoint(), h, nil, false)

	if _, err := c.Create(context.Background(), 1, nil); err != nil {
		t.Fatalf("Create: %v", err)
	}
	cap := ft.lastPost()
	if v := cap.header.Get("Authorization"); v != "Bearer "+dummyToken() {
		t.Fatalf("POST Authorization = %q, want %q", v, "Bearer "+dummyToken())
	}
}

func TestBasicModeTransportHTTPClient(t *testing.T) {
	ft := newFakeTus(t, false)
	auth.RegisterBuiltins()
	du, dp := dummyBasicCreds()
	p, err := auth.ProviderFor("basic")
	if err != nil {
		t.Fatalf("ProviderFor(basic): %v", err)
	}
	handler, _, err := p.Build(context.Background(), auth.Params{
		Target: ft.endpoint(),
		Creds:  auth.Credentials{ClientID: du, ClientSecret: dp},
	})
	if err != nil {
		t.Fatalf("basic Build: %v", err)
	}
	bc, ok := handler.(interface{ TransportHTTPClient() *http.Client })
	if !ok {
		t.Fatal("basic handler does not implement TransportHTTPClient")
	}
	c := NewClient(ft.endpoint(), handler, bc.TransportHTTPClient().Transport, false)

	if _, err := c.Create(context.Background(), 1, nil); err != nil {
		t.Fatalf("Create: %v", err)
	}
	want := "Basic " + base64.StdEncoding.EncodeToString([]byte(du+":"+dp))
	cap := ft.lastPost()
	if v := cap.header.Get("Authorization"); v != want {
		t.Fatalf("POST Authorization = %q, want %q", v, want)
	}
}

// ---------------------------------------------------------------------------
// Upload orchestration (D3)
// ---------------------------------------------------------------------------

func TestUploadChunkBoundaries(t *testing.T) {
	if defaultChunkSize != int64(64<<20) {
		t.Fatalf("defaultChunkSize = %d, want %d", defaultChunkSize, int64(64<<20))
	}
	tests := []struct {
		name        string
		chunk       int64
		size        int
		wantPatches int
	}{
		{"size below one chunk", 8, 7, 1},
		{"exact chunk boundary", 8, 8, 1},
		{"one byte past the boundary", 8, 9, 2},
		{"chunk larger than the file", 64, 9, 1},
		{"chunk size zero takes the default", 0, 100, 1},
		{"chunk size negative takes the default", -1, 100, 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ft := newFakeTus(t, false)
			c := NewClient(ft.endpoint(), nil, nil, false)
			content := patternBytes(tt.size)
			f := tempFile(t, content)

			progressTotal := 0
			var lastSent, lastTotal int64
			url, err := c.Upload(context.Background(), f, int64(tt.size), stdMetadata, tt.chunk, "", 3,
				func(sent, total int64) {
					progressTotal++
					lastSent, lastTotal = sent, total
				})
			if err != nil {
				t.Fatalf("Upload: %v", err)
			}
			if url != ft.uploadURL() {
				t.Fatalf("Upload url = %q, want %q", url, ft.uploadURL())
			}
			posts, _, patches, _ := ft.counts()
			if posts != 1 || patches != tt.wantPatches {
				t.Fatalf("posts=%d patches=%d, want 1/%d", posts, patches, tt.wantPatches)
			}
			cap := ft.lastPost()
			if v := cap.header.Get("Upload-Length"); v != strconv.Itoa(tt.size) {
				t.Fatalf("POST Upload-Length = %q, want %d", v, tt.size)
			}
			var assembled []byte
			limit := int64(defaultChunkSize)
			if tt.chunk > 0 {
				limit = tt.chunk
			}
			for i := 0; i < patches; i++ {
				p := ft.patchAt(i)
				if p.contentLength > limit {
					t.Fatalf("PATCH #%d ContentLength = %d, exceeds the chunk limit %d", i+1, p.contentLength, limit)
				}
				if p.contentLength <= 0 {
					t.Fatalf("PATCH #%d ContentLength = %d, want positive", i+1, p.contentLength)
				}
				assembled = append(assembled, p.body...)
			}
			if !bytes.Equal(assembled, content) {
				t.Fatalf("assembled upload = %d bytes, does not match the %d-byte file", len(assembled), tt.size)
			}
			offset, _, exists, err := c.HeadOffset(context.Background(), ft.uploadURL())
			if err != nil || !exists {
				t.Fatalf("post-upload HeadOffset: (%d, %v, %v)", offset, exists, err)
			}
			if offset != int64(tt.size) {
				t.Fatalf("final server offset = %d, want %d", offset, tt.size)
			}
			if progressTotal != tt.wantPatches {
				t.Fatalf("onProgress fired %d times, want %d (after each successful PATCH)", progressTotal, tt.wantPatches)
			}
			if lastSent != int64(tt.size) || lastTotal != int64(tt.size) {
				t.Fatalf("last progress = (%d, %d), want (%d, %d)", lastSent, lastTotal, tt.size, tt.size)
			}
		})
	}
}

func TestUploadSizeZero(t *testing.T) {
	ft := newFakeTus(t, false)
	ft.queuePost(fakeFault{status: http.StatusBadRequest, body: "upload length must be positive"})
	c := NewClient(ft.endpoint(), nil, nil, false)
	f := tempFile(t, nil)

	_, err := c.Upload(context.Background(), f, 0, stdMetadata, 0, "", 3, nil)
	if err == nil {
		t.Fatal("Upload succeeded, want the fake's 400 to surface")
	}
	if !strings.Contains(err.Error(), "upload length must be positive") {
		t.Fatalf("Upload error %q lacks the bounded server body", err.Error())
	}
	posts, _, patches, _ := ft.counts()
	if posts != 1 || patches != 0 {
		t.Fatalf("posts=%d patches=%d, want 1/0", posts, patches)
	}
	cap := ft.lastPost()
	if v := cap.header.Get("Upload-Length"); v != "0" {
		t.Fatalf("POST Upload-Length = %q, want 0 (the server decides)", v)
	}
}

func TestUploadResumeSwept(t *testing.T) {
	ft := newFakeTus(t, false)
	c := NewClient(ft.endpoint(), nil, nil, false)
	f := tempFile(t, patternBytes(16))

	_, err := c.Upload(context.Background(), f, 16, stdMetadata, 8, ft.uploadURL(), 3, nil)
	want := fmt.Sprintf("tus: resume target %s is no longer in staging; it may have landed or been swept — verify server-side", ft.uploadURL())
	if err == nil || err.Error() != want {
		t.Fatalf("Upload error = %v, want %q", err, want)
	}
	posts, heads, patches, _ := ft.counts()
	if posts != 0 || patches != 0 {
		t.Fatalf("posts=%d patches=%d, want 0/0 (never re-Create, never PATCH)", posts, patches)
	}
	if heads != 1 {
		t.Fatalf("heads = %d, want 1", heads)
	}
}

func TestUploadResumeOffsetPastSize(t *testing.T) {
	ft := newFakeTus(t, false)
	ft.seed(true, 50, 50)
	c := NewClient(ft.endpoint(), nil, nil, false)
	f := tempFile(t, patternBytes(10))

	_, err := c.Upload(context.Background(), f, 10, stdMetadata, 8, ft.uploadURL(), 3, nil)
	want := fmt.Sprintf("tus: the staged upload %s is at offset 50, past the file's size 10; the file may have changed since it was staged", ft.uploadURL())
	if err == nil || err.Error() != want {
		t.Fatalf("Upload error = %v, want %q", err, want)
	}
	_, _, patches, _ := ft.counts()
	if patches != 0 {
		t.Fatalf("patches = %d, want 0 (zero PATCHes past the file size)", patches)
	}
}

func TestUploadResumeContinuesFromServerOffset(t *testing.T) {
	ft := newFakeTus(t, false)
	ft.seed(true, 8, 16)
	c := NewClient(ft.endpoint(), nil, nil, false)
	content := patternBytes(16)
	f := tempFile(t, content)

	progressCalls := 0
	url, err := c.Upload(context.Background(), f, 16, stdMetadata, 8, ft.uploadURL(), 3, func(sent, total int64) {
		progressCalls++
		if sent != 16 || total != 16 {
			t.Errorf("progress = (%d, %d), want (16, 16)", sent, total)
		}
	})
	if err != nil {
		t.Fatalf("Upload: %v", err)
	}
	if url != ft.uploadURL() {
		t.Fatalf("Upload url = %q, want %q", url, ft.uploadURL())
	}
	_, _, patches, _ := ft.counts()
	if patches != 1 {
		t.Fatalf("patches = %d, want 1 (resumed at offset 8, one chunk left)", patches)
	}
	cap := ft.patchAt(0)
	if v := cap.header.Get("Upload-Offset"); v != "8" {
		t.Fatalf("PATCH Upload-Offset = %q, want 8 (the resumed server offset)", v)
	}
	if !bytes.Equal(cap.body, content[8:]) {
		t.Fatalf("PATCH body = %d bytes, want the remaining %d", len(cap.body), 8)
	}
	if progressCalls != 1 {
		t.Fatalf("onProgress fired %d times, want 1", progressCalls)
	}
}

func TestUploadResumeAlreadyComplete(t *testing.T) {
	ft := newFakeTus(t, false)
	ft.seed(true, 16, 16)
	c := NewClient(ft.endpoint(), nil, nil, false)
	f := tempFile(t, patternBytes(16))

	url, err := c.Upload(context.Background(), f, 16, stdMetadata, 8, ft.uploadURL(), 3, nil)
	if err != nil {
		t.Fatalf("Upload: %v", err)
	}
	if url != ft.uploadURL() {
		t.Fatalf("Upload url = %q, want %q", url, ft.uploadURL())
	}
	posts, heads, patches, _ := ft.counts()
	if posts != 0 || patches != 0 || heads != 1 {
		t.Fatalf("posts=%d heads=%d patches=%d, want 0/1/0 (fully staged, nothing to PATCH)", posts, heads, patches)
	}
}

func TestUpload409ResyncPermanentAbort(t *testing.T) {
	ft := newFakeTus(t, false)
	ft.queuePatch(fakeFault{status: http.StatusConflict, body: "offset mismatch"})
	c := NewClient(ft.endpoint(), nil, nil, false)
	f := tempFile(t, patternBytes(16))

	_, err := c.Upload(context.Background(), f, 16, stdMetadata, 8, "", 3, nil)
	if !errors.Is(err, ErrMismatchedOffset) {
		t.Fatalf("Upload error = %v, want errors.Is ErrMismatchedOffset", err)
	}
	want := "tus: the upload stalled at client offset 0 (the server is at offset 0): " +
		"tus: the server rejected the PATCH offset 0 with 409 Conflict: offset mismatch"
	if err.Error() != want {
		t.Fatalf("Upload error = %q, want %q", err.Error(), want)
	}
	posts, heads, patches, _ := ft.counts()
	if posts != 1 || patches != 1 || heads != 1 {
		t.Fatalf("posts=%d patches=%d heads=%d, want 1/1/1 (the HEAD resync must arrive)", posts, patches, heads)
	}
}

func TestUploadResyncAdvancedRetriesFromServerOffset(t *testing.T) {
	ft := newFakeTus(t, false)
	ft.queuePatch(fakeFault{status: http.StatusInternalServerError, body: "disk hiccup", advance: 5})
	c := NewClient(ft.endpoint(), nil, nil, false)
	content := patternBytes(16)
	f := tempFile(t, content)

	url, err := c.Upload(context.Background(), f, 16, stdMetadata, 8, "", 3, nil)
	if err != nil {
		t.Fatalf("Upload: %v", err)
	}
	if url != ft.uploadURL() {
		t.Fatalf("Upload url = %q, want %q", url, ft.uploadURL())
	}
	_, heads, patches, _ := ft.counts()
	if patches != 3 || heads != 1 {
		t.Fatalf("patches=%d heads=%d, want 3/1 (the resync lands mid-chunk: 8+3 bytes remain)", patches, heads)
	}
	cap := ft.patchAt(1)
	if v := cap.header.Get("Upload-Offset"); v != "5" {
		t.Fatalf("PATCH #2 Upload-Offset = %q, want 5 (the resynced server offset)", v)
	}
	if !bytes.Equal(cap.body, content[5:13]) {
		t.Fatalf("PATCH #2 body = %d bytes, want the next 8 from offset 5", len(cap.body))
	}
	cap = ft.patchAt(2)
	if v := cap.header.Get("Upload-Offset"); v != "13" {
		t.Fatalf("PATCH #3 Upload-Offset = %q, want 13", v)
	}
	if !bytes.Equal(cap.body, content[13:]) {
		t.Fatalf("PATCH #3 body = %d bytes, want the final 3", len(cap.body))
	}
}

func TestUploadResyncCompleteReturnsURL(t *testing.T) {
	ft := newFakeTus(t, false)
	ft.queuePatch(fakeFault{status: http.StatusInternalServerError, advance: 16})
	c := NewClient(ft.endpoint(), nil, nil, false)
	f := tempFile(t, patternBytes(16))

	url, err := c.Upload(context.Background(), f, 16, stdMetadata, 8, "", 3, nil)
	if err != nil {
		t.Fatalf("Upload: %v", err)
	}
	if url != ft.uploadURL() {
		t.Fatalf("Upload url = %q, want %q", url, ft.uploadURL())
	}
	posts, heads, patches, _ := ft.counts()
	if posts != 1 || patches != 1 || heads != 1 {
		t.Fatalf("posts=%d patches=%d heads=%d, want 1/1/1 (resync == size completes the upload)", posts, patches, heads)
	}
}

func TestUploadResync404Swept(t *testing.T) {
	ft := newFakeTus(t, false)
	ft.queuePatch(fakeFault{status: http.StatusInternalServerError, body: "boom"})
	ft.queueHead(fakeFault{status: http.StatusNotFound})
	c := NewClient(ft.endpoint(), nil, nil, false)
	f := tempFile(t, patternBytes(16))

	_, err := c.Upload(context.Background(), f, 16, stdMetadata, 8, "", 3, nil)
	want := fmt.Sprintf("tus: upload %s is no longer in staging; it may have landed or been swept — verify server-side", ft.uploadURL())
	if err == nil || err.Error() != want {
		t.Fatalf("Upload error = %v, want %q", err, want)
	}
	posts, heads, patches, _ := ft.counts()
	if posts != 1 || patches != 1 || heads != 1 {
		t.Fatalf("posts=%d patches=%d heads=%d, want 1/1/1 (never fake success, never re-Create)", posts, patches, heads)
	}
}

func TestUploadResync5xxConsumesRetry(t *testing.T) {
	ft := newFakeTus(t, false)
	ft.queuePatch(fakeFault{status: http.StatusInternalServerError, body: "transient"})
	c := NewClient(ft.endpoint(), nil, nil, false)
	f := tempFile(t, patternBytes(16))

	url, err := c.Upload(context.Background(), f, 16, stdMetadata, 8, "", 3, nil)
	if err != nil {
		t.Fatalf("Upload: %v", err)
	}
	if url != ft.uploadURL() {
		t.Fatalf("Upload url = %q, want %q", url, ft.uploadURL())
	}
	_, heads, patches, _ := ft.counts()
	if patches != 3 || heads != 1 {
		t.Fatalf("patches=%d heads=%d, want 3/1 (one retry consumed after the resync; the file still needs both chunks)", patches, heads)
	}
	if v := ft.patchAt(1).header.Get("Upload-Offset"); v != "0" {
		t.Fatalf("PATCH #2 Upload-Offset = %q, want 0 (retried from the same offset)", v)
	}
	if v := ft.patchAt(2).header.Get("Upload-Offset"); v != "8" {
		t.Fatalf("PATCH #3 Upload-Offset = %q, want 8 (the second chunk)", v)
	}
}

func TestUploadRetryZeroExhausts(t *testing.T) {
	ft := newFakeTus(t, false)
	ft.queuePatch(fakeFault{status: http.StatusInternalServerError, body: "transient"})
	c := NewClient(ft.endpoint(), nil, nil, false)
	f := tempFile(t, patternBytes(16))

	_, err := c.Upload(context.Background(), f, 16, stdMetadata, 8, "", 0, nil)
	want := "tus: the server returned 500 Internal Server Error: transient"
	if err == nil || err.Error() != want {
		t.Fatalf("Upload error = %v, want %q (low-context; the CLI adds URL+offset)", err, want)
	}
	_, heads, patches, _ := ft.counts()
	if patches != 1 || heads != 1 {
		t.Fatalf("patches=%d heads=%d, want 1/1 (resync classifies, no retry remains)", patches, heads)
	}
}

func TestUploadResyncFailureConsumesRetry(t *testing.T) {
	ft := newFakeTus(t, false)
	ft.queuePatch(fakeFault{status: http.StatusInternalServerError})
	ft.queueHead(fakeFault{status: http.StatusInternalServerError, body: "head hiccup"})
	c := NewClient(ft.endpoint(), nil, nil, false)
	f := tempFile(t, patternBytes(16))

	url, err := c.Upload(context.Background(), f, 16, stdMetadata, 8, "", 3, nil)
	if err != nil {
		t.Fatalf("Upload: %v", err)
	}
	if url != ft.uploadURL() {
		t.Fatalf("Upload url = %q, want %q", url, ft.uploadURL())
	}
	_, heads, patches, _ := ft.counts()
	if patches != 3 || heads != 1 {
		t.Fatalf("patches=%d heads=%d, want 3/1 (resync failure consumes one retry)", patches, heads)
	}
}

func TestUploadHijackOneResyncCompletes(t *testing.T) {
	ft := newFakeTus(t, false)
	ft.queuePatch(fakeFault{hijack: true, advance: 8})
	c := NewClient(ft.endpoint(), nil, nil, false)
	f := tempFile(t, patternBytes(16))

	url, err := c.Upload(context.Background(), f, 16, stdMetadata, 8, "", 3, nil)
	if err != nil {
		t.Fatalf("Upload: %v", err)
	}
	if url != ft.uploadURL() {
		t.Fatalf("Upload url = %q, want %q", url, ft.uploadURL())
	}
	_, heads, patches, _ := ft.counts()
	if patches != 2 || heads != 1 {
		t.Fatalf("patches=%d heads=%d, want 2/1 (exactly one HEAD resync after the drop)", patches, heads)
	}
	cap := ft.patchAt(1)
	if v := cap.header.Get("Upload-Offset"); v != "8" {
		t.Fatalf("PATCH #2 Upload-Offset = %q, want 8 (the server kept the partial bytes)", v)
	}
}

func TestUploadContextCancelMidPatch(t *testing.T) {
	ft := newFakeTus(t, false)
	ft.queuePatch(fakeFault{hang: true})
	c := NewClient(ft.endpoint(), nil, nil, false)
	f := tempFile(t, patternBytes(16))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	type result struct {
		url string
		err error
	}
	ch := make(chan result, 1)
	go func() {
		url, err := c.Upload(ctx, f, 16, stdMetadata, 8, "", 3, nil)
		ch <- result{url, err}
	}()

	waitFor(t, func() bool {
		_, _, patches, _ := ft.counts()
		return patches >= 1
	}, "the PATCH to reach the fake")
	cancel()

	select {
	case res := <-ch:
		if !errors.Is(res.err, context.Canceled) {
			t.Fatalf("Upload error = %v, want errors.Is context.Canceled", res.err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Upload did not return after the context was canceled")
	}
	posts, heads, patches, _ := ft.counts()
	if posts != 1 || patches != 1 || heads != 0 {
		t.Fatalf("posts=%d patches=%d heads=%d, want 1/1/0 (abort before any resync HEAD)", posts, patches, heads)
	}
}

// ---------------------------------------------------------------------------
// TLS rows (--insecure; the full flag lands in task 2, the client seam here)
// ---------------------------------------------------------------------------

func TestUploadTLSInsecure(t *testing.T) {
	ft := newFakeTus(t, true)
	c := NewClient(ft.endpoint(), nil, nil, true)
	f := tempFile(t, patternBytes(16))

	url, err := c.Upload(context.Background(), f, 16, stdMetadata, 8, "", 3, nil)
	if err != nil {
		t.Fatalf("Upload with insecure=true: %v", err)
	}
	if url != ft.uploadURL() {
		t.Fatalf("Upload url = %q, want %q", url, ft.uploadURL())
	}
}

func TestUploadTLSRejectedWithoutInsecure(t *testing.T) {
	ft := newFakeTus(t, true)
	c := NewClient(ft.endpoint(), nil, nil, false)
	f := tempFile(t, patternBytes(16))

	_, err := c.Upload(context.Background(), f, 16, stdMetadata, 8, "", 3, nil)
	if err == nil {
		t.Fatal("Upload succeeded against an untrusted TLS server, want a verification failure")
	}
	var certErr *tls.CertificateVerificationError
	if !errors.As(err, &certErr) {
		t.Fatalf("Upload error = %v, want errors.As *tls.CertificateVerificationError", err)
	}
}

// ---------------------------------------------------------------------------
// Delete (D1)
// ---------------------------------------------------------------------------

func TestDeleteStatuses(t *testing.T) {
	t.Run("204 succeeds", func(t *testing.T) {
		ft := newFakeTus(t, false)
		ft.seed(true, 16, 16)
		c := NewClient(ft.endpoint(), nil, nil, false)

		if err := c.Delete(context.Background(), ft.uploadURL()); err != nil {
			t.Fatalf("Delete: %v", err)
		}
		_, _, _, deletes := ft.counts()
		if deletes != 1 {
			t.Fatalf("deletes = %d, want 1", deletes)
		}
	})

	t.Run("404 maps to ErrNotFound", func(t *testing.T) {
		ft := newFakeTus(t, false)
		ft.seed(false, 0, 0)
		c := NewClient(ft.endpoint(), nil, nil, false)

		err := c.Delete(context.Background(), ft.uploadURL())
		if !errors.Is(err, ErrNotFound) {
			t.Fatalf("Delete error = %v, want errors.Is ErrNotFound", err)
		}
	})

	t.Run("410 rides the generic wrapper", func(t *testing.T) {
		ft := newFakeTus(t, false)
		ft.seed(true, 16, 16)
		ft.queueDelete(fakeFault{status: http.StatusGone, body: "gone"})
		c := NewClient(ft.endpoint(), nil, nil, false)

		err := c.Delete(context.Background(), ft.uploadURL())
		if err == nil {
			t.Fatal("Delete succeeded, want an error")
		}
		if errors.Is(err, ErrNotFound) {
			t.Fatalf("Delete error = %v, want the generic wrapper (410 has no sentinel)", err)
		}
		want := "tus: the server returned 410 Gone: gone"
		if err.Error() != want {
			t.Fatalf("Delete error = %q, want %q", err.Error(), want)
		}
	})
}

// TestUploadErrorCarriesURL pins the fix-wave contract: once the upload URL
// is known (Create succeeded), every PATCH-loop error path returns it
// alongside the error, so the CLI failure line can name the wire target.
func TestUploadErrorCarriesURL(t *testing.T) {
	ft := newFakeTus(t, false)
	ft.seed(true, 0, 64)
	ft.queuePatch(fakeFault{status: http.StatusInternalServerError, body: "boom"})
	f := tempFile(t, patternBytes(64))
	defer func() { _ = f.Close() }()

	url, err := NewClient(ft.endpoint(), nil, nil, false).Upload(context.Background(), f, 64, nil, 0, "", 0, nil)
	if err == nil {
		t.Fatal("Upload with a PATCH fault and no retry budget: nil error")
	}
	if url != ft.uploadURL() {
		t.Fatalf("Upload error URL = %q, want the staged %q", url, ft.uploadURL())
	}
}

// TestCreateLocationDowngradeWarning pins the Warn channel: an https
// endpoint whose 201 Location redirects the upload to plain http emits the
// functional security note BEFORE any PATCH fires; an https Location emits
// nothing.
func TestCreateLocationDowngradeWarning(t *testing.T) {
	var warnBuf bytes.Buffer

	ft := newFakeTus(t, true) // https endpoint
	ft.queuePost(fakeFault{status: http.StatusCreated, location: "http://attacker.example.com/files/abc123"})
	c := NewClient(ft.endpoint(), nil, nil, true)
	c.Warn = &warnBuf

	url, err := c.Create(context.Background(), 10, nil)
	if err != nil {
		t.Fatalf("Create with an http Location: %v", err)
	}
	if url != "http://attacker.example.com/files/abc123" {
		t.Fatalf("Create URL = %q, want the redirected Location", url)
	}
	want := "warning: the server redirected the upload to plain http (http://attacker.example.com/files/abc123); the credential travels unencrypted\n"
	if warnBuf.String() != want {
		t.Fatalf("Warn = %q, want %q", warnBuf.String(), want)
	}

	// https endpoint + https Location: no note.
	ft2 := newFakeTus(t, true)
	c2 := NewClient(ft2.endpoint(), nil, nil, true)
	c2.Warn = &warnBuf
	if _, err := c2.Create(context.Background(), 10, nil); err != nil {
		t.Fatalf("Create with an https Location: %v", err)
	}
	if warnBuf.String() != want {
		t.Fatalf("Warn after an https Location = %q, want unchanged %q", warnBuf.String(), want)
	}
}
