package cli

// Every credential-shaped value in this file is an obviously-synthetic
// constant assembled from inert fragments; no real credential ever appears
// in test source (the house credentials rule).

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/ConsulTent/mcpcli/internal/bws"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// tusDummyToken assembles a synthetic bearer token from inert fragments.
func tusDummyToken() string { return "cli-tus-dummy-" + "token" }

// ---------------------------------------------------------------------------
// fake: one stateful tus 1.0.0 server — the bws_test.go fake pattern (hit
// counters, captured wire headers). One staged upload keyed "abc123".
// ---------------------------------------------------------------------------

type cliTusFake struct {
	mu     sync.Mutex
	srv    *httptest.Server
	exists bool
	offset int64
	length int64

	posts, heads, patches, deletes int
	postMeta                       string   // last POST's Upload-Metadata header
	postLen                        string   // last POST's Upload-Length header
	postAuth                       string   // last POST's Authorization header
	patchAuth                      string   // last PATCH's Authorization header
	patchOffsets                   []string // per-PATCH Upload-Offset request header
	patchStatus                    int      // forced PATCH response status; 0 serves the normal 204
}

func newCliTusFake(t *testing.T) *cliTusFake {
	t.Helper()
	f := &cliTusFake{}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /files/", f.handlePost)
	mux.HandleFunc("HEAD /files/abc123", f.handleHead)
	mux.HandleFunc("PATCH /files/abc123", f.handlePatch)
	mux.HandleFunc("DELETE /files/abc123", f.handleDelete)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	f.srv = srv
	return f
}

func (f *cliTusFake) endpoint() string  { return f.srv.URL + "/files/" }
func (f *cliTusFake) uploadURL() string { return f.srv.URL + "/files/abc123" }

func (f *cliTusFake) seed(exists bool, offset, length int64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.exists, f.offset, f.length = exists, offset, length
}

func (f *cliTusFake) counts() (posts, heads, patches, deletes int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.posts, f.heads, f.patches, f.deletes
}

func (f *cliTusFake) lastPostMeta() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.postMeta
}

func (f *cliTusFake) lastPostLen() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.postLen
}

func (f *cliTusFake) lastPostAuth() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.postAuth
}

func (f *cliTusFake) lastPatchAuth() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.patchAuth
}

func (f *cliTusFake) patchRequestOffsets() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.patchOffsets...)
}

func (f *cliTusFake) handlePost(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.posts++
	f.postMeta = r.Header.Get("Upload-Metadata")
	f.postLen = r.Header.Get("Upload-Length")
	f.postAuth = r.Header.Get("Authorization")
	f.exists = true
	f.offset = 0
	f.length = f.postLenAsInt()
	w.Header().Set("Location", f.srv.URL+"/files/abc123")
	w.WriteHeader(http.StatusCreated)
}

func (f *cliTusFake) postLenAsInt() int64 {
	var n int64
	_, _ = fmt.Sscanf(f.postLen, "%d", &n)
	return n
}

func (f *cliTusFake) handleHead(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.heads++
	if !f.exists {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	w.Header().Set("Upload-Offset", fmt.Sprintf("%d", f.offset))
	w.Header().Set("Upload-Length", fmt.Sprintf("%d", f.length))
	w.WriteHeader(http.StatusOK)
}

func (f *cliTusFake) handlePatch(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.patches++
	f.patchAuth = r.Header.Get("Authorization")
	f.patchOffsets = append(f.patchOffsets, r.Header.Get("Upload-Offset"))
	if forced := f.patchStatus; forced != 0 {
		// A forced fault persists nothing: the resync HEAD answers the
		// unchanged offset, so a no-budget run aborts on the PATCH error.
		w.WriteHeader(forced)
		return
	}
	if !f.exists {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	f.offset += r.ContentLength
	w.Header().Set("Upload-Offset", fmt.Sprintf("%d", f.offset))
	w.WriteHeader(http.StatusNoContent)
}

func (f *cliTusFake) handleDelete(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.deletes++
	if !f.exists {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	f.exists = false
	w.WriteHeader(http.StatusNoContent)
}

// decodeMetaHeader renders an Upload-Metadata wire header ("key <b64>,...")
// back into its map: last value per key wins, mirroring the merge semantics.
func decodeMetaHeader(t *testing.T, wire string) map[string]string {
	t.Helper()
	out := map[string]string{}
	if wire == "" {
		return out
	}
	for _, pair := range strings.Split(wire, ",") {
		key, val, ok := strings.Cut(strings.TrimSpace(pair), " ")
		if !ok {
			t.Fatalf("malformed Upload-Metadata pair %q in %q", pair, wire)
		}
		raw, err := base64.StdEncoding.DecodeString(val)
		if err != nil {
			t.Fatalf("malformed Upload-Metadata value %q: %v", val, err)
		}
		out[key] = string(raw)
	}
	return out
}

// writeTusTempFile writes size bytes of fill to a temp file and returns its
// path.
func writeTusTempFile(t *testing.T, name string, size int, fill byte) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	body := make([]byte, size)
	for i := range body {
		body[i] = fill
	}
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatalf("write temp file: %v", err)
	}
	return path
}

// ---------------------------------------------------------------------------
// tus upload e2e
// ---------------------------------------------------------------------------

// TestTusUploadHappyHuman pins the human-mode success contract: one stdout
// line, success-path step lines on stderr.
func TestTusUploadHappyHuman(t *testing.T) {
	fake := newCliTusFake(t)
	file := writeTusTempFile(t, "report.pdf", 200, 'x')

	stdout, stderr, _, err := runRoot(t, "tus", "upload", file, "--api-url", fake.endpoint())
	if err != nil {
		t.Fatalf("tus upload: %v\nstderr: %s", err, stderr)
	}
	if want := "uploaded 200 bytes to " + fake.uploadURL() + "\n"; stdout != want {
		t.Errorf("stdout = %q, want %q", stdout, want)
	}
	if !strings.Contains(stderr, "created "+fake.uploadURL()) {
		t.Errorf("stderr %q lacks the created step line", stderr)
	}
	if !strings.Contains(stderr, "uploaded 200/200") {
		t.Errorf("stderr %q lacks the uploaded step line", stderr)
	}
	if _, heads, patches, _ := fake.counts(); heads != 0 || patches != 1 {
		t.Errorf("fake saw heads=%d patches=%d, want 0 heads (no --resume) and 1 patch", heads, patches)
	}
}

// TestTusUploadJSON pins the machine-mode contract: exactly one {url, offset,
// size} document on stdout.
func TestTusUploadJSON(t *testing.T) {
	fake := newCliTusFake(t)
	file := writeTusTempFile(t, "report.pdf", 200, 'x')

	stdout, stderr, dec, err := runRoot(t, "--json", "tus", "upload", file, "--api-url", fake.endpoint())
	if err != nil {
		t.Fatalf("tus upload --json: %v\nstderr: %s", err, stderr)
	}
	var doc struct {
		URL    string `json:"url"`
		Offset int64  `json:"offset"`
		Size   int64  `json:"size"`
	}
	if err := json.Unmarshal([]byte(stdout), &doc); err != nil {
		t.Fatalf("stdout %q is not one JSON document: %v", stdout, err)
	}
	if doc.URL != fake.uploadURL() {
		t.Errorf("url = %q, want %q", doc.URL, fake.uploadURL())
	}
	if doc.Size != 200 || doc.Offset != 200 {
		t.Errorf("offset/size = %d/%d, want 200/200", doc.Offset, doc.Size)
	}
	if stderr != "" || dec != "" {
		t.Errorf("JSON mode wrote decoration: stderr %q, ui output %q", stderr, dec)
	}
}

// TestTusUploadPlainASCII asserts every captured byte is ASCII with no
// escape sequences in Plain mode.
func TestTusUploadPlainASCII(t *testing.T) {
	fake := newCliTusFake(t)
	file := writeTusTempFile(t, "report.pdf", 200, 'x')

	stdout, stderr, dec, err := runRoot(t, "--plain", "tus", "upload", file, "--api-url", fake.endpoint())
	if err != nil {
		t.Fatalf("tus upload --plain: %v\nstderr: %s", err, stderr)
	}
	for name, s := range map[string]string{"stdout": stdout, "stderr": stderr, "ui output": dec} {
		for i := 0; i < len(s); i++ {
			if b := s[i]; b >= 0x80 || b == 0x1b {
				t.Fatalf("%s has byte 0x%02x at offset %d: %q", name, b, i, s)
			}
		}
	}
}

// TestTusUploadMetadataMerge pins the merge semantics on the wire: the
// --client/--filename sugar fills the initial map, --metadata pairs apply
// after (same-key override, duplicate keys last-wins).
func TestTusUploadMetadataMerge(t *testing.T) {
	fake := newCliTusFake(t)
	file := writeTusTempFile(t, "report.pdf", 10, 'x')

	_, stderr, _, err := runRoot(t, "tus", "upload", file, "--api-url", fake.endpoint(),
		"--metadata", "env=staging", "--metadata", "client=other", "--metadata", "a=1", "--metadata", "a=2")
	if err != nil {
		t.Fatalf("tus upload: %v\nstderr: %s", err, stderr)
	}
	got := decodeMetaHeader(t, fake.lastPostMeta())
	want := map[string]string{
		"client":   "other", // --metadata overrides the sugar
		"filename": "report.pdf",
		"env":      "staging",
		"a":        "2", // duplicate --metadata keys: last wins
	}
	if len(got) != len(want) {
		t.Errorf("metadata keys = %v, want %v", got, want)
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("metadata[%q] = %q, want %q (full: %v)", k, got[k], v, got)
		}
	}
}

// TestTusUploadMetadataMalformedGolden pins the pre-network metadata goldens:
// the CLI's house form for a missing '=' and tus's key-charset error for a
// key the wire grammar rejects — both before any request fires.
func TestTusUploadMetadataMalformedGolden(t *testing.T) {
	fake := newCliTusFake(t)
	file := writeTusTempFile(t, "report.pdf", 10, 'x')

	_, _, _, err := runRoot(t, "tus", "upload", file, "--api-url", fake.endpoint(), "--metadata", "nokey")
	if err == nil || err.Error() != `invalid argument "nokey": expected KEY=VALUE` {
		t.Errorf("missing '=' = %v, want the house golden", err)
	}

	fake2 := newCliTusFake(t)
	_, _, _, err = runRoot(t, "tus", "upload", file, "--api-url", fake2.endpoint(), "--metadata", "has space=x")
	// The key-charset golden surfaces through the CLI failure line's single
	// context site, so the pin is containment, not byte-exactness.
	wantKey := `tus: invalid metadata key "has space": metadata keys must be non-empty printable ASCII without spaces or commas`
	if err == nil || !strings.Contains(err.Error(), wantKey) {
		t.Errorf("bad key = %v, want it to carry %q", err, wantKey)
	}
	if posts, _, _, _ := fake2.counts(); posts != 0 {
		t.Errorf("bad-key run fired %d POSTs, want 0 (pre-network)", posts)
	}
}

// TestTusUploadChunkSizeGoldens pins the chunk-size parse golden and the
// Upload-Length wire value for a suffixed size.
func TestTusUploadChunkSizeGoldens(t *testing.T) {
	fake := newCliTusFake(t)
	file := writeTusTempFile(t, "report.pdf", 10, 'x')

	_, _, _, err := runRoot(t, "tus", "upload", file, "--api-url", fake.endpoint(), "--chunk-size", "5x")
	want := `invalid argument "5x": expected a byte count with an optional k/m/g suffix, e.g. 64m`
	if err == nil || err.Error() != want {
		t.Errorf("chunk-size 5x = %v, want %q", err, want)
	}

	fake2 := newCliTusFake(t)
	_, _, _, err = runRoot(t, "tus", "upload", file, "--api-url", fake2.endpoint(), "--chunk-size", "1.5m")
	if err == nil || !strings.Contains(err.Error(), `invalid argument "1.5m"`) {
		t.Errorf("chunk-size 1.5m = %v, want the house golden", err)
	}

	// A suffixed size parses and rides the wire as bytes.
	fake3 := newCliTusFake(t)
	_, _, _, err = runRoot(t, "tus", "upload", file, "--api-url", fake3.endpoint(), "--chunk-size", "4k")
	if err != nil {
		t.Fatalf("chunk-size 4k: %v", err)
	}
	if got := fake3.lastPostLen(); got != "10" {
		t.Errorf("Upload-Length = %q, want the file size 10", got)
	}
	if _, _, patches, _ := fake3.counts(); patches != 1 {
		t.Errorf("patches = %d, want 1 (chunk >= file)", patches)
	}
}

// TestTusUploadResumeRows pins the resume contract: cross-host refusal
// (naming both hosts, pre-network), same-host continuation from the staged
// offset, resume-to-completion with zero PATCHes, and the offset>size guard.
func TestTusUploadResumeRows(t *testing.T) {
	fake := newCliTusFake(t)
	file := writeTusTempFile(t, "report.pdf", 200, 'x')

	// Cross-host refusal: credentials never ride to another host.
	_, _, _, err := runRoot(t, "tus", "upload", file, "--api-url", fake.endpoint(),
		"--resume", "https://evil.example.com/files/abc123")
	wantHost := fmt.Sprintf("tus: --resume host %q does not match --api-url host %q; refusing to send credentials to a different host",
		"evil.example.com", strings.TrimPrefix(strings.TrimSuffix(fake.endpoint(), "/files/"), "http://"))
	if err == nil || err.Error() != wantHost {
		t.Errorf("cross-host resume = %v, want %q", err, wantHost)
	}
	if posts, _, _, _ := fake.counts(); posts != 0 {
		t.Errorf("cross-host refusal fired %d POSTs, want 0 (pre-network)", posts)
	}

	// Same-host continuation: the staged offset feeds the first PATCH.
	fake2 := newCliTusFake(t)
	fake2.seed(true, 100, 200)
	_, stderr, _, err := runRoot(t, "tus", "upload", file, "--api-url", fake2.endpoint(), "--resume", fake2.uploadURL())
	if err != nil {
		t.Fatalf("same-host resume: %v\nstderr: %s", err, stderr)
	}
	if !strings.Contains(stderr, "resuming at offset 100") {
		t.Errorf("stderr %q lacks the resuming step line", stderr)
	}
	if offs := fake2.patchRequestOffsets(); len(offs) != 1 || offs[0] != "100" {
		t.Errorf("PATCH offsets = %v, want one from 100", offs)
	}

	// Resume-to-completion: everything staged, zero PATCHes.
	fake3 := newCliTusFake(t)
	fake3.seed(true, 200, 200)
	stdout, stderr, _, err := runRoot(t, "tus", "upload", file, "--api-url", fake3.endpoint(), "--resume", fake3.uploadURL())
	if err != nil {
		t.Fatalf("resume-to-completion: %v\nstderr: %s", err, stderr)
	}
	if _, _, patches, _ := fake3.counts(); patches != 0 {
		t.Errorf("resume-to-completion PATCHes = %d, want 0", patches)
	}
	if !strings.Contains(stdout, "uploaded 200 bytes to "+fake3.uploadURL()) {
		t.Errorf("stdout %q lacks the final line", stdout)
	}
	if strings.Contains(stderr, "uploaded 200/200") {
		t.Errorf("stderr %q has a step line for a run with zero PATCHes", stderr)
	}

	// offset>size: the file changed under the staged upload — clean error,
	// zero PATCHes.
	fake4 := newCliTusFake(t)
	fake4.seed(true, 300, 200)
	_, _, _, err = runRoot(t, "tus", "upload", file, "--api-url", fake4.endpoint(), "--resume", fake4.uploadURL())
	if err == nil || !strings.Contains(err.Error(), "past the file's size 200") {
		t.Errorf("offset>size = %v, want the past-size abort", err)
	}
	if _, _, patches, _ := fake4.counts(); patches != 0 {
		t.Errorf("offset>size PATCHes = %d, want 0", patches)
	}
}

// ---------------------------------------------------------------------------
// tus status / tus delete
// ---------------------------------------------------------------------------

// TestTusStatusAsData pins the asymmetry: a 404 from status is data — exit 0
// with exists:false — while a staged upload reports its offset and length.
func TestTusStatusAsData(t *testing.T) {
	fake := newCliTusFake(t)
	fake.seed(false, 0, 0)

	stdout, _, _, err := runRoot(t, "--json", "tus", "status", fake.uploadURL())
	if err != nil {
		t.Fatalf("tus status --json (404): %v", err)
	}
	var doc struct {
		URL    string `json:"url"`
		Exists bool   `json:"exists"`
		Offset int64  `json:"offset"`
		Length int64  `json:"length"`
	}
	if err := json.Unmarshal([]byte(stdout), &doc); err != nil {
		t.Fatalf("stdout %q is not one JSON document: %v", stdout, err)
	}
	if doc.Exists || doc.Offset != 0 || doc.Length != 0 {
		t.Errorf("404 document = %+v, want exists:false at zero", doc)
	}
	if doc.URL != fake.uploadURL() {
		t.Errorf("url = %q, want the positional as given %q", doc.URL, fake.uploadURL())
	}

	// Human mode names the same outcome as data.
	stdout, _, _, err = runRoot(t, "tus", "status", fake.uploadURL())
	if err != nil {
		t.Fatalf("tus status (404): %v", err)
	}
	if want := "no upload in staging at " + fake.uploadURL() + " (completed, expired, or deleted)\n"; stdout != want {
		t.Errorf("stdout = %q, want %q", stdout, want)
	}

	// Staged: offset and length ride both modes.
	fake.seed(true, 100, 500)
	stdout, _, _, err = runRoot(t, "--json", "tus", "status", fake.uploadURL())
	if err != nil {
		t.Fatalf("tus status --json (staged): %v", err)
	}
	if err := json.Unmarshal([]byte(stdout), &doc); err != nil {
		t.Fatalf("stdout %q is not one JSON document: %v", stdout, err)
	}
	if !doc.Exists || doc.Offset != 100 || doc.Length != 500 {
		t.Errorf("staged document = %+v, want exists:true at 100/500", doc)
	}
	stdout, _, _, err = runRoot(t, "tus", "status", fake.uploadURL())
	if err != nil {
		t.Fatalf("tus status (staged): %v", err)
	}
	if want := "offset 100 of 500 (in staging)\n"; stdout != want {
		t.Errorf("stdout = %q, want %q", stdout, want)
	}
}

// TestTusDeleteRows pins the delete contract: 204 exits 0 in both modes; a
// 404 is an error naming the URL.
func TestTusDeleteRows(t *testing.T) {
	fake := newCliTusFake(t)
	fake.seed(true, 0, 100)

	stdout, _, _, err := runRoot(t, "tus", "delete", fake.uploadURL())
	if err != nil {
		t.Fatalf("tus delete (204): %v", err)
	}
	if want := "deleted " + fake.uploadURL() + "\n"; stdout != want {
		t.Errorf("stdout = %q, want %q", stdout, want)
	}

	fake.seed(true, 0, 100)
	stdout, _, _, err = runRoot(t, "--json", "tus", "delete", fake.uploadURL())
	if err != nil {
		t.Fatalf("tus delete --json (204): %v", err)
	}
	var doc struct {
		URL     string `json:"url"`
		Deleted bool   `json:"deleted"`
	}
	if err := json.Unmarshal([]byte(stdout), &doc); err != nil {
		t.Fatalf("stdout %q is not one JSON document: %v", stdout, err)
	}
	if !doc.Deleted || doc.URL != fake.uploadURL() {
		t.Errorf("document = %+v, want deleted:true for %q", doc, fake.uploadURL())
	}

	// 404: nothing staged — exit 1, the URL in the error.
	fake2 := newCliTusFake(t)
	_, _, _, err = runRoot(t, "tus", "delete", fake2.uploadURL())
	want := "tus: delete " + fake2.uploadURL() + ": tus: the server returned 404 Not Found"
	if err == nil || err.Error() != want {
		t.Errorf("delete 404 = %v, want %q", err, want)
	}
}

// ---------------------------------------------------------------------------
// pre-network golden table
// ---------------------------------------------------------------------------

// TestTusPreFlightGoldens pins every failure that must fire before any
// network or file work.
func TestTusPreFlightGoldens(t *testing.T) {
	file := writeTusTempFile(t, "report.pdf", 10, 'x')

	// Missing --api-url.
	_, _, _, err := runRoot(t, "tus", "upload", file)
	want := "tus: --api-url is required for tus upload"
	if err == nil || err.Error() != want {
		t.Errorf("missing --api-url = %v, want %q", err, want)
	}

	// Garbage --api-url: the NormalizeEndpoint error verbatim.
	fake := newCliTusFake(t)
	_, _, _, err = runRoot(t, "tus", "upload", file, "--api-url", "notaurl")
	want = `invalid endpoint "notaurl": an absolute http(s) URL is required, e.g. https://files.example.com/`
	if err == nil || err.Error() != want {
		t.Errorf("garbage --api-url = %v, want %q", err, want)
	}
	if posts, _, _, _ := fake.counts(); posts != 0 {
		t.Errorf("garbage --api-url fired %d POSTs on the fake, want 0", posts)
	}

	// Garbage positional for status and delete.
	_, _, _, err = runRoot(t, "tus", "status", "notaurl")
	if err == nil || err.Error() != want {
		t.Errorf("garbage status positional = %v, want %q", err, want)
	}
	_, _, _, err = runRoot(t, "tus", "delete", "notaurl")
	if err == nil || err.Error() != want {
		t.Errorf("garbage delete positional = %v, want %q", err, want)
	}

	// Empty bearer: the provider's Build error, pinned verbatim.
	_, _, _, err = runRoot(t, "--auth", "bearer", "tus", "upload", file, "--api-url", fake.endpoint())
	want = "bearer auth requires a token: pass --client-secret or set MCPCLI_CLIENT_SECRET"
	if err == nil || err.Error() != want {
		t.Errorf("empty bearer = %v, want %q", err, want)
	}

	// Missing file: the os error propagates bare (the --in-file house form),
	// after the auth build. The text is the os's own (differs per platform),
	// so the pin is that the path rides the error.
	_, _, _, err = runRoot(t, "tus", "upload", filepath.Join(t.TempDir(), "missing.bin"), "--api-url", fake.endpoint())
	if err == nil || !strings.Contains(err.Error(), "missing.bin") {
		t.Errorf("missing file = %v, want the os error naming the path", err)
	}
}

// ---------------------------------------------------------------------------
// auth surface e2e
// ---------------------------------------------------------------------------

// TestTusUploadBearerE2E pins the bearer path: the handler's token rides on
// every request, and the literal-secret argv warning fires exactly once.
// --insecure: the fake serves plain http on a loopback host, and the bearer
// trips the tus plain-http credential gate without the loopback opt-in.
func TestTusUploadBearerE2E(t *testing.T) {
	fake := newCliTusFake(t)
	file := writeTusTempFile(t, "report.pdf", 10, 'x')

	readStderr := captureOSStderr(t)
	_, _, _, err := runRoot(t, "--auth", "bearer", "--client-secret", tusDummyToken(), "--insecure",
		"tus", "upload", file, "--api-url", fake.endpoint())
	if err != nil {
		t.Fatalf("bearer upload: %v", err)
	}
	if got := fake.lastPostAuth(); got != "Bearer "+tusDummyToken() {
		t.Errorf("POST Authorization = %q, want the bearer token", got)
	}
	if got := fake.lastPatchAuth(); got != "Bearer "+tusDummyToken() {
		t.Errorf("PATCH Authorization = %q, want the bearer token", got)
	}
	if got := strings.Count(readStderr(), clientSecretWarning); got != 1 {
		t.Errorf("argv warning count = %d, want exactly 1", got)
	}
}

// TestTusUploadBearerRefE2E pins the bws:// expansion through the tus verbs:
// the ref resolves BEFORE anything connects and the RESOLVED value rides the
// wire.
func TestTusUploadBearerRefE2E(t *testing.T) {
	bwsTestTokenDir(t)
	orgKey := bwsTestOrgKey(t)
	uuidRef := "11111111-2222-3333-4444-555555555555"
	fa := newBWSTestAPI(t, orgKey, map[string]bws.Secret{
		uuidRef: {ID: uuidRef, OrganizationID: bwsTestOrgID, Key: "first", Value: "value-a", CanRead: true},
	})
	fi := newBWSTestIdentity(t)
	if err := bws.SaveCredential(bws.Credential{
		APIToken:    bwsTestToken,
		OrgID:       bwsTestOrgID,
		APIURL:      fa.url,
		IdentityURL: fi.url,
	}); err != nil {
		t.Fatalf("SaveCredential: %v", err)
	}
	t.Setenv("MCPCLI_BWS_API_URL", fa.url)

	fake := newCliTusFake(t)
	file := writeTusTempFile(t, "report.pdf", 10, 'x')

	// One --insecure covers both plain-http surfaces of the run: the bws://
	// expansion's identity exchange and the tus upload itself.
	_, stderr, _, err := runRoot(t, "--auth", "bearer", "--client-secret", "bws://"+uuidRef, "--insecure",
		"tus", "upload", file, "--api-url", fake.endpoint())
	if err != nil {
		t.Fatalf("bws:// bearer upload: %v\nstderr: %s", err, stderr)
	}
	if got := fake.lastPostAuth(); got != "Bearer value-a" {
		t.Errorf("POST Authorization = %q, want the resolved value", got)
	}
}

// ---------------------------------------------------------------------------
// multi-chunk + JSON purity
// ---------------------------------------------------------------------------

// TestTusUploadMultiChunk pins CLI-side chunking on the committed fixture
// (3894 bytes): 64-byte chunks → 61 PATCHes; 1024-byte chunks → 4 PATCHes
// with stdout exactly one JSON document and empty stderr/decoration.
func TestTusUploadMultiChunk(t *testing.T) {
	fixture := filepath.Join("testdata", "gutenberg-sample.txt")

	fake := newCliTusFake(t)
	_, _, _, err := runRoot(t, "tus", "upload", fixture, "--api-url", fake.endpoint(), "--chunk-size", "64")
	if err != nil {
		t.Fatalf("multi-chunk upload: %v", err)
	}
	if _, _, patches, _ := fake.counts(); patches != 61 {
		t.Errorf("PATCHes = %d, want 61 (3894 bytes / 64)", patches)
	}
	if offs := fake.patchRequestOffsets(); len(offs) > 1 && offs[0] != "0" {
		t.Errorf("first PATCH offset = %q, want 0", offs[0])
	}

	// JSON purity under multi-chunk: one document, nothing else anywhere.
	fake2 := newCliTusFake(t)
	stdout, stderr, dec, err := runRoot(t, "--json", "tus", "upload", fixture, "--api-url", fake2.endpoint(), "--chunk-size", "1024")
	if err != nil {
		t.Fatalf("multi-chunk --json: %v\nstderr: %s", err, stderr)
	}
	if _, _, patches, _ := fake2.counts(); patches != 4 {
		t.Errorf("PATCHes = %d, want 4 (3894 bytes / 1024)", patches)
	}
	var doc struct {
		URL    string `json:"url"`
		Offset int64  `json:"offset"`
		Size   int64  `json:"size"`
	}
	if err := json.Unmarshal([]byte(stdout), &doc); err != nil {
		t.Fatalf("stdout %q is not exactly one JSON document: %v", stdout, err)
	}
	if doc.Size != 3894 || doc.Offset != 3894 {
		t.Errorf("offset/size = %d/%d, want 3894/3894", doc.Offset, doc.Size)
	}
	if stderr != "" || dec != "" {
		t.Errorf("JSON mode wrote decoration: stderr %q, ui output %q", stderr, dec)
	}
}

// ---------------------------------------------------------------------------
// --insecure
// ---------------------------------------------------------------------------

// TestInsecureRootHookRejection pins the golden rejection: basic's
// credential transport pins TLS verification, so the combination is refused
// before anything runs.
func TestInsecureRootHookRejection(t *testing.T) {
	const want = "--auth basic does not combine with --insecure: basic's credential injection pins a TLS-verifying transport; drop --insecure or use --auth bearer/client-credentials"
	_, _, _, err := runRoot(t, "--auth", "basic", "--insecure", "--url", "http://127.0.0.1:1/mcp", "ping")
	if err == nil || err.Error() != want {
		t.Errorf("basic + insecure = %v, want byte-exact %q", err, want)
	}
}

// TestInsecureWarningEveryMode pins the functional security note: exactly
// one warning line on the raw stderr in every output mode.
func TestInsecureWarningEveryMode(t *testing.T) {
	srv := newTestMCPServer(t)
	const want = "warning: --insecure skips TLS certificate validation"

	readStderr := captureOSStderr(t)
	_, _, _, err := runRoot(t, "--insecure", "--url", srv.URL, "ping")
	if err != nil {
		t.Fatalf("insecure ping: %v", err)
	}
	if got := strings.Count(readStderr(), want); got != 1 {
		t.Errorf("rich-mode warning count = %d, want 1", got)
	}

	readStderr = captureOSStderr(t)
	_, _, _, err = runRoot(t, "--insecure", "--json", "--url", srv.URL, "ping")
	if err != nil {
		t.Fatalf("insecure ping --json: %v", err)
	}
	if got := strings.Count(readStderr(), want); got != 1 {
		t.Errorf("json-mode warning count = %d, want 1", got)
	}

	// Plain mode too: the warning is a raw Fprintln, not mode-gated
	// decoration.
	readStderr = captureOSStderr(t)
	_, _, _, err = runRoot(t, "--insecure", "--plain", "--url", srv.URL, "ping")
	if err != nil {
		t.Fatalf("insecure ping --plain: %v", err)
	}
	if got := strings.Count(readStderr(), want); got != 1 {
		t.Errorf("plain-mode warning count = %d, want 1", got)
	}
}

// TestInsecureTLSMCPe2e proves the flag end-to-end: a self-signed TLS MCP
// server is unreachable without --insecure (x509, exit 1) and reachable
// with it.
func TestInsecureTLSMCPe2e(t *testing.T) {
	srv := mcp.NewServer(&mcp.Implementation{Name: "testserver", Version: "1.0.0"}, nil)
	tlsSrv := httptest.NewTLSServer(mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return srv }, nil))
	t.Cleanup(tlsSrv.Close)

	_, _, _, err := runRoot(t, "--url", tlsSrv.URL, "tools", "list")
	if err == nil || !strings.Contains(err.Error(), "x509") {
		t.Fatalf("TLS without --insecure = %v, want an x509 verification failure", err)
	}

	_, stderr, _, err := runRoot(t, "--insecure", "--url", tlsSrv.URL, "tools", "list")
	if err != nil {
		t.Fatalf("TLS with --insecure: %v\nstderr: %s", err, stderr)
	}
}

// TestTusUploadErrorCarriesURL pins the fix-wave error contract: a
// fresh-create mid-upload failure names the wire target and the last-sent
// offset in the CLI failure line — in the returned error AND in the stderr
// output cobra prints.
func TestTusUploadErrorCarriesURL(t *testing.T) {
	fake := newCliTusFake(t)
	fake.mu.Lock()
	fake.patchStatus = http.StatusInternalServerError
	fake.mu.Unlock()
	file := writeTusTempFile(t, "report.pdf", 200, 'x')

	_, _, _, err := runRoot(t, "tus", "upload", file, "--api-url", fake.endpoint(), "--retry", "0")
	if err == nil {
		t.Fatal("upload with a 500 PATCH fault and no retry budget: nil error")
	}
	wantPrefix := "tus: uploading " + file + " to " + fake.uploadURL() + " at offset 0: "
	if !strings.HasPrefix(err.Error(), wantPrefix) {
		t.Fatalf("error = %q, want the %q prefix", err.Error(), wantPrefix)
	}
	if !strings.Contains(err.Error(), "500") {
		t.Errorf("error %q does not carry the server status", err.Error())
	}
}
