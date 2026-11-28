package camera

import (
	"errors"
	"io/fs"
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

// USB is what the USB device behind a camera node says of itself, which a move
// to another port does not change: its vendor and product numbers, its serial
// where it has one, and the number of the interface the node is on.
type USB struct {
	Vendor, Product, Serial, Interface string
}

// USBOf reads USB for /dev/<base> from sysfs: the node's device is the USB
// interface, or for some drivers the USB device itself. A node whose device is
// on no USB device has the zero USB and no error. An error says it could not
// be read, which is not that: the node has no device link, as for IdentityOf,
// or an attribute a USB device always has did not read, or the device went
// while it was read. The serial is the one attribute a USB device may lack,
// so one that is not found counts as none only when the device's vendor still
// reads after it.
func USBOf(base string) (USB, error) {
	dir, err := filepath.EvalSymlinks(filepath.Join(sysfsRoot, base, "device"))
	if err != nil {
		return USB{}, err
	}
	for _, device := range []string{dir, filepath.Dir(dir)} {
		vendor := filepath.Join(device, "idVendor")
		usb := USB{}
		if usb.Vendor, err = readAttribute(vendor); errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return USB{}, err
		}
		if usb.Product, err = readAttribute(filepath.Join(device, "idProduct")); err != nil {
			return USB{}, err
		}
		if device != dir {
			if usb.Interface, err = readAttribute(filepath.Join(dir, "bInterfaceNumber")); err != nil {
				return USB{}, err
			}
		}
		if usb.Serial, err = readAttribute(filepath.Join(device, "serial")); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return USB{}, err
		}
		if _, err := readAttribute(vendor); err != nil {
			return USB{}, err
		}
		return usb, nil
	}
	if _, err := os.Stat(dir); err != nil {
		return USB{}, err
	}
	return USB{}, nil
}

// readFile reads a sysfs attribute. A seam: a device that goes while it is
// read, and reads that fail for want of a descriptor, have no fixture.
var readFile = os.ReadFile

func readAttribute(path string) (string, error) {
	b, err := readFile(path)
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
