package discovery

import "testing"

func TestSensorlinkTXTParsed(t *testing.T) {
	on := lanDeviceFromService(MDNSService{TXTRecords: map[string]string{"sensorlink": "true", "assetid": "5", "orgid": "3"}})
	if !on.Sensorlink {
		t.Fatal("expected Sensorlink=true")
	}
	off := lanDeviceFromService(MDNSService{TXTRecords: map[string]string{"assetid": "5"}})
	if off.Sensorlink {
		t.Fatal("expected Sensorlink=false when key absent")
	}
}
