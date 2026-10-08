package cloudmcp

import (
	"crypto/tls"
	"crypto/x509"
	_ "embed"
	"fmt"
	"net/http"
	"strings"
)

// OpenAI publishes this intermediate for ChatGPT's managed connector identity.
// Trust the issuing CA, not a rotating leaf. Retrieved from
// https://developers.openai.com/plugins/mtls/openai-connectors-mtls-ca.pem.
//
//go:embed trust/openai-connectors-mtls-ca.pem
var openAIConnectorCA []byte

const OpenAIConnectorName = "mtls.prod.connectors.openai.com"

// ClientTransport validates the connector independently of the user's OAuth token.
// caPEM and dnsName must either both be empty (OpenAI) or both be configured
// (self-hosted connector). Public metadata does not require a client certificate.
func ClientTransport(caPEM []byte, dnsName string) (*tls.Config, func(http.Handler) http.Handler, error) {
	if len(caPEM) == 0 && dnsName == "" {
		caPEM, dnsName = openAIConnectorCA, OpenAIConnectorName
	}
	if len(caPEM) == 0 || dnsName == "" || strings.ContainsAny(dnsName, "*/\\ \t\r\n") {
		return nil, nil, fmt.Errorf("connector CA and exact DNS identity are required together")
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(caPEM) {
		return nil, nil, fmt.Errorf("invalid connector CA")
	}
	verify := func(state tls.ConnectionState) error {
		if len(state.PeerCertificates) == 0 {
			return fmt.Errorf("connector certificate required")
		}
		leaf := state.PeerCertificates[0]
		exact, clientAuth := false, false
		for _, name := range leaf.DNSNames {
			exact = exact || name == dnsName
		}
		for _, usage := range leaf.ExtKeyUsage {
			clientAuth = clientAuth || usage == x509.ExtKeyUsageClientAuth
		}
		if !exact || !clientAuth {
			return fmt.Errorf("invalid connector identity or purpose")
		}
		intermediates := x509.NewCertPool()
		for _, cert := range state.PeerCertificates[1:] {
			intermediates.AddCert(cert)
		}
		_, err := leaf.Verify(x509.VerifyOptions{Roots: roots, Intermediates: intermediates, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}})
		return err
	}
	config := &tls.Config{MinVersion: tls.VersionTLS13, ClientAuth: tls.RequestClientCert,
		// An empty acceptable-CA list allows the connector to select its own chain.
		// Verification is still mandatory for every authenticated route below.
		VerifyConnection: func(state tls.ConnectionState) error {
			if len(state.PeerCertificates) == 0 {
				return nil
			}
			return verify(state)
		},
	}
	middleware := func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			public := r.Method == http.MethodGet && (r.URL.Path == "/healthz" || strings.HasPrefix(r.URL.Path, "/.well-known/oauth-protected-resource/"))
			if !public && (r.TLS == nil || verify(*r.TLS) != nil) {
				http.Error(w, "connector authentication required", http.StatusUnauthorized)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
	return config, middleware, nil
}
