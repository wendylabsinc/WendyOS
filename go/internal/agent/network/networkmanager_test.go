package network

import (
	"context"
	"strings"
	"testing"

	"go.uber.org/zap"

	agentpb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
)

func TestSplitNMCLI(t *testing.T) {
	cases := []struct {
		in   string
		n    int
		want []string
	}{
		{"HomeNet:80:WPA2:*", 4, []string{"HomeNet", "80", "WPA2", "*"}},
		{"My\\:Net:50:WPA2:", 4, []string{"My:Net", "50", "WPA2", ""}},
		{"a:b:c", 3, []string{"a", "b", "c"}},
		{"trailing::", 3, []string{"trailing", "", ""}},
		// Emoji bytes (0xF0-0xF4 + 0x80-0xBF continuation bytes) never collide
		// with `:` (0x3A) or `\` (0x5C), so they must round-trip verbatim.
		{"Read Only Internet \xf0\x9f\xab\xa5:70:WPA2:", 4, []string{"Read Only Internet \xf0\x9f\xab\xa5", "70", "WPA2", ""}},
		// Escaped colon adjacent to an emoji.
		{"evil\\:ssid \xf0\x9f\x98\x88:90:WPA3:", 4, []string{"evil:ssid \xf0\x9f\x98\x88", "90", "WPA3", ""}},
	}
	for _, c := range cases {
		got := splitNMCLI(c.in, c.n)
		if len(got) != len(c.want) {
			t.Errorf("splitNMCLI(%q, %d) len=%d; want %d (got=%v)", c.in, c.n, len(got), len(c.want), got)
			continue
		}
		for i := range got {
			if got[i] != c.want[i] {
				t.Errorf("splitNMCLI(%q, %d)[%d] = %q; want %q", c.in, c.n, i, got[i], c.want[i])
			}
		}
	}
}

func TestClassifySecurity(t *testing.T) {
	cases := map[string]agentpb.WiFiSecurityType{
		"":            agentpb.WiFiSecurityType_WIFI_SECURITY_TYPE_OPEN,
		"--":          agentpb.WiFiSecurityType_WIFI_SECURITY_TYPE_OPEN,
		"WPA2":        agentpb.WiFiSecurityType_WIFI_SECURITY_TYPE_WPA2_PSK,
		"WPA1 WPA2":   agentpb.WiFiSecurityType_WIFI_SECURITY_TYPE_WPA2_PSK,
		"WPA3":        agentpb.WiFiSecurityType_WIFI_SECURITY_TYPE_WPA3_SAE,
		"WPA2 802.1X": agentpb.WiFiSecurityType_WIFI_SECURITY_TYPE_WPA2_ENTERPRISE,
		"WEP":         agentpb.WiFiSecurityType_WIFI_SECURITY_TYPE_WEP,
		"WPA":         agentpb.WiFiSecurityType_WIFI_SECURITY_TYPE_WPA_PSK,
	}
	for in, want := range cases {
		if got := classifySecurity(in); got != want {
			t.Errorf("classifySecurity(%q) = %v; want %v", in, got, want)
		}
	}
}

func TestClassifyKeyMgmt(t *testing.T) {
	cases := map[string]agentpb.WiFiSecurityType{
		"":          agentpb.WiFiSecurityType_WIFI_SECURITY_TYPE_OPEN,
		"none":      agentpb.WiFiSecurityType_WIFI_SECURITY_TYPE_WEP,
		"wpa-psk":   agentpb.WiFiSecurityType_WIFI_SECURITY_TYPE_WPA2_PSK,
		"sae":       agentpb.WiFiSecurityType_WIFI_SECURITY_TYPE_WPA3_SAE,
		"wpa-eap":   agentpb.WiFiSecurityType_WIFI_SECURITY_TYPE_WPA2_ENTERPRISE,
		"ieee8021x": agentpb.WiFiSecurityType_WIFI_SECURITY_TYPE_WPA2_ENTERPRISE,
		"owe":       agentpb.WiFiSecurityType_WIFI_SECURITY_TYPE_UNSPECIFIED,
	}
	for in, want := range cases {
		if got := classifyKeyMgmt(in); got != want {
			t.Errorf("classifyKeyMgmt(%q) = %v; want %v", in, got, want)
		}
	}
}

// A saved WEP profile has key-mgmt "none"; only a profile without a security
// setting is for an open network.
func TestSavedWEPNetworkIsNotListedAsOpen(t *testing.T) {
	path, _ := writeFakeNMCLI(t,
		reply{args: "-t -f NAME,UUID,TYPE,AUTOCONNECT-PRIORITY connection show",
			out: "Old:uuid-w:802-11-wireless:0\nCafe:uuid-o:802-11-wireless:0\n"},
		reply{args: batchArgs("uuid-w", "uuid-o"), out: batchOut(
			batchRecord("uuid-w", "none", "", "Old"),
			batchRecord("uuid-o", "", "", "Cafe"))},
	)
	n := &NMCLINetworkManager{logger: zap.NewNop(), nmcliPath: path}
	want := map[string]agentpb.WiFiSecurityType{
		"Old":  agentpb.WiFiSecurityType_WIFI_SECURITY_TYPE_WEP,
		"Cafe": agentpb.WiFiSecurityType_WIFI_SECURITY_TYPE_OPEN,
	}

	known, err := n.ListKnownWiFiNetworks(context.Background())
	if err != nil || len(known) != len(want) {
		t.Fatalf("ListKnownWiFiNetworks = %v, %v; want %d networks", known, err, len(want))
	}
	for _, k := range known {
		if k.GetSecurity() != want[k.GetSsid()] {
			t.Errorf("saved network %q is listed as %v; want %v", k.GetSsid(), k.GetSecurity(), want[k.GetSsid()])
		}
	}

	// Neither network is in range, so the scan list takes the label from the profile.
	profiles, err := n.listKnownProfiles(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for ssid, security := range want {
		if row := bySSID(parseWiFiList("", profiles), ssid); row == nil || row.GetSecurity() != security {
			t.Errorf("out-of-range row for %q = %v; want security %v", ssid, row, security)
		}
	}
}

// bySSID finds the parsed network for an SSID, or nil if absent.
func bySSID(nets []*agentpb.ListWiFiNetworksResponse_WiFiNetwork, ssid string) *agentpb.ListWiFiNetworksResponse_WiFiNetwork {
	for _, n := range nets {
		if n.GetSsid() == ssid {
			return n
		}
	}
	return nil
}

func TestParseWiFiList(t *testing.T) {
	// Real nmcli -t output format: IN-USE:SSID:SIGNAL:SECURITY.
	scan := strings.Join([]string{
		"*:HomeNet:80:WPA2",
		":OfficeNet:65:WPA2",
		":OpenNet:45:",
		"::30:WPA2",        // empty SSID, should be skipped
		":HomeNet:75:WPA2", // duplicate SSID, keeps the first row
	}, "\n")

	got := parseWiFiList(scan, nil)
	if len(got) != 3 {
		t.Fatalf("parsed %d networks; want 3 (got=%v)", len(got), got)
	}
	if got[0].GetSsid() != "HomeNet" || got[1].GetSsid() != "OfficeNet" || got[2].GetSsid() != "OpenNet" {
		t.Errorf("ssids = [%q %q %q]; want [HomeNet OfficeNet OpenNet]",
			got[0].GetSsid(), got[1].GetSsid(), got[2].GetSsid())
	}
	if home := bySSID(got, "HomeNet"); home == nil || !home.GetIsConnected() {
		t.Errorf("HomeNet IsConnected = %v; want true", home.GetIsConnected())
	}
	if office := bySSID(got, "OfficeNet"); office == nil || office.GetIsConnected() {
		t.Errorf("OfficeNet should not be connected")
	}
}

// TestParseWiFiList_ConnectedNotStrongestBSS reproduces the "Connected" label
// flicker: when the associated BSS (carrying nmcli's `*` IN-USE marker) is not
// the strongest BSS for its SSID, nmcli sorts it below a sibling BSS, so the
// first row for the SSID lacks the marker. The SSID must still report connected
// — the marker on any BSS counts. Modeled on a real multi-AP capture where the
// connected AP's signal dipped below a same-SSID sibling between polls.
func TestParseWiFiList_ConnectedNotStrongestBSS(t *testing.T) {
	scan := strings.Join([]string{
		":Cafe5G:100:WPA2",
		":GuestWiFi:100:WPA2",
		":Cafe5G:100:WPA2",
		":MeshNet:92:WPA2", // first row for the connected SSID: no marker
		":GuestWiFi:92:WPA2",
		"*:MeshNet:84:WPA2", // the associated BSS, sorted lower by signal
		":MeshNet:82:WPA2",
		":Patio:39:WPA2",
	}, "\n")

	got := parseWiFiList(scan, nil)

	mesh := bySSID(got, "MeshNet")
	if mesh == nil {
		t.Fatal("MeshNet missing from parsed networks")
	}
	if !mesh.GetIsConnected() {
		t.Errorf("MeshNet IsConnected = false; want true (the `*` BSS was not the strongest, so the marker was dropped)")
	}
	// The display row should still reflect the strongest BSS for the SSID.
	if mesh.GetSignalStrength() != 92 {
		t.Errorf("MeshNet signal = %d; want 92 (strongest BSS)", mesh.GetSignalStrength())
	}
	// Exactly one SSID entry for the connected network (BSS rows collapsed).
	count := 0
	for _, n := range got {
		if n.GetSsid() == "MeshNet" {
			count++
		}
	}
	if count != 1 {
		t.Errorf("MeshNet appears %d times; want 1", count)
	}
}

func TestParseWiFiStatus(t *testing.T) {
	// Simulate nmcli -t output: TYPE:STATE:CONNECTION
	tests := []struct {
		name     string
		lines    []string
		wantConn bool
		wantSSID string
	}{
		{
			name: "connected",
			lines: []string{
				"wifi:connected:MyNetwork",
				"ethernet:unavailable:",
			},
			wantConn: true,
			wantSSID: "MyNetwork",
		},
		{
			name: "disconnected",
			lines: []string{
				"wifi:disconnected:",
				"ethernet:unavailable:",
			},
			wantConn: false,
			wantSSID: "",
		},
		{
			name:     "no wifi device",
			lines:    []string{"ethernet:connected:eth0"},
			wantConn: false,
			wantSSID: "",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			connected := false
			ssid := ""

			for _, line := range tc.lines {
				fields := splitFields(line, 3)
				if len(fields) < 3 {
					continue
				}
				if fields[0] == "wifi" && fields[1] == "connected" {
					connected = true
					ssid = fields[2]
					break
				}
			}

			if connected != tc.wantConn {
				t.Errorf("connected = %v; want %v", connected, tc.wantConn)
			}
			if ssid != tc.wantSSID {
				t.Errorf("ssid = %q; want %q", ssid, tc.wantSSID)
			}
		})
	}
}

// splitFields mimics strings.SplitN(line, ":", n).
func splitFields(s string, n int) []string {
	result := make([]string, 0, n)
	for i := 0; i < n-1; i++ {
		idx := -1
		for j := 0; j < len(s); j++ {
			if s[j] == ':' {
				idx = j
				break
			}
		}
		if idx < 0 {
			break
		}
		result = append(result, s[:idx])
		s = s[idx+1:]
	}
	result = append(result, s)
	return result
}
