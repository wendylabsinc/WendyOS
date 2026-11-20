package commands

import (
	"fmt"
	"io"
	"os"

	"github.com/spf13/cobra"
	"github.com/wendylabsinc/wendy/go/internal/cli/liteclient"
	"github.com/wendylabsinc/wendy/go/internal/cli/liteenroll"
	litepb "github.com/wendylabsinc/wendy/go/proto/gen/litepb"
)

func newDeviceRefreshMeshTrustCmd() *cobra.Command {
	var rootsFile, issuersFile string
	cmd := &cobra.Command{
		Use:         "refresh-mesh-trust",
		Short:       "Update an enrolled Wendy Lite device's mesh trust over USB",
		Args:        cobra.NoArgs,
		Annotations: map[string]string{localEnrollmentAnnotation: "true"},
		RunE: func(cmd *cobra.Command, args []string) error {
			target, err := resolveTarget(cmd.Context(), SuppressProvisioningHint(), SuppressPickerEnroll())
			if err != nil {
				return err
			}
			if target.External == nil || target.External.ProviderKey != "wendy-lite" || target.External.ConnectionType() != "USB" {
				return fmt.Errorf("select an enrolled Wendy Lite device over USB")
			}
			device, err := connectLiteEnrollmentFn(*target.External)
			if err != nil {
				return err
			}
			defer device.Close()
			challenge, err := device.EnrollmentChallenge(true)
			if err != nil {
				return err
			}
			if !challenge.GetEnrolled() || !challenge.GetMeshTrustSupported() {
				return fmt.Errorf("device must be enrolled and running firmware that supports mesh trust updates")
			}
			path := rootsFile
			if path == "" {
				path = issuersFile
			}
			f, err := os.Open(path)
			if err != nil {
				return err
			}
			defer f.Close()
			const maxDER = 32 << 10
			const maxPEM = 2 * maxDER
			raw, err := io.ReadAll(io.LimitReader(f, maxPEM+1))
			if err != nil {
				return err
			}
			if len(raw) > maxPEM {
				return fmt.Errorf("mesh trust file is too large")
			}
			payload, err := liteenroll.MeshTrustUpdate(challenge, raw, rootsFile != "")
			if err != nil {
				return err
			}
			if err := device.PushConf(&litepb.WendyConf{Enrollment: payload}, liteclient.ConfPushModeMeshTrust, nil); err != nil {
				return err
			}
			fmt.Fprintln(cmd.OutOrStdout(), "Wendy Lite mesh trust updated.")
			return nil
		},
	}
	cmd.Flags().StringVar(&rootsFile, "device-roots", "", "Full classical Lite mesh anchor PEM bundle, including the Lite Device CA twin")
	cmd.Flags().StringVar(&issuersFile, "lite-device-intermediates", "", "Parent-signed Lite Device CA chain in PEM, excluding the platform root")
	cmd.MarkFlagsOneRequired("device-roots", "lite-device-intermediates")
	cmd.MarkFlagsMutuallyExclusive("device-roots", "lite-device-intermediates")
	return cmd
}
