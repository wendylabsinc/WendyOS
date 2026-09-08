package oci

import (
	"encoding/json"
	"errors"
	"testing"
)

// withStubbedStat points statDeviceNode at a fixed table for the duration of a
// test, so the cases can cover devices this machine does not have.
func withStubbedStat(t *testing.T, nodes map[string][2]int64) {
	t.Helper()
	orig := statDeviceNode
	t.Cleanup(func() { statDeviceNode = orig })
	statDeviceNode = func(p string) (int64, int64, error) {
		n, ok := nodes[p]
		if !ok {
			return 0, 0, errors.New("no such device")
		}
		return n[0], n[1], nil
	}
}

func pinnedSpec() *Spec {
	return &Spec{Linux: &Linux{Resources: &LinuxResources{}}}
}

func ruleFor(t *testing.T, spec *Spec, major, minor int64) bool {
	t.Helper()
	for _, r := range spec.Linux.Resources.Devices {
		if r.Major != nil && r.Minor != nil && *r.Major == major && *r.Minor == minor {
			return true
		}
	}
	return false
}

// The case main cannot reach at all: a device bound into the container with its
// numbers pinned only in a cgroup rule. The rule is an anonymous triple, so
// without a record there is nothing to re-resolve it from. AMD's /dev/kfd, i2c
// and serial all take this shape.
func TestRefreshHostDeviceNumbers_RepairsCgroupOnlyPin(t *testing.T) {
	withStubbedStat(t, map[string][2]int64{"/dev/kfd": {510, 0}})

	spec := pinnedSpec()
	// How addScopedCharDevice leaves a spec: a bind mount, a pinned rule, and
	// no device entry.
	spec.Mounts = append(spec.Mounts, Mount{Destination: "/dev/kfd", Source: "/dev/kfd", Type: "bind"})
	maj, min := int64(497), int64(0)
	spec.Linux.Resources.Devices = append(spec.Linux.Resources.Devices, LinuxDeviceCgroup{
		Allow: true, Type: "c", Major: &maj, Minor: &min, Access: "rw",
	})
	RecordPinnedDevice(spec, "/dev/kfd", "c", 497, 0)

	got := RefreshHostDeviceNumbers(spec)
	if !got.Changed() {
		t.Fatalf("Changed() = false; want the moved /dev/kfd repaired (%+v)", got)
	}
	if !ruleFor(t, spec, 510, 0) {
		t.Errorf("cgroup rules = %+v; want one allowing 510:0 — otherwise the bound node is right but blocked", spec.Linux.Resources.Devices)
	}
	if ruleFor(t, spec, 497, 0) {
		t.Error("stale 497:0 rule left behind; the container keeps an allowance for whatever now owns that number")
	}
}

// A device entry and its rule must move together: repairing one without the
// other leaves the container with a node it may not open.
func TestRefreshHostDeviceNumbers_RepairsEntryAndRuleTogether(t *testing.T) {
	withStubbedStat(t, map[string][2]int64{"/dev/nvgpu/igpu0/ctrl": {498, 4}})

	spec := pinnedSpec()
	addExactDeviceNodes(spec, []nvidiaDeviceNode{{path: "/dev/nvgpu/igpu0/ctrl", major: 497, minor: 16}})

	if got := RefreshHostDeviceNumbers(spec); !got.Changed() {
		t.Fatalf("Changed() = false; want the moved node repaired (%+v)", got)
	}
	if dev := spec.Linux.Devices[0]; dev.Major != 498 || dev.Minor != 4 {
		t.Errorf("device entry = %d:%d; want 498:4", dev.Major, dev.Minor)
	}
	if !ruleFor(t, spec, 498, 4) {
		t.Errorf("cgroup rules = %+v; want one allowing 498:4", spec.Linux.Resources.Devices)
	}
}

// Recording happens at the pinning site, so a spec built through the normal
// entitlement path is repairable without anyone re-deriving anything.
func TestAddScopedCharDevice_RecordsItsPin(t *testing.T) {
	withStubbedStat(t, map[string][2]int64{"/dev/ttyACM0": {166, 0}})

	spec := pinnedSpec()
	if _, _, err := addScopedCharDevice(spec, "/dev/ttyACM0"); err != nil {
		t.Fatalf("addScopedCharDevice: %v", err)
	}

	pins := decodePinnedDevices(spec)
	if len(pins) != 1 || pins[0].Path != "/dev/ttyACM0" || pins[0].Major != 166 {
		t.Fatalf("pins = %+v; want /dev/ttyACM0 recorded at 166:0", pins)
	}
}

// A spec from an older agent carries no record. Its device entries still have
// paths, so it must stay repairable — and pick up a record on the way.
func TestRefreshHostDeviceNumbers_UpgradesUnrecordedSpec(t *testing.T) {
	withStubbedStat(t, map[string][2]int64{"/dev/nvidia0": {195, 1}})

	spec := pinnedSpec()
	spec.Linux.Devices = []LinuxDevice{{Path: "/dev/nvidia0", Type: "c", Major: 195, Minor: 0}}

	if got := RefreshHostDeviceNumbers(spec); !got.Changed() {
		t.Fatalf("Changed() = false for an unrecorded spec; want it repaired from its device list (%+v)", got)
	}
	pins := decodePinnedDevices(spec)
	if len(pins) != 1 || pins[0].Minor != 1 {
		t.Errorf("pins = %+v; want the repaired pair recorded for next time", pins)
	}
}

// A device entry whose site forgot to record still gets repaired, because the
// refresh unions the record with the device list rather than trusting it.
func TestRefreshHostDeviceNumbers_UnionsRecordWithDeviceList(t *testing.T) {
	withStubbedStat(t, map[string][2]int64{
		"/dev/kfd":               {510, 0},
		"/dev/dri/renderD128":    {226, 128},
		"/dev/unrecorded-device": {240, 7},
	})

	spec := pinnedSpec()
	RecordPinnedDevice(spec, "/dev/kfd", "c", 497, 0)
	spec.Linux.Devices = []LinuxDevice{
		{Path: "/dev/dri/renderD128", Type: "c", Major: 226, Minor: 128},
		{Path: "/dev/unrecorded-device", Type: "c", Major: 240, Minor: 3},
	}

	got := RefreshHostDeviceNumbers(spec)
	if len(got.Updated) != 2 {
		t.Fatalf("Updated = %v; want both the recorded pin and the unrecorded entry repaired", got.Updated)
	}
	if spec.Linux.Devices[1].Minor != 7 {
		t.Errorf("unrecorded entry minor = %d; want 7", spec.Linux.Devices[1].Minor)
	}
	if len(decodePinnedDevices(spec)) != 3 {
		t.Errorf("pins = %+v; want all three recorded after the union", decodePinnedDevices(spec))
	}
}

// Wildcard rules are the deliberate choice for hotplug-heavy classes (video,
// USB, input): they cannot go stale, and rewriting them would narrow a grant
// that was meant to be broad.
func TestRefreshHostDeviceNumbers_LeavesWildcardRules(t *testing.T) {
	withStubbedStat(t, map[string][2]int64{"/dev/kfd": {510, 0}})

	spec := pinnedSpec()
	wildcard := int64(81)
	spec.Linux.Resources.Devices = append(spec.Linux.Resources.Devices,
		LinuxDeviceCgroup{Allow: true, Type: "c", Major: &wildcard, Access: "rw"})
	RecordPinnedDevice(spec, "/dev/kfd", "c", 497, 0)

	RefreshHostDeviceNumbers(spec)

	if r := spec.Linux.Resources.Devices[0]; r.Minor != nil || *r.Major != 81 {
		t.Errorf("wildcard rule changed to %v:%v; want it untouched", r.Major, r.Minor)
	}
}

// A device that is genuinely gone is reported, never rewritten to a guess.
func TestRefreshHostDeviceNumbers_ReportsMissing(t *testing.T) {
	withStubbedStat(t, map[string][2]int64{})

	spec := pinnedSpec()
	RecordPinnedDevice(spec, "/dev/kfd", "c", 497, 0)

	got := RefreshHostDeviceNumbers(spec)
	if got.Changed() {
		t.Error("Changed() = true for an absent device; want false")
	}
	if len(got.Missing) != 1 || got.Missing[0] != "/dev/kfd" {
		t.Errorf("Missing = %v; want [/dev/kfd]", got.Missing)
	}
}

// An up-to-date spec must not be rewritten: the caller persists only on change,
// and a spurious diff means a containerd write on every start.
func TestRefreshHostDeviceNumbers_NoopWhenCurrent(t *testing.T) {
	withStubbedStat(t, map[string][2]int64{"/dev/kfd": {497, 0}})

	spec := pinnedSpec()
	RecordPinnedDevice(spec, "/dev/kfd", "c", 497, 0)
	before := spec.Annotations[pinnedDevicesAnnotation]

	if got := RefreshHostDeviceNumbers(spec); got.Changed() {
		t.Errorf("Changed() = true for an up-to-date spec; want false (%+v)", got)
	}
	if spec.Annotations[pinnedDevicesAnnotation] != before {
		t.Error("record rewritten with no change; the spec must serialize identically")
	}
}

// Unreadable repair metadata must not break a start — it can only cost a repair.
func TestRefreshHostDeviceNumbers_ToleratesMalformedRecord(t *testing.T) {
	withStubbedStat(t, map[string][2]int64{"/dev/nvidia0": {195, 0}})

	spec := pinnedSpec()
	spec.Annotations = map[string]string{pinnedDevicesAnnotation: "{not json"}
	spec.Linux.Devices = []LinuxDevice{{Path: "/dev/nvidia0", Type: "c", Major: 195, Minor: 0}}

	if got := RefreshHostDeviceNumbers(spec); got.Changed() {
		t.Errorf("Changed() = true; want the malformed record ignored and the current spec left alone (%+v)", got)
	}
}

func TestRefreshHostDeviceNumbers_ToleratesEmptySpec(t *testing.T) {
	if got := RefreshHostDeviceNumbers(nil); got.Changed() {
		t.Error("RefreshHostDeviceNumbers(nil) reported a change")
	}
	if got := RefreshHostDeviceNumbers(&Spec{}); got.Changed() {
		t.Error("RefreshHostDeviceNumbers(spec without Linux) reported a change")
	}
}

// The record travels inside the spec, which is what containerd persists, so it
// must survive the JSON round-trip the agent does on every start.
func TestPinnedDevices_SurviveSpecRoundTrip(t *testing.T) {
	spec := pinnedSpec()
	RecordPinnedDevice(spec, "/dev/kfd", "c", 497, 0)

	raw, err := json.Marshal(spec)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var back Spec
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	pins := decodePinnedDevices(&back)
	if len(pins) != 1 || pins[0].Path != "/dev/kfd" {
		t.Errorf("pins after round-trip = %+v; want /dev/kfd", pins)
	}
}

// The upgrade path has to persist. A container created before pins were
// recorded gains its record on the next start even though no number moved —
// caught on real hardware, where a Jetson's GPU container kept an empty
// annotation across restarts because the caller only persisted on a change.
func TestRefreshHostDeviceNumbers_RecordCompletedSignalsAPersist(t *testing.T) {
	withStubbedStat(t, map[string][2]int64{"/dev/nvhost-gpu": {497, 1}})

	spec := pinnedSpec()
	spec.Linux.Devices = []LinuxDevice{{Path: "/dev/nvhost-gpu", Type: "c", Major: 497, Minor: 1}}

	got := RefreshHostDeviceNumbers(spec)
	if got.Changed() {
		t.Errorf("Changed() = true; nothing moved (%+v)", got)
	}
	if !got.RecordCompleted {
		t.Error("RecordCompleted = false; the record was written and must be persisted")
	}
	if !got.SpecModified() {
		t.Error("SpecModified() = false; the caller would skip the write and the record would never land")
	}
	if len(decodePinnedDevices(spec)) != 1 {
		t.Errorf("pins = %+v; want the device recorded", decodePinnedDevices(spec))
	}
}

// Once recorded, a start must not rewrite the spec again — otherwise every
// start of every GPU app becomes a containerd write.
func TestRefreshHostDeviceNumbers_RecordCompletedOnlyOnce(t *testing.T) {
	withStubbedStat(t, map[string][2]int64{"/dev/nvhost-gpu": {497, 1}})

	spec := pinnedSpec()
	spec.Linux.Devices = []LinuxDevice{{Path: "/dev/nvhost-gpu", Type: "c", Major: 497, Minor: 1}}

	if first := RefreshHostDeviceNumbers(spec); !first.RecordCompleted {
		t.Fatal("first refresh did not record")
	}
	if second := RefreshHostDeviceNumbers(spec); second.SpecModified() {
		t.Errorf("second refresh reported SpecModified = true (%+v); want no further writes", second)
	}
}

func TestRefreshHostDeviceNumbers_AnnotationCannotHideStaleEntry(t *testing.T) {
	withStubbedStat(t, map[string][2]int64{"/dev/nvidia-uvm": {498, 0}})
	spec := pinnedSpec()
	// CDI's explicit pair is stale; the entitlement discovers the current pair.
	addExactDeviceNodes(spec, []nvidiaDeviceNode{{path: "/dev/nvidia-uvm", major: 497, minor: 0}})
	addExactDeviceNodes(spec, []nvidiaDeviceNode{{path: "/dev/nvidia-uvm", major: 498, minor: 0}})
	DedupeDevices(spec)
	result := RefreshHostDeviceNumbers(spec)
	if !result.Changed() || spec.Linux.Devices[0].Major != 498 {
		t.Fatalf("stale finalized entry survived refresh: %+v; result=%+v", spec.Linux.Devices[0], result)
	}
	if again := RefreshHostDeviceNumbers(spec); again.SpecModified() {
		t.Fatalf("second refresh should be unchanged: %+v", again)
	}
}
