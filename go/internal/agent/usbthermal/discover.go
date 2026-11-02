package usbthermal

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

// attached is one supported camera currently on the bus, as sysfs describes
// it.
type attached struct {
	model        Model
	port         string // sysfs device name: bus-port chain, e.g. "1-2.3"
	busNum       int
	devNum       int
	serial       string
	manufacturer string
	product      string
	stableID     string // filled in by assignStableIDs
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
			model:        model,
			port:         name,
			busNum:       bus,
			devNum:       dev,
			serial:       readAttr(devDir, "serial"),
			manufacturer: readAttr(devDir, "manufacturer"),
			product:      readAttr(devDir, "product"),
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

// Stable identity, in the scheme ListVideoDevices already uses for V4L2
// cameras (see services/video_stable_id.go): "by-id:<name>" built from vendor,
// product and serial, which survives a reboot and a move to another port, or
// "by-path:<name>" from the port when that is all that separates a camera.
//
// udev cannot name these cameras itself, because it names video nodes and
// there is none. So the by-id name is built the way udev builds its
// /dev/v4l/by-id links -- "usb-" + vendor_product_serial with the same
// character clean-up -- and ends in "-thermal" where a video node's would end
// in "-video-index0".
//
// Same rule as udev for twins: two cameras that report the same serial (a
// factory placeholder serial is common on these modules; the exxon-01 unit
// reports YFDVS000000000001) cannot both own one by-id name, so the first by
// port keeps it and the other is named by its port.
const stableSuffix = "-thermal"

func assignStableIDs(cams []attached) {
	taken := map[string]bool{}
	for i := range cams {
		c := &cams[i]
		if c.serial != "" {
			name := "usb-" + udevSerial(c) + stableSuffix
			if !taken[name] {
				taken[name] = true
				c.stableID = "by-id:" + name
				continue
			}
		}
		c.stableID = "by-path:usb-" + c.port + stableSuffix
	}
}

// udevSerial is udev's ID_SERIAL: vendor_model_serial, each part cleaned the
// way udev cleans them.
func udevSerial(c *attached) string {
	vendor := c.manufacturer
	if vendor == "" {
		vendor = fmt.Sprintf("%04x", c.model.VendorID)
	}
	product := c.product
	if product == "" {
		product = fmt.Sprintf("%04x", c.model.ProductID)
	}
	return udevClean(vendor) + "_" + udevClean(product) + "_" + udevClean(c.serial)
}

// udevClean mirrors udev's udev_replace_whitespace followed by
// udev_replace_chars: trim, collapse each whitespace run to one '_', then turn
// anything outside [0-9A-Za-z#+-.:=@_] (and outside valid multi-byte UTF-8)
// into '_'. "TOPDON TECHNOLOGY Co., Ltd." becomes
// "TOPDON_TECHNOLOGY_Co.__Ltd.".
func udevClean(s string) string {
	fields := strings.FieldsFunc(s, unicode.IsSpace)
	s = strings.Join(fields, "_")
	var b strings.Builder
	for _, r := range s {
		switch {
		case r < utf8.RuneSelf && (r >= '0' && r <= '9' || r >= 'A' && r <= 'Z' || r >= 'a' && r <= 'z'):
			b.WriteRune(r)
		case strings.ContainsRune("#+-.:=@_", r):
			b.WriteRune(r)
		case r >= utf8.RuneSelf && r != utf8.RuneError:
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	return b.String()
}
