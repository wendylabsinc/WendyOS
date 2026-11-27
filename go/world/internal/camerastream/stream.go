// Package camerastream adapts the authenticated Wendy Lite camera command to
// bounded original JPEG receipts. It does not assign camera exposure times.
package camerastream

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

const MaxFrameBytes = 8 << 20

// Some camera buffers include zero padding after EOI. Bound discarded padding
// independently of the JPEG size so malformed streams cannot scan indefinitely.
const maxFramePaddingBytes = 64 << 10

type Config struct {
	WendyBinary string
	Device      string
	CameraID    uint32
	// EmulatedSource is an explicit executable emitting the actual emulator's
	// received MJPEG. It receives no shell or CLI arguments. The caller must
	// retain simulated provenance for every resulting receipt.
	EmulatedSource string
	SimulationRun  string
	// Zero values use 500 ms sampling, 60 seconds for the first complete JPEG,
	// a 10 second interframe watchdog and 2 second reconnect delay.
	// Sampling drops intermediate frames, never queues them.
	SampleInterval time.Duration
	StartupTimeout time.Duration
	FrameTimeout   time.Duration
	ReconnectDelay time.Duration
	command        func(context.Context, string, ...string) *exec.Cmd
}

type frame struct {
	data []byte
	seq  uint64
}

type callbackError struct{ error }

// Run receives original MJPEG bytes from one explicitly selected Wendy Lite
// camera. Sequence is this process's stream index, not the sensor's sequence.
// A fresh random session identifies every CLI connection. Callbacks are serial.
// Stream failures reconnect; configuration and persistence callback failures
// return. Cancellation kills and reaps the owned CLI process before returning.
func Run(ctx context.Context, cfg Config, onFrame func([]byte, string, uint64) error, onStatus func(string, string)) error {
	if !strings.HasPrefix(cfg.Device, "wendy-lite:") || len(cfg.Device) <= len("wendy-lite:") || len(cfg.Device) > 2048 || strings.ContainsAny(cfg.Device, "\x00\r\n") || onFrame == nil {
		return errors.New("an explicit Wendy Lite camera and frame callback are required")
	}
	if cfg.EmulatedSource != "" && (strings.TrimSpace(cfg.SimulationRun) == "" || strings.ContainsAny(cfg.EmulatedSource, "\x00\r\n")) {
		return errors.New("emulated camera source requires an explicit simulation run")
	}
	if cfg.WendyBinary == "" {
		cfg.WendyBinary = "wendy"
	}
	if cfg.SampleInterval == 0 {
		cfg.SampleInterval = 500 * time.Millisecond
	}
	if cfg.FrameTimeout == 0 {
		cfg.FrameTimeout = 10 * time.Second
	}
	if cfg.StartupTimeout == 0 {
		cfg.StartupTimeout = time.Minute
	}
	if cfg.ReconnectDelay == 0 {
		cfg.ReconnectDelay = 2 * time.Second
	}
	if cfg.SampleInterval < time.Millisecond || cfg.SampleInterval > time.Minute || cfg.StartupTimeout < time.Millisecond || cfg.StartupTimeout > time.Minute || cfg.FrameTimeout < time.Millisecond || cfg.FrameTimeout > time.Minute || cfg.ReconnectDelay < time.Millisecond || cfg.ReconnectDelay > time.Minute {
		return errors.New("invalid camera stream timing")
	}
	if cfg.command == nil {
		binary := cfg.WendyBinary
		if cfg.EmulatedSource != "" {
			binary = cfg.EmulatedSource
		}
		path, err := exec.LookPath(binary)
		if err != nil {
			return errors.New("camera capture executable is unavailable")
		}
		if cfg.EmulatedSource != "" {
			cfg.EmulatedSource = path
		} else {
			cfg.WendyBinary = path
		}
		cfg.command = exec.CommandContext
	}
	status := func(state, detail string) {
		if onStatus != nil {
			onStatus(state, detail)
		}
	}
	defer status("stopped", "Camera capture stopped")
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		var random [16]byte
		if _, err := rand.Read(random[:]); err != nil {
			return err
		}
		if cfg.EmulatedSource != "" {
			status("connecting", "Connecting to emulated firmware")
		} else {
			status("connecting", "Connecting through Wendy")
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		err := receive(ctx, cfg, hex.EncodeToString(random[:]), onFrame)
		if ctx.Err() != nil {
			return ctx.Err()
		}
		var callback callbackError
		if errors.As(err, &callback) {
			return callback.error
		}
		status("disconnected", "Camera stream unavailable; reconnecting")
		timer := time.NewTimer(cfg.ReconnectDelay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
}

func receive(parent context.Context, cfg Config, session string, onFrame func([]byte, string, uint64) error) error {
	ctx, cancel := context.WithCancel(parent)
	defer cancel()
	var cmd *exec.Cmd
	if cfg.EmulatedSource != "" {
		cmd = cfg.command(ctx, cfg.EmulatedSource)
	} else {
		cmd = cfg.command(ctx, cfg.WendyBinary, "device", "camera", "view", "--device", cfg.Device, "--id", strconv.FormatUint(uint64(cfg.CameraID), 10), "--stdout", "--non-interactive")
	}
	// No credentials or device errors are copied into World/UI diagnostics.
	cmd.Stderr = io.Discard
	cmd.WaitDelay = 2 * time.Second
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	if err = cmd.Start(); err != nil {
		stdout.Close()
		return err
	}
	frames := make(chan frame, 1)
	done := make(chan error, 1)
	parserExited := make(chan struct{})
	started := time.Now()
	var lastComplete atomic.Int64
	go func() {
		defer close(parserExited)
		reader := bufio.NewReaderSize(stdout, 64<<10)
		for seq := uint64(1); ; seq++ {
			data, err := readJPEG(reader, MaxFrameBytes)
			if err != nil {
				done <- err
				return
			}
			lastComplete.Store(time.Since(started).Nanoseconds())
			// Replacement bounds backpressure to one complete latest frame.
			select {
			case <-frames:
			default:
			}
			select {
			case frames <- frame{data, seq}:
			case <-ctx.Done():
				done <- ctx.Err()
				return
			}
		}
	}()
	defer func() {
		cancel()
		stdout.Close()
		<-parserExited
		_ = cmd.Wait()
	}()
	watch := time.NewTicker(min(cfg.StartupTimeout/2, cfg.FrameTimeout/2, time.Second))
	defer watch.Stop()
	var lastAccepted time.Time
	for {
		select {
		case <-parent.Done():
			return parent.Err()
		case <-watch.C:
			last := time.Duration(lastComplete.Load())
			timeout := cfg.FrameTimeout
			if last == 0 {
				timeout = cfg.StartupTimeout
			}
			if time.Since(started)-last >= timeout {
				return errors.New("camera frame timeout")
			}
		case err := <-done:
			// Complete buffered frames from a disconnected source are historical,
			// not fresh arrivals. Discard them instead of draining after EOF.
			return err
		case f := <-frames:
			select {
			case err := <-done:
				return err
			default:
			}
			if lastAccepted.IsZero() || time.Since(lastAccepted) >= cfg.SampleInterval {
				if err := onFrame(f.data, session, f.seq); err != nil {
					return callbackError{err}
				}
				lastAccepted = time.Now()
			}
		}
	}
}

// readJPEG respects length-delimited marker segments, entropy byte stuffing,
// restart markers and additional progressive scans. JPEG markers inside APP or
// comment payloads cannot terminate a frame. Bounded zero buffer padding between
// JPEGs is discarded; every returned byte belongs to the original JPEG. Other
// non-JPEG stdout fails the stream.
func readJPEG(r *bufio.Reader, limit int) ([]byte, error) {
	for skipped := 0; ; skipped++ {
		prefix, err := r.Peek(1)
		if err != nil {
			return nil, err
		}
		if prefix[0] != 0 {
			break
		}
		if skipped == maxFramePaddingBytes {
			return nil, errors.New("camera JPEG padding exceeds size bound")
		}
		_, _ = r.ReadByte()
	}
	data := make([]byte, 0, min(64<<10, limit))
	read := func() (byte, error) {
		if len(data) >= limit {
			return 0, errors.New("camera JPEG exceeds size bound")
		}
		b, err := r.ReadByte()
		if err == nil {
			data = append(data, b)
		}
		return b, err
	}
	a, err := read()
	if err != nil {
		return nil, err
	}
	b, err := read()
	if err != nil {
		return nil, io.ErrUnexpectedEOF
	}
	if a != 0xff || b != 0xd8 {
		return nil, errors.New("camera output is not MJPEG")
	}
	pending := byte(0)
	for {
		marker := pending
		pending = 0
		if marker == 0 {
			prefix, err := read()
			if err != nil {
				return nil, fmt.Errorf("incomplete JPEG marker: %w", err)
			}
			if prefix != 0xff {
				return nil, errors.New("invalid JPEG marker")
			}
			for {
				marker, err = read()
				if err != nil {
					return nil, fmt.Errorf("incomplete JPEG marker: %w", err)
				}
				if marker != 0xff {
					break
				}
			}
		}
		if marker == 0xd9 {
			return data, nil
		}
		if marker == 0x01 {
			continue
		}
		if marker == 0x00 || marker == 0xd8 || marker >= 0xd0 && marker <= 0xd7 {
			return nil, errors.New("unexpected JPEG marker")
		}
		hi, err := read()
		if err != nil {
			return nil, io.ErrUnexpectedEOF
		}
		lo, err := read()
		if err != nil {
			return nil, io.ErrUnexpectedEOF
		}
		n := int(hi)<<8 | int(lo)
		if n < 2 {
			return nil, errors.New("invalid JPEG segment length")
		}
		for i := 2; i < n; i++ {
			if _, err := read(); err != nil {
				return nil, fmt.Errorf("incomplete JPEG segment: %w", err)
			}
		}
		if marker != 0xda {
			continue
		}
		for {
			v, err := read()
			if err != nil {
				return nil, fmt.Errorf("incomplete JPEG scan: %w", err)
			}
			if v != 0xff {
				continue
			}
			for {
				v, err = read()
				if err != nil {
					return nil, fmt.Errorf("incomplete JPEG scan marker: %w", err)
				}
				if v != 0xff {
					break
				}
			}
			if v == 0x00 || v >= 0xd0 && v <= 0xd7 {
				continue
			}
			pending = v
			break
		}
	}
}
