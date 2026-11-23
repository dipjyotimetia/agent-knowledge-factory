package publish_test

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/dipjyotimetia/agent-knowledge-factory/internal/publish"
	"github.com/golang-jwt/jwt/v5"
)

func TestAppTokenUsesInstallationExchange(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	pemBytes := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})
	var sawAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/app/installations/99/access_tokens") {
			t.Errorf("path %s", r.URL.Path)
		}
		sawAuth = r.Header.Get("Authorization")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"token":"ghs_installation","expires_at":"2026-11-23T05:00:00Z"}`))
	}))
	t.Cleanup(srv.Close)
	source := publish.NewAppToken(publish.AppCredentials{
		BaseURL: srv.URL, AppID: "123", InstallationID: "99", PrivateKeyPEM: string(pemBytes), HTTP: srv.Client(),
	})
	token, err := source.Token(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if token != "ghs_installation" {
		t.Fatalf("token = %s", token)
	}
	if !strings.HasPrefix(sawAuth, "Bearer ") {
		t.Fatalf("auth = %s", sawAuth)
	}
	parsed, err := jwt.Parse(strings.TrimPrefix(sawAuth, "Bearer "), func(token *jwt.Token) (any, error) {
		return &key.PublicKey, nil
	}, jwt.WithValidMethods([]string{jwt.SigningMethodRS256.Alg()}))
	if err != nil || !parsed.Valid {
		t.Fatalf("app jwt: %v", err)
	}
	claims := parsed.Claims.(jwt.MapClaims)
	if claims["iss"] != "123" {
		t.Fatalf("iss = %v", claims["iss"])
	}
	again, err := source.Token(context.Background())
	if err != nil || again != token {
		t.Fatalf("cached token = %s err=%v", again, err)
	}
	_ = time.Minute
}
