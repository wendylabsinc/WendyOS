package commands

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/spf13/cobra"
	"github.com/wendylabsinc/wendy/go/internal/cli/grpcclient"
	"github.com/wendylabsinc/wendy/go/internal/shared/config"
	"github.com/wendylabsinc/wendy/go/internal/shared/models"
)

func TestEnrollmentUsesExistingCommands(t *testing.T) {
	cloud := newCloudCmd()
	for _, cmd := range cloud.Commands() {
		if cmd.Name() == "enroll-lite" {
			t.Fatal("Lite must use enroll-device, not a separate command")
		}
	}
	cmd, _, err := cloud.Find([]string{"enroll-device"})
	if err != nil || cmd.Name() != "enroll-device" || cmd.Hidden {
		t.Fatalf("existing Cloud enrollment command unavailable: %v", err)
	}
	for _, cmd := range []*cobra.Command{cmd, newDeviceEnrollCmd()} {
		for _, flag := range []string{"name", "org", "cloud-grpc", "acme-directory-url", "broker-host", "broker-port", "csr-url", "time-url", "device-roots", "tsa-roots", "https-roots"} {
			if cmd.Flags().Lookup(flag) == nil {
				t.Errorf("%s is missing --%s", cmd.Name(), flag)
			}
		}
		if err := cmd.ValidateRequiredFlags(); err != nil {
			t.Fatalf("Lite flags became mandatory for WendyOS: %v", err)
		}
		if cmd.Flags().Lookup("serial") != nil {
			t.Fatal("device selection must use the normal --device flag and picker")
		}
	}
}

func TestEnrollmentDispatchesSelectedProtocol(t *testing.T) {
	lite, agent, wifi := runLiteEnrollmentFn, runAgentEnrollmentFn, promptEnrollmentWifiFn
	t.Cleanup(func() {
		runLiteEnrollmentFn, runAgentEnrollmentFn, promptEnrollmentWifiFn = lite, agent, wifi
	})
	ctx := context.Background()
	cmd := newCloudEnrollDeviceCmd()
	cmd.SetContext(ctx)
	auth := &config.AuthConfig{CloudGRPC: "cloud.example:443"}
	opts := liteEnrollmentOptions{brokerHost: "broker.example", brokerPort: 5055, csrURL: "https://csr.example/v1/tenant", timeURL: "https://time.example/v1/time"}
	wantErr := errors.New("enrollment result")
	var calls []string
	runLiteEnrollmentFn = func(gotCmd *cobra.Command, port string, gotAuth *config.AuthConfig, name string, gotOpts liteEnrollmentOptions) error {
		calls = append(calls, "lite")
		if gotCmd != cmd || port != "/dev/cu.usbmodem123" || gotAuth != auth || name != "desk" || gotOpts != opts {
			t.Fatal("Lite did not receive the selected USB connection, auth, name, and PKI configuration")
		}
		return wantErr
	}
	conn := &grpcclient.AgentConnection{Host: "desk.local"}
	promptEnrollmentWifiFn = func(gotCtx context.Context, gotConn *grpcclient.AgentConnection) error {
		calls = append(calls, "wifi")
		if gotCtx != ctx || gotConn != conn {
			t.Fatal("Wi-Fi prompt received a different target")
		}
		return nil
	}
	runAgentEnrollmentFn = func(gotCtx context.Context, gotConn *grpcclient.AgentConnection, gotAuth *config.AuthConfig, name string, org int32, acme ...string) error {
		calls = append(calls, "agent")
		if gotCtx != ctx || gotConn != conn || gotAuth != auth || name != "desk" || org != 27 || len(acme) != 1 || acme[0] != "https://acme.example/directory" {
			t.Fatal("WendyOS enrollment arguments changed")
		}
		return wantErr
	}
	usb := &SelectedDevice{External: &models.ExternalDevice{ProviderKey: "wendy-lite", ConnectionInfo: map[string]string{"type": "USB", "serialPort": "/dev/cu.usbmodem123"}}}
	if err := runSelectedDeviceEnrollment(cmd, usb, auth, "desk", 0, "", opts); !errors.Is(err, wantErr) {
		t.Fatalf("Lite result: %v", err)
	}
	if err := runSelectedDeviceEnrollment(cmd, &SelectedDevice{Agent: conn}, auth, "desk", 27, "https://acme.example/directory", liteEnrollmentOptions{}); !errors.Is(err, wantErr) {
		t.Fatalf("WendyOS result: %v", err)
	}
	if strings.Join(calls, ",") != "lite,wifi,agent" {
		t.Fatalf("unexpected protocol calls: %v", calls)
	}
	for _, tc := range []struct {
		name, provider, transport, port, broker, acme, want string
		org                                                 int32
	}{
		{name: "LAN", provider: "wendy-lite", transport: "LAN", want: "physical USB"},
		{name: "BLE", provider: "wendy-lite", transport: "BLE", want: "physical USB"},
		{name: "missing port", provider: "wendy-lite", transport: "USB", want: "no USB serial port"},
		{name: "legacy org", provider: "wendy-lite", transport: "USB", port: "/dev/ttyUSB0", broker: "broker.example", org: 27, want: "--org and --acme-directory-url do not apply"},
		{name: "ACME", provider: "wendy-lite", transport: "USB", port: "/dev/ttyUSB0", broker: "broker.example", acme: "https://acme.example/directory", want: "--org and --acme-directory-url do not apply"},
		{name: "other provider", provider: "android-adb", transport: "USB", port: "/dev/ttyUSB0", want: "does not support this command"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			target := &SelectedDevice{External: &models.ExternalDevice{ProviderKey: tc.provider, ConnectionInfo: map[string]string{"type": tc.transport, "serialPort": tc.port}}}
			err := runSelectedDeviceEnrollment(cmd, target, auth, "desk", tc.org, tc.acme, liteEnrollmentOptions{brokerHost: tc.broker})
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("got %v, want %q", err, tc.want)
			}
			if len(calls) != 3 {
				t.Fatal("invalid target or flags reached an enrollment protocol")
			}
		})
	}
	opts.brokerHost = ""
	if err := runSelectedDeviceEnrollment(cmd, usb, auth, "desk", 0, "", opts); !errors.Is(err, wantErr) {
		t.Fatalf("Lite enrollment without a broker override: %v", err)
	}
}

func TestEnrollmentResolvesUSBDeviceIDWithDots(t *testing.T) {
	t.Setenv("WENDY_AGENT_SOCKET", "")
	old := findDeviceByIDFn
	t.Cleanup(func() { findDeviceByIDFn = old })
	id := "wendy-lite:/dev/cu.usbmodem123"
	want := &SelectedDevice{External: &models.ExternalDevice{ID: id, ProviderKey: "wendy-lite", ConnectionInfo: map[string]string{"type": "USB", "serialPort": "/dev/cu.usbmodem123"}}}
	findDeviceByIDFn = func(_ context.Context, gotID string) *SelectedDevice {
		if gotID != id {
			t.Fatalf("discovered %q instead of selected USB device %q", gotID, id)
		}
		return want
	}
	got, err := resolveTarget(context.Background(), SelectDevice(id), SuppressProvisioningHint(), SuppressPickerEnroll())
	if err != nil || got != want {
		t.Fatalf("USB selector fell through to agent dialing: %v, %v", got, err)
	}
}

func TestEnrollmentDoesNotDialMissingLiteAsAgent(t *testing.T) {
	t.Setenv("WENDY_AGENT_SOCKET", "")
	old := findDeviceByIDFn
	t.Cleanup(func() { findDeviceByIDFn = old })
	findDeviceByIDFn = func(context.Context, string) *SelectedDevice { return nil }
	got, err := resolveTarget(context.Background(), SelectDevice("wendy-lite:/dev/cu.usbmodem123"), SuppressPickerEnroll())
	if got != nil || err == nil || !strings.Contains(err.Error(), "was not found") {
		t.Fatalf("missing Lite device fell through to agent dialing: %v, %v", got, err)
	}
}

func TestEnrollmentStopsWhenWifiIsCancelled(t *testing.T) {
	wifi, agent := promptEnrollmentWifiFn, runAgentEnrollmentFn
	t.Cleanup(func() { promptEnrollmentWifiFn, runAgentEnrollmentFn = wifi, agent })
	promptEnrollmentWifiFn = func(context.Context, *grpcclient.AgentConnection) error { return ErrUserCancelled }
	runAgentEnrollmentFn = func(context.Context, *grpcclient.AgentConnection, *config.AuthConfig, string, int32, ...string) error {
		t.Fatal("enrolled after cancellation")
		return nil
	}
	cmd := newCloudEnrollDeviceCmd()
	cmd.SetContext(context.Background())
	err := runSelectedDeviceEnrollment(cmd, &SelectedDevice{Agent: &grpcclient.AgentConnection{}}, &config.AuthConfig{}, "test", 0, "", liteEnrollmentOptions{})
	if !errors.Is(err, ErrUserCancelled) {
		t.Fatalf("got %v", err)
	}
}
