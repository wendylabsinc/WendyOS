//go:build windows

package commands

import "fmt"

type localWifiNetwork struct {
	SSID           string
	SignalStrength int32
}

func scanLocalWifiNetworks() ([]localWifiNetwork, error) {
	return nil, fmt.Errorf("local WiFi scanning is not yet supported on Windows; use --ssid to specify the network")
}

const supportsKeychainLookup = false

// lookupKeychainPassword is not supported on Windows.
func lookupKeychainPassword(_ string) (string, error) {
	return "", nil
}
