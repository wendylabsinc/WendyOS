package liteclient

import (
	"context"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"net/url"
	"strings"
	"testing"
	"time"
)

func TestRelayDeviceTrustFallbackPreservesVerification(t *testing.T) {
	operator, primaryRoots, primary := relayPKI(t, devicePrincipal, true)
	primaryLeaf, err := x509.ParseCertificate(primary.Certificates[0].Certificate[0])
	if err != nil {
		t.Fatal(err)
	}
	root, key := issueRelayCertificate(t, 10, "Lite Device Authority", "", true, nil, nil)
	device, deviceKey := issueRelayCertificate(t, 11, "board", devicePrincipal, false, root, key)
	roots := string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: root.Raw}))
	mutateLeaf := func(change func(*x509.Certificate)) *x509.Certificate {
		leaf := *device
		change(&leaf)
		der, err := x509.CreateCertificate(rand.Reader, &leaf, root, deviceKey.Public(), key)
		if err != nil {
			t.Fatal(err)
		}
		cert, err := x509.ParseCertificate(der)
		if err != nil {
			t.Fatal(err)
		}
		return cert
	}
	wrongPrincipal := func(value string) *x509.Certificate {
		return mutateLeaf(func(leaf *x509.Certificate) {
			u, err := url.Parse(value)
			if err != nil {
				t.Fatal(err)
			}
			leaf.URIs = []*url.URL{u}
		})
	}
	// The normal verifier reports UnknownAuthorityError for an invalid
	// signature under a named, known issuer too. That must not fetch new trust.
	brokenPrimary := *primaryLeaf
	brokenPrimary.Signature = append([]byte(nil), primaryLeaf.Signature...)
	brokenPrimary.Signature[len(brokenPrimary.Signature)-1] ^= 1
	brokenAlternate := *device
	brokenAlternate.Signature = append([]byte(nil), device.Signature...)
	brokenAlternate.Signature[len(brokenAlternate.Signature)-1] ^= 1
	for _, tc := range []struct {
		name      string
		leaf      *x509.Certificate
		bundle    string
		lookupErr error
		wantCalls int
		wantOK    bool
	}{
		{name: "existing authority", leaf: primaryLeaf, bundle: roots, wantOK: true},
		{name: "classical authority", leaf: device, bundle: roots, wantCalls: 1, wantOK: true},
		{name: "untrusted peer supplied root", leaf: device, bundle: primaryRoots, wantCalls: 1},
		{name: "wrong device", leaf: wrongPrincipal(strings.Replace(devicePrincipal, "441bf6804ff8", "other", 1)), bundle: roots},
		{name: "wrong tenant", leaf: wrongPrincipal(strings.Replace(devicePrincipal, "11111111-1111-4111-8111-111111111111", "33333333-3333-4333-8333-333333333333", 1)), bundle: roots},
		{name: "expired", leaf: mutateLeaf(func(leaf *x509.Certificate) { leaf.NotAfter = time.Now().Add(-time.Minute) }), bundle: roots},
		{name: "known issuer bad signature", leaf: &brokenPrimary, bundle: roots},
		{name: "discovered issuer bad signature", leaf: &brokenAlternate, bundle: roots, wantCalls: 1},
		{name: "discovery fails", leaf: device, lookupErr: errors.New("unavailable"), wantCalls: 1},
		{name: "malformed discovered roots", leaf: device, bundle: "invalid", wantCalls: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg, err := deviceTLSConfig(operator, primaryRoots, "441bf6804ff8")
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			calls := 0
			addRelayDeviceTrustFallback(ctx, cfg, operator, primaryRoots, "441bf6804ff8", func(got context.Context) (string, error) {
				calls++
				if got != ctx {
					t.Fatal("discovery lost the handshake context")
				}
				return tc.bundle, tc.lookupErr
			})
			err = cfg.VerifyConnection(tls.ConnectionState{PeerCertificates: []*x509.Certificate{tc.leaf, root}})
			if (err == nil) != tc.wantOK || calls != tc.wantCalls {
				t.Fatalf("verification = %v, discovery calls = %d; want accepted=%v, calls=%d", err, calls, tc.wantOK, tc.wantCalls)
			}
		})
	}
}
