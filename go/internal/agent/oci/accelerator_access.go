package oci

import (
	"reflect"
	"slices"
	"sort"
)

// AddMissingAcceleratorAccess grants gpu and npu access a spec holds none of, as a
// deploy would now, and returns the added paths. A held grant is never widened, so
// a node that comes and goes cannot become a start requirement; NVIDIA is left to
// its CDI refresh, as that driver creates some nodes only on first use.
func AddMissingAcceleratorAccess(spec *Spec, gpu, npu bool) []string {
	if spec == nil || spec.Process == nil || spec.Linux == nil {
		return nil
	}
	var added []string
	if gpu {
		if grant := hostGPUGrant(); grant != gpuGrantNVIDIA {
			added = append(added, addMissingAccess(spec, func(s *Spec) { applyGPUGrant(s, grant) })...)
		}
	}
	if npu {
		added = append(added, addMissingAccess(spec, applyNPU)...)
	}
	sort.Strings(added)
	return added
}

// addMissingAccess runs grant on a scratch spec holding spec's groups and env, so
// those come out as at creation, and adds it all unless spec holds any of it.
func addMissingAccess(spec *Spec, grant func(*Spec)) []string {
	fresh := &Spec{
		Process: &Process{
			User: User{AdditionalGids: slices.Clone(spec.Process.User.AdditionalGids)},
			Env:  slices.Clone(spec.Process.Env),
		},
		Linux: &Linux{Resources: &LinuxResources{}},
	}
	grant(fresh)

	held := make(map[string]bool, len(spec.Linux.Devices)+len(spec.Mounts))
	for _, d := range spec.Linux.Devices {
		held[d.Path] = true
	}
	for _, m := range spec.Mounts {
		held[m.Destination] = true
	}
	if slices.ContainsFunc(fresh.Linux.Devices, func(d LinuxDevice) bool { return held[d.Path] }) ||
		slices.ContainsFunc(fresh.Mounts, func(m Mount) bool { return held[m.Destination] }) {
		return nil
	}
	var added []string
	for _, d := range fresh.Linux.Devices {
		spec.Linux.Devices = append(spec.Linux.Devices, d)
		added = append(added, d.Path)
	}
	for _, m := range fresh.Mounts {
		spec.Mounts = append(spec.Mounts, m)
		added = append(added, m.Destination)
	}
	if len(added) == 0 {
		return nil
	}

	if spec.Linux.Resources == nil {
		spec.Linux.Resources = &LinuxResources{}
	}
	for _, rule := range fresh.Linux.Resources.Devices {
		if !slices.ContainsFunc(spec.Linux.Resources.Devices, func(r LinuxDeviceCgroup) bool { return reflect.DeepEqual(r, rule) }) {
			spec.Linux.Resources.Devices = append(spec.Linux.Resources.Devices, rule)
		}
	}
	for _, pin := range decodePinnedDevices(fresh) {
		if slices.Contains(added, pin.Path) {
			RecordPinnedDeviceMapping(spec, pin)
		}
	}
	spec.Process.User.AdditionalGids = fresh.Process.User.AdditionalGids
	spec.Process.Env = fresh.Process.Env
	return added
}
