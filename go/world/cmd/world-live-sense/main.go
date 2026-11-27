// world-live-sense publishes one authenticated sensor from the live shared scene.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/wendylabsinc/wendy/go/world/internal/appsupport"
	"github.com/wendylabsinc/wendy/go/world/internal/client"
	"github.com/wendylabsinc/wendy/go/world/internal/livecapture"
	"github.com/wendylabsinc/wendy/go/world/internal/sensing"
	"github.com/wendylabsinc/wendy/go/world/internal/world"
)

func readToken(path string) (string, error) {
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return "", err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return "", err
	}
	if !st.Mode().IsRegular() || st.Mode().Perm()&0077 != 0 {
		return "", errors.New("sensor token must be a private regular file")
	}
	b, err := io.ReadAll(io.LimitReader(f, 257))
	if err != nil {
		return "", err
	}
	if len(b) > 256 {
		return "", errors.New("sensor token too large")
	}
	return strings.TrimSpace(string(b)), nil
}
func retryableCapture(err error) bool {
	var status livecapture.HTTPError
	if errors.As(err, &status) {
		return status.Status == 408 || status.Status == 429 || status.Status >= 500
	}
	return client.Transient(err) || errors.Is(err, livecapture.ErrStale)
}
func run(ctx context.Context, args []string) error {
	f := flag.NewFlagSet("world-live-sense", flag.ContinueOnError)
	credentials := f.String("credentials", "", "private sensor publication credentials")
	sensor := f.String("sensor", "", "one sensor ID")
	runID := f.String("run-id", "", "expected simulation run")
	adapter := f.String("adapter", "", "isolated live scene URL")
	tokenFile := f.String("sensor-token-file", "", "private read-only scene token")
	ttl := f.Duration("ttl", 5*time.Second, "observation lifetime")
	retention := f.Duration("evidence-retention", 7*24*time.Hour, "rolling evidence retention from capture time")
	duration := f.Duration("duration", 0, "optional bounded run duration")
	if err := f.Parse(args); err != nil {
		return err
	}
	if *sensor == "" || *runID == "" || f.NArg() != 0 || *ttl <= 0 || *ttl > time.Minute || *retention <= 0 || *retention > 7*24*time.Hour {
		return errors.New("sensor, run-id, TTL in (0,1m] and evidence retention in (0,7d] required")
	}
	cfg, publisher, err := appsupport.SimulationClient(*credentials)
	if err != nil {
		return err
	}
	if cfg.Grant.Grant.Source.Service != *sensor {
		return errors.New("sensor ID does not match authenticated service")
	}
	token, err := readToken(*tokenFile)
	if err != nil {
		return err
	}
	frame := world.FrameRef{ID: "office-floor", Revision: "office-frame-v1"}
	capture, err := livecapture.New(livecapture.Config{URL: *adapter, Token: token, RunID: *runID, Site: cfg.Grant.Grant.Site, Frame: frame})
	if err != nil {
		return err
	}
	if *duration > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, *duration)
		defer cancel()
	}
	epoch, err := awaitEpoch(ctx, publisher)
	if err != nil {
		if ctx.Err() != nil {
			return nil
		}
		return err
	}
	conversion := sensing.Config{Tenant: cfg.Grant.Grant.Tenant, Site: cfg.Grant.Grant.Site, Frame: frame, Source: cfg.Grant.Grant.Source, Epoch: epoch, RunID: *runID, TTL: *ttl}
	// This bounded cache preserves request metadata for duplicate content. It
	// never proves presence: rolling evidence is probed on every publication.
	retentionByDigest := map[string]int64{}
	order := make([]string, 0, 512)
	sequence := uint64(0)
	var pending capturePublication
	var retry retryDelay
	count := 0
	lastState := ""
	logState := func(state string) {
		if state != lastState {
			json.NewEncoder(os.Stdout).Encode(map[string]any{"sensor_id": *sensor, "state": state})
			lastState = state
		}
	}
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}
		if !retry.ready(time.Now()) {
			continue
		}
		tick, done := context.WithTimeout(ctx, 2*time.Second)
		snapshot, err := capture.Capture(tick)
		if err != nil {
			done()
			if ctx.Err() != nil {
				return nil
			}
			if retryableCapture(err) {
				logState("capture unavailable")
				retry.failed(time.Now())
				continue
			}
			return err
		}
		if snapshot.Sequence <= sequence {
			retry.reset()
			done()
			continue
		}
		var sample *sensing.Sample
		for i := range snapshot.Observations {
			if snapshot.Observations[i].SensorID == *sensor {
				sample = &snapshot.Observations[i]
				break
			}
		}
		if sample == nil {
			done()
			logState("sensor unavailable")
			retry.failed(time.Now())
			continue
		}
		if ref := sample.Evidence; ref != nil {
			data, e := snapshot.Evidence(*ref)
			if e != nil {
				done()
				return e
			}
			until := time.UnixMilli(snapshot.SampleUnixMS).Add(*retention)
			if cached := retentionByDigest[ref.SHA256]; cached > time.Now().UnixMilli() {
				until = time.UnixMilli(cached)
			}
			retainUntil, e := storeEvidence(tick, publisher, *ref, data, until)
			if e != nil {
				done()
				if ctx.Err() != nil {
					return nil
				}
				if !client.Transient(e) {
					return e
				}
				retry.failed(time.Now())
				logState("evidence upload unavailable")
				continue
			}
			if _, exists := retentionByDigest[ref.SHA256]; !exists {
				if len(order) == 512 {
					delete(retentionByDigest, order[0])
					order = order[1:]
				}
				order = append(order, ref.SHA256)
			}
			retentionByDigest[ref.SHA256] = retainUntil
		}
		if age := time.Now().Sub(time.UnixMilli(snapshot.SampleUnixMS)); age < 0 || age > 500*time.Millisecond {
			done()
			logState("capture expired before publication")
			continue
		}
		component, e := snapshot.Component(*sensor, conversion)
		if e != nil {
			done()
			return e
		}
		if e = pending.prepare(publisher, snapshot.Sequence, component); e != nil {
			done()
			return e
		}
		if e = publisher.PublishPrepared(tick, pending.signed); e != nil {
			done()
			if ctx.Err() != nil {
				return nil
			}
			if !client.Transient(e) {
				return e
			}
			retry.failed(time.Now())
			logState("publication unavailable")
			continue
		}
		retry.reset()
		sequence = snapshot.Sequence
		count++
		logState("publishing")
		if count == 1 || count%25 == 0 {
			json.NewEncoder(os.Stdout).Encode(map[string]any{"sensor_id": *sensor, "published": count, "epoch": epoch, "capture_sequence": sequence, "capture_unix_ms": snapshot.SampleUnixMS, "boot_id": snapshot.BootID})
		}
		done()
	}
}
func main() {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	if err := run(ctx, os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
