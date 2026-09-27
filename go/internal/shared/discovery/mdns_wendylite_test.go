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
