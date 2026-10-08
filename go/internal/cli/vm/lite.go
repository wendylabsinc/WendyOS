package vm

import "fmt"

const ProfileWendyLite = "wendy-lite"

// The initial Lite simulator runs the published ESP32-C6 firmware. These
// credentials belong only to the emulator's virtual access point.
const LiteSSID = "wendy-simulator"
const LitePassword = "wendy-simulator"

func (s Spec) liteArgs() ([]string, error) {
	if err := ValidName(s.Name); err != nil {
		return nil, err
	}
	if s.DiskPath == "" {
		return nil, fmt.Errorf("no ESP32 flash image")
	}
	if s.Net.Mode != NetUser {
		return nil, fmt.Errorf("Wendy Lite simulation requires user networking")
	}
	if s.Net.AgentPort < 1 || s.Net.AgentPort > 65535 {
		return nil, fmt.Errorf("invalid Wendy Lite host port")
	}
	return []string{"--chip", "esp32c6", "--firmware", s.DiskPath,
		"--save-state", "--net", fmt.Sprintf("user,hostfwd=tcp:127.0.0.1:%d-:5054", s.Net.AgentPort),
		"--wifi-ssid", LiteSSID, "--wifi-password", LitePassword}, nil
}
