// Package onboarding holds the host installation contract shared by CLI and MCP.
package onboarding

import (
	"context"
	"errors"
)

var ErrArtifactUnavailable = errors.New("installation artifact unavailable")

type Options struct {
	DeviceType string `json:"device_type"`
	Carrier    string `json:"carrier,omitempty"`
	Version    string `json:"version,omitempty"`
	Storage    string `json:"storage,omitempty"`
	Drive      string `json:"drive,omitempty"`
	RootfsOnly bool   `json:"rootfs_only,omitempty"`
}

type Drive struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Capacity  int64  `json:"capacity_bytes"`
	Removable bool   `json:"removable"`
}

type Plan struct {
	DeviceType     string   `json:"device_type"`
	HostOS         string   `json:"host_os"`
	Method         string   `json:"method"`
	Version        string   `json:"version,omitempty"`
	Storage        string   `json:"storage,omitempty"`
	Target         *Drive   `json:"target,omitempty"`
	EraseScope     string   `json:"erase_scope"`
	ArtifactURL    string   `json:"artifact_url,omitempty"`
	ArtifactSHA256 string   `json:"artifact_sha256,omitempty"`
	Command        []string `json:"command,omitempty"`
	Requirements   []string `json:"requirements"`
	NextSteps      []string `json:"next_steps"`
	Documentation  string   `json:"documentation"`
}

// Backend supplies host-dependent operations without importing command code
// into the MCP server. Plan and Drives are read-only. Start records a job;
// Resume can launch a write after an observed target is explicitly authorized.
type Backend struct {
	Plan   func(context.Context, Options) (*Plan, error)
	Drives func() ([]Drive, error)
	Start  func(context.Context, StartOptions) (*Job, error)
	Status func(context.Context, string) (*Job, error)
	Resume func(context.Context, ResumeOptions) (*Job, error)
}
