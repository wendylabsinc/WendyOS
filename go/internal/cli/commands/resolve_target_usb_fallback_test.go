package commands

import (
	"context"
	"errors"
	"io"
	"reflect"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/cli/grpcclient"
	"github.com/wendylabsinc/wendy/go/internal/shared/certs"
	"github.com/wendylabsinc/wendy/go/internal/shared/config"
	"github.com/wendylabsinc/wendy/go/internal/shared/discovery"
	"github.com/wendylabsinc/wendy/go/internal/shared/models"
	"github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
)

const (
	// usbGadgetKey is the pin key of the board's fixed USB address itself: the
	// key the USB-direct fallback dials under, which holds no pin.
	usbGadgetKey = "fe80::5741:1%gadget"
	// usbGadgetAddr is where the fallback dials the board, and usbGadgetMTLSAddr
	// where an enrolled board answers.
	usbGadgetAddr     = "[fe80::5741:1%gadget]:50051"
	usbGadgetMTLSAddr = "[fe80::5741:1%gadget]:50052"
)

// usbOnlyDevice is a named device in the minute after it is plugged in over
// USB: its own address does not answer yet, but the board's fixed USB address
// does. Everything a connect touches is stubbed, so nothing here reaches the
// network, the user's config or a real board.
type usbOnlyDevice struct {
	// targets is every dial the ladder was handed, in order.
	targets []dialTarget
	// usbLookups counts enumerations of the USB interfaces, which only the
	// USB-direct fallback makes.
	usbLookups int
	// conn is the connection the USB address answers with; closed reports
	// whether it was closed.
	conn   *grpcclient.AgentConnection
	closed *recordingCloser
	// updateChecks and brokerSeeds record the steps that follow the pin check.
	updateChecks []*grpcclient.AgentConnection
	brokerSeeds  map[string]*grpcclient.AgentConnection
	// beforePrimaryFails runs inside the failing dial of the device's own
	// address. retryErr, when set, is how that address fails once recovery
	// dials it again.
	beforePrimaryFails func()
	retryErr           error
}

// stubUSBOnlyDevice installs cfg (given a login for org 7) and a device whose
// own address fails with primaryErr while a board on USB answers, reporting
// hostname and proving the identity obs.
func stubUSBOnlyDevice(t *testing.T, cfg *config.Config, primaryErr error, hostname string, obs observedDeviceIdentity) *usbOnlyDevice {
	t.Helper()
	restoreDeviceGlobals(t)
	// Fails the test on a time broadcast, a certificate prompt or a refresh.
	isolateConnectionRecovery(t)
	isInteractiveTerminalFn = func() bool { return false }
	deviceFlag = ""
	t.Setenv("WENDY_AGENT_SOCKET", "")
	cfg.Auth = []config.AuthConfig{{CloudGRPC: "grpc.a.sh:443", Certificates: []config.CertificateInfo{{OrganizationID: 7}}}}
	setTempConfig(t, cfg)
	setPinCache(t)

	d := &usbOnlyDevice{closed: &recordingCloser{}, brokerSeeds: map[string]*grpcclient.AgentConnection{}}
	d.conn = &grpcclient.AgentConnection{
		Host: usbGadgetKey, Addr: usbGadgetMTLSAddr, IsMTLS: obs.mTLS,
		AgentService: &fakeAgentVersionClient{resp: &agentpb.GetAgentVersionResponse{Hostname: hostname}},
		ExtraClosers: []io.Closer{d.closed},
	}

	origLookup, origBrowse, origLadder, origDiscover := osLookupHostFn, lanBrowseFn, dialAgentLadderFn, discoverLANDevices
	origObserve, origFind := observeDeviceIdentityFn, findDeviceByIDFn
	origCands, origPreDial := usbDirectCandidatesFn, usbDirectPreDialFn
	origBrokerConnect, origBrokerStart, origUpdate := connectSessionBrokerFn, startSessionBrokerFn, checkAndOfferUpdateFn
	t.Cleanup(func() {
		osLookupHostFn, lanBrowseFn, dialAgentLadderFn, discoverLANDevices = origLookup, origBrowse, origLadder, origDiscover
		observeDeviceIdentityFn, findDeviceByIDFn = origObserve, origFind
		usbDirectCandidatesFn, usbDirectPreDialFn = origCands, origPreDial
		connectSessionBrokerFn, startSessionBrokerFn, checkAndOfferUpdateFn = origBrokerConnect, origBrokerStart, origUpdate
	})
	osLookupHostFn = func(context.Context, string) ([]string, error) { return nil, errors.New("no resolver in test") }
	lanBrowseFn = func(context.Context, time.Duration) ([]models.LANDevice, error) { return nil, nil }
	discoverLANDevices = func(context.Context, time.Duration) ([]models.LANDevice, error) { return nil, nil }
	findDeviceByIDFn = func(context.Context, string) *SelectedDevice { return nil }
	// The fallback's own connect (connectWithAutoTLS) is the real one, so the
	// USB connection is keyed exactly as it is outside tests.
	dialAgentLadderFn = func(_ context.Context, target dialTarget) (*grpcclient.AgentConnection, error, error) {
		retry := len(d.targets) > 0
		d.targets = append(d.targets, target)
		if target.Addr == usbGadgetAddr || target.Addr == usbGadgetMTLSAddr {
			return d.conn, nil, nil
		}
		if d.beforePrimaryFails != nil {
			d.beforePrimaryFails()
		}
		if retry && d.retryErr != nil {
			return nil, nil, d.retryErr
		}
		return nil, nil, primaryErr
	}
	observeDeviceIdentityFn = func(*grpcclient.AgentConnection) observedDeviceIdentity { return obs }
	usbDirectCandidatesFn = func() []discovery.USBDirectCandidate {
		d.usbLookups++
		return []discovery.USBDirectCandidate{{Interface: "gadget", Zone: "gadget"}}
	}
	usbDirectPreDialFn = func(context.Context, discovery.USBDirectCandidate) bool { return true }
	connectSessionBrokerFn = func(context.Context, string, certs.WendyIdentity) (*grpcclient.AgentConnection, error) {
		return nil, errors.New("no reusable session")
	}
	startSessionBrokerFn = func(key string, conn *grpcclient.AgentConnection) error {
		d.brokerSeeds[key] = conn
		return nil
	}
	checkAndOfferUpdateFn = func(_ context.Context, conn *grpcclient.AgentConnection) (*grpcclient.AgentConnection, error) {
		d.updateChecks = append(d.updateChecks, conn)
		return conn, nil
	}
	return d
}

// savedPins reads the device pins as they stand on disk.
func savedPins(t *testing.T) map[string]config.DevicePin {
	t.Helper()
	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	return cfg.DevicePins
}

// resolveUSBOnlyDevice runs resolveTarget as a command without a terminal
// does. The context is closed on return, and the caller closes the selection.
func resolveUSBOnlyDevice(opts ...resolveOption) (*SelectedDevice, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	return resolveTarget(ctx, append([]resolveOption{NonInteractive()}, opts...)...)
}

// WDY-4295: for about a minute after a board is plugged in over USB, a named
// device's own address does not answer. resolveTarget — `wendy device info` and
// most other commands — then reaches the board at its fixed USB address, as
// connectToAgent already did.
func TestResolveTargetReachesAnUnreachableNamedDeviceOverUSB(t *testing.T) {
	// key is the pin key the name is dialled and judged under; pins are filed
	// under its normalized form, wendy-thor.
	for _, tc := range []struct{ device, key, addr string }{
		{"wendy-thor.local", "wendy-thor.local", "wendy-thor.local:50051"},
		{"wendy-thor.local:50051", "wendy-thor.local", "wendy-thor.local:50051"},
		{"wendy-thor", "wendy-thor", "wendy-thor:50051"},
	} {
		t.Run(tc.device, func(t *testing.T) {
			offline := errors.New("device offline in test")
			d := stubUSBOnlyDevice(t, &config.Config{}, offline, "wendy-thor", assetObs("42"))
			deviceFlag = tc.device

			sel, err := resolveUSBOnlyDevice()
			if err != nil {
				t.Fatalf("resolveTarget = %v, want the board on USB (USB interfaces consulted %d times)", err, d.usbLookups)
			}
			defer sel.Close()
			if sel.Agent != d.conn || d.closed.closed {
				t.Fatalf("resolveTarget returned %+v (closed %v), want the live USB connection", sel.Agent, d.closed.closed)
			}
			if d.usbLookups != 1 {
				t.Errorf("USB interfaces consulted %d times, want once", d.usbLookups)
			}
			if len(d.targets) == 0 || d.targets[0].PinKey != tc.key || d.targets[0].Addr != tc.addr {
				t.Errorf("dials = %+v, want the device's own address %s under %q first", d.targets, tc.addr, tc.key)
			}
			// First use is recorded under the name that was asked for, and
			// nothing is filed under the USB address.
			if pins := savedPins(t); len(pins) != 1 || pins["wendy-thor"].AssetID != "42" {
				t.Errorf("pins = %+v, want only wendy-thor naming asset 42", pins)
			}
			if got := sel.Agent.DevicePinKey; got != tc.key {
				t.Errorf("the USB connection is kept as %q, want the requested %q", got, tc.key)
			}
			// It is not the address that was asked for, so it is never the typed one.
			if sel.Agent.SimulatorName != "" {
				t.Errorf("the USB connection was marked as VM %q", sel.Agent.SimulatorName)
			}
			if len(d.updateChecks) != 1 || d.updateChecks[0] != d.conn {
				t.Errorf("update checks = %v, want one, on the USB connection", d.updateChecks)
			}
			if len(d.brokerSeeds) != 1 || d.brokerSeeds[tc.addr] != d.conn {
				t.Errorf("session seeded as %v, want the USB connection under the requested endpoint %s", d.brokerSeeds, tc.addr)
			}
		})
	}
}

// The board on USB is used only when it reports the requested name; otherwise
// the failure of the device's own address is what the command reports.
func TestResolveTargetUSBFallbackNeedsTheRequestedHostname(t *testing.T) {
	for name, hostname := range map[string]string{
		"another board":           "wendy-pi",
		"agent reporting no name": "",
	} {
		t.Run(name, func(t *testing.T) {
			offline := errors.New("device offline in test")
			d := stubUSBOnlyDevice(t, &config.Config{}, offline, hostname, assetObs("42"))
			deviceFlag = "wendy-thor.local:50051"

			sel, err := resolveUSBOnlyDevice()
			if sel != nil {
				sel.Close()
				t.Fatalf("a board reporting %q was used as wendy-thor.local", hostname)
			}
			if !errors.Is(err, offline) {
				t.Fatalf("err = %v, want the original failure", err)
			}
			if d.usbLookups != 1 || !d.closed.closed {
				t.Errorf("USB consulted %d times, connection closed %v; want one attempt, closed", d.usbLookups, d.closed.closed)
			}
			if pins := savedPins(t); len(pins) != 0 {
				t.Errorf("pins = %+v, want none", pins)
			}
			if len(d.updateChecks) != 0 || len(d.brokerSeeds) != 0 {
				t.Errorf("update checks %v, session seeds %v; want neither", d.updateChecks, d.brokerSeeds)
			}
		})
	}
}

// The fallback is tried under the conditions connectToAgent tries it under: not
// for a finished command, a refusal, missing credentials, enrollment's own
// trust prompt or read-only monitoring, and never for a VM's key.
func TestResolveTargetDoesNotFallBackToUSB(t *testing.T) {
	const device, key, addr = "wendy-thor.local", "wendy-thor.local", "wendy-thor.local:50051"
	offline := errors.New("device offline in test")
	nothingAnswered := pinnedHostNoAuthenticatedEndpointError(key, []string{addr}, nil, false)
	saved := map[string]config.DevicePin{"wendy-thor": pinA}
	for _, tc := range []struct {
		name    string
		pins    map[string]config.DevicePin
		primary error
		opts    []resolveOption
		arrange func(d *usbOnlyDevice, cancel context.CancelFunc)
		want    error
	}{
		{name: "command cancelled", primary: offline, want: context.Canceled,
			arrange: func(d *usbOnlyDevice, cancel context.CancelFunc) { d.beforePrimaryFails = cancel }},
		{name: "another device answered", primary: refuseIdentity("device %q presented another device's identity", key), want: errDeviceIdentityRefused},
		{name: "saved binding names no device", primary: certs.ErrExpectedDeviceIdentity, want: certs.ErrExpectedDeviceIdentity},
		{name: "no login the device accepts", primary: errProvisionedAgentUnauthorized, want: errProvisionedAgentUnauthorized},
		{name: "enrollment asks to trust the device itself", pins: saved, primary: nothingAnswered, want: errNoAuthenticatedEndpoint,
			opts: []resolveOption{func(c *resolveConfig) { c.enrollmentTrustRecovery = true }}},
		// The same answer, reached only once clock recovery dialled again.
		{name: "enrollment asks to trust the device itself, after a retry", pins: saved, want: errNoAuthenticatedEndpoint,
			primary: newTLSHandshakeRejectedError(errors.New("remote error: tls: bad certificate")),
			opts:    []resolveOption{func(c *resolveConfig) { c.enrollmentTrustRecovery = true }},
			arrange: func(d *usbOnlyDevice, _ context.CancelFunc) {
				d.retryErr = nothingAnswered
				broadcastTimeFn = func(context.Context) error { return nil }
			}},
		{name: "read-only monitoring", primary: offline, want: offline, opts: []resolveOption{ReadOnlyMonitoring()}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := stubUSBOnlyDevice(t, &config.Config{DevicePins: tc.pins}, tc.primary, "wendy-thor", assetObs("42"))
			deviceFlag = device
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			if tc.arrange != nil {
				tc.arrange(d, cancel)
			}

			sel, err := resolveTarget(ctx, append([]resolveOption{NonInteractive()}, tc.opts...)...)
			if sel != nil {
				sel.Close()
				t.Fatal("connected to the board on USB")
			}
			if !errors.Is(err, tc.want) {
				t.Fatalf("err = %v, want %v", err, tc.want)
			}
			if d.usbLookups != 0 {
				t.Errorf("USB interfaces consulted %d times, want never", d.usbLookups)
			}
		})
	}

	// A deadline that passed while the device's own address was being dialled
	// ends the command too.
	t.Run("deadline passed", func(t *testing.T) {
		d := stubUSBOnlyDevice(t, &config.Config{}, offline, "wendy-thor", assetObs("42"))
		deviceFlag = device
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
		defer cancel()
		d.beforePrimaryFails = func() { <-ctx.Done() }

		sel, err := resolveTarget(ctx, NonInteractive())
		if sel != nil {
			sel.Close()
			t.Fatal("connected to the board on USB after the deadline")
		}
		if !errors.Is(err, context.DeadlineExceeded) || d.usbLookups != 0 {
			t.Fatalf("err = %v, USB consulted %d times; want the deadline and no USB attempt", err, d.usbLookups)
		}
	})

	// A VM is never on USB: a board that merely reports the VM's key would be
	// judged under a key that consults no loopback pin.
	t.Run("a VM's key", func(t *testing.T) {
		d := stubUSBOnlyDevice(t, &config.Config{}, offline, "vm:dev", assetObs("42"))
		stubLoopbackVMs(t, map[int]string{50051: "dev"})
		deviceFlag = "127.0.0.1:50051"

		sel, err := resolveUSBOnlyDevice()
		if sel != nil {
			sel.Close()
			t.Fatal("a board reporting vm:dev was used for 127.0.0.1:50051")
		}
		if !errors.Is(err, offline) || d.usbLookups != 0 {
			t.Fatalf("err = %v, USB consulted %d times; want the original failure and no USB attempt", err, d.usbLookups)
		}
	})
}

// A saved default that is on USB is used before the user is asked to pick
// another device.
func TestResolveTargetReachesTheDefaultDeviceOverUSBBeforeRecovery(t *testing.T) {
	for _, interactive := range []bool{false, true} {
		t.Run(map[bool]string{false: "no terminal", true: "terminal"}[interactive], func(t *testing.T) {
			d := stubUSBOnlyDevice(t, &config.Config{DefaultDevice: "wendy-thor.local"}, errors.New("device offline in test"), "wendy-thor", assetObs("42"))
			origSpinner, origConfirm, origPicker := runAgentConnectionSpinner, confirmDefaultRecoveryFn, pickDefaultRecoveryDeviceFn
			t.Cleanup(func() {
				runAgentConnectionSpinner, confirmDefaultRecoveryFn, pickDefaultRecoveryDeviceFn = origSpinner, origConfirm, origPicker
			})
			isInteractiveTerminalFn = func() bool { return interactive }
			runAgentConnectionSpinner = func(ctx context.Context, _ string, connect func(context.Context) (*grpcclient.AgentConnection, error)) (*grpcclient.AgentConnection, error) {
				return connect(ctx)
			}
			confirmDefaultRecoveryFn = func(string) (bool, error) {
				t.Error("asked to pick another device although the default is on USB")
				return false, nil
			}
			pickDefaultRecoveryDeviceFn = func(context.Context, map[string]bool, bool, bool, bool) (*SelectedDevice, error) {
				t.Error("opened the device picker although the default is on USB")
				return nil, errors.New("no picker in test")
			}

			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			sel, err := resolveTarget(ctx)
			if err != nil {
				t.Fatalf("resolveTarget = %v, want the default device on USB", err)
			}
			defer sel.Close()
			if sel.Agent != d.conn {
				t.Fatalf("resolveTarget returned %+v, want the USB connection", sel.Agent)
			}
		})
	}
}

// The board on USB is still the named device's stand-in, not a new device: it
// is judged against the identity saved under the requested name before
// anything else is done over it.
func TestResolveTargetJudgesTheUSBConnectionUnderTheRequestedName(t *testing.T) {
	const device, addr = "wendy-thor.local", "wendy-thor.local:50051"
	saved := map[string]config.DevicePin{"wendy-thor": pinA}
	nothingAnswered := func() error { return pinnedHostNoAuthenticatedEndpointError(device, []string{addr}, nil, false) }

	for _, tc := range []struct {
		name string
		obs  observedDeviceIdentity
	}{
		{"another device with the same name", assetObs("99")},
		{"a board that is no longer enrolled", observedDeviceIdentity{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := stubUSBOnlyDevice(t, &config.Config{DevicePins: saved}, nothingAnswered(), "wendy-thor", tc.obs)
			deviceFlag = device

			sel, err := resolveUSBOnlyDevice()
			if sel != nil {
				sel.Close()
				t.Fatal("connected to a board that is not the saved device")
			}
			if !errors.Is(err, errDeviceIdentityRefused) {
				t.Fatalf("err = %v, want an identity refusal", err)
			}
			if d.usbLookups != 1 || !d.closed.closed {
				t.Errorf("USB consulted %d times, connection closed %v; want one attempt, closed", d.usbLookups, d.closed.closed)
			}
			if pins := savedPins(t); !reflect.DeepEqual(pins, saved) {
				t.Errorf("pins = %+v, want the saved identity unchanged", pins)
			}
			// An update can upload an agent binary: never to a refused device.
			if len(d.updateChecks) != 0 || len(d.brokerSeeds) != 0 {
				t.Errorf("update checks %v, session seeds %v; want neither for a refused device", d.updateChecks, d.brokerSeeds)
			}
		})
	}

	t.Run("the saved device", func(t *testing.T) {
		d := stubUSBOnlyDevice(t, &config.Config{DevicePins: saved}, nothingAnswered(), "wendy-thor", assetObs("42"))
		deviceFlag = device

		sel, err := resolveUSBOnlyDevice()
		if err != nil {
			t.Fatalf("resolveTarget = %v, want the saved device on USB", err)
		}
		defer sel.Close()
		if sel.Agent != d.conn {
			t.Fatalf("resolveTarget returned %+v, want the USB connection", sel.Agent)
		}
		if pins := savedPins(t); !reflect.DeepEqual(pins, saved) {
			t.Errorf("pins = %+v, want the saved identity unchanged", pins)
		}

		// A reconnect after an agent or OS update redials the address that
		// answered. It must do so as the requested device, held to its saved
		// identity — not under the USB address's own key, which has no pin and
		// would accept whichever enrolled board is on the cable.
		d.targets = nil
		again, err := dialOSUpdateAgent(context.Background(), sel.Agent)
		if err != nil {
			t.Fatal(err)
		}
		if again != sel.Agent {
			again.Close()
		}
		if len(d.targets) != 1 {
			t.Fatalf("reconnect dials = %+v, want one", d.targets)
		}
		got := d.targets[0]
		if got.Addr != usbGadgetMTLSAddr || got.PinKey != device || got.Expected == nil || got.Expected.EntityID != "42" {
			t.Fatalf("reconnect dialled %s under %q expecting %+v, want %s under %q expecting asset 42",
				got.Addr, got.PinKey, got.Expected, usbGadgetMTLSAddr, device)
		}
	})
}
