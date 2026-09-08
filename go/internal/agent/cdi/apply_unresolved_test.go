package cdi

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/wendylabsinc/wendy/go/internal/agent/oci"
)

func emptySpec() *oci.Spec {
	return &oci.Spec{
		Process: &oci.Process{},
		Linux:   &oci.Linux{},
	}
}

// The regression this guards: a device node the host cannot resolve used to be
// injected as 0:0 with a matching cgroup allow rule, and the create succeeded.
// The container then held a device pointing at nothing, which from inside is
// indistinguishable from having no GPU — and nothing in the log said so.
func TestApplyCDIDevice_UnresolvableNodeErrorsAndInjectsNothing(t *testing.T) {
	absent := filepath.Join(t.TempDir(), "nvgpu-absent")
	spec := emptySpec()
	cdiSpec := &CDISpecification{
		Devices: []CDIDevice{{
			Name: "all",
			ContainerEdits: CDIContainerEdits{
				DeviceNodes: []CDIDeviceNode{{Path: absent, Type: "c"}},
			},
		}},
	}

	err := ApplyCDIDevice(spec, cdiSpec, "all")
	if err == nil {
		t.Fatal("ApplyCDIDevice = nil for an unresolvable device node; want an error")
	}
	if len(spec.Linux.Devices) != 0 {
		t.Errorf("spec gained devices %+v; want none — an unresolved node must not be injected at all", spec.Linux.Devices)
	}
	if spec.Linux.Resources != nil && len(spec.Linux.Resources.Devices) != 0 {
		t.Errorf("spec gained cgroup rules %+v; want none", spec.Linux.Resources.Devices)
	}
}

// Explicit numbers in the spec are honoured without touching the host, which is
// how a CDI spec pins a device the agent is not expected to stat.
func TestApplyCDIDevice_ExplicitNumbersNeedNoHostNode(t *testing.T) {
	major, minor := 195, 0
	spec := emptySpec()
	cdiSpec := &CDISpecification{
		Devices: []CDIDevice{{
			Name: "all",
			ContainerEdits: CDIContainerEdits{
				DeviceNodes: []CDIDeviceNode{{
					Path: filepath.Join(t.TempDir(), "nvidia0"), Type: "c",
					Major: &major, Minor: &minor,
				}},
			},
		}},
	}

	if err := ApplyCDIDevice(spec, cdiSpec, "all"); err != nil {
		t.Fatalf("ApplyCDIDevice = %v for pinned numbers; want nil", err)
	}
	if len(spec.Linux.Devices) != 1 {
		t.Fatalf("spec.Linux.Devices = %+v; want the pinned device", spec.Linux.Devices)
	}
	if got := spec.Linux.Devices[0]; got.Major != 195 || got.Minor != 0 {
		t.Errorf("device = %d:%d; want 195:0", got.Major, got.Minor)
	}
}

// Mounts and env still land even when a node fails, so a display-only app —
// which the caller treats as best-effort — keeps its driver userspace.
func TestApplyCDIDevice_MountsSurviveAnUnresolvableNode(t *testing.T) {
	dir := t.TempDir()
	lib := filepath.Join(dir, "libcuda.so.1")
	if err := os.WriteFile(lib, []byte("x"), 0o600); err != nil {
		t.Fatalf("writing fixture: %v", err)
	}
	spec := emptySpec()
	cdiSpec := &CDISpecification{
		Devices: []CDIDevice{{
			Name: "all",
			ContainerEdits: CDIContainerEdits{
				DeviceNodes: []CDIDeviceNode{{Path: filepath.Join(dir, "nvgpu-absent"), Type: "c"}},
				Mounts:      []CDIMount{{HostPath: lib, ContainerPath: lib}},
				Env:         []string{"NVIDIA_VISIBLE_DEVICES=all"},
			},
		}},
	}

	err := ApplyCDIDevice(spec, cdiSpec, "all")
	if err == nil {
		t.Fatal("ApplyCDIDevice = nil; want the unresolved node reported")
	}
	if len(spec.Mounts) != 1 {
		t.Errorf("spec.Mounts = %+v; want the library mount applied despite the node failure", spec.Mounts)
	}
	if len(spec.Process.Env) != 1 {
		t.Errorf("spec.Process.Env = %v; want the CDI env applied", spec.Process.Env)
	}
}

// Probing a well-known device name that the spec does not carry is an expected
// miss, and the caller distinguishes it from real trouble.
func TestApplyCDIDevice_MissingNameIsErrDeviceNotFound(t *testing.T) {
	cdiSpec := &CDISpecification{Devices: []CDIDevice{{Name: "igpu0"}}}
	err := ApplyCDIDevice(emptySpec(), cdiSpec, "all")
	if !errors.Is(err, ErrDeviceNotFound) {
		t.Errorf("ApplyCDIDevice = %v; want it to wrap ErrDeviceNotFound", err)
	}
}

// The two failure kinds must stay distinguishable: an absent spec is normal on
// a host without nvidia-ctk (the gpu entitlement injects NVIDIA nodes itself),
// while a spec whose device nodes will not resolve is a broken hand-off that
// callers must be able to refuse.
func TestApplyCDIDevice_UnresolvedNodesAreDistinguishableFromAMissingDevice(t *testing.T) {
	absent := filepath.Join(t.TempDir(), "nvgpu-absent")
	cdiSpec := &CDISpecification{
		Devices: []CDIDevice{{
			Name: "all",
			ContainerEdits: CDIContainerEdits{
				DeviceNodes: []CDIDeviceNode{{Path: absent, Type: "c"}},
			},
		}},
	}

	err := ApplyCDIDevice(emptySpec(), cdiSpec, "all")
	if !errors.Is(err, ErrDevicesUnresolved) {
		t.Errorf("ApplyCDIDevice = %v; want it to wrap ErrDevicesUnresolved", err)
	}
	if errors.Is(err, ErrDeviceNotFound) {
		t.Error("unresolved device nodes reported as ErrDeviceNotFound; the two cases must stay distinct")
	}

	notFound := ApplyCDIDevice(emptySpec(), cdiSpec, "igpu0")
	if errors.Is(notFound, ErrDevicesUnresolved) {
		t.Error("a missing device name reported as ErrDevicesUnresolved; that would fail creates on hosts with no NVIDIA provisioning")
	}
}

func TestApplyCDIDevice_RefreshUsesHostSource(t *testing.T) {
	spec := emptySpec()
	path := filepath.Join(t.TempDir(), "container-only-name")
	cdiSpec := &CDISpecification{Devices: []CDIDevice{{Name: "all", ContainerEdits: CDIContainerEdits{
		DeviceNodes: []CDIDeviceNode{{Path: path, HostPath: os.DevNull, Type: "c"}},
	}}}}
	if err := ApplyCDIDevice(spec, cdiSpec, "all"); err != nil {
		t.Fatal(err)
	}
	// The annotation must survive containerd's JSON round trip.
	encoded, err := json.Marshal(spec)
	if err != nil {
		t.Fatal(err)
	}
	var restored oci.Spec
	if err := json.Unmarshal(encoded, &restored); err != nil {
		t.Fatal(err)
	}
	result := oci.RefreshHostDeviceNumbers(&restored)
	if len(result.Missing) > 0 {
		t.Fatalf("valid host source rejected: %+v", result)
	}
	if restored.Linux.Devices[0].Path != path {
		t.Fatal("container destination changed")
	}
}

func TestApplyCDIDevice_ExplicitOnlyNodeSurvivesRefresh(t *testing.T) {
	spec := emptySpec()
	major, minor := 195, 0
	cdiSpec := &CDISpecification{Devices: []CDIDevice{{Name: "all", ContainerEdits: CDIContainerEdits{
		DeviceNodes: []CDIDeviceNode{{Path: filepath.Join(t.TempDir(), "synthetic"), Type: "c", Major: &major, Minor: &minor}},
	}}}}
	if err := ApplyCDIDevice(spec, cdiSpec, "all"); err != nil {
		t.Fatal(err)
	}
	if result := oci.RefreshHostDeviceNumbers(spec); len(result.Missing) > 0 || result.Changed() {
		t.Fatalf("explicit node changed: %+v", result)
	}
}

func TestApplyCDIDevice_SymlinkSurvivesRefresh(t *testing.T) {
	path := filepath.Join(t.TempDir(), "device")
	if err := os.Symlink(os.DevNull, path); err != nil {
		t.Fatal(err)
	}
	spec := emptySpec()
	cdiSpec := &CDISpecification{Devices: []CDIDevice{{Name: "all", ContainerEdits: CDIContainerEdits{DeviceNodes: []CDIDeviceNode{{Path: path, Type: "c"}}}}}}
	if err := ApplyCDIDevice(spec, cdiSpec, "all"); err != nil {
		t.Fatal(err)
	}
	result := oci.RefreshHostDeviceNumbers(spec)
	if len(result.Errors) > 0 || len(result.Missing) > 0 || result.Changed() {
		t.Fatalf("valid mapping rejected: %+v", result)
	}
}
