package services

import (
	"os"
	"runtime"

	"github.com/wendylabsinc/wendy/go/internal/agent/localmesh"
	"github.com/wendylabsinc/wendy/go/internal/shared/version"
)

// MeshAgentMetadata reports the same static version fields as GetAgentVersion,
// without running hardware probes during mesh startup.
func MeshAgentMetadata() *localmesh.AgentMetadata {
	m := &localmesh.AgentMetadata{Version: version.Version, OS: detectOS(), CPUArchitecture: runtime.GOARCH}
	if v, ok := wendyOSVersion(); ok {
		m.OSVersion = v
	} else {
		_, m.OSVersion = detectDistro()
	}
	if data, err := os.ReadFile("/etc/wendyos/device-type"); err == nil {
		m.DeviceType, _ = parseDeviceType(string(data))
	}
	return m
}
