// world-sensorlink-sim serves a finite synthetic camera run over SensorLink TCP.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/wendylabsinc/wendy/go/world/internal/sensorlinksim"
)

func parseDrop(text string) (*sensorlinksim.DropChunk, error) {
	if text == "" {
		return nil, nil
	}
	parts := strings.Split(text, ":")
	if len(parts) != 2 {
		return nil, errors.New("drop-chunk must be zero-based FRAME:CHUNK")
	}
	a, err := strconv.ParseUint(parts[0], 10, 32)
	if err != nil {
		return nil, err
	}
	b, err := strconv.ParseUint(parts[1], 10, 32)
	if err != nil {
		return nil, err
	}
	return &sensorlinksim.DropChunk{FrameSequence: uint32(a), ChunkSequence: uint32(b)}, nil
}
func run(ctx context.Context, args []string, output io.Writer) error {
	f := flag.NewFlagSet("world-sensorlink-sim", flag.ContinueOnError)
	input := f.String("input", "", "synthetic camera capture directory")
	sensor := f.String("sensor", "", "camera sensor ID")
	runID := f.String("run-id", "", "expected synthetic run")
	anchorMS := f.Int64("anchor-unix-ms", 0, "shared simulation wall-clock anchor")
	addr := f.String("addr", "127.0.0.1:50060", "listen address")
	network := f.Bool("allow-network-simulation", false, "allow a non-loopback simulation listener")
	channel := f.Uint("channel", 1, "SensorLink channel ID")
	chunk := f.Int("chunk-bytes", 512, "encoded payload bytes per chunk")
	quality := f.Int("jpeg-quality", 90, "JPEG encoding quality")
	drop := f.String("drop-chunk", "", "omit zero-based wire FRAME:CHUNK")
	offset := f.Int64("timestamp-offset-us", 0, "explicit wire timestamp fault, within one hour")
	if err := f.Parse(args); err != nil {
		return err
	}
	if f.NArg() != 0 || *anchorMS <= 0 || *channel == 0 || uint64(*channel) > 1<<32-1 {
		return errors.New("explicit capture, run, positive anchor and uint32 channel required")
	}
	anchor := time.UnixMilli(*anchorMS)
	if delay := time.Until(anchor); delay < -time.Minute || delay > time.Minute {
		return errors.New("simulation anchor must be within one minute")
	}
	host, _, err := net.SplitHostPort(*addr)
	if err != nil {
		return err
	}
	ip := net.ParseIP(host)
	if !*network && (ip == nil || !ip.IsLoopback()) {
		return errors.New("non-loopback listener requires allow-network-simulation")
	}
	omitted, err := parseDrop(*drop)
	if err != nil {
		return err
	}
	capture, err := sensorlinksim.Load(*input, *sensor, *runID, *quality)
	if err != nil {
		return err
	}
	cfg := sensorlinksim.Config{ChannelID: uint32(*channel), Anchor: anchor, ChunkBytes: *chunk, TimestampOffsetUS: *offset, Drop: omitted}
	deadline := anchor.Add(time.Duration(capture.Frames[len(capture.Frames)-1].Sample.SampleNS)).Add(30 * time.Second)
	ctx, cancel := context.WithDeadline(ctx, deadline)
	defer cancel()
	listener, err := net.Listen("tcp", *addr)
	if err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "sensorlink simulator listening on %s\n", listener.Addr())
	stats, err := sensorlinksim.Serve(ctx, listener, capture, cfg)
	if err != nil {
		return err
	}
	return json.NewEncoder(output).Encode(stats)
}
func main() {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	if err := run(ctx, os.Args[1:], os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
