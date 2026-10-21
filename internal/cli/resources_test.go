package cli

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// addLogResource registers a static text resource plus one resource template,
// so list tests see both shapes and the read test has something to fetch.
func addLogResource(s *mcp.Server) {
	s.AddResource(&mcp.Resource{
		URI:         "file:///logs/app.log",
		Name:        "app-log",
		Description: "The application log",
		MIMEType:    "text/plain",
	}, func(ctx context.Context, req *mcp.ReadResourceRequest) (*mcp.ReadResourceResult, error) {
		return &mcp.ReadResourceResult{
			Contents: []*mcp.ResourceContents{{
				URI:      req.Params.URI,
				MIMEType: "text/plain",
				Text:     "line one\nline two",
			}},
		}, nil
	})
	s.AddResourceTemplate(&mcp.ResourceTemplate{
		URITemplate: "file:///items/{id}",
		Name:        "item",
		Description: "An item by id",
	}, func(ctx context.Context, req *mcp.ReadResourceRequest) (*mcp.ReadResourceResult, error) {
		return &mcp.ReadResourceResult{
			Contents: []*mcp.ResourceContents{{URI: req.Params.URI, MIMEType: "text/plain", Text: "item"}},
		}, nil
	})
}

// TestResourcesListE2E: a server with one resource and one template lists
// both in the human table.
func TestResourcesListE2E(t *testing.T) {
	srv := newTestMCPServerWithTools(t, addLogResource)

	stdout, _, _, err := runRoot(t, "--url", srv.URL, "resources", "list")
	if err != nil {
		t.Fatalf("resources list: %v", err)
	}
	for _, want := range []string{
		"file:///logs/app.log", "app-log", "The application log",
		"file:///items/{id}", "item", "An item by id",
	} {
		if !strings.Contains(stdout, want) {
			t.Errorf("resources list stdout %q lacks %q", stdout, want)
		}
	}
}

// TestResourcesListJSON pins the --json shape of "resources list": one
// document holding both list results under "resources" and "templates".
func TestResourcesListJSON(t *testing.T) {
	srv := newTestMCPServerWithTools(t, addLogResource)

	stdout, stderr, dec, err := runRoot(t, "--url", srv.URL, "--json", "resources", "list")
	if err != nil {
		t.Fatalf("resources list --json: %v", err)
	}
	var doc struct {
		Resources struct {
			Resources []struct {
				URI  string `json:"uri"`
				Name string `json:"name"`
			} `json:"resources"`
		} `json:"resources"`
		Templates struct {
			ResourceTemplates []struct {
				URITemplate string `json:"uriTemplate"`
				Name        string `json:"name"`
			} `json:"resourceTemplates"`
		} `json:"templates"`
	}
	if err := json.Unmarshal([]byte(stdout), &doc); err != nil {
		t.Fatalf("resources list --json stdout %q is not JSON: %v", stdout, err)
	}
	if len(doc.Resources.Resources) != 1 || doc.Resources.Resources[0].URI != "file:///logs/app.log" || doc.Resources.Resources[0].Name != "app-log" {
		t.Errorf("resources list --json resources = %+v, want the app-log resource", doc.Resources.Resources)
	}
	if len(doc.Templates.ResourceTemplates) != 1 || doc.Templates.ResourceTemplates[0].URITemplate != "file:///items/{id}" || doc.Templates.ResourceTemplates[0].Name != "item" {
		t.Errorf("resources list --json templates = %+v, want the item template", doc.Templates.ResourceTemplates)
	}
	if stderr != "" || dec != "" {
		t.Errorf("resources list --json wrote decoration: stderr %q, ui output %q", stderr, dec)
	}
}

// TestResourcesReadE2E: reading a text resource prints its content raw to
// stdout — no table, no JSON encoding.
func TestResourcesReadE2E(t *testing.T) {
	srv := newTestMCPServerWithTools(t, addLogResource)

	stdout, _, _, err := runRoot(t, "--url", srv.URL, "resources", "read", "file:///logs/app.log")
	if err != nil {
		t.Fatalf("resources read: %v", err)
	}
	if !strings.Contains(stdout, "line one\nline two") {
		t.Errorf("resources read stdout %q does not carry the resource text verbatim", stdout)
	}
}

// addImageBlobResource registers a resource whose read result has two
// contents: a text entry and a gif blob entry, so --outfile tests can assert
// that text still prints while the blob diverts to a file.
func addImageBlobResource(s *mcp.Server) {
	s.AddResource(&mcp.Resource{
		URI:         "mem://img",
		Name:        "img",
		Description: "A text about an image plus its bytes",
	}, func(ctx context.Context, req *mcp.ReadResourceRequest) (*mcp.ReadResourceResult, error) {
		return &mcp.ReadResourceResult{Contents: []*mcp.ResourceContents{
			{URI: req.Params.URI, MIMEType: "text/plain", Text: "about the image"},
			{URI: req.Params.URI, MIMEType: "image/gif", Blob: []byte("GIFDATA")},
		}}, nil
	})
}

// TestResourcesReadOutfile: with --outfile the blob entry diverts to a
// MIME-derived file while the text entry still prints on stdout.
func TestResourcesReadOutfile(t *testing.T) {
	srv := newTestMCPServerWithTools(t, addImageBlobResource)
	tmp := t.TempDir()
	base := filepath.Join(tmp, "res")

	stdout, stderr, _, err := runRoot(t, "--url", srv.URL, "resources", "read", "mem://img", "--outfile", base)
	if err != nil {
		t.Fatalf("resources read --outfile: %v", err)
	}
	got, err := os.ReadFile(base + ".gif")
	if err != nil {
		t.Fatalf("read res.gif: %v", err)
	}
	if string(got) != "GIFDATA" {
		t.Errorf("res.gif = %q, want GIFDATA", got)
	}
	if !strings.Contains(stdout, "about the image") {
		t.Errorf("stdout %q does not still print the text entry", stdout)
	}
	if strings.Contains(stdout, "GIFDATA") {
		t.Errorf("stdout %q leaks the blob bytes; they belong in the file", stdout)
	}
	if !strings.Contains(stderr, "wrote") {
		t.Errorf("stderr %q lacks the wrote notice", stderr)
	}
}

// TestResourcesReadOutfileText: a text-only resource with --outfile is an
// error (exit 1) with nothing printed to stdout and no file written.
func TestResourcesReadOutfileText(t *testing.T) {
	srv := newTestMCPServerWithTools(t, addLogResource)
	tmp := t.TempDir()

	stdout, _, _, err := runRoot(t, "--url", srv.URL, "resources", "read", "file:///logs/app.log", "--outfile", filepath.Join(tmp, "res"))
	if err == nil || !strings.Contains(err.Error(), "no binary content") {
		t.Fatalf("text-only read with --outfile = %v, want the no-binary-content error", err)
	}
	if stdout != "" {
		t.Errorf("stdout = %q, want empty: the error must precede any printing", stdout)
	}
	entries, err := os.ReadDir(tmp)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Errorf("text-only read created files: %v", entries)
	}
}
