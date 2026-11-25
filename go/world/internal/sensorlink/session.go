package sensorlink

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"time"
	"unicode/utf8"

	pb "github.com/wendylabsinc/wendy/go/proto/gen/sensorlinkpb"
	"google.golang.org/protobuf/proto"
)

type Options struct {
	HandshakeTimeout time.Duration
	IdleTimeout      time.Duration
	MaxFrameBytes    int
}

// ValidateManifest checks bounded declarations and the selected channel set.
// Media decoding and calibration compatibility belong to the consuming adapter.
func ValidateManifest(m *pb.SensorManifest, channels []uint32) error {
	if m == nil || len(m.Sensors) == 0 || len(m.Sensors) > MaxManifestChannels || len(channels) < 1 || len(channels) > MaxChannels {
		return errors.New("invalid sensorlink manifest or subscription size")
	}
	selected := map[uint32]bool{}
	for _, id := range channels {
		if id == 0 || selected[id] {
			return errors.New("invalid selected channel")
		}
		selected[id] = true
	}
	seen := map[uint32]bool{}
	inputs := map[uint32]bool{}
	for _, d := range m.Sensors {
		if d == nil || d.ChannelId == 0 || seen[d.ChannelId] || d.Name == "" || len(d.Name) > 256 || !utf8.ValidString(d.Name) {
			return errors.New("invalid sensorlink channel descriptor")
		}
		seen[d.ChannelId] = true
		switch value := d.Format.(type) {
		case *pb.SensorDescriptor_Video:
			v := value.Video
			if v == nil || (v.Codec != pb.VideoFormat_MJPEG && v.Codec != pb.VideoFormat_H264) || v.Width < 1 || v.Width > 8192 || v.Height < 1 || v.Height > 8192 || v.Fps < 1 || v.Fps > 240 {
				return errors.New("invalid sensorlink video format")
			}
		case *pb.SensorDescriptor_Audio:
			v := value.Audio
			if v == nil || (v.Codec != pb.AudioFormat_PCM_S16LE && v.Codec != pb.AudioFormat_OPUS) || v.SampleRate < 1 || v.SampleRate > 384000 || v.Channels < 1 || v.Channels > 32 {
				return errors.New("invalid sensorlink audio format")
			}
		case *pb.SensorDescriptor_Sensor:
			v := value.Sensor
			if v == nil || v.Schema == "" || len(v.Schema) > 256 || !utf8.ValidString(v.Schema) || v.RateHz < 1 || v.RateHz > 1000000 || v.SampleBytes < 1 || v.SampleBytes > MaxFrameBytes {
				return errors.New("invalid sensorlink scalar format")
			}
		default:
			return errors.New("sensorlink descriptor has no supported format")
		}
		if selected[d.ChannelId] {
			// A zero input_id carries no physical-input grouping information.
			if d.InputId != 0 && inputs[d.InputId] {
				return errors.New("selected channels share an exclusive input")
			}
			inputs[d.InputId] = true
		}
	}
	for id := range selected {
		if !seen[id] {
			return errors.New("selected channel absent from manifest")
		}
	}
	return nil
}

// Stream retains at most one completed frame per channel, replacing an unsent
// frame when a newer one arrives. Next selects channels round-robin. The reader
// never blocks on a slow consumer. A session owns and closes its connection.
type Stream struct {
	Manifest   *pb.SensorManifest
	conn       net.Conn
	mu         sync.Mutex
	pending    map[uint32]*Frame
	channels   []uint32
	next       int
	dropped    uint64
	err        error
	ended      bool
	notify     chan struct{}
	done       chan struct{}
	closeOnce  sync.Once
	stopCancel func() bool
}

func Connect(ctx context.Context, conn net.Conn, channels []uint32, options Options) (*Stream, error) {
	if conn == nil {
		return nil, errors.New("sensorlink connection required")
	}
	fail := func(err error) (*Stream, error) { conn.Close(); return nil, err }
	if options.HandshakeTimeout == 0 {
		options.HandshakeTimeout = 3 * time.Second
	}
	if options.IdleTimeout == 0 {
		options.IdleTimeout = 5 * time.Second
	}
	if options.HandshakeTimeout < time.Millisecond || options.HandshakeTimeout > time.Minute || options.IdleTimeout < time.Millisecond || options.IdleTimeout > time.Minute {
		return fail(errors.New("sensorlink deadlines outside bounds"))
	}
	assembler, err := NewAssembler(channels, options.MaxFrameBytes)
	if err != nil {
		return fail(err)
	}
	// Cancellation closes the socket even during handshake, blocked read or write.
	stop := context.AfterFunc(ctx, func() { conn.Close() })
	handshakeDone := false
	defer func() {
		if !handshakeDone {
			stop()
		}
	}()
	if err = conn.SetDeadline(time.Now().Add(options.HandshakeTimeout)); err != nil {
		return fail(err)
	}
	env, err := ReadMessage(conn)
	if err != nil {
		return fail(fmt.Errorf("sensorlink manifest: %w", err))
	}
	manifest := env.GetManifest()
	if err = ValidateManifest(manifest, channels); err != nil {
		return fail(err)
	}
	if err = WriteMessage(conn, &pb.Envelope{Msg: &pb.Envelope_Subscribe{Subscribe: &pb.Subscribe{ChannelId: append([]uint32(nil), channels...)}}}); err != nil {
		return fail(err)
	}
	if err = conn.SetWriteDeadline(time.Time{}); err != nil {
		return fail(err)
	}
	s := &Stream{Manifest: proto.Clone(manifest).(*pb.SensorManifest), conn: conn, pending: map[uint32]*Frame{}, channels: append([]uint32(nil), channels...), notify: make(chan struct{}, 1), done: make(chan struct{}), stopCancel: stop}
	handshakeDone = true
	go s.read(ctx, assembler, options.IdleTimeout)
	return s, nil
}

func (s *Stream) read(ctx context.Context, a *Assembler, idle time.Duration) {
	var end error
	defer func() {
		if ctx.Err() != nil {
			end = ctx.Err()
		}
		if end == nil {
			end = io.EOF
		}
		s.mu.Lock()
		s.err = end
		s.ended = true
		s.mu.Unlock()
		close(s.done)
		s.stopCancel()
		s.conn.Close()
	}()
	for {
		if end = s.conn.SetReadDeadline(time.Now().Add(idle)); end != nil {
			return
		}
		var envelope *pb.Envelope
		envelope, end = ReadMessage(s.conn)
		if end != nil {
			return
		}
		if envelope.GetPing() != nil {
			continue
		}
		data := envelope.GetData()
		if data == nil {
			end = errors.New("unexpected sensorlink message after subscribe")
			return
		}
		before := a.Dropped()
		frame, err := a.Add(data)
		s.mu.Lock()
		s.dropped += a.Dropped() - before
		s.mu.Unlock()
		if err != nil {
			end = err
			return
		}
		if frame == nil {
			continue
		}
		s.mu.Lock()
		if s.pending[frame.ChannelID] != nil {
			s.dropped++
		}
		s.pending[frame.ChannelID] = frame
		s.mu.Unlock()
		select {
		case s.notify <- struct{}{}:
		default:
		}
	}
}

func (s *Stream) Next(ctx context.Context) (*Frame, error) {
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		s.mu.Lock()
		for offset := 0; offset < len(s.channels); offset++ {
			index := (s.next + offset) % len(s.channels)
			id := s.channels[index]
			if frame := s.pending[id]; frame != nil {
				delete(s.pending, id)
				s.next = (index + 1) % len(s.channels)
				s.mu.Unlock()
				return frame, nil
			}
		}
		ended, err := s.ended, s.err
		s.mu.Unlock()
		if ended {
			return nil, err
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-s.notify:
		case <-s.done:
		}
	}
}

func (s *Stream) Close() error {
	var err error
	s.closeOnce.Do(func() { err = s.conn.Close() })
	return err
}
func (s *Stream) Dropped() uint64 { s.mu.Lock(); defer s.mu.Unlock(); return s.dropped }
