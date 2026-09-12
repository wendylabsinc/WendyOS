package chat

import (
	"bytes"
	"errors"
	"io"
	"sync/atomic"
	"testing"
	"testing/synctest"
)

type fakeVoiceAudioDevice struct {
	startError error
	closeCount atomic.Int32
	onClose    func()
}

func (d *fakeVoiceAudioDevice) Start() error { return d.startError }
func (d *fakeVoiceAudioDevice) Close() error {
	d.closeCount.Add(1)
	if d.onClose != nil {
		d.onClose()
	}
	return nil
}

func TestVoiceAudioCaptureCopiesAndPreservesSamples(t *testing.T) {
	a := newBufferedVoiceAudio()
	defer a.Close()
	input := []byte{1, 2, 3, 4}
	a.samples(nil, input)
	clear(input)
	output := make([]byte, 3)
	n, err := a.Read(output)
	if err != nil || n != 2 || !bytes.Equal(output[:n], []byte{1, 2}) {
		t.Fatalf("capture did not preserve one whole sample: %v, %v", output[:n], err)
	}
	n, err = a.Read(output)
	if err != nil || n != 2 || !bytes.Equal(output[:n], []byte{3, 4}) {
		t.Fatalf("remaining sample was lost or retained the native buffer: %v, %v", output[:n], err)
	}
}

func TestVoiceAudioCaptureBoundsLatency(t *testing.T) {
	a := newBufferedVoiceAudio()
	defer a.Close()
	older := bytes.Repeat([]byte{1, 2}, voiceBufferBytes/2)
	newer := []byte{3, 4, 5, 6}
	a.samples(nil, older)
	a.samples(nil, newer)
	output := make([]byte, voiceBufferBytes)
	n, err := a.Read(output)
	if err != nil || n != voiceBufferBytes || !bytes.Equal(output[:n-4], older[4:]) || !bytes.Equal(output[n-4:n], newer) {
		t.Fatalf("capture did not drop stale samples: bytes=%d error=%v", n, err)
	}
	oversized := append(bytes.Repeat([]byte{7, 8}, voiceBufferBytes/2), 9, 10)
	a.samples(nil, oversized)
	n, err = a.Read(output)
	if err != nil || n != voiceBufferBytes || !bytes.Equal(output[:n], oversized[2:]) || len(a.capture.data) != voiceBufferBytes {
		t.Fatalf("oversized capture grew the queue or kept old samples: bytes=%d error=%v", n, err)
	}
}

func TestVoiceAudioConvertsNativeEndianWithoutChangingInput(t *testing.T) {
	a := newBufferedVoiceAudio()
	defer a.Close()
	a.nativeBigEndian = true
	input := []byte{0x12, 0x34, 0x56, 0x78}
	a.samples(nil, input)
	wire := make([]byte, len(input))
	if _, err := a.Read(wire); err != nil || !bytes.Equal(wire, []byte{0x34, 0x12, 0x78, 0x56}) {
		t.Fatalf("microphone samples were not converted to little-endian: %v, %v", wire, err)
	}
	if !bytes.Equal(input, []byte{0x12, 0x34, 0x56, 0x78}) {
		t.Fatal("capture conversion modified the native input buffer")
	}
	if _, err := a.Write(wire); err != nil {
		t.Fatal(err)
	}
	output := make([]byte, len(input))
	a.samples(output, nil)
	if !bytes.Equal(output, input) {
		t.Fatalf("speaker samples were not converted to native-endian: %v", output)
	}
}

func TestVoiceAudioPlaybackOrderAndSilence(t *testing.T) {
	a := newBufferedVoiceAudio()
	defer a.Close()
	for _, chunk := range [][]byte{{1, 2}, {3, 4, 5, 6}} {
		if n, err := a.Write(chunk); err != nil || n != len(chunk) {
			t.Fatalf("queue playback: bytes=%d error=%v", n, err)
		}
	}
	output := bytes.Repeat([]byte{0xff}, 8)
	a.samples(output, nil)
	if !bytes.Equal(output, []byte{1, 2, 3, 4, 5, 6, 0, 0}) {
		t.Fatalf("playback reordered samples or replayed stale bytes: %v", output)
	}
	a.samples(output, nil)
	if !bytes.Equal(output, make([]byte, len(output))) {
		t.Fatalf("empty playback did not output silence: %v", output)
	}
}

func TestVoiceAudioPlaybackBackpressure(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		a := newBufferedVoiceAudio()
		defer a.Close()
		first := bytes.Repeat([]byte{1, 2}, voiceBufferBytes/2)
		second := bytes.Repeat([]byte{3, 4}, voiceBufferBytes/2)
		done := make(chan error, 1)
		go func() { _, err := a.Write(append(first, second...)); done <- err }()
		synctest.Wait()
		select {
		case err := <-done:
			t.Fatalf("playback grew instead of waiting for the device: %v", err)
		default:
		}
		if a.playback.size != voiceBufferBytes || len(a.playback.data) != voiceBufferBytes {
			t.Fatal("playback queue exceeded its latency bound")
		}
		output := make([]byte, voiceBufferBytes)
		a.samples(output, nil)
		if !bytes.Equal(output, first) {
			t.Fatal("backpressure reordered the first audio chunk")
		}
		synctest.Wait()
		if err := <-done; err != nil {
			t.Fatal(err)
		}
		a.samples(output, nil)
		if !bytes.Equal(output, second) {
			t.Fatal("backpressure reordered the remaining audio")
		}
	})
}

func TestVoiceAudioFlushCancelsPendingPlayback(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		a := newBufferedVoiceAudio()
		defer a.Close()
		a.samples(nil, []byte{8, 9})
		if _, err := a.Write(make([]byte, voiceBufferBytes)); err != nil {
			t.Fatal(err)
		}
		done := make(chan error, 1)
		go func() { _, err := a.Write([]byte{1, 2}); done <- err }()
		synctest.Wait()
		if err := a.Flush(); err != nil {
			t.Fatal(err)
		}
		synctest.Wait()
		if err := <-done; !errors.Is(err, ErrVoicePlaybackInterrupted) {
			t.Fatalf("interrupted writer was not canceled: %v", err)
		}
		output := []byte{0xff, 0xff}
		a.samples(output, nil)
		if !bytes.Equal(output, []byte{0, 0}) {
			t.Fatalf("audio continued after interruption: %v", output)
		}
		if _, err := a.Write([]byte{3, 4}); err != nil {
			t.Fatal(err)
		}
		a.samples(output, nil)
		if !bytes.Equal(output, []byte{3, 4}) {
			t.Fatal("interruption prevented the next reply from playing")
		}
		if _, err := a.Read(output); err != nil || !bytes.Equal(output, []byte{8, 9}) {
			t.Fatalf("playback interruption discarded microphone input: %v", err)
		}
	})
}

func TestVoiceAudioGuardRejectsPacketStartedAfterFlush(t *testing.T) {
	a := newBufferedVoiceAudio()
	defer a.Close()
	var sessionGeneration atomic.Uint64
	packetGeneration := sessionGeneration.Load()
	stillCurrent := func() bool { return packetGeneration == sessionGeneration.Load() }
	// The session already selected this packet. An interruption wins before
	// the native writer begins and snapshots its own playback generation.
	sessionGeneration.Add(1)
	if err := a.Flush(); err != nil {
		t.Fatal(err)
	}
	if n, err := a.writePlayback([]byte{1, 2}, stillCurrent); n != 0 || !errors.Is(err, ErrVoicePlaybackInterrupted) {
		t.Fatalf("late stale packet reached playback: bytes=%d, error=%v", n, err)
	}
	output := []byte{0xff, 0xff}
	a.samples(output, nil)
	if !bytes.Equal(output, []byte{0, 0}) {
		t.Fatalf("stale audio was audible after interruption: %v", output)
	}
	// Fresh output remains playable after rejecting the old packet.
	packetGeneration = sessionGeneration.Load()
	if n, err := a.writePlayback([]byte{3, 4}, stillCurrent); err != nil || n != 2 {
		t.Fatalf("fresh packet rejected: bytes=%d, error=%v", n, err)
	}
	a.samples(output, nil)
	if !bytes.Equal(output, []byte{3, 4}) {
		t.Fatalf("fresh audio lost after interruption: %v", output)
	}
}

func TestVoiceAudioGuardRechecksAfterBackpressure(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		a := newBufferedVoiceAudio()
		defer a.Close()
		var sessionGeneration atomic.Uint64
		packetGeneration := sessionGeneration.Load()
		stillCurrent := func() bool { return packetGeneration == sessionGeneration.Load() }
		done := make(chan error, 1)
		go func() {
			_, err := a.writePlayback(bytes.Repeat([]byte{1, 2}, voiceBufferBytes), stillCurrent)
			done <- err
		}()
		synctest.Wait()
		select {
		case err := <-done:
			t.Fatalf("write did not wait for playback: %v", err)
		default:
		}
		// Invalidate while the writer waits. Simulate the next device callback
		// before Flush acquires the queue lock; no stale tail may be enqueued.
		sessionGeneration.Add(1)
		a.samples(make([]byte, voiceBufferBytes), nil)
		synctest.Wait()
		if err := <-done; !errors.Is(err, ErrVoicePlaybackInterrupted) {
			t.Fatalf("stale writer resumed after backpressure: %v", err)
		}
		output := []byte{0xff, 0xff}
		a.samples(output, nil)
		if !bytes.Equal(output, []byte{0, 0}) {
			t.Fatalf("stale tail reached playback: %v", output)
		}
	})
}

func TestVoiceAudioCloseUnblocksIOAndReleasesDevice(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		device := &fakeVoiceAudioDevice{}
		var buffers *bufferedVoiceAudio
		audio, err := startVoiceAudio(func(a *bufferedVoiceAudio) (voiceAudioDevice, error) { buffers = a; return device, nil })
		if err != nil {
			t.Fatal(err)
		}
		// Teardown can wait for a final native callback; Close must not hold
		// the buffer lock while waiting, or retain input from that callback.
		device.onClose = func() { buffers.samples(make([]byte, 2), []byte{9, 9}) }
		readDone, writeDone := make(chan error, 1), make(chan error, 1)
		go func() { _, err := audio.Read(make([]byte, 2)); readDone <- err }()
		go func() { _, err := audio.Write(make([]byte, voiceBufferBytes+2)); writeDone <- err }()
		synctest.Wait()
		if err := audio.Close(); err != nil {
			t.Fatal(err)
		}
		if err := audio.Close(); err != nil {
			t.Fatal(err)
		}
		synctest.Wait()
		if err := <-readDone; !errors.Is(err, io.EOF) {
			t.Fatalf("closing voice did not release microphone reader: %v", err)
		}
		if err := <-writeDone; !errors.Is(err, io.ErrClosedPipe) {
			t.Fatalf("closing voice did not release speaker writer: %v", err)
		}
		if device.closeCount.Load() != 1 || buffers.capture.size != 0 || buffers.playback.size != 0 {
			t.Fatal("closing voice retained audio or closed the device more than once")
		}
		if err := audio.Flush(); !errors.Is(err, io.ErrClosedPipe) {
			t.Fatalf("closed device accepted playback flush: %v", err)
		}
	})
}

func TestVoiceAudioStartFailureReleasesDevice(t *testing.T) {
	startError := errors.New("microphone access denied")
	device := &fakeVoiceAudioDevice{startError: startError}
	audio, err := startVoiceAudio(func(a *bufferedVoiceAudio) (voiceAudioDevice, error) { return device, nil })
	if audio != nil || !errors.Is(err, startError) || device.closeCount.Load() != 1 {
		t.Fatalf("failed startup leaked the device or lost its error: %v", err)
	}
}

func TestVoiceAudioDeviceFailureWakesReaderAndCloses(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		device := &fakeVoiceAudioDevice{}
		audio, err := startVoiceAudio(func(a *bufferedVoiceAudio) (voiceAudioDevice, error) { return device, nil })
		if err != nil {
			t.Fatal(err)
		}
		done := make(chan error, 1)
		go func() { _, err := audio.Read(make([]byte, 2)); done <- err }()
		synctest.Wait()
		audio.(*bufferedVoiceAudio).deviceStopped()
		synctest.Wait()
		if err := <-done; err == nil || errors.Is(err, io.EOF) {
			t.Fatalf("microphone disconnect was not reported: %v", err)
		}
		if device.closeCount.Load() != 1 {
			t.Fatal("microphone disconnect retained native resources")
		}
		_ = audio.Close()
	})
}
