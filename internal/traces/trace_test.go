package traces_test

import (
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/dipjyotimetia/agent-knowledge-factory/internal/traces"
)

func TestArticleExampleMinesTheHumanHeaderAndRedactsPII(t *testing.T) {
	raw := []byte(`[
		{"gen_ai.prompt.0": "the column was renamed; suggested fix is use eventId"},
		{"gen_ai.human": "please add a report header: Movement activity. Contact ada@bank.example or 415-555-1212"}
	]`)
	doc, err := traces.ImportJSON(raw)
	if err != nil {
		t.Fatal(err)
	}
	asks := traces.HumanAsks(doc)
	blob := strings.Join(asks, "\n")
	if !strings.Contains(blob, "eventId") || !strings.Contains(blob, "Movement activity") {
		t.Fatalf("asks = %s", blob)
	}
	if strings.Contains(blob, "ada@bank.example") || strings.Contains(blob, "415-555-1212") {
		t.Fatalf("pii leaked: %s", blob)
	}
}

func Test24ImportRejectsAmbiguousAndUnboundedTraces(t *testing.T) {
	for name, raw := range map[string]string{
		"duplicate turns": `{"turns":[{"role":"user","text":"hello"}],"turns":[]}`,
		"duplicate key":   `[{"gen_ai.prompt":"one","gen_ai.prompt":"two"}]`,
		"ambiguous shape": `{"turns":[{"role":"user","text":"hello"}],"gen_ai.human":"hello"}`,
		"wrong role":      `{"turns":[{"role":"root","text":"hello"}]}`,
		"trailing bytes":  `[{"gen_ai.prompt":"hello"}] []`,
		"unknown secret":  `[{"gen_ai.prompt":"hello","other":"AKIAIOSFODNN7EXAMPLE"}]`,
		"deep value":      `[{"gen_ai.prompt":"hello","other":` + strings.Repeat("[", 256) + "null" + strings.Repeat("]", 256) + "}]",
		"too many turns":  "[" + strings.Repeat(`{"gen_ai.human":"hello"},`, 200) + `{"gen_ai.human":"hello"}]`,
		"oversized":       strings.Repeat(" ", traces.MaxBytes+1),
		"invalid UTF-8":   "[\xff]",
		"invalid escape":  `[{"gen_ai.human":"\ud800"}]`,
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := traces.ImportJSON([]byte(raw)); !errors.Is(err, traces.ErrInvalid) {
				t.Fatalf("unexpected error: %v", err)
			}
		})
	}
}

func Test24RecognizedSensitiveFieldsAreDiscarded(t *testing.T) {
	doc, err := traces.ImportJSON([]byte(`[{
	    "gen_ai.prompt.0":"first",
	    "Authorization":"opaque-credential-sentinel",
	    "gen_ai.human":"Authorization: Basic opaque-credential-sentinel",
	    "gen_ai.prompt.1.content":"Contact pii-sentinel@example.test or 415-555-1212.",
	    "gen_ai.completion":"assistant only"
	}]`))
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"first", "[redacted]", "Contact [redacted-email] or [redacted-phone]."}
	if got := traces.HumanAsks(doc); !slices.Equal(got, want) {
		t.Fatalf("unexpected sanitized turns: %q", got)
	}
}
