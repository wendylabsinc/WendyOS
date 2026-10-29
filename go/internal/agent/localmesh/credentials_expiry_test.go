package localmesh

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net/url"
	"testing"
	"time"
)

func TestCredentialsExpiryUsesVerifiedPath(t *testing.T) {
	now := time.Now().Truncate(time.Second)
	type certificate struct {
		parsed      *x509.Certificate
		key         *ecdsa.PrivateKey
		pem, keyPEM string
	}
	serial := int64(0)
	mint := func(name, principal string, expiry time.Time, parent *certificate) certificate {
		t.Helper()
		key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		if err != nil {
			t.Fatal(err)
		}
		serial++
		template := &x509.Certificate{SerialNumber: big.NewInt(serial), Subject: pkix.Name{CommonName: name}, NotBefore: now.Add(-time.Hour), NotAfter: expiry, IsCA: principal == "", BasicConstraintsValid: true, KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth}}
		if template.IsCA {
			template.KeyUsage |= x509.KeyUsageCertSign
		} else {
			uri, err := url.Parse(principal)
			if err != nil {
				t.Fatal(err)
			}
			template.URIs = []*url.URL{uri}
		}
		issuer, signer := template, key
		if parent != nil {
			issuer, signer = parent.parsed, parent.key
		}
		der, err := x509.CreateCertificate(rand.Reader, template, issuer, key.Public(), signer)
		if err != nil {
			t.Fatal(err)
		}
		parsed, err := x509.ParseCertificate(der)
		if err != nil {
			t.Fatal(err)
		}
		keyDER, err := x509.MarshalECPrivateKey(key)
		if err != nil {
			t.Fatal(err)
		}
		return certificate{parsed, key, string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})), string(pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}))}
	}
	root := mint("root", "", now.Add(2*time.Hour), nil)
	issuer := mint("selected issuer", "", now.Add(30*time.Minute), &root)
	self := mint("self", testPrincipal(testTenantUUID, testDeviceAID), now.Add(time.Hour), &root)
	peer := mint("peer", testPrincipal(testTenantUUID, testDeviceBID), now.Add(time.Hour), &issuer)
	for _, selected := range []struct {
		name   string
		issuer certificate
		trust  string
	}{
		{"own intermediate", issuer, issuer.pem + root.pem},
		{"own root", root, root.pem},
	} {
		t.Run(selected.name, func(t *testing.T) {
			own := mint("own long leaf", testPrincipal(testTenantUUID, testDeviceAID), now.Add(3*time.Hour), &selected.issuer)
			for _, trust := range []string{selected.trust, own.pem + selected.trust} {
				credentials, err := NewCredentials(own.pem, trust, own.keyPEM)
				if err != nil {
					t.Fatal(err)
				}
				identity, err := credentials.Verify(credentials.Certificate.Certificate, now)
				if err != nil || !identity.NotAfter.Equal(selected.issuer.parsed.NotAfter) {
					t.Fatalf("own expiry=%v want=%v err=%v", identity.NotAfter, selected.issuer.parsed.NotAfter, err)
				}
				if _, err := credentials.Verify(credentials.Certificate.Certificate, selected.issuer.parsed.NotAfter.Add(time.Second)); err == nil {
					t.Fatal("local leaf bypassed expired issuer")
				}
			}
		})
	}
	for _, kind := range []string{"root", "intermediate"} {
		t.Run("expired own "+kind, func(t *testing.T) {
			var parent *certificate
			if kind == "intermediate" {
				parent = &root
			}
			expired := mint("expired own issuer", "", now.Add(-time.Minute), parent)
			own := mint("own leaf", testPrincipal(testTenantUUID, testDeviceAID), now.Add(time.Hour), &expired)
			trust := expired.pem
			if parent != nil {
				trust += parent.pem
			}
			for _, bundle := range []string{trust, own.pem + trust} {
				if _, err := NewCredentials(own.pem, bundle, own.keyPEM); err == nil {
					t.Fatal("own expired issuer accepted at startup")
				}
			}
		})
	}
	for _, tc := range []struct {
		name    string
		root    bool
		expires time.Time
	}{
		{"expired unused root", true, now.Add(-time.Minute)},
		{"soon expiring unused root", true, now.Add(time.Minute)},
		{"expired unused issuer", false, now.Add(-time.Minute)},
		{"soon expiring unused issuer", false, now.Add(time.Minute)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			unused := mint("unused", "", tc.expires, nil)
			trust, chain := root.pem, [][]byte{peer.parsed.Raw, issuer.parsed.Raw}
			if tc.root {
				trust += unused.pem
			} else {
				chain = append(chain, unused.parsed.Raw)
			}
			credentials, err := NewCredentials(self.pem, trust, self.keyPEM)
			if err != nil {
				t.Fatal(err)
			}
			identity, err := credentials.Verify(chain, now)
			if err != nil || !identity.NotAfter.Equal(issuer.parsed.NotAfter) {
				t.Fatalf("identity=%+v err=%v", identity, err)
			}
			if _, err := credentials.Verify(append(append([][]byte{}, chain...), []byte("invalid DER")), now); err == nil {
				t.Fatal("malformed unused certificate accepted")
			}
			active := credentials.Verify
			cache, err := OpenIdentityCache("", DefaultCacheLimits(), func(chain [][]byte, now time.Time) (Identity, error) { return active(chain, now) }, now)
			if err != nil {
				t.Fatal(err)
			}
			principal, err := cache.Put(chain, now)
			if err != nil {
				t.Fatal(err)
			}
			if _, _, ok := cache.Get(principal, now.Add(2*time.Minute)); !ok {
				t.Fatal("unused certificate shortened cached identity")
			}
			if _, _, ok := cache.Get(principal, issuer.parsed.NotAfter.Add(time.Second)); ok {
				t.Fatal("selected issuer expiry was ignored")
			}
			if _, err := cache.Put(chain, now); err != nil {
				t.Fatal(err)
			}
			newRoot := mint("replacement trust", "", now.Add(time.Hour), nil)
			newSelf := mint("new self", testPrincipal(testTenantUUID, testDeviceAID), now.Add(time.Hour), &newRoot)
			replacement, err := NewCredentials(newSelf.pem, newRoot.pem, newSelf.keyPEM)
			if err != nil {
				t.Fatal(err)
			}
			active = replacement.Verify
			if _, _, ok := cache.Get(principal, now); ok {
				t.Fatal("lookup retained removed trust")
			}
		})
	}
}
