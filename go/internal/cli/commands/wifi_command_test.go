package commands

import (
	"context"
	"errors"
	"io"
	"os"
	"reflect"
	"testing"

	"github.com/wendylabsinc/wendy/go/internal/shared/models"
)

type wifiCommandProvider struct {
	fakeProvider
	connectCalls int
	device       models.ExternalDevice
	ssid         string
	password     string
	connectErr   error
}

func (p *wifiCommandProvider) WifiConnect(_ context.Context, device models.ExternalDevice, ssid, password string) error {
	p.connectCalls++
	p.device, p.ssid, p.password = device, ssid, password
	return p.connectErr
}

func (p *wifiCommandProvider) WifiDisconnect(context.Context, models.ExternalDevice) error {
	return errors.New("unexpected WiFi disconnect")
}

func stubWifiCommandTarget(t *testing.T, target *SelectedDevice, resolveErr error) *int {
	t.Helper()
	oldResolve, oldPick := resolveWifiTargetFn, pickWifiNetworkFn
	t.Cleanup(func() { resolveWifiTargetFn, pickWifiNetworkFn = oldResolve, oldPick })
	calls := 0
	resolveWifiTargetFn = func(context.Context, ...resolveOption) (*SelectedDevice, error) {
		calls++
		return target, resolveErr
	}
	return &calls
}

func stubWifiCommandStdin(t *testing.T) {
	t.Helper()
	stdin, err := os.CreateTemp(t.TempDir(), "stdin")
	if err != nil {
		t.Fatal(err)
	}
	previous := os.Stdin
	os.Stdin = stdin
	t.Cleanup(func() {
		os.Stdin = previous
		stdin.Close()
	})
}

func executeWifiCommand(args ...string) error {
	cmd := newWifiCmd()
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	cmd.SetArgs(append([]string{}, args...))
	return cmd.ExecuteContext(context.Background())
}

func TestWifiCommandLiteProviderConnect(t *testing.T) {
	pickerErr := errors.New("network scan failed")
	connectErr := errors.New("WiFi connection failed")
	for _, tc := range []struct {
		name       string
		pickerErr  error
		connectErr error
	}{
		{name: "selected network"},
		{name: "picker error", pickerErr: pickerErr},
		{name: "connection error", connectErr: connectErr},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stubWifiCommandStdin(t)
			provider := &wifiCommandProvider{connectErr: tc.connectErr}
			external := &models.ExternalDevice{ID: "lite-1", ProviderKey: "wendy-lite"}
			target := &SelectedDevice{Provider: provider, External: external}
			resolveCalls := stubWifiCommandTarget(t, target, nil)
			pickCalls := 0
			pickWifiNetworkFn = func(_ context.Context, got *SelectedDevice) (string, error) {
				pickCalls++
				if got != target {
					t.Fatal("network picker received a different target")
				}
				return "Office WiFi", tc.pickerErr
			}

			err := executeWifiCommand()
			wantErr := tc.pickerErr
			if wantErr == nil {
				wantErr = tc.connectErr
			}
			if !errors.Is(err, wantErr) {
				t.Fatalf("error = %v, want %v", err, wantErr)
			}
			if *resolveCalls != 1 || pickCalls != 1 {
				t.Fatalf("target resolutions = %d, network picks = %d; want one each", *resolveCalls, pickCalls)
			}
			if tc.pickerErr != nil {
				if provider.connectCalls != 0 {
					t.Fatal("picker failure attempted to connect")
				}
				return
			}
			if provider.connectCalls != 1 || provider.ssid != "Office WiFi" || provider.password != "" || !reflect.DeepEqual(provider.device, *external) {
				t.Fatalf("connection = %+v, want selected device and network", provider)
			}
		})
	}
}

func TestWifiCommandBLELiteUsesNetworkPicker(t *testing.T) {
	target := &SelectedDevice{Bluetooth: &models.BluetoothDevice{ID: "lite-ble"}}
	resolveCalls := stubWifiCommandTarget(t, target, nil)
	pickCalls := 0
	pickWifiNetworkFn = func(_ context.Context, got *SelectedDevice) (string, error) {
		pickCalls++
		if got != target {
			t.Fatal("network picker received a different target")
		}
		return "", ErrUserCancelled
	}

	if err := executeWifiCommand(); !errors.Is(err, ErrUserCancelled) {
		t.Fatalf("error = %v, want cancellation", err)
	}
	if *resolveCalls != 1 || pickCalls != 1 {
		t.Fatalf("target resolutions = %d, network picks = %d; want one each", *resolveCalls, pickCalls)
	}
}

func TestWifiConnectExplicitCredentials(t *testing.T) {
	provider := &wifiCommandProvider{}
	external := &models.ExternalDevice{ID: "lite-1", ProviderKey: "wendy-lite"}
	resolveCalls := stubWifiCommandTarget(t, &SelectedDevice{Provider: provider, External: external}, nil)
	pickWifiNetworkFn = func(context.Context, *SelectedDevice) (string, error) {
		t.Fatal("explicit SSID opened the network picker")
		return "", nil
	}

	if err := executeWifiCommand("connect", "--ssid", " Hidden network ", "--password", " password with spaces "); err != nil {
		t.Fatal(err)
	}
	if *resolveCalls != 1 || provider.connectCalls != 1 {
		t.Fatalf("target resolutions = %d, connections = %d; want one each", *resolveCalls, provider.connectCalls)
	}
	if provider.ssid != " Hidden network " || provider.password != " password with spaces " || !reflect.DeepEqual(provider.device, *external) {
		t.Fatalf("connection = %+v, want unchanged explicit credentials", provider)
	}
}

func TestWifiCommandTargetError(t *testing.T) {
	resolveErr := errors.New("device unavailable")
	resolveCalls := stubWifiCommandTarget(t, nil, resolveErr)
	pickWifiNetworkFn = func(context.Context, *SelectedDevice) (string, error) {
		t.Fatal("target resolution failure opened the network picker")
		return "", nil
	}

	if err := executeWifiCommand(); !errors.Is(err, resolveErr) {
		t.Fatalf("error = %v, want target resolution error", err)
	}
	if *resolveCalls != 1 {
		t.Fatalf("target resolutions = %d, want one", *resolveCalls)
	}
}
