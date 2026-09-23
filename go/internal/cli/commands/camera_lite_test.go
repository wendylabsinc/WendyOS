package commands

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/wendylabsinc/wendy/go/internal/cli/liteclient"
	"github.com/wendylabsinc/wendy/go/internal/cli/providers"
	"github.com/wendylabsinc/wendy/go/internal/shared/models"
)

// liteCameraProvider is a provider that speaks WendyCom, embedding
// DeviceProvider (nil) because the stream command calls nothing else on it.
type liteCameraProvider struct {
	providers.DeviceProvider
	connected bool
	err       error
}

func (p *liteCameraProvider) ConnectWendyCom(models.ExternalDevice) (*liteclient.WendyLiteClient, error) {
	p.connected = true
	if p.err == nil {
		p.err = errors.New("liteCameraProvider: test never connects")
	}
	return nil, p.err
}

// plainProvider can do everything a provider must and no WendyCom.
type plainProvider struct{ providers.DeviceProvider }

func liteDevice(connType string, extra map[string]string) *models.ExternalDevice {
	info := map[string]string{"type": connType, "deviceId": "esp32-1"}
	for k, v := range extra {
		info[k] = v
	}
	return &models.ExternalDevice{
		ID:             "wendy-lite:/dev/cu.usbmodem101",
		DisplayName:    "Wendy Lite (COM7)",
		ProviderKey:    "wendy-lite",
		ConnectionInfo: info,
	}
}

// A Wendy Lite device must be refused before anything dials it whenever the
// refusal is knowable from the picked row: the operator gets the reason instead
// of a timeout, and the board's serial port is never taken in the meantime.
func TestCameraViewRejectsLiteDevicesItCannotStream(t *testing.T) {
	for _, tc := range []struct {
		name     string
		device   *models.ExternalDevice
		provider providers.DeviceProvider
		args     []string
		want     []string
	}{
		{
			name:     "bluetooth cannot carry video",
			device:   liteDevice("BLE", nil),
			provider: &liteCameraProvider{},
			want:     []string{"Bluetooth LE", "cannot carry a video stream", "over USB"},
		},
		{
			name:     "unflashed board has no camera",
			device:   liteDevice("USB", map[string]string{"needsInstall": "true"}),
			provider: &liteCameraProvider{},
			want:     []string{"no Wendy Lite firmware installed", "wendy run"},
		},
		{
			// A provider that speaks no WendyCom at all is refused for that,
			// not sent off to correct flags that were never the reason.
			name:     "provider cannot speak WendyCom at all",
			device:   liteDevice("USB", nil),
			provider: &plainProvider{},
			args:     []string{"--id", "0"},
			want:     []string{"does not support camera streaming"},
		},
		{
			// --id names an agent camera by boot order; a Lite board streams
			// what its manifest declares and has no such number.
			name:     "agent-only flags are refused, and named",
			device:   liteDevice("USB", nil),
			provider: &liteCameraProvider{},
			args:     []string{"--id", "0", "--width", "640"},
			want:     []string{"--id and --width", "sensor-link manifest"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stubCameraTarget(t, func(...resolveOption) (*SelectedDevice, error) {
				return &SelectedDevice{External: tc.device, Provider: tc.provider}, nil
			})
			cmd := newCameraViewCmd()
			cmd.SetOut(io.Discard)
			cmd.SetErr(io.Discard)
			cmd.SetArgs(append([]string{"--non-interactive"}, tc.args...))

			err := cmd.ExecuteContext(context.Background())
			if err == nil {
				t.Fatal("streaming was attempted, want a refusal")
			}
			for _, want := range tc.want {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error = %q, want it to mention %q", err, want)
				}
			}
			if p, ok := tc.provider.(*liteCameraProvider); ok && p.connected {
				t.Error("the device was dialled before the refusal")
			}
		})
	}
}

// A refusal that fires on a device the flags DO fit would be a regression the
// agent path pays for, so the rejection has to key on what was passed.
func TestCameraViewAcceptsALiteDeviceWithNoCameraFlags(t *testing.T) {
	provider := &liteCameraProvider{err: errors.New("serial port is in use by another process")}
	stubCameraTarget(t, func(...resolveOption) (*SelectedDevice, error) {
		return &SelectedDevice{External: liteDevice("USB", nil), Provider: provider}, nil
	})
	cmd := newCameraViewCmd()
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	cmd.SetArgs([]string{"--non-interactive"})

	err := cmd.ExecuteContext(context.Background())
	if !provider.connected {
		t.Fatal("the device was never dialled")
	}
	// The provider's own error names serial contention and the mTLS identities
	// it tried; wrapping it would bury both.
	if err == nil || !strings.Contains(err.Error(), "in use by another process") {
		t.Fatalf("error = %v, want the provider's error surfaced as-is", err)
	}
}

func TestAgentOnlyCameraFlagsNamesOnlyWhatWasPassed(t *testing.T) {
	cmd := newCameraViewCmd()
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	// --id 0 is the trap for a zero-value check: it is the flag's default and
	// still an explicit choice of camera 0.
	if err := cmd.ParseFlags([]string{"--id", "0", "--fps", "30"}); err != nil {
		t.Fatalf("ParseFlags: %v", err)
	}
	got := agentOnlyCameraFlags(cmd)
	want := []string{"--id", "--fps"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("agentOnlyCameraFlags = %v, want %v", got, want)
	}

	fresh := newCameraViewCmd()
	if got := agentOnlyCameraFlags(fresh); len(got) != 0 {
		t.Errorf("agentOnlyCameraFlags with nothing passed = %v, want none", got)
	}
}

// Ctrl+C and a closed source are how a viewer normally stops; only a real
// failure should reach the operator as one.
func TestLiteStreamEndSeparatesStoppingFromFailing(t *testing.T) {
	for _, quiet := range []error{context.Canceled, io.EOF} {
		if err := liteStreamEnd(quiet); err != nil {
			t.Errorf("liteStreamEnd(%v) = %v, want nil", quiet, err)
		}
	}
	boom := errors.New("link died")
	err := liteStreamEnd(boom)
	if !errors.Is(err, boom) || !strings.Contains(err.Error(), "receiving video") {
		t.Errorf("liteStreamEnd(%v) = %v, want it wrapped as a receive failure", boom, err)
	}
}
