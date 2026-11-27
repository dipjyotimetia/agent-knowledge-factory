package domain

import (
	"regexp"
	"strings"

	"github.com/google/uuid"
)

var actorPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.@-]{0,127}$`)

func ValidActorID(id string) bool {
	return actorPattern.MatchString(id)
}

// TraceID accepts the exact wire form and returns the UUID portion.
func TraceID(ref string) (string, bool) {
	id, ok := strings.CutPrefix(ref, "trace:")
	if !ok || len(id) != 36 {
		return "", false
	}
	parsed, err := uuid.Parse(id)
	return id, err == nil && parsed != uuid.Nil && parsed.String() == id
}
