package camera

import (
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// Identity names one camera node by what survives a re-enumeration: the sysfs
// device directory it hangs off (for a USB camera, its port and interface),
// and its sysfs index and name. The /dev/video<N> number is not part of it,
// because a replug or a reboot can change it.
type Identity struct {
	Device, Index, Name string
}

// IdentityOf reads the identity of /dev/<base> from sysfs: the resolved
// <sysfsRoot>/<base>/device link, and the index and name attributes. A node
// without a device link, such as a v4l2loopback node, has no identity.
func IdentityOf(base string) (Identity, error) {
	dir := filepath.Join(sysfsRoot, base)
	device, err := filepath.EvalSymlinks(filepath.Join(dir, "device"))
	if err != nil {
		return Identity{}, err
	}
	index, err := readAttribute(filepath.Join(dir, "index"))
	if err != nil {
		return Identity{}, err
	}
	name, err := readAttribute(filepath.Join(dir, "name"))
	if err != nil {
		return Identity{}, err
	}
	return Identity{Device: device, Index: index, Name: name}, nil
}

func readAttribute(path string) (string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(b)), nil
}

// PresentIdentities maps each video<N> node in sysfs that has an identity
// (IdentityOf succeeds, so it has an index) to that identity.
func PresentIdentities() map[string]Identity {
	entries, err := os.ReadDir(sysfsRoot)
	if err != nil {
		return map[string]Identity{}
	}
	out := make(map[string]Identity, len(entries))
	for _, e := range entries {
		base := e.Name()
		num, isVideo := strings.CutPrefix(base, "video")
		if !isVideo {
			continue
		}
		if _, err := strconv.ParseUint(num, 10, 32); err != nil {
			continue
		}
		if id, err := IdentityOf(base); err == nil {
			out[base] = id
		}
	}
	return out
}

// Resolve finds the present node of a stored identity. An exact match on
// Device, Index and Name wins. Otherwise a camera moved to another port is
// followed when it is the only present camera with the same Name and Index
// that claimed does not report (claimed says a camera belongs to another
// record; nil claims nothing). Otherwise there is none: two cameras of one
// model are told apart by port and never cross.
func Resolve(want Identity, present map[string]Identity, claimed func(Identity) bool) (base string, ok bool) {
	bases := make([]string, 0, len(present))
	for b := range present {
		bases = append(bases, b)
	}
	sort.Strings(bases)
	for _, b := range bases {
		if present[b] == want {
			return b, true
		}
	}
	var candidates []string
	for _, b := range bases {
		id := present[b]
		if id.Name != want.Name || id.Index != want.Index {
			continue
		}
		if claimed != nil && claimed(id) {
			continue
		}
		candidates = append(candidates, b)
	}
	if len(candidates) == 1 {
		return candidates[0], true
	}
	return "", false
}
