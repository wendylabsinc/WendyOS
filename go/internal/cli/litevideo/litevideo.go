// Package litevideo streams one camera channel from a Wendy Lite device over
// sensor-link (Proto/wendy/lite/sensorlink.proto): it fetches the device's
// sensor manifest, picks a camera channel out of it, subscribes, and delivers
// the frames the device then pushes.
//
// It sits outside cli/commands so both the CLI and the bundled MCP server can
// reach it — cli/mcp cannot import cli/commands, because commands imports mcp.
//
// One limit worth knowing before wondering why a stream is empty: a WendyCom
// message body carries a 16-bit length, so a whole frame — envelope and payload
// — cannot exceed 65535 bytes on a direct link, and SensorFrame has no
// fragmentation fields. That is roughly one VGA JPEG at decent quality. A
// device configured above it subscribes cleanly and then delivers nothing.
package litevideo

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/wendylabsinc/wendy/go/proto/gen/sensorlinkpb"
)

// Client is the slice of the WendyCom client this package needs.
// *liteclient.WendyLiteClient satisfies it structurally, so neither package
// imports the other and a test supplies a fake with no hardware in sight.
type Client interface {
	GetSensorManifest(timeout time.Duration) (*sensorlinkpb.SensorManifest, error)
	SensorLinkSubscribe(channelIDs []uint32, timeout time.Duration) error
	SensorLinkUnsubscribe(channelIDs []uint32, timeout time.Duration) error
	AddSensorFrameListener(fn func(*sensorlinkpb.SensorFrame)) func()
	Close() error
}

// flagKeyframe is bit0 of SensorFrame.flags (sensorlink.proto). The generated
// package has no constant for it.
const flagKeyframe = 1 << 0

// Frame is one whole encoded picture from a camera channel.
type Frame struct {
	// Data is a complete JPEG, or a complete H.264 access unit. It is never a
	// partial one: sensor-link frames whole pictures, unlike the agent's video
	// stream, whose chunks have to be concatenated.
	Data []byte
	// Seq is the device's own sequence number. A gap means the DEVICE dropped
	// a frame; frames this package drops are counted by Source.Dropped.
	Seq      uint32
	TsUs     uint64
	Keyframe bool
}

// Options tunes a Source. The zero value is usable: every field falls back to
// the default named on it.
type Options struct {
	// Buffer is how many frames may wait between the WendyCom read loop and
	// Recv. Default defaultBuffer. Frames arriving with the buffer full are
	// dropped — see Source.onFrame for why nothing else is possible.
	Buffer int
	// Timeout bounds each round trip to the device (manifest, subscribe,
	// unsubscribe). Default defaultTimeout.
	Timeout time.Duration
	// Idle is how long Recv waits for a frame before giving up. Default
	// defaultIdle; a negative value disables the timeout.
	//
	// It is not a nicety. When a link dies the client's read loop exits
	// without telling frame listeners anything, so without this a yanked cable
	// leaves Recv blocked forever on a frozen picture.
	Idle time.Duration
}

const (
	defaultBuffer  = 8
	defaultTimeout = 3 * time.Second
	defaultIdle    = 10 * time.Second
)

func (o Options) withDefaults() Options {
	if o.Buffer <= 0 {
		o.Buffer = defaultBuffer
	}
	if o.Timeout <= 0 {
		o.Timeout = defaultTimeout
	}
	if o.Idle == 0 {
		o.Idle = defaultIdle
	}
	return o
}

// Source is one subscribed camera channel. Recv delivers its frames; Close
// stops it.
type Source struct {
	client  Client
	opts    Options
	desc    *sensorlinkpb.SensorDescriptor
	cameras int

	frames chan Frame
	// closed signals teardown. Termination is signalled here and NOT by
	// closing frames: unregistering a listener only swaps the client's
	// published listener slice, so a dispatch that already loaded the old one
	// can still call onFrame after unregister returns. A send on a closed
	// channel there would panic the read loop.
	closed     chan struct{}
	unregister func()

	dropped atomic.Uint64
	// needKeyframe is read-loop-owned: onFrame is the only reader and the only
	// writer, and the client calls it from one goroutine, so it needs no lock.
	needKeyframe bool
	// resync records whether dropping a frame means the decoder needs a
	// keyframe to recover. True for H.264; false for MJPEG, where every frame
	// is independent.
	resync bool

	closeOnce sync.Once
	closeErr  error
}

// Open fetches c's sensor manifest, picks the first camera channel in it,
// subscribes to that channel and starts delivering its frames.
//
// It takes ownership of c: on success Close closes it, and on every error path
// Open closes it before returning, so a caller holding an error never has to
// wonder whether a serial port is still locked.
//
// ctx is honoured between device round trips; it cannot interrupt one in
// flight, because the WendyCom client bounds its calls by timeout rather than
// by context.
func Open(ctx context.Context, c Client, opts Options) (_ *Source, err error) {
	opts = opts.withDefaults()
	defer func() {
		if err != nil {
			c.Close() //nolint:errcheck — the caller is getting err, not a client
		}
	}()

	if err := ctx.Err(); err != nil {
		return nil, err
	}
	manifest, err := c.GetSensorManifest(opts.Timeout)
	if err != nil {
		return nil, fmt.Errorf("fetching sensor manifest: %w", err)
	}
	desc, cameras, err := FirstVideoChannel(manifest)
	if err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	s := &Source{
		client:  c,
		opts:    opts,
		desc:    desc,
		cameras: cameras,
		frames:  make(chan Frame, opts.Buffer),
		closed:  make(chan struct{}),
		resync:  desc.GetVideo().GetCodec() == sensorlinkpb.VideoFormat_H264,
	}
	// Listen before subscribing, so no frame of a device that answers fast can
	// arrive before there is anything to receive it.
	s.unregister = c.AddSensorFrameListener(s.onFrame)
	if err := c.SensorLinkSubscribe([]uint32{desc.GetChannelId()}, opts.Timeout); err != nil {
		s.unregister()
		return nil, fmt.Errorf("subscribing to channel %d (%q): %w", desc.GetChannelId(), desc.GetName(), err)
	}
	return s, nil
}

// Channel is the camera channel this Source subscribed to.
func (s *Source) Channel() *sensorlinkpb.SensorDescriptor { return s.desc }

// Format is the channel's declared video format. It can be nil, or carry an
// unspecified codec, on a device whose manifest does not describe the channel.
func (s *Source) Format() *sensorlinkpb.VideoFormat { return s.desc.GetVideo() }

// Cameras is how many camera channels the manifest listed. More than one means
// Open made a choice, which a caller should say out loud.
func (s *Source) Cameras() int { return s.cameras }

// Dropped is how many frames this Source discarded to keep up with the device.
// It does not count frames the device itself dropped — those show as gaps in
// Frame.Seq.
func (s *Source) Dropped() uint64 { return s.dropped.Load() }

// onFrame runs on the WendyCom read loop: one frame at a time, one goroutine,
// no lock held, and nothing under it may wait for the device. A full buffer
// therefore means the frame dies here — there is nowhere else it could be made
// to wait.
//
// The payload needs no copy: the client promises the frame stays valid after
// this returns, and it unmarshals every message fresh.
func (s *Source) onFrame(f *sensorlinkpb.SensorFrame) {
	if f.GetChannelId() != s.desc.GetChannelId() {
		return
	}
	keyframe := f.GetFlags()&flagKeyframe != 0
	if s.needKeyframe && !keyframe {
		// Still resyncing. Handing a decoder pictures that reference a frame it
		// never saw produces smear, which reads as a broken camera rather than
		// as a dropped frame.
		s.dropped.Add(1)
		return
	}
	select {
	case s.frames <- Frame{Data: f.GetPayload(), Seq: f.GetSeq(), TsUs: f.GetTsUs(), Keyframe: keyframe}:
		s.needKeyframe = false
	default:
		s.dropped.Add(1)
		s.needKeyframe = s.resync
	}
}

// ErrClosed is returned by Recv after Close.
var ErrClosed = errors.New("litevideo: source closed")

// Recv returns the next frame, blocking until one arrives. It returns ctx's
// error on cancellation, ErrClosed after Close, and an idle-timeout error when
// the device stops producing (see Options.Idle).
func (s *Source) Recv(ctx context.Context) (Frame, error) {
	var idle <-chan time.Time
	if s.opts.Idle > 0 {
		timer := time.NewTimer(s.opts.Idle)
		defer timer.Stop()
		idle = timer.C
	}
	select {
	case f := <-s.frames:
		return f, nil
	case <-s.closed:
		return Frame{}, ErrClosed
	case <-ctx.Done():
		return Frame{}, ctx.Err()
	case <-idle:
		return Frame{}, s.idleError()
	}
}

func (s *Source) idleError() error {
	return fmt.Errorf("no frames on channel %d (%q) for %s; the device stopped producing, the link dropped, "+
		"or its frames exceed the 65535-byte WendyCom message limit",
		s.desc.GetChannelId(), s.desc.GetName(), s.opts.Idle)
}

// Close stops delivery, tells the device to stop sending, and closes the
// client, in that order.
//
// Each step's place is forced. Unregistering first means no frame arrives
// during the rest. The unsubscribe has to come BEFORE the client closes,
// because it waits for the device's reply and that only arrives while the read
// loop is alive; and it has to run here, on the caller's goroutine, never from
// onFrame — a command that waits for a device reply from inside the read loop
// is waiting for itself.
func (s *Source) Close() error {
	s.closeOnce.Do(func() {
		s.unregister()
		close(s.closed)
		// Best effort: closing the link drops the subscription anyway, so a
		// device that has already gone costs one timeout, not a failure.
		unsubErr := s.client.SensorLinkUnsubscribe([]uint32{s.desc.GetChannelId()}, s.opts.Timeout)
		s.closeErr = errors.Join(unsubErr, s.client.Close())
	})
	return s.closeErr
}
