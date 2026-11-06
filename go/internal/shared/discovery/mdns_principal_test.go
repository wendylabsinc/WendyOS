package discovery

import "testing"

func TestLANDeviceFromServiceCarriesDevicePrincipalHint(t *testing.T) {
	const tenant = "11111111-1111-4111-8111-111111111111"
	const device = "22222222-2222-4222-8222-222222222222"
	const principal = "spiffe://wendy.sh/tenant/" + tenant + "/device/" + device
	svc := MDNSService{Hostname: "camera.local", IPAddress: "192.0.2.42", Port: 50052, TXTRecords: parseTXTRecord(txtWire("id="+device, "name=camera", "tls=true", "principal="+principal))}
	got := lanDeviceFromService(svc)
	if got.PrincipalURI != principal || got.TenantUUID != tenant || got.MeshName != "camera" || !got.IsMTLS {
		t.Fatalf("device principal advertisement did not reach LAN resolution: %+v", got)
	}
	for _, invalid := range []string{"", "urn:wendy:org:7:asset:42", "spiffe://other.example/tenant/" + tenant + "/device/" + device, "spiffe://wendy.sh/tenant/" + tenant + "/user/" + device, "spiffe://wendy.sh/tenant/" + tenant + "/device/42"} {
		svc.TXTRecords["principal"] = invalid
		got := lanDeviceFromService(svc)
		if got.PrincipalURI != "" || got.TenantUUID != "" {
			t.Fatalf("accepted a non-device principal hint %q: %+v", invalid, got)
		}
	}
}
