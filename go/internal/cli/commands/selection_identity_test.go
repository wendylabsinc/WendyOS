package commands

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/cli/grpcclient"
	"github.com/wendylabsinc/wendy/go/internal/cli/tui"
	"github.com/wendylabsinc/wendy/go/internal/shared/certs"
	"github.com/wendylabsinc/wendy/go/internal/shared/config"
	"github.com/wendylabsinc/wendy/go/internal/shared/discoverycache"
	"github.com/wendylabsinc/wendy/go/internal/shared/models"
	agentpb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
	pb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
)

func TestBLEDiscoveryAliasCannotReplaceSavedDevice(t *testing.T) {
	auth, peerB := legacyLiteTestPKI(t, 2, certs.AssetURN(2, 607))
	cfg := &config.Config{Auth: []config.AuthConfig{*auth}}
	cfg.SetDevicePin("robot-a", 2, "cloud.test:443", "606", "")
	cfg.SetDevicePinFrom("robot-b", 2, "cloud.test:443", "607", "", config.PinSourceCloud)
	setTempConfig(t, cfg)
	// An unauthenticated mDNS sighting claims that robot-a is also named robot-b.
	setPinCache(t, discoverycache.Entry{Hostname: "robot-a.local", DisplayName: "robot-b"})
	tlsConfig, err := bleAgentTLSConfig(&models.BluetoothDevice{DisplayName: "robot-a"}, auth.Certificates[0], nil)
	if err != nil {
		t.Fatal(err)
	}
	leaf, err := x509.ParseCertificate(peerB.Certificates[0].Certificate[0])
	if err != nil {
		t.Fatal(err)
	}
	if err := tlsConfig.VerifyConnection(tls.ConnectionState{PeerCertificates: []*x509.Certificate{leaf}}); err == nil {
		t.Fatal("robot B was accepted for robot A despite A's saved asset 606 binding")
	}
}

func TestEnrollmentSavesOriginalSelectedHostname(t *testing.T) {
	auth := oidcEnrollmentAuth(t)
	issue, roots := desktopEnrollmentTestCA(t, auth)
	auth.CloudGRPC = "cloud.test:443"
	setTempConfig(t, &config.Config{Auth: []config.AuthConfig{*auth}})
	setPinCache(t)
	plain, secure := desktopEnrollmentTestPorts(t)
	plainServer := grpc.NewServer()
	probe := &setupIdentityProbe{state: "unprovisioned"}
	agentpb.RegisterWendyAgentServiceServer(plainServer, probe)
	pb.RegisterWendyProvisioningServiceServer(plainServer, probe)
	go plainServer.Serve(plain)
	t.Cleanup(plainServer.Stop)
	identity, err := certs.ParseDevicePrincipal(certs.DeviceSPIFFEURI(testOperatorTenant, "11111111-1111-4111-8111-111111111111"))
	if err != nil {
		t.Fatal(err)
	}
	pair, err := issue(identity.Principal)
	if err != nil {
		t.Fatal(err)
	}
	secureServer := grpc.NewServer(grpc.Creds(credentials.NewTLS(&tls.Config{Certificates: []tls.Certificate{pair}, ClientCAs: roots, ClientAuth: tls.RequireAndVerifyClientCert})))
	pb.RegisterWendyProvisioningServiceServer(secureServer, infoProvisioningV2Server{response: &pb.IsProvisionedResponse{ResponseType: &pb.IsProvisionedResponse_Provisioned{Provisioned: &pb.ProvisionedResponse{PrincipalUri: identity.Principal}}}})
	go secureServer.Serve(secure)
	t.Cleanup(secureServer.Stop)
	_, port, err := net.SplitHostPort(plain.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()
	target, handled, err := connectUnboundEnrollmentTarget(ctx, "localhost", net.JoinHostPort("localhost", port))
	if err != nil || !handled {
		t.Fatalf("explicit enrollment setup: handled=%v error=%v", handled, err)
	}
	defer target.Close()
	next, err := verifyAndSaveEnrollment(ctx, target.Agent, auth, &identity, "cloud-name")
	if err != nil {
		t.Fatal(err)
	}
	defer next.Close()
	if expectedIdentityFor("cloud-name") == nil {
		t.Fatal("verified enrollment did not save its Cloud name")
	}
	if expectedIdentityFor("localhost") == nil {
		t.Fatalf("successful enrollment forgot original selected hostname; prior Host=%q Addr=%q", target.Agent.Host, target.Agent.Addr)
	}
}

func TestLANSelectionKeepsExpectedIdentityAcrossIPRoutes(t *testing.T) {
	for _, surface := range []string{"picker", "version probe", "fleet", "OS reconnect", "power reboot"} {
		for _, asset := range []int32{606, 607} {
			t.Run(fmt.Sprintf("%s/%d", surface, asset), func(t *testing.T) {
				auth, serverTLS := legacyLiteTestPKI(t, 2, certs.AssetURN(2, asset))
				auth.CloudGRPC = "cloud.test:443"
				cfg := &config.Config{Auth: []config.AuthConfig{*auth}}
				cfg.SetDevicePin("robot-a", 2, auth.CloudGRPC, "606", "")
				setTempConfig(t, cfg)
				setPinCache(t, discoverycache.Entry{Hostname: "robot-a.local", IP: "127.0.0.1", DisplayName: "robot-a"})
				listener, err := net.Listen("tcp", "127.0.0.1:0")
				if err != nil {
					t.Fatal(err)
				}
				server := grpc.NewServer(grpc.Creds(credentials.NewTLS(serverTLS)))
				agentpb.RegisterWendyAgentServiceServer(server, &selectionPowerModeProbe{})
				go server.Serve(listener)
				t.Cleanup(server.Stop)
				dev := models.LANDevice{Hostname: "robot-a.local", IPAddress: "127.0.0.1", Port: listener.Addr().(*net.TCPAddr).Port, IsMTLS: true}
				ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
				defer cancel()
				var conn *grpcclient.AgentConnection
				switch surface {
				case "picker":
					var target *SelectedDevice
					target, err = connectPickedLANDevice(ctx, &models.DiscoveredDevice{LAN: &dev}, listener.Addr().String(), true)
					if target != nil {
						conn = target.Agent
					}
				case "version probe":
					_, _, _, err = resolveLANAgentVersion(ctx, dev)
				case "fleet":
					conn, err = targetForDevice(dev).connect(ctx)
				case "power reboot":
					_, err = waitForPowerModeApplied(ctx, listener.Addr().String(), "robot-a.local", 1)
				case "OS reconnect":
					conn, err = dialOSUpdateAgent(ctx, &grpcclient.AgentConnection{Addr: listener.Addr().String(), Host: "127.0.0.1", IsMTLS: true, DevicePinKey: "robot-a.local"})
					if err == nil {
						_, err = conn.AgentService.GetAgentVersion(ctx, &agentpb.GetAgentVersionRequest{})
					}
				}
				if conn != nil {
					defer conn.Close()
				}
				if asset == 606 {
					if err != nil {
						t.Fatalf("selected hostname binding was lost: %v", err)
					}
					if conn != nil && conn.DevicePinKey != "robot-a.local" {
						t.Fatalf("selected name was not retained: %q", conn.DevicePinKey)
					}
				} else if err == nil {
					t.Fatal("different same-tenant device was accepted through an IP route")
				} else if ctx.Err() != nil {
					t.Fatalf("wrong peer timed out instead of producing an identity refusal: %v", err)
				}
			})
		}
	}
}

func TestEnrollmentModeReachesLocalPickerChoice(t *testing.T) {
	for _, state := range []string{"unprovisioned", "enrolled", "unknown"} {
		for _, enroll := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/enroll=%v", state, enroll), func(t *testing.T) {
				restoreDeviceGlobals(t)
				deviceFlag = ""
				jsonOutput = false
				setTempConfig(t, &config.Config{Auth: []config.AuthConfig{{Certificates: []config.CertificateInfo{selfSignedCLICert(t, 2)}}}})
				setPinCache(t)
				listener, err := net.Listen("tcp", "127.0.0.1:0")
				if err != nil {
					t.Fatal(err)
				}
				service := &setupIdentityProbe{state: state}
				server := grpc.NewServer()
				agentpb.RegisterWendyAgentServiceServer(server, service)
				pb.RegisterWendyProvisioningServiceServer(server, service)
				go server.Serve(listener)
				t.Cleanup(server.Stop)
				dev := &models.DiscoveredDevice{LAN: &models.LANDevice{Hostname: "new-robot.local", IPAddress: "127.0.0.1", Port: listener.Addr().(*net.TCPAddr).Port}}
				item := &tui.PickerItem{Value: &pickerEntry{mergedDevice: dev}}
				old := pickTargetDeviceFn
				pickTargetDeviceFn = func(ctx context.Context, _ map[string]bool, _, _, _ bool) (*SelectedDevice, error) {
					return connectLocalPickerChoice(ctx, item, true)
				}
				t.Cleanup(func() { pickTargetDeviceFn = old })
				ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
				defer cancel()
				target, err := resolveTargetInner(ctx, func(cfg *resolveConfig) {
					cfg.enrollmentTrustRecovery = enroll
					cfg.disableSessionBroker = true
					cfg.suppressUpdateCheck = true
				})
				if target != nil {
					defer target.Close()
				}
				want := enroll && state == "unprovisioned"
				if (err == nil) != want {
					t.Fatalf("picker enrollment=%v state=%s error=%v, want allowed=%v", enroll, state, err, want)
				}
				if want && target.Agent.DevicePinKey != "new-robot.local" {
					t.Fatalf("setup lost selected hostname: %q", target.Agent.DevicePinKey)
				}
				if !enroll && !errors.Is(err, certs.ErrExpectedDeviceIdentity) {
					t.Fatalf("ordinary picker relaxed missing identity: %v", err)
				}
			})
		}
	}
}

type selectionPowerModeProbe struct{ setupIdentityProbe }

func (*selectionPowerModeProbe) GetPowerMode(context.Context, *agentpb.GetPowerModeRequest) (*agentpb.GetPowerModeResponse, error) {
	return &agentpb.GetPowerModeResponse{Current: &agentpb.PowerMode{Id: 1}}, nil
}
