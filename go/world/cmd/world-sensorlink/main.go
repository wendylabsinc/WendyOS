// world-sensorlink publishes simulated MJPEG SensorLink observations.
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
	"syscall"
	"time"

	"github.com/wendylabsinc/wendy/go/world/internal/authority"
	"github.com/wendylabsinc/wendy/go/world/internal/client"
	"github.com/wendylabsinc/wendy/go/world/internal/sensorlink"
	"github.com/wendylabsinc/wendy/go/world/internal/sensorlinkbridge"
)

func readConfig(path string) (sensorlinkbridge.Config, error) {
	var zero sensorlinkbridge.Config
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return zero, err
	}
	defer f.Close()
	stat, err := f.Stat()
	if err != nil {
		return zero, err
	}
	if !stat.Mode().IsRegular() || stat.Mode().Perm()&0077 != 0 {
		return zero, errors.New("SensorLink configuration must be a private regular file")
	}
	raw, err := io.ReadAll(io.LimitReader(f, authority.MaxEnvelopeBytes+1))
	if err != nil {
		return zero, err
	}
	return sensorlinkbridge.Decode(raw)
}

func run(ctx context.Context, args []string, output io.Writer) error {
	f := flag.NewFlagSet("world-sensorlink", flag.ContinueOnError)
	configPath := f.String("config", "", "private explicit simulation bridge configuration")
	maxFrames := f.Int("max-frames", 0, "stop after this many published frames; zero reads until EOF")
	duration := f.Duration("duration", 30*time.Second, "maximum session duration")
	if err := f.Parse(args); err != nil {
		return err
	}
	if *configPath == "" || f.NArg() != 0 || *maxFrames < 0 || *maxFrames > 100000 || *duration < time.Millisecond || *duration > time.Hour {
		return errors.New("config, max-frames [0,100000] and duration [1ms,1h] required")
	}
	cfg, err := readConfig(*configPath)
	if err != nil {
		return err
	}
	pub, err := client.New(cfg.Publisher)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, *duration)
	defer cancel()
	conn, err := (&net.Dialer{Timeout: 3 * time.Second}).DialContext(ctx, "tcp", cfg.Endpoint)
	if err != nil {
		return err
	}
	stream, err := sensorlink.Connect(ctx, conn, []uint32{cfg.ChannelID}, sensorlink.Options{HandshakeTimeout: 3 * time.Second, IdleTimeout: 5 * time.Second, MaxFrameBytes: 8 << 20})
	if err != nil {
		conn.Close()
		return err
	}
	defer stream.Close()
	if err = cfg.ValidateManifest(stream.Manifest); err != nil {
		return err
	}
	epoch, err := pub.NextEpoch(ctx)
	if err != nil {
		return err
	}
	converter, err := sensorlinkbridge.New(cfg, epoch)
	if err != nil {
		return err
	}
	count := 0
	for *maxFrames == 0 || count < *maxFrames {
		frame, err := stream.Next(ctx)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return err
		}
		component, err := converter.Convert(frame, time.Now())
		if err != nil {
			return err
		}
		if err = sensorlinkbridge.Publish(ctx, pub, component, frame.Payload, time.Now); err != nil {
			return err
		}
		count++
	}
	if count == 0 || (*maxFrames > 0 && count != *maxFrames) {
		return errors.New("SensorLink stream ended before required frames were published")
	}
	return json.NewEncoder(output).Encode(map[string]any{"published": count, "sensor_id": cfg.SampleTemplate.SensorID, "channel_id": cfg.ChannelID, "epoch": epoch, "dropped": stream.Dropped(), "run_id": cfg.SampleTemplate.RunID, "source_clock": cfg.TimestampClock, "mode": "simulation", "media_type": "image/jpeg"})
}

func main() {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	if err := run(ctx, os.Args[1:], os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
