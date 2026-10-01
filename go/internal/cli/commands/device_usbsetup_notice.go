package commands

import "fmt"

// usbSetupNeededNotice explains, to whoever can't answer the USB-C setup
// prompt in `wendy discover` (an agent, a script, --json), why a tethered
// device can't be reached and how a person fixes it. The setup itself stays
// behind that prompt: it needs sudo and changes the host's network
// configuration.
func usbSetupNeededNotice(iface string) string {
	return fmt.Sprintf("A Wendy device is connected over USB-C (%s), but this host's link to it isn't configured, so it can't be reached yet. Run `wendy discover` in a terminal and accept the USB-C setup prompt (it needs sudo).", iface)
}

// pendingUSBSetupNotice returns usbSetupNeededNotice for a tethered device
// whose host link still needs setup, or "" when there is none. Only Linux hosts
// need that setup, so elsewhere it is always "".
func pendingUSBSetupNotice() string {
	if iface := pendingUSBSetupIface(); iface != "" {
		return usbSetupNeededNotice(iface)
	}
	return ""
}
