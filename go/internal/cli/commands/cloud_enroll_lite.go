package commands

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"github.com/spf13/cobra"
	"github.com/wendylabsinc/wendy/go/internal/cli/liteclient"
	"github.com/wendylabsinc/wendy/go/internal/cli/liteenroll"
	cloudpb "github.com/wendylabsinc/wendy/go/proto/gen/cloudpb/v2"
	litepb "github.com/wendylabsinc/wendy/go/proto/gen/litepb"
	"google.golang.org/protobuf/proto"
)

func newCloudEnrollLiteCmd() *cobra.Command {
	var serialPort, name, cloudGRPC, csrURL, timeURL, brokerHost string
	var brokerPort uint32
	cmd := &cobra.Command{
		Use: "enroll-lite", Short: "Enroll a USB-connected Wendy Lite with the selected PKI",
		Long: "Enrolls Wendy Lite using a single-use Tier C credential authorized by your PKI operator session. The board must have PKI-enabled firmware with pinned trust roots and working Wi-Fi. Its private key is generated on the board. Enrollment reboots the board.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := validateHostnameArg(name); err != nil {
				return err
			}
			ctx := cmd.Context()
			auth, err := resolveEnrollmentAuthEntry(cloudGRPC, 0)
			if err != nil {
				return err
			}
			auth, err = prepareEnrollmentAuth(ctx, auth)
			if err != nil {
				return err
			}
			device := liteclient.NewWendyLiteClient()
			if err = device.ConnectToSerial(serialPort); err != nil {
				return err
			}
			defer device.Close()
			identity, err := device.GetDeviceIdentity(5 * time.Second)
			if err != nil {
				return err
			}
			cfg, err := liteenroll.Config(auth, "lite-"+identity.ID, csrURL, timeURL, brokerHost, brokerPort)
			if err != nil {
				return err
			}
			challenge, err := device.EnrollmentChallenge(false)
			if err != nil {
				return err
			}
			if challenge.Enrolled {
				return fmt.Errorf("this board already has an issued identity; use its existing enrollment or operator recovery")
			}
			cfg.SignedTime, err = liteenroll.SignedTime(ctx, http.DefaultClient, cfg.TimeUrl, challenge.NonceHex)
			if err != nil {
				return err
			}
			cloud, err := dialCloudGRPC(auth)
			if err != nil {
				return err
			}
			defer cloud.Close()
			cloudCtx, err := cloudContext(ctx, auth)
			if err != nil {
				return err
			}
			mintCtx, cancel := context.WithTimeout(cloudCtx, 30*time.Second)
			asset, err := liteenroll.Mint(mintCtx, cloudpb.NewDeviceEnrollmentServiceClient(cloud), auth, cfg, name)
			cancel()
			if err != nil {
				return err
			}
			defer func() { cfg.Token = "" }()
			fmt.Fprintf(cmd.OutOrStdout(), "Cloud reserved asset %s for %s. Delivering its enrollment credential over USB.\n", asset, cfg.DeviceId)
			if err = device.PushConf(&litepb.WendyConf{DeviceName: proto.String(name), Enrollment: cfg}, liteclient.ConfPushModeUpdate, nil); err != nil {
				return fmt.Errorf("asset %s was reserved but USB setup failed: %w", asset, err)
			}
			if err = device.ResetTargetDevice(true, 0); err != nil {
				return fmt.Errorf("configuration stored; reboot the board to enroll: %w", err)
			}
			device.Close()
			fmt.Fprintln(cmd.OutOrStdout(), "Waiting for the board to obtain and verify its certificate from pki-core...")
			if err = waitLiteEnrollment(ctx, serialPort); err != nil {
				return fmt.Errorf("asset %s is reserved, but enrollment is not confirmed: %w", asset, err)
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Enrolled %s as asset %s. Broker presence is reported after the board establishes mTLS; check 'wendy cloud discover'.\n", name, asset)
			return nil
		},
	}
	cmd.Flags().StringVar(&serialPort, "serial", "", "Physical USB serial port")
	cmd.Flags().StringVar(&name, "name", "", "Cloud device name")
	cmd.Flags().StringVar(&cloudGRPC, "cloud-grpc", "", "Cloud endpoint used to select the operator session")
	cmd.Flags().StringVar(&brokerHost, "broker-host", "", "WendyCom broker TLS hostname")
	cmd.Flags().Uint32Var(&brokerPort, "broker-port", 5055, "WendyCom broker TLS port")
	cmd.Flags().StringVar(&csrURL, "csr-url", "", "CSR enrollment base URL override for self-hosted PKI")
	cmd.Flags().StringVar(&timeURL, "time-url", "", "Signed-time endpoint override for self-hosted PKI")
	_ = cmd.MarkFlagRequired("serial")
	_ = cmd.MarkFlagRequired("name")
	_ = cmd.MarkFlagRequired("broker-host")
	return cmd
}

func waitLiteEnrollment(ctx context.Context, serialPort string) error {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	ticker := time.NewTicker(3 * time.Second)
	defer ticker.Stop()
	var last error
	for {
		select {
		case <-ctx.Done():
			if last != nil {
				return fmt.Errorf("%w; last USB error: %v", ctx.Err(), last)
			}
			return fmt.Errorf("%w; check the board's Wi-Fi and PKI logs", ctx.Err())
		case <-ticker.C:
			client := liteclient.NewWendyLiteClient()
			if err := client.ConnectToSerial(serialPort); err != nil {
				last = err
				continue
			}
			state, err := client.EnrollmentChallenge(true)
			client.Close()
			if err != nil {
				last = err
				continue
			}
			last = nil
			if state.Enrolled {
				return nil
			}
		}
	}
}
