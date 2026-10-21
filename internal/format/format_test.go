package format

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestToolsListTable(t *testing.T) {
	res := &mcp.ListToolsResult{
		Tools: []*mcp.Tool{
			{Name: "get_weather", Description: "Get the current weather for a city"},
			{Name: "no_desc"},
		},
	}
	var buf bytes.Buffer
	if err := ToolsList(&buf, res, false); err != nil {
		t.Fatalf("ToolsList() error = %v", err)
	}
	out := buf.String()
	for _, want := range []string{"NAME", "DESCRIPTION", "get_weather", "Get the current weather for a city", "no_desc"} {
		if !strings.Contains(out, want) {
			t.Errorf("ToolsList() output missing %q\ngot:\n%s", want, out)
		}
	}
}

func TestToolsListJSONEmpty(t *testing.T) {
	var buf bytes.Buffer
	if err := EmitJSON(&buf, &mcp.ListToolsResult{Tools: []*mcp.Tool{}}); err != nil {
		t.Fatalf("EmitJSON() error = %v", err)
	}
	if !json.Valid(buf.Bytes()) {
		t.Errorf("EmitJSON() output is not valid JSON: %q", buf.String())
	}
	var m map[string]any
	if err := json.Unmarshal(buf.Bytes(), &m); err != nil {
		t.Fatalf("EmitJSON() output not a JSON object: %v", err)
	}
	if tools, ok := m["tools"].([]any); !ok || len(tools) != 0 {
		t.Errorf("EmitJSON() tools = %#v, want empty array", m["tools"])
	}
	if !strings.HasSuffix(buf.String(), "\n") {
		t.Errorf("EmitJSON() output missing trailing newline: %q", buf.String())
	}
}

func TestToolCallText(t *testing.T) {
	res := &mcp.CallToolResult{
		Content: []mcp.Content{&mcp.TextContent{Text: "hello"}},
	}
	var out, errOut bytes.Buffer
	if err := ToolCall(&out, &errOut, res); err != nil {
		t.Fatalf("ToolCall() error = %v", err)
	}
	if got := out.String(); got != "hello\n" {
		t.Errorf("ToolCall() stdout = %q, want %q", got, "hello\n")
	}
	if errOut.Len() != 0 {
		t.Errorf("ToolCall() stderr = %q, want empty", errOut.String())
	}
}

func TestToolCallIsErrorToStderr(t *testing.T) {
	res := &mcp.CallToolResult{
		Content: []mcp.Content{&mcp.TextContent{Text: "boom: city not found"}},
		IsError: true,
	}
	var out, errOut bytes.Buffer
	err := ToolCall(&out, &errOut, res)
	if !errors.Is(err, ErrToolError) {
		t.Errorf("ToolCall() error = %v, want ErrToolError", err)
	}
	if out.Len() != 0 {
		t.Errorf("ToolCall() stdout = %q, want empty on tool error", out.String())
	}
	if got := errOut.String(); !strings.Contains(got, "boom: city not found") {
		t.Errorf("ToolCall() stderr = %q, want error content", got)
	}
}

func TestToolCallStructuredJSON(t *testing.T) {
	res := &mcp.CallToolResult{
		StructuredContent: map[string]any{"temp_c": 21.5, "city": "Paris"},
	}
	var out, errOut bytes.Buffer
	if err := ToolCall(&out, &errOut, res); err != nil {
		t.Fatalf("ToolCall() error = %v", err)
	}
	if !json.Valid(bytes.TrimSpace(out.Bytes())) {
		t.Errorf("ToolCall() stdout not valid JSON: %q", out.String())
	}
	for _, want := range []string{`"temp_c": 21.5`, `"city": "Paris"`} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("ToolCall() stdout missing %q\ngot:\n%s", want, out.String())
		}
	}
}

func TestToolCallMediaSummaryLines(t *testing.T) {
	t.Run("image renders one summary line", func(t *testing.T) {
		res := &mcp.CallToolResult{
			Content: []mcp.Content{
				&mcp.ImageContent{Data: []byte("a1b2c3"), MIMEType: "image/png"},
			},
		}
		var out, errOut bytes.Buffer
		if err := ToolCall(&out, &errOut, res); err != nil {
			t.Fatalf("ToolCall() error = %v", err)
		}
		if got, want := out.String(), "[image/png content, 6 bytes]\n"; got != want {
			t.Errorf("ToolCall() stdout = %q, want %q", got, want)
		}
		if errOut.Len() != 0 {
			t.Errorf("ToolCall() stderr = %q, want empty", errOut.String())
		}
	})

	t.Run("audio renders one summary line", func(t *testing.T) {
		res := &mcp.CallToolResult{
			Content: []mcp.Content{
				&mcp.AudioContent{Data: []byte("xyz12345"), MIMEType: "audio/mpeg"},
			},
		}
		var out, errOut bytes.Buffer
		if err := ToolCall(&out, &errOut, res); err != nil {
			t.Fatalf("ToolCall() error = %v", err)
		}
		if got, want := out.String(), "[audio/mpeg content, 8 bytes]\n"; got != want {
			t.Errorf("ToolCall() stdout = %q, want %q", got, want)
		}
		if errOut.Len() != 0 {
			t.Errorf("ToolCall() stderr = %q, want empty", errOut.String())
		}
	})

	t.Run("other non-text blocks stay JSON-encoded", func(t *testing.T) {
		res := &mcp.CallToolResult{
			Content: []mcp.Content{
				&mcp.TextContent{Text: "look"},
				&mcp.EmbeddedResource{Resource: &mcp.ResourceContents{URI: "mem://x", Text: "hi"}},
			},
		}
		var out, errOut bytes.Buffer
		if err := ToolCall(&out, &errOut, res); err != nil {
			t.Fatalf("ToolCall() error = %v", err)
		}
		outStr := out.String()
		if !strings.Contains(outStr, "look\n") {
			t.Errorf("ToolCall() stdout missing raw text, got %q", outStr)
		}
		if !strings.Contains(outStr, `"uri": "mem://x"`) {
			t.Errorf("ToolCall() stdout missing JSON-encoded embedded resource, got %q", outStr)
		}
	})
}

func TestSafeMIME(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"passthrough", "image/png", "image/png"},
		{"strips escapes", "image/png\x1b]0;pwned\x07", "image/png0pwned"},
		{"caps length", strings.Repeat("a", 100), strings.Repeat("a", 64)},
		{"empty", "", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := safeMIME(tt.in); got != tt.want {
				t.Errorf("safeMIME(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

func TestResourceReadText(t *testing.T) {
	res := &mcp.ReadResourceResult{
		Contents: []*mcp.ResourceContents{
			{URI: "file:///etc/hostname", MIMEType: "text/plain", Text: "myhost\n"},
		},
	}
	var buf bytes.Buffer
	if err := ResourceRead(&buf, res); err != nil {
		t.Fatalf("ResourceRead() error = %v", err)
	}
	if got := buf.String(); !strings.Contains(got, "myhost\n") {
		t.Errorf("ResourceRead() output = %q, want raw text", got)
	}
	if strings.Contains(buf.String(), `"text"`) {
		t.Errorf("ResourceRead() output should be raw text, not JSON: %q", buf.String())
	}
}

func TestResourceReadBlobDecoded(t *testing.T) {
	payload := []byte{0x00, 0x01, 0xff, 'h', 'i'}
	res := &mcp.ReadResourceResult{
		Contents: []*mcp.ResourceContents{
			{URI: "file:///bin/blob", MIMEType: "application/octet-stream", Blob: payload},
		},
	}
	var buf bytes.Buffer
	if err := ResourceRead(&buf, res); err != nil {
		t.Fatalf("ResourceRead() error = %v", err)
	}
	if !bytes.Equal(buf.Bytes(), payload) {
		t.Errorf("ResourceRead() output = %q, want raw decoded bytes %q", buf.String(), payload)
	}
}

func TestResourcesList(t *testing.T) {
	res := &mcp.ListResourcesResult{
		Resources: []*mcp.Resource{
			{URI: "file:///logs/app.log", Name: "app.log", Description: "Application log"},
		},
	}
	tmpl := &mcp.ListResourceTemplatesResult{
		ResourceTemplates: []*mcp.ResourceTemplate{
			{URITemplate: "file:///logs/{date}", Name: "log-by-date"},
		},
	}
	var buf bytes.Buffer
	if err := ResourcesList(&buf, res, tmpl, false); err != nil {
		t.Fatalf("ResourcesList() error = %v", err)
	}
	out := buf.String()
	for _, want := range []string{"file:///logs/app.log", "app.log", "Application log", "file:///logs/{date}", "log-by-date"} {
		if !strings.Contains(out, want) {
			t.Errorf("ResourcesList() output missing %q\ngot:\n%s", want, out)
		}
	}
}

func TestPromptsList(t *testing.T) {
	res := &mcp.ListPromptsResult{
		Prompts: []*mcp.Prompt{
			{Name: "review_code", Description: "Ask the server to review code"},
		},
	}
	var buf bytes.Buffer
	if err := PromptsList(&buf, res, false); err != nil {
		t.Fatalf("PromptsList() error = %v", err)
	}
	out := buf.String()
	for _, want := range []string{"NAME", "DESCRIPTION", "review_code", "Ask the server to review code"} {
		if !strings.Contains(out, want) {
			t.Errorf("PromptsList() output missing %q\ngot:\n%s", want, out)
		}
	}
}

func TestPromptGet(t *testing.T) {
	res := &mcp.GetPromptResult{
		Description: "Two-step code review prompt",
		Messages: []*mcp.PromptMessage{
			{Role: "user", Content: &mcp.TextContent{Text: "Review this code"}},
			{Role: "assistant", Content: &mcp.TextContent{Text: "Sure, paste it."}},
		},
	}
	var buf bytes.Buffer
	if err := PromptGet(&buf, res); err != nil {
		t.Fatalf("PromptGet() error = %v", err)
	}
	out := buf.String()
	for _, want := range []string{"Review this code", "Sure, paste it.", "user", "assistant"} {
		if !strings.Contains(out, want) {
			t.Errorf("PromptGet() output missing %q\ngot:\n%s", want, out)
		}
	}
}

func TestServerInfo(t *testing.T) {
	res := &mcp.InitializeResult{
		ProtocolVersion: "2025-06-18",
		ServerInfo:      &mcp.Implementation{Name: "files-server", Version: "1.2.3"},
		Capabilities: &mcp.ServerCapabilities{
			Tools: &mcp.ToolCapabilities{},
		},
		Instructions: "Use tools carefully.",
	}
	var buf bytes.Buffer
	if err := ServerInfo(&buf, res); err != nil {
		t.Fatalf("ServerInfo() error = %v", err)
	}
	out := buf.String()
	for _, want := range []string{"files-server", "1.2.3", "2025-06-18", "tools", "Use tools carefully."} {
		if !strings.Contains(out, want) {
			t.Errorf("ServerInfo() output missing %q\ngot:\n%s", want, out)
		}
	}
	// The trailing footer carries the client's copyright and website; it is
	// ASCII-only so --plain's byte-ASCII contract holds on stdout too.
	for _, want := range []string{"mcp v", "Copyright ConsulTent Pte. Ltd., all Rights Reserved.", "https://consultent.ltd"} {
		if !strings.Contains(out, want) {
			t.Errorf("ServerInfo() footer missing %q\ngot:\n%s", want, out)
		}
	}
}

func TestNilResultsNoPanic(t *testing.T) {
	var buf bytes.Buffer
	if err := ToolsList(&buf, nil, false); err != nil {
		t.Errorf("ToolsList(nil) error = %v", err)
	}
	if err := ResourcesList(&buf, nil, nil, false); err != nil {
		t.Errorf("ResourcesList(nil, nil) error = %v", err)
	}
	if err := ResourceRead(&buf, nil); err != nil {
		t.Errorf("ResourceRead(nil) error = %v", err)
	}
	if err := PromptsList(&buf, nil, false); err != nil {
		t.Errorf("PromptsList(nil) error = %v", err)
	}
	if err := PromptGet(&buf, nil); err != nil {
		t.Errorf("PromptGet(nil) error = %v", err)
	}
	if err := ServerInfo(&buf, nil); err != nil {
		t.Errorf("ServerInfo(nil) error = %v", err)
	}
	var out, errOut bytes.Buffer
	if err := ToolCall(&out, &errOut, nil); err != nil {
		t.Errorf("ToolCall(nil) error = %v", err)
	}
}
