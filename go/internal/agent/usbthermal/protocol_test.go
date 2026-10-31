package usbthermal

import (
	"context"
	"encoding/hex"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"
)

// TestCommandsMatchTheVerifiedBytes: the two commands the driver sends are the
// exact bytes the TC001 Max answered on exxon-01, CRC included.
func TestCommandsMatchTheVerifiedBytes(t *testing.T) {
	cases := map[string]struct {
		got  []byte
		want string
	}{
		"read model":   {cmdReadModel, "0101810001000000000000001e0000004f90"},
		"start stream": {cmdStartStream, "012f81000000000000000000010000004930"},
	}
	for name, c := range cases {
		if got := hex.EncodeToString(c.got); got != c.want {
			t.Errorf("%s = %s, want %s", name, got, c.want)
		}
	}
}

func recordSleeps() (sleepFunc, func() []time.Duration) {
	var got []time.Duration
	return func(ctx context.Context, d time.Duration) error {
			got = append(got, d)
			return ctx.Err()
		}, func() []time.Duration {
			return got
		}
}

// TestStartAndStopSequence pins the exact control sequence, in order: it is the
// one verified on hardware, and the camera does not stream without each step.
func TestStartAndStopSequence(t *testing.T) {
	dev := newFakeDevice()
	sleep, sleeps := recordSleeps()
	s := &session{dev: dev, sleep: sleep}
	model, err := s.start(context.Background())
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	if model != "TC001 Max" {
		t.Fatalf("model = %q, want the zero padding trimmed", model)
	}
	model30 := "cmd 0101810001000000000000001e0000004f90"
	start := "cmd 012f81000000000000000000010000004930"
	want := []string{
		"claim 0", "claim 1",
		model30, "status", "resp 30", "status",
		start, "status", "resp 1", "status",
		"setintf 1 1",
		"ctrl 40 ee 0 1 len0",
		"bulk 81 16384",
		start, "status", "resp 1", "status",
	}
	if got := dev.callLog(); !slices.Equal(got, want) {
		t.Fatalf("start sequence:\n got %q\nwant %q", got, want)
	}
	if got := sleeps(); !slices.Equal(got, []time.Duration{time.Second, 2 * time.Second}) {
		t.Fatalf("pauses = %v, want [1s 2s] (after start_stream, after selecting the interface)", got)
	}

	if err := s.stop(); err != nil {
		t.Fatalf("stop: %v", err)
	}
	if got := dev.callLog()[len(want):]; !slices.Equal(got, []string{"setintf 1 0", "release 1", "release 0"}) {
		t.Fatalf("stop sequence = %q, want interface 1 back to alt 0, then both released", got)
	}
	if err := s.stop(); err != nil || len(dev.callLog()) != len(want)+3 {
		t.Fatalf("a second stop must do nothing (err %v, calls %q)", err, dev.callLog()[len(want):])
	}
}

// TestCommandWaitsWhileBusy: the status bit is polled until it clears.
func TestCommandWaitsWhileBusy(t *testing.T) {
	dev := newFakeDevice()
	dev.busyPolls = 2
	sleep, sleeps := recordSleeps()
	s := &session{dev: dev, sleep: sleep}
	if _, err := s.readModel(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := sleeps(); !slices.Equal(got, []time.Duration{statusPollInterval, statusPollInterval}) {
		t.Fatalf("polls slept %v, want two 10 ms waits", got)
	}
}

// TestCommandGivesUpOnAStuckCamera: a camera that never clears its busy bit
// fails the command instead of hanging the agent.
func TestCommandGivesUpOnAStuckCamera(t *testing.T) {
	dev := newFakeDevice()
	dev.busyPolls = -1 // forever
	sleep, _ := recordSleeps()
	s := &session{dev: dev, sleep: sleep}
	_, err := s.readModel(context.Background())
	if err == nil || !strings.Contains(err.Error(), "busy") {
		t.Fatalf("err = %v, want a busy error", err)
	}
}

// TestStartFailureLeavesTheCameraAsFound: when another process owns the
// streaming interface, the interface the agent did claim is released and the
// stream interface is never touched.
func TestStartFailureLeavesTheCameraAsFound(t *testing.T) {
	dev := newFakeDevice()
	dev.claimErr = ErrBusy
	sleep, _ := recordSleeps()
	s := &session{dev: dev, sleep: sleep}
	_, err := s.start(context.Background())
	if !errors.Is(err, ErrBusy) {
		t.Fatalf("err = %v, want ErrBusy", err)
	}
	if err := s.stop(); err != nil {
		t.Fatal(err)
	}
	if got := dev.callLog(); !slices.Equal(got, []string{"claim 0", "claim 1", "release 0"}) {
		t.Fatalf("calls = %q", got)
	}
}

// TestStartToleratesAPrimeReadTimeout: the throwaway read usually times out.
func TestStartToleratesAPrimeReadTimeout(t *testing.T) {
	dev := newFakeDevice() // no bulk source: every read times out
	sleep, _ := recordSleeps()
	s := &session{dev: dev, sleep: sleep}
	if _, err := s.start(context.Background()); err != nil {
		t.Fatalf("start: %v", err)
	}
	if err := s.stop(); err != nil {
		t.Fatal(err)
	}
}

// TestStartStopsWhenCancelled: a cancel during the start sequence's pauses
// ends it, and stop still restores the interface.
func TestStartStopsWhenCancelled(t *testing.T) {
	dev := newFakeDevice()
	ctx, cancel := context.WithCancel(context.Background())
	s := &session{dev: dev, sleep: func(ctx context.Context, d time.Duration) error {
		if d == settleAfterInterface {
			cancel()
		}
		return ctx.Err()
	}}
	if _, err := s.start(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if err := s.stop(); err != nil {
		t.Fatal(err)
	}
	log := dev.callLog()
	if got := log[len(log)-3:]; !slices.Equal(got, []string{"setintf 1 0", "release 1", "release 0"}) {
		t.Fatalf("after a cancelled start the camera was left %q", got)
	}
}
