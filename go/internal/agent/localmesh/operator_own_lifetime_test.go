package localmesh

import (
	"github.com/wendylabsinc/wendy/go/internal/shared/certs"
	"github.com/wendylabsinc/wendy/go/internal/shared/meshid"
	"testing"
	"time"
)

func TestOperatorOwnCredentialExpiryUsesCurrentFullPath(t *testing.T) {
	now := time.Now().Truncate(time.Second)
	wall := now
	uncertain := true
	policy := certs.CertificateTimePolicy{Now: func() time.Time { return wall }, Uncertain: func() bool { return uncertain }}
	cert, key := issuePolicyLeaf(t, testPrincipal(testTenantUUID, testDeviceAID), now.Add(time.Hour), now.Add(2*time.Hour))
	credentials, err := NewCredentialsWithTimePolicy(cert, testPKI().chainPEM, key, policy)
	if err != nil {
		t.Fatal(err)
	}
	verify := operatorOwnCredentialExpiry(credentials)
	if _, err := verify(); err != nil {
		t.Fatal("valid boot own path rejected", err)
	}
	uncertain = false
	if _, err := verify(); err == nil {
		t.Fatal("own NotBefore grace survived certainty")
	}
	uncertain = true
	wall = now.Add(3 * time.Hour)
	if _, err := verify(); err == nil {
		t.Fatal("own path expiry ignored")
	}
	wall = now
	// An unexpected authorized identity must not become the local endpoint.
	other, err := meshid.DeviceFromPrincipal(testPrincipal(testTenantUUID, testDeviceBID))
	if err != nil {
		t.Fatal(err)
	}
	credentials.Verify = func([][]byte, time.Time) (Identity, error) { return Identity{Device: other}, nil }
	if _, err := verify(); err == nil {
		t.Fatal("own principal changed")
	}
}
