package services

import (
	"fmt"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
)

func ips(addrs ...string) []net.IP {
	out := make([]net.IP, 0, len(addrs))
	for _, a := range addrs {
		out = append(out, net.ParseIP(a))
	}
	return out
}

func TestWiredAdvertisedAddrsKeepsOnlyRoutableIPv4OnLiveEthernet(t *testing.T) {
	links := []wiredLink{
		{ethernet: true, up: true, addrs: ips("10.42.0.120", "fe80::1", "2001:db8::5", "169.254.10.20", "10.98.0.1", "10.98.20.37")},
		{ethernet: false, up: true, addrs: ips("192.168.2.216")},
		{ethernet: true, up: false, addrs: ips("10.0.0.7")},
		{ethernet: false, up: true, addrs: ips("169.254.198.132")},
	}

	got := wiredAdvertisedAddrs(links)

	if want := []string{"10.42.0.120", "10.98.20.37"}; !slices.Equal(got, want) {
		t.Errorf("wiredAdvertisedAddrs = %v, want %v", got, want)
	}
}

// The CLI tries the first advertised address first.
func TestWiredAdvertisedAddrsKeepsInterfaceOrder(t *testing.T) {
	links := []wiredLink{
		{ethernet: true, up: true, addrs: ips("10.1.0.5", "10.1.0.6")},
		{ethernet: true, up: true, addrs: ips("10.2.0.5")},
	}

	got := wiredAdvertisedAddrs(links)

	if want := []string{"10.1.0.5", "10.1.0.6", "10.2.0.5"}; !slices.Equal(got, want) {
		t.Errorf("wiredAdvertisedAddrs = %v, want %v", got, want)
	}
}

// A TXT string is capped at 255 bytes; a bounded list always fits.
func TestWiredAdvertisedAddrsIsBounded(t *testing.T) {
	var many []net.IP
	for i := 1; i <= 20; i++ {
		many = append(many, net.IPv4(10, 0, 0, byte(i)))
	}

	got := wiredAdvertisedAddrs([]wiredLink{{ethernet: true, up: true, addrs: many}})

	if len(got) != maxWiredAdvertisedAddrs {
		t.Fatalf("advertised %d addresses, want %d", len(got), maxWiredAdvertisedAddrs)
	}
	if record := "wired=" + strings.Join(got, ","); len(record) > 255 {
		t.Errorf("TXT record is %d bytes, over the 255-byte limit", len(record))
	}
}

func TestAvahiContentWithWiredAddsTheRecordInsideTheWendyBlock(t *testing.T) {
	got := avahiContentWithWired(avahiFixture, []string{"10.42.0.120", "10.42.1.9"})

	const record = "<txt-record>wired=10.42.0.120,10.42.1.9</txt-record>"
	recordAt := strings.Index(got, record)
	if recordAt < 0 {
		t.Fatalf("wired TXT record not added:\n%s", got)
	}
	if typeAt := strings.Index(got, "<type>_wendyos._udp</type>"); typeAt > recordAt || strings.Index(got, "</service>") < recordAt {
		t.Errorf("wired record landed outside the _wendyos._udp block:\n%s", got)
	}
	for _, keep := range []string{"<txt-record>tls=true</txt-record>", "<port>50052</port>", "<txt-record>name=brave-dolphin</txt-record>"} {
		if !strings.Contains(got, keep) {
			t.Errorf("%s was clobbered:\n%s", keep, got)
		}
	}
}

func TestAvahiContentWithWiredReplacesAStaleValue(t *testing.T) {
	stale := avahiContentWithWired(avahiFixture, []string{"10.0.0.1"})

	got := avahiContentWithWired(stale, []string{"10.0.0.2"})

	if strings.Contains(got, "10.0.0.1") || strings.Count(got, "<txt-record>wired=") != 1 {
		t.Errorf("stale wired record not replaced in place:\n%s", got)
	}
}

// With the cable out, a lingering record would steer hosts at a dead address.
func TestAvahiContentWithWiredDropsTheRecordWhenNothingIsWired(t *testing.T) {
	advertised := avahiContentWithWired(avahiFixture, []string{"10.0.0.1"})

	if got := avahiContentWithWired(advertised, nil); got != avahiFixture {
		t.Errorf("wired record not removed cleanly:\n%s", got)
	}
}

func TestAvahiContentWithWiredLeavesOtherServicesAlone(t *testing.T) {
	const other = `<service-group><service><type>_ssh._tcp</type><port>22</port></service></service-group>`

	if got := avahiContentWithWired(other, []string{"10.0.0.1"}); got != other {
		t.Errorf("file without a _wendyos._udp block was modified:\n%s", got)
	}
}

// tickTwice feeds the same sample twice, the least that publishes it.
func tickTwice(w *wiredAdvertisement, addrs []string) {
	w.tick(zap.NewNop(), addrs)
	w.tick(zap.NewNop(), addrs)
}

func TestWiredAdvertisementPublishesAChangeThatHolds(t *testing.T) {
	_, svcPath := deviceTypeFixture(t, "", avahiFixture)
	restarts := 0
	w := wiredAdvertisement{serviceFile: svcPath, restartAvahi: countRestarts(&restarts)}

	w.tick(zap.NewNop(), []string{"10.42.0.120"})
	if got := readFile(t, svcPath); got != avahiFixture || restarts != 0 {
		t.Fatalf("acted on a single sample: restarts=%d\n%s", restarts, got)
	}
	w.tick(zap.NewNop(), []string{"10.42.0.120"})

	if got := readFile(t, svcPath); !strings.Contains(got, "<txt-record>wired=10.42.0.120</txt-record>") {
		t.Errorf("wired record not written:\n%s", got)
	}
	if restarts != 1 {
		t.Errorf("avahi restarts = %d, want 1", restarts)
	}
}

// Every restart withdraws the whole advertisement, so a lease that is missing
// for one sample, as before DHCP at boot, must cost nothing.
func TestWiredAdvertisementIgnoresABriefGap(t *testing.T) {
	current := avahiContentWithWired(avahiFixture, []string{"10.42.0.120"})
	_, svcPath := deviceTypeFixture(t, "", current)
	restarts := 0
	w := wiredAdvertisement{serviceFile: svcPath, restartAvahi: countRestarts(&restarts)}

	w.tick(zap.NewNop(), nil)
	tickTwice(&w, []string{"10.42.0.120"})

	if got := readFile(t, svcPath); got != current || restarts != 0 {
		t.Errorf("a one-sample gap was published: restarts=%d\n%s", restarts, got)
	}
}

// A lease can take most of a minute, as behind a spanning-tree switch at boot;
// the record from the last boot must ride it out.
func TestWiredAdvertisementKeepsTheRecordThroughASlowLease(t *testing.T) {
	current := avahiContentWithWired(avahiFixture, []string{"10.42.0.120"})
	_, svcPath := deviceTypeFixture(t, "", current)
	restarts := 0
	w := wiredAdvertisement{serviceFile: svcPath, restartAvahi: countRestarts(&restarts)}

	for i := 1; i < wiredRemovalSamples; i++ {
		w.tick(zap.NewNop(), nil)
	}
	tickTwice(&w, []string{"10.42.0.120"})

	if got := readFile(t, svcPath); got != current || restarts != 0 {
		t.Errorf("record dropped during a slow lease: restarts=%d\n%s", restarts, got)
	}
}

func TestWiredAdvertisementRemovesTheRecordOnceThePortStaysDown(t *testing.T) {
	_, svcPath := deviceTypeFixture(t, "", avahiContentWithWired(avahiFixture, []string{"10.42.0.120"}))
	restarts := 0
	w := wiredAdvertisement{serviceFile: svcPath, restartAvahi: countRestarts(&restarts)}

	for i := 0; i < wiredRemovalSamples; i++ {
		w.tick(zap.NewNop(), nil)
	}

	if got := readFile(t, svcPath); got != avahiFixture || restarts != 1 {
		t.Errorf("record not removed once the port stayed down: restarts=%d\n%s", restarts, got)
	}
}

// The window is per address: one of two ports leasing slowly must not cost
// two restarts.
func TestWiredAdvertisementKeepsAnAddressThroughOnePortsSlowLease(t *testing.T) {
	current := avahiContentWithWired(avahiFixture, []string{"10.0.0.1", "10.0.0.2"})
	_, svcPath := deviceTypeFixture(t, "", current)
	restarts := 0
	w := wiredAdvertisement{serviceFile: svcPath, restartAvahi: countRestarts(&restarts)}

	for i := 1; i < wiredRemovalSamples; i++ {
		w.tick(zap.NewNop(), []string{"10.0.0.1"})
	}
	tickTwice(&w, []string{"10.0.0.1", "10.0.0.2"})

	if got := readFile(t, svcPath); got != current || restarts != 0 {
		t.Errorf("address dropped during one port's slow lease: restarts=%d\n%s", restarts, got)
	}
}

func TestWiredAdvertisementDropsAnAddressOnceItStaysGone(t *testing.T) {
	_, svcPath := deviceTypeFixture(t, "", avahiContentWithWired(avahiFixture, []string{"10.0.0.1", "10.0.0.2"}))
	restarts := 0
	w := wiredAdvertisement{serviceFile: svcPath, restartAvahi: countRestarts(&restarts)}

	for i := 0; i < wiredRemovalSamples; i++ {
		w.tick(zap.NewNop(), []string{"10.0.0.1"})
	}

	if got := readFile(t, svcPath); !strings.Contains(got, "<txt-record>wired=10.0.0.1</txt-record>") || restarts != 1 {
		t.Errorf("address not dropped once gone: restarts=%d\n%s", restarts, got)
	}
}

func TestWiredAdvertisementIsQuietWhenCurrent(t *testing.T) {
	current := avahiContentWithWired(avahiFixture, []string{"10.42.0.120"})
	_, svcPath := deviceTypeFixture(t, "", current)
	before, err := os.Stat(svcPath)
	if err != nil {
		t.Fatal(err)
	}
	restarts := 0
	w := wiredAdvertisement{serviceFile: svcPath, restartAvahi: countRestarts(&restarts)}

	tickTwice(&w, []string{"10.42.0.120"})

	after, err := os.Stat(svcPath)
	if err != nil {
		t.Fatal(err)
	}
	if restarts != 0 || !after.ModTime().Equal(before.ModTime()) {
		t.Errorf("current advertisement touched: restarts=%d, rewritten=%v", restarts, !after.ModTime().Equal(before.ModTime()))
	}
}

// Desktop installs have no advertisement; one must never be invented.
func TestWiredAdvertisementSkipsAMissingServiceFile(t *testing.T) {
	svcPath := filepath.Join(t.TempDir(), "wendyos-mdns.service")
	restarts := 0
	w := wiredAdvertisement{serviceFile: svcPath, restartAvahi: countRestarts(&restarts)}

	tickTwice(&w, []string{"10.42.0.120"})

	if _, err := os.Stat(svcPath); !os.IsNotExist(err) {
		t.Errorf("service file created: %v", err)
	}
	if restarts != 0 {
		t.Errorf("avahi restarts = %d, want 0", restarts)
	}
}

// A failed restart leaves the edit in place and is retried later, without
// rewriting the file or retrying on every tick.
func TestWiredAdvertisementRetriesAFailedRestartLater(t *testing.T) {
	_, svcPath := deviceTypeFixture(t, "", avahiFixture)
	attempts, failing := 0, true
	w := wiredAdvertisement{serviceFile: svcPath, restartAvahi: func() bool {
		attempts++
		return !failing
	}}

	tickTwice(&w, []string{"10.42.0.120"})
	written := readFile(t, svcPath)
	failing = false
	for i := 0; i < wiredRestartRetryTicks; i++ {
		w.tick(zap.NewNop(), []string{"10.42.0.120"})
	}
	if attempts != 1 {
		t.Fatalf("restart attempts during the back-off = %d, want 1", attempts)
	}
	w.tick(zap.NewNop(), []string{"10.42.0.120"})

	if attempts != 2 {
		t.Errorf("restart attempts after the back-off = %d, want 2", attempts)
	}
	if got := readFile(t, svcPath); got != written || !strings.Contains(got, "wired=10.42.0.120") {
		t.Errorf("service file changed across the retry:\n%s", got)
	}
}

// A retried restart publishes the record already written, not a newer sample
// the file has not taken yet.
func TestWiredAdvertisementLogsThePublishedRecord(t *testing.T) {
	_, svcPath := deviceTypeFixture(t, "", avahiFixture)
	failing := true
	w := wiredAdvertisement{serviceFile: svcPath, restartAvahi: func() bool { return !failing }}
	core, logs := observer.New(zap.InfoLevel)
	logger := zap.New(core)
	both := []string{"10.0.0.1", "10.0.0.2"}

	tickTwice(&w, both)
	failing = false
	for i := 0; i < wiredRestartRetryTicks-2; i++ {
		w.tick(logger, both)
	}
	for i := 0; i < 4; i++ { // one address gone, but not yet for long
		w.tick(logger, both[:1])
	}

	entries := logs.FilterMessage("Advertising wired addresses over mDNS").All()
	if len(entries) != 1 || fmt.Sprint(entries[0].ContextMap()["addresses"]) != "[10.0.0.1 10.0.0.2]" {
		t.Errorf("logged %v, want the written record once", entries)
	}
	if got := readFile(t, svcPath); !strings.Contains(got, "wired=10.0.0.1,10.0.0.2") {
		t.Errorf("service file lost an address too early:\n%s", got)
	}
}

// Other writers run on the agent's startup and enrollment paths; a slow
// restart here must not hold them up.
func TestWiredAdvertisementRestartsAvahiWithoutTheFileLock(t *testing.T) {
	_, svcPath := deviceTypeFixture(t, "", avahiFixture)
	w := wiredAdvertisement{serviceFile: svcPath, restartAvahi: func() bool {
		if !avahiServiceFileMu.TryLock() {
			t.Error("avahi restarted while holding the service-file lock")
			return true
		}
		avahiServiceFileMu.Unlock()
		return true
	}}

	tickTwice(&w, []string{"10.42.0.120"})
}

// sysfsLink builds /sys/class/net/<name> under root. subsystem "" means the
// interface has no backing device (bridge, veth, dummy).
func sysfsLink(t *testing.T, root, name, arpType, subsystem string, wireless bool) string {
	t.Helper()
	dir := filepath.Join(root, "class", "net", name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "type"), []byte(arpType+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if wireless {
		if err := os.Mkdir(filepath.Join(dir, "wireless"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if subsystem != "" {
		dev := filepath.Join(root, "devices", name)
		bus := filepath.Join(root, "bus", subsystem)
		for _, d := range []string{dev, bus} {
			if err := os.MkdirAll(d, 0o755); err != nil {
				t.Fatal(err)
			}
		}
		if err := os.Symlink(bus, filepath.Join(dev, "subsystem")); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(dev, filepath.Join(dir, "device")); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func TestIsWiredEthernetClassifiesInterfaces(t *testing.T) {
	root := t.TempDir()
	tests := []struct {
		name string
		dir  string
		want bool
	}{
		{"onboard ethernet", sysfsLink(t, root, "eth0", "1", "platform", false), true},
		{"pci ethernet", sysfsLink(t, root, "enp1s0", "1", "pci", false), true},
		{"usb ethernet adapter", sysfsLink(t, root, "enx001122", "1", "usb", false), true},
		{"wifi", sysfsLink(t, root, "wlan0", "1", "sdio", true), false},
		{"usb gadget link to a host", sysfsLink(t, root, "usb0", "1", "gadget", false), false},
		{"virtual interface", sysfsLink(t, root, "dummy0", "1", "", false), false},
		{"non-ethernet link", sysfsLink(t, root, "can0", "280", "platform", false), false},
		{"missing interface", filepath.Join(root, "class", "net", "gone0"), false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isWiredEthernet(tt.dir); got != tt.want {
				t.Errorf("isWiredEthernet(%s) = %v, want %v", filepath.Base(tt.dir), got, tt.want)
			}
		})
	}
}
