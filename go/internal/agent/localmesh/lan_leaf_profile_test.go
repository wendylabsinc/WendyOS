package localmesh

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"math/big"
	"net/url"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/shared/meshid"
)

// Every fixture has the expected principal and a signature from the same
// trusted CA. Only the leaf profile differs from the enrolled device's.
func lanProfileCertificate(t *testing.T, principal string, usages []x509.ExtKeyUsage, ca bool) tls.Certificate {
	t.Helper()
	pki := testPKI()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	uri, err := url.Parse(principal)
	if err != nil {
		t.Fatal(err)
	}
	leaf := &x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()),
		NotBefore:    time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		URIs: []*url.URL{uri}, KeyUsage: x509.KeyUsageDigitalSignature,
		ExtKeyUsage: usages, BasicConstraintsValid: true, IsCA: ca,
	}
	if ca {
		leaf.KeyUsage |= x509.KeyUsageCertSign
	}
	der, err := x509.CreateCertificate(rand.Reader, leaf, pki.certificate, key.Public(), pki.key)
	if err != nil {
		t.Fatal(err)
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
}

func TestLANQUICRequiresClientAuthLeaf(t *testing.T) {
	for _, profile := range []struct {
		name   string
		usages []x509.ExtKeyUsage
		ca     bool
		valid  bool
	}{
		{name: "clientAuth only", usages: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}, valid: true},
		{name: "clientAuth and serverAuth", usages: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth, x509.ExtKeyUsageServerAuth}, valid: true},
		{name: "missing EKU"},
		{name: "any EKU only", usages: []x509.ExtKeyUsage{x509.ExtKeyUsageAny}},
		{name: "serverAuth only", usages: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}},
		{name: "CA leaf", usages: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}, ca: true},
	} {
		for _, side := range []string{"client", "server"} {
			t.Run(profile.name+"/"+side, func(t *testing.T) {
				a, b := testCredentials(t)
				client, err := a.PeerTLS(b.Device)
				if err != nil {
					t.Fatal(err)
				}
				server, err := lanServerTLS(b)
				if err != nil {
					t.Fatal(err)
				}
				if side == "client" {
					client.GetClientCertificate = nil
					client.Certificates = []tls.Certificate{lanProfileCertificate(t, a.Device.Principal(), profile.usages, profile.ca)}
				} else {
					server.GetCertificate, server.GetConfigForClient = nil, nil
					server.NextProtos = []string{LinkALPN}
					server.Certificates = []tls.Certificate{lanProfileCertificate(t, b.Device.Principal(), profile.usages, profile.ca)}
				}
				result := rootQUICRoundtrip(client, server)
				if (result.err == nil) != profile.valid {
					t.Fatalf("QUIC handshake error=%v, want accepted=%v", result.err, profile.valid)
				}
			})
		}
	}
}

func TestLANQUICResumptionRechecksLeafProfile(t *testing.T) {
	for _, profile := range []struct {
		name   string
		usages []x509.ExtKeyUsage
		ca     bool
	}{
		{name: "missing EKU"},
		{name: "any EKU only", usages: []x509.ExtKeyUsage{x509.ExtKeyUsageAny}},
		{name: "CA leaf", usages: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}, ca: true},
	} {
		for _, side := range []string{"client", "server"} {
			t.Run(profile.name+"/"+side, func(t *testing.T) {
				a, b := testCredentials(t)
				client, err := a.PeerTLSWithTickets(b.Device, LinkALPN, LinkQUICSessionScope)
				if err != nil {
					t.Fatal(err)
				}
				server, err := lanServerTLS(b)
				if err != nil {
					t.Fatal(err)
				}
				var strict, resumed atomic.Bool
				if side == "client" {
					client.GetClientCertificate = nil
					client.Certificates = []tls.Certificate{lanProfileCertificate(t, a.Device.Principal(), profile.usages, profile.ca)}
					verify := server.VerifyConnection
					server.VerifyConnection = func(state tls.ConnectionState) error {
						if !strict.Load() {
							return legacyLANProfileCheck(state.PeerCertificates, a.Device.Principal())
						}
						resumed.Store(state.DidResume)
						return verify(state)
					}
				} else {
					server.GetCertificate, server.GetConfigForClient = nil, nil
					server.NextProtos = []string{LinkALPN}
					server.Certificates = []tls.Certificate{lanProfileCertificate(t, b.Device.Principal(), profile.usages, profile.ca)}
					// Admit the old profile only for the first full handshake.
					// The production resumption hook retains its strict verifier.
					verifyPeer := client.VerifyPeerCertificate
					client.VerifyPeerCertificate = func(raw [][]byte, chains [][]*x509.Certificate) error {
						if strict.Load() {
							return verifyPeer(raw, chains)
						}
						leaf, err := x509.ParseCertificate(raw[0])
						if err != nil {
							return err
						}
						return legacyLANProfileCheck([]*x509.Certificate{leaf}, b.Device.Principal())
					}
					verify := client.VerifyConnection
					client.VerifyConnection = func(state tls.ConnectionState) error {
						resumed.Store(state.DidResume)
						return verify(state)
					}
				}
				// Seed a real session ticket using the previous permissive leaf
				// policy, then require the current policy on the restored chain.
				first := rootQUICRoundtrip(client, server)
				if first.err != nil || first.client.DidResume || first.server.DidResume {
					t.Fatalf("initial legacy-profile handshake: %+v", first)
				}
				strict.Store(true)
				if result := rootQUICRoundtrip(client, server); result.err == nil {
					t.Fatal("resumed QUIC connection accepted an invalid leaf profile")
				}
				if !resumed.Load() {
					t.Fatal("rejection did not exercise a resumed QUIC handshake")
				}
			})
		}
	}
}

// Reproduce the previous profile policy while retaining CA trust, validity,
// authentication usage and exact device identity for the initial handshake.
func legacyLANProfileCheck(chain []*x509.Certificate, principal string) error {
	roots := x509.NewCertPool()
	roots.AddCert(testPKI().certificate)
	if len(chain) == 0 {
		return errors.New("missing peer certificate")
	}
	if _, err := chain[0].Verify(x509.VerifyOptions{Roots: roots, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}}); err != nil {
		return err
	}
	peer, err := meshid.DeviceFromCert(chain[0])
	if err != nil {
		return err
	}
	if peer.Principal() != principal {
		return errors.New("unexpected device principal")
	}
	return nil
}
