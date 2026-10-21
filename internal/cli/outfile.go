package cli

import (
	"fmt"
	"io"
	"mime"
	"os"
	"path/filepath"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// media is one divertable content block — decoded bytes plus their MIME
// type — the common shape of image/audio tool content and resource blobs.
type media struct {
	mime string
	data []byte
}

// toolMedia collects the image and audio blocks of a tool result in content
// order. The SDK keeps Data as the decoded bytes ([]byte marshals as base64
// on the wire and is already decoded in memory), so no base64 step is needed.
func toolMedia(res *mcp.CallToolResult) []media {
	if res == nil {
		return nil
	}
	medias := make([]media, 0, len(res.Content))
	for _, c := range res.Content {
		// Image and audio need separate cases: a combined case would bind
		// the mcp.Content interface, which has no Data field.
		switch c := c.(type) {
		case *mcp.ImageContent:
			medias = append(medias, media{mime: c.MIMEType, data: c.Data})
		case *mcp.AudioContent:
			medias = append(medias, media{mime: c.MIMEType, data: c.Data})
		}
	}
	return medias
}

// resourceMedia collects the blob entries of a resource read result in
// content order, with their (already decoded) bytes and MIME types.
func resourceMedia(res *mcp.ReadResourceResult) []media {
	if res == nil {
		return nil
	}
	medias := make([]media, 0, len(res.Contents))
	for _, c := range res.Contents {
		if c == nil {
			continue
		}
		if len(c.Blob) > 0 {
			medias = append(medias, media{mime: c.MIMEType, data: c.Blob})
		}
	}
	return medias
}

// extForMIME maps a MIME type to a file extension via the stdlib registry's
// first hit, with .bin as the fallback for empty, unknown, or malformed
// results. The guard — "." plus 1–8 ASCII alphanumerics — closes
// Windows-registry oddities and NTFS ":" alternate-data-stream paths, since
// the MIME string is server-controlled.
func extForMIME(m string) string {
	// lookup error ignored on purpose: the .bin fallback below covers it
	exts, _ := mime.ExtensionsByType(m)
	if len(exts) == 0 {
		return ".bin"
	}
	ext := exts[0]
	if len(ext) < 2 || len(ext) > 9 || ext[0] != '.' {
		return ".bin"
	}
	for i := 1; i < len(ext); i++ {
		c := ext[i]
		isAlnum := '0' <= c && c <= '9' || 'a' <= c && c <= 'z' || 'A' <= c && c <= 'Z'
		if !isAlnum {
			return ".bin"
		}
	}
	return ext
}

// writeMediaFiles saves each media block under base with its MIME-derived
// extension, replacing any user-supplied one: a single block becomes
// base.ext, multiple blocks become base.1.ext, base.2.ext, … Each success
// prints "wrote <path> (N bytes)" to errOut in every output mode (the
// --client-secret warning precedent). Files are overwritten silently (curl
// -o precedent); a failed multi-file write keeps the already-written partials
// and wraps the error with the base path. No ctx parameter: there is no
// ctx-aware file IO to honor.
func writeMediaFiles(base string, medias []media, errOut io.Writer) error {
	// Drop a user-supplied extension so the MIME-derived one wins, trimming
	// only the final path element: a dotfile's whole name looks like an
	// extension (filepath.Ext(".hidden") is ".hidden"), so the empty-stem
	// guard keeps it whole — --outfile .hidden becomes .hidden.png, whether
	// the base is bare or under a directory.
	dir, file := filepath.Split(base)
	if stem := strings.TrimSuffix(file, filepath.Ext(file)); stem != "" {
		base = filepath.Join(dir, stem)
	}
	for i, m := range medias {
		ext := extForMIME(m.mime)
		path := base + ext
		if len(medias) > 1 {
			path = fmt.Sprintf("%s.%d%s", base, i+1, ext)
		}
		if err := os.WriteFile(path, m.data, 0o644); err != nil {
			return fmt.Errorf("--outfile %q: %w", base, err)
		}
		fmt.Fprintf(errOut, "wrote %s (%d bytes)\n", path, len(m.data))
	}
	return nil
}
