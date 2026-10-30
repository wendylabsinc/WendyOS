package commands

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/spf13/cobra"
	"github.com/wendylabsinc/wendy/go/internal/cli/liteclient"
	litepb "github.com/wendylabsinc/wendy/go/proto/gen/litepb"
)

type liteStatusReader interface {
	GetDeviceIdentity(time.Duration) (*liteclient.DeviceIdentity, error)
	EnrollmentChallenge(bool) (*litepb.WendyComEnrollmentChallenge, error)
}

type liteEnrollmentStatus struct {
	HardwareID string `json:"hardwareId"`
	Name       string `json:"name"`
	Enrolled   bool   `json:"enrolled"`
	DeviceID   string `json:"deviceId,omitempty"`
	TenantID   string `json:"tenantId,omitempty"`
}

func readLiteEnrollmentStatus(reader liteStatusReader) (*liteEnrollmentStatus, error) {
	identity, err := reader.GetDeviceIdentity(5 * time.Second)
	if err != nil {
		return nil, err
	}
	// Status-only must not rotate the nonce or begin/reset enrollment.
	status, err := reader.EnrollmentChallenge(true)
	if err != nil {
		return nil, err
	}
	if identity == nil || status == nil {
		return nil, fmt.Errorf("device returned no enrollment status")
	}
	return &liteEnrollmentStatus{HardwareID: identity.ID, Name: identity.Name,
		Enrolled: status.GetEnrolled(), DeviceID: status.GetDeviceId(), TenantID: status.GetTenantId()}, nil
}

func newDeviceEnrollmentStatusCmd() *cobra.Command {
	return &cobra.Command{
		Use: "enrollment-status", Short: "Read a Wendy Lite board's enrollment identity over USB without changing it", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx, cancel := context.WithTimeout(cmd.Context(), 20*time.Second)
			defer cancel()
			target, err := resolveTarget(ctx, NonInteractive(), SuppressProvisioningHint(), SuppressPickerEnroll())
			if err != nil {
				return err
			}
			defer target.Close()
			if target.External == nil || target.External.ProviderKey != "wendy-lite" || target.External.ConnectionType() != "USB" {
				return fmt.Errorf("select a Wendy Lite USB device with --device")
			}
			client, err := connectLiteEnrollmentFn(*target.External)
			if err != nil {
				return err
			}
			defer client.Close()
			status, err := readLiteEnrollmentStatus(client)
			if err != nil {
				return err
			}
			return json.NewEncoder(cmd.OutOrStdout()).Encode(status)
		},
	}
}
