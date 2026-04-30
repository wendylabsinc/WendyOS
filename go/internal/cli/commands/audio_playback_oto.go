//go:build darwin || windows

package commands

import (
	"context"
	"fmt"
	"io"
	"time"

	"github.com/ebitengine/oto/v3"
	"github.com/wendylabsinc/wendy/proto/gen/agentpb"
)

// playRealtimeAudio plays the gRPC audio stream through the local speakers.
// Chunks are fed into a small ring buffer so stale data is dropped rather
// than accumulating lag.
func playRealtimeAudio(ctx context.Context, stream interface {
	Recv() (*agentpb.AudioChunk, error)
}, sampleRate, channels uint32) error {
	if sampleRate == 0 {
		sampleRate = 48000
	}
	if channels == 0 {
		channels = 1
	}
	otoCtx, readyCh, err := oto.NewContext(&oto.NewContextOptions{
		SampleRate:   int(sampleRate),
		ChannelCount: int(channels),
		Format:       oto.FormatSignedInt16LE,
		BufferSize:   50 * time.Millisecond,
	})
	if err != nil {
		return fmt.Errorf("initialising audio output: %w", err)
	}
	select {
	case <-readyCh:
	case <-ctx.Done():
		return ctx.Err()
	}

	const ringSize = 4
	ring := make(chan []byte, ringSize)

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

	player := otoCtx.NewPlayer(newRingReader(ring))
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
}

func newRingReader(ch chan []byte) *ringReader { return &ringReader{ch: ch} }

func (r *ringReader) Read(p []byte) (int, error) {
	for len(r.buf) == 0 {
		chunk, ok := <-r.ch
		if !ok {
			return 0, io.EOF
		}
		r.buf = chunk
	}
	n := copy(p, r.buf)
	r.buf = r.buf[n:]
	return n, nil
}
