package services

import "testing"

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
