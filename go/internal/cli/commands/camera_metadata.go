package commands

import (
	"fmt"
	"io"
	"os"

	"github.com/spf13/cobra"
	pb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
)

func newCameraMetadataCmd() *cobra.Command {
	root := &cobra.Command{Use: "metadata", Short: "Read or publish camera calibration and site poses"}
	var id string
	list := &cobra.Command{Use: "list", Short: "Read camera metadata as protobuf JSON", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		conn, err := connectToAgent(cmd.Context(), DisableSessionBroker(), NonInteractive(), SuppressUpdateCheck())
		if err != nil {
			return err
		}
		defer conn.Close()
		req := &pb.ListCameraMetadataRequest{}
		if id != "" {
			req.StableIds = []string{id}
		}
		result, err := conn.CalibratedFrameService.ListCameraMetadata(cmd.Context(), req)
		if err != nil {
			return err
		}
		return writeCameraMetadataJSON(cmd.OutOrStdout(), result)
	}}
	list.Flags().StringVar(&id, "stable-id", "", "Exact stable camera identity; empty lists all")
	set := &cobra.Command{Use: "set <request.json>", Short: "Apply a SetCameraMetadataRequest with an expected revision", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		file, err := os.Open(args[0])
		if err != nil {
			return err
		}
		defer file.Close()
		data, err := io.ReadAll(io.LimitReader(file, 65537))
		if err != nil {
			return err
		}
		if len(data) > 65536 {
			return fmt.Errorf("metadata request exceeds 64 KiB")
		}
		req := &pb.SetCameraMetadataRequest{}
		if err := protojson.Unmarshal(data, req); err != nil {
			return err
		}
		if req.StableId == "" {
			return fmt.Errorf("stableId is required")
		}
		conn, err := connectToAgent(cmd.Context(), DisableSessionBroker(), NonInteractive(), SuppressUpdateCheck())
		if err != nil {
			return err
		}
		defer conn.Close()
		result, err := conn.CalibratedFrameService.SetCameraMetadata(cmd.Context(), req)
		if err != nil {
			return err
		}
		return writeCameraMetadataJSON(cmd.OutOrStdout(), result)
	}}
	root.AddCommand(list, set)
	return root
}

func writeCameraMetadataJSON(w io.Writer, message proto.Message) error {
	data, err := (protojson.MarshalOptions{Indent: "  "}).Marshal(message)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintln(w, string(data))
	return err
}
