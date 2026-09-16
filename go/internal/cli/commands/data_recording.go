package commands

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/spf13/cobra"
	"github.com/wendylabsinc/wendy/go/internal/agent/data"
	agentpbv2 "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func newDataExportStreamCmd() *cobra.Command {
	var service, output string
	var reclaim, follow bool
	var interval time.Duration
	c := &cobra.Command{Use: "export-stream <app-id> <stream>", Short: "Export durable stream records, optionally capturing continuously", Args: cobra.ExactArgs(2), RunE: func(cmd *cobra.Command, args []string) error {
		if output == "" {
			return errors.New("--output is required")
		}
		if follow && !reclaim {
			return errors.New("--follow requires --reclaim to drain the device journal")
		}
		if interval < 100*time.Millisecond {
			return errors.New("--interval must be at least 100ms")
		}
		if follow {
			if err := os.Mkdir(output, 0o750); err != nil && !errors.Is(err, os.ErrExist) {
				return err
			}
			if info, err := os.Stat(output); err != nil {
				return err
			} else if !info.IsDir() {
				return errors.New("--follow output must be a directory")
			}
			// Persist the destination directory before any remote reclamation.
			if err := syncRecordingDirectory(filepath.Dir(filepath.Clean(output))); err != nil {
				return err
			}
		}
		return withDataClient(cmd.Context(), func(client agentpbv2.DataServiceClient) error {
			for {
				destination := output
				if follow {
					destination = filepath.Join(output, time.Now().UTC().Format("20060102T150405.000000000")+".wdr")
				}
				req := &agentpbv2.DataRecordingExportRequest{AppId: args[0], Service: service, Stream: args[1], Checkpoint: reclaim}
				count, err := exportRecordingFile(cmd.Context(), client, req, destination)
				if err != nil {
					if !follow || cmd.Context().Err() != nil || !retryRecordingExport(err) {
						return err
					}
					fmt.Fprintf(cmd.ErrOrStderr(), "Export interrupted; retained data will be retried: %v\n", err)
				} else if follow && count == 0 {
					if err := os.Remove(destination); err != nil {
						return err
					}
				} else {
					fmt.Fprintf(cmd.ErrOrStderr(), "Exported %d records to %s\n", count, destination)
				}
				if !follow {
					return nil
				}
				timer := time.NewTimer(interval)
				select {
				case <-cmd.Context().Done():
					timer.Stop()
					return cmd.Context().Err()
				case <-timer.C:
				}
			}
		})
	}}
	c.Flags().StringVar(&service, "service", "", "Application service name")
	c.Flags().StringVarP(&output, "output", "o", "", "New output file, or destination directory with --follow")
	c.Flags().BoolVar(&reclaim, "reclaim", false, "Export an oldest journal chunk, sync it locally, then reclaim it on the device")
	c.Flags().BoolVar(&follow, "follow", false, "Continuously export chunks into --output directory; requires --reclaim")
	c.Flags().DurationVar(&interval, "interval", time.Second, "Delay between continuous export chunks")
	return c
}

func syncRecordingDirectory(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	return errors.Join(f.Sync(), f.Close())
}

// No remote acknowledgement is sent until both file contents and its directory
// entry are durable. On a failed acknowledgement keep the local file: the device
// may already have reclaimed it. A retry may export duplicates, never less data.
func exportRecordingFile(ctx context.Context, client agentpbv2.DataServiceClient, req *agentpbv2.DataRecordingExportRequest, output string) (int, error) {
	f, err := os.OpenFile(output, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return 0, err
	}
	durable := false
	defer func() {
		f.Close()
		if !durable {
			os.Remove(output)
		}
	}()
	stream, err := client.ExportRecording(ctx, req)
	if err != nil {
		return 0, err
	}
	count := 0
	for {
		r, err := stream.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			return count, err
		}
		if err = data.WriteRecording(f, r); err != nil {
			return count, err
		}
		count++
	}
	if err = f.Sync(); err != nil {
		return count, err
	}
	if err = f.Close(); err != nil {
		return count, err
	}
	if err = syncRecordingDirectory(filepath.Dir(output)); err != nil {
		return count, err
	}
	durable = true
	if req.Checkpoint && count > 0 {
		tokens := stream.Trailer().Get("wendy-recording-checkpoint")
		if len(tokens) != 1 || tokens[0] == "" {
			return count, errors.New("export saved, but server omitted reclamation checkpoint")
		}
		_, err = client.AcknowledgeRecordingExport(ctx, &agentpbv2.DataRecordingExportAckRequest{AppId: req.AppId, Service: req.Service, Stream: req.Stream, Checkpoint: tokens[0]})
		if err != nil {
			return count, fmt.Errorf("export saved to %s, but reclamation acknowledgement failed: %w", output, err)
		}
	}
	return count, nil
}

func retryRecordingExport(err error) bool {
	switch status.Code(err) {
	case codes.Unavailable, codes.DeadlineExceeded, codes.ResourceExhausted, codes.NotFound:
		return true
	default:
		return false
	}
}
