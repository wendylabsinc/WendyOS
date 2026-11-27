package oci

import (
	"encoding/json"
	"path/filepath"
	"reflect"
	"slices"
	"testing"

	"github.com/wendylabsinc/wendy/go/internal/agent/board"
)

func withNVIDIAGPUDiscovered(t *testing.T, present bool) {
	t.Helper()
	orig := nvidiaGPUDiscovered
	nvidiaGPUDiscovered = func() bool { return present }
	t.Cleanup(func() { nvidiaGPUDiscovered = orig })
}

// withoutNVIDIANodes leaves the host with no live NVIDIA node and no /dev/kfd.
func withoutNVIDIANodes(t *testing.T) {
	t.Helper()
	origGlobs, origKfd := nvidiaDeviceGlobs, kfdDevicePath
	t.Cleanup(func() { nvidiaDeviceGlobs = origGlobs; kfdDevicePath = origKfd })
	nvidiaDeviceGlobs = []string{filepath.Join(t.TempDir(), "no-nvidia-*")}
	kfdDevicePath = filepath.Join(t.TempDir(), "no-kfd")
}

func withBoard(t *testing.T, kind board.Kind) {
	t.Helper()
	orig := boardDetect
	boardDetect = func() board.Info { return board.Info{Kind: kind} }
	t.Cleanup(func() { boardDetect = orig })
}

func cloneSpec(t *testing.T, spec *Spec) *Spec {
	t.Helper()
	raw, err := json.Marshal(spec)
	if err != nil {
		t.Fatal(err)
	}
	var out Spec
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatal(err)
	}
	return &out
}

// An app deployed by an agent that took an Adreno for an NVIDIA GPU (or before
// msm loaded) holds the five synthetic entries; a start must swap them for the
// render node a fresh deploy gets.
func TestAcceleratorAccess_LegacyNVIDIAOnQualcommGainsRenderNode(t *testing.T) {
	dev := installFakeQualcommDevTree(t, map[string][2]int64{"renderD128": {226, 128}})
	render := filepath.Join(dev, "dri", "renderD128")
	withNVIDIAGPUDiscovered(t, false)
	withoutNVIDIANodes(t)
	withBoard(t, board.Generic)
	withStubbedStat(t, map[string][2]int64{render: {226, 128}})

	spec := DefaultSpec("/rootfs", []string{"/bin/sh"})
	applyGPUGrant(spec, gpuGrantNVIDIA)
	if len(spec.Linux.Devices) != 5 {
		t.Fatalf("legacy shape not reproduced: %+v", spec.Linux.Devices)
	}

	refresh := RefreshHostDeviceNumbers(spec)
	if len(refresh.Missing) != 0 || len(refresh.Removed) != 5 {
		t.Fatalf("legacy entries not removed: %+v", refresh)
	}
	added := AddMissingAcceleratorAccess(spec, true, false)
	if !reflect.DeepEqual(added, []string{render}) {
		t.Fatalf("added %v, want the render node", added)
	}
	if d, ok := deviceForPath(spec, render); !ok || d.Major != 226 || d.Minor != 128 {
		t.Fatalf("render node entry: %+v %v", d, ok)
	}
	if !ruleFor(t, spec, 226, 128) || hasMajorRule(spec, 195) {
		t.Fatalf("rules: %+v", spec.Linux.Resources.Devices)
	}
	if !slices.Contains(spec.Process.User.AdditionalGids, 107) || !slices.Contains(spec.Process.User.AdditionalGids, videoGroupGID) {
		t.Fatalf("groups: %v", spec.Process.User.AdditionalGids)
	}
	if !slices.ContainsFunc(decodePinnedDevices(spec), func(p PinnedDevice) bool { return p.Path == render }) {
		t.Fatal("render node not pinned, so a later boot could not re-resolve it")
	}

	if again := AddMissingAcceleratorAccess(spec, true, false); again != nil {
		t.Fatalf("second start added %v", again)
	}
	if again := RefreshHostDeviceNumbers(spec); again.SpecModified() || len(again.Missing) != 0 {
		t.Fatalf("granted spec does not refresh cleanly: %+v", again)
	}
}

// A discovered NVIDIA GPU beside another vendor's keeps the entries: its module
// may load later, and the start preflight still names them.
func TestAcceleratorAccess_LegacyNVIDIAKeptBesideAnNVIDIAGPU(t *testing.T) {
	installFakeQualcommDevTree(t, map[string][2]int64{"renderD128": {226, 128}})
	withNVIDIAGPUDiscovered(t, true)
	withoutNVIDIANodes(t)
	withStubbedStat(t, map[string][2]int64{})

	spec := DefaultSpec("/rootfs", []string{"/bin/sh"})
	applyGPUGrant(spec, gpuGrantNVIDIA)
	refresh := RefreshHostDeviceNumbers(spec)
	if len(refresh.Removed) != 0 || len(refresh.Missing) != 5 {
		t.Fatalf("entries for a real NVIDIA GPU were dropped: %+v", refresh)
	}
}

// A host whose GPU may be NVIDIA's gets nothing at start: its CDI refresh owns
// that grant, and the no-driver fallback would add nodes the host lacks.
func TestAcceleratorAccess_NVIDIAGrantAddsNothing(t *testing.T) {
	withNVIDIAGPUDiscovered(t, false)
	withoutNVIDIANodes(t)
	withBoard(t, board.Generic)
	withoutQualcommGPU(t)

	spec := DefaultSpec("/rootfs", []string{"/bin/sh"})
	before := cloneSpec(t, spec)
	if added := AddMissingAcceleratorAccess(spec, true, false); added != nil {
		t.Fatalf("added %v on an NVIDIA-grant host", added)
	}
	if !reflect.DeepEqual(spec, before) {
		t.Fatal("spec changed although nothing was added")
	}
}

func withoutQualcommGPU(t *testing.T) {
	t.Helper()
	orig := qualcommGPUPresent
	qualcommGPUPresent = func() bool { return false }
	t.Cleanup(func() { qualcommGPUPresent = orig })
}

func TestAcceleratorAccess_PiGainsVCIO(t *testing.T) {
	source := installFakeVCIO(t, 249)
	withoutQualcommGPU(t)
	withNVIDIAGPUDiscovered(t, false)
	withoutNVIDIANodes(t)

	spec := DefaultSpec("/rootfs", []string{"/bin/sh"})
	if added := AddMissingAcceleratorAccess(spec, true, false); !reflect.DeepEqual(added, []string{"/dev/vcio"}) {
		t.Fatalf("added %v", added)
	}
	if m, ok := mountForDest(spec, "/dev/vcio"); !ok || m.Source != source || !hasMajorRule(spec, 249) {
		t.Fatalf("VideoCore grant incomplete: %+v", spec)
	}
	if again := AddMissingAcceleratorAccess(spec, true, false); again != nil {
		t.Fatalf("second start added %v", again)
	}
}

func TestAcceleratorAccess_AMDGainsKFDAndRenderNode(t *testing.T) {
	dev := installFakeAMDDevTree(t, [2]int64{511, 0}, map[string][2]int64{"renderD128": {226, 128}})
	withNVIDIAGPUDiscovered(t, false)

	spec := DefaultSpec("/rootfs", []string{"/bin/sh"})
	added := AddMissingAcceleratorAccess(spec, true, false)
	kfd, render := filepath.Join(dev, "kfd"), filepath.Join(dev, "dri", "renderD128")
	if !reflect.DeepEqual(added, []string{render, kfd}) {
		t.Fatalf("added %v", added)
	}
	if !hasMount(spec, kfd) || !ruleFor(t, spec, 511, 0) || !ruleFor(t, spec, 226, 128) {
		t.Fatalf("AMD grant incomplete: %+v", spec)
	}
}

// A container created while the DSP was down: a start brings the nodes, the
// heap, their groups and the process attributes the image set, plus the bit
// the granted nodes need.
func TestAcceleratorAccess_FastRPCNodesAfterDSPCameUp(t *testing.T) {
	heap := [2]int64{252, 0}
	dir := installFakeFastrpcDevTree(t, map[string][2]int64{
		"fastrpc-cdsp":        {10, 262},
		"fastrpc-cdsp-secure": {10, 263},
	}, &heap)
	cdsp, secure, system := filepath.Join(dir, "fastrpc-cdsp"), filepath.Join(dir, "fastrpc-cdsp-secure"), filepath.Join(dir, "system")

	spec := DefaultSpec("/rootfs", []string{"/bin/sh"})
	spec.Process.Env = append(spec.Process.Env, fastrpcProcessAttrsEnv+"=1")
	added := AddMissingAcceleratorAccess(spec, false, true)
	if !reflect.DeepEqual(added, []string{cdsp, system}) {
		t.Fatalf("added %v", added)
	}
	if hasMount(spec, secure) {
		t.Fatal("signed-PD node granted")
	}
	if !ruleFor(t, spec, 10, 262) || !ruleFor(t, spec, 252, 0) || ruleFor(t, spec, 10, 263) {
		t.Fatalf("rules: %+v", spec.Linux.Resources.Devices)
	}
	if v, _ := envValue(spec.Process.Env, fastrpcProcessAttrsEnv); v != "9" {
		t.Fatalf("%s=%q, want the image's flag plus the unsigned bit", fastrpcProcessAttrsEnv, v)
	}
	if v, _ := envValue(spec.Process.Env, "MACHINE_NAME"); v != "Acme Board" {
		t.Fatalf("MACHINE_NAME=%q", v)
	}
	if !slices.Contains(spec.Process.User.AdditionalGids, 998) || !slices.Contains(spec.Process.User.AdditionalGids, 997) {
		t.Fatalf("groups: %v", spec.Process.User.AdditionalGids)
	}
	if !IsFastrpcNode(cdsp) || IsFastrpcNode(system) {
		t.Fatal("FastRPC node match")
	}

	before := cloneSpec(t, spec)
	if again := AddMissingAcceleratorAccess(spec, false, true); again != nil || !reflect.DeepEqual(spec, before) {
		t.Fatalf("second start changed the spec: added %v", again)
	}
}

// A container that already holds the grant, here through another entitlement,
// is left exactly as it is.
func TestAcceleratorAccess_HeldDeviceLeavesSpecAlone(t *testing.T) {
	dev := installFakeQualcommDevTree(t, map[string][2]int64{"renderD128": {226, 128}})
	withNVIDIAGPUDiscovered(t, false)
	withoutNVIDIANodes(t)
	withBoard(t, board.Generic)

	spec := DefaultSpec("/rootfs", []string{"/bin/sh"})
	spec.Linux.Devices = append(spec.Linux.Devices, LinuxDevice{Path: filepath.Join(dev, "dri", "renderD128"), Type: "c", Major: 226, Minor: 128})
	before := cloneSpec(t, spec)
	if added := AddMissingAcceleratorAccess(spec, true, false); added != nil || !reflect.DeepEqual(spec, before) {
		t.Fatalf("held grant changed: added %v", added)
	}
}

// A container holding part of a grant, such as one DSP that was up at deploy,
// keeps it as is: widening it would make a node that comes and goes a start
// requirement.
func TestAcceleratorAccess_PartialGrantNotWidened(t *testing.T) {
	dir := installFakeFastrpcDevTree(t, map[string][2]int64{
		"fastrpc-cdsp":  {10, 262},
		"fastrpc-gdsp0": {10, 264},
	}, nil)
	spec := DefaultSpec("/rootfs", []string{"/bin/sh"})
	spec.Mounts = append(spec.Mounts, Mount{Destination: filepath.Join(dir, "fastrpc-gdsp0"), Source: filepath.Join(dir, "fastrpc-gdsp0"), Type: "bind"})
	before := cloneSpec(t, spec)
	if added := AddMissingAcceleratorAccess(spec, false, true); added != nil || !reflect.DeepEqual(spec, before) {
		t.Fatalf("partial grant widened: added %v", added)
	}
}
