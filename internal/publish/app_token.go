package publish

import (
	"context"
	"crypto/rsa"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// AppCredentials are the GitHub App values from the runbook. The private key
// is never written to Git.
type AppCredentials struct {
	BaseURL        string
	AppID          string
	InstallationID string
	PrivateKeyPEM  string
	HTTP           *http.Client
}

// AppToken mints and caches an installation access token.
type AppToken struct {
	creds AppCredentials
	mu    sync.Mutex
	token string
	exp   time.Time
	now   func() time.Time
}

// NewAppToken returns a TokenSource that exchanges the app private key for an
// installation token.
func NewAppToken(creds AppCredentials) *AppToken {
	if creds.BaseURL == "" {
		creds.BaseURL = "https://api.github.com"
	}
	return &AppToken{creds: creds, now: time.Now}
}

// Token returns a cached installation token, minting a new one when it is
// within a minute of expiry.
func (a *AppToken) Token(ctx context.Context) (string, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	now := a.now()
	if a.token != "" && now.Add(time.Minute).Before(a.exp) {
		return a.token, nil
	}
	token, exp, err := a.mint(ctx, now)
	if err != nil {
		return "", err
	}
	a.token = token
	a.exp = exp
	return token, nil
}

func (a *AppToken) mint(ctx context.Context, now time.Time) (string, time.Time, error) {
	key, err := jwt.ParseRSAPrivateKeyFromPEM([]byte(a.creds.PrivateKeyPEM))
	if err != nil {
		return "", time.Time{}, fmt.Errorf("github app private key: %w", err)
	}
	signed, err := appJWT(a.creds.AppID, key, now)
	if err != nil {
		return "", time.Time{}, err
	}
	endpoint := strings.TrimRight(a.creds.BaseURL, "/") + "/app/installations/" + a.creds.InstallationID + "/access_tokens"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, nil)
	if err != nil {
		return "", time.Time{}, err
	}
	req.Header.Set("Authorization", "Bearer "+signed)
	req.Header.Set("Accept", "application/vnd.github+json")
	client := a.creds.HTTP
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(req)
	if err != nil {
		return "", time.Time{}, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", time.Time{}, err
	}
	if resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusOK {
		return "", time.Time{}, fmt.Errorf("github installation token: %d", resp.StatusCode)
	}
	var payload struct {
		Token     string    `json:"token"`
		ExpiresAt time.Time `json:"expires_at"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		return "", time.Time{}, err
	}
	if payload.Token == "" {
		return "", time.Time{}, fmt.Errorf("github installation token was empty")
	}
	if payload.ExpiresAt.IsZero() {
		payload.ExpiresAt = now.Add(50 * time.Minute)
	}
	return payload.Token, payload.ExpiresAt, nil
}

func appJWT(appID string, key *rsa.PrivateKey, now time.Time) (string, error) {
	claims := jwt.RegisteredClaims{
		Issuer:    appID,
		IssuedAt:  jwt.NewNumericDate(now.Add(-time.Minute)),
		ExpiresAt: jwt.NewNumericDate(now.Add(9 * time.Minute)),
	}
	return jwt.NewWithClaims(jwt.SigningMethodRS256, claims).SignedString(key)
}
