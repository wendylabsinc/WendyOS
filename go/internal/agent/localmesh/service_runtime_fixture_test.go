package localmesh

import (
	"context"
	"crypto/mldsa"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"math/big"
	"net/netip"
	"testing"
	"time"
)

// Test-only access for external packet composition without a netmesh import
// cycle. The node, signing, persistence, gossip and certificate checks are real.
func ServiceRuntimeCredentials(t *testing.T) (*Credentials, *Credentials) {
	t.Helper()
	return testCredentials(t)
}

// RenewServiceRuntimeCredentials renews the same device principal under a
// fresh ML-DSA-65 key, as provisioning_renew.go always does. NotBefore stays
// backdated here; pki-core stamps the issuance time instead.
func RenewServiceRuntimeCredentials(t *testing.T, old *Credentials) *Credentials {
	t.Helper()
	leaf, err := x509.ParseCertificate(old.Certificate.Certificate[0])
	if err != nil {
		t.Fatal(err)
	}
	leaf.SerialNumber = big.NewInt(time.Now().UnixNano())
	leaf.NotBefore = leaf.NotBefore.Add(time.Minute)
	leaf.NotAfter = leaf.NotAfter.Add(time.Hour)
	signer, err := mldsa.GenerateKey(mldsa.MLDSA65())
	if err != nil {
		t.Fatal(err)
	}
	pki := testPKI()
	der, err := x509.CreateCertificate(rand.Reader, leaf, pki.certificate, signer.Public(), pki.key)
	if err != nil {
		t.Fatal(err)
	}
	key, err := x509.MarshalPKCS8PrivateKey(signer)
	if err != nil {
		t.Fatal(err)
	}
	renewed, err := NewCredentials(string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})),
		pki.chainPEM, string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: key})))
	if err != nil {
		t.Fatal(err)
	}
	return renewed
}

func NewServiceRuntimeNode(t *testing.T, ctx context.Context, directory string, credentials *Credentials, network PacketNetwork) *Node {
	t.Helper()
	seedActorTestIdentity(t, directory, credentials)
	node, err := NewNodeWithNetwork(ctx, directory, credentials, "service-publication-regression", 50052, network)
	if err != nil {
		t.Fatal(err)
	}
	return node
}

// Request a normal Babel origin sequence refresh through the running actor.
// This is a routing stimulus, not a replacement publication or injected proof:
// the original production persistence/signing gate and HAVE/WANT exchange run.
func RequestServiceRuntimeRenewal(t *testing.T, node *Node) {
	t.Helper()
	view := node.Snapshot()
	var own Manifest
	for _, m := range view.Devices {
		if m.Device == node.Credentials.Device.UUID() {
			own = m
		}
	}
	if own.Route == nil || len(view.Links) != 1 {
		t.Fatal("service renewal requires one established neighbor and own origin")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	packet := routeSequenceRequest(netip.PrefixFrom(node.Credentials.Device.MeshAddress(), 128), own.Route.RouterID, own.Route.Seqno+1)
	if err := node.request(ctx, nodeEvent{id: view.Links[0].ID, packet: packet}); err != nil {
		t.Fatal(err)
	}
}

func ServiceRuntimeAcceptedLeaf(node *Node, principal string) []byte {
	now := time.Now()
	for _, record := range node.directory.Records(now) {
		if record.Principal == principal {
			if chain, ok := node.directory.CurrentRecordChain(record, now); ok && len(chain) != 0 {
				return chain[0]
			}
		}
	}
	return nil
}
