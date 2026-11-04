package services

import (
	"context"
	"testing"
	"time"
)

func TestCloudEnrollmentUsesPersistedProtocol(t *testing.T) {
	principal := "spiffe://wendy.sh/tenant/22f35004-d638-4b5c-869f-b815974d6eae/device/2122b8ea-8571-4c7b-9835-64f669d01462"
	for _, tc := range []struct {
		name       string
		enrolled   bool
		org, asset int32
		principal  string
		mode       CloudEnrollmentMode
		invalid    bool
	}{
		{name: "setup", mode: CloudUnenrolled},
		{name: "legacy", enrolled: true, org: 64, asset: 674, mode: CloudLegacy},
		{name: "PKI", enrolled: true, principal: principal, mode: CloudPKI},
		{name: "bad principal cannot fall back to numeric", enrolled: true, org: 64, asset: 674, principal: "broken", invalid: true},
		{name: "partial numeric enrollment", enrolled: true, org: 64, invalid: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e, err := classifyCloudEnrollment(tc.enrolled, "cloud.example:443", tc.org, tc.asset, tc.principal)
			if (err != nil) != tc.invalid || (err == nil && e.Mode != tc.mode) {
				t.Fatalf("mode=%q error=%v", e.Mode, err)
			}
		})
	}
}

func TestLegacyEnrollmentDoesNotFeedPKIWorkers(t *testing.T) {
	s := &ProvisioningService{enrolled: true, cloudHost: "legacy.example:443", orgID: 64, assetID: 674, certPEM: "legacy-or-dual-SAN-leaf", keyPEM: []byte("key")}
	if host, leaf, chain, key := s.PKICloudCredentials(); host != "" || leaf != "" || chain != "" || key != "" {
		t.Fatal("legacy identity was published to a PKI-only worker")
	}
}

// Restoring v1 transport must not invent automatic lifecycle operations which
// the pre-Wendy-Net v1 agent never ran, even for a transitional dual-SAN leaf.
func TestLegacyEnrollmentDoesNotStartPKIRenewal(t *testing.T) {
	leaf, key := legacyBrokerTestLeaf(t, true)
	service := &ProvisioningService{enrolled: true, cloudHost: "legacy:443", orgID: 64, assetID: 674, certPEM: leaf, keyPEM: []byte(key)}
	renewed, err := service.RenewIfDue(context.Background(), time.Now().AddDate(10, 0, 0))
	if err != nil || renewed {
		t.Fatalf("legacy renewal=%v %v", renewed, err)
	}
	if host, cert, chain, key := service.PKICloudCredentials(); host != "" || cert != "" || chain != "" || key != "" {
		t.Fatal("legacy enrollment exposed PKI worker credentials")
	}
}

func TestLegacyCredentialsKeyCopyDoesNotAliasProvisioning(t *testing.T) {
	service := &ProvisioningService{enrolled: true, cloudHost: "legacy:443", orgID: 64, assetID: 674, keyPEM: []byte("private key")}
	_, _, _, first, err := service.LegacyCloudCredentials()
	if err != nil {
		t.Fatal(err)
	}
	clear(first)
	_, _, _, second, err := service.LegacyCloudCredentials()
	if err != nil {
		t.Fatal(err)
	}
	defer clear(second)
	if string(second) != "private key" {
		t.Fatal("clearing a credential snapshot changed provisioning state")
	}
}
