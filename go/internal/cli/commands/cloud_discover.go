package commands

import (
	"fmt"
	"io"

	"github.com/spf13/cobra"
	cloudpb "github.com/wendylabsinc/wendy/proto/gen/cloudpb"
)

func newCloudDiscoverCmd() *cobra.Command {
	var cloudGRPC string
	var all bool

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
			req := &cloudpb.ListAssetsRequest{
				OrganizationId:  int32(cert.OrganizationID),
				IsComputeDevice: boolPtr(true),
			}
			if !all {
				req.OnlineOnly = boolPtr(true)
			}
			stream, err := assetClient.ListAssets(cloudContext(ctx, auth), req)
			if err != nil {
				return fmt.Errorf("listing devices: %w", err)
			}
			var assets []*cloudpb.Asset
			for {
				resp, err := stream.Recv()
				if err == io.EOF {
					break
				}
				if err != nil {
					return fmt.Errorf("listing devices: %w", err)
				}
				assets = append(assets, resp.GetAsset())
			}

			if len(assets) == 0 {
				if all {
					fmt.Fprintln(cmd.OutOrStdout(), "No enrolled devices found.")
				} else {
					fmt.Fprintln(cmd.OutOrStdout(), "No online devices found. Use --all to include offline devices.")
				}
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
	cmd.Flags().BoolVar(&all, "all", false, "Include offline devices")
	return cmd
}
