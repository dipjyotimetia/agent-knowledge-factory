// Package auth authenticates bearer tokens to a single workspace and role.
// A token never grants access to a second workspace.
package auth

import (
	"context"
	"crypto/subtle"
	"errors"
	"net/http"
	"strings"

	"github.com/dipjyotimetia/agent-knowledge-factory/internal/config"
	"github.com/dipjyotimetia/agent-knowledge-factory/internal/domain"
)

// Roles that the control API understands. None of them may merge a pull request.
const (
	RoleAgent    = "agent"
	RoleCurator  = "curator"
	RoleReviewer = "reviewer"
)

// Principal is the authenticated caller.
type Principal struct {
	WorkspaceID string
	Role        string
	ActorID     string
}

type principalKey struct{}

// FromContext returns the principal stored by Middleware.
func FromContext(ctx context.Context) (Principal, bool) {
	p, ok := ctx.Value(principalKey{}).(Principal)
	return p, ok
}

// Authenticator matches bearer tokens in constant time relative to each
// configured token's length. Token count is expected to stay small.
type Authenticator struct {
	tokens []config.Token
}

// New builds an authenticator. Empty token lists are allowed; every request
// then fails authentication.
func New(tokens []config.Token) (*Authenticator, error) {
	seen := map[string]struct{}{}
	for _, token := range tokens {
		if token.ActorID != "" && !domain.ValidActorID(token.ActorID) {
			return nil, errors.New("invalid token actor id")
		}
		if token.Token == "" {
			return nil, errors.New("empty token")
		}
		if _, ok := seen[token.Token]; ok {
			return nil, errors.New("duplicate token")
		}
		seen[token.Token] = struct{}{}
		if err := domain.ValidateWorkspaceID(token.WorkspaceID); err != nil {
			return nil, err
		}
	}
	return &Authenticator{tokens: tokens}, nil
}

// Authenticate returns the principal for a raw bearer token.
func (a *Authenticator) Authenticate(presented string) (Principal, bool) {
	for _, token := range a.tokens {
		if subtle.ConstantTimeCompare([]byte(token.Token), []byte(presented)) == 1 {
			return Principal{WorkspaceID: token.WorkspaceID, Role: token.Role, ActorID: token.ActorID}, true
		}
	}
	return Principal{}, false
}

// Middleware requires Authorization: Bearer <token>.
func (a *Authenticator) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		header := r.Header.Get("Authorization")
		scheme, value, ok := strings.Cut(header, " ")
		if !ok || !strings.EqualFold(scheme, "Bearer") || value == "" {
			http.Error(w, "authentication required", http.StatusUnauthorized)
			return
		}
		principal, ok := a.Authenticate(value)
		if !ok {
			http.Error(w, "authentication required", http.StatusUnauthorized)
			return
		}
		ctx := context.WithValue(r.Context(), principalKey{}, principal)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// RequireWorkspace rejects a caller-supplied workspace id that does not match
// the authenticated principal. An empty supplied id means "use the principal".
func RequireWorkspace(principal Principal, supplied string) error {
	if supplied == "" {
		return nil
	}
	if err := domain.ValidateWorkspaceID(supplied); err != nil {
		return err
	}
	if supplied != principal.WorkspaceID {
		return errors.New("workspace does not match credential")
	}
	return nil
}
