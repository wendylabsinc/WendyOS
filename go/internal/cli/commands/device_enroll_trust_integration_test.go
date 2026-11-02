package commands

import (
	"context"
	"errors"
	"io"
	"reflect"
	"testing"
	"time"

	"github.com/spf13/cobra"
	"github.com/wendylabsinc/wendy/go/internal/cli/grpcclient"
	"github.com/wendylabsinc/wendy/go/internal/cli/tui"
	"github.com/wendylabsinc/wendy/go/internal/shared/certs"
	"github.com/wendylabsinc/wendy/go/internal/shared/config"
	"github.com/wendylabsinc/wendy/go/internal/shared/discoverycache"
	"github.com/wendylabsinc/wendy/go/internal/shared/models"
	agentpb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
)

func isolateEnrollmentTrustIntegration(t *testing.T) {
	t.Helper()
	oldDevice, oldJSON, oldInteractive := deviceFlag, jsonOutput, isInteractiveTerminalFn
	oldConfirm, oldPlaintext := confirmEnrollmentTrustFn, plaintextConnectFn
	t.Cleanup(func() {
		deviceFlag, jsonOutput, isInteractiveTerminalFn = oldDevice, oldJSON, oldInteractive
		confirmEnrollmentTrustFn, plaintextConnectFn = oldConfirm, oldPlaintext
	})
	deviceFlag, jsonOutput = "", false
	isInteractiveTerminalFn = func() bool { return true }
	t.Setenv("WENDY_AGENT_SOCKET", "")
	setPinCache(t)
}

func TestEnrollmentCommandsTrustSelectedDevice(t *testing.T) {
	for _, command := range []struct {
		name string
		new  func() *cobra.Command
	}{{"device enroll", newDeviceEnrollCmd}, {"cloud enroll-device", newCloudEnrollDeviceCmd}} {
		for _, accept := range []bool{false, true} {
			answer := "decline"
			if accept {
				answer = "accept"
			}
			t.Run(command.name+"/"+answer, func(t *testing.T) {
				isolateEnrollmentTrustIntegration(t)
				const host, addr = "192.0.2.10", "192.0.2.10:50051"
				auth := fakeAuth(t)
				cfg := &config.Config{Auth: []config.AuthConfig{*auth}, DevicePins: map[string]config.DevicePin{
					host: {OrgID: 7, AssetID: "42", Source: config.PinSourceLAN},
				}}
				deviceFlag = host
				setTempConfig(t, cfg)

				oldPickAuth, oldFresh := pickAuthSessionFn, ensureFreshCertificateFn
				oldDial, oldBroker, oldDiscover := dialAgentLadderFn, connectSessionBrokerFn, discoverLANDevices
				oldWifi, oldEnroll := promptEnrollmentWifiFn, runAgentEnrollmentFn
				t.Cleanup(func() {
					pickAuthSessionFn, ensureFreshCertificateFn = oldPickAuth, oldFresh
					dialAgentLadderFn, connectSessionBrokerFn, discoverLANDevices = oldDial, oldBroker, oldDiscover
					promptEnrollmentWifiFn, runAgentEnrollmentFn = oldWifi, oldEnroll
				})
				pickAuthSessionFn = func(cfg *config.Config) (*config.AuthConfig, error) { return &cfg.Auth[0], nil }
				ensureFreshCertificateFn = func(context.Context, *config.AuthConfig) error { return nil }
				connectSessionBrokerFn = func(context.Context, string, certs.WendyIdentity) (*grpcclient.AgentConnection, error) {
					return nil, errors.New("no reusable session")
				}
				discoverLANDevices = func(context.Context, time.Duration) ([]models.LANDevice, error) { return nil, nil }
				var calls []string
				dialAgentLadderFn = func(ctx context.Context, target dialTarget) (*grpcclient.AgentConnection, error, error) {
					calls = append(calls, "dial")
					if target.PinKey != host || target.Addr != addr {
						t.Fatalf("authenticated dial target = %+v", target)
					}
					return dialAgentLadderWithCerts(ctx, target, nil)
				}
				confirmEnrollmentTrustFn = func() (bool, error) {
					calls = append(calls, "trust")
					// Selection changes during the prompt cannot redirect consent.
					deviceFlag = "another-device.local"
					return accept, nil
				}
				closed := false
				client := &fakeVersionAgentClient{respond: func(int) (*agentpb.GetAgentVersionResponse, error) {
					calls = append(calls, "probe")
					return &agentpb.GetAgentVersionResponse{Version: "test"}, nil
				}}
				conn := &grpcclient.AgentConnection{Host: host, AgentService: client, ExtraClosers: []io.Closer{closeTracker{closed: &closed}}}
				plaintextConnectFn = func(_ context.Context, got string) (*grpcclient.AgentConnection, error) {
					calls = append(calls, "plaintext")
					if got != addr {
						t.Fatalf("trusted address = %q, want original target %q", got, addr)
					}
					return conn, nil
				}
				promptEnrollmentWifiFn = func(_ context.Context, got *grpcclient.AgentConnection) error {
					calls = append(calls, "wifi")
					if got != conn || closed {
						t.Fatal("WiFi setup did not receive the live trusted connection")
					}
					return nil
				}
				runAgentEnrollmentFn = func(_ context.Context, got *grpcclient.AgentConnection, gotAuth *config.AuthConfig, name string, org int32, _ ...string) error {
					calls = append(calls, "enroll")
					if got != conn || gotAuth.CloudGRPC != auth.CloudGRPC || name != "reenrolled" || org != 0 || closed {
						t.Fatal("enrollment did not retain the selected device, account, and flags")
					}
					return nil
				}
				cmd := command.new()
				cmd.SetContext(context.Background())
				if err := cmd.Flags().Set("name", "reenrolled"); err != nil {
					t.Fatal(err)
				}
				err := cmd.RunE(cmd, nil)
				wantCalls := []string{"dial", "trust"}
				if accept {
					wantCalls = append(wantCalls, "plaintext", "probe", "wifi", "enroll")
					if err != nil || !closed {
						t.Fatalf("accepted enrollment error = %v, connection closed = %v", err, closed)
					}
				} else if !errors.Is(err, errNoAuthenticatedEndpoint) {
					t.Fatalf("declined enrollment error = %v, want original reachability failure", err)
				}
				if !reflect.DeepEqual(calls, wantCalls) {
					t.Fatalf("calls = %v, want %v", calls, wantCalls)
				}
				updated, err := config.Load()
				if err != nil {
					t.Fatal(err)
				}
				if _, pinned := updated.DevicePinFor(host); pinned == accept {
					t.Fatalf("pin present = %v after accepted = %v", pinned, accept)
				}
			})
		}
	}
}

func TestPickerEnrollmentTrustKeepsHighlightedDevice(t *testing.T) {
	isolateEnrollmentTrustIntegration(t)
	restorePickerEnrollmentFunctions(t)
	deviceFlag = "another-device.local"
	item := &tui.PickerItem{Name: "Highlighted device", Address: "192.0.2.20"}
	const pinKey, addr = "highlighted.local", "192.0.2.20:50051"
	const alias, unrelatedAlias = "highlighted.acme.mesh.wendy.internal", "another-cached-alias"
	setPinCache(t, discoverycache.Entry{ID: "device-42", Hostname: pinKey, MeshName: alias, DisplayName: unrelatedAlias})
	unrelatedPin := config.DevicePin{OrgID: 7, AssetID: "99", Source: config.PinSourceLAN}
	readPins := writePinTestConfig(t, map[string]config.DevicePin{
		"highlighted":  {OrgID: 7, AssetID: "42", Source: config.PinSourceLAN},
		alias:          {OrgID: 7, AssetID: "42", Source: config.PinSourceCloud},
		unrelatedAlias: unrelatedPin,
	})
	auth := &config.AuthConfig{Certificates: []config.CertificateInfo{{OrganizationID: 7}}}
	var calls []string
	connectPickerEnrollmentFn = func(ctx context.Context, got *tui.PickerItem, suppressUpdate bool) (*SelectedDevice, error) {
		calls = append(calls, "connect row")
		if got != item || !suppressUpdate {
			t.Fatal("picker enrollment lost its highlighted row or update option")
		}
		_, _, err := dialAgentLadderWithCerts(ctx, newDialTarget(pinKey, addr), nil)
		return nil, err
	}
	confirmEnrollmentTrustFn = func() (bool, error) {
		calls = append(calls, "trust")
		return true, nil
	}
	client := &fakeVersionAgentClient{respond: func(int) (*agentpb.GetAgentVersionResponse, error) {
		calls = append(calls, "probe")
		return &agentpb.GetAgentVersionResponse{Version: "test"}, nil
	}}
	closed := false
	conn := &grpcclient.AgentConnection{Host: item.Address, AgentService: client, ExtraClosers: []io.Closer{closeTracker{closed: &closed}}}
	plaintextConnectFn = func(_ context.Context, got string) (*grpcclient.AgentConnection, error) {
		calls = append(calls, "plaintext")
		if got != addr {
			t.Fatalf("trusted address = %q, want highlighted address %q", got, addr)
		}
		return conn, nil
	}
	promptPickerEnrollmentWifiFn = func(_ context.Context, got *grpcclient.AgentConnection) error {
		calls = append(calls, "wifi")
		if got != conn || closed {
			t.Fatal("WiFi setup did not use the live highlighted connection")
		}
		return nil
	}
	runPickerEnrollmentFn = func(_ context.Context, got *grpcclient.AgentConnection, gotAuth *config.AuthConfig, name string, org int32, _ ...string) error {
		calls = append(calls, "enroll")
		if got != conn || gotAuth != auth || name != "" || org != 0 || closed {
			t.Fatal("enrollment did not retain the highlighted device and selected account")
		}
		return nil
	}
	if err := enrollLocalPickerDevice(context.Background(), item, auth, true); err != nil {
		t.Fatal(err)
	}
	if want := []string{"connect row", "trust", "plaintext", "probe", "wifi", "enroll"}; !reflect.DeepEqual(calls, want) {
		t.Fatalf("calls = %v, want %v", calls, want)
	}
	if !closed || deviceFlag != "another-device.local" {
		t.Fatalf("closed = %v, device flag = %q", closed, deviceFlag)
	}
	if got, want := readPins(), map[string]config.DevicePin{unrelatedAlias: unrelatedPin}; !reflect.DeepEqual(got, want) {
		t.Fatalf("pins after enrollment = %v, want only the unrelated alias %v", got, want)
	}
}

func TestEnrollmentTrustDialFailureRetainsSelectionAndGoverningAlias(t *testing.T) {
	const pinKey, alias = "highlighted.local", "highlighted.acme.mesh.wendy.internal"
	setPinCache(t, discoverycache.Entry{ID: "device-42", Hostname: pinKey, MeshName: alias})
	setPinConfig(t, map[string]config.DevicePin{alias: {OrgID: 7, AssetID: "42", Source: config.PinSourceCloud}})
	candidates := []string{"192.0.2.20:50051", "[2001:db8::20]:50051"}
	wantCandidates := append([]string(nil), candidates...)
	target := newDialTargetCandidates(pinKey, candidates)
	conn, mtlsErr, err := dialAgentLadderWithCerts(context.Background(), target, nil)
	var failure *noAuthenticatedEndpointError
	if conn != nil || mtlsErr != nil || !errors.As(err, &failure) {
		t.Fatalf("ladder result = (%v, %v, %v), want structured reachability failure", conn, mtlsErr, err)
	}
	if failure.pinKey != pinKey || failure.pinnedKey != alias || failure.certSeen {
		t.Fatalf("failure selection = %q, governing pin = %q, certificate seen = %v", failure.pinKey, failure.pinnedKey, failure.certSeen)
	}
	candidates[0] = "192.0.2.99:50051"
	if !reflect.DeepEqual(failure.candidates, wantCandidates) {
		t.Fatalf("failure candidates were not captured: %v, want %v", failure.candidates, wantCandidates)
	}
	seen := pinnedHostNoAuthenticatedEndpointError(alias, wantCandidates, nil, true)
	if !seen.certSeen {
		t.Fatal("a certificate rejection lost its trust-recovery exclusion")
	}
}
