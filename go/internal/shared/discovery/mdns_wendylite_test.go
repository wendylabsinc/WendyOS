package discovery

import "testing"

func TestLANDeviceFromWendyLiteServiceParsesTXT(t *testing.T) {
	dev := LANDeviceFromWendyLiteService(MDNSService{
		Hostname: "esp32c6-abcd.local",
		TXTRecords: map[string]string{
			"id": "esp32c6-abcd", "assetid": "5", "orgid": "3",
			"caps": "sensors", "mtls": "true",
		},
		IPAddress: "10.0.0.9",
		Port:      5054,
	})
	if !dev.WendyLite {
		t.Fatal("expected WendyLite=true")
	}
	if !dev.IsMTLS {
		t.Fatal("expected IsMTLS=true from the mtls TXT key")
	}
	if !dev.Sensorlink {
		t.Fatal("expected Sensorlink=true from caps=sensors")
	}
	if dev.AssetID != 5 || dev.OrgID != 3 {
		t.Fatalf("got AssetID=%d OrgID=%d, want 5/3", dev.AssetID, dev.OrgID)
	}
}

func TestLANDeviceFromWendyLiteServiceIgnoresTLSKey(t *testing.T) {
	// _wendyos._udp's "tls" key must not be conflated with Wendy Lite's own
	// "mtls" key.
	dev := LANDeviceFromWendyLiteService(MDNSService{TXTRecords: map[string]string{"tls": "true"}})
	if dev.IsMTLS {
		t.Fatal("expected IsMTLS=false: the tls key is not mtls")
	}
}

func TestLANDeviceFromWendyLiteServiceParsesPKITenant(t *testing.T) {
	const tenant = "11111111-2222-4333-8444-555555555555"
	dev := LANDeviceFromWendyLiteService(MDNSService{TXTRecords: map[string]string{
		"id": "board-a", "tenant": tenant, "mtls": "true", "caps": "sensors",
	}})
	if dev.TenantUUID != tenant || !dev.IsMTLS || !dev.Sensorlink || !dev.WendyLite {
		t.Fatalf("PKI enrollment metadata lost: %+v", dev)
	}
	if dev.AssetID != 0 || dev.OrgID != 0 {
		t.Fatalf("PKI identity must not invent legacy IDs: %+v", dev)
	}
	// Without principal=, id= (a hardware id on older firmware) is no pin hint.
	if dev.PrincipalURI != "" {
		t.Fatalf("derived an identity hint from id=: %+v", dev)
	}
	for _, raw := range []string{"not-a-uuid", "11111111-2222-4333-8444-AAAAAAAAAAAA", "11111111222243338444555555555555"} {
		dev := LANDeviceFromWendyLiteService(MDNSService{TXTRecords: map[string]string{"tenant": raw}})
		if dev.TenantUUID != "" {
			t.Errorf("accepted noncanonical tenant %q", raw)
		}
	}
}

func TestLANDeviceFromWendyLitePrincipalHint(t *testing.T) {
	const tenant = "11111111-2222-4333-8444-555555555555"
	const principal = "spiffe://wendy.sh/tenant/" + tenant + "/device/fleet/custom-board"
	for _, tc := range []struct{ name, advertised, want string }{
		{"explicit custom identity", principal, principal},
		{"wrong tenant", "spiffe://wendy.sh/tenant/aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee/device/board-a", ""},
		{"operator", "spiffe://wendy.sh/tenant/" + tenant + "/operator/owner", ""},
		{"control bytes", "spiffe://wendy.sh/tenant/" + tenant + "/device/board\x1b[0m", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dev := LANDeviceFromWendyLiteService(MDNSService{TXTRecords: map[string]string{
				"id": "board-a", "tenant": tenant, "principal": tc.advertised, "mtls": "true", "caps": "sensors",
			}})
			if dev.PrincipalURI != tc.want {
				t.Fatalf("principal=%q want=%q", dev.PrincipalURI, tc.want)
			}
		})
	}
}
