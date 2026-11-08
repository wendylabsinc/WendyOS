package commands

import (
	"strings"
	"testing"

	"github.com/wendylabsinc/wendy/go/internal/shared/appconfig"
	"github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
)

func TestFormatNetworkInterfaces(t *testing.T) {
	ifaces := []*agentpb.NetworkInterface{
		{Name: "eth0", IpAddresses: []string{"192.168.1.42"}},
		{Name: "wlan0", IpAddresses: []string{"10.0.0.5", "2001:db8::1"}},
		{Name: "empty"},
	}

	got := formatNetworkInterfaces(ifaces)

	if !strings.HasPrefix(got, "Network:\n") {
		t.Errorf("output should start with a Network: header, got:\n%s", got)
	}
	if !strings.Contains(got, "eth0") || !strings.Contains(got, "192.168.1.42") {
		t.Errorf("output missing eth0 address:\n%s", got)
	}
	if !strings.Contains(got, "10.0.0.5, 2001:db8::1") {
		t.Errorf("output should join multiple addresses with a comma:\n%s", got)
	}
	if strings.Contains(got, "empty") {
		t.Errorf("interfaces without addresses should be skipped:\n%s", got)
	}
	if !strings.HasSuffix(got, "\n") {
		t.Errorf("output should end with a newline:\n%q", got)
	}
}

func TestFormatNetworkInterfacesUplink(t *testing.T) {
	metric := func(v uint32) *uint32 { return &v }
	tests := []struct {
		name   string
		ifaces []*agentpb.NetworkInterface
		want   string
	}{
		{
			name: "older agent: no notes, no trailing padding",
			ifaces: []*agentpb.NetworkInterface{
				{Name: "eth0", IpAddresses: []string{"192.168.1.42"}},
				{Name: "wlan0", IpAddresses: []string{"10.0.0.5"}},
			},
			want: "Network:\n  eth0   192.168.1.42\n  wlan0  10.0.0.5\n",
		},
		{
			name: "concheck off: default route and metrics only",
			ifaces: []*agentpb.NetworkInterface{
				{Name: "eth0", IpAddresses: []string{"192.168.1.42"}, IsDefaultRoute: true, RouteMetric: metric(100)},
				{Name: "wlan0", IpAddresses: []string{"10.0.0.5"}, RouteMetric: metric(300)},
				{Name: "usb0", IpAddresses: []string{"10.42.0.2"}},
			},
			want: "Network:\n" +
				"  eth0   192.168.1.42  default route, metric 100\n" +
				"  wlan0  10.0.0.5      metric 300\n" +
				"  usb0   10.42.0.2\n",
		},
		{
			name: "concheck on",
			ifaces: []*agentpb.NetworkInterface{
				{Name: "eth0", IpAddresses: []string{"192.168.1.42"}, RouteMetric: metric(20100), Connectivity: agentpb.NetworkConnectivity_NETWORK_CONNECTIVITY_LIMITED},
				{Name: "wlan0", IpAddresses: []string{"10.0.0.5"}, IsDefaultRoute: true, RouteMetric: metric(300), Connectivity: agentpb.NetworkConnectivity_NETWORK_CONNECTIVITY_FULL},
			},
			want: "Network:\n" +
				"  eth0   192.168.1.42  metric 20100, internet: limited\n" +
				"  wlan0  10.0.0.5      default route, metric 300, internet: full\n",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := formatNetworkInterfaces(tt.ifaces); got != tt.want {
				t.Errorf("formatNetworkInterfaces() =\n%q\nwant\n%q", got, tt.want)
			}
		})
	}
}

func TestBestReachableIP(t *testing.T) {
	cases := []struct {
		name   string
		ifaces []*agentpb.NetworkInterface
		want   string
	}{
		{
			name:   "none",
			ifaces: nil,
			want:   "",
		},
		{
			name: "prefers IPv4 over an earlier IPv6",
			ifaces: []*agentpb.NetworkInterface{
				{Name: "eth0", IpAddresses: []string{"2001:db8::1", "192.168.1.42"}},
			},
			want: "192.168.1.42",
		},
		{
			name: "first IPv4 across interfaces wins",
			ifaces: []*agentpb.NetworkInterface{
				{Name: "eth0", IpAddresses: []string{"192.168.1.42"}},
				{Name: "wlan0", IpAddresses: []string{"10.0.0.5"}},
			},
			want: "192.168.1.42",
		},
		{
			name: "falls back to IPv6 when no IPv4 exists",
			ifaces: []*agentpb.NetworkInterface{
				{Name: "eth0", IpAddresses: []string{"2001:db8::1"}},
			},
			want: "2001:db8::1",
		},
		{
			name: "ignores unparseable addresses",
			ifaces: []*agentpb.NetworkInterface{
				{Name: "eth0", IpAddresses: []string{"not-an-ip", "192.168.1.42"}},
			},
			want: "192.168.1.42",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := bestReachableIP(tc.ifaces); got != tc.want {
				t.Errorf("bestReachableIP() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestReachableAppURL(t *testing.T) {
	tcpReadiness := &appconfig.ReadinessConfig{TCPSocket: &appconfig.TCPSocketProbe{Port: 3001}}

	cases := []struct {
		name      string
		hookURL   string
		appID     string
		deviceIP  string
		httpPort  int
		readiness *appconfig.ReadinessConfig
		want      string
	}{
		{
			name:     "no device IP yields nothing",
			hookURL:  "http://${WENDY_HOSTNAME}:3001",
			deviceIP: "",
			want:     "",
		},
		{
			name:     "hook URL with hostname placeholder gets the IP swapped in",
			hookURL:  "http://${WENDY_HOSTNAME}:3001/dashboard",
			deviceIP: "192.168.1.42",
			want:     "http://192.168.1.42:3001/dashboard",
		},
		{
			name:     "windows-style placeholder is also rewritten",
			hookURL:  "http://%WENDY_HOSTNAME%:8080",
			deviceIP: "192.168.1.42",
			want:     "http://192.168.1.42:8080",
		},
		{
			name:      "http entitlement port wins over readiness port",
			deviceIP:  "192.168.1.42",
			httpPort:  8080,
			readiness: tcpReadiness,
			want:      "http://192.168.1.42:8080",
		},
		{
			name:      "hostname-templated hook wins over http entitlement port",
			hookURL:   "http://${WENDY_HOSTNAME}:7000/custom",
			deviceIP:  "192.168.1.42",
			httpPort:  8080,
			readiness: tcpReadiness,
			want:      "http://192.168.1.42:7000/custom",
		},
		{
			name:      "hook URL without a hostname placeholder falls back to readiness port",
			hookURL:   "http://localhost:9999",
			deviceIP:  "192.168.1.42",
			readiness: tcpReadiness,
			want:      "http://192.168.1.42:3001",
		},
		{
			name:      "no hook URL uses readiness TCP port over http",
			deviceIP:  "192.168.1.42",
			readiness: tcpReadiness,
			want:      "http://192.168.1.42:3001",
		},
		{
			name:      "IPv6 address is bracketed",
			deviceIP:  "2001:db8::1",
			readiness: tcpReadiness,
			want:      "http://[2001:db8::1]:3001",
		},
		{
			name:     "IPv6 address is bracketed in a hook URL template",
			hookURL:  "http://${WENDY_HOSTNAME}:3001/dashboard",
			deviceIP: "2001:db8::1",
			want:     "http://[2001:db8::1]:3001/dashboard",
		},
		{
			name:     "no hook URL and no readiness port yields nothing",
			deviceIP: "192.168.1.42",
			want:     "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := reachableAppURL(tc.hookURL, tc.appID, "", tc.deviceIP, tc.httpPort, tc.readiness)
			if got != tc.want {
				t.Errorf("reachableAppURL() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestURLSafeHost(t *testing.T) {
	cases := []struct {
		host string
		want string
	}{
		{"device.local", "device.local"},      // hostname untouched
		{"192.168.0.159", "192.168.0.159"},    // IPv4 untouched
		{"2001:db8::1", "[2001:db8::1]"},      // IPv6 bracketed
		{"fe80::1%en0", "[fe80::1%25en0]"},    // zone percent-escaped (RFC 6874)
		{"::ffff:192.168.0.1", "192.168.0.1"}, // IPv4-mapped unmapped to plain IPv4
		{"[2001:db8::1]", "[2001:db8::1]"},    // already bracketed passes through
		{"", ""},
	}
	for _, tc := range cases {
		if got := urlSafeHost(tc.host); got != tc.want {
			t.Errorf("urlSafeHost(%q) = %q, want %q", tc.host, got, tc.want)
		}
	}
}
