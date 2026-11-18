package commands

import (
	"context"
	"fmt"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/wendylabsinc/wendy/go/internal/cli/providers"
	"github.com/wendylabsinc/wendy/go/internal/cli/tui"
	"github.com/wendylabsinc/wendy/go/internal/shared/models"
)

const liteManualResetHelp = "If you used BOOT + RESET to enter flashing mode, release BOOT and press RESET once, or unplug and reconnect the board. The firmware is already installed; no reflash is needed."

var confirmLiteResetFn = func(ctx context.Context) (bool, error) {
	return mapConfirmCancel(tui.ConfirmDefaultYes("Have you reset the board and are ready to reconnect?", tea.WithContext(ctx)))
}

// A USB Serial/JTAG reset cannot clear the boot mode latched by BOOT + RESET.
// In particular, a watchdog reset is unsafe on C6. Allow a physical reset and
// resume the current run instead of sending the user back through installation.
func waitForLiteAfterInstall(ctx context.Context, p providers.DeviceProvider, device models.ExternalDevice, interactive bool, timeout time.Duration) error {
	usb := device.ConnectionInfo["serialPort"] != ""
	if usb {
		cliLogln("%s", liteManualResetHelp)
	}
	err := waitForDeviceReady(ctx, p, device, timeout)
	if err == nil || ctx.Err() != nil || !usb {
		return err
	}
	if interactive {
		cliNotice("Wendy Lite was flashed, but the board has not responded. %s", liteManualResetHelp)
		confirmed, promptErr := confirmLiteResetFn(ctx)
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if promptErr != nil {
			return promptErr
		}
		if !confirmed {
			return ErrUserCancelled
		}
		cliLogln("Reconnecting to the board...")
		err = waitForDeviceReady(ctx, p, device, timeout)
		if err == nil || ctx.Err() != nil {
			return err
		}
	}
	return fmt.Errorf("%s Then retry wendy run: %w", liteManualResetHelp, err)
}
