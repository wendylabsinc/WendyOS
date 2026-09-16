package commands

import (
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/spf13/cobra"
	"github.com/wendylabsinc/wendy/go/internal/agent/data"
	agentpbv2 "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
)

func newDataExportStreamCmd() *cobra.Command {
	var service, output string
	c := &cobra.Command{Use: "export-stream <app-id> <stream>", Short: "Export retained durable stream records to a binary .wdr file", Args: cobra.ExactArgs(2), RunE: func(cmd *cobra.Command, args []string) error {
		if output == "" {
			return errors.New("--output is required")
		}
		return withDataClient(cmd.Context(), func(client agentpbv2.DataServiceClient) error {
			stream, err := client.ExportRecording(cmd.Context(), &agentpbv2.DataRecordingExportRequest{AppId: args[0], Service: service, Stream: args[1]})
			if err != nil {
				return err
			}
			f, err := os.OpenFile(output, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
			if err != nil {
				return err
			}
			complete := false
			defer func() {
				f.Close()
				if !complete {
					os.Remove(output)
				}
			}()
			count := 0
			for {
				r, err := stream.Recv()
				if err == io.EOF {
					break
				}
				if err != nil {
					return err
				}
				if err = data.WriteRecording(f, r); err != nil {
					return err
				}
				count++
			}
			if err = f.Sync(); err != nil {
				return err
			}
			if err = f.Close(); err != nil {
				return err
			}
			complete = true
			fmt.Fprintf(cmd.ErrOrStderr(), "Exported %d records to %s\n", count, output)
			return nil
		})
	}}
	c.Flags().StringVar(&service, "service", "", "Application service name")
	c.Flags().StringVarP(&output, "output", "o", "", "New output file; existing files are never overwritten")
	return c
}
