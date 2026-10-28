package models

import "testing"

func TestMeshMergeUsesFullPrincipalAndPreservesPhysicalPaths(t *testing.T) {
	const tenant = "11111111-1111-4111-8111-111111111111"
	const device = "33333333-3333-4333-8333-333333333333"
	principal := "spiffe://wendy.sh/tenant/" + tenant + "/device/" + device
	mesh := MeshDevice{Selector: "mesh://" + tenant + "/" + device, Principal: principal, Tenant: tenant, Device: device, Name: "same name", Platform: "Lite", Caps: []string{"sensors", "future-optional"}}
	c := &DevicesCollection{LANDevices: []LANDevice{{DisplayName: "physical", Hostname: "physical.local", PrincipalURI: principal, TenantUUID: tenant}}, MeshDevices: []MeshDevice{mesh}}
	rows := c.MergedDevices()
	if len(rows) != 1 || rows[0].Mesh == nil || rows[0].LAN == nil || rows[0].ConnectionTypes() != "Mesh (Lite), LAN" {
		t.Fatal(rows)
	}
	if !rows[0].Sensorlink || len(rows[0].Caps) != 2 || rows[0].Caps[1] != "future-optional" {
		t.Fatal("mesh SensorLink capability or optional caps lost", rows)
	}
	c.MeshDevices = nil
	rows = c.MergedDevices()
	if len(rows) != 1 || rows[0].Mesh != nil || rows[0].LAN == nil {
		t.Fatal("mesh withdrawal removed LAN", rows)
	}
	if rows[0].Sensorlink {
		t.Fatal("withdrawn mesh capability remained on the physical path", rows)
	}
	other := mesh
	other.Tenant = "22222222-2222-4222-8222-222222222222"
	other.Principal = "spiffe://wendy.sh/tenant/" + other.Tenant + "/device/" + device
	other.Selector = "mesh://" + other.Tenant + "/" + device
	c.MeshDevices = []MeshDevice{mesh, other}
	rows = c.MergedDevices()
	if len(rows) != 2 {
		t.Fatal("name/UUID conflated tenants", rows)
	}
	c.LANDevices = nil
	c.MeshDevices = []MeshDevice{mesh}
	rows = c.MergedDevices()
	if rows[0].Address() != mesh.Selector || c.IsEmpty() {
		t.Fatal(rows)
	}
}
