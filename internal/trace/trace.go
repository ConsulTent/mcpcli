// Package trace collects round-trip timing for one mcp run: connection
// phases, every request/response call with approximate payload sizes, and
// totals. A nil *Tracer is valid and does nothing, so callers gate features
// on tracer != nil with no second nil-check at each hook. All output goes
// to writers the caller owns — stdout is never touched (the --json contract
// keeps stdout as exactly one result document).
package trace

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"strings"
	"sync"
	"time"

	"charm.land/lipgloss/v2"
	"charm.land/lipgloss/v2/table"
	"github.com/ConsulTent/mcpcli/internal/format"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// barWidth is the Rich-mode proportion-bar width in cells.
const barWidth = 12

// Tracer accumulates one run's timing. It is safe for concurrent use: the
// SDK permits concurrent in-flight calls, and the middleware records from
// whatever goroutine the call ran on.
type Tracer struct {
	mu       sync.Mutex
	phases   map[string]time.Duration
	sections []*section
	current  *section
	verbose  bool
}

type section struct {
	label string
	calls []Call
}

// Call is one request/response round trip. RTT covers the full exchange as
// seen by the sending middleware: SDK marshal + wire + server + decode.
type Call struct {
	Method    string
	RTT       time.Duration
	ReqBytes  int
	RespBytes int
	Err       error
}

// New returns a ready Tracer; verbose only affects the human render's
// totals line (the JSON document always carries full totals).
func New(verbose bool) *Tracer {
	return &Tracer{phases: map[string]time.Duration{}, verbose: verbose}
}

// SetPhase records a run-level phase duration (build, open, op). Nil-safe.
func (t *Tracer) SetPhase(name string, d time.Duration) {
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.phases[name] = d
}

// StartSection begins a labeled connection section — one per Open, so
// compat's per-version probes each get their own. Nil-safe.
func (t *Tracer) StartSection(label string) {
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	s := &section{label: label}
	t.sections = append(t.sections, s)
	t.current = s
}

// Record adds one call to the current section. Notifications (fire-and-forget
// sends with no round trip) are skipped. Nil-safe.
func (t *Tracer) Record(method string, rtt time.Duration, reqBytes, respBytes int, err error) {
	if t == nil || strings.HasPrefix(method, "notifications/") {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.current == nil {
		t.current = &section{label: "connect"}
		t.sections = append(t.sections, t.current)
	}
	t.current.calls = append(t.current.calls, Call{Method: method, RTT: rtt, ReqBytes: reqBytes, RespBytes: respBytes, Err: err})
}

// Middleware returns the client sending middleware that times every request
// around next() and records it. Register before Connect.
func (t *Tracer) Middleware() mcp.Middleware {
	return func(next mcp.MethodHandler) mcp.MethodHandler {
		return func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
			start := time.Now()
			res, err := next(ctx, method, req)
			t.Record(method, time.Since(start), approxBytes(req.GetParams()), approxBytes(res), err)
			return res, err
		}
	}
}

// Totals returns the summed round-trip time and per-direction payload bytes.
func (t *Tracer) Totals() (wire time.Duration, sent, recv int) {
	if t == nil {
		return 0, 0, 0
	}
	for _, s := range t.sections {
		for _, c := range s.calls {
			wire += c.RTT
			sent += c.ReqBytes
			recv += c.RespBytes
		}
	}
	return wire, sent, recv
}

// Render writes the human report: one labeled table per section, and under
// verbose a totals line splitting wire vs client-internal time. Rich mode
// carries the proportion bar column and ❌ failure markers; plain mode is
// byte-ASCII with ! failure markers and no bars.
func (t *Tracer) Render(w io.Writer, plain, verbose bool) error {
	if t == nil {
		return nil
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	max := t.maxRTT()
	totalCalls := 0
	for _, s := range t.sections {
		totalCalls += len(s.calls)
	}
	for _, s := range t.sections {
		fmt.Fprintln(w, s.label)
		headers := []string{"CALL", "TIME"}
		if !plain && totalCalls > 1 {
			headers = append(headers, "BAR")
		}
		headers = append(headers, "SENT", "RECV")
		tt := table.New().Border(border(plain)).Headers(headers...)
		for _, c := range s.calls {
			recv := humanBytes(c.RespBytes)
			if c.Err != nil {
				recv = "-"
			}
			cells := []string{callLabel(c, plain), duration(c.RTT)}
			if !plain && totalCalls > 1 {
				cells = append(cells, bar(c.RTT, max))
			}
			cells = append(cells, humanBytes(c.ReqBytes), recv)
			tt.Row(cells...)
		}
		if err := format.WriteTable(w, tt); err != nil {
			return err
		}
	}
	if verbose {
		wire, sent, recv := t.Totals()
		// Client-internal time is the run's non-wire time: everything spent
		// in open+op that was not a recorded round trip (our marshal, render,
		// and the SDK's handshake-side work).
		internal := t.phases["open"] + t.phases["op"] - wire
		sep := " · "
		if plain {
			sep = ", "
		}
		fmt.Fprintf(w, "totals: wire %s%sinternal %s%ssent %s%srecv %s\n",
			duration(wire), sep, duration(internal), sep, humanBytes(sent), sep, humanBytes(recv))
	}
	return nil
}

// EmitJSON writes the structured trace document (phases in ms, sections with
// per-call rtt_ms/req_bytes/resp_bytes, totals incl. client-internal time).
func (t *Tracer) EmitJSON(w io.Writer) error {
	if t == nil {
		return nil
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	wire, sent, recv := t.Totals()
	doc := struct {
		Phases   map[string]float64 `json:"phases"`
		Sections []jsonSection      `json:"sections"`
		Totals   jsonTotals         `json:"totals"`
	}{
		Phases: map[string]float64{},
	}
	for name, d := range t.phases {
		doc.Phases[name] = float64(d.Microseconds()) / 1000
	}
	for _, s := range t.sections {
		js := jsonSection{Label: s.label}
		for _, c := range s.calls {
			jc := jsonCall{Method: c.Method, RTTms: float64(c.RTT.Microseconds()) / 1000, ReqBytes: c.ReqBytes, RespBytes: c.RespBytes}
			if c.Err != nil {
				jc.Err = c.Err.Error()
			}
			js.Calls = append(js.Calls, jc)
		}
		doc.Sections = append(doc.Sections, js)
	}
	doc.Totals = jsonTotals{
		WireMs:     float64(wire.Microseconds()) / 1000,
		InternalMs: float64((t.phases["open"] + t.phases["op"] - wire).Microseconds()) / 1000,
		SentBytes:  sent,
		RecvBytes:  recv,
	}
	return format.EmitJSON(w, doc)
}

type jsonSection struct {
	Label string     `json:"label"`
	Calls []jsonCall `json:"calls"`
}

type jsonCall struct {
	Method    string  `json:"method"`
	RTTms     float64 `json:"rtt_ms"`
	ReqBytes  int     `json:"req_bytes"`
	RespBytes int     `json:"resp_bytes"`
	Err       string  `json:"err,omitempty"`
}

type jsonTotals struct {
	WireMs     float64 `json:"wire_ms"`
	InternalMs float64 `json:"internal_ms"`
	SentBytes  int     `json:"sent_bytes"`
	RecvBytes  int     `json:"recv_bytes"`
}

// maxRTT is the largest call RTT across sections (bar scale); caller holds
// the mutex.
func (t *Tracer) maxRTT() time.Duration {
	var max time.Duration
	n := 0
	for _, s := range t.sections {
		for _, c := range s.calls {
			n++
			if c.RTT > max {
				max = c.RTT
			}
		}
	}
	if n <= 1 {
		return 0 // degenerate: a single call has no proportion to show
	}
	return max
}

// callLabel names a row: the handshake reads as the boot row (on stdio the
// initialize round trip includes the child's package resolution and boot —
// spawn itself never blocks), failures carry the mode's marker.
func callLabel(c Call, plain bool) string {
	name := c.Method
	switch name {
	case "initialize":
		name = "boot (initialize)"
	case "server/discover":
		name = "boot (discover)"
	}
	if c.Err != nil {
		if plain {
			return "! " + name
		}
		return "❌ " + name
	}
	return name
}

// duration formats adaptively at millisecond precision, matching the
// spinner's "done in 1.066s" idiom.
func duration(d time.Duration) string {
	return d.Round(time.Millisecond).String()
}

// humanBytes renders a byte count for table cells.
func humanBytes(n int) string {
	switch {
	case n < 1024:
		return fmt.Sprintf("%dB", n)
	case n < 1<<20:
		return fmt.Sprintf("%.1fKB", float64(n)/1024)
	default:
		return fmt.Sprintf("%.1fMB", float64(n)/(1<<20))
	}
}

// bar renders the Rich-mode proportion cell: filled cells ∝ rtt against max,
// the rest dim. Empty when there is no scale to show.
func bar(rtt, max time.Duration) string {
	if max <= 0 {
		return ""
	}
	filled := int(math.Round(float64(rtt) / float64(max) * float64(barWidth)))
	if filled < 1 {
		filled = 1
	}
	if filled > barWidth {
		filled = barWidth
	}
	return strings.Repeat("▓", filled) + strings.Repeat("░", barWidth-filled)
}

// approxBytes sizes a wire payload by marshaling it; a nil or unmarshalable
// value counts as 0. The wire frame itself (id, jsonrpc) is a few bytes of
// constant overhead and is not included.
func approxBytes(v any) int {
	if v == nil {
		return 0
	}
	b, err := json.Marshal(v)
	if err != nil {
		return 0
	}
	return len(b)
}

// border mirrors format's unexported helper: the ASCII border keeps --plain
// output byte-scannable, the Unicode border is the Rich default.
func border(plain bool) lipgloss.Border {
	if plain {
		return lipgloss.ASCIIBorder()
	}
	return lipgloss.NormalBorder()
}
