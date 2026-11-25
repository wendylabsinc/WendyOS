package main

import (
	"bufio"
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

	"github.com/wendylabsinc/wendy/go/world/internal/client"
	"github.com/wendylabsinc/wendy/go/world/internal/ingest"
	"github.com/wendylabsinc/wendy/go/world/internal/world"
)

func run(ctx context.Context, args []string, input io.Reader, output io.Writer) error {
	f := flag.NewFlagSet("world-ingest", flag.ContinueOnError)
	credentials := f.String("credentials", "", "path to private signed client credentials")
	frameID := f.String("frame-id", "office-floor", "local frame ID")
	frameRevision := f.String("frame-revision", "office-frame-v1", "local frame revision")
	runID := f.String("run-id", "", "simulation run ID")
	ttl := f.Duration("ttl", 5*time.Second, "record lifetime")
	if e := f.Parse(args); e != nil {
		return e
	}
	if *credentials == "" || *runID == "" || f.NArg() != 0 {
		return errors.New("credentials and run-id are required")
	}
	file, e := os.Open(*credentials)
	if e != nil {
		return e
	}
	defer file.Close()
	b, e := io.ReadAll(io.LimitReader(file, 65537))
	if e != nil {
		return e
	}
	if len(b) > 65536 {
		return errors.New("credential file too large")
	}
	var cfg client.Config
	d := json.NewDecoder(bytes.NewReader(b))
	d.DisallowUnknownFields()
	if e = d.Decode(&cfg); e != nil {
		return e
	}
	if e = d.Decode(new(any)); !errors.Is(e, io.EOF) {
		return errors.New("invalid credential JSON")
	}
	if cfg.Grant.Grant.Domain != "simulation" {
		return errors.New("simulator ingest refuses physical credentials")
	}
	c, e := client.New(cfg)
	if e != nil {
		return e
	}
	epoch, e := c.NextEpoch(ctx)
	if e != nil {
		return e
	}
	converter, e := ingest.New(ingest.Config{Tenant: cfg.Grant.Grant.Tenant, Site: cfg.Grant.Grant.Site, Frame: world.FrameRef{ID: *frameID, Revision: *frameRevision}, Source: cfg.Grant.Grant.Source, Epoch: epoch, RunID: *runID, Anchor: time.Now(), TTL: *ttl})
	if e != nil {
		return e
	}
	scanner := bufio.NewScanner(input)
	scanner.Buffer(make([]byte, 4096), 65536)
	count := 0
	for scanner.Scan() {
		component, e := converter.Convert(scanner.Bytes())
		if e != nil {
			return fmt.Errorf("sample %d: %w", count+1, e)
		}
		// Pace simulated capture against a local anchor. Retain original relative
		// time and mark every record simulated; never use it as authority time.
		if delay := time.Until(component.Timing.SampleTime); delay > 0 {
			timer := time.NewTimer(delay)
			select {
			case <-ctx.Done():
				timer.Stop()
				return ctx.Err()
			case <-timer.C:
			}
		}
		if e = c.Publish(ctx, component); e != nil {
			return e
		}
		count++
	}
	if e = scanner.Err(); e != nil {
		return e
	}
	if count == 0 {
		return errors.New("no simulator observations received")
	}
	return json.NewEncoder(output).Encode(map[string]any{"published": count, "epoch": epoch, "run_id": *runID})
}
func main() {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	if e := run(ctx, os.Args[1:], os.Stdin, os.Stdout); e != nil {
		fmt.Fprintln(os.Stderr, e)
		os.Exit(1)
	}
}
