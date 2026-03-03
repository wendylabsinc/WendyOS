// Package cdi provides Container Device Interface (CDI) specification management.
package cdi

// CDISpecification represents a CDI spec document.
type CDISpecification struct {
	CDIVersion     string             `json:"cdiVersion" yaml:"cdiVersion"`
	Kind           string             `json:"kind" yaml:"kind"`
	ContainerEdits *CDIContainerEdits `json:"containerEdits,omitempty" yaml:"containerEdits,omitempty"`
	Devices        []CDIDevice        `json:"devices" yaml:"devices"`
}

// CDIDevice represents a single device entry in a CDI spec.
type CDIDevice struct {
	Name           string            `json:"name" yaml:"name"`
	ContainerEdits CDIContainerEdits `json:"containerEdits" yaml:"containerEdits"`
}

// CDIContainerEdits holds the edits to apply to a container for a device.
type CDIContainerEdits struct {
	DeviceNodes []CDIDeviceNode `json:"deviceNodes,omitempty" yaml:"deviceNodes,omitempty"`
	Mounts      []CDIMount      `json:"mounts,omitempty" yaml:"mounts,omitempty"`
	Env         []string        `json:"env,omitempty" yaml:"env,omitempty"`
	Hooks       []CDIHook       `json:"hooks,omitempty" yaml:"hooks,omitempty"`
}

// CDIDeviceNode describes a device node to expose in the container.
type CDIDeviceNode struct {
	Path        string `json:"path" yaml:"path"`
	HostPath    string `json:"hostPath,omitempty" yaml:"hostPath,omitempty"`
	Type        string `json:"type,omitempty" yaml:"type,omitempty"`
	Major       *int   `json:"major,omitempty" yaml:"major,omitempty"`
	Minor       *int   `json:"minor,omitempty" yaml:"minor,omitempty"`
	FileMode    *int   `json:"fileMode,omitempty" yaml:"fileMode,omitempty"`
	Permissions string `json:"permissions,omitempty" yaml:"permissions,omitempty"`
}

// EffectiveHostPath returns the host path, defaulting to Path if HostPath is empty.
func (n *CDIDeviceNode) EffectiveHostPath() string {
	if n.HostPath != "" {
		return n.HostPath
	}
	return n.Path
}

// CDIMount describes a mount to add to the container.
type CDIMount struct {
	HostPath      string   `json:"hostPath" yaml:"hostPath"`
	ContainerPath string   `json:"containerPath" yaml:"containerPath"`
	Type          string   `json:"type,omitempty" yaml:"type,omitempty"`
	Options       []string `json:"options,omitempty" yaml:"options,omitempty"`
}

// CDIHook describes a hook to run at a container lifecycle stage.
type CDIHook struct {
	HookName string   `json:"hookName" yaml:"hookName"`
	Path     string   `json:"path" yaml:"path"`
	Args     []string `json:"args,omitempty" yaml:"args,omitempty"`
	Env      []string `json:"env,omitempty" yaml:"env,omitempty"`
	Timeout  *int     `json:"timeout,omitempty" yaml:"timeout,omitempty"`
}

// CDIDeviceInfo provides information about an available CDI device.
type CDIDeviceInfo struct {
	Identifier  string
	Category    string
	Description string
	DevicePaths []string
}

// CDIError represents errors from CDI operations.
type CDIError struct {
	Message string
}

func (e *CDIError) Error() string {
	return e.Message
}
