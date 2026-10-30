//go:build linux || darwin

package netmesh

import (
	"fmt"
	"testing"

	"github.com/wendylabsinc/wendy/go/internal/shared/discovery"
	"github.com/wendylabsinc/wendy/go/internal/shared/meshid"
)

func TestOperatorLANHintsValidateAndBound(t *testing.T) {
	const tenant = "5c9e14bb-49c0-4178-9cc5-493a151f9885"
	self, err := meshid.PeerFromPrincipal("spiffe://wendy.sh/tenant/" + tenant + "/operator/alice")
	if err != nil {
		t.Fatal(err)
	}
	principal := "spiffe://wendy.sh/tenant/" + tenant + "/device/401789ca-91eb-40f7-8224-ada597012312"
	service := func(p, ip string) discovery.MDNSService {
		return discovery.MDNSService{IPAddress: ip, TXTRecords: map[string]string{"principal": p}}
	}
	services := []discovery.MDNSService{
		service(principal, "192.168.100.110"), service(principal, "192.168.100.110"),
		service(principal, "192.168.0.110"), service(principal, "169.254.1.2"),
		service(principal, "224.0.0.251"), service(principal, "127.0.0.1"),
		service(principal, "::1"), service(principal, "not-an-ip"),
		service(self.Principal(), "192.168.100.111"), service("", "192.168.100.111"),
		service("spiffe://wendy.sh/tenant/6272da32-5423-49e1-b854-8e32ee62993c/device/401789ca-91eb-40f7-8224-ada597012312", "192.168.100.111"),
	}
	if got := operatorHints(services, self); len(got) != 2 {
		t.Fatalf("unexpected hints: %+v", got)
	}
	for i := 1; i < 255; i++ {
		services = append(services, service(principal, fmt.Sprintf("10.1.1.%d", i)))
	}
	if got := operatorHints(services, self); len(got) != 128 {
		t.Fatalf("unbounded hints: %d", len(got))
	}
}
