package wendyconf

import (
	"strings"
	"testing"
)

func TestMarshalRoundTrip(t *testing.T) {
	creds := []WifiCredential{
		{SSID: "Home", Password: "homepass", Priority: 100},
		{SSID: "Office", Password: "off", Priority: 50, Hidden: true, Security: "wpa2"},
		{SSID: "Cafe"},
	}

	data := Marshal(creds)
	got := string(data)
	if !strings.Contains(got, "[wifi]\n") {
		t.Errorf("missing [wifi] section, got:\n%s", got)
	}
	if !strings.Contains(got, "[wifi.2]\n") {
		t.Errorf("missing [wifi.2] section, got:\n%s", got)
	}
	if !strings.Contains(got, "[wifi.3]\n") {
		t.Errorf("missing [wifi.3] section, got:\n%s", got)
	}
	if !strings.Contains(got, "hidden = true\n") {
		t.Errorf("hidden flag missing from marshal")
	}
	if strings.Contains(got, "hidden = false") {
		t.Errorf("hidden=false should be omitted to keep the file minimal")
	}

	// Mimic parseINI's output and round-trip back.
	sections := parseINIForTest(data)
	parsed := UnmarshalWiFi(sections)
	if len(parsed) != 3 {
		t.Fatalf("parsed %d creds; want 3", len(parsed))
	}
	// Highest priority first.
	if parsed[0].SSID != "Home" || parsed[0].Priority != 100 {
		t.Errorf("parsed[0] = %+v", parsed[0])
	}
	if parsed[1].SSID != "Office" || !parsed[1].Hidden || parsed[1].Security != "wpa2" {
		t.Errorf("parsed[1] = %+v", parsed[1])
	}
	if parsed[2].SSID != "Cafe" {
		t.Errorf("parsed[2] = %+v", parsed[2])
	}
}

func TestUnmarshalSkipsEmptySSID(t *testing.T) {
	sections := map[string]map[string]string{
		"wifi":   {"ssid": ""},
		"wifi.2": {"ssid": "Good"},
	}
	got := UnmarshalWiFi(sections)
	if len(got) != 1 || got[0].SSID != "Good" {
		t.Fatalf("got %+v; want one entry SSID=Good", got)
	}
}

// parseINIForTest replicates the minimal INI parser used by the agent so we
// can exercise the full write → parse cycle without importing the agent package.
func parseINIForTest(data []byte) map[string]map[string]string {
	result := make(map[string]map[string]string)
	var section string
	for _, raw := range strings.Split(string(data), "\n") {
		line := strings.TrimSpace(raw)
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, ";") {
			continue
		}
		if strings.HasPrefix(line, "[") && strings.HasSuffix(line, "]") {
			section = line[1 : len(line)-1]
			if result[section] == nil {
				result[section] = make(map[string]string)
			}
			continue
		}
		if section == "" {
			continue
		}
		if idx := strings.IndexByte(line, '='); idx > 0 {
			key := strings.TrimSpace(line[:idx])
			val := strings.TrimSpace(line[idx+1:])
			result[section][key] = val
		}
	}
	return result
}
