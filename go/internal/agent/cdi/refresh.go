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
	Env    []appliedEnv   `json:"env,omitempty"`
}

// appliedMount identifies one mount a CDI device contributed.
type appliedMount struct {
	Path string `json:"path"`
	// HostPath is omitted when it equals Path, which is nearly always.
	HostPath string `json:"hostPath,omitempty"`
}

// appliedEnv identifies one variable assignment a CDI device contributed.
type appliedEnv struct {
	Entry string `json:"entry"`
	// Earlier is how many identical assignments come before this one in the
	// spec. The image's variables precede the vendor's and the entitlements'
	// follow, and either may assign the same value the vendor does. The count
	// picks the vendor's copy out from between them.
	Earlier int `json:"earlier,omitempty"`
}

func appliedMountFor(m oci.Mount) appliedMount {
	if m.Source == m.Destination {
		return appliedMount{Path: m.Destination}
	}
	return appliedMount{Path: m.Destination, HostPath: m.Source}
}

// recordAppliedEdits notes what edits contributed to spec. envStart is where
// the first of edits.Env sits in the spec's variables.
func recordAppliedEdits(spec *oci.Spec, edits *CDIContainerEdits, envStart int) {
	record := appliedEdits{Digest: digestEdits(edits)}
	for _, m := range ociMounts(edits.Mounts) {
		record.Mounts = append(record.Mounts, appliedMountFor(m))
	}
	for i, entry := range edits.Env {
		applied := appliedEnv{Entry: entry}
		if spec.Process != nil && envStart+i <= len(spec.Process.Env) {
			for _, e := range spec.Process.Env[:envStart+i] {
				if e == entry {
					applied.Earlier++
				}
			}
		}
		record.Env = append(record.Env, applied)
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
	// content as it was, and whenever a device node was added.
	Changed bool
	// Inferred reports that the spec carried no record, so the vendor's mounts
	// were recognised by their options instead.
	Inferred bool
	// MountsRemoved counts the mounts the list no longer names, and MountsAdded
	// the ones it gained. A mount in both is not counted.
	MountsRemoved, MountsAdded int
	// HooksChanged and EnvChanged report a difference in either.
	HooksChanged, EnvChanged bool
	// DevicesAdded names the listed device nodes the spec lacked and now has.
	DevicesAdded []string
}

// RefreshAppliedEdits makes the part of spec that a CDI device contributed
// match what cdiSpec lists now: the mounts, hooks and variables are replaced,
// and device nodes the list names are added if the spec lacks them.
//
// A returned error names device nodes the list has and the host does not. The
// rest of the refresh has still been applied, and a later call tries those
// devices again.
func RefreshAppliedEdits(spec *oci.Spec, cdiSpec *CDISpecification, deviceName string) (EditRefresh, error) {
	edits, err := editsFor(cdiSpec, deviceName)
	if err != nil {
		return EditRefresh{}, err
	}

	var result EditRefresh
	record := decodeAppliedEdits(spec)
	digest := digestEdits(edits)
	if record == nil || digest == "" || record.Digest != digest {
		result.Changed = true
		result.Inferred = record == nil
		envStart := replaceVendorEdits(spec, edits, record, &result)
		recordAppliedEdits(spec, edits, envStart)
	}

	// Devices are checked on every call, whatever the digest says. A node the
	// host could not resolve last time left nothing behind in the spec, neither
	// an entry nor a pin, so the digest matching must not be taken to mean the
	// node is there.
	added, deviceErr := addMissingDevices(spec, edits.DeviceNodes)
	if len(added) > 0 {
		result.Changed = true
		result.DevicesAdded = added
	}
	return result, deviceErr
}

// replaceVendorEdits swaps the vendor's mounts, variables and hooks in spec
// for the ones edits lists, and returns where the vendor's variables now start.
func replaceVendorEdits(spec *oci.Spec, edits *CDIContainerEdits, record *appliedEdits, result *EditRefresh) (envStart int) {
	fresh := ociMounts(edits.Mounts)

	// Which of the spec's mounts and variables are the vendor's.
	var ownedMount func(oci.Mount) bool
	var ownedEnv func(string) bool
	if record != nil {
		// Each recorded entry claims one match, the earliest. An entitlement
		// may bind a path the vendor also lists; it is applied after the
		// vendor's edits, so it is the later of the two and is left in place.
		ownedMount = claimEach(record.Mounts, appliedMountFor)
		ownedEnv = claimRecordedEnv(record.Env)
	} else {
		// A container created before the record existed. Its vendor mounts are
		// the ones shaped like the vendor's: nvidia-ctk writes one option set
		// on every mount it lists. The shapes are those of the current list
		// and of the toolkit's earlier formats, since the container may predate
		// a toolkit update that changed them.
		shapes := make(map[string]bool, len(earlierToolkitMountOptions)+1)
		for _, m := range fresh {
			shapes[mountShape(m)] = true
		}
		for _, options := range earlierToolkitMountOptions {
			shapes[mountShape(oci.Mount{Type: "bind", Options: options})] = true
		}
		// The agent has never written the toolkit's own formats on a mount of
		// its own. The current list may use a set the agent does write, on the
		// Qualcomm runtime binds, so those are ruled out by path.
		ownedMount = func(m oci.Mount) bool { return shapes[mountShape(m)] && !isQualcommRuntimeMount(m) }
		// Variables have no shape to go by, so only one identical to a current
		// variable is taken over. Anything else may be the app's and stays.
		// With no record of which copy is the vendor's, the earliest is taken:
		// if that is the image's, the current variables land ahead of where a
		// fresh deploy puts them, which can only lower their precedence. Taking
		// a later copy could put them above an entitlement's.
		ownedEnv = claimEach(edits.Env, func(e string) string { return e })
	}

	// A later mount shadows an earlier one, so the vendor's block stays where a
	// fresh deploy puts it: ahead of the entitlements' mounts.
	listed := make(map[appliedMount]bool, len(fresh))
	for _, m := range fresh {
		listed[appliedMountFor(m)] = true
	}
	had := make(map[appliedMount]bool, len(fresh))
	spec.Mounts, _ = replaceBlock(spec.Mounts, func(m oci.Mount) bool {
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
		var env []string
		env, envStart = replaceBlock(spec.Process.Env, ownedEnv, edits.Env, false)
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
	return envStart
}

// addMissingDevices adds the nodes spec has no entry for and returns their
// paths. Device nodes are only ever added. One already in the spec keeps its
// entry, and its number is kept current by oci.RefreshHostDeviceNumbers. One
// the list dropped is left alone, because the gpu entitlement adds NVIDIA
// nodes as well and the two cannot be told apart afterwards.
func addMissingDevices(spec *oci.Spec, nodes []CDIDeviceNode) ([]string, error) {
	if spec.Linux == nil {
		return nil, nil
	}
	present := make(map[string]bool, len(spec.Linux.Devices))
	for _, d := range spec.Linux.Devices {
		present[d.Path] = true
	}
	var missing []CDIDeviceNode
	for _, node := range nodes {
		if !present[node.Path] {
			present[node.Path] = true
			missing = append(missing, node)
		}
	}
	if len(missing) == 0 {
		return nil, nil
	}
	before := len(spec.Linux.Devices)
	err := applyDeviceNodes(spec, missing)
	var added []string
	for _, d := range spec.Linux.Devices[before:] {
		added = append(added, d.Path)
	}
	return added, err
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

// earlierToolkitMountOptions are the option sets nvidia-ctk has written on the
// mounts it lists: the first through v1.17, the second since v1.18.
var earlierToolkitMountOptions = [][]string{
	{"ro", "nosuid", "nodev", "bind"},
	{"ro", "nosuid", "nodev", "rbind", "rprivate"},
}

// claimRecordedEnv returns a predicate that reports exactly the recorded
// copies of the vendor's variables, counting identical assignments in the
// order they are offered to it.
func claimRecordedEnv(recorded []appliedEnv) func(string) bool {
	vendors := make(map[string]map[int]bool, len(recorded))
	for _, r := range recorded {
		if vendors[r.Entry] == nil {
			vendors[r.Entry] = make(map[int]bool, 1)
		}
		vendors[r.Entry][r.Earlier] = true
	}
	seen := make(map[string]int, len(recorded))
	return func(entry string) bool {
		copies, ok := vendors[entry]
		if !ok {
			return false
		}
		n := seen[entry]
		seen[entry]++
		return copies[n]
	}
}

// mountShape reduces a mount to its type and option set, ignoring option order.
func mountShape(m oci.Mount) string {
	options := slices.Sorted(slices.Values(m.Options))
	return m.Type + "\x00" + strings.Join(options, ",")
}

// replaceBlock drops the elements owned reports as the vendor's and puts fresh
// where the first of them was. When none were there, fresh goes at the end, or
// at the start if atEnd is false. It also returns where fresh was put.
func replaceBlock[T any](existing []T, owned func(T) bool, fresh []T, atEnd bool) ([]T, int) {
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
	return slices.Insert(kept, insertAt, fresh...), insertAt
}
