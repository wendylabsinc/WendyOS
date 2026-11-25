package commands

import (
	"context"
	"errors"
	"io"
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/wendylabsinc/wendy/go/internal/cli/grpcclient"
	"github.com/wendylabsinc/wendy/go/internal/cli/tui"
	"github.com/wendylabsinc/wendy/go/internal/shared/models"
	"github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
	"google.golang.org/grpc"
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
	return executeWifiCommandContext(context.Background(), args...)
}

func executeWifiCommandContext(ctx context.Context, args ...string) error {
	cmd := newWifiCmd()
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	cmd.SetArgs(append([]string{}, args...))
	return cmd.ExecuteContext(ctx)
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
			pickWifiNetworkFn = func(_ context.Context, got *SelectedDevice) (wifiPick, error) {
				pickCalls++
				if got != target {
					t.Fatal("network picker received a different target")
				}
				return wifiPick{SSID: "Office WiFi"}, tc.pickerErr
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
	pickWifiNetworkFn = func(_ context.Context, got *SelectedDevice) (wifiPick, error) {
		pickCalls++
		if got != target {
			t.Fatal("network picker received a different target")
		}
		return wifiPick{}, ErrUserCancelled
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
	pickWifiNetworkFn = func(context.Context, *SelectedDevice) (wifiPick, error) {
		t.Fatal("explicit SSID opened the network picker")
		return wifiPick{}, nil
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
	pickWifiNetworkFn = func(context.Context, *SelectedDevice) (wifiPick, error) {
		t.Fatal("target resolution failure opened the network picker")
		return wifiPick{}, nil
	}

	if err := executeWifiCommand(); !errors.Is(err, resolveErr) {
		t.Fatalf("error = %v, want target resolution error", err)
	}
	if *resolveCalls != 1 {
		t.Fatalf("target resolutions = %d, want one", *resolveCalls)
	}
}

const (
	wpa2Network = agentpb.WiFiSecurityType_WIFI_SECURITY_TYPE_WPA2_PSK
	openNetwork = agentpb.WiFiSecurityType_WIFI_SECURITY_TYPE_OPEN
)

// stubWifiPick makes the network picker choose picked.
func stubWifiPick(picked wifiPick) {
	pickWifiNetworkFn = func(context.Context, *SelectedDevice) (wifiPick, error) { return picked, nil }
}

// stubWifiPasswordPrompt answers the password prompt with answers in turn and
// records what each prompt said.
func stubWifiPasswordPrompt(t *testing.T, answers ...string) *[]string {
	t.Helper()
	old := readPasswordFn
	t.Cleanup(func() { readPasswordFn = old })
	prompts := []string{}
	readPasswordFn = func(prompt string) (string, error) {
		prompts = append(prompts, prompt)
		if len(prompts) > len(answers) {
			t.Fatalf("password asked for %d times; want %d", len(prompts), len(answers))
		}
		return answers[len(prompts)-1], nil
	}
	return &prompts
}

// stubKeychainQuestion declines the keychain lookup and counts the questions.
func stubKeychainQuestion(t *testing.T) *int {
	t.Helper()
	old := confirmFn
	t.Cleanup(func() { confirmFn = old })
	asked := 0
	confirmFn = func(string) bool { asked++; return false }
	return &asked
}

func TestWifiConnectPasswordPromptFollowsPickedNetwork(t *testing.T) {
	const (
		anyNetwork   = "Password (leave empty for open networks): "
		savedNetwork = "Password (leave empty to use the saved one): "
		newNetwork   = "Password: "
		notice       = `"MyNet" is secured (WPA2) and needs a password.`
	)
	for _, tc := range []struct {
		name     string
		args     []string
		picked   wifiPick
		answers  []string
		prompts  []string
		keychain bool // asked where this machine has a keychain to look in
		notices  int
		password string
	}{
		{name: "open network", picked: wifiPick{SSID: "MyNet", Security: openNetwork}},
		{name: "saved open network", picked: wifiPick{SSID: "MyNet", Security: openNetwork, Saved: true}},
		{name: "saved network", picked: wifiPick{SSID: "MyNet", Security: wpa2Network, Saved: true},
			answers: []string{""}, prompts: []string{savedNetwork}, keychain: true},
		{name: "saved network with a new password", picked: wifiPick{SSID: "MyNet", Security: wpa2Network, Saved: true},
			answers: []string{"new-password"}, prompts: []string{savedNetwork}, keychain: true, password: "new-password"},
		{name: "secured network", picked: wifiPick{SSID: "MyNet", Security: wpa2Network},
			answers: []string{"hunter22"}, prompts: []string{newNetwork}, keychain: true, password: "hunter22"},
		{name: "secured network with empty answers", picked: wifiPick{SSID: "MyNet", Security: wpa2Network},
			answers: []string{"", "  ", "hunter22"}, prompts: []string{newNetwork, newNetwork, newNetwork},
			keychain: true, notices: 2, password: "hunter22"},
		{name: "network of unknown security", picked: wifiPick{SSID: "MyNet"},
			answers: []string{""}, prompts: []string{anyNetwork}, keychain: true},
		{name: "network of a security this CLI can't name", picked: wifiPick{SSID: "MyNet", Security: agentpb.WiFiSecurityType(99)},
			answers: []string{""}, prompts: []string{anyNetwork}, keychain: true},
		{name: "network named with --ssid", args: []string{"--ssid", "MyNet"},
			answers: []string{""}, prompts: []string{anyNetwork}, keychain: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stubHumanPresent(t, true)
			provider := &wifiCommandProvider{}
			stubWifiCommandTarget(t, &SelectedDevice{Provider: provider, External: &models.ExternalDevice{ID: "lite-1"}}, nil)
			stubWifiPick(tc.picked)
			prompts := stubWifiPasswordPrompt(t, tc.answers...)
			asked := stubKeychainQuestion(t)

			var err error
			out := captureStdout(t, func() { err = executeWifiCommand(append([]string{"connect"}, tc.args...)...) })
			if err != nil {
				t.Fatal(err)
			}
			if want := append([]string{}, tc.prompts...); !reflect.DeepEqual(*prompts, want) {
				t.Errorf("prompts = %q, want %q", *prompts, want)
			}
			if want := tc.keychain && supportsKeychainLookup; (*asked > 0) != want {
				t.Errorf("keychain questions = %d, want asked %v", *asked, want)
			}
			if got := strings.Count(out, notice); got != tc.notices {
				t.Errorf("%q printed %d times, want %d:\n%s", notice, got, tc.notices, out)
			}
			if provider.connectCalls != 1 || provider.ssid != "MyNet" || provider.password != tc.password {
				t.Errorf("connection = %+v, want one to MyNet with password %q", provider, tc.password)
			}
		})
	}
}

func TestWifiConnectPasswordPromptStops(t *testing.T) {
	for _, tc := range []struct {
		name string
		read func(cancel context.CancelFunc) (string, error)
		want error
	}{
		{name: "input ends", read: func(context.CancelFunc) (string, error) { return "", io.EOF }, want: io.EOF},
		// Ctrl+C cancels the command; the read still returns the empty line after it.
		{name: "ctrl+c", read: func(cancel context.CancelFunc) (string, error) { cancel(); return "", nil }, want: ErrUserCancelled},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stubHumanPresent(t, true)
			provider := &wifiCommandProvider{}
			stubWifiCommandTarget(t, &SelectedDevice{Provider: provider, External: &models.ExternalDevice{ID: "lite-1"}}, nil)
			stubWifiPick(wifiPick{SSID: "MyNet", Security: wpa2Network})
			stubKeychainQuestion(t)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			old := readPasswordFn
			t.Cleanup(func() { readPasswordFn = old })
			asks := 0
			readPasswordFn = func(string) (string, error) {
				if asks++; asks > 1 {
					t.Fatal("password asked for again after the prompt was abandoned")
				}
				return tc.read(cancel)
			}

			var err error
			out := captureStdout(t, func() { err = executeWifiCommandContext(ctx, "connect") })
			if !errors.Is(err, tc.want) {
				t.Fatalf("error = %v, want %v", err, tc.want)
			}
			if strings.Contains(out, "needs a password") || provider.connectCalls != 0 {
				t.Fatalf("output %q, connections = %d; want neither a notice nor a connection", out, provider.connectCalls)
			}
		})
	}
}

func TestWifiConnectDoesNotPromptWithoutTerminalOrWithPassword(t *testing.T) {
	for name, args := range map[string][]string{
		"no terminal":   {"connect"},
		"with password": {"connect", "--password", ""},
	} {
		t.Run(name, func(t *testing.T) {
			stubHumanPresent(t, name != "no terminal")
			provider := &wifiCommandProvider{}
			stubWifiCommandTarget(t, &SelectedDevice{Provider: provider, External: &models.ExternalDevice{ID: "lite-1"}}, nil)
			stubWifiPick(wifiPick{SSID: "MyNet", Security: wpa2Network})
			prompts := stubWifiPasswordPrompt(t)
			asked := stubKeychainQuestion(t)

			if err := executeWifiCommand(args...); err != nil {
				t.Fatal(err)
			}
			if len(*prompts) != 0 || *asked != 0 || provider.connectCalls != 1 || provider.password != "" {
				t.Fatalf("prompts = %q, keychain questions = %d, connection = %+v; want only a connection with no password", *prompts, *asked, provider)
			}
		})
	}
}

type wifiConnectRecorder struct {
	agentpb.WendyAgentServiceClient
	request *agentpb.ConnectToWiFiRequest
}

func (c *wifiConnectRecorder) ConnectToWiFi(_ context.Context, req *agentpb.ConnectToWiFiRequest, _ ...grpc.CallOption) (*agentpb.ConnectToWiFiResponse, error) {
	c.request = req
	return &agentpb.ConnectToWiFiResponse{Success: true}, nil
}

// The device chooses key management from its own scan; a hint from the picker
// would override that choice.
func TestWifiConnectSendsNoSecurityHint(t *testing.T) {
	stubHumanPresent(t, true)
	client := &wifiConnectRecorder{}
	stubWifiCommandTarget(t, &SelectedDevice{Agent: &grpcclient.AgentConnection{AgentService: client}}, nil)
	stubWifiPick(wifiPick{SSID: "MyNet", Security: agentpb.WiFiSecurityType_WIFI_SECURITY_TYPE_WPA3_SAE})
	stubWifiPasswordPrompt(t, "hunter22")
	stubKeychainQuestion(t)

	var err error
	captureStdout(t, func() { err = executeWifiCommand("connect") })
	if err != nil {
		t.Fatal(err)
	}
	if got, want := client.request, (&agentpb.ConnectToWiFiRequest{Ssid: "MyNet", Password: "hunter22"}); got == nil ||
		got.GetSsid() != want.GetSsid() || got.GetPassword() != want.GetPassword() || got.Security != nil || got.Hidden != nil {
		t.Fatalf("request = %v, want only the SSID and password", got)
	}
}

func TestWifiPickFromPickerRow(t *testing.T) {
	deviceRows := wifiPickerItems([]*agentpb.ListWiFiNetworksResponse_WiFiNetwork{
		{Ssid: "Home", Security: wpa2Network, IsKnown: true},
		{Ssid: "Cafe", Security: openNetwork},
	})
	hostRows := localWifiPickerItems([]localWifiNetwork{{SSID: "Home", Security: "WPA2"}})
	for name, tc := range map[string]struct {
		row  tui.PickerItem
		want wifiPick
	}{
		"saved network the device scanned": {deviceRows[0], wifiPick{SSID: "Home", Security: wpa2Network, Saved: true}},
		"open network the device scanned":  {deviceRows[1], wifiPick{SSID: "Cafe", Security: openNetwork}},
		"network this machine scanned":     {hostRows[0], wifiPick{SSID: "Home"}},
	} {
		if got, ok := wifiPickFrom(&tc.row); !ok || got != tc.want {
			t.Errorf("%s: pick = %+v, %v; want %+v", name, got, ok, tc.want)
		}
	}
	if _, ok := wifiPickFrom(&tui.PickerItem{Value: 7}); ok {
		t.Error("a row with no network was read as a pick")
	}
}
