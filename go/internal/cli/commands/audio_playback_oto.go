//go:build darwin || windows

package commands

import (
	"context"
	"fmt"
	"io"
	"sync"
	"time"

	"github.com/ebitengine/oto/v3"
	"github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
)

const realtimeAudioAvailable = true

// Oto permits one context per process. Reuse it when the audio picker starts
// another listening session after the previous player has been closed.
var realtimeAudioOutput struct {
	sync.Mutex
	ctx        *oto.Context
	ready      chan struct{}
	sampleRate uint32
	channels   uint32
}

func realtimeAudioContext(sampleRate, channels, bufferMs uint32) (*oto.Context, chan struct{}, error) {
	realtimeAudioOutput.Lock()
	defer realtimeAudioOutput.Unlock()
	if realtimeAudioOutput.ctx != nil {
		if realtimeAudioOutput.sampleRate != sampleRate || realtimeAudioOutput.channels != channels {
			return nil, nil, fmt.Errorf("audio output is already configured for %d Hz, %d channels", realtimeAudioOutput.sampleRate, realtimeAudioOutput.channels)
		}
		return realtimeAudioOutput.ctx, realtimeAudioOutput.ready, nil
	}
	ctx, ready, err := oto.NewContext(&oto.NewContextOptions{
		SampleRate:   int(sampleRate),
		ChannelCount: int(channels),
		Format:       oto.FormatSignedInt16LE,
		BufferSize:   time.Duration(bufferMs) * time.Millisecond,
	})
	if err != nil {
		return nil, nil, err
	}
	realtimeAudioOutput.ctx = ctx
	realtimeAudioOutput.ready = ready
	realtimeAudioOutput.sampleRate = sampleRate
	realtimeAudioOutput.channels = channels
	return ctx, ready, nil
}

// playRealtimeAudio plays the gRPC audio stream through the local speakers.
// Chunks are fed into a small jitter buffer so stale data is dropped rather
// than accumulating lag. bufferMs sets the target playback latency: it sizes
// both the oto output buffer and the jitter-buffer depth. Lower values reduce
// latency but make playback more prone to dropouts on a jittery link.
func playRealtimeAudio(ctx context.Context, stream interface {
	Recv() (*agentpb.AudioChunk, error)
}, sampleRate, channels, bufferMs uint32) error {
	if sampleRate == 0 {
		sampleRate = 48000
	}
	if channels == 0 {
		channels = 1
	}
	if bufferMs < minBufferMs {
		bufferMs = minBufferMs
	}
	otoCtx, readyCh, err := realtimeAudioContext(sampleRate, channels, bufferMs)
	if err != nil {
		return fmt.Errorf("initialising audio output: %w", err)
	}
	select {
	case <-readyCh:
	case <-ctx.Done():
		return ctx.Err()
	}

	ring := make(chan []byte, jitterDepthChunks(bufferMs, defaultAgentChunkMs))

	recvErr := make(chan error, 1)
	go func() {
		defer close(ring)
		for {
			chunk, err := stream.Recv()
			if err != nil {
				recvErr <- err
				return
			}
			data := make([]byte, len(chunk.GetPcmData()))
			copy(data, chunk.GetPcmData())
			// Evict oldest chunk when ring is full so we stay current.
			for {
				select {
				case ring <- data:
					goto sent
				default:
					select {
					case <-ring:
					default:
					}
				}
			}
		sent:
		}
	}()

	player := otoCtx.NewPlayer(&ringReader{ch: ring, ctx: ctx})
	player.Play()
	defer player.Close()

	select {
	case err := <-recvErr:
		if err != nil && err != io.EOF {
			return fmt.Errorf("receiving audio: %w", err)
		}
	case <-ctx.Done():
	}
	return nil
}

// ringReader adapts a channel of PCM byte slices into an io.Reader for oto.
type ringReader struct {
	ch  chan []byte
	buf []byte
	ctx context.Context
}

func newRingReader(ch chan []byte) *ringReader { return &ringReader{ch: ch, ctx: context.Background()} }

func (r *ringReader) Read(p []byte) (int, error) {
	for len(r.buf) == 0 {
		select {
		case <-r.ctx.Done():
			return 0, io.EOF
		case chunk, ok := <-r.ch:
			if !ok {
				return 0, io.EOF
			}
			r.buf = chunk
		}
	}
	n := copy(p, r.buf)
	r.buf = r.buf[n:]
	return n, nil
}
