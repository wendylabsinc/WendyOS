package cdi

import (
	"fmt"
	"strings"
	"syscall"

	"github.com/wendylabsinc/wendy/internal/agent/oci"
)

// ApplyCDIDevice applies a named CDI device from a CDI specification to an OCI spec.
// It adds device nodes, mounts, environment variables, and hooks.
func ApplyCDIDevice(spec *oci.Spec, cdiSpec *CDISpecification, deviceName string) error {
	// Find the device in the CDI spec.
	var device *CDIDevice
	for i := range cdiSpec.Devices {
		if cdiSpec.Devices[i].Name == deviceName {
			device = &cdiSpec.Devices[i]
			break
		}
	}
	if device == nil {
		return &CDIError{Message: fmt.Sprintf("device '%s' not found in CDI spec", deviceName)}
	}

	// Apply global container edits if present.
	if cdiSpec.ContainerEdits != nil {
		applyContainerEdits(spec, cdiSpec.ContainerEdits)
	}

	// Apply device-specific container edits.
	edits := &device.ContainerEdits
	applyContainerEdits(spec, edits)

	return nil
}

func applyContainerEdits(spec *oci.Spec, edits *CDIContainerEdits) {
	// 1. Add device nodes.
	for _, node := range edits.DeviceNodes {
		major, minor := resolveDeviceNumbers(&node)

		deviceType := node.Type
		if deviceType == "" {
			deviceType = "c"
		}

		ociDevice := oci.LinuxDevice{
			Path:  node.Path,
			Type:  deviceType,
			Major: int64(major),
			Minor: int64(minor),
		}
		spec.Linux.Devices = append(spec.Linux.Devices, ociDevice)

		// Add cgroup device allowance.
		if spec.Linux.Resources == nil {
			spec.Linux.Resources = &oci.LinuxResources{}
		}
		majorI64 := int64(major)
		minorI64 := int64(minor)
		spec.Linux.Resources.Devices = append(spec.Linux.Resources.Devices, oci.LinuxDeviceCgroup{
			Allow:  true,
			Type:   deviceType,
			Major:  &majorI64,
			Minor:  &minorI64,
			Access: "rwm",
		})
	}

	// 2. Add mounts.
	for _, mount := range edits.Mounts {
		mountType := mount.Type
		if mountType == "" {
			mountType = "bind"
		}
		options := mount.Options
		if options == nil {
			options = []string{"rbind", "nosuid", "nodev", "ro"}
		}
		spec.Mounts = append(spec.Mounts, oci.Mount{
			Destination: mount.ContainerPath,
			Type:        mountType,
			Source:      mount.HostPath,
			Options:     options,
		})
	}

	// 3. Add environment variables.
	if len(edits.Env) > 0 {
		spec.Process.Env = append(spec.Process.Env, edits.Env...)
	}

	// 4. Add hooks.
	for _, cdiHook := range edits.Hooks {
		ociHook := oci.Hook{
			Path: cdiHook.Path,
			Args: cdiHook.Args,
			Env:  cdiHook.Env,
		}
		if cdiHook.Timeout != nil {
			ociHook.Timeout = cdiHook.Timeout
		}

		if spec.Hooks == nil {
			spec.Hooks = &oci.Hooks{}
		}

		switch strings.ToLower(cdiHook.HookName) {
		case "prestart":
			spec.Hooks.Prestart = append(spec.Hooks.Prestart, ociHook)
		case "createruntime":
			spec.Hooks.CreateRuntime = append(spec.Hooks.CreateRuntime, ociHook)
		case "createcontainer":
			spec.Hooks.CreateContainer = append(spec.Hooks.CreateContainer, ociHook)
		case "startcontainer":
			spec.Hooks.StartContainer = append(spec.Hooks.StartContainer, ociHook)
		case "poststart":
			spec.Hooks.Poststart = append(spec.Hooks.Poststart, ociHook)
		case "poststop":
			spec.Hooks.Poststop = append(spec.Hooks.Poststop, ociHook)
		}
	}
}

// resolveDeviceNumbers returns major/minor numbers for a CDI device node.
// If the CDI spec provides them, those are used; otherwise, stat(2) is called
// on the host path.
func resolveDeviceNumbers(node *CDIDeviceNode) (major, minor int) {
	if node.Major != nil && node.Minor != nil {
		return *node.Major, *node.Minor
	}

	devicePath := node.EffectiveHostPath()

	var st syscall.Stat_t
	if err := syscall.Stat(devicePath, &st); err != nil {
		return 0, 0
	}

	// Extract major/minor from st.Rdev.
	// On Linux: major = (rdev >> 8) & 0xfff, minor = (rdev & 0xff) | ((rdev >> 12) & 0xfff00)
	// On macOS: major = (rdev >> 24) & 0xff, minor = rdev & 0xffffff
	rdev := uint64(st.Rdev)
	major = int((rdev >> 8) & 0xfff)
	minor = int((rdev & 0xff) | ((rdev >> 12) & 0xfff00))

	return major, minor
}
