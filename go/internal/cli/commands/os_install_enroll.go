//go:build darwin || linux || windows

package commands

import (
	"context"
	"errors"
	"fmt"
	"strconv"

	"github.com/wendylabsinc/wendy/go/internal/cli/tui"
	"github.com/wendylabsinc/wendy/go/internal/shared/config"
)

// preEnrollOptions carries the pre-enrollment flags from cobra into the
// install flow.
type preEnrollOptions struct {
	mode      preEnrollMode
	cloudGRPC string // --cloud-grpc: auth session to enroll with when several exist
}

// skipEnrollmentValue is the picker value for the explicit skip option.
const skipEnrollmentValue = "skip-enrollment"

// mapConfirmCancel converts the TUI cancel sentinel to the package-level one
// so Ctrl+C at a confirm prompt cancels the install cleanly (exit 0) instead
// of surfacing as an error.
func mapConfirmCancel(ok bool, err error) (bool, error) {
	if errors.Is(err, tui.ErrCancelled) {
		return false, ErrUserCancelled
	}
	return ok, err
}

// Interactive hooks used by the pre-enrollment flow, declared as package vars
// so unit tests can stub them (same pattern as promptDeviceName).
var (
	promptEnrollmentSession = func(items []tui.PickerItem) (string, error) {
		return pickFromItems("Select the Wendy Cloud session to use for enrollment", items)
	}
	confirmPreEnroll = func() (bool, error) {
		return mapConfirmCancel(tui.ConfirmDefaultYes("Pre-enroll this device with Wendy Cloud?"))
	}
	confirmContinueUnenrolled = func() (bool, error) {
		return mapConfirmCancel(tui.Confirm("Continue installing without enrollment?"))
	}
	preEnrollDeviceFn = preEnrollDevice
	preEnrollACMEFn   = preEnrollDeviceACME
)

// selectEnrollmentAuth resolves which auth session to use for pre-enrollment.
// With multiple sessions in interactive mode it presents a picker that
// includes an explicit skip option (WDY-1476). Returns (nil, nil) when the
// user chooses to skip enrollment.
func selectEnrollmentAuth(cfg *config.Config, cloudGRPC string, interactive bool) (*config.AuthConfig, error) {
	// Short-circuit for: not-logged-in, --cloud-grpc, single session, and a
	// valid persisted default. A nil picker makes the multi-session-no-default
	// case return ErrMultipleSessions so we can fall back to the skip-capable
	// picker below (WDY-1476).
	auth, err := config.ResolveAuth(cfg, cloudGRPC, nil)
	if err == nil {
		return auth, nil
	}
	if !errors.Is(err, config.ErrMultipleSessions) {
		return nil, err
	}
	if !interactive {
		return nil, err // message mentions --cloud-grpc
	}

	items := make([]tui.PickerItem, 0, len(cfg.Auth)+1)
	for i := range cfg.Auth {
		a := &cfg.Auth[i]
		name := a.CloudDashboard
		if name == "" {
			name = a.CloudGRPC
		}
		desc := a.CloudGRPC
		if len(a.Certificates) > 0 {
			desc = fmt.Sprintf("org %d — %s", a.Certificates[0].OrganizationID, a.CloudGRPC)
		}
		items = append(items, tui.PickerItem{Name: name, Description: desc, Value: strconv.Itoa(i)})
	}
	items = append(items, tui.PickerItem{
		Name:        "Skip enrollment",
		Description: "continue installing without enrolling this device",
		Value:       skipEnrollmentValue,
	})

	picked, err := promptEnrollmentSession(items)
	if err != nil {
		return nil, err
	}
	if picked == skipEnrollmentValue {
		return nil, nil
	}
	idx, convErr := strconv.Atoi(picked)
	if convErr != nil || idx < 0 || idx >= len(cfg.Auth) {
		return nil, fmt.Errorf("invalid session selection %q", picked)
	}
	return authEntryWithCerts(&cfg.Auth[idx])
}

// authEntryWithCerts rejects sessions that cannot enroll because they hold no
// certificate material.
func authEntryWithCerts(a *config.AuthConfig) (*config.AuthConfig, error) {
	if len(a.Certificates) == 0 {
		return nil, fmt.Errorf("auth session %s has no certificates; re-run 'wendy auth login'", a.CloudGRPC)
	}
	return a, nil
}

// resolvePreEnrollment drives the pre-enrollment step of os install. It runs
// before the image download/write, so any abort here costs the user nothing.
// Returns the provisioning JSON for the config partition, or nil when
// enrollment is skipped (user choice, auto mode without a TTY/sessions, or an
// acknowledged failure). The install must not proceed past a failed
// enrollment without explicit user acknowledgement (WDY-1476).
func resolvePreEnrollment(ctx context.Context, cfg *config.Config, opts preEnrollOptions, interactive bool, deviceName string) (*PreProvisionedState, error) {
	switch opts.mode {
	case preEnrollSkip:
		return nil, nil
	case preEnrollAuto:
		if !interactive || len(cfg.Auth) == 0 {
			return nil, nil
		}
		ok, err := confirmPreEnroll()
		if err != nil {
			return nil, err
		}
		if !ok {
			return nil, nil
		}
	}

	auth, err := selectEnrollmentAuth(cfg, opts.cloudGRPC, interactive)
	if err != nil {
		if errors.Is(err, ErrUserCancelled) {
			return nil, err
		}
		if !interactive {
			return nil, fmt.Errorf("--pre-enroll: %w", err)
		}
		fmt.Printf("Cannot pre-enroll: %v\n", err)
		return nil, ackContinueUnenrolled()
	}
	if auth == nil {
		fmt.Println("Skipping enrollment. The device will boot unenrolled; run 'wendy device enroll' after first boot.")
		return nil, nil
	}

	org, orgErr := resolveOrg(ctx, auth, false)
	if orgErr != nil {
		if errors.Is(orgErr, ErrUserCancelled) {
			return nil, orgErr
		}
		if !interactive {
			return nil, fmt.Errorf("--pre-enroll: resolving organization: %w", orgErr)
		}
		fmt.Printf("Cannot resolve organization: %v\n", orgErr)
		return nil, ackContinueUnenrolled()
	}

	fmt.Printf("Pre-enrolling device with Wendy Cloud (org: %s / ID: %d)...\n", org.Name, org.ID)
	state, enrollErr := preEnrollDeviceFn(ctx, auth, deviceName, org.ID, nil)
	if enrollErr == nil {
		fmt.Println("Device pre-enrolled. It will be secure from first boot.")
		return state, nil
	}
	if !interactive {
		return nil, fmt.Errorf("--pre-enroll: pre-enrollment failed: %w", enrollErr)
	}
	fmt.Printf("Pre-enrollment failed: %v\n", enrollErr)
	return nil, ackContinueUnenrolled()
}

// ackContinueUnenrolled pauses for explicit confirmation before continuing an
// install whose enrollment step failed. Returns ErrUserCancelled when the
// user declines, nil when they accept.
func ackContinueUnenrolled() error {
	ok, err := confirmContinueUnenrolled()
	if err != nil {
		return err
	}
	if !ok {
		return ErrUserCancelled
	}
	fmt.Println("Continuing without enrollment. Run 'wendy device enroll' after first boot.")
	return nil
}

// resolveACMEPreEnrollment is the EAB pre-enroll for full-OS install images
// (disk/Orin/Thor). It parallels resolvePreEnrollment's session-selection and
// abort UX but mints an EAB credential via DeviceEnrollmentService.EnrollDevice
// instead of the retired v1 token+certificate path, and drops the org lookup:
// the tenant rides in the operator session's signed identity. The ESP32/litepb
// install keeps resolvePreEnrollment unchanged (it is a different device class
// that does not run the ACME agent). Returns nil to bake nothing.
func resolveACMEPreEnrollment(ctx context.Context, cfg *config.Config, opts preEnrollOptions, interactive bool, deviceName string) (*acmeEnrollmentBake, error) {
	switch opts.mode {
	case preEnrollSkip:
		return nil, nil
	case preEnrollAuto:
		if !interactive || len(cfg.Auth) == 0 {
			return nil, nil
		}
		ok, err := confirmPreEnroll()
		if err != nil {
			return nil, err
		}
		if !ok {
			return nil, nil
		}
	}

	auth, err := selectEnrollmentAuth(cfg, opts.cloudGRPC, interactive)
	if err != nil {
		if errors.Is(err, ErrUserCancelled) {
			return nil, err
		}
		if !interactive {
			return nil, fmt.Errorf("--pre-enroll: %w", err)
		}
		fmt.Printf("Cannot pre-enroll: %v\n", err)
		return nil, ackContinueUnenrolled()
	}
	if auth == nil {
		fmt.Println("Skipping enrollment. The device will boot unenrolled; run 'wendy device enroll' after first boot.")
		return nil, nil
	}

	fmt.Println("Pre-enrolling device with Wendy Cloud...")
	state, enrollErr := preEnrollACMEFn(ctx, auth, deviceName)
	if enrollErr == nil {
		fmt.Println("Cloud enrollment reserved; credential staged for first boot. Automatic ACME enrollment requires an image with a compatible agent; device enrollment and Cloud presence are not yet verified.")
		return state, nil
	}
	if !interactive {
		return nil, fmt.Errorf("--pre-enroll: pre-enrollment failed: %w", enrollErr)
	}
	fmt.Printf("Pre-enrollment failed: %v\n", enrollErr)
	return nil, ackContinueUnenrolled()
}
