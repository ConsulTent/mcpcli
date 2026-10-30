package jev

import (
	"encoding/json"
	"errors"
	"testing"
)

// TestOpKindString pins the canonical primitive names — they are the wire
// contract of the JSON document, so a rename here is a breaking change.
func TestOpKindString(t *testing.T) {
	tests := []struct {
		name string
		op   OpKind
		want string
	}{
		{"noul", OpKindNoul, "noul"},
		{"choice", OpKindChoice, "choice"},
		{"score", OpKindScore, "score"},
		{"verify", OpKindVerify, "verify"},
		{"screen", OpKindScreen, "screen"},
		{"gate", OpKindGate, "gate"},
		{"zero value", OpKindUnknown, "unknown"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.op.String(); got != tt.want {
				t.Fatalf("OpKind(%d).String() = %q, want %q", tt.op, got, tt.want)
			}
		})
	}
}

// TestOpKindMarshalJSON pins that verdicts carry primitive names, not ints.
func TestOpKindMarshalJSON(t *testing.T) {
	b, err := json.Marshal(OpKindChoice)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if string(b) != `"choice"` {
		t.Fatalf("marshal = %s, want \"choice\"", b)
	}
}

// TestOpKindUnmarshalJSON pins the round trip and the refusal of unknown
// names — the zero value must never masquerade as a real op.
func TestOpKindUnmarshalJSON(t *testing.T) {
	var op OpKind
	if err := json.Unmarshal([]byte(`"score"`), &op); err != nil || op != OpKindScore {
		t.Fatalf("unmarshal score = %v (err %v), want OpKindScore", op, err)
	}
	if err := json.Unmarshal([]byte(`"box"`), &op); err == nil {
		t.Fatal("unmarshal of an unknown name succeeded, want error")
	}
}

// TestIsAuthError pins the classification the CI lane depends on: recognized
// rejections match (token-bounded codes and phrases), everything a healthy
// server can produce does not — a false positive would silently skip a real
// battery failure, so the negative rows matter as much as the positive ones.
func TestIsAuthError(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"plain http 401", errors.New("provider returned 401"), true},
		{"http 401 unauthorized", errors.New("HTTP 401 Unauthorized"), true},
		{"payment required 402", errors.New("402 Payment Required from typesafe"), true},
		{"forbidden 403", errors.New("request failed with status 403"), true},
		{"invalid api key phrase", errors.New("invalid API key provided"), true},
		{"billing phrase", errors.New("your billing account is on hold"), true},
		{"unauthorized phrase", errors.New("user is unauthorized to access this resource"), true},
		// Negatives: numbers embedded in larger tokens must not match.
		{"elapsed ms contains 14012", errors.New("call took 14012ms"), false},
		{"id contains 4012", errors.New("request id 4012 not found"), false},
		{"url path segment 401abc", errors.New("fetch https://x.example/401abc failed"), false},
		{"rate limit 429", errors.New("429 Too Many Requests"), false},
		{"generic tool error", errors.New("connection reset by peer"), false},
		{"malformed json output", errors.New("invalid character 'z' looking for beginning of value"), false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := IsAuthError(tt.err); got != tt.want {
				t.Fatalf("IsAuthError(%v) = %v, want %v", tt.err, got, tt.want)
			}
		})
	}
}

// TestQuestionZeroValue documents that an untouched Question carries no
// threshold: Parse and Build treat Threshold as unset unless HasThreshold.
func TestQuestionZeroValue(t *testing.T) {
	var q Question
	if q.HasThreshold {
		t.Fatal("zero Question must not claim a threshold")
	}
	if q.Threshold != 0 {
		t.Fatalf("zero Question threshold = %v, want 0", q.Threshold)
	}
}
