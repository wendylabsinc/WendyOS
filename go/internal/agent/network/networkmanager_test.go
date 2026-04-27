package network

import (
	"testing"

	agentpb "github.com/wendylabsinc/wendy/proto/gen/agentpb"
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

func TestParseWiFiList(t *testing.T) {
	// Simulate nmcli -t output format: SSID:SIGNAL:SECURITY:IN-USE
	lines := []string{
		"HomeNet:80:WPA2:*",
		"OfficeNet:65:WPA2:",
		"OpenNet:45::",
		":30:WPA2:",        // empty SSID, should be skipped
		"HomeNet:75:WPA2:", // duplicate SSID, should be skipped
	}

	seen := make(map[string]bool)
	type network struct {
		ssid string
	}
	var networks []network

	for _, line := range lines {
		fields := splitFields(line, 4)
		if len(fields) < 4 {
			continue
		}
		ssid := fields[0]
		if ssid == "" || seen[ssid] {
			continue
		}
		seen[ssid] = true
		networks = append(networks, network{ssid: ssid})
	}

	if len(networks) != 3 {
		t.Fatalf("parsed %d networks; want 3", len(networks))
	}
	if networks[0].ssid != "HomeNet" {
		t.Errorf("networks[0].ssid = %q; want HomeNet", networks[0].ssid)
	}
	if networks[1].ssid != "OfficeNet" {
		t.Errorf("networks[1].ssid = %q; want OfficeNet", networks[1].ssid)
	}
	if networks[2].ssid != "OpenNet" {
		t.Errorf("networks[2].ssid = %q; want OpenNet", networks[2].ssid)
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
