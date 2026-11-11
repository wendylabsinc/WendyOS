package discovery

import (
	"testing"
)

func TestApplicationTLSDiscoveryRequiresPrincipalAndMTLS(t *testing.T) {
	const principal = "spiffe://wendy.sh/tenant/11111111-1111-4111-8111-111111111111/device/22222222-2222-4222-8222-222222222222"
	for _, test := range []struct {
		port, identity, tls string
		want                uint16
	}{
		{"43023", principal, "true", 43023},
		{"43023", "", "true", 0},
		{"43023", principal, "false", 0},
		{"0", principal, "true", 0},
		{"65536", principal, "true", 0},
		{"admin:50052", principal, "true", 0},
	} {
		device := lanDeviceFromService(MDNSService{TXTRecords: map[string]string{"principal": test.identity, "tls": test.tls, "app-tls": test.port}})
		if device.AppTLSPort != test.want {
			t.Fatalf("app-tls=%q principal=%q tls=%q => %d", test.port, test.identity, test.tls, device.AppTLSPort)
		}
	}
}
