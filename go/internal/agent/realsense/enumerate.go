package realsense

import (
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

const intelVID = "8086"

// knownModels names the PIDs this package has evidence for. An unlisted D4xx
// enumerates fine — identification keys on the device's own name and VID —
// but callers gating depth support can insist on a known model so that a new
// variant fails visibly ("unrecognised model") instead of wrongly.
var knownModels = map[string]string{
	"0ad3": "D415",
	"0b07": "D435",
	"0b3a": "D435i", // hardware-validated 2026-10-12
	"0b5b": "D405",
	"0b5c": "D455",
}

// Camera is one RealSense module: its USB identity and the V4L2 capture nodes
// of its depth and colour functions. Node numbers renumber across power
// cycles (observed live: the same D435i moved video6/8/10 → video0–5), so
// nothing may persist them; Serial is the stable identity.
type Camera struct {
	Serial     string
	PID        string // USB product id, lower-case hex, no 0x
	Model      string // from knownModels, or "" when unrecognised
	DepthNode  string // /dev/videoN of the depth function (interface 00)
	ColourNode string // /dev/videoN of the colour function (interface 03), "" if absent
}

// Detect walks a sysfs/dev root ("" for the live system) and returns every
// RealSense module found, grouped by physical USB device. The root is a
// parameter so fixture trees drive the tests — the gpudiscovery pattern.
func Detect(root string) ([]Camera, error) {
	entries, err := filepath.Glob(filepath.Join(root, "/sys/class/video4linux/video*"))
	if err != nil {
		return nil, err
	}
	sort.Slice(entries, func(i, j int) bool { return videoIndex(entries[i]) < videoIndex(entries[j]) })

	// One UVC function (interface) exposes several videoN nodes — the capture
	// node first, metadata nodes after it. Grouping by USB device path and
	// keeping the lowest-numbered node per interface selects the capture node.
	type key struct{ device, iface string }
	nodes := map[key]string{}
	devices := map[string]string{} // USB device sysfs path -> example interface path
	for _, e := range entries {
		name, _ := os.ReadFile(filepath.Join(e, "name"))
		if !strings.Contains(string(name), "RealSense") {
			continue
		}
		// videoN and videoN/device are symlinks into /sys/devices; resolve
		// before taking the parent, or ".." collapses textually and lands
		// back in the class directory.
		ifaceDir, err := filepath.EvalSymlinks(filepath.Join(e, "device"))
		if err != nil {
			continue
		}
		iface, _ := os.ReadFile(filepath.Join(ifaceDir, "bInterfaceNumber"))
		usbDev := filepath.Dir(ifaceDir)
		vid, _ := os.ReadFile(filepath.Join(usbDev, "idVendor"))
		if strings.TrimSpace(string(vid)) != intelVID {
			continue
		}
		k := key{usbDev, strings.TrimSpace(string(iface))}
		if _, taken := nodes[k]; !taken {
			nodes[k] = "/dev/" + filepath.Base(e)
		}
		devices[usbDev] = ""
	}

	var cams []Camera
	for usbDev := range devices {
		pid, _ := os.ReadFile(filepath.Join(usbDev, "idProduct"))
		serial, _ := os.ReadFile(filepath.Join(usbDev, "serial"))
		cam := Camera{
			Serial:     strings.TrimSpace(string(serial)),
			PID:        strings.ToLower(strings.TrimSpace(string(pid))),
			DepthNode:  nodes[key{usbDev, "00"}],
			ColourNode: nodes[key{usbDev, "03"}],
		}
		cam.Model = knownModels[cam.PID]
		if cam.DepthNode == "" {
			// A RealSense without its depth function is not usable through
			// this package; leave it to the generic camera stack.
			continue
		}
		cams = append(cams, cam)
	}
	sort.Slice(cams, func(i, j int) bool { return cams[i].Serial < cams[j].Serial })
	return cams, nil
}

func videoIndex(path string) int {
	n, _ := strconv.Atoi(strings.TrimPrefix(filepath.Base(path), "video"))
	return n
}
