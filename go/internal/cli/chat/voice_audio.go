package chat

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"sync"
)

const (
	voiceSampleRate = 24000
	voiceSampleSize = 2
	// Keep at most half a second of audio in each native queue. Capture drops
	// stale samples when the network stalls; playback applies backpressure.
	voiceBufferBytes = voiceSampleRate * voiceSampleSize / 2
)

var ErrVoicePlaybackInterrupted = errors.New("voice playback interrupted")

// VoiceAudio streams mono, signed 16-bit little-endian PCM at 24 kHz through the
// microphone and speakers on the computer running Wendy. Close releases both
// devices and unblocks pending reads/writes. Flush drops queued playback and
// interrupts pending writes with ErrVoicePlaybackInterrupted.
type VoiceAudio interface {
	Read([]byte) (int, error)
	Write([]byte) (int, error)
	Flush() error
	Close() error
}

// voicePlaybackWriter lets the live session reject a stale packet even when an
// interruption occurs just before Write begins. stillCurrent must not block;
// it is checked while holding the native playback queue mutex.
type voicePlaybackWriter interface {
	writePlayback(p []byte, stillCurrent func() bool) (int, error)
}

type voiceAudioDevice interface {
	Start() error
	Close() error
}

type bufferedVoiceAudio struct {
	mu                 sync.Mutex
	writeMu            sync.Mutex
	captureReady       *sync.Cond
	playbackReady      *sync.Cond
	capture            voicePCMQueue
	playback           voicePCMQueue
	playbackGeneration uint64
	nativeBigEndian    bool
	closed             bool
	readError          error
	device             voiceAudioDevice
	closeOnce          sync.Once
	closeError         error
}

func newBufferedVoiceAudio() *bufferedVoiceAudio {
	a := &bufferedVoiceAudio{
		capture:         voicePCMQueue{data: make([]byte, voiceBufferBytes)},
		playback:        voicePCMQueue{data: make([]byte, voiceBufferBytes)},
		nativeBigEndian: binary.NativeEndian.Uint16([]byte{1, 0}) != 1,
	}
	a.captureReady = sync.NewCond(&a.mu)
	a.playbackReady = sync.NewCond(&a.mu)
	return a
}

// startVoiceAudio keeps device setup injectable, so tests exercise cleanup and
// native callbacks without opening the microphone or speakers.
func startVoiceAudio(open func(*bufferedVoiceAudio) (voiceAudioDevice, error)) (VoiceAudio, error) {
	a := newBufferedVoiceAudio()
	device, err := open(a)
	if err != nil {
		_ = a.Close()
		return nil, err
	}
	a.device = device
	if err := device.Start(); err != nil {
		_ = a.Close()
		return nil, fmt.Errorf("start microphone and speakers: %w; check your terminal's microphone permission and default audio devices", err)
	}
	return a, nil
}

func (a *bufferedVoiceAudio) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	// Read whole samples so dropping old capture data cannot shift PCM frames.
	p = p[:len(p)&^1]
	if len(p) == 0 {
		return 0, fmt.Errorf("voice audio reads need room for a 16-bit sample")
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	for a.capture.size == 0 && !a.closed {
		a.captureReady.Wait()
	}
	if a.closed {
		if a.readError != nil {
			return 0, a.readError
		}
		return 0, io.EOF
	}
	return a.capture.read(p), nil
}

func (a *bufferedVoiceAudio) Write(p []byte) (int, error) {
	return a.writePlayback(p, nil)
}

func (a *bufferedVoiceAudio) writePlayback(p []byte, stillCurrent func() bool) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	if len(p)%voiceSampleSize != 0 {
		return 0, fmt.Errorf("voice playback requires complete 16-bit samples")
	}
	a.mu.Lock()
	generation := a.playbackGeneration
	a.mu.Unlock()
	// Concurrent writes remain contiguous in the speaker queue. Take the
	// generation first so Flush also cancels writers waiting for this mutex.
	a.writeMu.Lock()
	defer a.writeMu.Unlock()
	a.mu.Lock()
	defer a.mu.Unlock()
	written := 0
	for written < len(p) {
		if a.closed {
			return written, io.ErrClosedPipe
		}
		if generation != a.playbackGeneration || (stillCurrent != nil && !stillCurrent()) {
			return written, ErrVoicePlaybackInterrupted
		}
		if a.playback.size == len(a.playback.data) {
			a.playbackReady.Wait()
			continue
		}
		written += a.playback.write(p[written:])
	}
	return written, nil
}

func (a *bufferedVoiceAudio) Flush() error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.closed {
		return io.ErrClosedPipe
	}
	a.playback.reset()
	a.playbackGeneration++
	a.playbackReady.Broadcast()
	return nil
}

func (a *bufferedVoiceAudio) Close() error {
	a.closeOnce.Do(func() {
		a.mu.Lock()
		a.closeBuffersLocked()
		a.mu.Unlock()
		// Device shutdown waits for any native callback to finish. Never hold
		// the buffer mutex while waiting, or the callback could deadlock.
		if a.device != nil {
			a.closeError = a.device.Close()
		}
	})
	return a.closeError
}

func (a *bufferedVoiceAudio) closeBuffersLocked() {
	a.closed = true
	a.capture.reset()
	a.playback.reset()
	a.captureReady.Broadcast()
	a.playbackReady.Broadcast()
}

// samples is called only by the native audio callback. It never waits for
// capture consumers or playback producers, and allocates no audio buffers.
func (a *bufferedVoiceAudio) samples(output, input []byte) {
	a.mu.Lock()
	defer a.mu.Unlock()
	clear(output)
	if a.closed {
		return
	}
	if len(input) > 0 {
		input = input[:len(input)&^1]
		if len(input) > len(a.capture.data) {
			input = input[len(input)-len(a.capture.data):]
		}
		if excess := a.capture.size + len(input) - len(a.capture.data); excess > 0 {
			a.capture.discard(excess)
		}
		start := (a.capture.head + a.capture.size) % len(a.capture.data)
		a.capture.write(input)
		// miniaudio uses native-endian samples; the voice protocol is always
		// little-endian. Swap our copy without modifying the native input.
		if a.nativeBigEndian {
			for i := 0; i < len(input); i += voiceSampleSize {
				pos := (start + i) % len(a.capture.data)
				a.capture.data[pos], a.capture.data[pos+1] = a.capture.data[pos+1], a.capture.data[pos]
			}
		}
		a.captureReady.Broadcast()
	}
	if a.playback.read(output[:len(output)&^1]) > 0 {
		a.playbackReady.Broadcast()
	}
	if a.nativeBigEndian {
		for i := 0; i+1 < len(output); i += voiceSampleSize {
			output[i], output[i+1] = output[i+1], output[i]
		}
	}
}

// A device can stop after an unplug or permission change. Wake the session's
// reader immediately and finish cleanup outside the native callback thread.
func (a *bufferedVoiceAudio) deviceStopped() {
	a.mu.Lock()
	if a.closed {
		a.mu.Unlock()
		return
	}
	a.readError = errors.New("voice audio device stopped; check your microphone and speakers, then enable voice again")
	a.closeBuffersLocked()
	a.mu.Unlock()
	go func() { _ = a.Close() }()
}

// voicePCMQueue is a fixed-size byte ring; its owner holds the buffer mutex.
type voicePCMQueue struct {
	data []byte
	head int
	size int
}

func (q *voicePCMQueue) write(p []byte) int {
	n := min(len(p), len(q.data)-q.size)
	tail := (q.head + q.size) % len(q.data)
	first := copy(q.data[tail:], p[:n])
	copy(q.data, p[first:n])
	q.size += n
	return n
}

func (q *voicePCMQueue) read(p []byte) int {
	n := min(len(p), q.size)
	first := copy(p[:n], q.data[q.head:])
	copy(p[first:n], q.data[:n-first])
	q.discard(n)
	return n
}

func (q *voicePCMQueue) discard(n int) {
	q.head = (q.head + n) % len(q.data)
	q.size -= n
}

func (q *voicePCMQueue) reset() {
	clear(q.data)
	q.head, q.size = 0, 0
}
