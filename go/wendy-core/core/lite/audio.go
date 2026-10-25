package lite

import (
	"context"
	"fmt"
	"io"
	"sync"
	"time"

	sensorlinkpb "github.com/wendylabsinc/WendyOS/go/wendy-core/core/lite/pb"
)

// AudioClient supplies WendyCom sensor operations for an audio subscription.
type AudioClient interface {
	AddSensorDataListener(func(*sensorlinkpb.SensorData)) func()
	SensorLinkSubscribe([]uint32, time.Duration) error
	SensorLinkUnsubscribe([]uint32, time.Duration) error
	Done() <-chan struct{}
}

// AudioChunk is a complete PCM S16LE frame; its bytes belong to the receiver.
type AudioChunk struct {
	PcmData              []byte
	TimestampNs          uint64
	SampleRate, Channels uint32
}

// AudioOptions selects a microphone by optional ID and exact native format.
type AudioOptions struct {
	ID, SampleRate, Channels    uint32
	IDSet, RateSet, ChannelsSet bool
}

// AudioChannels returns the manifest entries describing microphones.
func AudioChannels(manifest *sensorlinkpb.SensorManifest) []*sensorlinkpb.SensorDescriptor {
	var channels []*sensorlinkpb.SensorDescriptor
	for _, ch := range manifest.GetSensors() {
		if ch.GetAudio() != nil {
			channels = append(channels, ch)
		}
	}
	return channels
}

// SelectAudio chooses the first PCM S16LE microphone matching the request.
func SelectAudio(manifest *sensorlinkpb.SensorManifest, opts AudioOptions) (*sensorlinkpb.SensorDescriptor, error) {
	for _, ch := range AudioChannels(manifest) {
		if opts.IDSet && ch.GetChannelId() != opts.ID {
			continue
		}
		a := ch.GetAudio()
		if a.GetCodec() != sensorlinkpb.AudioFormat_PCM_S16LE || a.GetSampleRate() == 0 || a.GetSampleRate() > 384000 || a.GetChannels() == 0 || a.GetChannels() > 8 {
			if opts.IDSet {
				return nil, fmt.Errorf("SensorLink audio channel %d has an unsupported format: %s, %d Hz, %d channels", ch.GetChannelId(), a.GetCodec(), a.GetSampleRate(), a.GetChannels())
			}
			continue
		}
		if (opts.RateSet && opts.SampleRate != a.GetSampleRate()) || (opts.ChannelsSet && opts.Channels != a.GetChannels()) {
			continue
		}
		return ch, nil
	}
	return nil, fmt.Errorf("no PCM S16LE SensorLink microphone matches the requested ID or format; see `wendy device audio list`")
}

// AudioStream reassembles microphone frames with a bounded latency queue.
type AudioStream struct {
	ctx                           context.Context
	client                        AudioClient
	channel, SampleRate, Channels uint32
	frames                        chan *AudioChunk
	stop                          chan struct{}
	remove                        func()
	once                          sync.Once
	mu                            sync.Mutex
	closed                        bool
	asm                           Assembler
}

// SubscribeAudio starts a microphone subscription; Close releases it.
func SubscribeAudio(ctx context.Context, client AudioClient, ch *sensorlinkpb.SensorDescriptor) (*AudioStream, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	a := ch.GetAudio()
	s := &AudioStream{ctx: ctx, client: client, channel: ch.GetChannelId(), SampleRate: a.GetSampleRate(), Channels: a.GetChannels(), frames: make(chan *AudioChunk, 8), stop: make(chan struct{})}
	s.remove = client.AddSensorDataListener(s.deliver)
	if err := client.SensorLinkSubscribe([]uint32{s.channel}, 3*time.Second); err != nil {
		s.Close()
		return nil, fmt.Errorf("subscribing to SensorLink microphone: %w", err)
	}
	return s, nil
}

func (s *AudioStream) deliver(d *sensorlinkpb.SensorData) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed || d == nil || d.GetChannelId() != s.channel {
		return
	}
	// Bound malformed input before assembly. The assembler also bounds the total frame.
	if len(d.GetPayload()) > MaxFrameBytes {
		s.asm = Assembler{}
		return
	}
	f := s.asm.Add(d)
	if f == nil || len(f.Payload) == 0 || len(f.Payload)%(2*int(s.Channels)) != 0 {
		return
	}
	chunk := &AudioChunk{PcmData: f.Payload, TimestampNs: f.TsUs * 1000, SampleRate: s.SampleRate, Channels: s.Channels}
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

func (s *AudioStream) Recv() (*AudioChunk, error) {
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

func (s *AudioStream) Close() {
	s.once.Do(func() {
		s.remove()
		s.mu.Lock()
		s.closed = true
		s.asm = Assembler{}
		close(s.stop)
		s.mu.Unlock()
		select {
		case <-s.client.Done():
		default:
			s.client.SensorLinkUnsubscribe([]uint32{s.channel}, time.Second)
		}
	})
}
