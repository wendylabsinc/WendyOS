//go:build linux

package discovery

import (
	"testing"
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
		name     string
		line     string
		wantOK   bool
		wantID   string
		wantIP   string
		wantPort int
	}{
		{
			name:     "valid resolved line with link-local IPv6",
			line:     `=;enp0s20f0u9;IPv6;WendyOS\032on\032wendyos-calm-zinnia;_wendyos._udp;local;wendyos-calm-zinnia.local;fe80::ffab:7cf6:ef:21c5;50051;"displayname=Calm Zinnia" "name=calm-zinnia" "wendyosdevice=769dc651-4eb2-49f3-b9f6-3e473f15694a" "id=WendyOS Device calm-zinnia"`,
			wantOK:   true,
			wantID:   "769dc651-4eb2-49f3-b9f6-3e473f15694a",
			wantIP:   "fe80::ffab:7cf6:ef:21c5%enp0s20f0u9",
			wantPort: 50051,
		},
		{
			name:     "global IPv6 does not get zone ID",
			line:     `=;eth0;IPv6;WendyOS\032device;_wendyos._udp;local;wendyos.local;2001:db8::1;50051;"wendyosdevice=abc123"`,
			wantOK:   true,
			wantID:   "abc123",
			wantIP:   "2001:db8::1",
			wantPort: 50051,
		},
		{
			name:     "IPv4 does not get zone ID",
			line:     `=;eth0;IPv4;WendyOS\032device;_wendyos._udp;local;wendyos.local;192.168.1.10;50051;"wendyosdevice=abc123"`,
			wantOK:   true,
			wantID:   "abc123",
			wantIP:   "192.168.1.10",
			wantPort: 50051,
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
			if !dev.IsWendyDevice {
				t.Fatal("IsWendyDevice = false, want true")
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
