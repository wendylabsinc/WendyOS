package services

import (
	"net"
	"reflect"
	"testing"

	agentpb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
)

func TestDefaultRouteMetrics(t *testing.T) {
	const ipv4 = `Iface	Destination	Gateway 	Flags	RefCnt	Use	Metric	Mask		MTU	Window	IRTT
eth0	00000000	0101A8C0	0003	0	0	100	00000000	0	0	0
eth0	0001A8C0	00000000	0001	0	0	100	00FFFFFF	0	0	0
wlan0	00000000	01000A0A	0003	0	0	600	00000000	0	0	0
wlan0	00000000	01000A0A	0003	0	0	300	00000000	0	0	0
usb0	0000FEA9	00000000	0001	0	0	700	0000FFFF	0	0	0
`
	const ipv6 = `00000000000000000000000000000000 00 00000000000000000000000000000000 00 fe800000000000000000000000000001 00000064 00000001 00000000 00000003     eth0
00000000000000000000000000000000 00 00000000000000000000000000000000 00 00000000000000000000000000000000 00000258 00000001 00000000 00000001     wwan0
fe800000000000000000000000000000 40 00000000000000000000000000000000 00 00000000000000000000000000000000 00000100 00000001 00000000 00000001     wlan0
00000000000000000000000000000000 00 00000000000000000000000000000000 00 00000000000000000000000000000000 ffffffff 00000001 00000000 00200200       lo
`
	tests := []struct {
		name       string
		ipv4, ipv6 string
		want       map[string]uint32
	}{
		{"none", "", "", map[string]uint32{}},
		{"ipv4 lowest default wins, non-default ignored", ipv4, "", map[string]uint32{"eth0": 100, "wlan0": 300}},
		{"ipv6 hex metrics, lo placeholder ignored", "", ipv6, map[string]uint32{"eth0": 100, "wwan0": 600}},
		{"both families merge", ipv4, ipv6, map[string]uint32{"eth0": 100, "wlan0": 300, "wwan0": 600}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := defaultRouteMetrics(tt.ipv4, tt.ipv6); !reflect.DeepEqual(got, tt.want) {
				t.Errorf("defaultRouteMetrics() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestParseNMConnectivity(t *testing.T) {
	const (
		unknown = agentpb.NetworkConnectivity_NETWORK_CONNECTIVITY_UNKNOWN
		none    = agentpb.NetworkConnectivity_NETWORK_CONNECTIVITY_NONE
		portal  = agentpb.NetworkConnectivity_NETWORK_CONNECTIVITY_PORTAL
		limited = agentpb.NetworkConnectivity_NETWORK_CONNECTIVITY_LIMITED
		full    = agentpb.NetworkConnectivity_NETWORK_CONNECTIVITY_FULL
	)
	tests := []struct {
		name string
		out  string
		want map[string]agentpb.NetworkConnectivity
	}{
		{"empty", "", map[string]agentpb.NetworkConnectivity{}},
		{
			// Captured shape of `nmcli -t -f DEVICE,IP4-CONNECTIVITY,IP6-CONNECTIVITY device status`.
			name: "per family, best wins",
			out:  "eth0:limited:none\nwlan0:full:none\nusb0:none:none\nwwan0:portal:limited\nlo:unknown:unknown\n",
			want: map[string]agentpb.NetworkConnectivity{"eth0": limited, "wlan0": full, "usb0": none, "wwan0": limited, "lo": unknown},
		},
		{"ipv6 only", "eth0:none:full", map[string]agentpb.NetworkConnectivity{"eth0": full}},
		{"portal", "wlan0:portal:unknown", map[string]agentpb.NetworkConnectivity{"wlan0": portal}},
		{"unrecognised state is unknown", "eth0:checking:", map[string]agentpb.NetworkConnectivity{"eth0": unknown}},
		{"malformed lines skipped", "eth0:full\n:full:full\nError: NetworkManager is not running.", map[string]agentpb.NetworkConnectivity{}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := parseNMConnectivity(tt.out); !reflect.DeepEqual(got, tt.want) {
				t.Errorf("parseNMConnectivity() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestAnnotateUplinks(t *testing.T) {
	ifaces := func() []*agentpb.NetworkInterface {
		return []*agentpb.NetworkInterface{
			{Name: "eth0", IpAddresses: []string{"192.168.1.42"}},
			{Name: "wlan0", IpAddresses: []string{"10.10.0.5", "2001:db8::5"}},
		}
	}
	type row struct {
		Default bool
		Metric  *uint32
		Conn    agentpb.NetworkConnectivity
	}
	u := func(v uint32) *uint32 { return &v }
	full := agentpb.NetworkConnectivity_NETWORK_CONNECTIVITY_FULL
	limited := agentpb.NetworkConnectivity_NETWORK_CONNECTIVITY_LIMITED
	tests := []struct {
		name    string
		sources []net.IP
		metrics map[string]uint32
		conn    map[string]agentpb.NetworkConnectivity
		want    []row
	}{
		{
			name: "nothing known",
			want: []row{{}, {}},
		},
		{
			name:    "ipv4 source picks ethernet",
			sources: []net.IP{net.ParseIP("192.168.1.42"), net.ParseIP("2001:db8::5")},
			metrics: map[string]uint32{"eth0": 100, "wlan0": 300},
			want:    []row{{Default: true, Metric: u(100)}, {Metric: u(300)}},
		},
		{
			// NM's penalty moved the route: wlan0 now wins although eth0 is up.
			name:    "wifi carries the route when ethernet lost internet",
			sources: []net.IP{net.ParseIP("10.10.0.5")},
			metrics: map[string]uint32{"eth0": 20100, "wlan0": 300},
			conn:    map[string]agentpb.NetworkConnectivity{"eth0": limited, "wlan0": full},
			want:    []row{{Metric: u(20100), Conn: limited}, {Default: true, Metric: u(300), Conn: full}},
		},
		{
			name:    "ipv6-only route",
			sources: []net.IP{net.ParseIP("2001:db8::5")},
			want:    []row{{}, {Default: true}},
		},
		{
			// A source on an unreported interface (a mesh tunnel) marks nothing
			// rather than guessing.
			name:    "source on unlisted interface falls through to next family",
			sources: []net.IP{net.ParseIP("172.31.0.9"), net.ParseIP("2001:db8::5")},
			want:    []row{{}, {Default: true}},
		},
		{
			name:    "only one interface is marked",
			sources: []net.IP{net.ParseIP("192.168.1.42"), net.ParseIP("10.10.0.5")},
			want:    []row{{Default: true}, {}},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ifaces()
			annotateUplinks(got, tt.sources, tt.metrics, tt.conn)
			var rows []row
			for _, iface := range got {
				rows = append(rows, row{iface.GetIsDefaultRoute(), iface.RouteMetric, iface.GetConnectivity()})
			}
			if got := rows; !reflect.DeepEqual(got, tt.want) {
				t.Errorf("annotateUplinks() = %v, want %v", got, tt.want)
			}
		})
	}
}
