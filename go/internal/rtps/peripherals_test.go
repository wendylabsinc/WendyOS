package rtps

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestDefaultRouteInterfaces(t *testing.T) {
	dir := t.TempDir()
	ipv4 := filepath.Join(dir, "route")
	ipv6 := filepath.Join(dir, "ipv6_route")
	// wlan0 carries the IPv4 default route; eth0 only its own subnet, like a
	// robot on a dedicated cable; eth1 carries an IPv6 default route.
	must(t, os.WriteFile(ipv4, []byte(
		"Iface\tDestination\tGateway \tFlags\tRefCnt\tUse\tMetric\tMask\t\tMTU\tWindow\tIRTT\n"+
			"wlan0\t00000000\t0164A8C0\t0003\t0\t0\t600\t00000000\t0\t0\t0\n"+
			"eth0\t007BA8C0\t00000000\t0001\t0\t0\t100\t00FFFFFF\t0\t0\t0\n"), 0o600))
	must(t, os.WriteFile(ipv6, []byte(
		"00000000000000000000000000000000 00 00000000000000000000000000000000 00 fe800000000000000000000000000001 00000400 00000001 00000000 00450003     eth1\n"+
			"00000000000000000000000000000000 00 00000000000000000000000000000000 00 00000000000000000000000000000000 ffffffff 00000001 00000000 00200200       lo\n"), 0o600))

	got, err := defaultRouteInterfaces(ipv4, ipv6)
	if err != nil {
		t.Fatal(err)
	}
	if want := map[string]bool{"wlan0": true, "eth1": true}; !reflect.DeepEqual(got, want) {
		t.Errorf("uplinks = %v, want %v", got, want)
	}
	if _, err := defaultRouteInterfaces(filepath.Join(dir, "missing"), ipv6); err == nil {
		t.Error("a missing IPv4 table should be an error")
	}
	if got, err := defaultRouteInterfaces(ipv4, filepath.Join(dir, "missing")); err != nil || !got["wlan0"] {
		t.Errorf("a missing IPv6 table should be ignored: %v, %v", got, err)
	}
	if _, err := defaultRouteInterfaces(ipv4, dir); err == nil {
		t.Error("an unreadable IPv6 table should be an error")
	}
	must(t, os.WriteFile(ipv6, []byte(strings.Repeat("0", 70_000)), 0o600))
	if _, err := defaultRouteInterfaces(ipv4, ipv6); err == nil {
		t.Error("an unreadable IPv6 table should be an error")
	}
}

func TestPeripheralInterfaces(t *testing.T) {
	sys := t.TempDir()
	// eth1 and enx0 are physical; zt0 (a VPN) and br0 are not.
	for _, name := range []string{"eth0", "eth1", "enx0"} {
		must(t, os.MkdirAll(filepath.Join(sys, name, "device"), 0o755))
	}
	for _, name := range []string{"zt0", "br0"} {
		must(t, os.MkdirAll(filepath.Join(sys, name), 0o755))
	}
	got := peripheralInterfaces([]string{"eth0", "eth1", "enx0", "zt0", "br0", "bad\"/>"}, map[string]bool{"eth0": true}, sys)
	if want := []string{"eth1", "enx0"}; !reflect.DeepEqual(got, want) {
		t.Errorf("peripheral interfaces = %v, want %v", got, want)
	}
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}
