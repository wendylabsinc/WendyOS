package localmesh

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"math/big"
	"net/url"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/wendylabsinc/wendy/go/internal/agent/mtls"
	"github.com/wendylabsinc/wendy/go/internal/shared/certs"
)

func issuePolicyLeaf(t *testing.T, principal string, nb, na time.Time) (string, string) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	uri, err := url.Parse(principal)
	if err != nil {
		t.Fatal(err)
	}
	parent := testPKI()
	template := &x509.Certificate{SerialNumber: big.NewInt(time.Now().UnixNano()), NotBefore: nb, NotAfter: na, URIs: []*url.URL{uri}, KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth, x509.ExtKeyUsageServerAuth}}
	der, err := x509.CreateCertificate(rand.Reader, template, parent.certificate, key.Public(), parent.key)
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})), string(pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: encoded}))
}

func TestCertificatePolicyOwnDeviceAndOperatorPaths(t *testing.T) {
	now := time.Now().Truncate(time.Second)
	wall, floor := now.Add(-598*time.Second), now.Add(-30*time.Second)
	uncertain := true
	policy := certs.CertificateTimePolicy{Now: func() time.Time { return wall }, Floor: func() time.Time { return floor }, Uncertain: func() bool { return uncertain }}
	selfPrincipal := testPrincipal(testTenantUUID, testDeviceAID)
	selfPEM, selfKey := issuePolicyLeaf(t, selfPrincipal, now.Add(-5*time.Minute), now.Add(time.Hour))
	strict := certs.CertificateTimePolicy{Now: policy.Now}
	if _, err := NewCredentialsWithTimePolicy(selfPEM, testPKI().chainPEM, selfKey, strict); err == nil {
		t.Fatal("strict own startup accepted not-yet-valid leaf")
	}
	self, err := NewCredentialsWithTimePolicy(selfPEM, testPKI().chainPEM, selfKey, policy)
	if err != nil {
		t.Fatal(err)
	}
	for _, role := range []string{"device", "operator"} {
		t.Run(role, func(t *testing.T) {
			principal := testPrincipal(testTenantUUID, testDeviceBID)
			if role == "operator" {
				principal = "spiffe://wendy.sh/tenant/" + testTenantUUID + "/operator/time-policy"
			}
			peerNB := floor.Add(12 * time.Hour)
			peerPEM, peerKey := issuePolicyLeaf(t, principal, peerNB, peerNB.Add(time.Hour))
			var peer *Credentials
			if role == "operator" {
				peer, err = NewOperatorCredentialsWithTimePolicy(peerPEM, testPKI().chainPEM, peerKey, uuid.MustParse("aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"), policy)
			} else {
				peer, err = NewCredentialsWithTimePolicy(peerPEM, testPKI().chainPEM, peerKey, policy)
			}
			if err != nil {
				t.Fatal(err)
			}
			identity, err := self.Verify(peer.Certificate.Certificate, wall)
			if err != nil || identity.NotBefore.IsZero() || !identity.NotBefore.Equal(peerNB) {
				t.Fatal("bundle policy/window mismatch", identity, err)
			}
			if _, err = self.VerifyTLS(peer.Certificate.Certificate, wall); err != nil {
				t.Fatal("inbound TLS disagreed with bundle trust", err)
			}
			if role == "operator" {
				verified, err := self.VerifyPrimaryOperator(peer.Certificate.Certificate, wall)
				if err != nil || verified.NotBefore() != identity.NotBefore || verified.NotAfter() != identity.NotAfter {
					t.Fatal("primary operator path disagreed with identity", err)
				}
			} else {
				outbound, err := self.PeerTLS(peer.Device)
				if err != nil {
					t.Fatal(err)
				}
				if err = outbound.VerifyPeerCertificate(peer.Certificate.Certificate, nil); err != nil {
					t.Fatal("outbound TLS lost local policy", err)
				}
			}
			server, err := mtls.NewTLSConfigWithTimePolicy(selfPEM, testPKI().chainPEM, selfKey, nil, policy)
			if err != nil {
				t.Fatal(err)
			}
			if err = server.VerifyPeerCertificate(peer.Certificate.Certificate, nil); err != nil {
				t.Fatal("agent inbound TLS lost local policy", err)
			}
			parsed := make([]*x509.Certificate, len(peer.Certificate.Certificate))
			for i, der := range peer.Certificate.Certificate {
				parsed[i], err = x509.ParseCertificate(der)
				if err != nil {
					t.Fatal(err)
				}
			}
			if err = server.VerifyConnection(tls.ConnectionState{DidResume: true, PeerCertificates: parsed}); err != nil {
				t.Fatal("resumed server TLS lost local policy", err)
			}
			uncertain = false
			if _, err = self.Verify(peer.Certificate.Certificate, wall); err == nil {
				t.Fatal("known UTC retained boot-only leaf accommodation")
			}
			if err = server.VerifyConnection(tls.ConnectionState{DidResume: true, PeerCertificates: parsed}); err == nil {
				t.Fatal("resumed TLS bypassed certainty transition")
			}
			uncertain = true
		})
	}
	operatorPEM, operatorKey := issuePolicyLeaf(t, "spiffe://wendy.sh/tenant/"+testTenantUUID+"/operator/not-device", now.Add(-time.Hour), now.Add(time.Hour))
	if _, err = NewCredentialsWithTimePolicy(operatorPEM, testPKI().chainPEM, operatorKey, policy); err == nil {
		t.Fatal("time policy changed credential role")
	}
	foreignPEM, foreignKey := issuePolicyLeaf(t, testPrincipal(testOtherTenant, testDeviceBID), now.Add(-time.Hour), now.Add(time.Hour))
	foreign, err := NewCredentialsWithTimePolicy(foreignPEM, testPKI().chainPEM, foreignKey, policy)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = self.Verify(foreign.Certificate.Certificate, wall); err == nil {
		t.Fatal("time policy changed tenant scope")
	}
}

func TestExplicitCertificatePolicyLeavesLegacyPrimaryLifetimeUnchanged(t *testing.T) {
	now := time.Now().Truncate(time.Second)
	selfPEM, selfKey := issuePolicyLeaf(t, testPrincipal(testTenantUUID, testDeviceAID), now.Add(-time.Hour), now.Add(time.Hour))
	legacy, err := NewCredentials(selfPEM, testPKI().chainPEM, selfKey)
	if err != nil {
		t.Fatal(err)
	}
	policy := certs.CertificateTimePolicy{Now: func() time.Time { return now }, Floor: func() time.Time { return now.Add(-time.Minute) }, Uncertain: func() bool { return false }}
	mesh, err := NewCredentialsWithTimePolicy(selfPEM, testPKI().chainPEM, selfKey, policy)
	if err != nil {
		t.Fatal(err)
	}
	opPEM, opKey := issuePolicyLeaf(t, "spiffe://wendy.sh/tenant/"+testTenantUUID+"/operator/legacy-lifetime", now.Add(-time.Hour), now.Add(800*24*time.Hour))
	op, err := NewOperatorCredentials(opPEM, testPKI().chainPEM, opKey, uuid.MustParse("aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"))
	if err != nil {
		t.Fatal("legacy primary lifetime policy changed", err)
	}
	if _, err = legacy.VerifyPrimaryOperator(op.Certificate.Certificate, now); err != nil {
		t.Fatal("legacy primary policy narrowed", err)
	}
	if _, err = mesh.VerifyPrimaryOperator(op.Certificate.Certificate, now); err == nil {
		t.Fatal("explicit mesh primary policy omitted lifetime maximum")
	}
	policy.Floor = func() time.Time { return now.Add(2 * time.Hour) }
	if _, err = NewCredentialsWithTimePolicy(selfPEM, testPKI().chainPEM, selfKey, policy); err == nil {
		t.Fatal("own credential expiry ignored selected local base")
	}
}
