package workflows

import (
	"strings"
	"testing"

	"github.com/dipjyotimetia/agent-knowledge-factory/internal/curator"
)

func TestHumanLinesKeepsOnlyReviewerAdditions(t *testing.T) {
	proposal := curator.Proposal{Edits: []curator.Edit{{
		Path: "skills/a/SKILL.md",
		Old:  "deduplicate by `movementId`",
		New:  "deduplicate by `eventId`",
	}}}
	original := map[string]string{"skills/a/SKILL.md": "deduplicate by `movementId`\n"}
	merged := map[string]string{"skills/a/SKILL.md": "deduplicate by `eventId`\nREVIEWED_SENTENCE\n"}
	lines := HumanLines(proposal, original, merged)
	if len(lines) != 1 || lines[0] != "REVIEWED_SENTENCE" {
		t.Fatalf("lines = %#v", lines)
	}
	if strings.Contains(strings.Join(lines, " "), "eventId") {
		t.Fatal("curator line was treated as a human addition")
	}
}
