package cli

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/ConsulTent/mcpcli/internal/format"
	"github.com/ConsulTent/mcpcli/internal/ui"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// replPrompt is written to stderr before every read; results go to stdout, so
// piped result streams stay clean.
const replPrompt = "mcp> "

// replUnknownCommand is the hint for an unrecognized command word.
const replUnknownCommand = "unknown command, try help"

// replHelp lists the commands runRepl understands. Input is split on
// whitespace like shell words; quoting is not supported (v1 keeps the parser
// to strings.Fields), and a JSON-OBJ token must therefore be a single
// whitespace-free JSON object.
const replHelp = `mcp REPL commands:
  help                    show this command list
  tools                   list the server's tools
  call NAME [JSON-OBJ]    call a tool; JSON-OBJ is a JSON object of arguments
  resources               list resources and resource templates
  read URI                read a resource by URI
  prompts                 list the server's prompts
  get NAME [JSON-OBJ]     get a prompt; JSON-OBJ fills its string arguments
  info                    show the server's identity and capabilities
  ping                    time a round trip to the server
  exit, quit              leave the REPL

Input is split on whitespace like shell words; quoting is not supported, so
JSON-OBJ must be one whitespace-free JSON object, e.g. call echo {"msg":"hi"}.`

// runRepl runs the line-based interactive REPL against an already-open
// session: the caller (the -i wiring in root.go) owns the connection, the
// REPL only dispatches to it, so no command here ever reconnects.
//
// The prompt goes to errOut (stderr) and results to out (stdout); in JSON ui
// mode there is no prompt and no decoration at all — pure result documents on
// stdout and {"error":...} documents on stderr. Output follows the global
// ui.Current mode, like every other command in this package.
//
// f carries the GlobalFlags the REPL needs: each command runs under its own
// context.WithTimeout(ctx, f.Timeout) window. Errors from operations and from
// malformed input are printed to errOut and the loop continues; only
// exit/quit or end of input (Ctrl+D) end it, returning nil.
func runRepl(ctx context.Context, in io.Reader, out, errOut io.Writer, s *mcp.ClientSession, f *GlobalFlags) error {
	scanner := bufio.NewScanner(in)
	for {
		if ui.Current != ui.JSON {
			fmt.Fprint(errOut, replPrompt)
		}
		if !scanner.Scan() {
			return scanner.Err() // nil on clean EOF
		}
		fields := strings.Fields(scanner.Text())
		if len(fields) == 0 {
			continue
		}
		opCtx, cancel := context.WithTimeout(ctx, f.Timeout)
		done := replCommand(opCtx, fields, out, errOut, s)
		cancel()
		if done {
			return nil
		}
	}
}

// replCommand runs one REPL line against the session. It reports whether the
// loop should end (exit/quit); every other failure — an operation error or a
// malformed line — is printed to errOut and swallowed so the session keeps
// serving the next command.
func replCommand(ctx context.Context, fields []string, out, errOut io.Writer, s *mcp.ClientSession) (done bool) {
	var err error
	switch strings.ToLower(fields[0]) {
	case "help":
		if ui.Current != ui.JSON {
			fmt.Fprintln(errOut, replHelp)
		}
	case "exit", "quit":
		return true
	case "tools":
		err = opListTools(ctx, s, out)
	case "call":
		err = replCall(ctx, fields, out, errOut, s)
	case "resources":
		err = opListResources(ctx, s, out)
	case "read":
		err = replRead(ctx, fields, out, s)
	case "prompts":
		err = opListPrompts(ctx, s, out)
	case "get":
		err = replGet(ctx, fields, out, s)
	case "info":
		err = opInfo(s, out)
	case "ping":
		err = opPing(ctx, s, out)
	default:
		if ui.Current == ui.JSON {
			err = errors.New(replUnknownCommand)
		} else {
			fmt.Fprintln(errOut, replUnknownCommand)
		}
	}
	if err != nil {
		replError(errOut, err)
	}
	return false
}

// replError reports one failed REPL command on errOut: ❌ <err> in Rich,
// ERROR: <err> in Plain, {"error":"…"} in JSON. Write failures are discarded:
// a dead stdout cannot be reported anywhere better, and the loop must go on.
func replError(errOut io.Writer, err error) {
	if ui.Current == ui.JSON {
		_ = format.EmitJSON(errOut, map[string]string{"error": err.Error()})
		return
	}
	ui.Fail(errOut, "%s", err)
}

// replCall mirrors "tools call NAME". The optional third token is the tool's
// argument object; if present it must be a JSON object, else the line fails
// without touching the session. Rendering is shared with the command.
func replCall(ctx context.Context, fields []string, out, errOut io.Writer, s *mcp.ClientSession) error {
	if len(fields) < 2 {
		return errors.New("usage: call NAME [JSON-OBJ]")
	}
	var args map[string]any
	if len(fields) > 2 {
		var err error
		if args, err = replJSONObject(fields[2]); err != nil {
			return err
		}
	}
	return opCallTool(ctx, s, fields[1], args, out, errOut)
}

// replRead mirrors "resources read URI".
func replRead(ctx context.Context, fields []string, out io.Writer, s *mcp.ClientSession) error {
	if len(fields) < 2 {
		return errors.New("usage: read URI")
	}
	return opReadResource(ctx, s, fields[1], out)
}

// replGet mirrors "prompts get NAME". The optional third token fills the
// prompt's arguments; values must be JSON scalars, since MCP prompt
// arguments are string-typed (see replPromptArgs).
func replGet(ctx context.Context, fields []string, out io.Writer, s *mcp.ClientSession) error {
	if len(fields) < 2 {
		return errors.New("usage: get NAME [JSON-OBJ]")
	}
	var args map[string]string
	if len(fields) > 2 {
		var err error
		if args, err = replPromptArgs(fields[2]); err != nil {
			return err
		}
	}
	return opGetPrompt(ctx, s, fields[1], args, out)
}

// replJSONObject decodes one whitespace-free JSON object token into tool
// arguments. Trailing garbage after the object is rejected, so a typo like
// {"a":1}} fails instead of silently dropping it.
func replJSONObject(token string) (map[string]any, error) {
	dec := json.NewDecoder(strings.NewReader(token))
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, fmt.Errorf("arguments must be a JSON object: %w", err)
	}
	obj, ok := v.(map[string]any)
	if !ok {
		return nil, errors.New("arguments must be a JSON object")
	}
	if _, err := dec.Token(); err != io.EOF {
		return nil, errors.New("arguments must be a single JSON object")
	}
	return obj, nil
}

// replPromptArgs decodes a JSON object token into prompt arguments: every
// value must be a scalar (string, number, or boolean) and is stringified with
// %v, because MCP prompt arguments are string-typed. Objects, arrays, and
// null are rejected so {"a":{"b":1}} fails loudly instead of becoming "[map
// [b:true]]" on the wire.
func replPromptArgs(token string) (map[string]string, error) {
	obj, err := replJSONObject(token)
	if err != nil {
		return nil, err
	}
	args := make(map[string]string, len(obj))
	for key, value := range obj {
		switch value.(type) {
		case string, bool, float64:
			args[key] = fmt.Sprintf("%v", value)
		default:
			return nil, fmt.Errorf("prompt argument %q must be a scalar (string, number, or boolean)", key)
		}
	}
	return args, nil
}
