// Package robotarm discovers USB serial metadata and persists host-local arm
// bindings. It deliberately contains no serial I/O or motor-control code.
package robotarm

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"

	pb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
)

// Identify derives an endpoint identity from USB metadata, never a tty number.
// A serial number is adapter identity, not proof of the attached arm's model.
func Identify(d *pb.RobotArm) {
	d.IdentityKind = "usb_serial"
	key, kind := d.SerialNumber, "s"
	if key == "" {
		d.IdentityKind, key, kind = "usb_location", d.UsbLocation, "l"
	}
	d.Id = fmt.Sprintf("usb:%s:%s:%s:%s:%s", d.VendorId, d.ProductId, kind, hex.EncodeToString([]byte(key)), d.UsbInterface)
	d.Candidate = d.VendorId == "1a86" && (d.ProductId == "55d3" || d.ProductId == "55d4" || d.ProductId == "7523")
	d.Connected, d.ConnectionStatus, d.AccessStatus = true, "present", "unknown"
	d.Note = "USB serial device; arm model, motor power, port access and busy state are unverified."
	if d.Candidate {
		d.Note = "SO-101 adapter candidate (shared USB adapter IDs); attached arm is unverified."
	}
	if d.IdentityKind == "usb_location" {
		d.Note += " No serial ID: binding follows this USB port; replacement devices cannot be distinguished."
	}
}

var ErrUnsupported = errors.New("USB arm inventory requires a Linux Agent or the native Mac Agent")

func Discover(ctx context.Context) ([]*pb.RobotArm, error) {
	if runtime.GOOS != "linux" {
		return nil, ErrUnsupported
	}
	return DiscoverLinux(ctx, "/sys/class/tty", "/dev")
}

// DiscoverLinux only reads sysfs and node metadata. Roots are injectable for tests.
func DiscoverLinux(ctx context.Context, ttyRoot, devRoot string) ([]*pb.RobotArm, error) {
	entries, err := os.ReadDir(ttyRoot)
	if err != nil {
		return nil, err
	}
	devices := make([]*pb.RobotArm, 0)
	read := func(dir, name string) string {
		b, _ := os.ReadFile(filepath.Join(dir, name))
		return strings.TrimSpace(string(b))
	}
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		endpoint, err := filepath.EvalSymlinks(filepath.Join(ttyRoot, entry.Name(), "device"))
		if err != nil {
			continue
		} // Includes unplug during enumeration.
		iface := ""
		for dir := endpoint; dir != "/" && dir != "."; dir = filepath.Dir(dir) {
			if value := read(dir, "bInterfaceNumber"); value != "" {
				if n, err := strconv.ParseUint(value, 16, 8); err == nil {
					iface = strconv.FormatUint(n, 10)
				}
			}
			vid, pid := read(dir, "idVendor"), read(dir, "idProduct")
			if vid == "" || pid == "" {
				continue
			}
			// No serial node is opened; an unpowered arm can still expose its adapter.
			path := filepath.Join(devRoot, entry.Name())
			if _, err := os.Stat(path); err != nil {
				break
			}
			d := &pb.RobotArm{DevicePath: path, VendorId: strings.ToLower(vid), ProductId: strings.ToLower(pid),
				SerialNumber: read(dir, "serial"), Manufacturer: read(dir, "manufacturer"), Product: read(dir, "product"),
				UsbLocation: filepath.Base(dir), UsbInterface: iface}
			Identify(d)
			devices = append(devices, d)
			break
		}
	}
	sort.Slice(devices, func(i, j int) bool { return devices[i].DevicePath < devices[j].DevicePath })
	return devices, nil
}
