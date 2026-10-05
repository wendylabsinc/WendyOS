package commands

import (
	"crypto/tls"
	"crypto/x509"
	"net/url"
	"testing"

	"github.com/wendylabsinc/wendy/go/internal/cli/grpcclient"
	"github.com/wendylabsinc/wendy/go/internal/shared/certs"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/peer"
)

func TestUnenrollPeerUsesDirectTLSUUIDIdentity(t *testing.T) {
	principal, err := certs.ParsePrincipal(testUnenrollJournal().Principal)
	if err != nil {
		t.Fatal(err)
	}
	uri, _ := url.Parse(principal.Principal)
	conn := &grpcclient.AgentConnection{IsMTLS: true}
	remote := &peer.Peer{AuthInfo: credentials.TLSInfo{State: tls.ConnectionState{HandshakeComplete: true, PeerCertificates: []*x509.Certificate{{Raw: []byte("verified certificate DER"), URIs: []*url.URL{uri}}}}}}
	// UUID identities have legacy org=0 and no legacy observer entry. The
	// authenticated RPC transport still supplies the actual verified certificate.
	if _, observed := conn.ObservedServerIdentity(); observed {
		t.Fatal("unexpected numeric observer")
	}
	if fingerprint, err := directUnenrollPeerFingerprint(conn, remote, principal); err != nil || len(fingerprint) != 64 {
		t.Fatalf("UUID peer rejected: %v", err)
	}
	for _, kind := range []string{"plaintext", "proxy", "missing-peer", "wrong-principal", "incomplete-handshake"} {
		t.Run(kind, func(t *testing.T) {
			c := grpcclient.AgentConnection{IsMTLS: true}
			p := *remote
			id := principal
			switch kind {
			case "plaintext":
				c.IsMTLS = false
			case "proxy":
				c.IsSessionProxy = true
			case "missing-peer":
				p.AuthInfo = nil
			case "wrong-principal":
				id.Principal += "other"
			case "incomplete-handshake":
				state := p.AuthInfo.(credentials.TLSInfo)
				state.State.HandshakeComplete = false
				p.AuthInfo = state
			}
			if _, err := directUnenrollPeerFingerprint(&c, &p, id); err == nil {
				t.Fatal("unsafe peer accepted")
			}
		})
	}
}
