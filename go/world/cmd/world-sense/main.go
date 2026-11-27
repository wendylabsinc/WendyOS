// world-sense publishes typed samples and capture-node media from a virtual sensor.
package main

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/wendylabsinc/wendy/go/world/internal/appsupport"
	"github.com/wendylabsinc/wendy/go/world/internal/sensing"
	"github.com/wendylabsinc/wendy/go/world/internal/world"
)

func run(ctx context.Context, args []string) error {
	f := flag.NewFlagSet("world-sense", flag.ContinueOnError)
	credentials := f.String("credentials", "", "private sensor credentials")
	directory := f.String("input", "", "rendered run directory")
	sensor := f.String("sensor", "", "sensor ID to ingest")
	runID := f.String("run-id", "", "expected simulation run")
	anchorMS := f.Int64("anchor-unix-ms", 0, "shared real-time simulation anchor")
	ttl := f.Duration("ttl", 5*time.Second, "observation lifetime")
	if err := f.Parse(args); err != nil {
		return err
	}
	if *directory == "" || *sensor == "" || *runID == "" || f.NArg() != 0 {
		return errors.New("input, sensor and run-id required")
	}
	cfg, c, err := appsupport.SimulationClient(*credentials)
	if err != nil {
		return err
	}
	if cfg.Grant.Grant.Source.Service != *sensor {
		return errors.New("sensor does not match authenticated service")
	}
	epoch, err := c.NextEpoch(ctx)
	if err != nil {
		return err
	}
	anchor := time.Now()
	if *anchorMS != 0 {
		anchor = time.UnixMilli(*anchorMS)
	}
	if delta := time.Until(anchor); delta < -time.Minute || delta > time.Minute {
		return errors.New("anchor must be within one minute")
	}
	converter, err := sensing.New(sensing.Config{Tenant: cfg.Grant.Grant.Tenant, Site: cfg.Grant.Grant.Site, Frame: world.FrameRef{ID: "office-floor", Revision: "office-frame-v1"}, Source: cfg.Grant.Grant.Source, Epoch: epoch, RunID: *runID, Anchor: anchor, TTL: *ttl})
	if err != nil {
		return err
	}
	root, err := os.OpenRoot(*directory)
	if err != nil {
		return err
	}
	defer root.Close()
	file, err := appsupport.OpenCapture(root, "samples.jsonl")
	if err != nil {
		return err
	}
	defer file.Close()
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 4096), 65536)
	uploaded := map[string]bool{}
	published := 0
	retain := anchor.Add(time.Hour)
	for scanner.Scan() {
		sample, err := sensing.Decode(scanner.Bytes())
		if err != nil {
			return err
		}
		if sample.SensorID != *sensor {
			continue
		}
		component, err := converter.Convert(scanner.Bytes())
		if err != nil {
			return err
		}
		if ref := sample.Evidence; ref != nil && !uploaded[ref.SHA256] {
			// A validated lowercase digest is the complete filename; no source path or URL is trusted.
			blob, err := appsupport.OpenCapture(root, filepath.Join("blobs", ref.SHA256))
			if err != nil {
				return err
			}
			data, err := io.ReadAll(io.LimitReader(blob, 8<<20+1))
			blob.Close()
			if err != nil {
				return err
			}
			hash := sha256.Sum256(data)
			if len(data) > 8<<20 || int64(len(data)) != ref.Size || hex.EncodeToString(hash[:]) != ref.SHA256 {
				return errors.New("capture evidence digest/size mismatch")
			}
			if _, err = c.UploadEvidence(ctx, data, ref.ContentType, retain); err != nil {
				return err
			}
			uploaded[ref.SHA256] = true
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
			return errors.New("sample expired before publication")
		}
		if err = c.Publish(ctx, component); err != nil {
			return err
		}
		published++
	}
	if err = scanner.Err(); err != nil {
		return err
	}
	if published == 0 {
		return errors.New("no matching sensor samples")
	}
	return json.NewEncoder(os.Stdout).Encode(map[string]any{"published": published, "evidence_uploaded": len(uploaded), "sensor_id": *sensor, "epoch": epoch})
}
func main() {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	if err := run(ctx, os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
