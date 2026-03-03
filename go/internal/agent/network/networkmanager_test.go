package network

import (
	"testing"
)

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
