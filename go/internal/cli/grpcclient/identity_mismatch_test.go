package grpcclient

import (
	"testing"

	"github.com/wendylabsinc/wendy/go/internal/shared/certs"
)

func TestVerifiedIdentitySinkToleratesNoDestination(t *testing.T) {
	verifiedIdentitySink(nil)(certs.WendyIdentity{OrgID: 7, EntityType: "asset", EntityID: "42"})
}
func TestIdentityMismatchNilConnection(t *testing.T) {
	c := &AgentConnection{}
	if got, ok := c.IdentityMismatch(); ok || got != nil {
		t.Fatalf("identity mismatch=%v,%v", got, ok)
	}
	if got, ok := c.PinMismatch(); ok || got != nil {
		t.Fatalf("pin mismatch=%v,%v", got, ok)
	}
}
