package cdi

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"reflect"
	"slices"
	"strings"

	"github.com/wendylabsinc/wendy/go/internal/agent/oci"
)

// appliedEditsAnnotation records which parts of a spec came from a CDI device.
//
// A CDI spec is a snapshot of the host: it lists driver files by exact name,
// and an OS update renames them. A container definition outlives that update,
// because containerd persists it and recreates only the task. Without a record
// of which mounts the vendor's list contributed, nothing can tell them from an
// entitlement's mounts, so the stale ones cannot be replaced and the runtime
// refuses to create the container over the first file it cannot find.
const appliedEditsAnnotation = "sh.wendy/cdi-edits"

// appliedEdits is the record kept in appliedEditsAnnotation.
type appliedEdits struct {
	// Digest identifies the edits the spec was last brought in line with, so
	// an unchanged host costs a start one comparison and no write.
	Digest string         `json:"digest"`
	Mounts []appliedMount `json:"mounts,omitempty"`
	Env    []string       `json:"env,omitempty"`
}

// appliedMount identifies one mount a CDI device contributed.
type appliedMount struct {
	Path string `json:"path"`
	// HostPath is omitted when it equals Path, which is nearly always.
	HostPath string `json:"hostPath,omitempty"`
}

func appliedMountFor(m oci.Mount) appliedMount {
	if m.Source == m.Destination {
		return appliedMount{Path: m.Destination}
	}
	return appliedMount{Path: m.Destination, HostPath: m.Source}
}

// recordAppliedEdits notes what edits contributed to spec.
func recordAppliedEdits(spec *oci.Spec, edits *CDIContainerEdits) {
	record := appliedEdits{Digest: digestEdits(edits), Env: edits.Env}
	for _, m := range ociMounts(edits.Mounts) {
		record.Mounts = append(record.Mounts, appliedMountFor(m))
	}
	raw, err := json.Marshal(record)
	if err != nil {
		return
	}
	if spec.Annotations == nil {
		spec.Annotations = make(map[string]string)
	}
	spec.Annotations[appliedEditsAnnotation] = string(raw)
}

// digestEdits fingerprints what a CDI device contributes, leaving out device
// numbers. nvidia-ctk writes the major it saw when it generated the spec, and
// several of those are allocated at module load, so an unchanged host lists
// different numbers from one boot to the next. Keeping numbers current is
// oci.RefreshHostDeviceNumbers' job; here they would only make every boot look
// like a new list.
func digestEdits(edits *CDIContainerEdits) string {
	type deviceNode struct{ Path, HostPath, Type string }
	stable := struct {
		DeviceNodes []deviceNode
		Mounts      []CDIMount
		Env         []string
		Hooks       []CDIHook
	}{Mounts: edits.Mounts, Env: edits.Env, Hooks: edits.Hooks}
	for _, node := range edits.DeviceNodes {
		stable.DeviceNodes = append(stable.DeviceNodes, deviceNode{node.Path, node.HostPath, node.Type})
	}
	raw, err := json.Marshal(stable)
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(raw)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// decodeAppliedEdits returns the record, or nil when spec carries none. A
// malformed record is treated as absent: it can only cost a repair.
func decodeAppliedEdits(spec *oci.Spec) *appliedEdits {
	raw := spec.Annotations[appliedEditsAnnotation]
	if raw == "" {
		return nil
	}
	var record appliedEdits
	if err := json.Unmarshal([]byte(raw), &record); err != nil {
		return nil
	}
	return &record
}

// EditRefresh reports what RefreshAppliedEdits did to a spec.
type EditRefresh struct {
	// Changed reports whether the spec was modified and needs persisting. It
	// is true whenever the host's list differs from the one the spec was last
	// brought in line with, even if the difference left the spec's own
	// content as it was.
	Changed bool
	// Inferred reports that the spec carried no record, so the vendor's mounts
	// were recognised by their options instead.
	Inferred bool
	// MountsRemoved counts the mounts the list no longer names, and MountsAdded
	// the ones it gained. A mount in both is not counted.
	MountsRemoved, MountsAdded int
	// HooksChanged and EnvChanged report a difference in either.
	HooksChanged, EnvChanged bool
	// DevicesAdded names the device nodes the list gained.
	DevicesAdded []string
}

// RefreshAppliedEdits makes the part of spec that a CDI device contributed
// match what cdiSpec lists now: the mounts, hooks and variables are replaced,
// and device nodes the list gained are added.
//
// A returned error names device nodes the list has and the host does not. The
// rest of the refresh has still been applied.
func RefreshAppliedEdits(spec *oci.Spec, cdiSpec *CDISpecification, deviceName string) (EditRefresh, error) {
	edits, err := editsFor(cdiSpec, deviceName)
	if err != nil {
		return EditRefresh{}, err
	}
	record := decodeAppliedEdits(spec)
	if record != nil {
		if digest := digestEdits(edits); digest != "" && record.Digest == digest {
			return EditRefresh{}, nil
		}
	}
	result := EditRefresh{Changed: true, Inferred: record == nil}
	fresh := ociMounts(edits.Mounts)

	// Which of the spec's mounts and variables are the vendor's.
	var ownedMount func(oci.Mount) bool
	var ownedEnv func(string) bool
	if record != nil {
		// Each recorded entry claims one match, the earliest. An entitlement
		// may bind a path the vendor also lists; it is applied after the
		// vendor's edits, so it is the later of the two and is left in place.
		ownedMount = claimEach(record.Mounts, appliedMountFor)
		ownedEnv = claimEach(record.Env, func(e string) string { return e })
	} else {
		// A container created before the record existed. Its vendor mounts are
		// the ones shaped like the vendor's: nvidia-ctk writes one option set
		// on every mount it lists, and no mount the agent adds itself uses it.
		// The shapes are read from the current list instead of being fixed
		// here, so they follow whatever the installed toolkit writes.
		shapes := make(map[string]bool, 1)
		for _, m := range fresh {
			shapes[mountShape(m)] = true
		}
		ownedMount = func(m oci.Mount) bool { return shapes[mountShape(m)] }
		// Variables have no shape to go by, so only one identical to a current
		// variable is taken over. Anything else may be the app's and stays.
		ownedEnv = claimEach(edits.Env, func(e string) string { return e })
	}

	// A later mount shadows an earlier one, so the vendor's block stays where a
	// fresh deploy puts it: ahead of the entitlements' mounts.
	listed := make(map[appliedMount]bool, len(fresh))
	for _, m := range fresh {
		listed[appliedMountFor(m)] = true
	}
	had := make(map[appliedMount]bool, len(fresh))
	spec.Mounts = replaceBlock(spec.Mounts, func(m oci.Mount) bool {
		if !ownedMount(m) {
			return false
		}
		if key := appliedMountFor(m); !had[key] {
			had[key] = true
			if !listed[key] {
				result.MountsRemoved++
			}
		}
		return true
	}, fresh, true)
	for key := range listed {
		if !had[key] {
			result.MountsAdded++
		}
	}

	// The last assignment of a variable wins, and entitlements rely on coming
	// after the vendor's (NVIDIA_VISIBLE_DEVICES=void becomes =all). With no
	// earlier variable to stand in for, the current ones go first, where
	// everything else still overrides them.
	if spec.Process != nil {
		env := replaceBlock(spec.Process.Env, ownedEnv, edits.Env, false)
		result.EnvChanged = !slices.Equal(env, spec.Process.Env)
		if result.EnvChanged {
			spec.Process.Env = env
		}
	}

	// Hooks need no record: the agent adds none of its own, so every hook in a
	// stored spec came from the vendor's list.
	previous := spec.Hooks
	spec.Hooks = nil
	applyHooks(spec, edits.Hooks)
	result.HooksChanged = !reflect.DeepEqual(previous, spec.Hooks)

	// Device nodes are only ever added. One already in the spec keeps its
	// entry, and its number is kept current by oci.RefreshHostDeviceNumbers.
	// One the list dropped is left alone, because the gpu entitlement adds
	// NVIDIA nodes as well and the two cannot be told apart afterwards.
	var deviceErr error
	if spec.Linux != nil {
		present := make(map[string]bool, len(spec.Linux.Devices))
		for _, d := range spec.Linux.Devices {
			present[d.Path] = true
		}
		var gained []CDIDeviceNode
		for _, node := range edits.DeviceNodes {
			if !present[node.Path] {
				present[node.Path] = true
				gained = append(gained, node)
			}
		}
		before := len(spec.Linux.Devices)
		deviceErr = applyDeviceNodes(spec, gained)
		for _, d := range spec.Linux.Devices[before:] {
			result.DevicesAdded = append(result.DevicesAdded, d.Path)
		}
	}

	recordAppliedEdits(spec, edits)
	return result, deviceErr
}

// claimEach returns a predicate that reports an element as the vendor's once
// for every time its key appears in recorded, in the order elements are
// offered to it.
func claimEach[E any, K comparable](recorded []K, key func(E) K) func(E) bool {
	remaining := make(map[K]int, len(recorded))
	for _, k := range recorded {
		remaining[k]++
	}
	return func(e E) bool {
		k := key(e)
		if remaining[k] == 0 {
			return false
		}
		remaining[k]--
		return true
	}
}

// mountShape reduces a mount to its type and option set, ignoring option order.
func mountShape(m oci.Mount) string {
	options := slices.Sorted(slices.Values(m.Options))
	return m.Type + "\x00" + strings.Join(options, ",")
}

// replaceBlock drops the elements owned reports as the vendor's and puts fresh
// where the first of them was. When none were there, fresh goes at the end, or
// at the start if atEnd is false.
func replaceBlock[T any](existing []T, owned func(T) bool, fresh []T, atEnd bool) []T {
	kept := make([]T, 0, len(existing)+len(fresh))
	insertAt := -1
	for _, e := range existing {
		if owned(e) {
			if insertAt < 0 {
				insertAt = len(kept)
			}
			continue
		}
		kept = append(kept, e)
	}
	if insertAt < 0 {
		insertAt = 0
		if atEnd {
			insertAt = len(kept)
		}
	}
	return slices.Insert(kept, insertAt, fresh...)
}
