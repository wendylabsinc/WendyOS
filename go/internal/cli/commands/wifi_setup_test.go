package commands

import (
	"context"
	"errors"
	"net"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/cli/grpcclient"
	"github.com/wendylabsinc/wendy/go/internal/cli/tui"
	"github.com/wendylabsinc/wendy/go/internal/shared/certs"
	"github.com/wendylabsinc/wendy/go/internal/shared/config"
	"github.com/wendylabsinc/wendy/go/internal/shared/models"
	"github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
	pb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

type firstBootWiFiProbe struct {
	setupIdentityProbe
	connects atomic.Int32
}

func (s *firstBootWiFiProbe) IsProvisioned(ctx context.Context, req *pb.IsProvisionedRequest) (*pb.IsProvisionedResponse, error) {
	if s.state == "error" {
		return nil, status.Error(codes.Unavailable, "enrollment status unavailable")
	}
	return s.setupIdentityProbe.IsProvisioned(ctx, req)
}

func (*firstBootWiFiProbe) ListWiFiNetworks(context.Context, *agentpb.ListWiFiNetworksRequest) (*agentpb.ListWiFiNetworksResponse, error) {
	return &agentpb.ListWiFiNetworksResponse{Networks: []*agentpb.ListWiFiNetworksResponse_WiFiNetwork{{Ssid: "Setup WiFi"}}}, nil
}

func (s *firstBootWiFiProbe) ConnectToWiFi(_ context.Context, req *agentpb.ConnectToWiFiRequest) (*agentpb.ConnectToWiFiResponse, error) {
	if req.GetSsid() != "Setup WiFi" || req.GetPassword() != "fixture-password" {
		return nil, status.Error(codes.InvalidArgument, "unexpected credentials")
	}
	s.connects.Add(1)
	return &agentpb.ConnectToWiFiResponse{Success: true}, nil
}

func TestWiFiFirstBootSetup(t *testing.T) {
	for _, selection := range []string{"explicit", "default", "picker"} {
		for _, state := range []string{"unprovisioned", "enrolled", "unknown", "error", "saved identity", "legacy pin"} {
			t.Run(selection+"/"+state, func(t *testing.T) {
				restoreDeviceGlobals(t)
				deviceFlag, jsonOutput = "", false
				listener, err := net.Listen("tcp", "127.0.0.1:0")
				if err != nil {
					t.Fatal(err)
				}
				service := &firstBootWiFiProbe{setupIdentityProbe: setupIdentityProbe{state: state}}
				if state == "saved identity" || state == "legacy pin" {
					// An untrusted peer claiming to be fresh must not bypass an existing binding.
					service.state = "unprovisioned"
				}
				server := grpc.NewServer()
				agentpb.RegisterWendyAgentServiceServer(server, service)
				pb.RegisterWendyProvisioningServiceServer(server, service)
				go server.Serve(listener)
				t.Cleanup(server.Stop)
				cfg := &config.Config{Auth: []config.AuthConfig{{Certificates: []config.CertificateInfo{selfSignedCLICert(t, 2)}}}}
				key := "127.0.0.1"
				if selection == "picker" {
					key = "new-robot.local"
				}
				if state == "saved identity" {
					cfg.SetDevicePin(key, 2, "cloud.test:443", "42", "")
				}
				if state == "legacy pin" {
					cfg.SetDevicePin(key, 2, "cloud.test:443", "", "")
				}
				if selection == "default" {
					cfg.DefaultDevice = listener.Addr().String()
				}
				setTempConfig(t, cfg)
				setPinCache(t)
				before, err := config.Load()
				if err != nil {
					t.Fatal(err)
				}
				old := pickTargetDeviceFn
				t.Cleanup(func() { pickTargetDeviceFn = old })
				pickTargetDeviceFn = func(ctx context.Context, _ map[string]bool, _, _, _ bool) (*SelectedDevice, error) {
					if selection != "picker" {
						t.Fatal("unexpected device picker")
					}
					dev := &models.DiscoveredDevice{LAN: &models.LANDevice{Hostname: key, IPAddress: "127.0.0.1", Port: listener.Addr().(*net.TCPAddr).Port}}
					return connectLocalPickerChoice(ctx, &tui.PickerItem{Value: &pickerEntry{mergedDevice: dev}}, true)
				}
				ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
				defer cancel()
				target, err := resolveWifiTarget(ctx, func(c *resolveConfig) {
					if selection == "explicit" {
						c.device = listener.Addr().String()
					}
					c.disableSessionBroker, c.suppressUpdateCheck = true, true
					c.nonInteractive = selection != "picker"
				})
				if target != nil {
					defer target.Close()
				}
				if state != "unprovisioned" {
					if err == nil {
						t.Fatal("Wi-Fi accepted an enrolled, unknown, or previously bound device without authentication")
					}
				} else {
					if err != nil {
						t.Fatalf("first-boot Wi-Fi setup: %v", err)
					}
					if target.Agent.DevicePinKey != key {
						t.Fatalf("selected device changed: %q", target.Agent.DevicePinKey)
					}
					client, err := newWifiClient(target)
					if err != nil {
						t.Fatal(err)
					}
					defer client.Close()
					networks, err := client.List(ctx)
					if err != nil || len(networks) != 1 || networks[0].Ssid != "Setup WiFi" {
						t.Fatalf("scan: %v, %v", networks, err)
					}
					// Credentials need consent over an unverified setup connection.
					if err := client.Connect(ctx, &agentpb.ConnectToWiFiRequest{Ssid: "Setup WiFi", Password: "fixture-password"}); !errors.Is(err, errUnverifiedWiFiCredentials) || service.connects.Load() != 0 {
						t.Fatalf("Wi-Fi credentials sent without consent: %v", err)
					}
					if err := confirmUnverifiedWiFiCredentials(target); !errors.Is(err, errUnverifiedWiFiCredentials) {
						t.Fatalf("non-interactive consent did not require the flag: %v", err)
					}
					allowUnverifiedWiFiDevice = true
					t.Cleanup(func() { allowUnverifiedWiFiDevice = false })
					if err := confirmUnverifiedWiFiCredentials(target); err != nil {
						t.Fatal(err)
					}
					if err := client.Connect(ctx, &agentpb.ConnectToWiFiRequest{Ssid: "Setup WiFi", Password: "fixture-password"}); err != nil {
						t.Fatal(err)
					}
					if service.connects.Load() != 1 {
						t.Fatal("Wi-Fi credentials did not reach the setup device")
					}
					// A later ordinary command reaches the unenrolled device too,
					// and its connection is still marked unverified.
					ordinary, err := resolveTargetInner(ctx, func(c *resolveConfig) {
						c.device = listener.Addr().String()
						c.disableSessionBroker = true
						c.nonInteractive = true
					})
					if ordinary != nil {
						ordinary.Close()
					}
					if err != nil || !ordinary.Agent.UnverifiedPlaintext() {
						t.Fatalf("ordinary management of an unenrolled device: %v", err)
					}
				}
				after, err := config.Load()
				if err != nil {
					t.Fatal(err)
				}
				if !reflect.DeepEqual(before.DevicePins, after.DevicePins) {
					t.Fatal("Wi-Fi setup changed saved device trust")
				}
			})
		}
	}
}

// serveSetupProbe serves a plaintext agent reporting state and returns its address.
func serveSetupProbe(t *testing.T, state string) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	service := &firstBootWiFiProbe{setupIdentityProbe: setupIdentityProbe{state: state}}
	server := grpc.NewServer()
	agentpb.RegisterWendyAgentServiceServer(server, service)
	pb.RegisterWendyProvisioningServiceServer(server, service)
	go server.Serve(listener)
	t.Cleanup(server.Stop)
	return listener.Addr().String()
}

// closedLoopbackAddr returns an address with nothing listening on it.
func closedLoopbackAddr(t *testing.T) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := listener.Addr().String()
	listener.Close()
	return addr
}

// The setup exception only adds a connection the ordinary resolver refuses.
// Every other outcome keeps the ordinary resolver's typed errors and fallbacks.
func TestWiFiSetupKeepsOrdinaryResolution(t *testing.T) {
	for _, tc := range []struct {
		name      string
		loggedIn  bool
		state     string // "" means nothing listens
		isDefault bool
		check     func(*testing.T, *SelectedDevice, error)
	}{
		{name: "logged in, unreachable", loggedIn: true, check: func(t *testing.T, _ *SelectedDevice, err error) {
			if err == nil || errors.Is(err, certs.ErrExpectedDeviceIdentity) {
				t.Fatalf("unreachable device: want a connection failure, got %v", err)
			}
		}},
		{name: "logged in, enrolled", loggedIn: true, state: "enrolled", check: func(t *testing.T, _ *SelectedDevice, err error) {
			if !errors.Is(err, errEnrolledWithoutTLS) {
				t.Fatalf("enrolled plaintext answer was not refused: %v", err)
			}
		}},
		{name: "logged in, unknown state", loggedIn: true, state: "unknown", check: func(t *testing.T, _ *SelectedDevice, err error) {
			if !errors.Is(err, errEnrolledWithoutTLS) {
				t.Fatalf("unknown state was not refused: %v", err)
			}
		}},
		{name: "logged out, unprovisioned", state: "unprovisioned", check: func(t *testing.T, target *SelectedDevice, err error) {
			if err != nil || target == nil || target.unverifiedSetup {
				t.Fatalf("logged-out Wi-Fi left the ordinary plaintext path: target=%+v err=%v", target, err)
			}
		}},
		{name: "logged out, offline default", isDefault: true, check: func(t *testing.T, _ *SelectedDevice, err error) {
			if err == nil || !strings.Contains(err.Error(), "is set but could not be reached") {
				t.Fatalf("offline default lost its default-device error: %v", err)
			}
		}},
		{name: "logged in, unprovisioned", loggedIn: true, state: "unprovisioned", check: func(t *testing.T, target *SelectedDevice, err error) {
			if err != nil || target == nil || !target.unverifiedSetup {
				t.Fatalf("setup connection was not marked unverified: target=%+v err=%v", target, err)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			restoreDeviceGlobals(t)
			deviceFlag, jsonOutput = "", false
			addr := closedLoopbackAddr(t)
			if tc.state != "" {
				addr = serveSetupProbe(t, tc.state)
			}
			cfg := &config.Config{}
			if tc.loggedIn {
				cfg.Auth = []config.AuthConfig{{Certificates: []config.CertificateInfo{selfSignedCLICert(t, 2)}}}
			}
			if tc.isDefault {
				cfg.DefaultDevice = addr
			}
			setTempConfig(t, cfg)
			setPinCache(t)
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			target, err := resolveWifiTarget(ctx, func(c *resolveConfig) {
				if !tc.isDefault {
					c.device = addr
				}
				c.disableSessionBroker, c.suppressUpdateCheck, c.nonInteractive = true, true, true
			})
			if target != nil {
				defer target.Close()
			}
			tc.check(t, target, err)
		})
	}
}

// A picked row whose LAN address does not answer still falls back to
// Bluetooth, as it did before the setup exception.
func TestWiFiSetupPickerKeepsBluetoothFallback(t *testing.T) {
	restoreDeviceGlobals(t)
	deviceFlag, jsonOutput = "", false
	setTempConfig(t, &config.Config{})
	setPinCache(t)
	_, portText, _ := net.SplitHostPort(closedLoopbackAddr(t))
	portNumber, _ := net.LookupPort("tcp", portText)
	bluetooth := &models.BluetoothDevice{ID: "ble-1", DisplayName: "new-robot", IsWendyDevice: true}
	dev := &models.DiscoveredDevice{
		LAN:       &models.LANDevice{Hostname: "new-robot.invalid", IPAddress: "127.0.0.1", Port: portNumber},
		Bluetooth: bluetooth,
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	target, err := connectLocalPickerChoice(withWiFiSetupSelection(ctx), &tui.PickerItem{Value: &pickerEntry{mergedDevice: dev}}, true)
	if err != nil || target == nil || target.Bluetooth != bluetooth {
		t.Fatalf("Wi-Fi setup picker lost its Bluetooth fallback: target=%+v err=%v", target, err)
	}
}

func stubUnverifiedWiFiConsent(t *testing.T, interactive bool, answer bool, answerErr error) *int {
	t.Helper()
	oldInteractive, oldConfirm, oldAllow, oldJSON := isInteractiveTerminalFn, confirmUnverifiedWiFiFn, allowUnverifiedWiFiDevice, jsonOutput
	t.Cleanup(func() {
		isInteractiveTerminalFn, confirmUnverifiedWiFiFn, allowUnverifiedWiFiDevice, jsonOutput = oldInteractive, oldConfirm, oldAllow, oldJSON
	})
	prompts := 0
	isInteractiveTerminalFn = func() bool { return interactive }
	confirmUnverifiedWiFiFn = func() (bool, error) { prompts++; return answer, answerErr }
	allowUnverifiedWiFiDevice, jsonOutput = false, false
	return &prompts
}

func TestUnverifiedWiFiConsent(t *testing.T) {
	unverified := func() *SelectedDevice {
		return &SelectedDevice{Agent: &grpcclient.AgentConnection{DevicePinKey: "robot.local"}, unverifiedSetup: true}
	}
	t.Run("verified target", func(t *testing.T) {
		prompts := stubUnverifiedWiFiConsent(t, true, false, nil)
		if err := confirmUnverifiedWiFiCredentials(&SelectedDevice{Agent: &grpcclient.AgentConnection{}}); err != nil || *prompts != 0 {
			t.Fatalf("verified target prompted: %v", err)
		}
	})
	t.Run("json refuses", func(t *testing.T) {
		prompts := stubUnverifiedWiFiConsent(t, true, true, nil)
		jsonOutput = true
		if err := confirmUnverifiedWiFiCredentials(unverified()); !errors.Is(err, errUnverifiedWiFiCredentials) || *prompts != 0 {
			t.Fatalf("--json sent credentials without the flag: %v", err)
		}
	})
	t.Run("flag", func(t *testing.T) {
		prompts := stubUnverifiedWiFiConsent(t, false, false, nil)
		allowUnverifiedWiFiDevice = true
		if err := confirmUnverifiedWiFiCredentials(unverified()); err != nil || *prompts != 0 {
			t.Fatalf("flag did not grant consent: %v", err)
		}
	})
	t.Run("accept once", func(t *testing.T) {
		prompts := stubUnverifiedWiFiConsent(t, true, true, nil)
		target := unverified()
		for range 2 {
			if err := confirmUnverifiedWiFiCredentials(target); err != nil {
				t.Fatal(err)
			}
		}
		if *prompts != 1 || !target.unverifiedSetupAccepted {
			t.Fatalf("consent prompts = %d", *prompts)
		}
	})
	t.Run("decline", func(t *testing.T) {
		stubUnverifiedWiFiConsent(t, true, false, nil)
		target := unverified()
		err := confirmUnverifiedWiFiCredentials(target)
		if !errors.Is(err, errUnverifiedWiFiDeclined) || !errors.Is(err, ErrUserCancelled) || target.unverifiedSetupAccepted {
			t.Fatalf("decline: %v", err)
		}
	})
	t.Run("cancel", func(t *testing.T) {
		stubUnverifiedWiFiConsent(t, true, false, tui.ErrCancelled)
		err := confirmUnverifiedWiFiCredentials(unverified())
		if !errors.Is(err, ErrUserCancelled) || errors.Is(err, errUnverifiedWiFiDeclined) {
			t.Fatalf("cancel: %v", err)
		}
	})
}

type enrollmentWifiConnectClient struct {
	enrollmentWifiClient
	connects int
}

func (c *enrollmentWifiConnectClient) ConnectToWiFi(context.Context, *agentpb.ConnectToWiFiRequest, ...grpc.CallOption) (*agentpb.ConnectToWiFiResponse, error) {
	c.connects++
	return &agentpb.ConnectToWiFiResponse{Success: true}, nil
}

// Enrollment's Wi-Fi prompt sends the password over the same unverified setup
// connection, so it asks too. Declining skips Wi-Fi, not enrollment.
func TestEnrollmentWifiAsksBeforeUnverifiedPassword(t *testing.T) {
	for _, tc := range []struct {
		name        string
		unverified  bool
		accept      bool
		wantPrompts int
		wantConnect int
	}{
		{name: "verified", wantConnect: 1},
		{name: "unverified accepted", unverified: true, accept: true, wantPrompts: 1, wantConnect: 1},
		{name: "unverified declined", unverified: true, wantPrompts: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			prompts := stubUnverifiedWiFiConsent(t, true, tc.accept, nil)
			confirm, pick, password := confirmEnrollmentWifiFn, pickEnrollmentWifiFn, promptWifiPassword
			t.Cleanup(func() { confirmEnrollmentWifiFn, pickEnrollmentWifiFn, promptWifiPassword = confirm, pick, password })
			confirmEnrollmentWifiFn = func() (bool, error) { return true, nil }
			pickEnrollmentWifiFn = func(context.Context, *SelectedDevice) (string, error) { return "Home", nil }
			promptWifiPassword = func(string) (string, error) { return "fixture-password", nil }
			client := &enrollmentWifiConnectClient{enrollmentWifiClient: enrollmentWifiClient{response: &agentpb.GetWiFiStatusResponse{}}}
			conn := &grpcclient.AgentConnection{AgentService: client}
			target := &SelectedDevice{Agent: conn, unverifiedSetup: tc.unverified}
			if err := promptWifiIfNeeded(context.WithValue(context.Background(), wifiCredentialTargetKey{}, target), conn); err != nil {
				t.Fatal(err)
			}
			if *prompts != tc.wantPrompts || client.connects != tc.wantConnect {
				t.Fatalf("prompts = %d, connects = %d", *prompts, client.connects)
			}
		})
	}
}

// The MCP Wi-Fi tools' device argument reaches a first-boot device the same
// way the CLI's Wi-Fi commands do, and reports it as unverified.
func TestMCPWiFiDeviceReachesFirstBootDevice(t *testing.T) {
	for _, tc := range []struct {
		state          string
		wantUnverified bool
		wantIdentity   bool
	}{
		{state: "unprovisioned", wantUnverified: true},
		{state: "enrolled", wantIdentity: true},
	} {
		t.Run(tc.state, func(t *testing.T) {
			restoreDeviceGlobals(t)
			deviceFlag, jsonOutput = "", false
			addr := serveSetupProbe(t, tc.state)
			setTempConfig(t, &config.Config{Auth: []config.AuthConfig{{Certificates: []config.CertificateInfo{selfSignedCLICert(t, 2)}}}})
			setPinCache(t)
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			conn, unverified, err := connectMCPWiFiDevice(ctx, addr)
			if conn != nil {
				defer conn.Close()
			}
			if tc.wantIdentity {
				if !errors.Is(err, errEnrolledWithoutTLS) {
					t.Fatalf("enrolled device: %v", err)
				}
				return
			}
			if err != nil || conn == nil || unverified != tc.wantUnverified {
				t.Fatalf("conn=%v unverified=%v err=%v", conn, unverified, err)
			}
			// device_connect reaches it too, still marked unverified so the
			// Wi-Fi tools ask before sending a password over it.
			session, err := connectMCPDevice(ctx, addr)
			if session != nil {
				defer session.Close()
			}
			if err != nil || !session.UnverifiedPlaintext() {
				t.Fatalf("device_connect: unverified=%v err=%v", session.UnverifiedPlaintext(), err)
			}
		})
	}
}
