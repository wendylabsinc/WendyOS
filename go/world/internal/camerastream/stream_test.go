package camerastream

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"image"
	"image/color"
	"image/jpeg"
	"io"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

func testJPEG(t testing.TB) []byte {
	t.Helper()
	im := image.NewRGBA(image.Rect(0, 0, 8, 8))
	for y := 0; y < 8; y++ {
		for x := 0; x < 8; x++ {
			im.Set(x, y, color.RGBA{uint8(x * 30), uint8(y * 30), 80, 255})
		}
	}
	var b bytes.Buffer
	if err := jpeg.Encode(&b, im, nil); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

type fragments struct{ data []byte }

func (r *fragments) Read(p []byte) (int, error) {
	if len(r.data) == 0 {
		return 0, io.EOF
	}
	p[0] = r.data[0]
	r.data = r.data[1:]
	return 1, nil
}

func TestJPEGFramingKeepsAPPMarkerBytesAndFragments(t *testing.T) {
	original := testJPEG(t)
	// APP1 contains literal SOI/EOI bytes. They are data, not delimiters.
	withAPP := append([]byte{0xff, 0xd8, 0xff, 0xe1, 0, 8, 0xff, 0xd9, 0xff, 0xd8, 1, 2}, original[2:]...)
	r := bufio.NewReader(&fragments{append(append([]byte{}, withAPP...), original...)})
	for _, want := range [][]byte{withAPP, original} {
		got, err := readJPEG(r, MaxFrameBytes)
		if err != nil || !bytes.Equal(got, want) {
			t.Fatalf("framing mismatch: %v", err)
		}
		if _, err = jpeg.Decode(bytes.NewReader(got)); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := readJPEG(r, MaxFrameBytes); !errors.Is(err, io.EOF) {
		t.Fatalf("end: %v", err)
	}
}

func TestJPEGFramingDiscardsBoundedCameraBufferPadding(t *testing.T) {
	original := testJPEG(t)
	// The physical Seeed camera occasionally ends a payload with zero-filled
	// buffer space after EOI. Keep that space out of the original JPEG receipt.
	stream := append(append(append([]byte{}, original...), make([]byte, 1984)...), original...)
	r := bufio.NewReader(&fragments{stream})
	for i := 0; i < 2; i++ {
		got, err := readJPEG(r, MaxFrameBytes)
		if err != nil || !bytes.Equal(got, original) {
			t.Fatalf("frame %d changed: %v", i, err)
		}
	}
	for name, prefix := range map[string][]byte{
		"maximum padding": make([]byte, maxFramePaddingBytes),
		"excess padding":  make([]byte, maxFramePaddingBytes+1),
		"nonzero padding": {0, 0, 1},
	} {
		t.Run(name, func(t *testing.T) {
			got, err := readJPEG(bufio.NewReader(bytes.NewReader(append(prefix, original...))), MaxFrameBytes)
			if name == "maximum padding" {
				if err != nil || !bytes.Equal(got, original) {
					t.Fatalf("bounded padding rejected: %v", err)
				}
			} else if err == nil || got != nil {
				t.Fatal("invalid padding accepted")
			}
		})
	}
}

func TestJPEGFramingRejectsPartialInvalidAndOversize(t *testing.T) {
	valid := testJPEG(t)
	for name, data := range map[string][]byte{
		"nonjpeg":      []byte("login required"),
		"partial":      valid[:len(valid)-1],
		"short length": {0xff, 0xd8, 0xff, 0xe1, 0, 1},
		"nested image": {0xff, 0xd8, 0xff, 0xd8},
		"oversize":     valid,
	} {
		t.Run(name, func(t *testing.T) {
			limit := MaxFrameBytes
			if name == "oversize" {
				limit = 20
			}
			got, err := readJPEG(bufio.NewReader(bytes.NewReader(data)), limit)
			if err == nil || got != nil {
				t.Fatalf("accepted %q", name)
			}
		})
	}
}

// Helper children emit generated JPEGs only. No physical camera is used.
func TestCameraHelperProcess(t *testing.T) {
	mode := os.Getenv("WORLD_CAMERA_TEST_HELPER")
	if mode == "" {
		return
	}
	if mode == "stall" {
		_, _ = os.Stdout.Write([]byte{0xff, 0xd8})
		time.Sleep(time.Minute)
		os.Exit(0)
	}
	data := testJPEG(t)
	if mode == "delayed_then_stall" {
		time.Sleep(200 * time.Millisecond)
		_, _ = os.Stdout.Write(data)
		time.Sleep(time.Minute)
		os.Exit(0)
	}
	for {
		if _, err := os.Stdout.Write(data); err != nil {
			os.Exit(1)
		}
		if mode == "once" {
			time.Sleep(80 * time.Millisecond)
			os.Exit(0)
		}
		time.Sleep(time.Millisecond)
	}
}

func helperConfig(mode string, commands *[]*exec.Cmd) Config {
	return Config{Device: "wendy-lite:test.local", CameraID: 4, SampleInterval: time.Millisecond, FrameTimeout: time.Second, ReconnectDelay: 10 * time.Millisecond,
		command: func(ctx context.Context, _ string, args ...string) *exec.Cmd {
			cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestCameraHelperProcess$")
			cmd.Env = append(os.Environ(), "WORLD_CAMERA_TEST_HELPER="+mode)
			*commands = append(*commands, cmd)
			return cmd
		},
	}
}

func assertReaped(t *testing.T, commands []*exec.Cmd) {
	t.Helper()
	for _, cmd := range commands {
		if cmd.Process == nil || cmd.ProcessState == nil {
			t.Fatal("child not reaped")
		}
	}
}

func TestReconnectUsesNewSessionAndReapsChildren(t *testing.T) {
	var commands []*exec.Cmd
	cfg := helperConfig("once", &commands)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	stop := errors.New("storage callback stopped")
	sessions := map[string]bool{}
	var statuses []string
	err := Run(ctx, cfg, func(data []byte, session string, seq uint64) error {
		if !bytes.Equal(data, testJPEG(t)) || len(session) != 32 || seq != 1 {
			t.Error("invalid receipt")
		}
		sessions[session] = true
		if len(sessions) == 2 {
			return stop
		}
		return nil
	}, func(status, detail string) { statuses = append(statuses, status) })
	if !errors.Is(err, stop) || len(sessions) != 2 {
		t.Fatalf("sessions=%d err=%v", len(sessions), err)
	}
	if strings.Join(statuses, ",") != "connecting,disconnected,connecting,stopped" {
		t.Fatalf("status=%v", statuses)
	}
	assertReaped(t, commands)
}

func TestWatchdogReconnectsIncompleteStreamAndCancellationReaps(t *testing.T) {
	var commands []*exec.Cmd
	cfg := helperConfig("stall", &commands)
	cfg.StartupTimeout = 80 * time.Millisecond
	cfg.FrameTimeout = 80 * time.Millisecond
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	connecting := 0
	err := Run(ctx, cfg, func([]byte, string, uint64) error { t.Error("partial frame delivered"); return nil }, func(status, detail string) {
		if status == "connecting" {
			connecting++
			if connecting == 3 {
				cancel()
			}
		}
	})
	if !errors.Is(err, context.Canceled) || connecting != 3 {
		t.Fatalf("attempts=%d err=%v", connecting, err)
	}
	assertReaped(t, commands)
}

func TestDelayedFirstFrameUsesStartupTimeoutThenInterframeWatchdog(t *testing.T) {
	var commands []*exec.Cmd
	cfg := helperConfig("delayed_then_stall", &commands)
	cfg.StartupTimeout = 2 * time.Second
	cfg.FrameTimeout = 100 * time.Millisecond
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	started := time.Now()
	var received time.Time
	connecting := 0
	err := Run(ctx, cfg, func(data []byte, session string, seq uint64) error {
		if !bytes.Equal(data, testJPEG(t)) || seq != 1 {
			t.Error("invalid first frame")
		}
		received = time.Now()
		return nil
	}, func(status, detail string) {
		if status == "connecting" {
			connecting++
			if connecting == 2 {
				cancel()
			}
		}
	})
	if !errors.Is(err, context.Canceled) || connecting != 2 || received.IsZero() {
		t.Fatalf("attempts=%d received=%v err=%v", connecting, received, err)
	}
	if received.Sub(started) < 200*time.Millisecond {
		t.Fatal("helper did not delay its first frame beyond the interframe timeout")
	}
	if elapsed := time.Since(received); elapsed < cfg.FrameTimeout || elapsed >= cfg.StartupTimeout/2 {
		t.Fatalf("post-frame stall used wrong timeout: %v", elapsed)
	}
	assertReaped(t, commands)
}

func TestSlowConsumerSkipsFramesWithoutBacklog(t *testing.T) {
	var commands []*exec.Cmd
	cfg := helperConfig("stream", &commands)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	stop := errors.New("complete")
	var first uint64
	err := Run(ctx, cfg, func(_ []byte, _ string, seq uint64) error {
		if first == 0 {
			first = seq
			time.Sleep(100 * time.Millisecond)
			return nil
		}
		if seq <= first+2 {
			t.Errorf("backlog retained: first=%d next=%d", first, seq)
		}
		return stop
	}, nil)
	if !errors.Is(err, stop) {
		t.Fatal(err)
	}
	assertReaped(t, commands)
}

func TestRefusesNonLiteDeviceBeforeStarting(t *testing.T) {
	var commands []*exec.Cmd
	cfg := helperConfig("stream", &commands)
	cfg.Device = "some-gateway"
	if err := Run(context.Background(), cfg, func([]byte, string, uint64) error { return nil }, nil); err == nil || len(commands) != 0 {
		t.Fatal("unsupported capture started")
	}
}
