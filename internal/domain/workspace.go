// Package domain holds pure knowledge-factory types and invariants.
package domain

import (
	"errors"
	"fmt"
	"regexp"
)

// ErrInvalidWorkspaceID is returned when a workspace id is empty or unsafe
// to interpolate into logs and URIs. The database enforces the same pattern.
var ErrInvalidWorkspaceID = errors.New("invalid workspace id")

var workspaceIDPattern = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9]|-[a-z0-9]){0,63}$`)

// ValidateWorkspaceID checks the canonical workspace identifier.
// Ids are 1–64 characters: lowercase letters, digits, and single hyphens.
func ValidateWorkspaceID(id string) error {
	if len(id) < 1 || len(id) > 64 || !workspaceIDPattern.MatchString(id) {
		return fmt.Errorf("%w: %q", ErrInvalidWorkspaceID, id)
	}
	return nil
}
