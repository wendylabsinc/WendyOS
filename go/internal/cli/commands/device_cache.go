package commands

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"time"

	"github.com/spf13/cobra"
	"github.com/wendylabsinc/wendy/go/internal/cli/grpcclient"
	agentpbv2 "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

type deviceCachePruneClient interface {
	PruneCache(context.Context, *agentpbv2.PruneCacheRequest, ...grpc.CallOption) (*agentpbv2.PruneCacheResponse, error)
}

func newDeviceCacheCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "cache",
		Short: "Manage cached container data on the target device",
	}
	cmd.AddCommand(newDeviceCachePruneCmd())
	return cmd
}

func newDeviceCachePruneCmd() *cobra.Command {
	var dryRun, all bool
	var minAgeFlag string
	cmd := &cobra.Command{
		Use:   "prune",
		Short: "Release unused container layer and snapshot caches",
		Long: "Releases Wendy's cache pins on container layer blobs and unpacked snapshots. With " +
			"--all, releases every Wendy cache pin immediately (layer blobs and unpacked " +
			"snapshots) and forces a synchronous containerd garbage-collection pass. Apps with a " +
			"container on the device, running or stopped, keep their layers. Images whose " +
			"container was deleted lose their unpacked snapshots and are re-unpacked on the next " +
			"'wendy run' from the still-cached layer blobs. Do not run --all while a deploy to " +
			"this device is in progress. --all cannot help when container storage is on the OS " +
			"root slot (WDY-3127); power-cycle the device instead.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			minAge, err := parsePruneMinAge(all, minAgeFlag)
			if err != nil {
				return err
			}
			conn, err := connectToAgent(cmd.Context(), SuppressUpdateCheck())
			if err != nil {
				return err
			}
			defer conn.Close()
			return runDeviceCachePrune(cmd.Context(), conn, cmd.OutOrStdout(), devicePruneOptions{
				dryRun:  dryRun,
				minAge:  minAge,
				jsonOut: jsonOutput,
			})
		},
	}
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "Report eligible cache data without releasing it")
	cmd.Flags().BoolVar(&all, "all", false, "Release every Wendy cache pin regardless of age")
	cmd.Flags().StringVar(&minAgeFlag, "min-age", "", "Release cache pins older than this duration (e.g. 1h)")
	cmd.MarkFlagsMutuallyExclusive("all", "min-age")
	return cmd
}

// devicePruneOptions drives runDeviceCachePruneRPC. minAge is nil when
// neither --all nor --min-age was given (the agent applies its own
// default); the zero duration means --all (release everything).
type devicePruneOptions struct {
	dryRun  bool
	minAge  *time.Duration
	jsonOut bool
}

// parsePruneMinAge resolves --all/--min-age into the MinAgeSeconds value to
// send: nil when neither flag was given (agent default), the zero duration
// for --all, or the parsed duration for --min-age. Negative durations are
// rejected here so the CLI never issues the RPC with a nonsensical value.
func parsePruneMinAge(all bool, minAge string) (*time.Duration, error) {
	if all {
		zero := time.Duration(0)
		return &zero, nil
	}
	if minAge == "" {
		return nil, nil
	}
	d, err := time.ParseDuration(minAge)
	if err != nil {
		return nil, fmt.Errorf("invalid --min-age %q: %w", minAge, err)
	}
	if d < 0 {
		return nil, fmt.Errorf("--min-age must not be negative: %q", minAge)
	}
	return &d, nil
}

func runDeviceCachePrune(ctx context.Context, conn *grpcclient.AgentConnection, out io.Writer, opts devicePruneOptions) error {
	client := agentpbv2.NewWendyContainerServiceClient(conn.Conn)
	return runDeviceCachePruneRPC(ctx, client, out, opts)
}

func runDeviceCachePruneRPC(ctx context.Context, client deviceCachePruneClient, out io.Writer, opts devicePruneOptions) error {
	req := &agentpbv2.PruneCacheRequest{DryRun: opts.dryRun}
	var requestedSeconds *uint64
	if opts.minAge != nil {
		secs := uint64(opts.minAge.Seconds())
		req.MinAgeSeconds = proto.Uint64(secs)
		requestedSeconds = &secs
	}

	resp, err := client.PruneCache(ctx, req)
	if err != nil {
		if status.Code(err) == codes.Unimplemented {
			return fmt.Errorf("this device agent does not support cache pruning; update it with 'wendy device update'")
		}
		return fmt.Errorf("pruning device cache: %w", err)
	}

	if opts.jsonOut {
		data, err := json.MarshalIndent(map[string]any{
			"dryRun":            opts.dryRun,
			"contentBlobs":      resp.GetContentBlobs(),
			"contentBytes":      resp.GetContentBytes(),
			"snapshots":         resp.GetSnapshots(),
			"snapshotBytes":     resp.GetSnapshotBytes(),
			"minimumAgeSeconds": resp.GetMinimumAgeSeconds(),
			"minAgeSeconds":     requestedSeconds,
			"reclaimedBytes":    resp.ReclaimedBytes,
		}, "", "  ")
		if err != nil {
			return err
		}
		_, err = fmt.Fprintln(out, string(data))
		return err
	}

	effectiveAge := resp.GetMinimumAgeSeconds()
	if requestedSeconds != nil && effectiveAge != *requestedSeconds {
		if _, err = fmt.Fprintf(out, "Warning: this device agent ignored --min-age/--all and pruned with its default of %s; update it with 'wendy device update'.\n", formatCacheAge(effectiveAge)); err != nil {
			return err
		}
	}

	totalObjects := resp.GetContentBlobs() + resp.GetSnapshots()
	totalBytes := saturatingAdd(resp.GetContentBytes(), resp.GetSnapshotBytes())
	if totalObjects == 0 {
		if effectiveAge == 0 {
			_, err = fmt.Fprintln(out, "No cache entries are eligible for pruning.")
			return err
		}
		_, err = fmt.Fprintf(out, "No cache entries older than %s are eligible for pruning.\n", formatCacheAge(effectiveAge))
		return err
	}
	action := "Released"
	if opts.dryRun {
		action = "Eligible"
	}
	if _, err = fmt.Fprintf(out, "%s: %s across %d layer blobs and %d snapshots.\n",
		action, formatBytes(int64(min(totalBytes, uint64(math.MaxInt64)))), resp.GetContentBlobs(), resp.GetSnapshots()); err != nil {
		return err
	}
	if !opts.dryRun {
		if resp.ReclaimedBytes != nil {
			_, err = fmt.Fprintf(out, "Containerd reclaimed %s on the container storage filesystem.\n",
				formatBytes(int64(min(*resp.ReclaimedBytes, uint64(math.MaxInt64)))))
		} else {
			_, err = fmt.Fprintln(out, "Containerd will reclaim unreachable data in the background; current images and active apps are preserved.")
		}
	}
	return err
}

func saturatingAdd(a, b uint64) uint64 {
	if math.MaxUint64-a < b {
		return math.MaxUint64
	}
	return a + b
}

func formatCacheAge(seconds uint64) string {
	if seconds%3600 == 0 {
		return fmt.Sprintf("%dh", seconds/3600)
	}
	if seconds%60 == 0 {
		return fmt.Sprintf("%dm", seconds/60)
	}
	return fmt.Sprintf("%ds", seconds)
}
