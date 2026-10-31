package cli

import (
	"bytes"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestParseInFiles(t *testing.T) {
	jevVerifyKeys := map[string]struct{}{"claim": {}, "evidence": {}}
	cases := []struct {
		name      string
		vals      []string
		validKeys map[string]struct{}
		allowBare bool
		want      []inFileSpec
		wantErr   string
	}{
		{
			name:      "bare path is the primary text",
			vals:      []string{"patch.diff"},
			allowBare: true,
			want:      []inFileSpec{{Path: "patch.diff"}},
		},
		{
			name:    "bare path rejected without a primary slot",
			vals:    []string{"patch.diff"},
			wantErr: "no primary text input",
		},
		{
			name:      "second bare path rejected",
			vals:      []string{"a.txt", "b.txt"},
			allowBare: true,
			wantErr:   "may only be given once",
		},
		{
			name:      "keyed path",
			vals:      []string{"evidence=ci.log"},
			validKeys: jevVerifyKeys,
			want:      []inFileSpec{{Key: "evidence", Path: "ci.log"}},
		},
		{
			name:      "keyed path splits on first equals like --arg",
			vals:      []string{"a=b=c.txt"},
			validKeys: nil,
			want:      []inFileSpec{{Key: "a", Path: "b=c.txt"}},
		},
		{
			name:      "unknown key names the valid set",
			vals:      []string{"purpose=x.txt"},
			validKeys: jevVerifyKeys,
			wantErr:   `unknown key "purpose" (valid: claim=, evidence=)`,
		},
		{
			name:    "empty key rejected",
			vals:    []string{"=path.txt"},
			wantErr: "expected [KEY=]PATH",
		},
		{
			name:      "empty path rejected",
			vals:      []string{"evidence="},
			validKeys: jevVerifyKeys,
			wantErr:   "expected [KEY=]PATH",
		},
		{
			name:    "empty value rejected",
			vals:    []string{""},
			wantErr: "expected [KEY=]PATH",
		},
		{
			name:      "bare and keyed mix",
			vals:      []string{"q.txt", "context=bg.txt"},
			allowBare: true,
			want:      []inFileSpec{{Path: "q.txt"}, {Key: "context", Path: "bg.txt"}},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			specs, err := parseInFiles(tc.vals, tc.validKeys, tc.allowBare)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("parseInFiles(%q) err = %v, want containing %q", tc.vals, err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseInFiles(%q) unexpected err: %v", tc.vals, err)
			}
			if len(specs) != len(tc.want) {
				t.Fatalf("specs = %+v, want %+v", specs, tc.want)
			}
			for i := range specs {
				if specs[i] != tc.want[i] {
					t.Fatalf("spec[%d] = %+v, want %+v", i, specs[i], tc.want[i])
				}
			}
		})
	}
}

func TestApplyInFiles(t *testing.T) {
	fake := func(path string) ([]byte, error) {
		if path == "boom" {
			return nil, fs.ErrNotExist
		}
		return []byte("content of " + path), nil
	}
	t.Run("primary and keyed values in spec order", func(t *testing.T) {
		var out bytes.Buffer
		specs := []inFileSpec{{Path: "q.txt"}, {Key: "evidence", Path: "a.log"}, {Key: "evidence", Path: "b.log"}}
		primary, values, err := applyInFiles(specs, fake, &out)
		if err != nil {
			t.Fatalf("applyInFiles err: %v", err)
		}
		if primary != "content of q.txt" {
			t.Fatalf("primary = %q", primary)
		}
		if len(values) != 2 || values[0].Key != "evidence" || values[0].Content != "content of a.log" || values[1].Content != "content of b.log" {
			t.Fatalf("values = %+v", values)
		}
		wantNotices := "read q.txt (16 bytes)\nread a.log (16 bytes)\nread b.log (16 bytes)\n"
		if out.String() != wantNotices {
			t.Fatalf("notices = %q, want %q", out.String(), wantNotices)
		}
	})
	t.Run("read error wraps path and stays errors.Is-able", func(t *testing.T) {
		var out bytes.Buffer
		_, _, err := applyInFiles([]inFileSpec{{Key: "x", Path: "boom"}}, fake, &out)
		if err == nil {
			t.Fatal("want error")
		}
		if !strings.Contains(err.Error(), `--in-file "boom":`) {
			t.Fatalf("err = %v, want --in-file path prefix", err)
		}
		if !errors.Is(err, fs.ErrNotExist) {
			t.Fatalf("err = %v, want errors.Is fs.ErrNotExist", err)
		}
		if out.Len() != 0 {
			t.Fatalf("no notice expected on error, got %q", out.String())
		}
	})
	t.Run("nil read uses the default bounded reader", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "real.txt")
		if err := os.WriteFile(path, []byte("real"), 0o644); err != nil {
			t.Fatal(err)
		}
		var out bytes.Buffer
		_, values, err := applyInFiles([]inFileSpec{{Key: "k", Path: path}}, nil, &out)
		if err != nil {
			t.Fatalf("applyInFiles err: %v", err)
		}
		if len(values) != 1 || values[0].Content != "real" {
			t.Fatalf("values = %+v", values)
		}
	})
}

func TestInFileValueHelpers(t *testing.T) {
	values := []inFileValue{{Key: "evidence", Content: "a"}, {Key: "context", Content: "c"}, {Key: "evidence", Content: "b"}}
	if got := lastInFile(values, "evidence"); got != "b" {
		t.Fatalf("lastInFile = %q, want b (last wins)", got)
	}
	if got := lastInFile(values, "missing"); got != "" {
		t.Fatalf("lastInFile missing = %q, want empty", got)
	}
	if got := inFileValues(values, "evidence"); len(got) != 2 || got[0] != "a" || got[1] != "b" {
		t.Fatalf("inFileValues = %v, want [a b] in spec order", got)
	}
}

func TestReadInputBounded(t *testing.T) {
	t.Run("small file passes through", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "s.txt")
		if err := os.WriteFile(path, []byte("héllo"), 0o644); err != nil {
			t.Fatal(err)
		}
		b, err := readInputBounded(path)
		if err != nil || string(b) != "héllo" {
			t.Fatalf("readInputBounded = %q, %v", b, err)
		}
	})
	t.Run("file exactly at the cap passes", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "cap.txt")
		if err := os.WriteFile(path, bytes.Repeat([]byte("a"), inFileMaxBytes), 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := readInputBounded(path); err != nil {
			t.Fatalf("cap-sized file err: %v", err)
		}
	})
	t.Run("file over the cap names the size and cap", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "big.txt")
		if err := os.WriteFile(path, bytes.Repeat([]byte("a"), inFileMaxBytes+1), 0o644); err != nil {
			t.Fatal(err)
		}
		_, err := readInputBounded(path)
		want := "bytes exceeds the 8 MiB cap"
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Fatalf("err = %v, want containing %q", err, want)
		}
	})
	t.Run("invalid UTF-8 names the first bad offset", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "bad.txt")
		if err := os.WriteFile(path, []byte("ab\xffcd"), 0o644); err != nil {
			t.Fatal(err)
		}
		_, err := readInputBounded(path)
		if err == nil || !strings.Contains(err.Error(), "not valid UTF-8 (first invalid byte at offset 2)") {
			t.Fatalf("err = %v, want pinned offset 2", err)
		}
	})
	t.Run("truncated multibyte rune reported at its offset", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "trunc.txt")
		if err := os.WriteFile(path, []byte("é\x80"), 0o644); err != nil { // valid é (0xC3 0xA9) then a stray continuation byte
			t.Fatal(err)
		}
		_, err := readInputBounded(path)
		if err == nil || !strings.Contains(err.Error(), "offset 2") {
			t.Fatalf("err = %v, want offset 2", err)
		}
	})
	t.Run("missing file stays errors.Is-able", func(t *testing.T) {
		_, err := readInputBounded(filepath.Join(t.TempDir(), "nope.txt"))
		if !errors.Is(err, fs.ErrNotExist) {
			t.Fatalf("err = %v, want fs.ErrNotExist", err)
		}
	})
}

func TestStdinReadOnce(t *testing.T) {
	t.Run("readArgsInput rejects a second stdin read", func(t *testing.T) {
		stdinRead = true
		defer func() { stdinRead = false }()
		_, err := readArgsInput("-")
		if !errors.Is(err, errStdinUsedTwice) {
			t.Fatalf("err = %v, want errStdinUsedTwice", err)
		}
	})
	t.Run("readInputBounded shares the guard", func(t *testing.T) {
		stdinRead = true
		defer func() { stdinRead = false }()
		_, err := readInputBounded("-")
		var wrapped error = err
		if !errors.Is(wrapped, errStdinUsedTwice) {
			t.Fatalf("err = %v, want errStdinUsedTwice through the guard", err)
		}
	})
}

// The fixture doubles as the README sample and the e2e payload: it must stay
// real multi-byte UTF-8 (typographic dashes and quotes) or the decode-based
// e2e assertions silently weaken to ASCII-only coverage.
func TestGutenbergFixtureUTF8(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("testdata", "gutenberg-sample.txt"))
	if err != nil {
		t.Fatalf("fixture: %v", err)
	}
	if !utf8.Valid(b) {
		t.Fatal("fixture is not valid UTF-8")
	}
	if len(b) < 3000 {
		t.Fatalf("fixture is only %d bytes; a regeneration lost the page", len(b))
	}
	if !strings.ContainsRune(string(b), '—') || !strings.ContainsRune(string(b), '“') {
		t.Fatal("fixture lost its typographic (multi-byte) characters")
	}
}
