package carriers

import (
	"github.com/wendylabsinc/wendy/go/internal/shared/discovery"
	"testing"
)

func TestUSBDiscoveryKeepsInterfaceScopeAndRequiresTLSIdentity(t *testing.T) {
	candidates := usbCandidates([]discovery.USBDirectCandidate{{Interface: "en77", Zone: "en77"}, {Interface: "en78", Zone: "en78"}, {Interface: "en79"}})
	if len(candidates) != 2 {
		t.Fatalf("candidates: %+v", candidates)
	}
	if candidates[0].Address != "[fe80::5741:1%en77]:43024" || candidates[1].Address != "[fe80::5741:1%en78]:43024" {
		t.Fatalf("lost USB interface scope: %+v", candidates)
	}
	for _, c := range candidates {
		if !c.valid() || c.Device.IsValid() {
			t.Fatalf("USB hint must be dialable but cannot supply an authenticated identity: %+v", c)
		}
	}
	if candidates[0].key() == candidates[1].key() {
		t.Fatal("separate USB devices were deduplicated")
	}
}
