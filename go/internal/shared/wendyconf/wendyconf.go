// Package wendyconf defines the on-disk format for `wendy.conf`, the config
// partition file that pre-seeds a WendyOS device with WiFi credentials (and,
// in the future, other first-boot settings). It is shared between the CLI
// (which writes the file during `wendy os install`) and the agent (which
// reads and applies it on first boot).
package wendyconf

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// WifiCredential is one pre-seeded network profile.
type WifiCredential struct {
	SSID     string
	Password string
	// Priority is NetworkManager's autoconnect-priority. Higher wins. Zero is
	// treated as unspecified.
	Priority int32
	// Hidden indicates the SSID is non-broadcasting. Maps to nmcli `hidden yes`.
	Hidden bool
	// Security is a free-form string matching the `wifi rank` / `wifi connect`
	// enum values (e.g. "wpa2", "wpa3", "open"). Empty means autodetect.
	Security string
}

// Marshal writes the INI representation. The first credential goes into the
// `[wifi]` section (backwards compatible with the single-credential format);
// additional credentials go into `[wifi.2]`, `[wifi.3]`, … sections so the
// order is stable across writes.
func Marshal(creds []WifiCredential) []byte {
	var b strings.Builder
	for i, c := range creds {
		section := "wifi"
		if i > 0 {
			section = fmt.Sprintf("wifi.%d", i+1)
		}
		fmt.Fprintf(&b, "[%s]\n", section)
		fmt.Fprintf(&b, "ssid = %s\n", c.SSID)
		if c.Password != "" {
			fmt.Fprintf(&b, "password = %s\n", c.Password)
		}
		if c.Priority != 0 {
			fmt.Fprintf(&b, "priority = %d\n", c.Priority)
		}
		if c.Hidden {
			b.WriteString("hidden = true\n")
		}
		if c.Security != "" {
			fmt.Fprintf(&b, "security = %s\n", c.Security)
		}
		if i < len(creds)-1 {
			b.WriteString("\n")
		}
	}
	return []byte(b.String())
}

// UnmarshalWiFi extracts every `[wifi]` and `[wifi.N]` section from parsed INI
// data and returns the credentials in priority order (highest first; ties
// broken by original section order). Sections without a non-empty SSID are
// dropped so the agent never tries to connect to a nameless network.
func UnmarshalWiFi(sections map[string]map[string]string) []WifiCredential {
	type entry struct {
		order int
		cred  WifiCredential
	}
	var out []entry
	for name, fields := range sections {
		if name != "wifi" && !strings.HasPrefix(name, "wifi.") {
			continue
		}
		ssid := fields["ssid"]
		if ssid == "" {
			continue
		}
		c := WifiCredential{
			SSID:     ssid,
			Password: fields["password"],
			Security: strings.ToLower(fields["security"]),
		}
		if p, err := strconv.Atoi(fields["priority"]); err == nil {
			c.Priority = int32(p)
		}
		if h, err := strconv.ParseBool(fields["hidden"]); err == nil {
			c.Hidden = h
		}
		order := 0
		if name != "wifi" {
			if n, err := strconv.Atoi(strings.TrimPrefix(name, "wifi.")); err == nil {
				order = n
			}
		}
		out = append(out, entry{order: order, cred: c})
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].cred.Priority != out[j].cred.Priority {
			return out[i].cred.Priority > out[j].cred.Priority
		}
		return out[i].order < out[j].order
	})
	result := make([]WifiCredential, len(out))
	for i, e := range out {
		result[i] = e.cred
	}
	return result
}
