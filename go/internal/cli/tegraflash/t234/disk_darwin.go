//go:build darwin

package t234

import (
	"fmt"
	"os/exec"
	"regexp"
	"strconv"
	"strings"

	"github.com/google/gousb"
)

// tegraUSBHint reports which Tegra-relevant USB devices are present, so a
// timed-out stage-2 wait can distinguish a board that rebooted into recovery
// from one still exposing the flashing gadget or gone from USB entirely.
func tegraUSBHint() string {
	ctx := gousb.NewContext()
	ctx.Debug(0)
	defer ctx.Close()

	var found []string
	// The filter is called for every device; returning false opens none of
	// them (reading the descriptor needs no claim, so this can't fail on a
	// busy/permission-guarded device).
	_, _ = ctx.OpenDevices(func(d *gousb.DeviceDesc) bool {
		if label := tegraUSBLabel(uint16(d.Vendor), uint16(d.Product)); label != "" {
			found = append(found, label)
		}
		return false
	})
	if len(found) == 0 {
		return "No NVIDIA recovery (0955:*) or flashing-gadget (1d6b:0104) USB device is present — the board has left USB."
	}
	return "Tegra USB devices present: " + strings.Join(found, ", ")
}

// listUMSDisks finds USB mass-storage whole disks and their SCSI inquiry
// strings by walking `ioreg -rc IOSCSILogicalUnitNub`: the logical-unit nub
// carries "Vendor Identification"/"Product Identification" (the INQUIRY
// response) and its subtree holds the IOMedia with the whole-disk "BSD Name".
// (The properties live on the nub itself — rooting the query any deeper,
// e.g. at IOSCSIPeripheralDeviceType00, loses them; verified on the real
// flashing gadget.)
func listUMSDisks() ([]UMSDisk, error) {
	// Root at each USB device in the default IOService plane so the chunk
	// contains both the gadget's physical locationID and its descendant SCSI
	// inquiry/IOMedia properties. The IOUSB plane omits the block-storage
	// subtree, leaving Vendor Identification / BSD Name empty.
	out, err := exec.Command("ioreg", "-rc", "IOUSBHostDevice", "-l", "-w0").Output()
	if err != nil {
		return nil, fmt.Errorf("ioreg: %w", err)
	}
	return parseUMSDisks(string(out)), nil
}

// parseUMSDisks extracts flashpkg-style UMS LUNs from `ioreg -rc IOUSBHostDevice
// -l -w0` output. Split out from listUMSDisks so the nested-hub topology parsing
// can be exercised without a live device.
func parseUMSDisks(out string) []UMSDisk {
	var disks []UMSDisk
	for _, chunk := range splitIoregSubtrees(out) {
		if ioregInt(chunk, "idVendor") != GadgetVendorID || ioregInt(chunk, "idProduct") != GadgetProductID {
			continue
		}
		port := macUSBPortPath(ioregInt(chunk, "locationID"))
		// Each LUN has its own nub; the first piece is the device itself.
		for _, lun := range splitIoreg(chunk, ioregLUNLine)[1:] {
			vendor := ioregString(lun, "Vendor Identification")
			if vendor == "" {
				continue
			}
			bsd := ioregString(lun, "BSD Name")
			if !wholeDiskRe.MatchString(bsd) {
				continue // no media yet, or a partition slice matched first
			}
			name, serial := splitInquiry(vendor, ioregString(lun, "Product Identification"))
			d := UMSDisk{
				DevPath:  "/dev/" + bsd,
				RawPath:  "/dev/r" + bsd,
				Vendor:   name,
				Serial:   serial,
				PortPath: port,
			}
			if size := ioregInt(lun, "Size"); size > 0 {
				d.SizeBytes = size
			}
			disks = append(disks, d)
		}
	}
	return disks
}

func macUSBPortPath(location int64) string {
	if location <= 0 {
		return ""
	}
	bus := (location >> 24) & 0xff
	var ports []string
	for shift := 20; shift >= 0; shift -= 4 {
		port := (location >> shift) & 0xf
		if port == 0 {
			break
		}
		ports = append(ports, strconv.FormatInt(port, 10))
	}
	// bus 0 is a real controller on Apple Silicon; gousb (portKey) numbers it
	// the same way, so only an empty port chain (a root hub) is unusable here.
	if len(ports) == 0 {
		return ""
	}
	return fmt.Sprintf("%d-%s", bus, strings.Join(ports, "."))
}

// rawUMSInquiry lists every IOSCSILogicalUnitNub's raw INQUIRY vendor/product
// and BSD name, without the whole-disk filter listUMSDisks applies — a
// diagnostic for a wait that timed out. Showing LUNs that lack a "diskN" BSD
// name reveals a device that exported the LUN but that macOS never surfaced as
// a whole disk.
func rawUMSInquiry() string {
	out, err := exec.Command("ioreg", "-rc", "IOSCSILogicalUnitNub", "-l", "-w0").Output()
	if err != nil {
		return ""
	}
	var b strings.Builder
	for _, chunk := range splitIoreg(string(out), ioregLUNLine) {
		vendor := ioregString(chunk, "Vendor Identification")
		if vendor == "" {
			continue
		}
		bsd := ioregString(chunk, "BSD Name")
		if bsd == "" {
			bsd = "(no BSD name)"
		}
		fmt.Fprintf(&b, "  - vendor=%q product=%q bsd=%q size=%d\n",
			strings.TrimSpace(vendor), strings.TrimSpace(ioregString(chunk, "Product Identification")),
			bsd, ioregInt(chunk, "Size"))
	}
	return b.String()
}

// ioregDeviceLine matches an ioreg tree line that introduces an IOUSBHostDevice,
// at any indentation depth — "+-o <name>@<loc>  <class IOUSBHostDevice, id …>"
// whether it sits at column 0 or nested under a parent (e.g. "  | | +-o …").
var ioregDeviceLine = regexp.MustCompile(`\+-o .*<class IOUSBHostDevice[,>]`)

// ioregLUNLine matches the line that introduces one SCSI logical unit.
var ioregLUNLine = regexp.MustCompile(`\+-o .*<class IOSCSILogicalUnitNub[,>]`)

// splitIoregSubtrees breaks `ioreg -rc IOUSBHostDevice` output into one chunk per
// USB device. It splits on every IOUSBHostDevice line at any depth, not only
// column-0 roots: `ioreg -r` nests a device inside its parent hub when the target
// sits behind one, and Apple Silicon's internal USB 2.0 hub is itself an
// IOUSBHostDevice. Splitting only on column-0 boundaries collapsed hub+device
// into a single chunk, so listUMSDisks read the hub's idVendor and discarded the
// real gadget — a flash could never find its flashpkg LUN through that hub
// (WDY-2621). A device's non-device descendants (USB interfaces, SCSI nubs,
// IOMedia) carry no IOUSBHostDevice line, so they stay in the device's chunk,
// which is where Vendor Identification and BSD Name live.
func splitIoregSubtrees(out string) []string {
	return splitIoreg(out, ioregDeviceLine)
}

// splitIoreg breaks ioreg output into chunks, starting a new one at every line
// that matches boundary.
func splitIoreg(out string, boundary *regexp.Regexp) []string {
	var chunks []string
	var cur strings.Builder
	for _, line := range strings.Split(out, "\n") {
		if boundary.MatchString(line) && cur.Len() > 0 {
			chunks = append(chunks, cur.String())
			cur.Reset()
		}
		cur.WriteString(line)
		cur.WriteString("\n")
	}
	if cur.Len() > 0 {
		chunks = append(chunks, cur.String())
	}
	return chunks
}

// wholeDiskRe matches a whole-disk BSD name (diskN, not a diskNsM slice).
var wholeDiskRe = regexp.MustCompile(`^disk\d+$`)

// ioregStrRe/ioregIntRe hold extractors for the fixed set of ioreg keys this
// package reads. Populated once at init and read-only thereafter, so concurrent
// callers are safe; an unlisted key falls back to a one-off compile.
var (
	ioregStrRe = map[string]*regexp.Regexp{}
	ioregIntRe = map[string]*regexp.Regexp{}
)

func init() {
	for _, key := range []string{"Vendor Identification", "Product Identification", "BSD Name"} {
		ioregStrRe[key] = compileIoregStrRe(key)
	}
	for _, key := range []string{"idVendor", "idProduct", "locationID", "Size"} {
		ioregIntRe[key] = compileIoregIntRe(key)
	}
}

func compileIoregStrRe(key string) *regexp.Regexp {
	return regexp.MustCompile(`"` + regexp.QuoteMeta(key) + `" = "([^"]*)"`)
}

func compileIoregIntRe(key string) *regexp.Regexp {
	return regexp.MustCompile(`"` + regexp.QuoteMeta(key) + `" = (0x[0-9a-fA-F]+|\d+)`)
}

// ioregString extracts the first `"key" = "value"` in an ioreg chunk.
func ioregString(chunk, key string) string {
	re, ok := ioregStrRe[key]
	if !ok {
		re = compileIoregStrRe(key)
	}
	if m := re.FindStringSubmatch(chunk); m != nil {
		return m[1]
	}
	return ""
}

// ioregInt extracts the first `"key" = <number>` in an ioreg chunk.
func ioregInt(chunk, key string) int64 {
	re, ok := ioregIntRe[key]
	if !ok {
		re = compileIoregIntRe(key)
	}
	if m := re.FindStringSubmatch(chunk); m != nil {
		if n, err := strconv.ParseInt(m[1], 0, 64); err == nil {
			return n
		}
	}
	return 0
}

// unmountUMSDisk unmounts anything macOS auto-mounted from the LUN (e.g. the
// FAT config partition after the GPT lands). Best-effort: the LUNs usually
// carry no mountable filesystem, and diskutil's exit status is not surfaced —
// only the Windows implementation can distinguish "nothing mounted" from a
// lock refusal worth reporting.
func unmountUMSDisk(d UMSDisk) error {
	exec.Command("diskutil", "unmountDisk", "force", d.DevPath).Run() //nolint:errcheck
	return nil
}

// ejectUMSDisk ejects the LUN's medium — the "host is done" signal the
// flashing initrd waits for. diskutil eject never disconnects the USB device.
func ejectUMSDisk(d UMSDisk) error {
	if out, err := exec.Command("diskutil", "eject", d.DevPath).CombinedOutput(); err != nil {
		return fmt.Errorf("diskutil eject %s: %v: %s", d.DevPath, err, strings.TrimSpace(string(out)))
	}
	return nil
}
