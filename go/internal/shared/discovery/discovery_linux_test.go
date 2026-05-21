//go:build linux

package discovery

import (
	"net"
	"strings"
	"testing"

	"github.com/hashicorp/mdns"
	"github.com/wendylabsinc/wendy/internal/shared/models"
)

// ── avahiUnescape ───────────────────────────────────────────────────

func TestAvahiUnescape(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{"no escapes", "hello", "hello"},
		{"space", `WendyOS\032on\032wendyos-calm-zinnia`, "WendyOS on wendyos-calm-zinnia"},
		{"multiple escapes", `a\032b\033c`, "a b!c"},
		{"trailing backslash", `hello\`, `hello\`},
		{"short escape", `hello\04`, `hello\04`},
		{"empty", "", ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := avahiUnescape(tt.input)
			if got != tt.want {
				t.Fatalf("avahiUnescape(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

// ── parseAvahiTXT ───────────────────────────────────────────────────

func TestParseAvahiTXT(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  map[string]string
	}{
		{
			"standard TXT",
			`"displayname=Calm Zinnia" "name=calm-zinnia" "wendyosdevice=769dc651"`,
			map[string]string{
				"displayname":   "Calm Zinnia",
				"name":          "calm-zinnia",
				"wendyosdevice": "769dc651",
			},
		},
		{"empty", "", map[string]string{}},
		{"no quotes", "key=val", map[string]string{"key": "val"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := parseAvahiTXT(tt.input)
			if len(got) != len(tt.want) {
				t.Fatalf("parseAvahiTXT(%q) returned %d entries, want %d", tt.input, len(got), len(tt.want))
			}
			for k, want := range tt.want {
				if got[k] != want {
					t.Fatalf("parseAvahiTXT(%q)[%q] = %q, want %q", tt.input, k, got[k], want)
				}
			}
		})
	}
}

// ── parseAvahiResolveLine ───────────────────────────────────────────

func TestParseAvahiResolveLine(t *testing.T) {
	tests := []struct {
		name       string
		line       string
		wantOK     bool
		wantID     string
		wantIP     string
		wantPort   int
		wantIsMTLS bool
		wantIface  string
		wantUSB    string
	}{
		{
			name:       "valid resolved line with link-local IPv6",
			line:       `=;enp0s20f0u9;IPv6;WendyOS\032on\032wendyos-calm-zinnia;_wendyos._udp;local;wendyos-calm-zinnia.local;fe80::ffab:7cf6:ef:21c5;50051;"displayname=Calm Zinnia" "name=calm-zinnia" "wendyosdevice=769dc651-4eb2-49f3-b9f6-3e473f15694a" "id=WendyOS Device calm-zinnia"`,
			wantOK:     true,
			wantID:     "769dc651-4eb2-49f3-b9f6-3e473f15694a",
			wantIP:     "fe80::ffab:7cf6:ef:21c5%enp0s20f0u9",
			wantPort:   50051,
			wantIsMTLS: false,
			wantIface:  "enp0s20f0u9",
			wantUSB:    "enp0s20f0u9",
		},
		{
			name:       "provisioned device with tls=true sets IsMTLS",
			line:       `=;eth0;IPv4;WendyOS\032provisioned;_wendyos._udp;local;wendyos-prov.local;192.168.1.20;50052;"wendyosdevice=prov-uuid" "tls=true"`,
			wantOK:     true,
			wantID:     "prov-uuid",
			wantIP:     "192.168.1.20",
			wantPort:   50052,
			wantIsMTLS: true,
			wantIface:  "eth0",
		},
		{
			name:      "global IPv6 does not get zone ID",
			line:      `=;eth0;IPv6;WendyOS\032device;_wendyos._udp;local;wendyos.local;2001:db8::1;50051;"wendyosdevice=abc123"`,
			wantOK:    true,
			wantID:    "abc123",
			wantIP:    "2001:db8::1",
			wantPort:  50051,
			wantIface: "eth0",
		},
		{
			name:      "IPv4 does not get zone ID",
			line:      `=;eth0;IPv4;WendyOS\032device;_wendyos._udp;local;wendyos.local;192.168.1.10;50051;"wendyosdevice=abc123"`,
			wantOK:    true,
			wantID:    "abc123",
			wantIP:    "192.168.1.10",
			wantPort:  50051,
			wantIface: "eth0",
		},
		{
			name:   "browse line (not resolved)",
			line:   `+;enp0s20f0u9;IPv6;WendyOS\032on\032wendyos-calm-zinnia;_wendyos._udp;local`,
			wantOK: false,
		},
		{
			name:   "empty line",
			line:   "",
			wantOK: false,
		},
		{
			name:   "too few fields",
			line:   "=;a;b;c",
			wantOK: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dev, ok := parseAvahiResolveLine(tt.line)
			if ok != tt.wantOK {
				t.Fatalf("parseAvahiResolveLine() ok = %v, want %v", ok, tt.wantOK)
			}
			if !ok {
				return
			}
			if dev.ID != tt.wantID {
				t.Fatalf("ID = %q, want %q", dev.ID, tt.wantID)
			}
			if dev.IPAddress != tt.wantIP {
				t.Fatalf("IPAddress = %q, want %q", dev.IPAddress, tt.wantIP)
			}
			if dev.Port != tt.wantPort {
				t.Fatalf("Port = %d, want %d", dev.Port, tt.wantPort)
			}
			if dev.IsMTLS != tt.wantIsMTLS {
				t.Fatalf("IsMTLS = %v, want %v", dev.IsMTLS, tt.wantIsMTLS)
			}
			if dev.NetworkInterface != tt.wantIface {
				t.Fatalf("NetworkInterface = %q, want %q", dev.NetworkInterface, tt.wantIface)
			}
			if tt.wantUSB != "" && !strings.HasPrefix(dev.USB, tt.wantUSB) {
				t.Fatalf("USB = %q, want prefix %q", dev.USB, tt.wantUSB)
			}
			if !dev.IsWendyDevice {
				t.Fatal("IsWendyDevice = false, want true")
			}
		})
	}
}

// ── lanDeviceFromMDNSEntry ──────────────────────────────────────────

func TestLANDeviceFromMDNSEntrySetsMTLS(t *testing.T) {
	entry := &mdns.ServiceEntry{
		Name:       "wendyos-prov._wendyos._udp.local.",
		Host:       "wendyos-prov.local.",
		AddrV4:     net.ParseIP("192.168.1.20"),
		Port:       50052,
		InfoFields: []string{"wendyosdevice=prov-uuid", "tls=true"},
	}

	dev, ok := lanDeviceFromMDNSEntry(entry, nil)
	if !ok {
		t.Fatal("lanDeviceFromMDNSEntry returned false")
	}
	if !dev.IsMTLS {
		t.Fatal("IsMTLS = false, want true for tls=true TXT record")
	}
	if dev.ID != "prov-uuid" {
		t.Fatalf("ID = %q, want %q", dev.ID, "prov-uuid")
	}
	if dev.IPAddress != "192.168.1.20" {
		t.Fatalf("IPAddress = %q, want %q", dev.IPAddress, "192.168.1.20")
	}
	if dev.Port != 50052 {
		t.Fatalf("Port = %d, want %d", dev.Port, 50052)
	}
	if dev.InterfaceType != string(models.InterfaceLAN) {
		t.Fatalf("InterfaceType = %q, want %q", dev.InterfaceType, models.InterfaceLAN)
	}
	if !dev.IsWendyDevice {
		t.Fatal("IsWendyDevice = false, want true")
	}
}

func TestLANDeviceFromMDNSEntryNoMTLSWithoutTLS(t *testing.T) {
	entry := &mdns.ServiceEntry{
		Name:       "wendyos-device._wendyos._udp.local.",
		Host:       "wendyos-device.local.",
		AddrV4:     net.ParseIP("192.168.1.10"),
		Port:       50051,
		InfoFields: []string{"wendyosdevice=some-uuid"},
	}

	dev, ok := lanDeviceFromMDNSEntry(entry, nil)
	if !ok {
		t.Fatal("lanDeviceFromMDNSEntry returned false")
	}
	if dev.IsMTLS {
		t.Fatal("IsMTLS = true, want false when tls TXT record is absent")
	}
}

func TestLANDeviceFromMDNSEntryAddsIPv6LinkLocalZone(t *testing.T) {
	iface := &net.Interface{Name: "usb0"}
	entry := &mdns.ServiceEntry{
		Name:   "wendyos-device._wendyos._udp.local.",
		Host:   "wendyos-device.local.",
		AddrV6: net.ParseIP("fe80::1"),
		Port:   50051,
	}

	dev, ok := lanDeviceFromMDNSEntry(entry, iface)
	if !ok {
		t.Fatal("lanDeviceFromMDNSEntry returned false")
	}
	want := "fe80::1%usb0"
	if dev.IPAddress != want {
		t.Fatalf("IPAddress = %q, want %q", dev.IPAddress, want)
	}
}

func TestLANDeviceFromMDNSEntryFiltersWrongService(t *testing.T) {
	entry := &mdns.ServiceEntry{
		Name: "phone._remotepairing._tcp.local.",
		Host: "phone.local.",
		Port: 1234,
	}

	_, ok := lanDeviceFromMDNSEntry(entry, nil)
	if ok {
		t.Fatal("lanDeviceFromMDNSEntry returned true for wrong service type")
	}
}

// ── parseMDNSInfoFields ─────────────────────────────────────────────

func TestParseMDNSInfoFields(t *testing.T) {
	tests := []struct {
		name   string
		fields []string
		want   map[string]string
	}{
		{
			name:   "empty",
			fields: nil,
			want:   map[string]string{},
		},
		{
			name:   "no tls record",
			fields: []string{"id=some-device", "name=my-device"},
			want:   map[string]string{"id": "some-device", "name": "my-device"},
		},
		{
			name:   "tls=true for provisioned device",
			fields: []string{"wendyosdevice=prov-uuid", "tls=true"},
			want:   map[string]string{"wendyosdevice": "prov-uuid", "tls": "true"},
		},
		{
			name:   "tls=false is not treated as mTLS",
			fields: []string{"wendyosdevice=some-uuid", "tls=false"},
			want:   map[string]string{"wendyosdevice": "some-uuid", "tls": "false"},
		},
		{
			name:   "entry without equals sign is skipped",
			fields: []string{"noequals", "key=val"},
			want:   map[string]string{"key": "val"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := parseMDNSInfoFields(tt.fields)
			if len(got) != len(tt.want) {
				t.Fatalf("parseMDNSInfoFields(%v) returned %d entries, want %d: got %v", tt.fields, len(got), len(tt.want), got)
			}
			for k, want := range tt.want {
				if got[k] != want {
					t.Fatalf("parseMDNSInfoFields()[%q] = %q, want %q", k, got[k], want)
				}
			}
			// Verify tls→IsMTLS mapping works correctly.
			isMTLS := got["tls"] == "true"
			wantMTLS := tt.want["tls"] == "true"
			if isMTLS != wantMTLS {
				t.Fatalf("tls→IsMTLS = %v, want %v", isMTLS, wantMTLS)
			}
		})
	}
}

// ── parseAvahiMDNSService ───────────────────────────────────────────

func TestParseAvahiMDNSService(t *testing.T) {
	line := `=;enp0s20f0u9;IPv6;WendyOS\032on\032wendyos-calm-zinnia;_wendyos._udp;local;wendyos-calm-zinnia.local;fe80::ffab:7cf6:ef:21c5;50051;"displayname=Calm Zinnia" "wendyosdevice=769dc651"`

	svc, ok := parseAvahiMDNSService(line)
	if !ok {
		t.Fatal("parseAvahiMDNSService() returned false")
	}
	if svc.InstanceName != "WendyOS on wendyos-calm-zinnia" {
		t.Fatalf("InstanceName = %q, want %q", svc.InstanceName, "WendyOS on wendyos-calm-zinnia")
	}
	if svc.Hostname != "wendyos-calm-zinnia.local" {
		t.Fatalf("Hostname = %q, want %q", svc.Hostname, "wendyos-calm-zinnia.local")
	}
	if svc.IPAddress != "fe80::ffab:7cf6:ef:21c5%enp0s20f0u9" {
		t.Fatalf("IPAddress = %q, want %q", svc.IPAddress, "fe80::ffab:7cf6:ef:21c5%enp0s20f0u9")
	}
	if svc.Port != 50051 {
		t.Fatalf("Port = %d, want %d", svc.Port, 50051)
	}
	if svc.TXTRecords["wendyosdevice"] != "769dc651" {
		t.Fatalf("TXTRecords[wendyosdevice] = %q, want %q", svc.TXTRecords["wendyosdevice"], "769dc651")
	}
}
