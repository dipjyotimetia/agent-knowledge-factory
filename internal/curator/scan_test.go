package curator_test

import (
	"testing"

	"github.com/dipjyotimetia/agent-knowledge-factory/internal/curator"
)

func TestScanClaimsFindsDisagreeingValues(t *testing.T) {
	files := map[string]string{
		"a.md": "External consumers must deduplicate by `movementId`.",
		"b.md": "External consumers must deduplicate by `eventId`.",
		"c.md": "status is `PENDING` and status is `POSTED`.",
	}
	got, err := curator.ScanClaims(files, curator.DefaultClaimPatterns())
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("conflicts = %+v", got)
	}
}
