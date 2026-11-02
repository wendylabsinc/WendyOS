package meshoperatorlink

import (
	"crypto/x509"
	"testing"
	"time"
)

func TestIssuerCompletionRetainsWireAndBounds(t *testing.T) {
	leaf := &x509.Certificate{Raw: []byte("original TLS certificate")}
	for name, candidates := range map[string]IssuerCandidates{
		"drops presentation": func([][]byte, time.Time) [][]byte { return nil },
		"replaces presentation": func(wire [][]byte, _ time.Time) [][]byte {
			wire[0] = []byte("different certificate")
			return wire
		},
		"edits presentation in place": func(wire [][]byte, _ time.Time) [][]byte {
			wire[0][0] ^= 1
			return wire
		},
		"oversized bundle": func(wire [][]byte, _ time.Time) [][]byte {
			return append(wire, make([][]byte, 16)...)
		},
		"malformed candidate": func(wire [][]byte, _ time.Time) [][]byte {
			return append(wire, []byte("not a certificate"))
		},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := completePeerCertificates([]*x509.Certificate{leaf}, candidates, time.Now()); err == nil {
				t.Fatal("invalid completion accepted")
			}
			if string(leaf.Raw) != "original TLS certificate" {
				t.Fatal("completion modified the original TLS certificate")
			}
		})
	}
	for name, candidates := range map[string]IssuerCandidates{
		"legacy":    nil,
		"unchanged": func(wire [][]byte, _ time.Time) [][]byte { return wire },
	} {
		t.Run(name, func(t *testing.T) {
			chain, err := completePeerCertificates([]*x509.Certificate{leaf}, candidates, time.Now())
			if err != nil || len(chain) != 1 || chain[0] != leaf {
				t.Fatalf("wire presentation changed: %v, %v", chain, err)
			}
		})
	}
}
