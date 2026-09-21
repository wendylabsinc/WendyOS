//go:build darwin || windows

package commands

import (
	"context"
	"io"
	"testing"
	"time"

	"github.com/ebitengine/oto/v3"
)

func TestAudioRingReaderCancellationUnblocksPlayback(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	reader := &ringReader{ch: make(chan []byte), ctx: ctx}
	result := make(chan error, 1)
	go func() {
		_, err := reader.Read(make([]byte, 16))
		result <- err
	}()
	cancel()
	select {
	case err := <-result:
		if err != io.EOF {
			t.Fatalf("cancelled read = %v, want EOF", err)
		}
	case <-time.After(time.Second):
		t.Fatal("cancelled playback remains blocked waiting for audio")
	}
}

func TestAudioOutputContextReusedForAnotherListen(t *testing.T) {
	// A cached context should be returned without opening a native audio device.
	realtimeAudioOutput.Lock()
	previous, ready := realtimeAudioOutput.ctx, realtimeAudioOutput.ready
	rate, channels := realtimeAudioOutput.sampleRate, realtimeAudioOutput.channels
	cached := &oto.Context{}
	realtimeAudioOutput.ctx = cached
	realtimeAudioOutput.ready = make(chan struct{})
	realtimeAudioOutput.sampleRate, realtimeAudioOutput.channels = 16000, 1
	realtimeAudioOutput.Unlock()
	t.Cleanup(func() {
		realtimeAudioOutput.Lock()
		defer realtimeAudioOutput.Unlock()
		realtimeAudioOutput.ctx, realtimeAudioOutput.ready = previous, ready
		realtimeAudioOutput.sampleRate, realtimeAudioOutput.channels = rate, channels
	})
	for i := 0; i < 2; i++ {
		ctx, _, err := realtimeAudioContext(16000, 1, 150)
		if err != nil || ctx != cached {
			t.Fatalf("listen %d did not reuse the output context: %v", i, err)
		}
	}
	if _, _, err := realtimeAudioContext(48000, 2, 150); err == nil {
		t.Fatal("incompatible audio format should report an error")
	}
}
