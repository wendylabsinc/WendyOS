package commands

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/wendylabsinc/wendy/go/internal/cli/grpcclient"
)

// fakeRebootDevice scripts a device for runDeviceReboot. Time only moves when
// the code under test waits, so every test is instant and deterministic.
type fakeRebootDevice struct {
	clock time.Time

	rebootErr   error
	rebootCalls int
	// probes are consumed one per uptime probe; the last one repeats.
	probes []rebootProbe
	probed int

	confirmAnswer bool
	confirmAsked  []string
}

type rebootProbe struct {
	// uptime is what the device reports; nil with a nil err is a device that
	// answers without an uptime.
	uptime *uint64
	err    error
}

func seconds(n uint64) *uint64 { return &n }

func (f *fakeRebootDevice) rebooter() deviceRebooter {
	return deviceRebooter{
		label: "orin-nano",
		reboot: func(context.Context) error {
			f.rebootCalls++
			return f.rebootErr
		},
		uptime: func(context.Context) (*uint64, error) {
			i := f.probed
			if i >= len(f.probes) {
				i = len(f.probes) - 1
			}
			f.probed++
			return f.probes[i].uptime, f.probes[i].err
		},
		now: func() time.Time { return f.clock },
		wait: func(_ context.Context, d time.Duration) error {
			f.clock = f.clock.Add(d)
			return nil
		},
		confirm: func(question string) bool {
			f.confirmAsked = append(f.confirmAsked, question)
			return f.confirmAnswer
		},
	}
}

func newFakeRebootDevice() *fakeRebootDevice {
	return &fakeRebootDevice{clock: time.Date(2026, 11, 14, 12, 0, 0, 0, time.UTC)}
}

var errDeviceDown = errors.New("connection refused")

func TestDeviceRebootWaitsForANewBoot(t *testing.T) {
	f := newFakeRebootDevice()
	// Still shutting down and answering with its old uptime, then down, then
	// back with an uptime shorter than the time since the request.
	f.probes = []rebootProbe{
		{uptime: seconds(90_000)},
		{err: errDeviceDown},
		{err: errDeviceDown},
		{uptime: seconds(4)},
	}
	var out bytes.Buffer
	err := runDeviceReboot(context.Background(), f.rebooter(), deviceRebootOptions{assumeYes: true, wait: true}, &out)
	if err != nil {
		t.Fatalf("runDeviceReboot() = %v, want success", err)
	}
	if f.rebootCalls != 1 {
		t.Fatalf("reboot requested %d times, want 1", f.rebootCalls)
	}
	if f.probed != 4 {
		t.Fatalf("probed %d times, want 4: the first answer was the old boot", f.probed)
	}
	if !strings.Contains(out.String(), "orin-nano is back online") {
		t.Fatalf("output %q does not say the device is back", out.String())
	}
}

// A device that keeps answering with an uptime longer than the time since the
// request never went down. Saying "back online" would be false.
func TestDeviceRebootDoesNotMistakeTheOldBootForANewOne(t *testing.T) {
	f := newFakeRebootDevice()
	f.probes = []rebootProbe{{uptime: seconds(90_000)}}
	var out bytes.Buffer
	err := runDeviceReboot(context.Background(), f.rebooter(), deviceRebootOptions{assumeYes: true, wait: true, timeout: 30 * time.Second}, &out)
	if err == nil {
		t.Fatalf("runDeviceReboot() succeeded for a device that never rebooted; output %q", out.String())
	}
	if !strings.Contains(err.Error(), "did not see orin-nano restart") {
		t.Fatalf("error %q does not say the restart was not seen", err)
	}
	if strings.Contains(out.String(), "is back online") {
		t.Fatalf("output %q claims the device is back", out.String())
	}
}

func TestDeviceRebootTimesOutWhenTheDeviceStaysAway(t *testing.T) {
	f := newFakeRebootDevice()
	f.probes = []rebootProbe{{err: errDeviceDown}}
	var out bytes.Buffer
	err := runDeviceReboot(context.Background(), f.rebooter(), deviceRebootOptions{assumeYes: true, wait: true, timeout: 30 * time.Second}, &out)
	if err == nil {
		t.Fatal("runDeviceReboot() succeeded for a device that never came back")
	}
	if !strings.Contains(err.Error(), "has not come back") {
		t.Fatalf("error %q does not say the device has not come back", err)
	}
}

// An agent without an uptime reading can only be judged by having been seen
// offline in between.
func TestDeviceRebootWithoutUptimeNeedsToSeeTheDeviceGoOffline(t *testing.T) {
	f := newFakeRebootDevice()
	f.probes = []rebootProbe{{}, {err: errDeviceDown}, {}}
	var out bytes.Buffer
	err := runDeviceReboot(context.Background(), f.rebooter(), deviceRebootOptions{assumeYes: true, wait: true}, &out)
	if err != nil {
		t.Fatalf("runDeviceReboot() = %v, want success", err)
	}
	if f.probed != 3 {
		t.Fatalf("probed %d times, want 3: the first answer came before the device went down", f.probed)
	}
}

func TestDeviceRebootAsksFirst(t *testing.T) {
	f := newFakeRebootDevice()
	f.confirmAnswer = false
	var out bytes.Buffer
	err := runDeviceReboot(context.Background(), f.rebooter(), deviceRebootOptions{interactive: true, wait: true}, &out)
	if err != nil {
		t.Fatalf("runDeviceReboot() = %v, want a clean abort", err)
	}
	if len(f.confirmAsked) != 1 || !strings.Contains(f.confirmAsked[0], "orin-nano") {
		t.Fatalf("confirmation asked %q, want one question naming the device", f.confirmAsked)
	}
	if f.rebootCalls != 0 {
		t.Fatal("the device was rebooted after the user declined")
	}
	if !strings.Contains(out.String(), "not rebooted") {
		t.Fatalf("output %q does not say the device was not rebooted", out.String())
	}
}

func TestDeviceRebootYesSkipsTheQuestion(t *testing.T) {
	f := newFakeRebootDevice()
	f.probes = []rebootProbe{{err: errDeviceDown}, {uptime: seconds(3)}}
	var out bytes.Buffer
	if err := runDeviceReboot(context.Background(), f.rebooter(), deviceRebootOptions{interactive: true, assumeYes: true, wait: true}, &out); err != nil {
		t.Fatalf("runDeviceReboot() = %v, want success", err)
	}
	if len(f.confirmAsked) != 0 {
		t.Fatalf("--yes still asked %q", f.confirmAsked)
	}
}

// Without a terminal nobody can answer, and a reboot stops the device's apps:
// the command must not go ahead on its own.
func TestDeviceRebootWithoutATerminalNeedsYes(t *testing.T) {
	f := newFakeRebootDevice()
	var out bytes.Buffer
	err := runDeviceReboot(context.Background(), f.rebooter(), deviceRebootOptions{wait: true}, &out)
	if err == nil || !strings.Contains(err.Error(), "--yes") {
		t.Fatalf("runDeviceReboot() = %v, want an error naming --yes", err)
	}
	if f.rebootCalls != 0 || len(f.confirmAsked) != 0 {
		t.Fatal("the device was rebooted or a question was asked with nobody to answer")
	}
}

func TestDeviceRebootNoWaitReturnsOnceTheDeviceAccepted(t *testing.T) {
	f := newFakeRebootDevice()
	var out bytes.Buffer
	if err := runDeviceReboot(context.Background(), f.rebooter(), deviceRebootOptions{assumeYes: true}, &out); err != nil {
		t.Fatalf("runDeviceReboot() = %v, want success", err)
	}
	if f.probed != 0 {
		t.Fatalf("probed %d times with waiting turned off", f.probed)
	}
	if !strings.Contains(out.String(), "orin-nano is rebooting") {
		t.Fatalf("output %q does not say the device is rebooting", out.String())
	}
}

func TestDeviceRebootJSON(t *testing.T) {
	t.Run("waited", func(t *testing.T) {
		f := newFakeRebootDevice()
		f.probes = []rebootProbe{{err: errDeviceDown}, {uptime: seconds(7)}}
		var out bytes.Buffer
		if err := runDeviceReboot(context.Background(), f.rebooter(), deviceRebootOptions{assumeYes: true, wait: true, asJSON: true}, &out); err != nil {
			t.Fatalf("runDeviceReboot() = %v, want success", err)
		}
		var got map[string]any
		if err := json.Unmarshal(out.Bytes(), &got); err != nil {
			t.Fatalf("output is not one JSON object: %v\n%s", err, out.String())
		}
		if got["status"] != "rebooted" || got["device"] != "orin-nano" || got["uptimeSeconds"] != float64(7) {
			t.Fatalf("JSON = %v, want status rebooted, device orin-nano, uptimeSeconds 7", got)
		}
	})
	t.Run("not waited", func(t *testing.T) {
		f := newFakeRebootDevice()
		var out bytes.Buffer
		if err := runDeviceReboot(context.Background(), f.rebooter(), deviceRebootOptions{assumeYes: true, asJSON: true}, &out); err != nil {
			t.Fatalf("runDeviceReboot() = %v, want success", err)
		}
		var got map[string]any
		if err := json.Unmarshal(out.Bytes(), &got); err != nil {
			t.Fatalf("output is not one JSON object: %v\n%s", err, out.String())
		}
		if got["status"] != "requested" || got["device"] != "orin-nano" {
			t.Fatalf("JSON = %v, want status requested, device orin-nano", got)
		}
	})
}

func TestDeviceRebootExplainsRefusals(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want []string
	}{
		{
			name: "agent too old",
			err:  status.Error(codes.Unimplemented, "unknown service wendy.agent.services.v2.WendyPowerService"),
			want: []string{"does not support reboot", "wendy device update"},
		},
		{
			name: "update running",
			err:  status.Error(codes.FailedPrecondition, "an agent or OS update is being installed; reboot the device once it has finished"),
			want: []string{"an agent or OS update is being installed"},
		},
		{
			name: "host cannot be rebooted",
			err:  status.Error(codes.Unavailable, "the agent cannot reboot this host"),
			want: []string{"the agent cannot reboot this host"},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			f := newFakeRebootDevice()
			f.rebootErr = test.err
			var out bytes.Buffer
			err := runDeviceReboot(context.Background(), f.rebooter(), deviceRebootOptions{assumeYes: true, wait: true}, &out)
			if err == nil {
				t.Fatal("runDeviceReboot() succeeded although the device refused")
			}
			for _, want := range test.want {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error %q does not mention %q", err, want)
				}
			}
			if f.probed != 0 {
				t.Errorf("waited for a reboot the device refused (%d probes)", f.probed)
			}
		})
	}
}

// Messages name the device the way the user addressed it, not the address the
// name happened to resolve to.
func TestRebootDeviceLabelPrefersTheSelectedName(t *testing.T) {
	tests := []struct {
		name string
		conn *grpcclient.AgentConnection
		want string
	}{
		{"selected name over resolved address", &grpcclient.AgentConnection{DevicePinKey: "my-device.local", Host: "169.254.18.126", Addr: "169.254.18.126:50052"}, "my-device.local"},
		{"simulator", &grpcclient.AgentConnection{SimulatorName: "sim", DevicePinKey: "127.0.0.1:50051", Host: "127.0.0.1"}, "vm:sim"},
		{"host when no name was kept", &grpcclient.AgentConnection{Host: "192.168.1.20", Addr: "192.168.1.20:50051"}, "192.168.1.20"},
		{"address as a last resort", &grpcclient.AgentConnection{Addr: "192.168.1.20:50051"}, "192.168.1.20:50051"},
		{"nothing known", &grpcclient.AgentConnection{}, "the device"},
		{"no connection", nil, "the device"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := rebootDeviceLabel(test.conn); got != test.want {
				t.Fatalf("rebootDeviceLabel() = %q, want %q", got, test.want)
			}
		})
	}
}
