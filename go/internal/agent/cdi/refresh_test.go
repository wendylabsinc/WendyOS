package cdi

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/wendylabsinc/wendy/go/internal/agent/oci"
)

// vendorOptions is the option set nvidia-ctk writes on every library mount.
var vendorOptions = []string{"ro", "nosuid", "nodev", "rbind", "rprivate"}

func vendorMount(path string) CDIMount {
	return CDIMount{HostPath: path, ContainerPath: path, Options: vendorOptions}
}

func vendorSpec(edits CDIContainerEdits) *CDISpecification {
	return &CDISpecification{Devices: []CDIDevice{{Name: "all", ContainerEdits: edits}}}
}

func destinations(spec *oci.Spec) []string {
	out := make([]string, 0, len(spec.Mounts))
	for _, m := range spec.Mounts {
		out = append(out, m.Destination)
	}
	return out
}

// An OS update renames driver libraries. A container provisioned from the old
// list names files the host no longer has, and the runtime refuses to create it.
func TestRefreshAppliedEdits_ReplacesMountsTheHostNoLongerLists(t *testing.T) {
	spec := oci.DefaultSpec("rootfs", nil)
	before := vendorSpec(CDIContainerEdits{Mounts: []CDIMount{
		vendorMount("/usr/lib/libcublas.so.13.4.0.1"),
		vendorMount("/usr/lib/libnvinfer.so.10"),
	}})
	if err := ApplyCDIDevice(spec, before, "all"); err != nil {
		t.Fatal(err)
	}
	// An entitlement adds its own mounts after the vendor's.
	spec.Mounts = append(spec.Mounts, oci.Mount{
		Destination: "/dev/dri", Source: "/dev/dri", Type: "bind",
		Options: []string{"rbind", "rw", "nosuid", "noexec"},
	})

	after := vendorSpec(CDIContainerEdits{Mounts: []CDIMount{
		vendorMount("/usr/lib/libcublas.so.13.4.1.3"),
		vendorMount("/usr/lib/libnvinfer.so.10"),
	}})
	got, err := RefreshAppliedEdits(spec, after, "all")
	if err != nil {
		t.Fatal(err)
	}
	if !got.Changed {
		t.Fatal("refresh reported no change for a renamed library")
	}
	// One library was renamed and one kept, so one mount went and one came.
	if got.MountsRemoved != 1 || got.MountsAdded != 1 {
		t.Errorf("removed %d, added %d; want 1 and 1", got.MountsRemoved, got.MountsAdded)
	}

	dests := destinations(spec)
	if slices.Contains(dests, "/usr/lib/libcublas.so.13.4.0.1") {
		t.Error("the library the host no longer has is still mounted")
	}
	renamed := slices.Index(dests, "/usr/lib/libcublas.so.13.4.1.3")
	if renamed < 0 {
		t.Fatal("the renamed library was not mounted")
	}
	if n := len(dests) - len(slices.Compact(slices.Sorted(slices.Values(dests)))); n != 0 {
		t.Errorf("%d destinations are mounted twice: %v", n, dests)
	}
	// Later mounts shadow earlier ones, so the vendor block must stay where a
	// fresh deploy would have put it: ahead of the entitlement's mounts.
	if entitlement := slices.Index(dests, "/dev/dri"); entitlement < renamed {
		t.Errorf("vendor mounts moved behind the entitlement mount: %v", dests)
	}
}

// The record is what makes the refresh independent of how the vendor writes
// its mounts. A toolkit update that changes the options leaves the old mounts
// with nothing in common with the new ones except the record.
func TestRefreshAppliedEdits_FollowsTheRecordNotTheMountOptions(t *testing.T) {
	spec := oci.DefaultSpec("rootfs", nil)
	before := vendorSpec(CDIContainerEdits{Mounts: []CDIMount{{
		HostPath: "/usr/lib/libcublas.so.13.4.0.1", ContainerPath: "/usr/lib/libcublas.so.13.4.0.1",
		Options: []string{"ro", "nosuid", "nodev", "bind"},
	}}})
	if err := ApplyCDIDevice(spec, before, "all"); err != nil {
		t.Fatal(err)
	}

	after := vendorSpec(CDIContainerEdits{Mounts: []CDIMount{vendorMount("/usr/lib/libcublas.so.13.4.1.3")}})
	got, err := RefreshAppliedEdits(spec, after, "all")
	if err != nil {
		t.Fatal(err)
	}
	if got.Inferred {
		t.Error("a spec ApplyCDIDevice built was treated as having no record")
	}
	if slices.Contains(destinations(spec), "/usr/lib/libcublas.so.13.4.0.1") {
		t.Error("the recorded mount survived because its options differ from the current list's")
	}
}

// An entitlement may bind the same path the vendor lists, with its own
// options, and being later in the spec it is the one the app sees. The record
// names the vendor's mount only, so the entitlement's must survive a refresh.
func TestRefreshAppliedEdits_KeepsAnEntitlementMountAtAVendorPath(t *testing.T) {
	spec := oci.DefaultSpec("rootfs", nil)
	before := vendorSpec(CDIContainerEdits{Mounts: []CDIMount{
		vendorMount("/usr/lib/libcublas.so.13.4.0.1"),
		vendorMount("/etc/vulkan"),
	}})
	if err := ApplyCDIDevice(spec, before, "all"); err != nil {
		t.Fatal(err)
	}
	writable := oci.Mount{Destination: "/etc/vulkan", Source: "/etc/vulkan", Type: "bind", Options: []string{"rbind", "rw"}}
	spec.Mounts = append(spec.Mounts, writable)

	after := vendorSpec(CDIContainerEdits{Mounts: []CDIMount{
		vendorMount("/usr/lib/libcublas.so.13.4.1.3"),
		vendorMount("/etc/vulkan"),
	}})
	if _, err := RefreshAppliedEdits(spec, after, "all"); err != nil {
		t.Fatal(err)
	}
	last := spec.Mounts[len(spec.Mounts)-1]
	if last.Destination != "/etc/vulkan" || !slices.Equal(last.Options, writable.Options) {
		t.Errorf("the entitlement's mount is no longer the last word on /etc/vulkan: %+v", spec.Mounts)
	}
}

// Every GPU container passes through the refresh on every start. When the
// host's list has not changed there is nothing to persist.
func TestRefreshAppliedEdits_LeavesACurrentSpecAlone(t *testing.T) {
	spec := oci.DefaultSpec("rootfs", nil)
	current := vendorSpec(CDIContainerEdits{
		Mounts: []CDIMount{vendorMount("/usr/lib/libnvinfer.so.10")},
		Env:    []string{"NVIDIA_VISIBLE_DEVICES=void"},
		Hooks:  []CDIHook{{HookName: "createContainer", Path: "/usr/bin/nvidia-ctk", Args: []string{"nvidia-ctk", "hook", "update-ldcache"}}},
	})
	if err := ApplyCDIDevice(spec, current, "all"); err != nil {
		t.Fatal(err)
	}
	want, err := json.Marshal(spec)
	if err != nil {
		t.Fatal(err)
	}

	got, err := RefreshAppliedEdits(spec, current, "all")
	if err != nil {
		t.Fatal(err)
	}
	if got.Changed {
		t.Error("refresh reported a change although the host's list is the one the spec was built from")
	}
	after, err := json.Marshal(spec)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(want) {
		t.Errorf("spec was rewritten (%d bytes before, %d after)", len(want), len(after))
	}
}

// Every container deployed by an older agent has no record, and those are the
// containers an OS update strands. The vendor's mounts are recognised by the
// mount options the vendor's list uses, which nothing else the agent writes
// shares.
func TestRefreshAppliedEdits_RecognisesVendorMountsWithoutARecord(t *testing.T) {
	spec := oci.DefaultSpec("rootfs", nil)
	defaults := slices.Clone(spec.Mounts)
	bind := func(path string, options ...string) oci.Mount {
		return oci.Mount{Destination: path, Source: path, Type: "bind", Options: options}
	}
	spec.Mounts = append(spec.Mounts,
		bind("/usr/lib/libcublas.so.13.4.0.1", vendorOptions...),
		bind("/usr/lib/libgstrtspserver-1.0.so.0.2805.0", vendorOptions...),
		bind("/dev/dri", "rbind", "rw", "nosuid", "noexec"),
		// Read-only and executable like a vendor mount, but not the vendor's:
		// the Qualcomm runtime bind and the L4T CSV fallback write this shape.
		bind("/opt/wendyos/npu/lib", "rbind", "ro", "nosuid", "nodev"),
	)

	current := vendorSpec(CDIContainerEdits{Mounts: []CDIMount{vendorMount("/usr/lib/libcublas.so.13.4.1.3")}})
	got, err := RefreshAppliedEdits(spec, current, "all")
	if err != nil {
		t.Fatal(err)
	}
	if !got.Changed || !got.Inferred {
		t.Errorf("got %+v; want a change made by inference", got)
	}

	dests := destinations(spec)
	for _, stale := range []string{"/usr/lib/libcublas.so.13.4.0.1", "/usr/lib/libgstrtspserver-1.0.so.0.2805.0"} {
		if slices.Contains(dests, stale) {
			t.Errorf("%s is still mounted", stale)
		}
	}
	for _, kept := range []string{"/usr/lib/libcublas.so.13.4.1.3", "/dev/dri", "/opt/wendyos/npu/lib"} {
		if !slices.Contains(dests, kept) {
			t.Errorf("%s is not mounted", kept)
		}
	}
	if !slices.EqualFunc(spec.Mounts[:len(defaults)], defaults, func(a, b oci.Mount) bool {
		return a.Destination == b.Destination
	}) {
		t.Errorf("the default mounts were disturbed: %v", dests)
	}

	// The first refresh writes the record, so the next start has nothing to infer.
	again, err := RefreshAppliedEdits(spec, current, "all")
	if err != nil {
		t.Fatal(err)
	}
	if again.Changed {
		t.Error("a second refresh against the same list changed the spec again")
	}
}

// nvidia-ctk's hooks name library files as well: create-symlinks links a
// soname to the versioned file. Left alone they link to files that are gone.
func TestRefreshAppliedEdits_ReplacesHooks(t *testing.T) {
	symlink := func(link string) CDIHook {
		return CDIHook{
			HookName: "createContainer", Path: "/usr/bin/nvidia-ctk",
			Args: []string{"nvidia-ctk", "hook", "create-symlinks", "--link", link},
		}
	}
	spec := oci.DefaultSpec("rootfs", nil)
	before := vendorSpec(CDIContainerEdits{Hooks: []CDIHook{symlink("libcublas.so.13.4.0.1::/usr/lib/libcublas.so.13")}})
	if err := ApplyCDIDevice(spec, before, "all"); err != nil {
		t.Fatal(err)
	}

	after := vendorSpec(CDIContainerEdits{Hooks: []CDIHook{symlink("libcublas.so.13.4.1.3::/usr/lib/libcublas.so.13")}})
	if _, err := RefreshAppliedEdits(spec, after, "all"); err != nil {
		t.Fatal(err)
	}
	if spec.Hooks == nil || len(spec.Hooks.CreateContainer) != 1 {
		t.Fatalf("want exactly the current hook, got %+v", spec.Hooks)
	}
	if got := spec.Hooks.CreateContainer[0].Args[4]; got != "libcublas.so.13.4.1.3::/usr/lib/libcublas.so.13" {
		t.Errorf("hook still links %q", got)
	}

	if _, err := RefreshAppliedEdits(spec, vendorSpec(CDIContainerEdits{}), "all"); err != nil {
		t.Fatal(err)
	}
	if spec.Hooks != nil {
		t.Errorf("hooks survived a list that has none: %+v", spec.Hooks)
	}
}

// The vendor's variables sit below the entitlements' in a spec, which is how
// the gpu entitlement turns NVIDIA_VISIBLE_DEVICES=void into =all. A refresh
// that appended the current variables would hand that override back.
func TestRefreshAppliedEdits_ReplacesEnvWhereItWas(t *testing.T) {
	spec := oci.DefaultSpec("rootfs", nil)
	spec.Process.Env = []string{"PATH=/usr/bin"}
	before := vendorSpec(CDIContainerEdits{Env: []string{"NVIDIA_VISIBLE_DEVICES=void", "NVIDIA_REMOVED=1"}})
	if err := ApplyCDIDevice(spec, before, "all"); err != nil {
		t.Fatal(err)
	}
	spec.Process.Env = append(spec.Process.Env, "NVIDIA_VISIBLE_DEVICES=all")

	after := vendorSpec(CDIContainerEdits{Env: []string{"NVIDIA_VISIBLE_DEVICES=void", "NVIDIA_ADDED=1"}})
	if _, err := RefreshAppliedEdits(spec, after, "all"); err != nil {
		t.Fatal(err)
	}
	want := []string{"PATH=/usr/bin", "NVIDIA_VISIBLE_DEVICES=void", "NVIDIA_ADDED=1", "NVIDIA_VISIBLE_DEVICES=all"}
	if !slices.Equal(spec.Process.Env, want) {
		t.Errorf("env = %v\n want %v", spec.Process.Env, want)
	}
}

func devicePaths(spec *oci.Spec) []string {
	out := make([]string, 0, len(spec.Linux.Devices))
	for _, d := range spec.Linux.Devices {
		out = append(out, d.Path)
	}
	return out
}

// A newer OS can list a device node the old one did not. The container gets
// it the way a fresh deploy would: a node, and a rule allowing it.
func TestRefreshAppliedEdits_AddsDeviceNodesTheListGained(t *testing.T) {
	spec := oci.DefaultSpec("rootfs", nil)
	existing := CDIDeviceNode{Path: "/dev/nvmap", HostPath: os.DevNull}
	if err := ApplyCDIDevice(spec, vendorSpec(CDIContainerEdits{DeviceNodes: []CDIDeviceNode{existing}}), "all"); err != nil {
		t.Fatal(err)
	}
	rules := len(spec.Linux.Resources.Devices)

	gained := CDIDeviceNode{Path: "/dev/nvgained", HostPath: os.DevNull}
	got, err := RefreshAppliedEdits(spec, vendorSpec(CDIContainerEdits{DeviceNodes: []CDIDeviceNode{existing, gained}}), "all")
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"/dev/nvmap", "/dev/nvgained"}; !slices.Equal(devicePaths(spec), want) {
		t.Errorf("devices = %v; want %v", devicePaths(spec), want)
	}
	if added := len(spec.Linux.Resources.Devices) - rules; added != 1 {
		t.Errorf("%d device rules added; want 1, for the gained node only", added)
	}
	if !slices.Equal(got.DevicesAdded, []string{"/dev/nvgained"}) {
		t.Errorf("DevicesAdded = %v", got.DevicesAdded)
	}
}

// A device the list names but the host does not have is reported, not fatal:
// the files are still brought up to date, because a container that cannot be
// created at all is worse than one missing a node.
func TestRefreshAppliedEdits_KeepsGoingPastAnUnresolvableDevice(t *testing.T) {
	spec := oci.DefaultSpec("rootfs", nil)
	before := vendorSpec(CDIContainerEdits{Mounts: []CDIMount{vendorMount("/usr/lib/libcublas.so.13.4.0.1")}})
	if err := ApplyCDIDevice(spec, before, "all"); err != nil {
		t.Fatal(err)
	}

	after := vendorSpec(CDIContainerEdits{
		Mounts:      []CDIMount{vendorMount("/usr/lib/libcublas.so.13.4.1.3")},
		DeviceNodes: []CDIDeviceNode{{Path: filepath.Join(t.TempDir(), "absent")}},
	})
	got, err := RefreshAppliedEdits(spec, after, "all")
	if !errors.Is(err, ErrDevicesUnresolved) {
		t.Fatalf("err = %v; want ErrDevicesUnresolved", err)
	}
	if !got.Changed || !slices.Contains(destinations(spec), "/usr/lib/libcublas.so.13.4.1.3") {
		t.Errorf("the files were not refreshed: %+v %v", got, destinations(spec))
	}
	if len(spec.Linux.Devices) != 0 {
		t.Errorf("an unresolvable node was added: %v", devicePaths(spec))
	}
}

// Without a record the vendor's variables cannot be told from the app's, so
// nothing is removed. A variable the list still sets is left where it is, and
// a new one goes beside it, still below the entitlement's override.
func TestRefreshAppliedEdits_AddsEnvWithoutARecord(t *testing.T) {
	spec := oci.DefaultSpec("rootfs", nil)
	spec.Process.Env = []string{"PATH=/usr/bin", "NVIDIA_VISIBLE_DEVICES=void", "NVIDIA_VISIBLE_DEVICES=all"}

	current := vendorSpec(CDIContainerEdits{Env: []string{"NVIDIA_VISIBLE_DEVICES=void", "NVIDIA_ADDED=1"}})
	if _, err := RefreshAppliedEdits(spec, current, "all"); err != nil {
		t.Fatal(err)
	}
	want := []string{"PATH=/usr/bin", "NVIDIA_VISIBLE_DEVICES=void", "NVIDIA_ADDED=1", "NVIDIA_VISIBLE_DEVICES=all"}
	if !slices.Equal(spec.Process.Env, want) {
		t.Errorf("env = %v\n want %v", spec.Process.Env, want)
	}
}
