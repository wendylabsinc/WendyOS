package localmesh

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"math/big"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/agent/netgossip"
	"github.com/wendylabsinc/wendy/go/internal/agent/netwire"
	"github.com/wendylabsinc/wendy/go/internal/shared/meshid"
)

func PublicationLossCredentials(t *testing.T) []*Credentials {
	t.Helper()
	var result []*Credentials
	for _, id := range []string{testDeviceAID, testDeviceBID, testDeviceCID, "2b1c0000-0000-4000-8000-000000000480"} {
		result = append(result, testCredentialsFor(t, testPrincipal(testTenantUUID, id)))
	}
	return result
}

func RotatePublicationLossKey(t *testing.T, old *Credentials) *Credentials {
	t.Helper()
	leaf, err := x509.ParseCertificate(old.Certificate.Certificate[0])
	if err != nil {
		t.Fatal(err)
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	leaf.SerialNumber = big.NewInt(time.Now().UnixNano())
	// Keep the original validity start so the current-leaf check must reject
	// obsolete signatures by key binding, not merely by issuance chronology.
	leaf.NotAfter = leaf.NotAfter.Add(time.Hour)
	pki := testPKI()
	der, err := x509.CreateCertificate(rand.Reader, leaf, pki.certificate, key.Public(), pki.key)
	if err != nil {
		t.Fatal(err)
	}
	private, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	renewed, err := NewCredentials(string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})), pki.chainPEM,
		string(pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: private})))
	if err != nil {
		t.Fatal(err)
	}
	return renewed
}

type publicationLossLink struct {
	*streamNodeLink
	ctx    context.Context
	filter func(context.Context, ControlMessage, string) bool
}

func (l *publicationLossLink) WriteControl(message ControlMessage) error {
	if l.filter != nil && l.filter(l.ctx, message, message.origin) {
		return nil // Deliberately lose this publication at the final export boundary.
	}
	return l.streamNodeLink.WriteControl(message)
}

// Test-only final-export loss hook. Keep the production AttachStream TLS and
// hello checks before wrapping its real stream link; all unfiltered control,
// packet framing, scheduler, gossip and Babel I/O remain production paths.
func AttachPublicationLossStream(ctx context.Context, node *Node, peer meshid.Device, conn *tls.Conn,
	filter func(context.Context, ControlMessage, string) bool) error {
	defer conn.Close()
	if err := node.verifyPeerCertificates(peer, conn.ConnectionState().PeerCertificates); err != nil {
		return err
	}
	bound, err := openStreamHelloPeer(ctx, conn, node.Credentials.Device, peer, StreamALPN)
	if err != nil {
		return err
	}
	link := &publicationLossLink{streamNodeLink: newStreamNodeLink(conn), ctx: ctx, filter: filter}
	return node.attachLink(WithCarrier(ctx, "tls-stream"), bound, 256, link)
}

func PublicationLossCurrentLeaf(node *Node, principal string) []byte {
	chain, _, ok := node.cache.Get(principal, time.Now())
	if !ok || len(chain) == 0 {
		return nil
	}
	return chain[0]
}

// Match the exact retained signed envelope, not merely its signer or revision.
func PublicationLossAcceptedDigest(node *Node, principal string) []byte {
	now := time.Now()
	for _, record := range node.directory.Records(now) {
		if record.Principal != principal {
			continue
		}
		if _, ok := node.directory.CurrentRecordChain(record, now); !ok {
			return nil
		}
		body, err := netwire.Marshal(record)
		if err != nil {
			return nil
		}
		id := netgossip.Identify(netgossip.Manifest, body)
		return id.Hash[:]
	}
	return nil
}
