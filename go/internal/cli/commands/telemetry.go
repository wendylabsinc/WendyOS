package commands

import (
	"encoding/json"
	"fmt"
	"io"

	"github.com/spf13/cobra"
	"github.com/wendylabsinc/wendy/proto/gen/agentpb"
)

func newTelemetryCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "telemetry",
		Short: "Stream telemetry data from the target device",
	}

	cmd.AddCommand(
		newTelemetryLogsCmd(),
		newTelemetryStreamCmd(),
	)

	return cmd
}

func newTelemetryLogsCmd() *cobra.Command {
	var appName string
	var serviceName string
	var minSeverity int32

	cmd := &cobra.Command{
		Use:   "logs",
		Short: "Stream logs from containers on the device",
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			conn, err := connectToAgent(ctx)
			if err != nil {
				return err
			}
			defer conn.Close()

			req := &agentpb.StreamLogsRequest{}
			if appName != "" {
				req.AppName = &appName
			}
			if serviceName != "" {
				req.ServiceName = &serviceName
			}
			if minSeverity > 0 {
				req.MinSeverity = &minSeverity
			}
			stream, err := conn.TelemetryService.StreamLogs(ctx, req)
			if err != nil {
				return fmt.Errorf("starting log stream: %w", err)
			}

			for {
				resp, err := stream.Recv()
				if err == io.EOF {
					break
				}
				if err != nil {
					return fmt.Errorf("receiving logs: %w", err)
				}

				logs := resp.GetLogs()
				if logs == nil {
					continue
				}

				// Print log records from OTLP format.
				for _, rl := range logs.GetResourceLogs() {
					for _, sl := range rl.GetScopeLogs() {
						for _, lr := range sl.GetLogRecords() {
							body := lr.GetBody()
							if body != nil {
								fmt.Println(body.GetStringValue())
							}
						}
					}
				}
			}

			return nil
		},
	}

	cmd.Flags().StringVar(&appName, "app", "", "Filter by application name")
	cmd.Flags().StringVar(&serviceName, "service", "", "Filter by service name")
	cmd.Flags().Int32Var(&minSeverity, "min-severity", 0, "Minimum log severity level")

	return cmd
}

func newTelemetryStreamCmd() *cobra.Command {
	var appName string
	var serviceName string

	cmd := &cobra.Command{
		Use:   "stream",
		Short: "Stream telemetry data as JSONL",
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			conn, err := connectToAgent(ctx)
			if err != nil {
				return err
			}
			defer conn.Close()

			streamReq := &agentpb.StreamLogsRequest{}
			if appName != "" {
				streamReq.AppName = &appName
			}
			if serviceName != "" {
				streamReq.ServiceName = &serviceName
			}
			stream, err := conn.TelemetryService.StreamLogs(ctx, streamReq)
			if err != nil {
				return fmt.Errorf("starting telemetry stream: %w", err)
			}

			for {
				resp, err := stream.Recv()
				if err == io.EOF {
					break
				}
				if err != nil {
					return fmt.Errorf("receiving telemetry: %w", err)
				}

				data, err := json.Marshal(resp)
				if err != nil {
					continue
				}
				fmt.Println(string(data))
			}

			return nil
		},
	}

	cmd.Flags().StringVar(&appName, "app", "", "Filter by application name")
	cmd.Flags().StringVar(&serviceName, "service", "", "Filter by service name")

	return cmd
}
