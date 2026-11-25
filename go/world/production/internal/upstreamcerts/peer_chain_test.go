// Extracted from Wendy f29c41dbb86e8d05a8562b52c86a2f5120ace076; package name changed.
package upstreamcerts

import (
	"crypto/x509"
	"encoding/asn1"
	"testing"
)

func TestPermitsUsageRejectsUnknownEKUs(t *testing.T) {
	for _, usage := range []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth, x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageAny} {
		cert := &x509.Certificate{ExtKeyUsage: []x509.ExtKeyUsage{usage}, UnknownExtKeyUsage: []asn1.ObjectIdentifier{{1, 2, 3, 4}}}
		if permitsUsage(cert, usage) {
			t.Fatal("mixed known and unknown EKUs accepted")
		}
		cert.UnknownExtKeyUsage = nil
		if !permitsUsage(cert, usage) {
			t.Fatal("known EKU rejected")
		}
	}
}
