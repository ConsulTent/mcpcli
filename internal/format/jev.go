package format

import (
	"fmt"
	"io"
	"maps"
	"slices"
	"strconv"
	"strings"

	"github.com/ConsulTent/mcpcli/internal/jev"
)

// JevVerdicts renders one line per verdict, data-driven: a field renders only
// when the server supplied it, and plain mode swaps the middle-dot separator
// for "|" so the output stays byte-ASCII. Rich and plain share the same line
// shapes — escalation is data (exit semantics live in the caller).
func JevVerdicts(w io.Writer, verdicts []jev.Verdict, plain bool) error {
	sep := " · "
	if plain {
		sep = " | "
	}
	for _, v := range verdicts {
		var b strings.Builder
		b.WriteString(v.Type.String())
		b.WriteByte(' ')
		switch a := v.Answer.(type) {
		case float64:
			b.WriteString(formatFloat(a))
		case string:
			b.WriteString(a)
		default:
			// Parse enforces per-op answer typing, so this arm is
			// unreachable for parsed verdicts.
			b.WriteString("(unrenderable answer)")
		}
		if v.Label != "" {
			b.WriteString(" " + v.Label)
		}
		if v.Confidence != nil {
			b.WriteString(sep + "confidence " + formatFloat(*v.Confidence))
		}
		if len(v.Probabilities) > 0 {
			b.WriteString(sep + joinProbabilities(v.Probabilities))
		}
		if len(v.Legend) > 0 {
			b.WriteString(sep + joinLegend(v.Legend))
		}
		if v.SafeToApply != nil {
			b.WriteString(sep + "safe_to_apply " + formatFloat(*v.SafeToApply))
		}
		if len(v.ReasonCodes) > 0 {
			b.WriteString(sep + "reason_codes " + strings.Join(v.ReasonCodes, ","))
		}
		if v.Escalate {
			b.WriteString(sep + "ESCALATE")
			if v.Reason != "" {
				b.WriteString(" (" + v.Reason + ")")
			}
			if v.Hint != "" {
				b.WriteString(": " + v.Hint)
			}
		} else if v.Hint != "" {
			b.WriteString(sep + v.Hint)
		}
		b.WriteByte('\n')
		if _, err := fmt.Fprint(w, b.String()); err != nil {
			return fmt.Errorf("write verdict: %w", err)
		}
	}
	return nil
}

// formatFloat renders a number without trailing zeros (0.97, not 0.970000).
func formatFloat(f float64) string {
	return strconv.FormatFloat(f, 'g', -1, 64)
}

// joinProbabilities renders a distribution deterministically: keys sorted,
// each as "key p" — map iteration order must never reach the output.
func joinProbabilities(p map[string]float64) string {
	parts := make([]string, 0, len(p))
	for _, k := range slices.Sorted(maps.Keys(p)) {
		parts = append(parts, k+" "+formatFloat(p[k]))
	}
	return strings.Join(parts, " / ")
}

// joinLegend renders a score legend as sorted index=label pairs.
func joinLegend(l map[string]string) string {
	parts := make([]string, 0, len(l))
	for _, k := range slices.Sorted(maps.Keys(l)) {
		parts = append(parts, k+"="+l[k])
	}
	return "legend " + strings.Join(parts, ", ")
}
