package commands

import (
	"context"
	"errors"
	"net"
	"reflect"
	"sync/atomic"
	"testing"
	"time"

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
					if err := client.Connect(ctx, &agentpb.ConnectToWiFiRequest{Ssid: "Setup WiFi", Password: "fixture-password"}); err != nil {
						t.Fatal(err)
					}
					if service.connects.Load() != 1 {
						t.Fatal("Wi-Fi credentials did not reach the setup device")
					}
					// A later ordinary command must not inherit the setup exception.
					ordinary, err := resolveTargetInner(ctx, func(c *resolveConfig) {
						c.device = listener.Addr().String()
						c.disableSessionBroker = true
						c.nonInteractive = true
					})
					if ordinary != nil {
						ordinary.Close()
					}
					if !errors.Is(err, certs.ErrExpectedDeviceIdentity) {
						t.Fatalf("ordinary management inherited Wi-Fi setup: %v", err)
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
