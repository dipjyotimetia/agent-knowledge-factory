package domain

import "testing"

func TestValidateWorkspaceID(t *testing.T) {
	ok := []string{"a", "banking-demo", "team1"}
	for _, id := range ok {
		if err := ValidateWorkspaceID(id); err != nil {
			t.Fatalf("ValidateWorkspaceID(%q) unexpected error: %v", id, err)
		}
	}
	bad := []string{"", "Banking", "has_underscore", "-leading", "trailing-", "a--b", "sp ace", "../x"}
	for _, id := range bad {
		if err := ValidateWorkspaceID(id); err == nil {
			t.Fatalf("ValidateWorkspaceID(%q) expected error", id)
		}
	}
}
