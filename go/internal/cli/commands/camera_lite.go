package commands

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"sync"
	"time"

	"github.com/spf13/cobra"
	"github.com/wendylabsinc/wendy/go/internal/agent/sensorlink"
	"github.com/wendylabsinc/wendy/go/internal/cli/providers"
	"github.com/wendylabsinc/wendy/go/internal/cli/tui"
	agentpb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
	sensorlinkpb "github.com/wendylabsinc/wendy/go/proto/gen/sensorlinkpb"
	"google.golang.org/protobuf/encoding/protojson"
)

type liteCameraClient interface {
	GetSensorManifest(time.Duration) (*sensorlinkpb.SensorManifest, error)
	SensorLinkSubscribe([]uint32, time.Duration) error
	SensorLinkUnsubscribe([]uint32, time.Duration) error
	AddSensorDataListener(func(*sensorlinkpb.SensorData)) func()
	Done() <-chan struct{}
	Close() error
}

var connectLiteCameraFn = func(target *SelectedDevice) (liteCameraClient, error) {
	p, ok := target.Provider.(*providers.MicroWendyProvider)
	if !ok {
		return nil, fmt.Errorf("selected device has no Wendy Lite provider")
	}
	return p.ConnectSensorLink(*target.External)
}

func isLiteCameraTarget(target *SelectedDevice) bool {
	return target.External != nil && target.External.ProviderKey == "wendy-lite"
}

// The provider's dial does not take a context. Bound the caller's wait and
// close any connection that finishes after cancellation. Once connected,
// cancellation also interrupts pending WendyCom requests.
func openLiteCamera(ctx context.Context, target *SelectedDevice) (liteCameraClient, func(), error) {
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}
	type result struct {
		client liteCameraClient
		err    error
	}
	connect := connectLiteCameraFn
	ready := make(chan result)
	dialCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	go func() {
		client, err := connect(target)
		select {
		case ready <- result{client, err}:
		case <-dialCtx.Done():
			if client != nil {
				client.Close()
			}
		}
	}()
	select {
	case <-dialCtx.Done():
		return nil, nil, dialCtx.Err()
	case r := <-ready:
		if r.err != nil {
			return nil, nil, r.err
		}
		stop := context.AfterFunc(ctx, func() { r.client.Close() })
		return r.client, func() {
			stop()
			r.client.Close()
		}, nil
	}
}

func liteVideoChannels(manifest *sensorlinkpb.SensorManifest) []*sensorlinkpb.SensorDescriptor {
	channels := make([]*sensorlinkpb.SensorDescriptor, 0)
	for _, channel := range manifest.GetSensors() {
		if channel.GetVideo() != nil {
			channels = append(channels, channel)
		}
	}
	return channels
}

func listLiteCameras(cmd *cobra.Command, target *SelectedDevice) error {
	client, closeClient, err := openLiteCamera(cmd.Context(), target)
	if err != nil {
		return err
	}
	defer closeClient()
	manifest, err := client.GetSensorManifest(3 * time.Second)
	if err != nil {
		return fmt.Errorf("listing SensorLink cameras: %w", err)
	}
	channels := liteVideoChannels(manifest)
	if jsonOutput {
		rows := make([]json.RawMessage, 0, len(channels))
		marshal := protojson.MarshalOptions{UseProtoNames: true, EmitUnpopulated: true}
		for _, ch := range channels {
			data, err := marshal.Marshal(ch)
			if err != nil {
				return err
			}
			rows = append(rows, data)
		}
		enc := json.NewEncoder(cmd.OutOrStdout())
		enc.SetIndent("", "  ")
		return enc.Encode(rows)
	}
	if len(channels) == 0 {
		fmt.Fprintln(cmd.OutOrStdout(), "No cameras found.")
		return nil
	}
	var rows [][]string
	for _, ch := range channels {
		v := ch.GetVideo()
		rows = append(rows, []string{
			fmt.Sprint(ch.GetChannelId()), ch.GetName(), v.GetCodec().String(),
			fmt.Sprintf("%dx%d", v.GetWidth(), v.GetHeight()), fmt.Sprint(v.GetFps()),
		})
	}
	fmt.Fprint(cmd.OutOrStdout(), tui.RenderTable([]string{"ID", "Name", "Codec", "Size", "FPS"}, rows))
	return nil
}

type liteCameraOptions struct {
	id                 uint32
	idSet              bool
	stableID           string
	width, height, fps uint32
	stdout, raw        bool
	nonInteractive     bool
}

func selectLiteCamera(manifest *sensorlinkpb.SensorManifest, opts liteCameraOptions, picker cameraPicker) (*sensorlinkpb.SensorDescriptor, error) {
	var candidates []*agentpb.VideoDevice
	channels := make(map[uint32]*sensorlinkpb.SensorDescriptor)
	for _, ch := range liteVideoChannels(manifest) {
		v := ch.GetVideo()
		if opts.idSet && ch.GetChannelId() != opts.id {
			continue
		}
		if (opts.width != 0 && opts.width != v.GetWidth()) ||
			(opts.height != 0 && opts.height != v.GetHeight()) ||
			(opts.fps != 0 && opts.fps != v.GetFps()) {
			continue
		}
		channels[ch.GetChannelId()] = ch
		candidates = append(candidates, &agentpb.VideoDevice{
			Id:   ch.GetChannelId(),
			Name: fmt.Sprintf("%s (%s %dx%d, %d fps)", ch.GetName(), v.GetCodec(), v.GetWidth(), v.GetHeight(), v.GetFps()),
			Path: fmt.Sprintf("SensorLink channel %d", ch.GetChannelId()),
		})
	}
	if len(candidates) == 0 && (opts.idSet || opts.width != 0 || opts.height != 0 || opts.fps != 0) {
		return nil, fmt.Errorf("no SensorLink video channel matches the requested ID or format; see `wendy device camera list`")
	}
	id, err := resolveCameraID(candidates, opts.id, opts.idSet, picker)
	if err != nil {
		return nil, err
	}
	ch := channels[id]
	if ch == nil {
		return nil, fmt.Errorf("SensorLink video channel %d not found", id)
	}
	switch ch.GetVideo().GetCodec() {
	case sensorlinkpb.VideoFormat_MJPEG, sensorlinkpb.VideoFormat_H264:
		return ch, nil
	default:
		return nil, fmt.Errorf("SensorLink channel %d uses unsupported video codec %s", id, ch.GetVideo().GetCodec())
	}
}

func viewLiteCamera(cmd *cobra.Command, target *SelectedDevice, opts liteCameraOptions) error {
	if opts.stableID != "" {
		return fmt.Errorf("Wendy Lite cameras use SensorLink channel IDs; use --id from `wendy device camera list` instead of --stable-id")
	}
	if opts.raw {
		return fmt.Errorf("Wendy Lite SensorLink cameras provide encoded video; --raw is unavailable")
	}
	ctx, cancel := context.WithCancel(cmd.Context())
	defer cancel()
	client, closeClient, err := openLiteCamera(ctx, target)
	if err != nil {
		return err
	}
	defer closeClient()
	manifest, err := client.GetSensorManifest(3 * time.Second)
	if err != nil {
		return fmt.Errorf("listing SensorLink cameras: %w", err)
	}
	var picker cameraPicker
	if !opts.nonInteractive && isInteractiveTerminal() {
		picker = pickCamera
	}
	ch, err := selectLiteCamera(manifest, opts, picker)
	if err != nil {
		return err
	}
	stream, err := subscribeLiteCamera(ctx, client, ch)
	if err != nil {
		return err
	}
	defer stream.Close()
	codec := ch.GetVideo().GetCodec()
	cliLogln("Streaming SensorLink channel %d (%s, %dx%d, %d fps). Ctrl+C to stop.",
		ch.GetChannelId(), codec, ch.GetVideo().GetWidth(), ch.GetVideo().GetHeight(), ch.GetVideo().GetFps())
	if opts.stdout {
		err = stream.writeTo(cmd.OutOrStdout())
	} else {
		// Wait for a frame before launching a viewer, so connection failures
		// are reported even when GStreamer is not installed.
		var first *sensorlink.SensorFrame
		first, err = stream.Recv()
		if err == nil {
			err = playCameraPipeline(ctx, liteCameraPipeline(codec), !opts.nonInteractive, func(w io.Writer) error {
				if _, err := w.Write(first.Payload); err != nil {
					return err
				}
				return stream.writeTo(w)
			})
		}
	}
	if ctx.Err() != nil {
		return nil
	}
	return err
}

func liteCameraPipeline(codec sensorlinkpb.VideoFormat_Codec) []string {
	if codec == sensorlinkpb.VideoFormat_H264 {
		return playbackPipelineArgs(agentpb.VideoCodec_VIDEO_CODEC_H264)
	}
	return []string{
		"fdsrc", "fd=0", "!", "jpegparse",
		"!", "queue", "max-size-buffers=2", "leaky=downstream",
		"!", "jpegdec", "!", "videoconvert",
		"!", "queue", "max-size-buffers=1", "leaky=downstream",
		"!", "autovideosink", "sync=false",
	}
}

type liteCameraStream struct {
	ctx     context.Context
	client  liteCameraClient
	channel uint32
	h264    bool
	frames  chan *sensorlink.SensorFrame
	stop    chan struct{}
	remove  func()
	once    sync.Once
	mu      sync.Mutex
	closed  bool
	asm     sensorlink.Assembler
	// H.264 frames after a gap depend on missing data. Resume at a keyframe.
	waitKeyframe bool
	haveSeq      bool
	seq          uint32
}

func subscribeLiteCamera(ctx context.Context, client liteCameraClient, ch *sensorlinkpb.SensorDescriptor) (*liteCameraStream, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	queueSize := 8
	if ch.GetVideo().GetCodec() == sensorlinkpb.VideoFormat_MJPEG {
		queueSize = 1
	}
	s := &liteCameraStream{
		ctx: ctx, client: client, channel: ch.GetChannelId(),
		h264:   ch.GetVideo().GetCodec() == sensorlinkpb.VideoFormat_H264,
		frames: make(chan *sensorlink.SensorFrame, queueSize), stop: make(chan struct{}),
	}
	s.waitKeyframe = s.h264
	// The board may send data before acknowledging the subscription.
	s.remove = client.AddSensorDataListener(s.deliver)
	if err := client.SensorLinkSubscribe([]uint32{s.channel}, 3*time.Second); err != nil {
		s.Close()
		return nil, fmt.Errorf("subscribing to SensorLink camera: %w", err)
	}
	return s, nil
}

func (s *liteCameraStream) deliver(d *sensorlinkpb.SensorData) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed || d == nil || d.GetChannelId() != s.channel {
		return
	}
	if len(d.GetPayload()) > sensorlink.MaxFrameBytes {
		s.asm = sensorlink.Assembler{}
		s.waitKeyframe = s.h264
		return
	}
	f := s.asm.Add(d)
	if f == nil {
		return
	}
	if s.h264 && s.haveSeq && f.Seq != s.seq+1 {
		s.waitKeyframe = true
	}
	s.haveSeq, s.seq = true, f.Seq
	if s.waitKeyframe && f.Flags&sensorlink.FlagKeyframe == 0 {
		return
	}
	// JPEG frames decode independently. Replace the waiting frame so a slow
	// viewer resumes at the latest image instead of draining stale images.
	if !s.h264 {
		select {
		case <-s.frames:
		default:
		}
	}
	select {
	case s.frames <- f:
		s.waitKeyframe = false
	default:
		// Never block WendyCom's read loop, which also receives command
		// replies. Only complete frames enter this bounded queue.
		s.waitKeyframe = s.h264
	}
}

func (s *liteCameraStream) Recv() (*sensorlink.SensorFrame, error) {
	select {
	case <-s.ctx.Done():
		return nil, s.ctx.Err()
	case <-s.stop:
		return nil, io.EOF
	case <-s.client.Done():
		return nil, fmt.Errorf("SensorLink camera disconnected: %w", io.ErrUnexpectedEOF)
	case f := <-s.frames:
		return f, nil
	}
}

func (s *liteCameraStream) writeTo(w io.Writer) error {
	for {
		f, err := s.Recv()
		if err != nil {
			return err
		}
		if _, err := w.Write(f.Payload); err != nil {
			return fmt.Errorf("writing SensorLink video: %w", err)
		}
	}
}

func (s *liteCameraStream) Close() {
	s.once.Do(func() {
		s.remove()
		s.mu.Lock()
		s.closed = true
		s.asm = sensorlink.Assembler{}
		close(s.stop)
		s.mu.Unlock()
		select {
		case <-s.client.Done():
		default:
			// Disconnecting also releases the subscription if this request fails.
			s.client.SensorLinkUnsubscribe([]uint32{s.channel}, time.Second)
		}
	})
}
