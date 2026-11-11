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

// nvidia-ctk writes the device major it saw when it generated the list, and
// several of those are allocated at module load, so the same list carries
// different numbers from one boot to the next. That is not a change of list:
// treating it as one rewrites every GPU container's spec on every boot.
func TestRefreshAppliedEdits_IgnoresDeviceNumbersThatMovedBetweenBoots(t *testing.T) {
	list := func(major int) *CDISpecification {
		return vendorSpec(CDIContainerEdits{
			Mounts:      []CDIMount{vendorMount("/usr/lib/libnvinfer.so.10")},
			DeviceNodes: []CDIDeviceNode{{Path: "/dev/host1x-fence", HostPath: os.DevNull, Major: &major}},
		})
	}
	spec := oci.DefaultSpec("rootfs", nil)
	if err := ApplyCDIDevice(spec, list(495), "all"); err != nil {
		t.Fatal(err)
	}

	got, err := RefreshAppliedEdits(spec, list(496), "all")
	if err != nil {
		t.Fatal(err)
	}
	if got.Changed {
		t.Errorf("a moved device number was treated as a new list: %+v", got)
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

// The toolkit has changed the options it writes before: "bind" through v1.17,
// "rbind" and "rprivate" since v1.18. If an OS update brings a toolkit that
// writes a third set, a container with no record still carries mounts in one
// of the earlier formats, and going by the current list's options alone would
// leave them in place, next to the new ones, naming files that are gone.
func TestRefreshAppliedEdits_RecognisesEarlierToolkitMountFormatsWithoutARecord(t *testing.T) {
	for name, earlier := range map[string][]string{
		"through v1.17": {"ro", "nosuid", "nodev", "bind"},
		"since v1.18":   {"ro", "nosuid", "nodev", "rbind", "rprivate"},
	} {
		t.Run(name, func(t *testing.T) {
			spec := oci.DefaultSpec("rootfs", nil)
			spec.Mounts = append(spec.Mounts,
				oci.Mount{Destination: "/usr/lib/libcublas.so.13.4.0.1", Source: "/usr/lib/libcublas.so.13.4.0.1", Type: "bind", Options: earlier},
				oci.Mount{Destination: "/dev/dri", Source: "/dev/dri", Type: "bind", Options: []string{"rbind", "rw", "nosuid", "noexec"}},
			)

			current := vendorSpec(CDIContainerEdits{Mounts: []CDIMount{{
				HostPath: "/usr/lib/libcublas.so.13.4.1.3", ContainerPath: "/usr/lib/libcublas.so.13.4.1.3",
				Options: []string{"ro", "nosuid", "nodev", "rbind"},
			}}})
			got, err := RefreshAppliedEdits(spec, current, "all")
			if err != nil {
				t.Fatal(err)
			}
			dests := destinations(spec)
			if slices.Contains(dests, "/usr/lib/libcublas.so.13.4.0.1") {
				t.Errorf("the mount in the earlier format survived: %v", dests)
			}
			if !slices.Contains(dests, "/usr/lib/libcublas.so.13.4.1.3") || !slices.Contains(dests, "/dev/dri") {
				t.Errorf("mounts = %v; want the current library and the entitlement's /dev/dri", dests)
			}
			if got.MountsRemoved != 1 || got.MountsAdded != 1 {
				t.Errorf("removed %d, added %d; want 1 and 1", got.MountsRemoved, got.MountsAdded)
			}
		})
	}
}

// Going by options alone is only safe while nothing else in a spec shares
// them. The Qualcomm runtime binds do share a set the vendor's list could
// use, and the agent wrote them itself, so they are told apart by path and
// never taken for the vendor's.
func TestRefreshAppliedEdits_NeverTakesTheQualcommRuntimeForTheVendors(t *testing.T) {
	shared := []string{"rbind", "ro", "nosuid", "nodev"}
	transport := oci.Mount{Destination: "/opt/wendyos/npu/lib", Source: "/run/wendyos/npu/lib", Type: "bind", Options: shared}
	shell := oci.Mount{
		Destination: "/usr/share/qcom/qcs8300/Qualcomm/QCS8275/dsp/cdsp/fastrpc_shell_unsigned_3",
		Source:      "/usr/share/qcom/qcs8300/Qualcomm/QCS8275/dsp/cdsp/fastrpc_shell_unsigned_3",
		Type:        "bind", Options: shared,
	}
	spec := oci.DefaultSpec("rootfs", nil)
	spec.Mounts = append(spec.Mounts,
		oci.Mount{Destination: "/usr/lib/libcublas.so.13.4.0.1", Source: "/usr/lib/libcublas.so.13.4.0.1", Type: "bind", Options: shared},
		transport, shell,
	)

	current := vendorSpec(CDIContainerEdits{Mounts: []CDIMount{{
		HostPath: "/usr/lib/libcublas.so.13.4.1.3", ContainerPath: "/usr/lib/libcublas.so.13.4.1.3", Options: shared,
	}}})
	if _, err := RefreshAppliedEdits(spec, current, "all"); err != nil {
		t.Fatal(err)
	}
	dests := destinations(spec)
	if slices.Contains(dests, "/usr/lib/libcublas.so.13.4.0.1") {
		t.Errorf("the stale library survived: %v", dests)
	}
	for _, kept := range []oci.Mount{transport, shell} {
		if !slices.Contains(dests, kept.Destination) {
			t.Errorf("the Qualcomm runtime mount %s was taken for the vendor's and removed", kept.Destination)
		}
	}
}

// A record this agent cannot read, written by a different version or damaged,
// must not block the repair. The container is handled like one with no record.
func TestRefreshAppliedEdits_TreatsAnUnreadableRecordAsNoRecord(t *testing.T) {
	spec := oci.DefaultSpec("rootfs", nil)
	spec.Mounts = append(spec.Mounts, oci.Mount{
		Destination: "/usr/lib/libcublas.so.13.4.0.1", Source: "/usr/lib/libcublas.so.13.4.0.1", Type: "bind", Options: vendorOptions,
	})
	// "env" as a list of strings is not the shape this version writes.
	spec.Annotations = map[string]string{appliedEditsAnnotation: `{"digest":"sha256:0","mounts":[{"path":"/usr/lib/libcublas.so.13.4.0.1"}],"env":["NVIDIA_VISIBLE_DEVICES=void"]}`}

	current := vendorSpec(CDIContainerEdits{Mounts: []CDIMount{vendorMount("/usr/lib/libcublas.so.13.4.1.3")}})
	got, err := RefreshAppliedEdits(spec, current, "all")
	if err != nil {
		t.Fatal(err)
	}
	if !got.Changed || !got.Inferred {
		t.Errorf("got %+v; want the repair made by inference", got)
	}
	if dests := destinations(spec); slices.Contains(dests, "/usr/lib/libcublas.so.13.4.0.1") || !slices.Contains(dests, "/usr/lib/libcublas.so.13.4.1.3") {
		t.Errorf("mounts were not brought up to date: %v", dests)
	}
	if again, err := RefreshAppliedEdits(spec, current, "all"); err != nil || again.Changed {
		t.Errorf("the rewritten record was not readable on the next call: %+v, %v", again, err)
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

// The image's variables come before the vendor's in a spec and the
// entitlements' after. When the image and the vendor set a variable to the
// same value, the vendor's copy is the later of those two, and it is the one
// that must be replaced. Replacing the image's leaves the stale value last,
// where it still wins.
func TestRefreshAppliedEdits_ReplacesTheVendorsCopyOfAVariableTheImageAlsoSets(t *testing.T) {
	spec := oci.DefaultSpec("rootfs", nil)
	spec.Process.Env = []string{"PATH=/usr/bin", "GST_PLUGIN_PATH=/old/plugins"}
	before := vendorSpec(CDIContainerEdits{Env: []string{"GST_PLUGIN_PATH=/old/plugins"}})
	if err := ApplyCDIDevice(spec, before, "all"); err != nil {
		t.Fatal(err)
	}
	spec.Process.Env = append(spec.Process.Env, "NVIDIA_VISIBLE_DEVICES=all")

	after := vendorSpec(CDIContainerEdits{Env: []string{"GST_PLUGIN_PATH=/new/plugins"}})
	if _, err := RefreshAppliedEdits(spec, after, "all"); err != nil {
		t.Fatal(err)
	}
	want := []string{"PATH=/usr/bin", "GST_PLUGIN_PATH=/old/plugins", "GST_PLUGIN_PATH=/new/plugins", "NVIDIA_VISIBLE_DEVICES=all"}
	if !slices.Equal(spec.Process.Env, want) {
		t.Fatalf("env = %v\n want %v", spec.Process.Env, want)
	}

	// The list goes back to the value the image also sets. The vendor's copy
	// is again the later one, and the next refresh has to find it there.
	if _, err := RefreshAppliedEdits(spec, before, "all"); err != nil {
		t.Fatal(err)
	}
	later := vendorSpec(CDIContainerEdits{Env: []string{"GST_PLUGIN_PATH=/newer/plugins"}})
	if _, err := RefreshAppliedEdits(spec, later, "all"); err != nil {
		t.Fatal(err)
	}
	want = []string{"PATH=/usr/bin", "GST_PLUGIN_PATH=/old/plugins", "GST_PLUGIN_PATH=/newer/plugins", "NVIDIA_VISIBLE_DEVICES=all"}
	if !slices.Equal(spec.Process.Env, want) {
		t.Errorf("after two more refreshes env = %v\n want %v", spec.Process.Env, want)
	}
}

// The other neighbour: an entitlement can set a variable to the same value
// the vendor does. The entitlement's copy is the later one and must stay.
func TestRefreshAppliedEdits_KeepsAnEntitlementsCopyOfAVendorVariable(t *testing.T) {
	spec := oci.DefaultSpec("rootfs", nil)
	spec.Process.Env = []string{"PATH=/usr/bin"}
	before := vendorSpec(CDIContainerEdits{Env: []string{"NVIDIA_DRIVER_CAPABILITIES=all"}})
	if err := ApplyCDIDevice(spec, before, "all"); err != nil {
		t.Fatal(err)
	}
	spec.Process.Env = append(spec.Process.Env, "NVIDIA_DRIVER_CAPABILITIES=all")

	after := vendorSpec(CDIContainerEdits{Env: []string{"NVIDIA_DRIVER_CAPABILITIES=compute"}})
	if _, err := RefreshAppliedEdits(spec, after, "all"); err != nil {
		t.Fatal(err)
	}
	want := []string{"PATH=/usr/bin", "NVIDIA_DRIVER_CAPABILITIES=compute", "NVIDIA_DRIVER_CAPABILITIES=all"}
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

// A device the list names can be missing on the host when a container is
// first refreshed, a driver that has not finished loading for instance. The
// refresh must not count as done: the next start has to try the device again,
// because nothing else will. The device-number refresh only repairs devices
// the spec already has.
func TestRefreshAppliedEdits_RetriesADeviceThatWasMissingLastTime(t *testing.T) {
	host := filepath.Join(t.TempDir(), "nvhost-late")
	list := vendorSpec(CDIContainerEdits{
		Mounts:      []CDIMount{vendorMount("/usr/lib/libnvinfer.so.10")},
		DeviceNodes: []CDIDeviceNode{{Path: "/dev/nvhost-late", HostPath: host}},
	})
	spec := oci.DefaultSpec("rootfs", nil)

	first, err := RefreshAppliedEdits(spec, list, "all")
	if !errors.Is(err, ErrDevicesUnresolved) || !first.Changed {
		t.Fatalf("first refresh: %+v, %v; want the files refreshed and the device reported", first, err)
	}

	// The device appears. The list is the same one as before.
	if err := os.Symlink(os.DevNull, host); err != nil {
		t.Fatal(err)
	}
	second, err := RefreshAppliedEdits(spec, list, "all")
	if err != nil {
		t.Fatal(err)
	}
	if !second.Changed || !slices.Equal(second.DevicesAdded, []string{"/dev/nvhost-late"}) {
		t.Errorf("second refresh: %+v; want the device added now that the host has it", second)
	}
	if !slices.Contains(devicePaths(spec), "/dev/nvhost-late") {
		t.Errorf("the container still has no /dev/nvhost-late: %v", devicePaths(spec))
	}

	third, err := RefreshAppliedEdits(spec, list, "all")
	if err != nil || third.Changed {
		t.Errorf("third refresh: %+v, %v; want nothing left to do", third, err)
	}
}

// While a listed device stays missing there is nothing to write on later
// starts, only the same report.
func TestRefreshAppliedEdits_DoesNotRewriteWhileADeviceStaysMissing(t *testing.T) {
	list := vendorSpec(CDIContainerEdits{
		Mounts:      []CDIMount{vendorMount("/usr/lib/libnvinfer.so.10")},
		DeviceNodes: []CDIDeviceNode{{Path: "/dev/nvhost-late", HostPath: filepath.Join(t.TempDir(), "absent")}},
	})
	spec := oci.DefaultSpec("rootfs", nil)
	if _, err := RefreshAppliedEdits(spec, list, "all"); !errors.Is(err, ErrDevicesUnresolved) {
		t.Fatalf("err = %v; want ErrDevicesUnresolved", err)
	}

	again, err := RefreshAppliedEdits(spec, list, "all")
	if !errors.Is(err, ErrDevicesUnresolved) {
		t.Errorf("err = %v; want the missing device reported again", err)
	}
	if again.Changed {
		t.Errorf("the spec was rewritten with nothing new to put in it: %+v", again)
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
