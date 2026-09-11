package models

import (
	"strings"
	"testing"
)

// Local VMs are listed apart from LAN devices: a JSON reader sees them under
// their own key, and a collection holding only a simulator is not "nothing
// found".
func TestDevicesCollectionListsSimulatorsApart(t *testing.T) {
	c := &DevicesCollection{Simulators: []LANDevice{{ID: "vm:sim", DisplayName: "sim", IPAddress: "127.0.0.1", Port: 50051}}}
	if c.IsEmpty() {
		t.Fatal("a collection holding only a simulator reported itself empty")
	}
	if len(c.LANDevices) != 0 {
		t.Fatal("simulators must not be counted as LAN devices")
	}
	out, err := c.ToJSON()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), `"simulators"`) {
		t.Errorf("JSON lacks a simulators key:\n%s", out)
	}
}
