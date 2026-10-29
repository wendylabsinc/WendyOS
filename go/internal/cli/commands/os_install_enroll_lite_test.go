//go:build darwin || linux || windows

package commands

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"
	"github.com/wendylabsinc/wendy/go/internal/shared/config"
	"github.com/wendylabsinc/wendy/go/internal/shared/discovery"
	"github.com/wendylabsinc/wendy/go/internal/shared/models"
)

func TestLiteInstallEnrollmentSelectsSessionWithoutLegacyRPCs(t *testing.T) {
	stubEnrollPrompts(t)
	resolveOrgFn = func(context.Context, *config.AuthConfig, bool) (OrgResolution, error) {
		t.Fatal("Lite install must not call the retired v1 organization API")
		return OrgResolution{}, nil
	}
	auth := oidcEnrollmentAuth(t)
	auth.CloudGRPC = "grpc.wendy.sh:443"
	cfg := &config.Config{Auth: []config.AuthConfig{*auth}}
	for _, mode := range []preEnrollMode{preEnrollAuto, preEnrollForced} {
		confirmPreEnroll = func() (bool, error) { return true, nil }
		got, err := resolveLiteInstallEnrollment(cfg, preEnrollOptions{mode: mode}, mode == preEnrollAuto)
		if err != nil || got == nil || got.CloudGRPC != auth.CloudGRPC {
			t.Fatalf("session = %v, error = %v", got, err)
		}
	}
}

func TestLiteInstallEnrollmentSkipAndFailures(t *testing.T) {
	for _, tc := range []struct {
		name        string
		mode        preEnrollMode
		interactive bool
		confirm     bool
		ack         bool
		wantErr     string
	}{
		{name: "explicit skip", mode: preEnrollSkip, interactive: true},
		{name: "automatic noninteractive", mode: preEnrollAuto},
		{name: "declined enrollment", mode: preEnrollAuto, interactive: true},
		{name: "invalid session stops forced install", mode: preEnrollForced, wantErr: "--pre-enroll:"},
		{name: "invalid session acknowledged", mode: preEnrollAuto, interactive: true, confirm: true, ack: true},
		{name: "invalid session cancels", mode: preEnrollAuto, interactive: true, confirm: true, wantErr: ErrUserCancelled.Error()},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stubEnrollPrompts(t)
			confirmPreEnroll = func() (bool, error) { return tc.confirm, nil }
			confirmContinueUnenrolled = func() (bool, error) { return tc.ack, nil }
			// A legacy session cannot supply the tenant operator identity.
			cfg := &config.Config{Auth: twoSessionConfig().Auth[:1]}
			got, err := resolveLiteInstallEnrollment(cfg, preEnrollOptions{mode: tc.mode}, tc.interactive)
			if got != nil {
				t.Fatal("unexpected enrollment session")
			}
			if tc.wantErr == "" {
				if err != nil {
					t.Fatal(err)
				}
			} else if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("error = %v, want %q", err, tc.wantErr)
			}
		})
	}
}

func TestEnrollInstalledLiteUsesLiveEnrollmentOnce(t *testing.T) {
	origWait, origEnroll := waitInstalledLiteFn, runLiteEnrollmentFn
	t.Cleanup(func() { waitInstalledLiteFn, runLiteEnrollmentFn = origWait, origEnroll })
	waitErr := errors.New("board did not boot")
	enrollErr := errors.New("asset reserved but certificate not confirmed")
	for _, tc := range []struct {
		name               string
		waitErr, enrollErr error
	}{
		{name: "success"},
		{name: "readiness failure", waitErr: waitErr},
		{name: "enrollment failure", enrollErr: enrollErr},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.WithValue(context.Background(), struct{}{}, "install")
			auth := &config.AuthConfig{CloudGRPC: "grpc.wendy.sh:443"}
			original := discovery.SerialPortInfo{Port: "/dev/original"}
			calls := 0
			waitInstalledLiteFn = func(gotCtx context.Context, port discovery.SerialPortInfo, interactive bool) (discovery.SerialPortInfo, error) {
				if gotCtx != ctx || port != original || !interactive {
					t.Fatal("readiness target changed")
				}
				return discovery.SerialPortInfo{Port: "/dev/confirmed-after-reboot"}, tc.waitErr
			}
			runLiteEnrollmentFn = func(cmd *cobra.Command, target models.ExternalDevice, gotAuth *config.AuthConfig, name string, opts liteEnrollmentOptions) error {
				calls++
				if cmd.Context() != ctx || gotAuth != auth || name != "camera" || opts.brokerPort != 5055 {
					t.Fatal("live enrollment lost install options")
				}
				if target.ProviderKey != "wendy-lite" || target.ConnectionType() != "USB" || target.ConnectionInfo["serialPort"] != "/dev/confirmed-after-reboot" || target.OSVersion != "0.11.0" {
					t.Fatalf("wrong enrollment target: %+v", target)
				}
				return tc.enrollErr
			}
			err := enrollInstalledLite(ctx, original, auth, "camera", "0.11.0", true)
			wantCalls := 1
			if tc.waitErr != nil {
				wantCalls = 0
			}
			if calls != wantCalls {
				t.Fatalf("enrollment calls = %d, want %d", calls, wantCalls)
			}
			wantErr := tc.waitErr
			if wantErr == nil {
				wantErr = tc.enrollErr
			}
			if wantErr == nil {
				if err != nil {
					t.Fatal(err)
				}
			} else if !errors.Is(err, wantErr) || !strings.Contains(err.Error(), "firmware was flashed") || !strings.Contains(err.Error(), "without reflashing") {
				t.Fatalf("missing partial-success or recovery guidance: %v", err)
			}
		})
	}
}

func TestWaitInstalledLiteDoesNotSwitchDevices(t *testing.T) {
	origProbe, origPorts, origTimeout := probeInstalledLiteFn, installedLitePortsFn, installedLiteBootTimeout
	t.Cleanup(func() {
		probeInstalledLiteFn, installedLitePortsFn, installedLiteBootTimeout = origProbe, origPorts, origTimeout
	})
	installedLiteBootTimeout = 5 * time.Millisecond
	original := discovery.SerialPortInfo{Port: "/dev/original"}
	for _, tc := range []struct {
		name                  string
		ready, canceled, uart bool
		wantErr               string
	}{
		{name: "ready", ready: true},
		{name: "renamed port needs selection", wantErr: "changed after reboot"},
		{name: "cancel", canceled: true, wantErr: "context canceled"},
		{name: "UART cannot enroll", uart: true, wantErr: "native USB"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			probeInstalledLiteFn = func(ctx context.Context, port string) error {
				if tc.canceled || tc.uart {
					t.Fatal("unexpected probe")
				}
				if port != original.Port {
					t.Fatalf("probed another board: %s", port)
				}
				if tc.ready {
					return nil
				}
				return errors.New("port gone")
			}
			installedLitePortsFn = func() ([]discovery.SerialPortInfo, error) {
				return []discovery.SerialPortInfo{{Port: "/dev/another-board"}}, nil
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if tc.canceled {
				cancel()
			}
			port := original
			if tc.uart {
				port.Transport = discovery.SerialTransportUARTBridge
			}
			got, err := waitInstalledLite(ctx, port, false)
			if tc.wantErr == "" {
				if err != nil || got != original {
					t.Fatalf("got %+v, %v", got, err)
				}
			} else if err == nil || !strings.Contains(err.Error(), tc.wantErr) || got.Port != "" {
				t.Fatalf("got %+v, %v; want %q", got, err, tc.wantErr)
			}
		})
	}
}
