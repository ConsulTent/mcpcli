// Package format turns MCP SDK result types into human- or machine-readable
// output. Formatters are pure data formatting: no colors, no emoji, no
// output-mode logic. Callers decide human-vs-JSON by calling a formatter
// directly or serializing the result with EmitJSON.
package format

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"

	"charm.land/lipgloss/v2"
	"charm.land/lipgloss/v2/table"
	"github.com/ConsulTent/mcpcli/internal/version"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// ErrToolError is returned by ToolCall when the server reports the tool call
// as failed (CallToolResult.IsError). The result content itself is written to
// the caller's error writer.
var ErrToolError = errors.New("tool returned an error")

// EmitJSON writes v as indented JSON followed by a trailing newline.
func EmitJSON(w io.Writer, v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal JSON: %w", err)
	}
	b = append(b, '\n')
	if _, err := w.Write(b); err != nil {
		return fmt.Errorf("write JSON: %w", err)
	}
	return nil
}

// border returns the table border for a human rendering: the ASCII border
// when the caller asked for plain output (the --plain contract is zero
// non-ASCII bytes), the Unicode border otherwise. Formatters stay mode-free:
// plain is data the caller passes, not ui state they read.
func border(plain bool) lipgloss.Border {
	if plain {
		return lipgloss.ASCIIBorder()
	}
	return lipgloss.NormalBorder()
}

// ToolsList renders a tools/list result as a table of names and descriptions.
func ToolsList(w io.Writer, res *mcp.ListToolsResult, plain bool) error {
	t := table.New().Border(border(plain)).Headers("NAME", "DESCRIPTION")
	if res != nil {
		for _, tool := range res.Tools {
			if tool == nil {
				continue
			}
			t.Row(tool.Name, tool.Description)
		}
	}
	return WriteTable(w, t)
}

// ToolCall renders a tools/call result. Text content blocks are printed raw to
// out. If there is no text content but StructuredContent is present, it is
// marshaled as JSON to out. Image and audio content blocks render as one
// compact ASCII summary line each ([mime content, N bytes]); any other
// non-text content blocks are JSON-encoded to out. When the result reports
// IsError, everything is written to errOut and ErrToolError is returned.
func ToolCall(out, errOut io.Writer, res *mcp.CallToolResult) error {
	if res == nil {
		return nil
	}
	w := io.Writer(out)
	if res.IsError {
		w = errOut
	}

	var texts []string
	var others []mcp.Content
	for _, c := range res.Content {
		if c == nil {
			continue
		}
		if tc, ok := c.(*mcp.TextContent); ok {
			texts = append(texts, tc.Text)
		} else {
			others = append(others, c)
		}
	}

	for _, text := range texts {
		fmt.Fprintln(w, text)
	}
	if len(texts) == 0 && res.StructuredContent != nil {
		if err := EmitJSON(w, res.StructuredContent); err != nil {
			return err
		}
	}
	for _, c := range others {
		// Image and audio need separate cases: a combined case would bind
		// the mcp.Content interface, which has no Data field.
		switch c := c.(type) {
		case *mcp.ImageContent:
			fmt.Fprintf(w, "[%s content, %d bytes]\n", safeMIME(c.MIMEType), len(c.Data))
		case *mcp.AudioContent:
			fmt.Fprintf(w, "[%s content, %d bytes]\n", safeMIME(c.MIMEType), len(c.Data))
		default:
			if err := EmitJSON(w, c); err != nil {
				return err
			}
		}
	}

	if res.IsError {
		return ErrToolError
	}
	return nil
}

// safeMIME keeps only terminal-safe characters and caps length, mirroring
// the CLI's safeVersion approach (format must not import internal/cli).
// Only [A-Za-z0-9/+. -] survives, capped at 64 runes, so a server-controlled
// MIME string cannot carry ANSI escapes into the summary line.
func safeMIME(s string) string {
	const max = 64
	var b []byte
	for i := 0; i < len(s) && len(b) < max; i++ {
		c := s[i]
		switch {
		case c >= '0' && c <= '9', c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z',
			c == '/', c == '+', c == '.', c == ' ', c == '-':
			b = append(b, c)
		}
	}
	return string(b)
}

// ResourcesList renders resources/list and resources/templates/list results as
// two tables (templates only when the second result is non-empty).
func ResourcesList(w io.Writer, res *mcp.ListResourcesResult, tmpl *mcp.ListResourceTemplatesResult, plain bool) error {
	t := table.New().Border(border(plain)).Headers("URI", "NAME", "DESCRIPTION")
	if res != nil {
		for _, r := range res.Resources {
			if r == nil {
				continue
			}
			t.Row(r.URI, r.Name, r.Description)
		}
	}
	if err := WriteTable(w, t); err != nil {
		return err
	}

	if tmpl == nil || len(tmpl.ResourceTemplates) == 0 {
		return nil
	}
	ut := table.New().Border(border(plain)).Headers("URI TEMPLATE", "NAME", "DESCRIPTION")
	for _, rt := range tmpl.ResourceTemplates {
		if rt == nil {
			continue
		}
		ut.Row(rt.URITemplate, rt.Name, rt.Description)
	}
	return WriteTable(w, ut)
}

// ResourceRead renders a resources/read result: text contents are printed raw;
// blob contents are written to w as their raw (already base64-decoded) bytes.
func ResourceRead(w io.Writer, res *mcp.ReadResourceResult) error {
	if res == nil {
		return nil
	}
	for _, rc := range res.Contents {
		if rc == nil {
			continue
		}
		if rc.Text != "" {
			fmt.Fprintln(w, rc.Text)
			continue
		}
		if len(rc.Blob) > 0 {
			if _, err := w.Write(rc.Blob); err != nil {
				return fmt.Errorf("write blob: %w", err)
			}
		}
	}
	return nil
}

// PromptsList renders a prompts/list result as a table of names and
// descriptions.
func PromptsList(w io.Writer, res *mcp.ListPromptsResult, plain bool) error {
	t := table.New().Border(border(plain)).Headers("NAME", "DESCRIPTION")
	if res != nil {
		for _, p := range res.Prompts {
			if p == nil {
				continue
			}
			t.Row(p.Name, p.Description)
		}
	}
	return WriteTable(w, t)
}

// PromptGet renders a prompts/get result: the description (if any) followed by
// one line per message. Text content is printed raw; any other content block
// is JSON-encoded.
func PromptGet(w io.Writer, res *mcp.GetPromptResult) error {
	if res == nil {
		return nil
	}
	if res.Description != "" {
		fmt.Fprintln(w, res.Description)
	}
	for _, msg := range res.Messages {
		if msg == nil {
			continue
		}
		if tc, ok := msg.Content.(*mcp.TextContent); ok {
			fmt.Fprintf(w, "%s: %s\n", msg.Role, tc.Text)
			continue
		}
		fmt.Fprintf(w, "%s: ", msg.Role)
		if err := EmitJSON(w, msg.Content); err != nil {
			return err
		}
	}
	return nil
}

// ServerInfo renders an initialize result as labeled lines describing the
// server, its version, the negotiated protocol version, and capabilities.
// For JSON output, callers serialize the InitializeResult itself with EmitJSON.
func ServerInfo(w io.Writer, res *mcp.InitializeResult) error {
	if res == nil {
		return nil
	}
	if res.ServerInfo != nil {
		fmt.Fprintf(w, "%-14s%s\n", "Server:", res.ServerInfo.Name)
		fmt.Fprintf(w, "%-14s%s\n", "Version:", res.ServerInfo.Version)
	}
	fmt.Fprintf(w, "%-14s%s\n", "Protocol:", res.ProtocolVersion)
	if res.Instructions != "" {
		fmt.Fprintf(w, "%-14s%s\n", "Instructions:", res.Instructions)
	}
	fmt.Fprintf(w, "%-14s%s\n", "Capabilities:", strings.Join(capabilityNames(res.Capabilities), ", "))
	// ASCII-only footer (mcp vX.Y.Z - copyright - website): stdout must stay
	// byte-ASCII in --plain mode, and this line ships in every mode.
	fmt.Fprintf(w, "\nmcp %s - Copyright ConsulTent Pte. Ltd., all Rights Reserved. - https://consultent.ltd\n", version.Version)
	return nil
}

// capabilityNames lists the capabilities a server declares, in a stable order.
func capabilityNames(c *mcp.ServerCapabilities) []string {
	if c == nil {
		return nil
	}
	var names []string
	if c.Tools != nil {
		names = append(names, "tools")
	}
	if c.Resources != nil {
		names = append(names, "resources")
	}
	if c.Prompts != nil {
		names = append(names, "prompts")
	}
	if c.Logging != nil {
		names = append(names, "logging")
	}
	if c.Completions != nil {
		names = append(names, "completions")
	}
	if len(c.Experimental) > 0 {
		names = append(names, "experimental")
	}
	if len(c.Extensions) > 0 {
		names = append(names, "extensions")
	}
	return names
}

// WriteTable renders t to w followed by a newline (Render does not end in
// one) and propagates write errors. Shared by this package's formatters and
// by cli's compat report.
func WriteTable(w io.Writer, t *table.Table) error {
	_, err := fmt.Fprintln(w, t.Render())
	if err != nil {
		return fmt.Errorf("write table: %w", err)
	}
	return nil
}
