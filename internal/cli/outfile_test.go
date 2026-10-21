package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestExtForMIME pins the extension mapping: the stdlib registry's first hit
// for known types, .bin for empty, unparseable, and unknown types.
func TestExtForMIME(t *testing.T) {
	tests := []struct {
		name string
		mime string
		want string
	}{
		{"png", "image/png", ".png"},
		{"empty", "", ".bin"},
		{"garbage", "garbage", ".bin"},
		{"unknown", "image/x-really-unknown", ".bin"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := extForMIME(tt.mime); got != tt.want {
				t.Errorf("extForMIME(%q) = %q, want %q", tt.mime, got, tt.want)
			}
		})
	}
}

// TestWriteMediaFiles covers the file naming and notice contract: a single
// media block gets the bare name, multiple blocks get numbered names, a
// user-supplied extension is replaced by the MIME-derived one, dotfiles keep
// their dot, every success notes the path and byte count on errOut, and a
// base under a missing directory errors naming --outfile.
func TestWriteMediaFiles(t *testing.T) {
	tests := []struct {
		name   string
		file   string // base name written under the test's temp dir
		medias []media
		check  func(t *testing.T, tmp, errOut string)
	}{
		{
			name:   "single media",
			file:   "img",
			medias: []media{{mime: "image/png", data: []byte("PNGDATA")}},
			check: func(t *testing.T, tmp, errOut string) {
				got, err := os.ReadFile(filepath.Join(tmp, "img.png"))
				if err != nil {
					t.Fatalf("img.png: %v", err)
				}
				if string(got) != "PNGDATA" {
					t.Errorf("img.png = %q, want PNGDATA", got)
				}
				if !strings.Contains(errOut, "wrote "+filepath.Join(tmp, "img.png")+" (7 bytes)") {
					t.Errorf("errOut %q lacks the wrote notice", errOut)
				}
			},
		},
		{
			name: "multiple media numbered",
			file: "img",
			medias: []media{
				{mime: "image/png", data: []byte("PNGDATA1")},
				{mime: "image/png", data: []byte("PNGDATA2")},
			},
			check: func(t *testing.T, tmp, errOut string) {
				for name, want := range map[string]string{
					"img.1.png": "PNGDATA1",
					"img.2.png": "PNGDATA2",
				} {
					got, err := os.ReadFile(filepath.Join(tmp, name))
					if err != nil {
						t.Fatalf("%s: %v", name, err)
					}
					if string(got) != want {
						t.Errorf("%s = %q, want %q", name, got, want)
					}
					if !strings.Contains(errOut, "wrote "+filepath.Join(tmp, name)) {
						t.Errorf("errOut %q lacks the wrote notice for %s", errOut, name)
					}
				}
			},
		},
		{
			name:   "user extension replaced",
			file:   "shot.jpg",
			medias: []media{{mime: "image/png", data: []byte("PNGDATA")}},
			check: func(t *testing.T, tmp, errOut string) {
				got, err := os.ReadFile(filepath.Join(tmp, "shot.png"))
				if err != nil {
					t.Fatalf("shot.png: %v", err)
				}
				if string(got) != "PNGDATA" {
					t.Errorf("shot.png = %q, want PNGDATA", got)
				}
			},
		},
		{
			name:   "dotfile base keeps dot",
			file:   ".hidden",
			medias: []media{{mime: "image/png", data: []byte("PNGDATA")}},
			check: func(t *testing.T, tmp, errOut string) {
				got, err := os.ReadFile(filepath.Join(tmp, ".hidden.png"))
				if err != nil {
					t.Fatalf(".hidden.png: %v", err)
				}
				if string(got) != "PNGDATA" {
					t.Errorf(".hidden.png = %q, want PNGDATA", got)
				}
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tmp := t.TempDir()
			var errOut bytes.Buffer
			if err := writeMediaFiles(filepath.Join(tmp, tt.file), tt.medias, &errOut); err != nil {
				t.Fatalf("writeMediaFiles: %v", err)
			}
			tt.check(t, tmp, errOut.String())
		})
	}

	t.Run("missing directory errors", func(t *testing.T) {
		base := filepath.Join(t.TempDir(), "no-such-dir", "img")
		var errOut bytes.Buffer
		err := writeMediaFiles(base, []media{{mime: "image/png", data: []byte("PNGDATA")}}, &errOut)
		if err == nil || !strings.Contains(err.Error(), "--outfile") {
			t.Fatalf("writeMediaFiles under a missing directory = %v, want an error naming --outfile", err)
		}
	})
}
