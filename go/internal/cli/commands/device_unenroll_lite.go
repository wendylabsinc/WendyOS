package commands

import (
	"context"
	"encoding/hex"
	"fmt"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"github.com/wendylabsinc/wendy/go/internal/cli/liteclient"
	"github.com/wendylabsinc/wendy/go/internal/cli/providers"
	"github.com/wendylabsinc/wendy/go/internal/shared/meshid"
	"github.com/wendylabsinc/wendy/go/internal/shared/models"
	litepb "github.com/wendylabsinc/wendy/go/proto/gen/litepb"
)

type liteUnenrollmentDevice interface {
	GetDeviceIdentity(time.Duration) (*liteclient.DeviceIdentity, error)
	EnrollmentChallenge(bool) (*litepb.WendyComEnrollmentChallenge, error)
	ResetEnrollment(string, string) error
	ResetTargetDevice(bool, time.Duration) error
	WaitReboot(context.Context)
	Close() error
}

var (
	connectLiteUnenrollmentFn = func(target models.ExternalDevice) (liteUnenrollmentDevice, error) {
		return (&providers.MicroWendyProvider{}).ConnectEnrollment(target)
	}
	unenrollLiteCloudFn = runCloudDeviceUnenrollWithResetForTenant
)

func runLiteDeviceUnenroll(cmd *cobra.Command, target models.ExternalDevice, cloudGRPC string, assumeYes bool) error {
	if target.ConnectionType() != "USB" {
		return fmt.Errorf("clearing a Wendy Lite enrollment requires a USB connection; select the USB device with --device, or use --cloud-only to revoke its cloud enrollment while keeping its local identity")
	}
	device, err := connectLiteUnenrollmentFn(target)
	if err != nil {
		return fmt.Errorf("connecting to Wendy Lite device: %w; if its firmware is unavailable, use 'wendy device unenroll --cloud-only --device <asset-id-or-name>'", err)
	}
	defer device.Close()
	identity, err := device.GetDeviceIdentity(5 * time.Second)
	if err != nil {
		return fmt.Errorf("reading Wendy Lite device identity: %w; remove its cloud enrollment with 'wendy device unenroll --cloud-only --device <asset-id-or-name>'", err)
	}
	if identity == nil || strings.TrimSpace(identity.ID) == "" {
		return fmt.Errorf("Wendy Lite device returned no device ID; remove its cloud enrollment with 'wendy device unenroll --cloud-only --device <asset-id-or-name>'")
	}
	// Check the firmware before deleting anything in Cloud. Older firmware
	// cannot finish unenrollment and must not leave the board stranded.
	challenge, err := device.EnrollmentChallenge(true)
	if err != nil {
		return fmt.Errorf("checking Wendy Lite enrollment reset support: %w; update the firmware over USB, or use --cloud-only to remove only the cloud enrollment", err)
	}
	if challenge == nil || !challenge.GetEnrollmentResetSupported() {
		return fmt.Errorf("this Wendy Lite firmware cannot clear its enrollment identity; update the firmware over USB before unenrolling, or use --cloud-only to revoke cloud access while keeping the local identity")
	}

	// Firmware that predates device UUIDs does not report its configured
	// identity. Updated firmware reports it, including an older lite- one.
	if !challenge.GetUuidEnrollmentSupported() {
		return fmt.Errorf("this Wendy Lite firmware does not report its enrollment identity; update the firmware over USB before unenrolling, or use --cloud-only to remove only the cloud enrollment")
	}
	// The firmware reports the configured PKI identity, including pending
	// enrollments.
	deviceID, tenantID := challenge.GetDeviceId(), challenge.GetTenantId()
	configured := deviceID != "" || tenantID != "" || challenge.GetEnrolled()
	if configured && (deviceID == "" || tenantID == "") {
		return fmt.Errorf("configured board did not report its complete PKI device and tenant identity; cloud enrollment was not changed")
	}
	if tenantID != "" {
		if _, err := meshid.ParseTenant(tenantID); err != nil {
			return fmt.Errorf("board returned an invalid PKI tenant identity; cloud enrollment was not changed: %w", err)
		}
	}
	// A board without an identity may still have an asset a failed enrollment
	// reserved. Find it by the hardware id enrollment records as its serial.
	hardwareID := ""
	if deviceID == "" {
		hardwareID = identity.ID
	}
	return unenrollLiteCloudFn(cmd, cloudGRPC, deviceID, tenantID, hardwareID, assumeYes, func() error {
		if err := cmd.Context().Err(); err != nil {
			return err
		}
		// Create the nonce only after confirmation and cloud cleanup. A
		// preflight must not invalidate a pending enrollment's signed time.
		challenge, err := device.EnrollmentChallenge(false)
		if err != nil {
			return fmt.Errorf("getting the board's reset challenge: %w", err)
		}
		nonce, err := hex.DecodeString(challenge.GetNonceHex())
		if err != nil || len(nonce) != 32 || !challenge.GetEnrollmentResetSupported() {
			return fmt.Errorf("Wendy Lite returned an invalid enrollment reset challenge")
		}
		if challenge.GetDeviceId() != deviceID || challenge.GetTenantId() != tenantID {
			return fmt.Errorf("the board's configured PKI identity changed during cloud cleanup; its local identity was not reset")
		}
		if err := device.ResetEnrollment(identity.ID, challenge.NonceHex); err != nil {
			return fmt.Errorf("clearing the board's enrollment: %w", err)
		}
		state, err := device.EnrollmentChallenge(true)
		if err != nil {
			return fmt.Errorf("verifying the board's cleared identity: %w", err)
		}
		if state == nil || state.Enrolled || state.GetDeviceId() != "" || state.GetTenantId() != "" {
			return fmt.Errorf("the board did not confirm that its enrollment identity was cleared")
		}
		if err := device.ResetTargetDevice(true, 0); err != nil {
			return fmt.Errorf("the enrollment identity was cleared but reboot failed; reboot the board before enrolling again: %w", err)
		}
		device.WaitReboot(cmd.Context())
		return nil
	})
}
