package usbthermal

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
)

// attached is one supported camera currently on the bus, as sysfs describes
// it.
type attached struct {
	model    Model
	port     string // sysfs device name: bus-port chain, e.g. "1-2.3"
	busNum   int
	devNum   int
	serial   string
	product  string
	stableID string // filled in by assignStableIDs
}

// devNode is the usbfs node for the device. devnum changes every time the
// device is plugged in, so it is re-read from sysfs before every open.
func (a attached) devNode(devRoot string) string {
	return filepath.Join(devRoot, "bus", "usb", fmt.Sprintf("%03d", a.busNum), fmt.Sprintf("%03d", a.devNum))
}

// scanSysfs lists the supported cameras under <sysfsRoot>/bus/usb/devices.
// Reading it touches no device: these are attribute files the kernel filled in
// at enumeration, so listing never disturbs a camera that is streaming.
func scanSysfs(sysfsRoot string) []attached {
	dir := filepath.Join(sysfsRoot, "bus", "usb", "devices")
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var out []attached
	for _, e := range entries {
		name := e.Name()
		// Interfaces ("1-2.3:1.0") and root hubs ("usb1") are not devices a
		// camera can be.
		if strings.Contains(name, ":") || strings.HasPrefix(name, "usb") {
			continue
		}
		devDir := filepath.Join(dir, name)
		vendor, ok1 := readHexAttr(devDir, "idVendor")
		product, ok2 := readHexAttr(devDir, "idProduct")
		if !ok1 || !ok2 {
			continue
		}
		model, ok := modelFor(vendor, product)
		if !ok {
			continue
		}
		bus, ok1 := readIntAttr(devDir, "busnum")
		dev, ok2 := readIntAttr(devDir, "devnum")
		if !ok1 || !ok2 {
			continue
		}
		out = append(out, attached{
			model:   model,
			port:    name,
			busNum:  bus,
			devNum:  dev,
			serial:  readAttr(devDir, "serial"),
			product: readAttr(devDir, "product"),
		})
	}
	slices.SortFunc(out, func(a, b attached) int { return strings.Compare(a.port, b.port) })
	assignStableIDs(out)
	return out
}

func readAttr(dir, name string) string {
	b, err := os.ReadFile(filepath.Join(dir, name))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

func readHexAttr(dir, name string) (uint16, bool) {
	v, err := strconv.ParseUint(readAttr(dir, name), 16, 16)
	return uint16(v), err == nil
}

func readIntAttr(dir, name string) (int, bool) {
	v, err := strconv.Atoi(readAttr(dir, name))
	return v, err == nil && v > 0
}

// These cameras can share a factory serial, including when only one is
// attached at discovery time. Use Wendy's by-path scheme for every camera,
// rather than promoting a surviving twin into another camera's by-id name.
// The name survives rescans, replugging and agent restarts on the same USB
// port; it identifies that port, not a camera moved to another port.
func assignStableIDs(cams []attached) {
	for i := range cams {
		cams[i].stableID = "by-path:usb-" + cams[i].port + "-thermal"
	}
}
