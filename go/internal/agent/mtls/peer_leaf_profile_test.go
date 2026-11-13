package mtls

import (
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/asn1"
	"testing"
	"time"
)

func TestMLDSAPeerVerifierRequiresClientAuthLeaf(t *testing.T) {
	root, key := buildMLDSACACert(t, pkix.Name{CommonName: "profile test CA"}, true)
	base := buildMLDSALeafCert(t, root, key)
	for _, profile := range []struct {
		name  string
		oids  []asn1.ObjectIdentifier
		ca    bool
		valid bool
	}{
		{name: "clientAuth only", oids: []asn1.ObjectIdentifier{{1, 3, 6, 1, 5, 5, 7, 3, 2}}, valid: true},
		{name: "clientAuth and serverAuth", oids: []asn1.ObjectIdentifier{{1, 3, 6, 1, 5, 5, 7, 3, 2}, {1, 3, 6, 1, 5, 5, 7, 3, 1}}, valid: true},
		{name: "missing EKU"},
		{name: "any EKU only", oids: []asn1.ObjectIdentifier{{2, 5, 29, 37, 0}}},
		{name: "serverAuth only", oids: []asn1.ObjectIdentifier{{1, 3, 6, 1, 5, 5, 7, 3, 1}}},
		{name: "CA leaf", oids: []asn1.ObjectIdentifier{{1, 3, 6, 1, 5, 5, 7, 3, 2}}, ca: true},
	} {
		t.Run(profile.name, func(t *testing.T) {
			leaf := signPeerTestCertificate(t, base, root, key, func(c *tbsCertificate) {
				var extensions []pkix.Extension
				for _, ext := range c.Extensions {
					if !ext.Id.Equal(asn1.ObjectIdentifier{2, 5, 29, 37}) {
						extensions = append(extensions, ext)
					}
				}
				if len(profile.oids) != 0 {
					ext, err := buildEKUExt(profile.oids)
					if err != nil {
						t.Fatal(err)
					}
					extensions = append(extensions, ext)
				}
				if profile.ca {
					ext, err := buildBasicConstraintsExt(true)
					if err != nil {
						t.Fatal(err)
					}
					extensions = append(extensions, ext)
				}
				c.Extensions = extensions
			})
			for _, fallback := range []bool{false, true} {
				verify, err := NewPeerCertificateVerifier(peerTrustPEM(root), nil, time.Time{})
				if err != nil {
					t.Fatal(err)
				}
				if fallback {
					// Force the supplementary verifier even on Go versions with
					// native ML-DSA support, using the same strict leaf policy.
					verify = buildPeerCertificateVerifierWithCache(x509.NewCertPool(), []*x509.Certificate{root}, nil, time.Time{}, newPeerPathCache())
				}
				for attempt := 0; attempt < 2; attempt++ {
					_, err := verify([][]byte{leaf.Raw}, time.Now())
					if (err == nil) != profile.valid {
						t.Fatalf("fallback=%v attempt=%d: error=%v, want accepted=%v", fallback, attempt, err, profile.valid)
					}
				}
			}
		})
	}
}
