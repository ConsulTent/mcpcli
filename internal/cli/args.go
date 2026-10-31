package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
)

// parseToolArgs decodes --arg KEY=VALUE pairs into a tool arguments object.
// Each pair is split on the first "="; a pair without "=" is an error naming
// the pair. The value is JSON-decoded when it parses as JSON (count=3 becomes
// the number 3, flag=true the boolean true, x={"a":1} an object) and kept as
// the raw string otherwise. Repeated keys: last wins.
func parseToolArgs(pairs []string) (map[string]any, error) {
	args := make(map[string]any, len(pairs))
	for _, pair := range pairs {
		key, value, ok := strings.Cut(pair, "=")
		if !ok {
			return nil, fmt.Errorf("invalid argument %q: expected KEY=VALUE", pair)
		}
		args[key] = decodeArgValue(value)
	}
	return args, nil
}

// decodeArgValue returns v JSON-decoded when it parses as a JSON value and the
// raw string otherwise.
func decodeArgValue(v string) any {
	var decoded any
	if err := json.Unmarshal([]byte(v), &decoded); err == nil {
		return decoded
	}
	return v
}

// loadArgsFile reads a JSON object of tool arguments from the file at path;
// "-" reads standard input. Anything that is not a JSON object is an error
// naming the path.
func loadArgsFile(path string) (map[string]any, error) {
	data, err := readArgsInput(path)
	if err != nil {
		return nil, err
	}
	var args map[string]any
	if err := json.Unmarshal(data, &args); err != nil {
		return nil, fmt.Errorf("args file %s: expected a JSON object: %w", path, err)
	}
	return args, nil
}

// readArgsInput returns the bytes of an args source: standard input for "-",
// the file at path otherwise. Stdin may be consumed once per invocation
// (stdinRead); a second "-" read anywhere (-A, jev gate --input, --in-file)
// returns errStdinUsedTwice rather than silently reading a drained pipe.
func readArgsInput(path string) ([]byte, error) {
	if path == "-" {
		if stdinRead {
			return nil, errStdinUsedTwice
		}
		stdinRead = true
		data, err := io.ReadAll(os.Stdin)
		if err != nil {
			return nil, fmt.Errorf("read args from stdin: %w", err)
		}
		return data, nil
	}
	return os.ReadFile(path)
}
