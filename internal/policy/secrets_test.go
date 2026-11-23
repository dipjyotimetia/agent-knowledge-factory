package policy

import "testing"

func TestScanSecrets(t *testing.T) {
	findings := ScanSecrets("observed", "the key is AKIAIOSFODNN7EXAMPLE and should not be stored")
	if len(findings) != 1 || findings[0].Rule != "aws-access-key" || findings[0].Field != "observed" {
		t.Fatalf("findings = %+v", findings)
	}
	if len(ScanSecrets("summary", "dedupe by eventId")) != 0 {
		t.Fatal("ordinary text was flagged")
	}
	if Redact("token=ghp_abcdefghijklmnopqrstuvwxyz") != "[redacted]" {
		t.Fatal("secret query was kept")
	}
	if Redact("dedupe by eventId") != "dedupe by eventId" {
		t.Fatal("ordinary query was redacted")
	}
}
