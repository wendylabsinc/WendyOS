package main

import (
	"testing"

	"github.com/wendylabsinc/wendy/go/internal/shared/certs"
	"github.com/wendylabsinc/wendy/go/internal/shared/unitree"
)

func TestUnitreePairingOwner(t *testing.T) {
	const tenant = "3fd28c47-700a-4ef6-9f43-19be3d13418e"
	for _, tc := range []struct {
		name     string
		scope    certs.Scope
		enrolled bool
		want     unitree.PairingOwner
	}{
		{"local", certs.Scope{}, false, unitree.PairingOwner{Local: true}},
		{"unknown enrolled", certs.Scope{}, true, unitree.PairingOwner{}},
		{"legacy tenant", certs.Scope{OrgID: 42}, true, unitree.PairingOwner{OrgID: 42}},
		{"dual tenant", certs.Scope{OrgID: 42, TenantUUID: tenant}, true, unitree.PairingOwner{OrgID: 42, TenantUUID: tenant}},
		{"PKI tenant", certs.Scope{TenantUUID: tenant}, true, unitree.PairingOwner{TenantUUID: tenant}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := unitreePairingOwner(tc.scope, tc.enrolled); got != tc.want {
				t.Fatalf("owner = %+v, want %+v", got, tc.want)
			}
		})
	}
}
