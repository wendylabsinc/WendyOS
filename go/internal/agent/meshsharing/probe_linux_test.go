//go:build linux

package meshsharing

import "testing"

func TestIndependentDefaultRequiresExactDeviceAndNonMeshGateway(t *testing.T) {
	checks := []struct {
		route string
		want  bool
	}{
		{"default via 10.201.0.1 dev eth0 proto dhcp metric 100", true},
		{"default via 10.201.0.1 dev eth0peer metric 100", false},
		{"default via 10.88.1.204 dev eth0 metric 100", false},
		{"default via 10.99.1.1 dev eth0 metric 100", false},
		{"default via 127.0.0.1 dev eth0 metric 100", false},
		{"default dev eth0 scope link", false},
	}
	for _, check := range checks {
		if got := hasPhysicalDefault(check.route, "eth0"); got != check.want {
			t.Errorf("route %q: got %v, want %v", check.route, got, check.want)
		}
	}
	for _, address := range []string{"10.88.1.204", "10.99.2.1", "127.0.0.1", "169.254.1.1", "224.0.0.1", "garbage"} {
		if independentIPv4(address) {
			t.Errorf("mesh or invalid DNS accepted: %s", address)
		}
	}
	if !independentIPv4("10.201.0.1") {
		t.Fatal("independent DNS rejected")
	}
}
