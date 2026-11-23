package workflows

import (
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"os"

	"go.temporal.io/sdk/client"
)

// TemporalConfig is how a process reaches Temporal.
// APIKey and the TLS files are optional. An API key turns on TLS.
// Cert and key together are the client certificate for Temporal Cloud mTLS.
type TemporalConfig struct {
	Address    string
	Namespace  string
	APIKey     string
	TLSCert    string
	TLSKey     string
	TLSCA      string
	ServerName string
}

// TemporalFromLookup reads the Temporal environment variables.
func TemporalFromLookup(lookup func(string) string) TemporalConfig {
	if lookup == nil {
		lookup = func(string) string { return "" }
	}
	return TemporalConfig{
		Address:    lookup("TEMPORAL_ADDRESS"),
		Namespace:  lookup("TEMPORAL_NAMESPACE"),
		APIKey:     lookup("TEMPORAL_API_KEY"),
		TLSCert:    lookup("TEMPORAL_TLS_CERT"),
		TLSKey:     lookup("TEMPORAL_TLS_KEY"),
		TLSCA:      lookup("TEMPORAL_TLS_CA"),
		ServerName: lookup("TEMPORAL_TLS_SERVER_NAME"),
	}
}

// Options builds a client configuration. It does not dial.
func (c TemporalConfig) Options() (client.Options, error) {
	if c.Address == "" {
		return client.Options{}, fmt.Errorf("TEMPORAL_ADDRESS is unset")
	}
	namespace := c.Namespace
	if namespace == "" {
		namespace = "default"
	}
	opts := client.Options{HostPort: c.Address, Namespace: namespace}
	if c.APIKey != "" {
		opts.Credentials = client.NewAPIKeyStaticCredentials(c.APIKey)
	}
	tlsConfig, err := c.tlsConfig()
	if err != nil {
		return client.Options{}, err
	}
	if tlsConfig != nil {
		opts.ConnectionOptions.TLS = tlsConfig
	}
	return opts, nil
}

// Dial connects to the Temporal frontend. The caller closes the client.
func Dial(cfg TemporalConfig) (client.Client, error) {
	opts, err := cfg.Options()
	if err != nil {
		return nil, err
	}
	return client.Dial(opts)
}

func (c TemporalConfig) tlsConfig() (*tls.Config, error) {
	if (c.TLSCert == "") != (c.TLSKey == "") {
		return nil, fmt.Errorf("TEMPORAL_TLS_CERT and TEMPORAL_TLS_KEY must be set together")
	}
	if c.TLSCert == "" && c.TLSCA == "" && c.ServerName == "" {
		return nil, nil
	}
	cfg := &tls.Config{MinVersion: tls.VersionTLS12, ServerName: c.ServerName}
	if c.TLSCert != "" {
		cert, err := tls.LoadX509KeyPair(c.TLSCert, c.TLSKey)
		if err != nil {
			return nil, fmt.Errorf("temporal client certificate: %w", err)
		}
		cfg.Certificates = []tls.Certificate{cert}
	}
	if c.TLSCA != "" {
		pem, err := os.ReadFile(c.TLSCA) // #nosec G304 -- path is the operator TEMPORAL_TLS_CA file, not request input
		if err != nil {
			return nil, fmt.Errorf("temporal CA: %w", err)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(pem) {
			return nil, fmt.Errorf("temporal CA contains no certificates")
		}
		cfg.RootCAs = pool
	}
	return cfg, nil
}
