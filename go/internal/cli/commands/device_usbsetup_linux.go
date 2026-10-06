//go:build linux

package commands

import (
	"context"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"strings"

	"github.com/wendylabsinc/wendy/go/internal/shared/discovery"
	"github.com/wendylabsinc/wendy/go/internal/shared/nmcli"
)

const (
	// usbSetupNMConnPrefix prefixes the per-device NetworkManager profile name
	// (see usbSetupConnName). The bare prefix was the name of the old
	// interface-bound "shared" profile, which setup now deletes.
	usbSetupNMConnPrefix = "wendy-usb"
	// usbSetupUdevPath is the host udev rule that keeps ModemManager off the gadget.
	usbSetupUdevPath = "/etc/udev/rules.d/99-wendy-usb.rules"
	// usbGadgetVID/PID identify the WendyOS USB-CDC composite gadget.
	usbGadgetVID = "1d6b"
	usbGadgetPID = "0104"
)

// usbSetupUdevRule tags the Wendy USB gadget so ModemManager leaves its CDC-ACM
// serial console (/dev/ttyACM*) and CDC-NCM network port alone.
const usbSetupUdevRule = `# Installed by 'wendy discover' USB-C auto-setup.
# Stop ModemManager from probing the Wendy USB gadget's serial console
# (/dev/ttyACM*) and network port (VID:PID 1d6b:0104).
ACTION=="add|change", SUBSYSTEM=="tty", SUBSYSTEMS=="usb", ATTRS{idVendor}=="1d6b", ATTRS{idProduct}=="0104", ENV{ID_MM_DEVICE_IGNORE}="1"
ACTION=="add|change", SUBSYSTEM=="net", SUBSYSTEMS=="usb", ATTRS{idVendor}=="1d6b", ATTRS{idProduct}=="0104", ENV{ID_MM_DEVICE_IGNORE}="1"
`

// runUSBSetup configures this Linux host so a USB-C-tethered Wendy device is
// reachable. It brings up a link-local NetworkManager profile (IPv4 169.254/16
// and IPv6 fe80::/64, matching a stock device's link-local usb0) and installs a
// udev rule so ModemManager stops grabbing the gadget's serial console. The
// profile is bound to the gadget's host-side MAC (derived from the device
// serial), not the interface name, which changes with every USB port.
//
// It must run as root and is only ever invoked via the hidden "__usb-setup"
// subcommand under sudo (see maybeOfferUSBSetup).
func runUSBSetup(ctx context.Context, iface string, w io.Writer) error {
	if os.Geteuid() != 0 {
		return fmt.Errorf("__usb-setup must run as root")
	}

	iface, err := resolveUSBSetupInterface(iface)
	if err != nil {
		return err
	}

	mac, err := usbIfaceMAC(iface)
	if err != nil {
		return err
	}
	conn := usbSetupConnName(mac)

	fmt.Fprintln(w, "Wendy USB-C host setup")
	fmt.Fprintf(w, "  interface: %s (MAC %s)\n", iface, mac)
	fmt.Fprintln(w, "  ip mode:   link-local (IPv4 169.254/16, IPv6 fe80::/64; no DHCP)")
	fmt.Fprintf(w, "  udev rule: %s (ModemManager-ignore for %s:%s)\n", usbSetupUdevPath, usbGadgetVID, usbGadgetPID)

	addArgs := usbSetupAddArgs(conn, mac)

	nmcliPath, err := exec.LookPath("nmcli")
	if err != nil {
		return fmt.Errorf("nmcli not found; install NetworkManager or configure %s manually: %w", iface, err)
	}

	// Replace any stale profile for this device, and the old interface-bound
	// "shared" profile. Ignore errors — they may not exist.
	_ = nmcli.Command(ctx, nmcliPath, "connection", "delete", conn).Run()
	_ = nmcli.Command(ctx, nmcliPath, "connection", "delete", usbSetupNMConnPrefix).Run()

	if b, err := nmcli.Command(ctx, nmcliPath, addArgs...).CombinedOutput(); err != nil {
		return fmt.Errorf("nmcli connection add failed: %v: %s", err, strings.TrimSpace(string(b)))
	}
	if b, err := nmcli.Command(ctx, nmcliPath, "connection", "up", conn).CombinedOutput(); err != nil {
		return fmt.Errorf("nmcli connection up failed: %v: %s", err, strings.TrimSpace(string(b)))
	}
	fmt.Fprintf(w, "✓ configured %s (link-local, profile %s)\n", iface, conn)

	if err := os.WriteFile(usbSetupUdevPath, []byte(usbSetupUdevRule), 0o644); err != nil {
		return fmt.Errorf("writing %s: %w", usbSetupUdevPath, err)
	}
	if err := reloadUdev(ctx); err != nil {
		return fmt.Errorf("reloading udev: %w", err)
	}
	fmt.Fprintf(w, "✓ installed ModemManager-ignore rule: %s\n", usbSetupUdevPath)
	fmt.Fprintln(w, "\nDone. Re-plug the device if it was already connected, then run 'wendy discover'.")
	return nil
}

// usbIfaceMAC returns the MAC address of iface, which the profile binds to.
func usbIfaceMAC(iface string) (net.HardwareAddr, error) {
	link, err := net.InterfaceByName(iface)
	if err != nil {
		return nil, fmt.Errorf("looking up %s: %w", iface, err)
	}
	if len(link.HardwareAddr) == 0 {
		return nil, fmt.Errorf("%s has no MAC address to bind the profile to", iface)
	}
	return link.HardwareAddr, nil
}

// usbSetupConnName names the profile for the gadget whose host-side interface
// has this MAC, so every tethered device gets its own profile.
func usbSetupConnName(mac net.HardwareAddr) string {
	return usbSetupNMConnPrefix + "-" + strings.ReplaceAll(mac.String(), ":", "")
}

// usbSetupAddArgs is the nmcli argv that creates the link-local profile for the
// gadget with this host-side MAC. No ifname: the MAC alone selects the device.
func usbSetupAddArgs(conn string, mac net.HardwareAddr) []string {
	return []string{"connection", "add", "type", "ethernet", "con-name", conn,
		"ethernet.mac-address", mac.String(), "connection.autoconnect", "yes",
		"ipv4.method", "link-local", "ipv6.method", "link-local"}
}

// resolveUSBSetupInterface returns the gadget interface to configure, using the
// override when given or auto-detecting otherwise.
func resolveUSBSetupInterface(override string) (string, error) {
	if override != "" {
		return override, nil
	}
	found, err := discovery.USBNetworkInterfaceNames()
	if err != nil {
		return "", fmt.Errorf("detecting USB interfaces: %w", err)
	}
	switch len(found) {
	case 0:
		return "", fmt.Errorf("no USB gadget interface found.\n" +
			"  Connect the device with a data (not charge-only) USB-C cable, wait a few seconds, then retry.")
	case 1:
		return found[0], nil
	default:
		return "", fmt.Errorf("multiple USB interfaces found (%s); pass one with --iface <name>", strings.Join(found, ", "))
	}
}

// reloadUdev reloads udev rules and re-triggers matching so the ModemManager
// ignore tag applies to an already-connected device.
func reloadUdev(ctx context.Context) error {
	udevadm, err := exec.LookPath("udevadm")
	if err != nil {
		return fmt.Errorf("udevadm not found: %w", err)
	}
	if b, err := exec.CommandContext(ctx, udevadm, "control", "--reload-rules").CombinedOutput(); err != nil {
		return fmt.Errorf("udevadm control: %v: %s", err, strings.TrimSpace(string(b)))
	}
	if b, err := exec.CommandContext(ctx, udevadm, "trigger").CombinedOutput(); err != nil {
		return fmt.Errorf("udevadm trigger: %v: %s", err, strings.TrimSpace(string(b)))
	}
	return nil
}
