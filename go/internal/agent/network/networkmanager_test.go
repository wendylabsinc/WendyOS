package network

import (
	"strings"
	"testing"

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

// TestParseWiFiProfileDetails reads nmcli's output for five profiles, one of
// them open and one hidden (profileDetailsOutput, in profiles_test.go).
func TestParseWiFiProfileDetails(t *testing.T) {
	want := map[string]wifiProfileDetail{
		"5b7e3a0c-1d2f-4e6a-9b8c-0d1e2f3a4b5c": {SSID: "Alpha", KeyMgmt: "wpa-psk"},
		"8c2d4f6a-3b5e-4c7d-8e9f-1a2b3c4d5e6f": {SSID: "Bravo", KeyMgmt: "wpa-psk"},
		"1e3f5a7c-9b2d-4f6e-8a1c-3e5f7a9b2d4f": {SSID: "Charlie", KeyMgmt: "sae"},
		"d4c3b2a1-6f5e-4d8c-9b0a-7e6f5d4c3b2a": {SSID: "Delta Guest"},
		"3a9f1c2e-7d4b-4e8a-b6c5-2f1e0d9c8b7a": {SSID: "HomeNet", KeyMgmt: "wpa-psk"},
	}
	got := parseWiFiProfileDetails(profileDetailsOutput)
	if len(got) != len(want) {
		t.Fatalf("parsed %d profiles; want %d (got=%+v)", len(got), len(want), got)
	}
	for u, w := range want {
		if got[u] != w {
			t.Errorf("profile %s = %+v; want %+v", u, got[u], w)
		}
	}
}

// TestParseWiFiProfileDetailsEdgeCases: escaped SSIDs are unescaped, and a
// block that isn't uuid + ssid (+ key-mgmt) is dropped rather than read
// across a profile boundary.
func TestParseWiFiProfileDetailsEdgeCases(t *testing.T) {
	out := strings.Join([]string{
		"11111111-1111-4111-8111-111111111111",
		`Cafe\:5G`,
		"wpa-psk",
		"",
		"22222222-2222-4222-8222-222222222222", // block with no SSID line
		"",
		"33333333-3333-4333-8333-333333333333",
		"Hidden Lab",
		"sae",
		"unexpected",
		"",
		"44444444-4444-4444-8444-444444444444",
		"Open Cafe",
	}, "\n")
	got := parseWiFiProfileDetails(out)
	want := map[string]wifiProfileDetail{
		"11111111-1111-4111-8111-111111111111": {SSID: "Cafe:5G", KeyMgmt: "wpa-psk"},
		"44444444-4444-4444-8444-444444444444": {SSID: "Open Cafe"},
	}
	if len(got) != len(want) {
		t.Fatalf("parsed %+v; want %+v", got, want)
	}
	for u, w := range want {
		if got[u] != w {
			t.Errorf("profile %s = %+v; want %+v", u, got[u], w)
		}
	}
}

func TestOffersPSKAndSAE(t *testing.T) {
	cases := []struct {
		name string
		scan []string
		want bool
	}{
		{"transition network", []string{"*:Home:80:WPA2", ":Mixed:70:WPA2 WPA3"}, true},
		{"WPA1 and WPA3", []string{":Mixed:70:WPA1 WPA3"}, true},
		{"PSK and SAE on separate access points", []string{":Mixed:70:WPA2", ":Mixed:40:WPA3"}, true},
		{"WPA2 only", []string{":Mixed:70:WPA2", ":Mixed:40:WPA1 WPA2"}, false},
		{"WPA3 only", []string{":Mixed:70:WPA3"}, false},
		{"open", []string{":Mixed:70:"}, false},
		{"enterprise", []string{":Mixed:70:WPA2 WPA3 802.1X"}, false},
		{"another SSID offers both", []string{":Mixed:70:WPA2", ":Other:60:WPA2 WPA3"}, false},
		{"not in range", []string{":Other:60:WPA2 WPA3"}, false},
		{"escaped SSID", []string{`:Mixed\:5G:70:WPA2 WPA3`}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := offersPSKAndSAE(strings.Join(tc.scan, "\n"), "Mixed"); got != tc.want {
				t.Errorf("offersPSKAndSAE = %v; want %v", got, tc.want)
			}
		})
	}
	if !offersPSKAndSAE(`:Mixed\:5G:70:WPA2 WPA3`, "Mixed:5G") {
		t.Error("offersPSKAndSAE missed an escaped SSID")
	}
}
