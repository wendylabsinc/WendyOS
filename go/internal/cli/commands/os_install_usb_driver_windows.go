//go:build windows

package commands

import (
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/spf13/cobra"
	"github.com/wendylabsinc/wendy/go/internal/cli/qdl"
	"github.com/wendylabsinc/wendy/go/internal/cli/tegraflash/winusb"
)

// Every recovery family uses the same selected-device/version/ID-coverage
// check. A different working board must never suppress an upgrade or repair.
func ensureUSBDriver(d winusb.Device, profile winusb.DriverProfile) error {
	return ensureUSBDriverTo(os.Stdout, d, profile)
}

func ensureUSBDriverTo(out io.Writer, d winusb.Device, profile winusb.DriverProfile) error {
	return ensureUSBDriverWith(out, d, profile, usbDriverOperations{
		ready: winusb.DriverReady, validate: winusb.ValidateDriverTarget,
		elevated: isElevated, install: winusb.InstallDriverFor,
		elevate: runElevatedUSBDriver, find: findUSBDriverTarget,
	})
}

type usbDriverOperations struct {
	ready    func(winusb.Device, winusb.DriverProfile) (bool, error)
	validate func(winusb.Device) error
	elevated func() (bool, error)
	install  func(io.Writer, winusb.Device, winusb.DriverProfile) error
	elevate  func(string) error
	find     func(string) (winusb.Device, error)
}

func ensureUSBDriverWith(out io.Writer, d winusb.Device, profile winusb.DriverProfile, ops usbDriverOperations) error {
	ready, err := ops.ready(d, profile)
	if err != nil {
		return err
	}
	if ready {
		return nil
	}
	if err := ops.validate(d); err != nil {
		return err
	}
	elevated, err := ops.elevated()
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "Installing or updating the %s USB driver…\n", profile.Name)
	if elevated {
		return ops.install(out, d, profile)
	}
	// Elevate only the driver operation. The parent retains its selected board,
	// wizard answers and live flash session, including a stage-2 gadget handoff.
	fmt.Fprintln(out, "Windows will ask for administrator approval. Flashing will continue here.")
	if err := ops.elevate(d.InstanceID); err != nil {
		return err
	}
	current, err := ops.find(d.InstanceID)
	if err != nil {
		return err
	}
	ready, err = ops.ready(current, profile)
	if err != nil {
		return err
	}
	if !ready {
		return fmt.Errorf("%s did not acquire a compatible %s binding", d.InstanceID, profile.Name)
	}
	return nil
}

func findUSBDriverTarget(instance string) (winusb.Device, error) {
	vid, _, ok := winusb.ParseVIDPID(instance)
	if !ok || !strings.HasPrefix(strings.ToUpper(instance), `USB\VID_`) {
		return winusb.Device{}, fmt.Errorf("invalid USB instance %q", instance)
	}
	devices, err := winusb.ListVendor(vid)
	if err != nil {
		return winusb.Device{}, err
	}
	for _, d := range devices {
		if strings.EqualFold(d.InstanceID, instance) {
			return d, nil
		}
	}
	return winusb.Device{}, fmt.Errorf("selected USB device %s is no longer present", instance)
}

func addUSBDriverCommand(root *cobra.Command) {
	root.AddCommand(&cobra.Command{
		Use: "__usb-driver INSTANCE", Hidden: true, Args: cobra.ExactArgs(1),
		// A privileged helper must not load or mutate the administrator's CLI
		// credentials, analytics or first-run settings.
		PersistentPreRunE:  func(*cobra.Command, []string) error { return nil },
		PersistentPostRunE: func(*cobra.Command, []string) error { return nil },
		RunE: func(cmd *cobra.Command, args []string) error {
			elevated, err := isElevated()
			if err != nil {
				return err
			}
			if !elevated {
				return fmt.Errorf("USB driver installation requires administrator privileges")
			}
			d, err := findUSBDriverTarget(args[0])
			if err != nil {
				return err
			}
			profile := winusb.JetsonDriver()
			if d.VID == qdl.VendorQualcomm {
				profile = winusb.DragonwingDriver()
			}
			// DriverReady validates the profile's supported IDs before installing.
			return ensureUSBDriverTo(cmd.OutOrStdout(), d, profile)
		},
	})
}

func prepareDragonwingHost(info qdl.DeviceInfo) error {
	devices, err := winusb.ListVendor(qdl.VendorQualcomm)
	if err != nil {
		return err
	}
	for _, d := range devices {
		if strings.EqualFold(d.InstanceID, info.Instance) {
			return ensureUSBDriver(d, winusb.DragonwingDriver())
		}
	}
	return fmt.Errorf("selected EDL device %s disconnected", info.Instance)
}
