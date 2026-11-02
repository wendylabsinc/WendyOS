package netmesh

import (
	"context"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/wendylabsinc/wendy/go/internal/agent/localmesh"
	"github.com/wendylabsinc/wendy/go/internal/shared/meshoperator"
)

// An ordinary opaque operator subject accepted by the public identity parser
// must also fit every cache, signed-origin and durable routing-state boundary.
func TestOperatorPrincipalLimitAcrossNodeRestart(t *testing.T) {
	var authority fixtureAuthority
	credentials := fixtureCredentials(t, &authority)
	key := credentials[1].Signer
	prefix := "spiffe://wendy.sh/tenant/11111111-1111-4111-8111-111111111111/operator/"
	principal := prefix + strings.Repeat("s", meshoperator.MaxPrincipalBytes-len(prefix))
	subject, err := url.Parse(principal)
	if err != nil {
		t.Fatal(err)
	}
	leaf, err := x509.ParseCertificate(credentials[1].Certificate.Certificate[0])
	if err != nil {
		t.Fatal(err)
	}
	leaf.URIs = []*url.URL{subject}
	der, err := x509.CreateCertificate(rand.Reader, leaf, authority.certificate, key.Public(), authority.key)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	certPEM := string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))
	chainPEM := string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: authority.certificate.Raw}))
	keyPEM := string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}))
	operator, err := localmesh.NewOperatorCredentials(certPEM, chainPEM, keyPEM, uuid.MustParse("aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"))
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	var previousRevision uint64
	for range 2 {
		network, err := NewNetwork(operator.Device)
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		node, err := localmesh.NewNodeWithNetwork(ctx, dir, operator, "operator", 0, network)
		if err != nil {
			cancel()
			network.Close()
			t.Fatal("create operator node:", err)
		}
		done := make(chan error, 1)
		go func() { done <- node.Run() }()
		var currentRevision uint64
		for ctx.Err() == nil && currentRevision == 0 {
			snapshot := node.Snapshot()
			for _, manifest := range snapshot.Devices {
				if manifest.Owner != nil && manifest.Owner.Principal == principal && manifest.Owner.Instance == operator.Device.UUID() {
					currentRevision = manifest.Revision
				}
			}
			if currentRevision == 0 {
				time.Sleep(time.Millisecond)
			}
		}
		cancel()
		select {
		case err := <-done:
			if err != nil && ctx.Err() == nil {
				t.Fatal(err)
			}
		case <-time.After(time.Second):
			t.Fatal("operator node did not stop")
		}
		if currentRevision <= previousRevision {
			t.Fatalf("signed origin did not advance across restart: %d -> %d", previousRevision, currentRevision)
		}
		previousRevision = currentRevision
	}
}
