package mcp

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"

	"github.com/wendylabsinc/wendy/go/internal/cli/grpcclient"
	"github.com/wendylabsinc/wendy/go/internal/shared/config"
	"github.com/wendylabsinc/wendy/go/proto/gen/cloudpb"
)

func commandTargetStatus(t *testing.T, s *mcpServer) map[string]any {
	t.Helper()
	result, err := s.handleWendyStatus(context.Background(), callToolReq("wendy_status", nil))
	if err != nil || result.IsError {
		t.Fatalf("wendy_status: result=%v error=%v", result, err)
	}
	var out map[string]any
	if err := json.Unmarshal([]byte(toolResultText(t, result)), &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func assertCommandTarget(t *testing.T, s *mcpServer, want commandTarget) {
	t.Helper()
	out := commandTargetStatus(t, s)
	target, present := out["command_target"]
	if want.Device == "" {
		if present {
			t.Fatalf("unexpected command target: %v", target)
		}
		return
	}
	data, err := json.Marshal(target)
	if err != nil {
		t.Fatal(err)
	}
	var got commandTarget
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("command target = %+v, want %+v", got, want)
	}
	if out["connection_type"] != want.Transport {
		t.Fatalf("connection type %v disagrees with target %+v", out["connection_type"], got)
	}
}

func TestCommandTargetDirectConnect(t *testing.T) {
	for _, startup := range []bool{false, true} {
		for _, tc := range []struct {
			name    string
			address string
			conn    *grpcclient.AgentConnection
			want    string
		}{
			{name: "requested_custom_port", address: "woof.local:51234", conn: &grpcclient.AgentConnection{Host: "192.0.2.1", Addr: "192.0.2.1:51234"}, want: "woof.local:51234"},
			{name: "exact_ipv6_endpoint", conn: &grpcclient.AgentConnection{Host: "2001:db8::1", Addr: "[2001:db8::1]:51234"}, want: "[2001:db8::1]:51234"},
			{name: "simulator_alias", address: "sim", conn: &grpcclient.AgentConnection{Host: "127.0.0.1", Addr: "127.0.0.1:60001", SimulatorName: "robot-lab"}, want: "vm:robot-lab"},
			{name: "socket_ignores_requested_device", address: "woof.local:51234", conn: &grpcclient.AgentConnection{Host: "unix:/tmp/agent.sock"}},
			{name: "prebuilt_has_no_replay_endpoint", conn: &grpcclient.AgentConnection{Host: "friendly-name"}},
		} {
			name := tc.name
			if startup {
				name += "/startup"
			}
			t.Run(name, func(t *testing.T) {
				s := New(&config.Config{}, func(context.Context, string) (*grpcclient.AgentConnection, error) { return tc.conn, nil })
				connect := s.ConnectTo
				if startup {
					connect = s.ConnectToOnStartup
				}
				if err := connect(context.Background(), tc.address); err != nil {
					t.Fatal(err)
				}
				want := commandTarget{}
				if tc.want != "" {
					want = commandTarget{Device: tc.want, Transport: "direct"}
				}
				assertCommandTarget(t, s, want)
			})
		}
	}
}

func TestUnifiedConnectPreservesCloudIdentityAndBroker(t *testing.T) {
	t.Setenv("WENDY_BROKER_URL", "selected-relay:443")
	const selector = "cloud://shared.example:443/org/7/asset/42"
	for _, startup := range []bool{false, true} {
		s := New(&config.Config{}, func(_ context.Context, device string) (*grpcclient.AgentConnection, error) {
			if device != selector {
				t.Fatalf("lost selector %s", device)
			}
			return &grpcclient.AgentConnection{Host: "127.0.0.1", Addr: "127.0.0.1:61000"}, nil
		})
		if startup {
			if err := s.ConnectToOnStartup(context.Background(), selector); err != nil {
				t.Fatal(err)
			}
		} else {
			r, err := s.handleDeviceConnect(context.Background(), callToolReq("device_connect", map[string]any{"device": selector}))
			if err != nil || r.IsError {
				t.Fatalf("connect: %v %v", r, err)
			}
		}
		assertCommandTarget(t, s, commandTarget{Device: selector, Selector: selector, Transport: "cloud", CloudGRPC: "shared.example:443", BrokerURL: "selected-relay:443"})
		target, err := s.runTarget(callToolReq("run", nil))
		if err != nil || target.Selector != selector {
			t.Fatalf("deployment lost target: %v %v", target, err)
		}
	}
}

func TestCommandTargetCloudResolvedEndpoint(t *testing.T) {
	cfg := &config.Config{Auth: []config.AuthConfig{{
		CloudGRPC: "private-cloud.example:5443",
		APIKey:    "secret-api-key",
		Certificates: []config.CertificateInfo{{
			PemCertificate: "secret-certificate", PemPrivateKey: "secret-private-key",
		}},
	}}}
	s := New(cfg, nil)
	auth, err := s.cloudAuthEntry("")
	if err != nil {
		t.Fatal(err)
	}
	asset := &cloudpb.Asset{Name: "Selected Woof"}
	for _, broker := range []string{"", "private-broker.example:5444"} {
		t.Run(broker, func(t *testing.T) {
			s.setConnection(&grpcclient.AgentConnection{Host: "localhost"}, "cloud", cloudCommandTarget(auth, asset, broker))
			assertCommandTarget(t, s, commandTarget{Device: "Selected Woof", Transport: "cloud", CloudGRPC: auth.CloudGRPC, BrokerURL: broker})
			out := commandTargetStatus(t, s)
			encoded, err := json.Marshal(out["command_target"])
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(string(encoded), "secret-") || strings.Contains(string(encoded), "localhost") {
				t.Fatalf("target contains credentials or a tunnel placeholder: %s", encoded)
			}
			if broker == "" && strings.Contains(string(encoded), "broker_url") {
				t.Fatalf("default broker should be omitted: %s", encoded)
			}
		})
	}
}

func TestCommandTargetReplacementAndDisconnect(t *testing.T) {
	s := New(&config.Config{}, nil)
	s.setConnection(&grpcclient.AgentConnection{Host: "Cloud Woof"}, "cloud", commandTarget{Device: "Cloud Woof", Transport: "cloud", CloudGRPC: "cloud.example:443"})
	s.SetConn(&grpcclient.AgentConnection{Host: "woof.local", Addr: "woof.local:51234"})
	assertCommandTarget(t, s, commandTarget{Device: "woof.local:51234", Transport: "direct"})
	s.SetConnType("cloud")
	assertCommandTarget(t, s, commandTarget{})
	s.SetConn(nil)
	assertCommandTarget(t, s, commandTarget{})
	out := commandTargetStatus(t, s)
	if out["connected"] != false {
		t.Fatalf("disconnect returned connected status: %v", out)
	}
	s.SetConn(&grpcclient.AgentConnection{Host: "unknown"})
	assertCommandTarget(t, s, commandTarget{})
}

func TestCommandTargetStartupCannotOverwriteExplicitChange(t *testing.T) {
	for _, disconnect := range []bool{false, true} {
		t.Run(map[bool]string{false: "cloud_connect", true: "device_disconnect"}[disconnect], func(t *testing.T) {
			started := make(chan struct{})
			proceed := make(chan struct{})
			s := New(&config.Config{}, func(context.Context, string) (*grpcclient.AgentConnection, error) {
				close(started)
				<-proceed
				return &grpcclient.AgentConnection{Host: "default.local", Addr: "default.local:50051"}, nil
			})
			done := make(chan error, 1)
			go func() { done <- s.ConnectToOnStartup(context.Background(), "default.local:50051") }()
			<-started
			want := commandTarget{}
			if disconnect {
				if _, err := s.handleDeviceDisconnect(context.Background(), callToolReq("device_disconnect", nil)); err != nil {
					t.Fatal(err)
				}
			} else {
				want = commandTarget{Device: "Cloud Woof", Transport: "cloud", CloudGRPC: "cloud.example:443"}
				s.setConnection(&grpcclient.AgentConnection{Host: "Cloud Woof"}, "cloud", want)
			}
			close(proceed)
			if err := <-done; err != nil {
				t.Fatal(err)
			}
			assertCommandTarget(t, s, want)
			if disconnect && s.GetConn() != nil {
				t.Fatal("startup reconnected after explicit disconnect")
			}
		})
	}
}

func TestCommandTargetStatusSnapshotStaysConsistent(t *testing.T) {
	s := New(&config.Config{}, nil)
	var writers sync.WaitGroup
	writers.Add(1)
	go func() {
		defer writers.Done()
		for range 1000 {
			s.SetConn(&grpcclient.AgentConnection{Host: "direct", Addr: "direct:51234"})
			s.setConnection(&grpcclient.AgentConnection{Host: "cloud"}, "cloud", commandTarget{Device: "cloud", Transport: "cloud", CloudGRPC: "cloud.example:443"})
			s.SetConn(nil)
		}
	}()
	defer writers.Wait()
	for range 1000 {
		out := commandTargetStatus(t, s)
		target, ok := out["command_target"].(map[string]any)
		if out["connected"] == false {
			if ok {
				t.Fatalf("disconnected snapshot contains target: %v", out)
			}
			continue
		}
		if !ok || target["transport"] != out["connection_type"] || target["transport"] != out["device"] {
			t.Fatalf("inconsistent connection snapshot: %v", out)
		}
	}
}

// A cloud:// selector passed to device_connect (or used as the startup
// default) is replayable: record the selector itself, not the transient local
// tunnel address, so run can hand the same identity to a CLI child.
func TestCommandTargetCloudSelectorConnect(t *testing.T) {
	t.Setenv("WENDY_BROKER_URL", "selected-relay:443")
	const selector = "cloud://shared.example:443/org/7/asset/42"
	for _, startup := range []bool{false, true} {
		s := New(&config.Config{}, func(_ context.Context, device string) (*grpcclient.AgentConnection, error) {
			if device != selector {
				t.Fatalf("connect got %q, want %q", device, selector)
			}
			return &grpcclient.AgentConnection{Host: "127.0.0.1", Addr: "127.0.0.1:61000"}, nil
		})
		if startup {
			if err := s.ConnectToOnStartup(context.Background(), selector); err != nil {
				t.Fatal(err)
			}
		} else {
			r, err := s.handleDeviceConnect(context.Background(), callToolReq("device_connect", map[string]any{"address": selector}))
			if err != nil || r.IsError {
				t.Fatalf("device_connect: %v %v", r, err)
			}
		}
		assertCommandTarget(t, s, commandTarget{Device: selector, Selector: selector, Transport: "cloud", CloudGRPC: "shared.example:443", BrokerURL: "selected-relay:443"})
	}
}

func TestCloudCommandTargetPinsOrganizationOrTenantAndAsset(t *testing.T) {
	auth := &config.AuthConfig{CloudGRPC: "shared.example:443", Certificates: []config.CertificateInfo{{OrganizationID: 7}}}
	want := commandTarget{Device: "robot", Transport: "cloud", CloudGRPC: "shared.example:443", BrokerURL: "relay:443", Selector: "cloud://shared.example:443/org/7/asset/42"}
	if got := cloudCommandTarget(auth, mcpCloudDevice{name: "robot", legacyID: 42, key: "42"}, "relay:443"); got != want {
		t.Fatalf("legacy target = %+v, want %+v", got, want)
	}
	auth.Certificates[0].PrincipalURI = "spiffe://wendy.sh/tenant/11111111-1111-4111-8111-111111111111/operator/test"
	got := cloudCommandTarget(auth, mcpCloudDevice{name: "robot", key: "22222222-2222-4222-8222-222222222222", isV2: true}, "")
	if got.Selector != "cloud://shared.example:443/tenant/11111111-1111-4111-8111-111111111111/asset/22222222-2222-4222-8222-222222222222" {
		t.Fatalf("v2 target = %+v", got)
	}
	// No certificate identity: keep the name-based target (run then uses the
	// legacy cloud path) rather than inventing a selector.
	got = cloudCommandTarget(&config.AuthConfig{CloudGRPC: "shared.example:443"}, mcpCloudDevice{name: "robot", legacyID: 42}, "")
	if got.Selector != "" || got.Device != "robot" || got.Transport != "cloud" {
		t.Fatalf("identity-less target = %+v", got)
	}
}

// `wendy run --device NAME` resolves a bare name differently from
// device_connect (by device ID, or a same-named cloud device after a LAN
// failure), so a bare host is replayed as the host:port device_connect dialed.
func TestCommandTargetReplaysBareHostsAsHostPort(t *testing.T) {
	for _, startup := range []bool{false, true} {
		for address, want := range map[string]string{
			"robot":       "robot:50051",
			"woof.local":  "woof.local:50051",
			"192.0.2.7":   "192.0.2.7:50051",
			"fe80::1%en0": "[fe80::1%en0]:50051",
		} {
			s := New(&config.Config{}, func(context.Context, string) (*grpcclient.AgentConnection, error) {
				return &grpcclient.AgentConnection{Host: "192.0.2.7", Addr: "192.0.2.7:50052"}, nil
			})
			connect := s.ConnectTo
			if startup {
				connect = s.ConnectToOnStartup
			}
			if err := connect(context.Background(), address); err != nil {
				t.Fatal(err)
			}
			assertCommandTarget(t, s, commandTarget{Device: want, Transport: "direct"})
		}
	}
}
