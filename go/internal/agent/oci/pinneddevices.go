package oci

import (
	"encoding/json"
	"fmt"
	"reflect"
	"slices"
	"sort"
)

// pinnedDevicesAnnotation records every host device whose major/minor pair this
// spec pins, and the path each pair was resolved from.
//
// The pairs themselves are already in the spec — as device entries, as cgroup
// allow rules, or as both — but a cgroup rule is only a triple of numbers. It
// does not say which device it was written for, so nothing could ever re-resolve
// it: a rule reading "allow c 497:16" gives no hint that it once meant
// /dev/kfd. That is why a stale number used to be repairable only by rebuilding
// the container. This annotation is the missing half.
const pinnedDevicesAnnotation = "sh.wendy/pinned-devices"

// PinnedDevice is one host device node whose numbers this spec has pinned.
type PinnedDevice struct {
	Path string `json:"path"`
	// HostPath may differ from the container destination. Empty means Path
	// for annotations written by older agents.
	HostPath string `json:"hostPath,omitempty"`
	// Static preserves an explicitly numbered CDI node with no host source.
	Static bool   `json:"static,omitempty"`
	Type   string `json:"type"`
	Major  int64  `json:"major"`
	Minor  int64  `json:"minor"`
}

// DeviceRefresh reports what RefreshHostDeviceNumbers found.
type DeviceRefresh struct {
	// Updated describes each device whose pinned pair no longer matched the
	// host and was rewritten, as "path (old -> new)".
	Updated []string
	// Removed names legacy synthetic devices dropped during platform migration.
	Removed []string
	// RulesChanged reports a repair to device access rules.
	RulesChanged bool
	// Missing names the pinned devices that no longer exist on the host at all.
	// Their numbers are left alone: there is nothing to re-resolve them to, and
	// a device that is genuinely gone is a different problem from one that
	// moved.
	Missing []string
	// RecordCompleted reports that the pin record was written or extended even
	// though no number moved — the upgrade path for a container created before
	// pins were recorded. Without it such a container would keep deriving its
	// pins from the device list on every start and never gain a record for its
	// cgroup-only devices, because the caller persists on Changed() alone and
	// nothing had changed.
	RecordCompleted bool
}

// Changed reports whether any device number was repaired.
func (r DeviceRefresh) Changed() bool {
	return len(r.Updated) > 0 || len(r.Removed) > 0 || r.RulesChanged
}

// SpecModified reports whether the spec needs persisting — a repaired number,
// or a record that was completed on the way.
func (r DeviceRefresh) SpecModified() bool { return r.Changed() || r.RecordCompleted }

// RecordPinnedDevice notes that this spec pins path at major:minor, so the pair
// can be re-resolved against the host later. Every site that writes an exact
// major:minor into a spec must call it; a pair recorded nowhere is a pair
// nothing can repair.
//
// Exported for the cdi package, which injects device nodes from a vendor's CDI
// spec. A site that adds a device *entry* is also picked up from the device
// list (see RefreshHostDeviceNumbers), so forgetting the call there degrades to
// the old coverage rather than losing it; a site that pins only a cgroup rule
// has no such safety net.
func RecordPinnedDevice(spec *Spec, path, devType string, major, minor int64) {
	RecordPinnedDeviceMapping(spec, PinnedDevice{Path: path, Type: devType, Major: major, Minor: minor})
}

// RecordPinnedDeviceMapping preserves source provenance separately from the
// container path. As with DedupeDevices, the first provisioner's mapping wins.
func RecordPinnedDeviceMapping(spec *Spec, pin PinnedDevice) {
	if spec == nil || pin.Path == "" {
		return
	}
	pins := decodePinnedDevices(spec)
	for i := range pins {
		if pins[i].Path == pin.Path {
			pins[i].Type, pins[i].Major, pins[i].Minor = pin.Type, pin.Major, pin.Minor
			encodePinnedDevices(spec, pins)
			return
		}
	}
	encodePinnedDevices(spec, append(pins, pin))
}

func (p PinnedDevice) sourcePath() string {
	if p.HostPath != "" {
		return p.HostPath
	}
	return p.Path
}

// decodePinnedDevices returns the recorded pins, or nil when the spec carries
// none. A malformed annotation is treated as absent rather than fatal: it can
// only cost a repair opportunity, and refusing to start a container over
// unreadable repair metadata would be worse than the problem it prevents.
func decodePinnedDevices(spec *Spec) []PinnedDevice {
	if spec == nil || spec.Annotations == nil {
		return nil
	}
	raw, ok := spec.Annotations[pinnedDevicesAnnotation]
	if !ok || raw == "" {
		return nil
	}
	var pins []PinnedDevice
	if err := json.Unmarshal([]byte(raw), &pins); err != nil {
		return nil
	}
	return pins
}

func encodePinnedDevices(spec *Spec, pins []PinnedDevice) {
	if len(pins) == 0 {
		return
	}
	// Stable order so an unchanged spec serializes identically.
	sort.Slice(pins, func(i, j int) bool { return pins[i].Path < pins[j].Path })
	raw, err := json.Marshal(pins)
	if err != nil {
		return
	}
	if spec.Annotations == nil {
		spec.Annotations = make(map[string]string)
	}
	spec.Annotations[pinnedDevicesAnnotation] = string(raw)
}

// RefreshHostDeviceNumbers re-resolves every device number this spec pins and
// rewrites the ones the host has moved, keeping device entries, cgroup allow
// rules, and the pin record in step.
//
// Why a container needs this at all: the numbers in a spec are a snapshot of
// the host taken when the container was created. Several of the device majors
// an AI box depends on — Jetson's nvgpu and nvidia-uvm nodes, AMD's /dev/kfd —
// are allocated from the kernel's dynamic pool at module load, so they are
// stable for a boot rather than for the life of a container definition. A
// container definition outlives a boot: containerd persists it and recreates
// only the task. A spec can therefore name a number the running kernel has
// since moved, which from inside the container is indistinguishable from the
// hardware being absent — CUDA reports "no device", or the kernel denies
// permission, while the host looks perfectly healthy.
//
// Both failure shapes are covered, because both kinds of pin are recorded:
//   - a device entry plus its cgroup rule (the GPU nodes) — the container's own
//     node ends up pointing at nothing;
//   - a bind-mounted host node with only a cgroup rule (/dev/kfd, i2c, serial)
//     — the node is right but the rule blocks it.
//
// Specs written before pins were recorded fall back to the device list, so a
// container created by an older agent is still repairable without a redeploy,
// and picks up a pin record on its first refresh.
func RefreshHostDeviceNumbers(spec *Spec) DeviceRefresh {
	var out DeviceRefresh
	if spec == nil || spec.Linux == nil {
		return out
	}

	out.Removed = removePiNVIDIAFallback(spec)

	// The record is the only way to reach a cgroup-only pin, but device entries
	// carry their own path, so union the two rather than trusting the record to
	// be complete. That keeps a container created by an older agent repairable,
	// and keeps an injection site that forgets to record from silently dropping
	// out of coverage.
	recorded := decodePinnedDevices(spec)
	original := unionWithDeviceList(recorded, spec)
	pins := append([]PinnedDevice(nil), original...)
	if len(pins) == 0 {
		return out
	}
	recordIncomplete := !slices.Equal(original, recorded)

	changed := false

	for i := range pins {
		pin := &pins[i]
		if pin.Static {
			continue
		}
		major, minor, err := statDeviceNode(pin.sourcePath())
		if err != nil {
			out.Missing = append(out.Missing, pin.sourcePath())
			continue
		}
		if major == pin.Major && minor == pin.Minor {
			continue
		}

		out.Updated = append(out.Updated, fmt.Sprintf("%s (%d:%d -> %d:%d)", pin.Path, pin.Major, pin.Minor, major, minor))
		updateDeviceEntry(spec, pin.Path, major, minor)
		pin.Major, pin.Minor = major, minor
		changed = true
	}

	out.RulesChanged = reconcileDeviceRules(spec, original, pins, recorded)
	changed = changed || out.RulesChanged

	// Re-encode when anything moved, and also when the union found pins the
	// record was missing — that is the chance to complete it, so a later
	// refresh has a path for every pinned rule rather than only for the
	// entries.
	if changed || recordIncomplete {
		encodePinnedDevices(spec, pins)
		out.RecordCompleted = !changed && recordIncomplete
	}

	sort.Strings(out.Updated)
	sort.Strings(out.Missing)
	return out
}

// unionWithDeviceList adds a pin for every device entry the record does not
// already cover. Only device entries carry a path, so this recovers the
// entry-shaped pins — the coverage the code had before pins were recorded — and
// never the bind-mounted, cgroup-only ones, which is precisely why the record
// exists.
func unionWithDeviceList(recorded []PinnedDevice, spec *Spec) []PinnedDevice {
	indices := make(map[string]int, len(recorded))
	pins := append([]PinnedDevice(nil), recorded...)
	for i, p := range pins {
		indices[p.Path] = i
	}
	for _, dev := range spec.Linux.Devices {
		if dev.Path == "" {
			continue
		}
		if i, ok := indices[dev.Path]; ok {
			// The finalized device entry is authoritative. CDI and entitlements
			// may record different pairs before device deduplication keeps the
			// first entry; a newer annotation must not conceal that stale entry.
			pins[i].Type, pins[i].Major, pins[i].Minor = dev.Type, dev.Major, dev.Minor
			continue
		}
		indices[dev.Path] = len(pins)
		pins = append(pins, PinnedDevice{Path: dev.Path, Type: dev.Type, Major: dev.Major, Minor: dev.Minor})
	}
	return pins
}

func updateDeviceEntry(spec *Spec, path string, major, minor int64) {
	for i := range spec.Linux.Devices {
		if spec.Linux.Devices[i].Path == path {
			spec.Linux.Devices[i].Major = major
			spec.Linux.Devices[i].Minor = minor
		}
	}
}

type devicePair struct {
	Type         string
	Major, Minor int64
}

func pairFor(pin PinnedDevice) devicePair { return devicePair{pin.Type, pin.Major, pin.Minor} }

// Reconcile from the original rule set so swaps, aliases, duplicate grants and
// split read/write rules cannot consume each other's updates. Preserve rule
// order and policy; never invent access for a device that had no allowance.
func reconcileDeviceRules(spec *Spec, original, resolved, recorded []PinnedDevice) bool {
	if spec.Linux.Resources == nil {
		return false
	}
	targets := make(map[devicePair][]devicePair)
	byPath := make(map[string]devicePair)
	add := func(old, next devicePair) {
		if !slices.Contains(targets[old], next) {
			targets[old] = append(targets[old], next)
		}
	}
	for i, pin := range original {
		next := pairFor(resolved[i])
		add(pairFor(pin), next)
		byPath[pin.Path] = next
	}
	// An earlier provisioner can leave a second rule using the numbers kept in
	// the annotation rather than in the finalized device entry.
	for _, pin := range recorded {
		if next, ok := byPath[pin.Path]; ok {
			add(pairFor(pin), next)
		}
	}
	oldRules := spec.Linux.Resources.Devices
	var rules []LinuxDeviceCgroup
	for _, rule := range oldRules {
		if rule.Major != nil && rule.Minor != nil {
			next, ok := targets[devicePair{rule.Type, *rule.Major, *rule.Minor}]
			if ok {
				for _, pair := range next {
					replacement := rule
					replacement.Major, replacement.Minor = new(pair.Major), new(pair.Minor)
					rules = append(rules, replacement)
				}
				continue
			}
		}
		rules = append(rules, rule)
		// Preserve whole-class grants. If a known device moves outside one, carry
		// only that device's existing access forward, never the entire new major.
		if rule.Major != nil && rule.Minor == nil {
			var added []devicePair
			for i, pin := range original {
				next := pairFor(resolved[i])
				if rule.Type == pin.Type && *rule.Major == pin.Major && next.Major != pin.Major && !slices.Contains(added, next) {
					replacement := rule
					replacement.Major, replacement.Minor = new(next.Major), new(next.Minor)
					rules = append(rules, replacement)
					added = append(added, next)
				}
			}
		}
	}
	if reflect.DeepEqual(oldRules, rules) {
		return false
	}
	spec.Linux.Resources.Devices = rules
	return true
}

// Older agents added five synthetic NVIDIA entries even on a Pi whose GPU
// entitlement only supplied VideoCore access. Remove that exact legacy shape;
// real NVIDIA devices (including an external GPU on a Pi) are left intact.
func removePiNVIDIAFallback(spec *Spec) []string {
	if !boardDetect().IsRaspberryPi() || len(discoverNvidiaDeviceNodes()) != 0 {
		return nil
	}
	paths := map[string]bool{
		"/dev/nvidia0": true, "/dev/nvidiactl": true, "/dev/nvidia-uvm": true,
		"/dev/nvidia-uvm-tools": true, "/dev/nvidia-modeset": true,
	}
	found := make(map[string]bool)
	for _, d := range spec.Linux.Devices {
		if paths[d.Path] {
			if d.Type != "c" || d.Major != 195 || d.Minor != 0 {
				return nil
			}
			found[d.Path] = true
		} else if d.Type == "c" && d.Major == 195 {
			return nil
		}
	}
	if len(found) != len(paths) {
		return nil
	}
	var removed []string
	devices := spec.Linux.Devices[:0]
	for _, d := range spec.Linux.Devices {
		if paths[d.Path] {
			removed = append(removed, d.Path)
		} else {
			devices = append(devices, d)
		}
	}
	spec.Linux.Devices = devices
	if spec.Linux.Resources != nil {
		rules := spec.Linux.Resources.Devices[:0]
		for _, rule := range spec.Linux.Resources.Devices {
			if rule.Allow && rule.Type == "c" && rule.Major != nil && *rule.Major == 195 && rule.Minor == nil && rule.Access == "rw" {
				continue
			}
			rules = append(rules, rule)
		}
		spec.Linux.Resources.Devices = rules
	}
	pins := decodePinnedDevices(spec)
	kept := pins[:0]
	for _, pin := range pins {
		if !paths[pin.Path] {
			kept = append(kept, pin)
		}
	}
	delete(spec.Annotations, pinnedDevicesAnnotation)
	encodePinnedDevices(spec, kept)
	sort.Strings(removed)
	return removed
}
