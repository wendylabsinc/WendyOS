package localmesh

import (
	"bytes"
	"crypto"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/mldsa"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"math/big"
	"net/url"
	"testing"
	"time"
)

func principalBoundCredentials(t *testing.T, principal string, key crypto.Signer) *Credentials {
	t.Helper()
	uri, err := url.Parse(principal)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	leaf := &x509.Certificate{SerialNumber: big.NewInt(now.UnixNano()), NotBefore: now.Add(-time.Hour), NotAfter: now.Add(time.Hour), URIs: []*url.URL{uri}, KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth, x509.ExtKeyUsageServerAuth}}
	ca := testPKI()
	der, err := x509.CreateCertificate(rand.Reader, leaf, ca.certificate, key.Public(), ca.key)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	credentials, err := NewCredentials(string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})), ca.chainPEM, string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER})))
	if err != nil {
		t.Fatal(err)
	}
	return credentials
}

func principalBoundDirectory(t *testing.T, credentials *Credentials, now time.Time) *Directory {
	t.Helper()
	cache, err := OpenIdentityCache("", DefaultCacheLimits(), credentials.Verify, now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := cache.Put(credentials.Certificate.Certificate, now); err != nil {
		t.Fatal(err)
	}
	directory, err := NewDirectory(credentials.Device.Tenant(), 8, cache)
	if err != nil {
		t.Fatal(err)
	}
	return directory
}

func TestManifestBindsPrincipalWhenTenantsTrustSameKeyAndDeviceUUID(t *testing.T) {
	for _, algorithm := range []string{"P256", "Ed25519", "RSA", "MLDSA65", "MLDSA87"} {
		t.Run(algorithm, func(t *testing.T) {
			var key crypto.Signer
			var err error
			switch algorithm {
			case "MLDSA65":
				key, err = mldsa.GenerateKey(mldsa.MLDSA65())
			case "MLDSA87":
				key, err = mldsa.GenerateKey(mldsa.MLDSA87())
			case "P256":
				key, err = ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
			case "Ed25519":
				_, key, err = ed25519.GenerateKey(rand.Reader)
			case "RSA":
				key, err = rsa.GenerateKey(rand.Reader, 2048)
			}
			if err != nil {
				t.Fatal(err)
			}
			a := principalBoundCredentials(t, testPrincipal(testTenantUUID, testDeviceBID), key)
			b := principalBoundCredentials(t, testPrincipal(testOtherTenant, testDeviceBID), key)
			if a.Device.MeshAddress() != b.Device.MeshAddress() || a.Device.UUID() != b.Device.UUID() {
				t.Fatal("test must isolate the tenant principal, not the key, UUID, or prefix")
			}
			now := time.Now()
			manifest := routeManifest(a.Device, now)
			proof, err := SignManifest(manifest, a.Certificate.Certificate, key, now)
			if err != nil {
				t.Fatal(err)
			}
			if changed, err := principalBoundDirectory(t, a, now).Accept(proof, now); err != nil || !changed {
				t.Fatalf("original principal rejected: changed=%v err=%v", changed, err)
			}
			relabelled := proof
			relabelled.Principal = b.Device.Principal()
			destination := principalBoundDirectory(t, b, now)
			if _, err := destination.Accept(relabelled, now); err == nil {
				t.Fatalf("same-key tenant relabeling did not fail signature verification: %v", err)
			}
			if len(destination.Snapshot(now)) != 0 || len(destination.RouteAuthorizations(now)) != 0 {
				t.Fatal("relabelled proof changed directory or routes")
			}
			valid, err := SignManifest(manifest, b.Certificate.Certificate, key, now)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(valid.Body, proof.Body) {
				t.Fatal("body differs; test did not isolate the full-principal signature binding")
			}
			if changed, err := destination.Accept(valid, now); err != nil || !changed {
				t.Fatalf("correctly signed second-tenant proof rejected: changed=%v err=%v", changed, err)
			}
		})
	}
}

func TestManifestV3PreimageIsUnambiguous(t *testing.T) {
	principal := testPrincipal(testTenantUUID, testDeviceBID)
	body := []byte("{ \"version\": 3 }")
	want := []byte("wendy-local-mesh/manifest/3\x00\x00\x00\x00\x69" + principal + string(body))
	if len(principal) != 105 {
		t.Fatal("fixed wire fixture principal length changed")
	}
	input, opts := signingInput(ed25519.PublicKey{}, principal, body)
	if !bytes.Equal(input, want) || opts.HashFunc() != crypto.Hash(0) {
		t.Fatal("Ed25519 preimage does not match domain, BE length, canonical principal, exact body")
	}
	hash := sha256.Sum256(want)
	for _, key := range []crypto.PublicKey{&ecdsa.PublicKey{}, &rsa.PublicKey{}} {
		input, opts := signingInput(key, principal, body)
		if !bytes.Equal(input, hash[:]) || opts.HashFunc() != crypto.SHA256 {
			t.Fatal("hashed preimage differs from the version-3 wire contract")
		}
	}
}

func TestManifestV3RejectsLegacyVersionsAndDomains(t *testing.T) {
	d, chain, key, device, now := directoryFixture(t)
	for _, version := range []int{1, 2} {
		m := routeManifest(device, now)
		m.Version = version
		if version == 1 {
			m.Route = nil
		}
		if _, err := SignManifest(m, chain, key, now); err == nil {
			t.Fatalf("signed legacy version %d", version)
		}
		body, _ := json.Marshal(m)
		legacy := SignedManifest{Principal: device.Principal(), Body: body, Signature: ed25519.Sign(key, append([]byte("wendy-local-mesh/manifest/2\x00"), body...))}
		if _, err := d.Accept(legacy, now); err == nil {
			t.Fatalf("accepted legacy version %d", version)
		}
	}
	m := routeManifest(device, now)
	body, _ := json.Marshal(m)
	for _, domain := range []string{"wendy-local-mesh/manifest/2\x00", "wendy-local-mesh/manifest/3\x00"} {
		wire := SignedManifest{Principal: device.Principal(), Body: body, Signature: ed25519.Sign(key, append([]byte(domain), body...))}
		if _, err := d.Accept(wire, now); err == nil {
			t.Fatal("accepted unbound signature without length-prefixed principal")
		}
	}
	if len(d.Snapshot(now)) != 0 || len(d.RouteAuthorizations(now)) != 0 {
		t.Fatal("legacy records changed presence or routing")
	}
}
