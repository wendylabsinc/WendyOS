package carriers

import (
	"testing"

	"github.com/wendylabsinc/wendy/go/internal/shared/discovery"
	"github.com/wendylabsinc/wendy/go/internal/shared/meshdiscovery"
)

func TestNativeLANUsesBothDeclaredServiceTypesAndPorts(t *testing.T) {
	self := testDevice(1)
	peer := testDevice(2)
	service := discovery.MDNSService{IPAddress: "192.0.2.2", Port: 53000, InterfaceName: "en0", TXTRecords: map[string]string{"version": "1", "protocol": "tls-v1", "mesh": meshdiscovery.DefaultHint(self.Tenant()).String(), "principal": peer.Principal(), "role": "router", "platform": "WendyOS"}}
	candidates := lanCandidates([]discovery.MDNSService{service, service}, self, TCP)
	if len(candidates) != 1 || candidates[0].Address != "192.0.2.2:53000" || candidates[0].Device != peer {
		t.Fatalf("TCP hints=%+v", candidates)
	}
	// The operator callback can use TCP even when the device also advertises QUIC.
	service.Port = 53001
	service.TXTRecords["protocol"] = "quic-v1"
	candidates = lanCandidates([]discovery.MDNSService{service}, self, QUIC)
	if len(candidates) != 1 || candidates[0].Transport != QUIC || candidates[0].Address != "192.0.2.2:53001" {
		t.Fatalf("QUIC hints=%+v", candidates)
	}
	service.TXTRecords["platform"] = "Lite"
	if len(lanCandidates([]discovery.MDNSService{service}, self, QUIC)) != 0 {
		t.Fatal("Lite-only role offered to operator TLS before routing through WendyOS")
	}
	service.TXTRecords["platform"] = "WendyOS"
	service.TXTRecords["role"] = "endpoint"
	if len(lanCandidates([]discovery.MDNSService{service}, self, QUIC)) != 0 {
		t.Fatal("retired leaf endpoint offered as full router")
	}
}
