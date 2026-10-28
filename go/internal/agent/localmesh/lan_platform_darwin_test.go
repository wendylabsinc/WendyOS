//go:build darwin

package localmesh

import "testing"

func TestHardwarePortsClassifiesEthernetAndWiFi(t *testing.T) {
	ports := hardwarePorts("Hardware Port: Wi-Fi\nDevice: en0\n\nHardware Port: USB 10/100/1000 LAN\nDevice: en8\n\nHardware Port: Thunderbolt Bridge\nDevice: bridge0\n")
	if !ports["en0"] {
		t.Fatal("Wi-Fi not recognized")
	}
	if wifi, ok := ports["en8"]; !ok || wifi {
		t.Fatal("USB Ethernet not recognized")
	}
	if _, ok := ports["bridge0"]; ok {
		t.Fatal("bridge selected")
	}
}
