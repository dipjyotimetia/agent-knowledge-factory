package traces_test

import (
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
