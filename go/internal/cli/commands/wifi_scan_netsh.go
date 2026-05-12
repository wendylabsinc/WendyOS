package commands

import (
	"bufio"
	"regexp"
	"strconv"
	"strings"
)

// localWifiNetwork is the host-side scan result type shared across platforms.
type localWifiNetwork struct {
	SSID           string
	SignalStrength int32 // 0–100 percentage, or 0 if unknown
}

// ssidLine matches `SSID 1 : MyNetwork`. The `\d+` between `SSID` and the
// colon rejects `BSSID` lines, which would otherwise match `^.SSID`.
var ssidLine = regexp.MustCompile(`^SSID\s+\d+\s*:\s*(.*)$`)

// signalLine matches `         Signal             : 78%`.
var signalLine = regexp.MustCompile(`^\s*Signal\s*:\s*(\d+)%`)

// parseNetshNetworks parses the localized text output of
// `netsh wlan show networks mode=bssid` into a deduplicated list of SSIDs,
// each annotated with the strongest signal strength observed across all of
// its BSSIDs. Hidden networks (empty SSID after the colon) are skipped.
//
// Lives in a non-OS-tagged file so its tests run under the standard PR CI
// `go test ./...` job on Linux even though the only caller is the Windows
// `scanLocalWifiNetworks` implementation.
func parseNetshNetworks(output string) []localWifiNetwork {
	type entry struct {
		index  int
		signal int32
	}
	entries := make(map[string]*entry)
	order := 0

	var currentSSID string
	var haveSSID bool

	scanner := bufio.NewScanner(strings.NewReader(output))
	for scanner.Scan() {
		line := scanner.Text()

		if m := ssidLine.FindStringSubmatch(line); m != nil {
			currentSSID = strings.TrimSpace(m[1])
			haveSSID = currentSSID != ""
			if haveSSID {
				if _, ok := entries[currentSSID]; !ok {
					entries[currentSSID] = &entry{index: order}
					order++
				}
			}
			continue
		}

		if !haveSSID {
			continue
		}

		if m := signalLine.FindStringSubmatch(line); m != nil {
			if v, err := strconv.Atoi(m[1]); err == nil {
				signal := int32(v)
				if e := entries[currentSSID]; e != nil && signal > e.signal {
					e.signal = signal
				}
			}
		}
	}

	out := make([]localWifiNetwork, len(entries))
	for ssid, e := range entries {
		out[e.index] = localWifiNetwork{SSID: ssid, SignalStrength: e.signal}
	}
	return out
}
