package discovery

import "testing"

func TestCapsTXTParsedWithBackCompat(t *testing.T) {
	on := lanDeviceFromService(MDNSService{TXTRecords: map[string]string{"caps": "sensors,foo"}})
	if !contains(on.Caps, "sensors") || !on.Sensorlink {
		t.Fatal("caps=sensors not honored")
	}
	legacy := lanDeviceFromService(MDNSService{TXTRecords: map[string]string{"sensorlink": "true"}})
	if !legacy.Sensorlink || !contains(legacy.Caps, "sensors") {
		t.Fatal("legacy sensorlink=true not mapped to caps")
	}
	off := lanDeviceFromService(MDNSService{TXTRecords: map[string]string{}})
	if off.Sensorlink || len(off.Caps) != 0 {
		t.Fatal("expected no caps")
	}
}
