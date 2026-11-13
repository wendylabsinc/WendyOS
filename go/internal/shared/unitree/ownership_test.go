package unitree

import "testing"

func TestPairingOwnerUsesExistingScopeSemantics(t *testing.T) {
	const tenant = "3fd28c47-700a-4ef6-9f43-19be3d13418e"
	for _, tc := range []struct {
		name        string
		left, right PairingOwner
		match       bool
	}{
		{"local", PairingOwner{Local: true}, PairingOwner{Local: true}, true},
		{"unknown", PairingOwner{}, PairingOwner{}, false},
		{"local versus enrolled", PairingOwner{Local: true}, PairingOwner{OrgID: 42}, false},
		{"legacy versus dual", PairingOwner{OrgID: 42}, PairingOwner{OrgID: 42, TenantUUID: tenant}, true},
		{"dual versus UUID-only", PairingOwner{OrgID: 42, TenantUUID: tenant}, PairingOwner{TenantUUID: tenant}, true},
		{"unmapped", PairingOwner{OrgID: 42}, PairingOwner{TenantUUID: tenant}, false},
		{"different UUID beats equal org", PairingOwner{OrgID: 42, TenantUUID: tenant}, PairingOwner{OrgID: 42, TenantUUID: "72900574-08b3-446c-a5d8-59b35264069e"}, false},
		{"malformed UUID", PairingOwner{TenantUUID: "not-a-tenant"}, PairingOwner{TenantUUID: "not-a-tenant"}, false},
		{"mixed local", PairingOwner{Local: true, OrgID: 42}, PairingOwner{Local: true, OrgID: 42}, false},
		{"negative org", PairingOwner{OrgID: -1, TenantUUID: tenant}, PairingOwner{OrgID: -1, TenantUUID: tenant}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.left.Matches(tc.right) != tc.match || tc.right.Matches(tc.left) != tc.match {
				t.Fatal("feature ownership diverged from validated scope comparison")
			}
		})
	}
}
