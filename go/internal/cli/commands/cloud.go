package commands

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"github.com/spf13/cobra"
	"github.com/wendylabsinc/wendy/internal/cli/tui"
	cloudpb "github.com/wendylabsinc/wendy/proto/gen/cloudpb"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

func newCloudCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "cloud",
		Short: "Interact with Wendy Cloud",
	}
	cmd.AddCommand(newCloudDiscoverCmd())
	cmd.AddCommand(newCloudEnrollCmd())
	return cmd
}

func newCloudDiscoverCmd() *cobra.Command {
	var orgID int32
	var cloudGRPC string

	cmd := &cobra.Command{
		Use:   "discover",
		Short: "List all devices enrolled in your organisation",
		RunE: func(cmd *cobra.Command, args []string) error {
			return runCloudDiscover(cmd.Context(), cloudGRPC, orgID)
		},
	}

	cmd.Flags().Int32Var(&orgID, "org", 0, "Organisation ID to list devices for (default: all)")
	cmd.Flags().StringVar(&cloudGRPC, "cloud-grpc", "localhost:50051", "Cloud gRPC endpoint")

	return cmd
}

func runCloudDiscover(ctx context.Context, cloudGRPC string, orgID int32) error {
	conn, err := grpc.NewClient(cloudGRPC, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return fmt.Errorf("connect to cloud: %w", err)
	}
	defer conn.Close()

	// The cloud server streams assets one per message. Use the raw stream API
	// to collect them all before rendering.
	stream, err := conn.NewStream(ctx, &grpc.StreamDesc{
		StreamName:    "ListAssets",
		ServerStreams: true,
	}, "/wendycloud.v1.AssetService/ListAssets")
	if err != nil {
		return fmt.Errorf("open stream: %w", err)
	}

	if err := stream.SendMsg(&cloudpb.ListAssetsRequest{OrganizationId: orgID}); err != nil {
		return fmt.Errorf("send request: %w", err)
	}
	if err := stream.CloseSend(); err != nil {
		return fmt.Errorf("close send: %w", err)
	}

	var assets []*cloudpb.Asset
	for {
		var msg cloudpb.ListAssetsResponse
		if err := stream.RecvMsg(&msg); err != nil {
			if err == io.EOF {
				break
			}
			return fmt.Errorf("receive: %w", err)
		}
		assets = append(assets, msg.GetAssets()...)
	}

	if len(assets) == 0 {
		fmt.Println("No devices found.")
		return nil
	}

	if jsonOutput {
		data, err := json.MarshalIndent(assets, "", "  ")
		if err != nil {
			return fmt.Errorf("marshal response: %w", err)
		}
		fmt.Println(string(data))
		return nil
	}

	headers := []string{"Name", "Type", "OS", "IP Address", "Tags"}
	rows := make([][]string, 0, len(assets))
	for _, a := range assets {
		rows = append(rows, []string{
			a.GetName(),
			a.GetDeviceType(),
			a.GetOsType(),
			a.GetIpAddress(),
			strings.Join(a.GetTags(), ", "),
		})
	}

	fmt.Print(tui.RenderTable(headers, rows))
	return nil
}

func newCloudEnrollCmd() *cobra.Command {
	var orgID int32
	var cloudGRPC string
	var name string
	var deviceType string
	var ipAddress string
	var osType string
	var osVersion string
	var architecture string

	cmd := &cobra.Command{
		Use:   "enroll",
		Short: "Register this device in Wendy Cloud without going through the cert enrollment flow",
		Long: `POC only: directly creates an asset record in the cloud database.
Use this to register a device during development when the full OAuth / cert
enrollment flow is not available. Remove before production.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runCloudEnroll(cmd.Context(), cloudGRPC, orgID, name, deviceType, ipAddress, osType, osVersion, architecture)
		},
	}

	cmd.Flags().Int32Var(&orgID, "org", 1, "Organisation ID to register the device under")
	cmd.Flags().StringVar(&cloudGRPC, "cloud-grpc", "localhost:50051", "Cloud gRPC endpoint")
	cmd.Flags().StringVar(&name, "name", "", "Human-readable device name (required)")
	cmd.Flags().StringVar(&deviceType, "device-type", "", "Device type, e.g. jetson-orin-nano")
	cmd.Flags().StringVar(&ipAddress, "ip", "", "Device IP address")
	cmd.Flags().StringVar(&osType, "os", "WendyOS", "Operating system type")
	cmd.Flags().StringVar(&osVersion, "os-version", "", "Operating system version")
	cmd.Flags().StringVar(&architecture, "arch", "arm64", "CPU architecture")

	_ = cmd.MarkFlagRequired("name")

	return cmd
}

func runCloudEnroll(
	ctx context.Context,
	cloudGRPC string,
	orgID int32,
	name, deviceType, ipAddress, osType, osVersion, architecture string,
) error {
	conn, err := grpc.NewClient(cloudGRPC, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return fmt.Errorf("connect to cloud: %w", err)
	}
	defer conn.Close()

	client := cloudpb.NewAssetServiceClient(conn)

	req := &cloudpb.CreateAssetRequest{
		OrganizationId:  orgID,
		Name:            name,
		IsComputeDevice: true,
		AssetType:       "device",
		Tags:            []string{"poc", "direct-enrolled"},
	}
	if osType != "" {
		req.OsType = &osType
	}
	if osVersion != "" {
		req.OsVersion = &osVersion
	}
	if architecture != "" {
		req.Architecture = &architecture
	}
	if deviceType != "" {
		req.DeviceType = &deviceType
	}
	if ipAddress != "" {
		req.IpAddress = &ipAddress
	}

	asset, err := client.CreateAsset(ctx, req)
	if err != nil {
		return fmt.Errorf("enroll device: %w", err)
	}

	fmt.Printf("Device enrolled successfully.\n")
	fmt.Printf("  ID:   %d\n", asset.GetId())
	fmt.Printf("  Name: %s\n", asset.GetName())
	fmt.Printf("  Org:  %d\n", asset.GetOrganizationId())
	if asset.GetIpAddress() != "" {
		fmt.Printf("  IP:   %s\n", asset.GetIpAddress())
	}
	return nil
}
