package models

import "fmt"

// ExternalDevice represents a device managed by a pluggable provider (local, Docker, ADB, etc.).
type ExternalDevice struct {
	ID              string            `json:"id"`
	DisplayName     string            `json:"displayName"`
	ProviderKey     string            `json:"providerKey"`
	ConnectionInfo  map[string]string `json:"connectionInfo,omitempty"`
	IsWendyDevice   bool              `json:"isWendyDevice"`
	AgentVersion    string            `json:"agentVersion,omitempty"`
	OS              string            `json:"os,omitempty"`
	OSVersion       string            `json:"osVersion,omitempty"`
	CPUArchitecture string            `json:"cpuArchitecture,omitempty"`
}

// HumanReadable returns a human-friendly string describing this external device.
func (d ExternalDevice) HumanReadable() string {
	s := d.DisplayName
	if s == "" {
		s = d.ID
	}
	if d.ProviderKey != "" {
		s += fmt.Sprintf(" (%s)", d.ProviderKey)
	}
	if d.AgentVersion != "" {
		s += " v" + d.AgentVersion
	}
	return s
}
