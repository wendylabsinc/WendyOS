package liteclient

import (
	"context"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"net"
	"net/url"
	"strings"
	"testing"
	"time"
)

func TestLegacyTLS12DeviceIdentity(t *testing.T) {
	root, rootKey := issueRelayCertificate(t, 1, "root", "", true, nil, nil)
	user, userKey := issueRelayCertificate(t, 2, "user", "urn:wendy:org:2:user:42", false, root, rootKey)
	client := tls.Certificate{Certificate: [][]byte{user.Raw}, PrivateKey: userKey}
	chain := string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: root.Raw}))
	for _, tc := range []struct {
		name, commonName, uri string
		untrusted, expired    bool
		transitional          bool
		wantTLS               bool
	}{
		{name: "matching asset", uri: "urn:wendy:org:2:asset:606", wantTLS: true},
		{name: "legacy common name", commonName: "sh/wendy/2/606", wantTLS: true},
		{name: "wrong organization", uri: "urn:wendy:org:3:asset:606"},
		{name: "wrong asset", uri: "urn:wendy:org:2:asset:607"},
		{name: "wrong role", uri: "urn:wendy:org:2:user:606"},
		{name: "missing identity"},
		{name: "untrusted issuer", uri: "urn:wendy:org:2:asset:606", untrusted: true},
		{name: "expired device", uri: "urn:wendy:org:2:asset:606", expired: true},
		{name: "tenant device with legacy identity", uri: "urn:wendy:org:2:asset:606", transitional: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			issuer, issuerKey := root, rootKey
			if tc.untrusted {
				issuer, issuerKey = issueRelayCertificate(t, 3, "untrusted root", "", true, nil, nil)
			}
			name := tc.commonName
			if name == "" {
				name = "board"
			}
			server, serverKey := issueRelayCertificate(t, 4, name, tc.uri, false, issuer, issuerKey)
			if tc.expired || tc.transitional {
				template := *server
				if tc.expired {
					template.NotBefore = time.Now().Add(-2 * time.Hour)
					template.NotAfter = time.Now().Add(-time.Hour)
				}
				if tc.transitional {
					principal, err := url.Parse("spiffe://wendy.sh/tenant/11111111-1111-4111-8111-111111111111/service/asset-606")
					if err != nil {
						t.Fatal(err)
					}
					template.URIs = append(template.URIs, principal)
				}
				der, err := x509.CreateCertificate(rand.Reader, &template, issuer, serverKey.Public(), issuerKey)
				if err != nil {
					t.Fatal(err)
				}
				server, err = x509.ParseCertificate(der)
				if err != nil {
					t.Fatal(err)
				}
			}
			pool := x509.NewCertPool()
			pool.AddCert(root)
			listener, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			defer listener.Close()
			completed := make(chan error, 1)
			go func() {
				raw, err := listener.Accept()
				if err != nil {
					completed <- err
					return
				}
				defer raw.Close()
				_ = raw.SetDeadline(time.Now().Add(5 * time.Second))
				conn := tls.Server(raw, &tls.Config{
					MinVersion: tls.VersionTLS12, MaxVersion: tls.VersionTLS12,
					Certificates: []tls.Certificate{{Certificate: [][]byte{server.Raw}, PrivateKey: serverKey}},
					ClientCAs:    pool, ClientAuth: tls.RequireAndVerifyClientCert,
				})
				completed <- conn.Handshake()
			}()
			c := NewWendyLiteClient()
			err = c.ConnectWithLegacyAuthenticationContext(context.Background(), listener.Addr().String(), client, chain, 2, 606)
			serverErr := <-completed
			if tc.wantTLS {
				// The mock closes after TLS, before WendyCom. Both sides must
				// authenticate successfully before the client reaches that step.
				if serverErr != nil || err == nil || !strings.HasPrefix(err.Error(), "handshake:") {
					t.Fatalf("TLS=%v WendyCom=%v", serverErr, err)
				}
			} else if err == nil || !strings.HasPrefix(err.Error(), "connect (mTLS):") {
				t.Fatalf("untrusted or wrong device accepted: %v", err)
			}
		})
	}
}

func TestLegacyTLSRequiresUserAndTarget(t *testing.T) {
	root, rootKey := issueRelayCertificate(t, 1, "root", "", true, nil, nil)
	chain := string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: root.Raw}))
	for _, tc := range []struct {
		name, uri      string
		orgID, assetID int32
	}{
		{name: "missing organization", uri: "urn:wendy:org:2:user:42", assetID: 606},
		{name: "missing asset", uri: "urn:wendy:org:2:user:42", orgID: 2},
		{name: "wrong organization", uri: "urn:wendy:org:3:user:42", orgID: 2, assetID: 606},
		{name: "wrong role", uri: "urn:wendy:org:2:asset:42", orgID: 2, assetID: 606},
		{name: "missing identity", orgID: 2, assetID: 606},
		{name: "tenant operator", uri: "spiffe://wendy.sh/tenant/11111111-1111-4111-8111-111111111111/operator/test", orgID: 2, assetID: 606},
	} {
		t.Run(tc.name, func(t *testing.T) {
			leaf, key := issueRelayCertificate(t, 2, "client", tc.uri, false, root, rootKey)
			cfg, err := legacyDeviceTLSConfig(tls.Certificate{Certificate: [][]byte{leaf.Raw}, PrivateKey: key}, chain, tc.orgID, tc.assetID)
			if err == nil || cfg != nil {
				t.Fatalf("invalid legacy client or target accepted: %v", err)
			}
		})
	}
	if cfg, err := legacyDeviceTLSConfig(tls.Certificate{}, chain, 2, 606); err == nil || cfg != nil {
		t.Fatal("missing client certificate accepted")
	}
}

func TestLegacyLANHandshakeCancellation(t *testing.T) {
	root, rootKey := issueRelayCertificate(t, 1, "root", "", true, nil, nil)
	user, userKey := issueRelayCertificate(t, 2, "user", "urn:wendy:org:2:user:42", false, root, rootKey)
	cert := tls.Certificate{Certificate: [][]byte{user.Raw}, PrivateKey: userKey}
	chain := string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: root.Raw}))
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	completed := make(chan error, 1)
	go func() {
		completed <- NewWendyLiteClient().ConnectWithLegacyAuthenticationContext(ctx, listener.Addr().String(), cert, chain, 2, 606)
	}()
	conn, err := listener.Accept()
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	cancel()
	select {
	case err := <-completed:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("handshake error = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("legacy LAN TLS handshake ignored cancellation")
	}
}
