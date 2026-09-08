package cdi

import (
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/wendylabsinc/wendy/go/internal/agent/oci"
)

// ErrDevicesUnresolved reports that a CDI device exists but one or more of the
// host device nodes it names could not be resolved. It marks the difference
// between provisioning that is absent — normal on a host without nvidia-ctk,
// and survivable, since the gpu entitlement injects NVIDIA nodes by itself —
// and provisioning that is present and broken, which yields a container
// holding devices that point at nothing.
var ErrDevicesUnresolved = errors.New("CDI device nodes could not be resolved on this host")

// ErrDeviceNotFound reports that a CDI spec does not carry a device under the
// requested name. It is distinct from a device that exists but cannot be
// applied: callers probe well-known names ("all") and must be able to tell an
// expected miss from real provisioning trouble.
var ErrDeviceNotFound = errors.New("device not found in CDI spec")

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
		return fmt.Errorf("device %q: %w", deviceName, ErrDeviceNotFound)
	}

	// Apply global container edits if present.
	var errs []error
	if cdiSpec.ContainerEdits != nil {
		errs = append(errs, applyContainerEdits(spec, cdiSpec.ContainerEdits))
	}

	// Apply device-specific container edits.
	edits := &device.ContainerEdits
	errs = append(errs, applyContainerEdits(spec, edits))

	// Edits are applied even when some nodes fail to resolve, so a caller that
	// treats device provisioning as best-effort (a display-only app) still gets
	// the library mounts and env. A caller that cannot function without the
	// device (a gpu entitlement) must fail on this error rather than start a
	// container that is missing the hardware it asked for.
	return errors.Join(errs...)
}

func applyContainerEdits(spec *oci.Spec, edits *CDIContainerEdits) error {
	var errs []error

	// 1. Add device nodes.
	for _, node := range edits.DeviceNodes {
		major, minor, err := resolveDeviceNumbers(&node)
		if err != nil {
			// Never substitute a placeholder pair for a node we could not
			// resolve. Returning 0,0 here (the previous behaviour) injected a
			// container device pointing at nothing plus a cgroup rule allowing
			// it, so a GPU app started cleanly and then died at its first CUDA
			// call with nothing in the log naming the cause. Skip the node and
			// report it instead.
			errs = append(errs, fmt.Errorf("%w: %w", ErrDevicesUnresolved, err))
			continue
		}

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
		// Record where this pair came from, so a boot that renumbers the device
		// can be repaired instead of leaving the container pointing at nothing.
		// An explicitly numbered node without a host source is valid CDI.
		// Only mark it static if absent at provisioning; an existing host
		// device must still be refreshed and checked on subsequent starts.
		_, sourceErr := os.Stat(node.EffectiveHostPath())
		static := node.HostPath == "" && node.Major != nil && node.Minor != nil && os.IsNotExist(sourceErr)
		oci.RecordPinnedDeviceMapping(spec, oci.PinnedDevice{
			Path: node.Path, HostPath: node.EffectiveHostPath(), Type: deviceType,
			Major: int64(major), Minor: int64(minor), Static: static,
		})

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

	return errors.Join(errs...)
}

// resolveDeviceNumbers returns major/minor numbers for a CDI device node.
// If the CDI spec provides them, those are used; otherwise, stat(2) is called
// on the host path.
//
// A stat failure is an error, never a (0, 0) result: the numbers land in the
// container's OCI spec, and a spec that names a device by a number pair the
// host does not have is indistinguishable, from inside the container, from
// having no GPU at all.
func resolveDeviceNumbers(node *CDIDeviceNode) (major, minor int, err error) {
	if node.Major != nil && node.Minor != nil {
		return *node.Major, *node.Minor, nil
	}

	deviceType := node.Type
	if deviceType == "" {
		deviceType = "c"
	}
	maj, min, resolveErr := oci.ResolveDeviceNode(node.EffectiveHostPath(), deviceType, true)
	if resolveErr != nil {
		return 0, 0, fmt.Errorf("resolving device numbers for %s: %w", node.EffectiveHostPath(), resolveErr)
	}
	return int(maj), int(min), nil
}
