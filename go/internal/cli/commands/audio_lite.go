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
	"github.com/wendylabsinc/wendy/go/internal/cli/tui"
	agentpb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
	sensorlinkpb "github.com/wendylabsinc/wendy/go/proto/gen/sensorlinkpb"
	"google.golang.org/protobuf/encoding/protojson"
)

type liteAudioOptions struct {
	id, sampleRate, channels, bufferMs  uint32
	idSet, rateSet, channelsSet, stdout bool
}

func liteAudioChannels(manifest *sensorlinkpb.SensorManifest) []*sensorlinkpb.SensorDescriptor {
	var channels []*sensorlinkpb.SensorDescriptor
	for _, ch := range manifest.GetSensors() {
		if ch.GetAudio() != nil {
			channels = append(channels, ch)
		}
	}
	return channels
}

func listLiteAudio(cmd *cobra.Command, target *SelectedDevice) error {
	// Camera and audio use the same authenticated SensorLink transport.
	client, closeClient, err := openLiteCamera(cmd.Context(), target)
	if err != nil {
		return err
	}
	defer closeClient()
	manifest, err := client.GetSensorManifest(3 * time.Second)
	if err != nil {
		return fmt.Errorf("listing SensorLink microphones: %w", err)
	}
	channels := liteAudioChannels(manifest)
	if jsonOutput {
		rows := make([]json.RawMessage, 0, len(channels))
		marshal := protojson.MarshalOptions{UseProtoNames: true, EmitUnpopulated: true}
		for _, ch := range channels {
			b, err := marshal.Marshal(ch)
			if err != nil {
				return err
			}
			rows = append(rows, b)
		}
		enc := json.NewEncoder(cmd.OutOrStdout())
		enc.SetIndent("", "  ")
		return enc.Encode(rows)
	}
	if len(channels) == 0 {
		fmt.Fprintln(cmd.OutOrStdout(), "No microphones found.")
		return nil
	}
	var rows [][]string
	for _, ch := range channels {
		a := ch.GetAudio()
		rows = append(rows, []string{fmt.Sprint(ch.GetChannelId()), ch.GetName(), a.GetCodec().String(), fmt.Sprint(a.GetSampleRate()), fmt.Sprint(a.GetChannels())})
	}
	fmt.Fprint(cmd.OutOrStdout(), tui.RenderTable([]string{"ID", "Name", "Codec", "Sample rate", "Channels"}, rows))
	return nil
}

func selectLiteAudio(manifest *sensorlinkpb.SensorManifest, opts liteAudioOptions) (*sensorlinkpb.SensorDescriptor, error) {
	for _, ch := range liteAudioChannels(manifest) {
		if opts.idSet && ch.GetChannelId() != opts.id {
			continue
		}
		a := ch.GetAudio()
		if a.GetCodec() != sensorlinkpb.AudioFormat_PCM_S16LE || a.GetSampleRate() == 0 || a.GetSampleRate() > 384000 || a.GetChannels() == 0 || a.GetChannels() > 8 {
			if opts.idSet {
				return nil, fmt.Errorf("SensorLink audio channel %d has an unsupported format: %s, %d Hz, %d channels", ch.GetChannelId(), a.GetCodec(), a.GetSampleRate(), a.GetChannels())
			}
			continue
		}
		if (opts.rateSet && opts.sampleRate != a.GetSampleRate()) || (opts.channelsSet && opts.channels != a.GetChannels()) {
			continue
		}
		return ch, nil
	}
	return nil, fmt.Errorf("no PCM S16LE SensorLink microphone matches the requested ID or format; see `wendy device audio list`")
}

func listenLiteAudio(cmd *cobra.Command, target *SelectedDevice, opts liteAudioOptions) error {
	ctx := cmd.Context()
	client, closeClient, err := openLiteCamera(ctx, target)
	if err != nil {
		return err
	}
	defer closeClient()
	manifest, err := client.GetSensorManifest(3 * time.Second)
	if err != nil {
		return fmt.Errorf("listing SensorLink microphones: %w", err)
	}
	ch, err := selectLiteAudio(manifest, opts)
	if err != nil {
		return err
	}
	stream, err := subscribeLiteAudio(ctx, client, ch)
	if err != nil {
		return err
	}
	defer stream.Close()
	a := ch.GetAudio()
	fmt.Fprintf(cmd.ErrOrStderr(), "Streaming %s, SensorLink channel %d (%d Hz, %d channels, PCM S16LE). Ctrl+C to stop.\n", ch.GetName(), ch.GetChannelId(), a.GetSampleRate(), a.GetChannels())
	if !opts.stdout {
		err = playRealtimeAudio(ctx, stream, a.GetSampleRate(), a.GetChannels(), opts.bufferMs)
	} else {
		for {
			var chunk *agentpb.AudioChunk
			chunk, err = stream.Recv()
			if err != nil {
				break
			}
			var n int
			n, err = cmd.OutOrStdout().Write(chunk.GetPcmData())
			if err == nil && n != len(chunk.GetPcmData()) {
				err = io.ErrShortWrite
			}
			if err != nil {
				break
			}
		}
	}
	if ctx.Err() != nil {
		return nil
	}
	return err
}

type liteAudioStream struct {
	ctx                           context.Context
	client                        liteCameraClient
	channel, sampleRate, channels uint32
	frames                        chan *agentpb.AudioChunk
	stop                          chan struct{}
	remove                        func()
	once                          sync.Once
	mu                            sync.Mutex
	closed                        bool
	asm                           sensorlink.Assembler
}

func subscribeLiteAudio(ctx context.Context, client liteCameraClient, ch *sensorlinkpb.SensorDescriptor) (*liteAudioStream, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	a := ch.GetAudio()
	s := &liteAudioStream{ctx: ctx, client: client, channel: ch.GetChannelId(), sampleRate: a.GetSampleRate(), channels: a.GetChannels(), frames: make(chan *agentpb.AudioChunk, 8), stop: make(chan struct{})}
	s.remove = client.AddSensorDataListener(s.deliver)
	if err := client.SensorLinkSubscribe([]uint32{s.channel}, 3*time.Second); err != nil {
		s.Close()
		return nil, fmt.Errorf("subscribing to SensorLink microphone: %w", err)
	}
	return s, nil
}

func (s *liteAudioStream) deliver(d *sensorlinkpb.SensorData) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed || d == nil || d.GetChannelId() != s.channel {
		return
	}
	// Bound malformed input before assembly. The assembler also bounds the total frame.
	if len(d.GetPayload()) > sensorlink.MaxFrameBytes {
		s.asm = sensorlink.Assembler{}
		return
	}
	f := s.asm.Add(d)
	if f == nil || len(f.Payload) == 0 || len(f.Payload)%(2*int(s.channels)) != 0 {
		return
	}
	chunk := &agentpb.AudioChunk{PcmData: f.Payload, TimestampNs: f.TsUs * 1000, SampleRate: s.sampleRate, Channels: s.channels}
	select {
	case s.frames <- chunk:
	default:
		// Keep latency bounded without blocking WendyCom's command replies.
		select {
		case <-s.frames:
		default:
		}
		s.frames <- chunk
	}
}

func (s *liteAudioStream) Recv() (*agentpb.AudioChunk, error) {
	select {
	case <-s.ctx.Done():
		return nil, s.ctx.Err()
	case <-s.stop:
		return nil, io.EOF
	case <-s.client.Done():
		return nil, fmt.Errorf("SensorLink microphone disconnected: %w", io.ErrUnexpectedEOF)
	case chunk := <-s.frames:
		return chunk, nil
	}
}

func (s *liteAudioStream) Close() {
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
			s.client.SensorLinkUnsubscribe([]uint32{s.channel}, time.Second)
		}
	})
}
