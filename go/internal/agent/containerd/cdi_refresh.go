package containerd

import (
	"context"
	"encoding/json"
	"fmt"

	containerd "github.com/containerd/containerd/v2/client"
	"go.uber.org/zap"

	"github.com/wendylabsinc/wendy/go/internal/agent/cdi"
	localoci "github.com/wendylabsinc/wendy/go/internal/agent/oci"
	"github.com/wendylabsinc/wendy/go/internal/shared/appconfig"
)

// loadNvidiaCDISpec reads the host's NVIDIA CDI spec. Behind a var so tests
// can stand in for a host that has one.
var loadNvidiaCDISpec = func() (*cdi.CDISpecification, string, error) {
	return cdi.NewManager().LoadNVIDIACDISpec()
}

// nvidiaCDIDeviceName returns the device applyNvidiaCDISpec applies from
// cdiSpec: the one named "all", which nvidia-ctk generates in CSV mode, or
// failing that the first.
func nvidiaCDIDeviceName(cdiSpec *cdi.CDISpecification) (string, bool) {
	if len(cdiSpec.Devices) == 0 {
		return "", false
	}
	for _, device := range cdiSpec.Devices {
		if device.Name == "all" {
			return device.Name, true
		}
	}
	return cdiSpec.Devices[0].Name, true
}

// refreshNvidiaCDIForStart brings the NVIDIA driver files, hooks and
// variables in a container's stored spec in line with the host's current CDI
// spec, and persists the spec when they differ.
//
// The stored spec carries them as they were when the container was created:
// one bind mount per driver file, by exact name. An OS update renames those
// files, and the container definition outlives the update, because containerd
// persists it and recreates only the task. The runtime then refuses to create
// the container over the first file it cannot find, on every restart, until
// someone redeploys the app. The host regenerates its CDI spec on every boot,
// so the current list is always there to compare against.
//
// This is a repair, not a gate: when it cannot run or cannot be saved, the
// start goes ahead with the stored spec and the runtime has the last word,
// exactly as before.
func (c *Client) refreshNvidiaCDIForStart(ctx context.Context, container containerd.Container, appName string, labels map[string]string) {
	appCfg := &appconfig.AppConfig{Entitlements: parseEntitlementsFromAnnotations(labels)}
	if !needsNvidiaCDI(appCfg) {
		return
	}

	cdiSpec, specPath, err := loadNvidiaCDISpec()
	if err != nil {
		// No spec at all is the normal state of a board without NVIDIA
		// provisioning, and of one on the L4T CSV fallback. Either way there is
		// no list to compare the stored spec against.
		if specPath != "" {
			c.logger.Warn("Could not load NVIDIA CDI spec to refresh driver files; starting with the stored spec",
				zap.String("app_name", appName), zap.String("cdi_spec_path", specPath), zap.Error(err))
		}
		return
	}
	device, ok := nvidiaCDIDeviceName(cdiSpec)
	if !ok {
		return
	}

	info, err := container.Info(ctx)
	if err != nil {
		c.logger.Warn("Could not load container record to refresh NVIDIA driver files; starting with the stored spec",
			zap.String("app_name", appName), zap.Error(err))
		return
	}
	if info.Spec == nil {
		return
	}
	var spec localoci.Spec
	if err := json.Unmarshal(info.Spec.GetValue(), &spec); err != nil {
		c.logger.Warn("Could not decode stored spec to refresh NVIDIA driver files; starting with the stored spec",
			zap.String("app_name", appName), zap.Error(err))
		return
	}

	// An error here names device nodes the list has and the host does not.
	// The rest of the refresh is still applied, and the next start tries them
	// again, so it is reported every time and not only when the spec changes.
	refresh, deviceErr := cdi.RefreshAppliedEdits(&spec, cdiSpec, device)
	if deviceErr != nil {
		c.logger.Warn("NVIDIA CDI spec names device nodes this host does not have; they were not added",
			zap.String("app_name", appName), zap.String("cdi_spec_path", specPath), zap.Error(deviceErr))
	}
	if !refresh.Changed {
		return
	}

	// Creation validates the assembled spec before the runtime sees it
	// (WDY-1102), and the refresh puts mounts into a spec too. The host's CDI
	// file is trusted, but a spec that fails the same check is not written.
	if err := localoci.ValidateMounts(&spec); err != nil {
		c.logger.Warn("Refusing to persist refreshed NVIDIA driver files; starting with the stored spec",
			zap.String("app_name", appName), zap.String("cdi_spec_path", specPath), zap.Error(err))
		return
	}

	specJSON, err := marshalRefreshedCDISpec(info.Spec.GetValue(), &spec, len(refresh.DevicesAdded) > 0)
	if err == nil {
		err = c.persistSpecJSON(ctx, container, info, specJSON)
	}
	if err != nil {
		c.logger.Warn("Could not persist refreshed NVIDIA driver files; starting with the stored spec",
			zap.String("app_name", appName), zap.Error(err))
		return
	}

	c.logger.Info("Refreshed NVIDIA driver files before start",
		zap.String("app_name", appName), zap.String("cdi_spec_path", specPath),
		zap.Int("mounts_removed", refresh.MountsRemoved), zap.Int("mounts_added", refresh.MountsAdded),
		zap.Bool("hooks_changed", refresh.HooksChanged), zap.Bool("env_changed", refresh.EnvChanged),
		zap.Strings("devices_added", refresh.DevicesAdded),
		// True once per container created by an agent that kept no record.
		zap.Bool("inferred", refresh.Inferred))
}

// marshalRefreshedCDISpec writes what a CDI refresh can change back into the
// stored document, leaving every other field as it was, including the ones
// the agent's spec type does not model. The device list and its rules are
// rewritten only when devicesAdded says they changed.
func marshalRefreshedCDISpec(original []byte, spec *localoci.Spec, devicesAdded bool) ([]byte, error) {
	if devicesAdded {
		withDevices, err := marshalRefreshedDeviceSpec(original, spec)
		if err != nil {
			return nil, err
		}
		original = withDevices
	}

	var document, process map[string]json.RawMessage
	if err := json.Unmarshal(original, &document); err != nil {
		return nil, err
	}
	if document == nil {
		return nil, fmt.Errorf("stored spec is not an object")
	}

	set := func(object map[string]json.RawMessage, key string, value any, empty bool) error {
		if empty {
			delete(object, key)
			return nil
		}
		raw, err := json.Marshal(value)
		if err != nil {
			return err
		}
		object[key] = raw
		return nil
	}

	if err := set(document, "mounts", spec.Mounts, len(spec.Mounts) == 0); err != nil {
		return nil, err
	}
	if err := set(document, "hooks", spec.Hooks, spec.Hooks == nil); err != nil {
		return nil, err
	}
	if err := set(document, "annotations", spec.Annotations, len(spec.Annotations) == 0); err != nil {
		return nil, err
	}
	if spec.Process != nil && len(document["process"]) > 0 {
		if err := json.Unmarshal(document["process"], &process); err != nil {
			return nil, err
		}
		if process != nil {
			if err := set(process, "env", spec.Process.Env, len(spec.Process.Env) == 0); err != nil {
				return nil, err
			}
			if err := set(document, "process", process, false); err != nil {
				return nil, err
			}
		}
	}
	return json.Marshal(document)
}
