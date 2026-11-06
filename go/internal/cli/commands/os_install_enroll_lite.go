//go:build darwin || linux || windows

package commands

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/spf13/cobra"
	"github.com/wendylabsinc/wendy/go/internal/cli/liteclient"
	"github.com/wendylabsinc/wendy/go/internal/cli/liteenroll"
	"github.com/wendylabsinc/wendy/go/internal/cli/tui"
	"github.com/wendylabsinc/wendy/go/internal/shared/config"
	"github.com/wendylabsinc/wendy/go/internal/shared/discovery"
	"github.com/wendylabsinc/wendy/go/internal/shared/models"
)

// Lite must boot before it can supply the nonce and trust capabilities needed
// for Tier C enrollment. Select and validate the session before flashing, but
// leave credential issuance to the live USB enrollment flow after reboot.
func resolveLiteInstallEnrollment(cfg *config.Config, opts preEnrollOptions, interactive bool) (*config.AuthConfig, error) {
	switch opts.mode {
	case preEnrollSkip:
		return nil, nil
	case preEnrollAuto:
		if !interactive || len(cfg.Auth) == 0 {
			return nil, nil
		}
		ok, err := confirmPreEnroll()
		if err != nil || !ok {
			return nil, err
		}
	}
	auth, err := selectEnrollmentAuth(cfg, opts.cloudGRPC, interactive)
	if err == nil && auth != nil {
		// Validate derived endpoints and the tenant operator identity without
		// any Cloud calls or minting a credential before the board is ready.
		_, err = liteenroll.Config(auth, uuid.NewString(), "", "", "", 5055)
	}
	if err != nil {
		if errors.Is(err, ErrUserCancelled) {
			return nil, err
		}
		if !interactive {
			return nil, fmt.Errorf("--pre-enroll: %w", err)
		}
		fmt.Printf("Cannot enroll Wendy Lite: %v\n", err)
		return nil, ackContinueUnenrolled()
	}
	if auth != nil {
		fmt.Println("Wendy Lite will enroll with Wendy Cloud over native USB after flashing and rebooting.")
	}
	return auth, nil
}

var (
	installedLiteBootTimeout = 30 * time.Second
	waitInstalledLiteFn      = waitInstalledLite
	installedLitePortsFn     = discovery.ResolveESP32SerialPorts
	probeInstalledLiteFn     = func(ctx context.Context, port string) error {
		client := liteclient.NewWendyLiteClient()
		defer client.Close()
		return client.ConnectToSerialContext(ctx, port)
	}
)

func enrollInstalledLite(ctx context.Context, port discovery.SerialPortInfo, auth *config.AuthConfig, name, version string, interactive bool) error {
	fmt.Println("Waiting for Wendy Lite to start on USB...")
	ready, err := waitInstalledLiteFn(ctx, port, interactive)
	if err == nil {
		cmd := &cobra.Command{}
		cmd.SetContext(ctx)
		target := models.ExternalDevice{
			ID: "wendy-lite:" + ready.Port, ProviderKey: "wendy-lite", OSVersion: version,
			ConnectionInfo: map[string]string{"type": "USB", "serialPort": ready.Port},
		}
		// Do not retry this operation: it can reserve an asset and consume a
		// single-use credential. runEnrollLiteDevice verifies the certificate.
		err = runLiteEnrollmentFn(cmd, target, auth, name, liteEnrollmentOptions{brokerPort: 5055})
	}
	if err != nil {
		return fmt.Errorf("firmware was flashed, but Cloud enrollment is not confirmed: %w; use 'wendy device enroll --cloud-grpc %s' over native USB to finish setup without reflashing", err, auth.CloudGRPC)
	}
	return nil
}

func waitInstalledLite(ctx context.Context, port discovery.SerialPortInfo, interactive bool) (discovery.SerialPortInfo, error) {
	if port.Transport != discovery.SerialTransportNativeUSB {
		return discovery.SerialPortInfo{}, fmt.Errorf("Wendy Lite enrollment requires native USB")
	}
	if err := waitInstalledLitePort(ctx, port.Port); err == nil {
		return port, nil
	} else if ctx.Err() != nil {
		return discovery.SerialPortInfo{}, ctx.Err()
	} else {
		// Native USB can re-enumerate under a new path. Never automatically
		// send credentials to another port, even if only one is visible.
		ports, scanErr := installedLitePortsFn()
		if scanErr != nil || len(ports) == 0 {
			return discovery.SerialPortInfo{}, err
		}
		for _, candidate := range ports {
			if candidate.Port == port.Port {
				return discovery.SerialPortInfo{}, err
			}
		}
		if !interactive {
			return discovery.SerialPortInfo{}, fmt.Errorf("USB port %s changed after reboot; select the board with 'wendy device enroll'", port.Port)
		}
		selected, pickErr := selectESP32InstallDevice(ports, port, true, func(_ string, items []tui.PickerItem) (string, error) {
			return pickFromItems("Select the newly flashed Wendy Lite device to enroll", items)
		})
		if pickErr != nil {
			return discovery.SerialPortInfo{}, pickErr
		}
		if err := waitInstalledLitePort(ctx, selected.Port); err != nil {
			return discovery.SerialPortInfo{}, err
		}
		return selected, nil
	}
}

func waitInstalledLitePort(ctx context.Context, port string) error {
	ctx, cancel := context.WithTimeout(ctx, installedLiteBootTimeout)
	defer cancel()
	var last error
	for {
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("waiting for Wendy Lite on %s: %w (last connection error: %v)", port, err, last)
		}
		probeCtx, probeCancel := context.WithTimeout(ctx, 3*time.Second)
		last = probeInstalledLiteFn(probeCtx, port)
		probeCancel()
		if last == nil {
			return nil
		}
		select {
		case <-ctx.Done():
		case <-time.After(time.Second):
		}
	}
}
