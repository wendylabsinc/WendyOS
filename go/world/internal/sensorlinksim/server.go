package sensorlinksim

import (
	"context"
	"errors"
	"fmt"
	"math"
	"net"
	"time"

	"github.com/wendylabsinc/wendy/go/world/internal/sensorlink"
	pb "github.com/wendylabsinc/wendy/go/proto/gen/sensorlinkpb"
)

type DropChunk struct{ FrameSequence, ChunkSequence uint32 }
type Config struct {
	ChannelID                      uint32
	Anchor                         time.Time
	ChunkBytes                     int
	TimestampOffsetUS              int64
	Drop                           *DropChunk
	HandshakeTimeout, WriteTimeout time.Duration
	PingInterval                   time.Duration
}
type FrameReport struct {
	FrameSequence uint32 `json:"frame_seq"`
	TimestampUS   uint64 `json:"ts_us"`
	JPEGHash      string `json:"jpeg_sha256"`
	Bytes         int    `json:"bytes"`
	Chunks        int    `json:"chunks"`
	OmittedChunks int    `json:"omitted_chunks"`
}
type Stats struct {
	Mode          string        `json:"mode"`
	RunID         string        `json:"run_id"`
	SensorID      string        `json:"sensor_id"`
	ChannelID     uint32        `json:"channel_id"`
	Subscribed    bool          `json:"subscribed"`
	Frames        []FrameReport `json:"frames"`
	FramesSent    int           `json:"frames_sent"`
	ChunksSent    int           `json:"chunks_sent"`
	ChunksOmitted int           `json:"chunks_omitted"`
	PingsSent     int           `json:"pings_sent"`
}

func validateConfig(capture Capture, cfg *Config) error {
	if err := capture.Validate(); err != nil {
		return err
	}
	if cfg.ChannelID == 0 || cfg.Anchor.IsZero() || cfg.Anchor.UnixMicro() <= 0 || cfg.Anchor.UnixMicro() > math.MaxInt64-int64(25*time.Hour/time.Microsecond) || cfg.ChunkBytes < 64 || cfg.ChunkBytes > 64<<10 || cfg.TimestampOffsetUS < -int64(time.Hour/time.Microsecond) || cfg.TimestampOffsetUS > int64(time.Hour/time.Microsecond) {
		return errors.New("invalid SensorLink simulation configuration")
	}
	if cfg.HandshakeTimeout == 0 {
		cfg.HandshakeTimeout = 10 * time.Second
	}
	if cfg.WriteTimeout == 0 {
		cfg.WriteTimeout = 5 * time.Second
	}
	if cfg.PingInterval == 0 {
		cfg.PingInterval = time.Second
	}
	if cfg.HandshakeTimeout < time.Millisecond || cfg.HandshakeTimeout > time.Minute || cfg.WriteTimeout < time.Millisecond || cfg.WriteTimeout > time.Minute {
		return errors.New("invalid connection deadline")
	}
	if cfg.PingInterval < 10*time.Millisecond || cfg.PingInterval > time.Second {
		return errors.New("invalid ping interval")
	}
	if cfg.Drop != nil {
		found := false
		for _, f := range capture.Frames {
			if uint32(f.Sample.Sequence-1) == cfg.Drop.FrameSequence && uint64(cfg.Drop.ChunkSequence) < uint64((len(f.JPEG)+cfg.ChunkBytes-1)/cfg.ChunkBytes) {
				found = true
			}
		}
		if !found {
			return errors.New("drop-chunk does not select an existing chunk")
		}
	}
	for _, f := range capture.Frames {
		if cfg.Anchor.UnixMicro()+f.Sample.SampleNS/1000+cfg.TimestampOffsetUS <= 0 {
			return errors.New("fault timestamp becomes nonpositive")
		}
	}
	return nil
}

// Serve handles one finite subscription. Closing or cancelling the source never
// changes its configured capture timestamps. The listener is always closed.
func Serve(ctx context.Context, listener net.Listener, capture Capture, cfg Config) (Stats, error) {
	defer listener.Close()
	if err := validateConfig(capture, &cfg); err != nil {
		return Stats{}, err
	}
	stop := context.AfterFunc(ctx, func() { _ = listener.Close() })
	defer stop()
	conn, err := listener.Accept()
	if err != nil {
		if ctx.Err() != nil {
			return Stats{}, ctx.Err()
		}
		return Stats{}, err
	}
	return ServeConn(ctx, conn, capture, cfg)
}

// ServeConn implements the manifest/subscription exchange and sends original
// camera intervals as chunked SensorData protobufs. The caller must not mutate
// the capture while a session runs. The connection closes on every return path.
func ServeConn(ctx context.Context, conn net.Conn, capture Capture, cfg Config) (Stats, error) {
	defer conn.Close()
	stats := Stats{Mode: "sensorlink-unix-us-simulation", RunID: capture.RunID, SensorID: capture.SensorID, ChannelID: cfg.ChannelID, Frames: []FrameReport{}}
	if err := validateConfig(capture, &cfg); err != nil {
		return stats, err
	}
	stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stop()
	if err := conn.SetDeadline(time.Now().Add(cfg.HandshakeTimeout)); err != nil {
		return stats, err
	}
	manifest := &pb.SensorManifest{Sensors: []*pb.SensorDescriptor{{ChannelId: cfg.ChannelID, InputId: cfg.ChannelID, Name: capture.SensorID, Format: &pb.SensorDescriptor_Video{Video: &pb.VideoFormat{Codec: pb.VideoFormat_MJPEG, Width: uint32(capture.Width), Height: uint32(capture.Height), Fps: uint32(capture.FPS)}}}}}
	if err := sensorlink.WriteMessage(conn, &pb.Envelope{Msg: &pb.Envelope_Manifest{Manifest: manifest}}); err != nil {
		return stats, err
	}
	envelope, err := sensorlink.ReadMessage(conn)
	if err != nil {
		return stats, err
	}
	sub := envelope.GetSubscribe()
	if sub == nil || len(sub.ChannelId) != 1 || sub.ChannelId[0] != cfg.ChannelID {
		return stats, errors.New("subscription must select exactly the configured channel")
	}
	stats.Subscribed = true
	if err = conn.SetDeadline(time.Time{}); err != nil {
		return stats, err
	}
	for _, frame := range capture.Frames {
		due := cfg.Anchor.Add(time.Duration(frame.Sample.SampleNS))
		for delay := time.Until(due); delay > 0; delay = time.Until(due) {
			timer := time.NewTimer(min(delay, cfg.PingInterval))
			select {
			case <-ctx.Done():
				timer.Stop()
				return stats, ctx.Err()
			case <-timer.C:
			}
			if time.Now().Before(due) {
				if err = conn.SetWriteDeadline(time.Now().Add(cfg.WriteTimeout)); err != nil {
					return stats, err
				}
				if err = sensorlink.WriteMessage(conn, &pb.Envelope{Msg: &pb.Envelope_Ping{Ping: &pb.Ping{TsUs: uint64(time.Now().UnixMicro())}}}); err != nil {
					return stats, fmt.Errorf("ping while waiting for capture: %w", err)
				}
				stats.PingsSent++
			}
		}
		if err = ctx.Err(); err != nil {
			return stats, err
		}
		seq := uint32(frame.Sample.Sequence - 1)
		stamp := uint64(cfg.Anchor.UnixMicro() + frame.Sample.SampleNS/1000 + cfg.TimestampOffsetUS)
		chunks := (len(frame.JPEG) + cfg.ChunkBytes - 1) / cfg.ChunkBytes
		report := FrameReport{FrameSequence: seq, TimestampUS: stamp, JPEGHash: hash(frame.JPEG), Bytes: len(frame.JPEG), Chunks: chunks}
		for offset, chunk := 0, uint32(0); offset < len(frame.JPEG); chunk++ {
			end := min(offset+cfg.ChunkBytes, len(frame.JPEG))
			if cfg.Drop != nil && cfg.Drop.FrameSequence == seq && cfg.Drop.ChunkSequence == chunk {
				report.OmittedChunks++
				stats.ChunksOmitted++
				offset = end
				continue
			}
			flags := uint32(0)
			if chunk == 0 {
				flags |= sensorlink.FlagKeyframe
			}
			if end == len(frame.JPEG) {
				flags |= sensorlink.FlagLastChunk
			}
			data := &pb.SensorData{ChannelId: cfg.ChannelID, FrameSeq: seq, ChunkSeq: chunk, TsUs: stamp, Flags: flags, Payload: frame.JPEG[offset:end]}
			if err = conn.SetWriteDeadline(time.Now().Add(cfg.WriteTimeout)); err != nil {
				return stats, err
			}
			if err = sensorlink.WriteMessage(conn, &pb.Envelope{Msg: &pb.Envelope_Data{Data: data}}); err != nil {
				return stats, fmt.Errorf("frame %d chunk %d: %w", seq, chunk, err)
			}
			stats.ChunksSent++
			offset = end
		}
		stats.Frames = append(stats.Frames, report)
		stats.FramesSent++
	}
	return stats, nil
}
