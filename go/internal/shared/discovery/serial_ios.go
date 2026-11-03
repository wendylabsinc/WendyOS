package discovery

// iOS does not expose host serial ports. LAN discovery uses native DNS-SD.
func resolveESP32SerialPorts() ([]SerialPortInfo, error) { return nil, nil }
