package commands

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/cli/providers"
	"github.com/wendylabsinc/wendy/go/internal/shared/models"
)

type restartTestProvider struct {
	fakeProvider
	probe func(context.Context, models.ExternalDevice) (*providers.ProviderDeviceInfo, error)
}

func (p *restartTestProvider) GetDeviceInfo(ctx context.Context, device models.ExternalDevice) (*providers.ProviderDeviceInfo, error) {
	return p.probe(ctx, device)
}

func TestWaitForLiteAfterInstall(t *testing.T) {
	probeErr := errors.New("serial handshake: sentinel not received")
	promptErr := errors.New("prompt failed")
	for _, tc := range []struct {
		name        string
		ready       bool
		interactive bool
		usb         bool
		confirm     bool
		promptErr   error
		retryFails  bool
		wantErr     error
		wantProbes  int
		wantPrompts int
	}{
		{name: "automatic restart", ready: true, interactive: true, usb: true, wantProbes: 1},
		{name: "manual reset resumes run", interactive: true, usb: true, confirm: true, wantProbes: 2, wantPrompts: 1},
		{name: "decline cancels", interactive: true, usb: true, wantErr: ErrUserCancelled, wantProbes: 1, wantPrompts: 1},
		{name: "prompt failure", interactive: true, usb: true, promptErr: promptErr, wantErr: promptErr, wantProbes: 1, wantPrompts: 1},
		{name: "retry remains bounded", interactive: true, usb: true, confirm: true, retryFails: true, wantErr: probeErr, wantProbes: 2, wantPrompts: 1},
		{name: "noninteractive keeps recovery advice", usb: true, wantErr: probeErr, wantProbes: 1},
		{name: "network device never asks for USB reset", interactive: true, wantErr: probeErr, wantProbes: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			device := models.ExternalDevice{ID: "selected-board", ConnectionInfo: map[string]string{}}
			if tc.usb {
				device.ConnectionInfo["serialPort"] = "/dev/test-esp32"
			}
			probes, prompts := 0, 0
			p := &restartTestProvider{probe: func(ctx context.Context, got models.ExternalDevice) (*providers.ProviderDeviceInfo, error) {
				probes++
				if got.ID != device.ID || got.ConnectionInfo["serialPort"] != device.ConnectionInfo["serialPort"] {
					t.Fatal("reconnect changed the selected board")
				}
				if _, ok := ctx.Deadline(); !ok {
					t.Fatal("readiness probe has no deadline")
				}
				if tc.ready || (prompts > 0 && !tc.retryFails) {
					return &providers.ProviderDeviceInfo{DeviceType: "esp32c6"}, nil
				}
				<-ctx.Done()
				return nil, probeErr
			}}
			previous := confirmLiteResetFn
			t.Cleanup(func() { confirmLiteResetFn = previous })
			confirmLiteResetFn = func(context.Context) (bool, error) {
				prompts++
				return tc.confirm, tc.promptErr
			}
			err := waitForLiteAfterInstall(context.Background(), p, device, tc.interactive, 10*time.Millisecond)
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("error = %v, want %v", err, tc.wantErr)
			}
			if probes != tc.wantProbes || prompts != tc.wantPrompts {
				t.Fatalf("probes/prompts = %d/%d, want %d/%d", probes, prompts, tc.wantProbes, tc.wantPrompts)
			}
			if tc.usb && errors.Is(err, probeErr) {
				for _, hint := range []string{"release BOOT", "press RESET once", "no reflash", "retry wendy run"} {
					if !strings.Contains(err.Error(), hint) {
						t.Errorf("missing recovery hint %q: %v", hint, err)
					}
				}
			}
		})
	}
}

func TestWaitForLiteAfterInstallCancellation(t *testing.T) {
	for _, duringPrompt := range []bool{false, true} {
		t.Run(map[bool]string{false: "during probe", true: "during prompt"}[duringPrompt], func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			probes, prompts := 0, 0
			p := &restartTestProvider{probe: func(probeCtx context.Context, _ models.ExternalDevice) (*providers.ProviderDeviceInfo, error) {
				probes++
				if !duringPrompt {
					cancel()
				}
				<-probeCtx.Done()
				return nil, probeCtx.Err()
			}}
			previous := confirmLiteResetFn
			t.Cleanup(func() { confirmLiteResetFn = previous })
			confirmLiteResetFn = func(context.Context) (bool, error) {
				prompts++
				cancel()
				return true, nil
			}
			device := models.ExternalDevice{ConnectionInfo: map[string]string{"serialPort": "/dev/test-esp32"}}
			err := waitForLiteAfterInstall(ctx, p, device, true, 10*time.Millisecond)
			if !errors.Is(err, context.Canceled) || probes != 1 {
				t.Fatalf("error/probes = %v/%d, want cancellation and no retry", err, probes)
			}
			if !duringPrompt && prompts != 0 {
				t.Fatal("canceled probe opened a prompt")
			}
		})
	}
}
