// world-replay republishes recorded observations in an explicit evaluation run.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/wendylabsinc/wendy/go/world/internal/authority"
	"github.com/wendylabsinc/wendy/go/world/internal/client"
	"github.com/wendylabsinc/wendy/go/world/internal/recording"
	"github.com/wendylabsinc/wendy/go/world/internal/replay"
	"github.com/wendylabsinc/wendy/go/world/internal/sensing"
	"github.com/wendylabsinc/wendy/go/world/internal/world"
)

func credentials(path string) (client.Config, *client.Client, error) {
	var cfg client.Config
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return cfg, nil, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return cfg, nil, err
	}
	if !st.Mode().IsRegular() || st.Mode().Perm()&0077 != 0 {
		return cfg, nil, errors.New("credentials must be a private regular file")
	}
	b, err := io.ReadAll(io.LimitReader(f, authority.MaxEnvelopeBytes+1))
	if err != nil {
		return cfg, nil, err
	}
	if err = authority.DecodeStrict(b, &cfg); err != nil {
		return cfg, nil, err
	}
	if cfg.Grant.Grant.Domain != "simulation" {
		return cfg, nil, errors.New("replay publication requires simulation credentials")
	}
	c, err := client.New(cfg)
	return cfg, c, err
}

func upload(ctx context.Context, c *client.Client, ref sensing.EvidenceRef, data []byte) (time.Time, error) {
	out, err := c.UploadEvidence(ctx, data, ref.ContentType, time.Time{})
	if err == nil {
		return time.UnixMilli(out.RetainUntilUnixMS), nil
	}
	got, meta, readErr := c.DownloadEvidence(ctx, ref.SHA256, ref.Size)
	if readErr != nil {
		return time.Time{}, err
	}
	if meta.ContentType != ref.ContentType || meta.Size != ref.Size || !bytes.Equal(got, data) {
		return time.Time{}, errors.New("existing evidence differs from recording")
	}
	return time.UnixMilli(meta.RetainUntilUnixMS), nil
}

func run(ctx context.Context, args []string, output io.Writer) error {
	f := flag.NewFlagSet("world-replay", flag.ContinueOnError)
	credentialPath := f.String("credentials", "", "private replay publisher credentials")
	directory := f.String("input", "", "verified recording directory")
	digest := f.String("sha256", "", "expected exact recording manifest digest")
	runID := f.String("run-id", "", "new replay evaluation run")
	anchorMS := f.Int64("anchor-unix-ms", 0, "shared replay start in wall time")
	ttl := f.Duration("ttl", 5*time.Second, "maximum replay observation lifetime")
	if err := f.Parse(args); err != nil {
		return err
	}
	if f.NArg() != 0 || *directory == "" || *runID == "" || !sensing.DigestValid(*digest) || *anchorMS <= 0 || *ttl <= 0 || *ttl > time.Hour {
		return errors.New("input, exact sha256, new run-id, shared anchor-unix-ms and TTL in (0,1h] required")
	}
	bundle, err := recording.Load(*directory)
	if err != nil {
		return err
	}
	if bundle.SHA256 != *digest {
		return errors.New("recording digest differs from requested archive")
	}
	cfg, c, err := credentials(*credentialPath)
	if err != nil {
		return err
	}
	g := cfg.Grant.Grant
	if g.Tenant != bundle.Manifest.Tenant || g.Site != bundle.Manifest.Site || *runID == bundle.Manifest.RunID {
		return errors.New("recording scope mismatch or reused original run")
	}
	anchor := time.UnixMilli(*anchorMS)
	if delay := time.Until(anchor); delay < -time.Second || delay > time.Minute {
		return errors.New("replay start must be between one second ago and one minute ahead")
	}
	deadline := anchor.Add(bundle.Manifest.EndedAt.Sub(bundle.Manifest.StartedAt)).Add(*ttl + 30*time.Second)
	ctx, cancel := context.WithDeadline(ctx, deadline)
	defer cancel()
	var originals []world.Component
	for _, original := range bundle.Manifest.Records {
		if original.Key.Source.Service == g.Source.Service {
			originals = append(originals, original)
		}
	}
	if len(originals) == 0 {
		return errors.New("recording has no samples for authenticated sensor service")
	}
	config := replay.Config{Tenant: g.Tenant, Site: g.Site, RunID: *runID, RecordingSHA256: *digest, Source: g.Source, Epoch: 1, CaptureAnchor: bundle.Manifest.StartedAt, ReplayAnchor: anchor, TTL: *ttl}
	// Validate every selected record before network writes or epoch allocation.
	for i, original := range originals {
		config.Sequence = uint64(i + 1)
		if _, err := replay.Build(original, config); err != nil {
			return err
		}
	}
	retentions := map[string]time.Time{}
	for _, original := range originals {
		sample, err := sensing.FromComponent(original)
		if err != nil {
			return err
		}
		if ref := sample.Evidence; ref != nil {
			if _, exists := retentions[ref.SHA256]; exists {
				continue
			}
			until, err := upload(ctx, c, *ref, bundle.Blobs[ref.SHA256])
			if err != nil {
				return err
			}
			retentions[ref.SHA256] = until
		}
	}
	epoch, err := c.NextEpoch(ctx)
	if err != nil {
		return err
	}
	config.Epoch = epoch
	for i, original := range originals {
		config.Sequence = uint64(i + 1)
		component, err := replay.Build(original, config)
		if err != nil {
			return err
		}
		sample, err := sensing.FromComponent(original)
		if err != nil {
			return err
		}
		if ref := sample.Evidence; ref != nil {
			// Evidence must outlive the entire published observation interval.
			component.TTL = min(component.TTL, retentions[ref.SHA256].Sub(component.Timing.SampleTime))
			if component.TTL <= 0 {
				return errors.New("recording evidence expires before replay")
			}
		}
		if _, _, err = replay.FromComponent(component); err != nil {
			return err
		}
		if delay := time.Until(component.Timing.SampleTime); delay > 0 {
			timer := time.NewTimer(delay)
			select {
			case <-ctx.Done():
				timer.Stop()
				return ctx.Err()
			case <-timer.C:
			}
		}
		if !time.Now().Before(component.Timing.SampleTime.Add(component.TTL)) {
			return errors.New("replay sample expired before publication")
		}
		if err = c.Publish(ctx, component); err != nil {
			return err
		}
	}
	return json.NewEncoder(output).Encode(map[string]any{"published": len(originals), "evidence_uploaded_or_reused": len(retentions), "recording_sha256": *digest, "original_run_id": bundle.Manifest.RunID, "replay_run_id": *runID, "sensor_id": g.Source.Service, "epoch": epoch, "mode": "replayed", "speed": 1})
}

func main() {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	if err := run(ctx, os.Args[1:], os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
