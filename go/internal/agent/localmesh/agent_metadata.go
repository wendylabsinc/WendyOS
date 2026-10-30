package localmesh

// AgentMetadata is covered by the device manifest signature and lease.
type AgentMetadata struct {
	Version         string `json:"version" cbor:"v"`
	OS              string `json:"os" cbor:"os"`
	OSVersion       string `json:"osVersion" cbor:"osVersion"`
	DeviceType      string `json:"deviceType,omitempty" cbor:"deviceType,omitempty"`
	CPUArchitecture string `json:"cpuArchitecture,omitempty" cbor:"cpuArchitecture,omitempty"`
}
