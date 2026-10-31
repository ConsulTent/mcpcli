package trace

import (
	"bytes"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestNilTracerSafe(t *testing.T) {
	var tr *Tracer
	tr.SetPhase("build", time.Second)
	tr.StartSection("connect")
	tr.Record("tools/list", time.Millisecond, 10, 20, nil)
	if err := tr.Render(&bytes.Buffer{}, false, false); err != nil {
		t.Fatalf("Render on nil tracer: %v", err)
	}
	if err := tr.EmitJSON(&bytes.Buffer{}); err != nil {
		t.Fatalf("EmitJSON on nil tracer: %v", err)
	}
}

func TestRecordSkipsNotifications(t *testing.T) {
	tr := New(false)
	tr.StartSection("connect")
	tr.Record("notifications/initialized", time.Microsecond, 5, 0, nil)
	tr.Record("tools/list", time.Millisecond, 10, 20, nil)
	if got := len(tr.sections[0].calls); got != 1 {
		t.Fatalf("calls = %d, want 1 (notification skipped)", got)
	}
}

func TestRecordConcurrent(t *testing.T) {
	tr := New(false)
	tr.StartSection("connect")
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			tr.Record("tools/list", time.Millisecond, 1, 1, nil)
		}()
	}
	wg.Wait()
	if got := len(tr.sections[0].calls); got != 50 {
		t.Fatalf("calls = %d, want 50 under concurrency", got)
	}
}

func TestTotalsMath(t *testing.T) {
	tr := New(false)
	tr.StartSection("connect")
	tr.Record("initialize", 900*time.Millisecond, 100, 200, nil)
	tr.Record("tools/list", 100*time.Millisecond, 10, 20, nil)
	wire, sent, recv := tr.Totals()
	if wire != time.Second {
		t.Fatalf("wire = %v, want 1s", wire)
	}
	if sent != 110 || recv != 220 {
		t.Fatalf("sent=%d recv=%d, want 110/220", sent, recv)
	}
}

func TestRenderRich(t *testing.T) {
	tr := New(false)
	tr.StartSection("connect")
	tr.Record("initialize", 1013*time.Millisecond, 2048, 4096, nil)
	tr.Record("tools/list", 18*time.Millisecond, 120, 3400, nil)
	tr.Record("tools/call", 5*time.Millisecond, 10, 0, errString("boom"))
	var out bytes.Buffer
	if err := tr.Render(&out, false /* rich */, false); err != nil {
		t.Fatal(err)
	}
	s := out.String()
	for _, want := range []string{
		"connect", "CALL", "TIME", "BAR", "SENT", "RECV",
		"boot (initialize)", "1.013s", "18ms",
		"▓",                       // proportion bar
		"2.0KB", "4.0KB", "3.3KB", // humanized bytes
		"❌ tools/call", // failed row marker (rich)
		"-",            // no recv on failure
	} {
		if !strings.Contains(s, want) {
			t.Fatalf("rich render missing %q:\n%s", want, s)
		}
	}
}

func TestRenderPlainASCII(t *testing.T) {
	tr := New(false)
	tr.StartSection("connect")
	tr.Record("initialize", 1013*time.Millisecond, 2048, 4096, nil)
	tr.Record("tools/list", 18*time.Millisecond, 120, 3400, nil)
	tr.Record("tools/call", 5*time.Millisecond, 10, 0, errString("boom"))
	var out bytes.Buffer
	if err := tr.Render(&out, true /* plain */, true /* verbose */); err != nil {
		t.Fatal(err)
	}
	s := out.String()
	if !strings.Contains(s, "! tools/call") {
		t.Fatalf("plain render missing ASCII failure prefix:\n%s", s)
	}
	if strings.Contains(s, "BAR") || strings.Contains(s, "▓") || strings.Contains(s, "❌") || strings.Contains(s, "·") {
		t.Fatalf("plain render must carry no bars, emoji, or non-ASCII separators:\n%s", s)
	}
	if !strings.Contains(s, "totals:") {
		t.Fatalf("verbose plain render missing totals:\n%s", s)
	}
	for b := range s {
		if s[b] > 0x7E {
			t.Fatalf("plain render has non-ASCII byte %q at %d:\n%s", s[b], b, s)
		}
	}
}

func TestVerboseTotalsLine(t *testing.T) {
	tr := New(true)
	tr.StartSection("connect")
	tr.Record("tools/list", 100*time.Millisecond, 10, 20, nil)
	tr.SetPhase("op", 150*time.Millisecond)
	var out bytes.Buffer
	if err := tr.Render(&out, false, true); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "totals:") || !strings.Contains(out.String(), "50ms") { // 150ms op − 100ms wire
		t.Fatalf("totals line missing internal split:\n%s", out.String())
	}
	out.Reset()
	if err := tr.Render(&out, false, false); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out.String(), "totals:") {
		t.Fatalf("non-verbose render must omit totals:\n%s", out.String())
	}
}

func TestEmitJSONShape(t *testing.T) {
	tr := New(true)
	tr.SetPhase("build", 10*time.Millisecond)
	tr.StartSection("connect")
	tr.Record("initialize", 900*time.Millisecond, 100, 200, nil)
	tr.Record("tools/call", 5*time.Millisecond, 10, 0, errString("boom"))
	tr.SetPhase("op", 950*time.Millisecond)
	var out bytes.Buffer
	if err := tr.EmitJSON(&out); err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Phases   map[string]float64 `json:"phases"`
		Sections []struct {
			Label string `json:"label"`
			Calls []struct {
				Method    string  `json:"method"`
				RTTms     float64 `json:"rtt_ms"`
				ReqBytes  int     `json:"req_bytes"`
				RespBytes int     `json:"resp_bytes"`
				Err       string  `json:"err"`
			} `json:"calls"`
		} `json:"sections"`
		Totals struct {
			WireMs     float64 `json:"wire_ms"`
			InternalMs float64 `json:"internal_ms"`
			SentBytes  int     `json:"sent_bytes"`
			RecvBytes  int     `json:"recv_bytes"`
		} `json:"totals"`
	}
	if err := json.Unmarshal(out.Bytes(), &doc); err != nil {
		t.Fatalf("trace doc: %v\n%s", err, out.String())
	}
	if doc.Phases["build"] != 10 || doc.Phases["op"] != 950 {
		t.Fatalf("phases = %v", doc.Phases)
	}
	calls := doc.Sections[0].Calls
	if len(calls) != 2 || calls[0].Method != "initialize" || calls[0].RTTms != 900 || calls[1].Err != "boom" {
		t.Fatalf("calls = %+v", calls)
	}
	if doc.Totals.WireMs != 905 || doc.Totals.SentBytes != 110 || doc.Totals.RecvBytes != 200 {
		t.Fatalf("totals = %+v", doc.Totals)
	}
	if doc.Totals.InternalMs != 45 { // 950ms op − 905ms wire
		t.Fatalf("internal_ms = %v, want 45", doc.Totals.InternalMs)
	}
}

func TestHumanBytes(t *testing.T) {
	cases := map[int]string{0: "0B", 512: "512B", 2048: "2.0KB", 3400: "3.3KB", 5 << 20: "5.0MB"}
	for in, want := range cases {
		if got := humanBytes(in); got != want {
			t.Fatalf("humanBytes(%d) = %q, want %q", in, got, want)
		}
	}
}

func TestBarScale(t *testing.T) {
	if got := bar(time.Second, time.Second); got != "▓▓▓▓▓▓▓▓▓▓▓▓" {
		t.Fatalf("full bar = %q", got)
	}
	if got := bar(0, 0); got != "" { // degenerate: no scale
		t.Fatalf("degenerate bar = %q", got)
	}
	got := bar(250*time.Millisecond, time.Second)
	if len([]rune(got)) != 12 || !strings.HasPrefix(got, "▓▓▓") || !strings.HasSuffix(got, "░░░") {
		t.Fatalf("quarter bar = %q", got)
	}
}

type errString string

func (e errString) Error() string { return string(e) }
