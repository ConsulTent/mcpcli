package cli

import (
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"
	"unicode/utf8"
)

// inFileMaxBytes caps each --in-file read (including its "-" stdin form):
// generous for logs and patches, while a mistaken binary or device cannot
// fill memory.
const inFileMaxBytes = 8 << 20

// stdinRead records that stdin ("-") has been consumed this invocation. It
// is shared by every stdin reader (-A, jev gate --input, --in-file) so a
// second read fails loudly instead of silently returning empty bytes (a
// drained pipe reads as 0 bytes, nil error). Tests reset it directly; the
// cli package's tests run sequentially.
var stdinRead bool

// errStdinUsedTwice is returned, wrapped, by any stdin reader invoked twice
// in one invocation.
var errStdinUsedTwice = errors.New(`stdin ("-") may only be read once per invocation`)

// inFileSpec is one parsed --in-file value: a bare PATH (the command's
// primary text input, where it has one) or KEY=PATH (a named text slot).
// Key "" marks the bare form.
type inFileSpec struct {
	Key  string
	Path string
}

// inFileValue is one read --in-file value, in spec order.
type inFileValue struct {
	Key     string
	Content string
}

// parseInFiles splits --in-file values into specs. Each value splits on the
// first "=" exactly like --arg KEY=VALUE, so a PATH containing "=" parses as
// KEY=PATH — pass an explicit key or rename. validKeys nil accepts any key
// (tools call, prompts get); otherwise the key must be in the set and the
// error names it. allowBare permits the bare form (jev ask/screen, whose
// positional the file replaces); a second bare value is an error, there is
// one primary slot. File contents are raw strings, never JSON-decoded.
func parseInFiles(vals []string, validKeys map[string]struct{}, allowBare bool) ([]inFileSpec, error) {
	specs := make([]inFileSpec, 0, len(vals))
	bare := false
	for _, val := range vals {
		key, path, ok := strings.Cut(val, "=")
		switch {
		case val == "":
			return nil, fmt.Errorf("invalid --in-file value %q: expected [KEY=]PATH", val)
		case !ok:
			if !allowBare {
				return nil, fmt.Errorf("invalid --in-file value %q: this command has no primary text input, use KEY=PATH", val)
			}
			if bare {
				return nil, fmt.Errorf("invalid --in-file value %q: the primary text input may only be given once", val)
			}
			bare = true
			specs = append(specs, inFileSpec{Path: val})
		case key == "" || path == "":
			return nil, fmt.Errorf("invalid --in-file value %q: expected [KEY=]PATH", val)
		default:
			if validKeys != nil {
				if _, ok := validKeys[key]; !ok {
					return nil, fmt.Errorf("invalid --in-file value %q: unknown key %q (valid: %s)", val, key, strings.Join(inFileKeyNames(validKeys), ", "))
				}
			}
			specs = append(specs, inFileSpec{Key: key, Path: path})
		}
	}
	return specs, nil
}

// inFileKeyNames returns the sorted valid keys with their "=" suffix for
// error messages.
func inFileKeyNames(validKeys map[string]struct{}) []string {
	names := make([]string, 0, len(validKeys))
	for k := range validKeys {
		names = append(names, k+"=")
	}
	slices.Sort(names)
	return names
}

// applyInFiles reads every spec's source and returns the primary text (the
// bare spec's content, "" when there is none) with the keyed values in spec
// order — callers decide last-wins or append per slot, keeping duplicate-key
// handling at the merge points. read defaults to readInputBounded. Each
// success prints "read <path> (N bytes)" to errOut in every output mode
// (the --outfile "wrote …" precedent): input reads leave a trace without
// touching stdout.
func applyInFiles(specs []inFileSpec, read func(string) ([]byte, error), errOut io.Writer) (string, []inFileValue, error) {
	if read == nil {
		read = readInputBounded
	}
	var primary string
	values := make([]inFileValue, 0, len(specs))
	for _, spec := range specs {
		data, err := read(spec.Path)
		if err != nil {
			return "", nil, fmt.Errorf("--in-file %q: %w", spec.Path, err)
		}
		fmt.Fprintf(errOut, "read %s (%d bytes)\n", spec.Path, len(data))
		if spec.Key == "" {
			primary = string(data)
		} else {
			values = append(values, inFileValue{Key: spec.Key, Content: string(data)})
		}
	}
	return primary, values, nil
}

// lastInFile returns the last value for key (last wins, like --arg), or "".
func lastInFile(values []inFileValue, key string) string {
	v := ""
	for _, val := range values {
		if val.Key == key {
			v = val.Content
		}
	}
	return v
}

// inFileValues returns every value for key in spec order (repeatable slots,
// like jev evidence: file values append after flag values).
func inFileValues(values []inFileValue, key string) []string {
	var out []string
	for _, val := range values {
		if val.Key == key {
			out = append(out, val.Content)
		}
	}
	return out
}

// readInputBounded reads one --in-file source: stdin for "-", the file at
// path otherwise. Reads are capped at inFileMaxBytes during the read — a
// regular file over the cap is rejected from its size before loading, and
// anything else (stdin, special files) by the read length — and must be
// valid UTF-8, with the first invalid byte's offset named: no silent U+FFFD
// replacement through JSON marshaling.
func readInputBounded(path string) ([]byte, error) {
	var data []byte
	var err error
	if path == "-" {
		if stdinRead {
			return nil, errStdinUsedTwice
		}
		stdinRead = true
		data, err = io.ReadAll(io.LimitReader(os.Stdin, inFileMaxBytes+1))
	} else {
		f, oerr := os.Open(path)
		if oerr != nil {
			return nil, oerr
		}
		defer func() { _ = f.Close() }()
		info, serr := f.Stat()
		if serr != nil {
			return nil, serr
		}
		if info.Mode().IsRegular() && info.Size() > int64(inFileMaxBytes) {
			return nil, fmt.Errorf("%d bytes exceeds the %d MiB cap", info.Size(), inFileMaxBytes>>20)
		}
		data, err = io.ReadAll(io.LimitReader(f, inFileMaxBytes+1))
	}
	if err != nil {
		return nil, err
	}
	if len(data) > inFileMaxBytes {
		return nil, fmt.Errorf("%d bytes exceeds the %d MiB cap", len(data), inFileMaxBytes>>20)
	}
	if !utf8.Valid(data) {
		return nil, fmt.Errorf("not valid UTF-8 (first invalid byte at offset %d)", utf8InvalidOffset(data))
	}
	return data, nil
}

// utf8InvalidOffset returns the byte offset of the first invalid UTF-8
// sequence; callers only invoke it on input utf8.Valid rejected.
func utf8InvalidOffset(b []byte) int {
	for i := 0; i < len(b); {
		r, size := utf8.DecodeRune(b[i:])
		if r == utf8.RuneError && size == 1 {
			return i
		}
		i += size
	}
	return len(b)
}
