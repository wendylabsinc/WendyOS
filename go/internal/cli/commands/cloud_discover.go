package commands

import (
	"fmt"
	"io"

	"github.com/spf13/cobra"
	cloudpb "github.com/wendylabsinc/wendy/proto/gen/cloudpb"
)

func newCloudDiscoverCmd() *cobra.Command {
	var cloudGRPC string

	cmd := &cobra.Command{
		Use:   "discover",
		Short: "List enrolled devices in Wendy Cloud",
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()

			auth, err := pickAuthEntry(cloudGRPC)
			if err != nil {
				return err
			}
			if len(auth.Certificates) == 0 {
				return fmt.Errorf("auth entry has no certificates; re-run 'wendy auth login'")
			}
			cert := auth.Certificates[0]

			conn, err := dialCloudGRPC(auth)
			if err != nil {
				return err
			}
			defer conn.Close()

			assetClient := cloudpb.NewAssetServiceClient(conn)
			stream, err := assetClient.ListAssets(cloudContext(ctx, auth), &cloudpb.ListAssetsRequest{
				OrganizationId:  int32(cert.OrganizationID),
				IsComputeDevice: boolPtr(true),
				OnlineOnly:      boolPtr(true),
			})
			if err != nil {
				return fmt.Errorf("listing devices: %w", err)
			}

			var assets []*cloudpb.Asset
			for {
				msg, err := stream.Recv()
				if err == io.EOF {
					break
				}
				if err != nil {
					return fmt.Errorf("listing devices: %w", err)
				}
				if a := msg.GetAsset(); a != nil {
					assets = append(assets, a)
				}
			}

			if len(assets) == 0 {
				fmt.Fprintln(cmd.OutOrStdout(), "No enrolled devices found.")
				return nil
			}

			fmt.Fprintf(cmd.OutOrStdout(), "%-8s  %s\n", "ID", "Name")
			fmt.Fprintf(cmd.OutOrStdout(), "%-8s  %s\n", "--------", "----")
			for _, a := range assets {
				fmt.Fprintf(cmd.OutOrStdout(), "%-8d  %s\n", a.GetId(), a.GetName())
			}
			return nil
		},
	}

	cmd.Flags().StringVar(&cloudGRPC, "cloud-grpc", "", "Cloud gRPC endpoint (required when multiple auth sessions exist)")
	return cmd
}
