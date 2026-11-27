package services

// Stable identity for local V4L2 cameras.
//
// A camera's /dev/videoN number is assigned in enumeration order at boot, so it
// belongs to the boot rather than to the camera: a reboot or a replug can
// renumber the nodes. A caller that pinned a number then addresses whatever the
// kernel put there instead -- and because opening the wrong camera SUCCEEDS, it
// streams a valid picture from the wrong sensor and reports no error at all.
// `name` and `driver` cannot rescue that: they identify the model, so two
// cameras of the same type are indistinguishable.
//
// udev publishes two stable names per node, and they are not equally durable:
//
//	/dev/v4l/by-id/    vendor + product + serial   survives a reboot AND a move
//	                                               to another port
//	/dev/v4l/by-path/  the USB port topology       survives a reboot but NOT a
//	                                               move
//
// A camera gets ONE identity, not two: by-id where that name identifies it,
// by-path only where it does not.
//
// It does not whenever two connected cameras would be given the same by-id
// name. udev builds the name from vendor + model + serial, so two cameras of
// one model that share a factory serial -- common with cheap cameras, and true
// of every unit of a model that has no serial string at all -- produce the SAME
// name. Only one link can exist, and it points at whichever twin udev processed
// last, so after a reboot it can point at the other one. A caller that pinned
// the name would then open the other camera, successfully: the same silent
// failure as a renumbered /dev/videoN, behind a name that promised to prevent
// it.
//
// So a by-id name that two connected cameras share is never handed out and
// never honoured. Twins are found from what the kernel knows rather than from
// udev's links -- the vendor, model and serial of the USB device behind each
// node, read from sysfs -- and while both are connected each reports its PORT,
// which does tell them apart, and a request for the shared by-id name is
// refused with the ports to use instead rather than resolved to either twin.
//
// That is the weaker guarantee, stated as what it is: a twin moved to another
// port gets a new identity, and plugging in a second same-serial camera changes
// the first one's identity from by-id to by-path (unplugging it changes it
// back). Both are visible in the listing. A name that silently means a
// different camera is not.
//
// What this cannot do is tell twins apart while only one of them is connected.
// Nothing the camera reports differs, so the one that is there has the by-id
// name to itself, whichever one it is.
//
// The value carries the scheme it came from ("by-id:" / "by-path:") so a
// caller can see which of the two durability guarantees it actually holds,
// rather than having to know how udev names things.

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

const (
	v4lByIDDir   = "/dev/v4l/by-id"
	v4lByPathDir = "/dev/v4l/by-path"
	// Where the kernel lists every video node, each with a `device` link to
	// the hardware it belongs to. Twins are detected from here.
	v4lSysDir = "/sys/class/video4linux"

	// Scheme tags on the reported identity, so a caller can see which
	// durability guarantee it holds without knowing how udev names things.
	schemeByID   = "by-id"
	schemeByPath = "by-path"
)

// stableNames holds the udev names for one /dev/videoN node. Either may be
// empty: CSI and network cameras have no /dev/v4l entry, and a kernel without
// the udev rules has neither directory.
type stableNames struct {
	byID   string
	byPath string
	// twin is set when another connected camera would be given this node's
	// by-id name, which makes that name useless as an identity whichever of
	// them currently holds the link. It has the same value on the matching
	// node of every such camera, which is how a refusal finds the ports to
	// offer instead. Empty for a camera with no twin connected.
	twin string
}

// stableID is the single identity reported for a node: the by-id name when
// there is one and no connected twin shares it, the port otherwise, empty when
// udev knows neither.
func (n stableNames) stableID() string {
	switch {
	case n.byID != "" && n.twin == "":
		return schemeByID + ":" + n.byID
	case n.byPath != "":
		return schemeByPath + ":" + n.byPath
	default:
		return ""
	}
}

// readStableNames maps a resolved device path ("/dev/video2") to its udev
// names, and marks the nodes of same-serial twins (see markTwins).
//
// A missing directory is NOT an error. Plenty of devices have no /dev/v4l at
// all, and losing the enrichment is strictly better than failing enumeration --
// the numbers still work, they are simply not stable.
func readStableNames(byIDDir, byPathDir, sysDir string) map[string]stableNames {
	out := map[string]stableNames{}
	for dev, name := range firstNamePerDevice(byIDDir) {
		n := out[dev]
		n.byID = name
		out[dev] = n
	}
	for dev, name := range firstNamePerDevice(byPathDir) {
		n := out[dev]
		n.byPath = name
		out[dev] = n
	}
	markTwins(out, sysDir)
	return out
}

// markTwins sets stableNames.twin on every node whose by-id name another
// connected camera would also be given.
//
// udev names a node "usb-<vendor>_<model>_<serial>-video-index<index>". Vendor
// and model are the device's manufacturer and product strings, or its numeric
// IDs where it has no such string, and a missing serial is simply left out. So
// two nodes collide when their USB devices agree on all three -- no serial on
// either counts as agreeing -- and the nodes have the same index within their
// camera. That alone is not a twin: a camera with two video interfaces has an
// index0 on each. What makes a twin is that the colliding nodes belong to
// DIFFERENT physical USB devices.
//
// The strings are compared, not only the numeric IDs, because the strings are
// what the name is made of. Two different models that both call themselves
// "USB Camera" collide although their IDs differ, and two brands built on one
// bridge chip share its IDs and its default serial yet get names of their own.
//
// The kernel is asked rather than udev because udev cannot answer: of a
// colliding pair it publishes one link, so the losing twin looks like any
// camera that never had a by-id name, and the winner like a camera with a
// perfectly good one. Every video node in sysfs is considered, not only the
// ones udev named, for the same reason.
//
// A node sysfs says nothing about -- not USB, or sysfs is unreadable -- is left
// alone, and the names read exactly as they did before twins were detected.
func markTwins(names map[string]stableNames, sysDir string) {
	entries, err := os.ReadDir(sysDir)
	if err != nil {
		return
	}
	// What each node ("video0") would be named, as far as a collision goes,
	// and the physical devices that would be given each such name.
	collision := map[string]string{}
	devices := map[string]map[string]bool{}
	for _, e := range entries {
		if !strings.HasPrefix(e.Name(), "video") {
			continue
		}
		device, vendor, model, ok := usbDeviceOf(sysDir, e.Name())
		if !ok {
			continue
		}
		if s := sysAttr(device, "manufacturer"); s != "" {
			vendor = s
		}
		if s := sysAttr(device, "product"); s != "" {
			model = s
		}
		key := strings.Join([]string{vendor, model, sysAttr(device, "serial"),
			sysAttr(filepath.Join(sysDir, e.Name()), "index")}, "\x00")
		collision[e.Name()] = key
		if devices[key] == nil {
			devices[key] = map[string]bool{}
		}
		devices[key][device] = true
	}
	for devPath, n := range names {
		if key, ok := collision[filepath.Base(devPath)]; ok && len(devices[key]) > 1 {
			n.twin = key
			names[devPath] = n
		}
	}
}

// usbDeviceOf walks from a video node's sysfs entry up through its parents to
// the USB device it belongs to, and returns that device's directory and IDs.
// The directory is unique per physical device, so two nodes of one camera get
// the same one and two cameras of one model do not.
//
// False for a node with no USB device above it (CSI, loopback) and whenever
// sysfs cannot be read.
func usbDeviceOf(sysDir, node string) (dir, vendor, product string, ok bool) {
	dir, err := filepath.EvalSymlinks(filepath.Join(sysDir, node, "device"))
	if err != nil {
		return "", "", "", false
	}
	for ; dir != "/" && dir != "."; dir = filepath.Dir(dir) {
		v, ve := os.ReadFile(filepath.Join(dir, "idVendor"))
		p, pe := os.ReadFile(filepath.Join(dir, "idProduct"))
		if ve == nil && pe == nil {
			return dir, strings.TrimSpace(string(v)), strings.TrimSpace(string(p)), true
		}
	}
	return "", "", "", false
}

// sysAttr reads one sysfs attribute, empty when it is absent. Absent is a
// value here, not a failure: a USB device with no serial string has no
// `serial` file, and one with no manufacturer string no `manufacturer`.
func sysAttr(dir, name string) string {
	b, err := os.ReadFile(filepath.Join(dir, name))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

// firstNamePerDevice resolves every symlink in dir and records its basename
// against the device node it points at.
//
// Entries are skipped rather than fatal: one dangling symlink must not cost
// every other camera its identity. os.ReadDir returns sorted entries, so where
// two names somehow reach the same node the winner is deterministic.
func firstNamePerDevice(dir string) map[string]string {
	out := map[string]string{}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return out
	}
	for _, e := range entries {
		target, err := filepath.EvalSymlinks(filepath.Join(dir, e.Name()))
		if err != nil {
			continue
		}
		if !strings.HasPrefix(filepath.Base(target), "video") {
			continue
		}
		if _, seen := out[target]; seen {
			continue
		}
		out[target] = e.Name()
	}
	return out
}

// resolveStableID returns the /dev/videoN number for a scheme-tagged stable id,
// or false.
//
// A "by-path:" name resolves even for a camera whose reported identity is its
// by-id: asking to stream from a physical port is legitimate, it just has to be
// asked for. Inheriting it from a listing is what we avoid.
//
// The match is exact. A substring or prefix rule would be friendlier to type
// and is precisely the wrong trade here: "usb-Acme_Camera_SN1" would match both
// "...SN1-video-index0" and "...SN10-video-index0", and the caller would get
// one of them with no indication that the name was ambiguous. An exact name is
// something ListVideoDevices already hands you.
//
// A "by-id:" name that two connected cameras share does not resolve, for the
// same reason: the node holding the link is one of the cameras it could mean,
// not the one it does. sharedByIDRefusal says so to the caller.
func resolveStableID(names map[string]stableNames, stableID string) (uint32, bool) {
	scheme, want, tagged := strings.Cut(strings.TrimSpace(stableID), ":")
	if !tagged || want == "" {
		return 0, false
	}
	// An untagged or unknown scheme is not an identity this API ever handed
	// out. Guessing which of the two was meant is how a name for one camera
	// quietly resolves to another.
	var pick func(stableNames) string
	switch scheme {
	case schemeByID:
		pick = func(n stableNames) string { return n.byID }
	case schemeByPath:
		pick = func(n stableNames) string { return n.byPath }
	default:
		return 0, false
	}
	for devPath, n := range names {
		if pick(n) != want {
			continue
		}
		if scheme == schemeByID && n.twin != "" {
			return 0, false
		}
		num, err := deviceNumber(devPath)
		if err != nil {
			return 0, false
		}
		return num, true
	}
	return 0, false
}

// sharedByIDRefusal is the error for a "by-id:" name that two connected
// cameras share, and nil for every other name.
//
// It is separate from "no camera with that stable id" because the fix is
// different. The camera is there; what is gone is the name's ability to say
// which one, and an operator told "not found" goes looking for a missing
// camera. So the error says why, and names the identity each of those cameras
// is listed under now -- the alternative is reading agent logs on a device they
// may not have.
func sharedByIDRefusal(names map[string]stableNames, stableID string) error {
	scheme, want, _ := strings.Cut(strings.TrimSpace(stableID), ":")
	if scheme != schemeByID || want == "" {
		return nil
	}
	twin := ""
	for _, n := range names {
		if n.byID == want {
			twin = n.twin
			break
		}
	}
	if twin == "" {
		return nil
	}
	var ports []string
	for _, n := range names {
		if n.twin == twin && n.byPath != "" {
			ports = append(ports, schemeByPath+":"+n.byPath)
		}
	}
	slices.Sort(ports)
	use := fmt.Sprintf("use the port-based id of the camera you mean: %v", ports)
	if len(ports) == 0 {
		use = "and this device has no port-based id for them either"
	}
	return status.Errorf(codes.FailedPrecondition,
		"stable id %q is ambiguous: two or more connected cameras have the same vendor, model and serial number, so it cannot tell them apart; %s",
		strings.TrimSpace(stableID), use)
}

// deviceNumber extracts N from a "/dev/videoN" path. Same parse listCameras
// does on the glob results, kept here so the two cannot drift.
func deviceNumber(devPath string) (uint32, error) {
	base := filepath.Base(devPath)
	n, err := strconv.ParseUint(strings.TrimPrefix(base, "video"), 10, 32)
	if err != nil {
		return 0, fmt.Errorf("not a video device path: %q", devPath)
	}
	return uint32(n), nil
}
