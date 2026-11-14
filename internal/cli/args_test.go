package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestParseToolArgs pins the k=v decoding contract: the value is JSON-decoded
// when it parses as JSON and otherwise kept as the raw string; repeated keys
// last-wins; a pair without "=" is an error naming the pair.
func TestParseToolArgs(t *testing.T) {
	t.Run("json values decode", func(t *testing.T) {
		args, err := parseToolArgs([]string{"count=3", "flag=true", "x={\"a\":1}", "xs=[1,2]", `q="hi"`})
		if err != nil {
			t.Fatalf("parseToolArgs: %v", err)
		}
		if got := args["count"]; got != float64(3) {
			t.Errorf("count = %v (%T), want float64(3)", got, got)
		}
		if got := args["flag"]; got != true {
			t.Errorf("flag = %v (%T), want true", got, got)
		}
		obj, ok := args["x"].(map[string]any)
		if !ok {
			t.Fatalf("x = %v (%T), want a map", args["x"], args["x"])
		}
		if obj["a"] != float64(1) {
			t.Errorf("x.a = %v, want 1", obj["a"])
		}
		arr, ok := args["xs"].([]any)
		if !ok || len(arr) != 2 || arr[0] != float64(1) || arr[1] != float64(2) {
			t.Errorf("xs = %v (%T), want [1 2]", args["xs"], args["xs"])
		}
		if got := args["q"]; got != "hi" {
			t.Errorf("q = %v (%T), want \"hi\"", got, got)
		}
	})

	t.Run("string fallback", func(t *testing.T) {
		args, err := parseToolArgs([]string{"name=hello world", "path=/tmp/x", "empty=", "code=3x"})
		if err != nil {
			t.Fatalf("parseToolArgs: %v", err)
		}
		for k, want := range map[string]string{"name": "hello world", "path": "/tmp/x", "empty": "", "code": "3x"} {
			if got := args[k]; got != want {
				t.Errorf("%s = %q (%T), want %q", k, got, got, want)
			}
		}
	})

	t.Run("repeated keys last wins", func(t *testing.T) {
		args, err := parseToolArgs([]string{"k=1", "k=2"})
		if err != nil {
			t.Fatalf("parseToolArgs: %v", err)
		}
		if args["k"] != float64(2) {
			t.Errorf("k = %v, want 2 (last wins)", args["k"])
		}
	})

	t.Run("missing equals is an error naming the pair", func(t *testing.T) {
		_, err := parseToolArgs([]string{"count=3", "noequals"})
		if err == nil || !strings.Contains(err.Error(), "noequals") {
			t.Fatalf("parseToolArgs with pair \"noequals\" = %v, want an error naming the pair", err)
		}
	})

	t.Run("empty pairs give empty map", func(t *testing.T) {
		args, err := parseToolArgs(nil)
		if err != nil {
			t.Fatalf("parseToolArgs(nil): %v", err)
		}
		if len(args) != 0 {
			t.Errorf("args = %v, want empty", args)
		}
	})
}

// TestLoadArgsFile pins the args-file contract: a JSON object from a path or
// "-" for stdin; anything that is not a JSON object — null included, which
// unmarshals into a nil map without error — is an error naming the path, and
// the bytes must be valid UTF-8 with the first invalid offset named (no
// silent U+FFFD into tool arguments).
func TestLoadArgsFile(t *testing.T) {
	t.Run("json object file", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "args.json")
		if err := os.WriteFile(path, []byte(`{"msg":"hi","count":3}`), 0o600); err != nil {
			t.Fatal(err)
		}
		args, err := loadArgsFile(path)
		if err != nil {
			t.Fatalf("loadArgsFile: %v", err)
		}
		if args["msg"] != "hi" || args["count"] != float64(3) {
			t.Errorf("args = %v, want {msg:hi count:3}", args)
		}
	})

	t.Run("dash reads stdin", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "stdin.json")
		if err := os.WriteFile(path, []byte(`{"from":"stdin"}`), 0o600); err != nil {
			t.Fatal(err)
		}
		f, err := os.Open(path)
		if err != nil {
			t.Fatal(err)
		}
		defer f.Close()
		saved := os.Stdin
		os.Stdin = f
		defer func() { os.Stdin = saved }()

		args, err := loadArgsFile("-")
		if err != nil {
			t.Fatalf("loadArgsFile(-): %v", err)
		}
		if args["from"] != "stdin" {
			t.Errorf("args = %v, want {from:stdin}", args)
		}
	})

	t.Run("non-object file is an error naming the path", func(t *testing.T) {
		for name, content := range map[string]string{
			"array": `[1,2,3]`,
			"jsonl": `{"a":1}
{"b":2}`,
		} {
			path := filepath.Join(t.TempDir(), "bad.json")
			if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
				t.Fatal(err)
			}
			_, err := loadArgsFile(path)
			if err == nil || !strings.Contains(err.Error(), path) {
				t.Errorf("%s file: loadArgsFile = %v, want an error naming %s", name, err, path)
			}
		}
	})

	t.Run("null file is an error naming the path", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "null.json")
		if err := os.WriteFile(path, []byte(`null`), 0o600); err != nil {
			t.Fatal(err)
		}
		_, err := loadArgsFile(path)
		if err == nil || !strings.Contains(err.Error(), "expected a JSON object") || !strings.Contains(err.Error(), path) {
			t.Fatalf("loadArgsFile(null) = %v, want an error naming the path", err)
		}
	})

	t.Run("invalid UTF-8 file is an error naming the offset", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "latin1.json")
		if err := os.WriteFile(path, []byte("{\"msg\":\"caf\xe9\"}"), 0o600); err != nil {
			t.Fatal(err)
		}
		_, err := loadArgsFile(path)
		if err == nil || !strings.Contains(err.Error(), "not valid UTF-8") || !strings.Contains(err.Error(), "offset 11") {
			t.Fatalf("loadArgsFile(latin-1) = %v, want the UTF-8 error naming offset 11", err)
		}
	})

	t.Run("missing file is an error naming the path", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "absent.json")
		_, err := loadArgsFile(path)
		if err == nil || !strings.Contains(err.Error(), path) {
			t.Fatalf("loadArgsFile(missing) = %v, want an error naming the path", err)
		}
	})
}
