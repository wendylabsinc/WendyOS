// Package logfields defines the canonical snake_case keys used for structured
// zap log fields in the wendy-agent. Using these constants keeps attribute
// keys consistent across the agent so logs are queryable once exported as OTel
// records via services.TelemetryCore.
//
// Prefer these constants for the common, reused fields. Log errors with
// zap.Error(err) (key "error"). Keys are snake_case; add new constants here
// rather than repeating string literals at call sites.
package logfields

const (
	AppID         = "app_id"
	AppName       = "app_name"
	ContainerID   = "container_id"
	ContainerName = "container_name"
	ServiceName   = "service_name"
	Image         = "image"
	Path          = "path"
	Device        = "device"
	Hostname      = "hostname"
	SSID          = "ssid"
	Reason        = "reason"
	Method        = "method"
	Serial        = "serial"
	Digest        = "digest"
	Duration      = "duration"
	Size          = "size"
	ArtifactURL   = "artifact_url"
	Status        = "status"
	Address       = "address"
	Output        = "output"

	// Bluetooth link fields (WDY-3189).
	Adapter                       = "adapter"
	LinkType                      = "link_type"
	ReasonCode                    = "reason_code"
	SupervisionTimeoutMS          = "supervision_timeout_ms"
	RequestedSupervisionTimeoutMS = "requested_supervision_timeout_ms"
)
