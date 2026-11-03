package mcp

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/cli/grpcclient"
	"github.com/wendylabsinc/wendy/go/internal/shared/config"
	"google.golang.org/grpc"
	"google.golang.org/grpc/connectivity"
	"google.golang.org/grpc/credentials/insecure"
)

// certificateTestConn is a direct mTLS connection that is not Ready (gRPC
// dials lazily, so it stays Idle) and whose certificate expires at notAfter.
func certificateTestConn(t *testing.T, notAfter time.Time) *grpcclient.AgentConnection {
	t.Helper()
	cc, err := grpc.NewClient("passthrough:///127.0.0.1:1", grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	conn := grpcclient.NewFromConn(cc)
	conn.Host, conn.Addr, conn.IsMTLS = "mesh-1.local", "mesh-1.local:50052", true
	conn.CertInfo = &config.CertificateInfo{PemCertificate: authTestCertPEM(t, notAfter)}
	t.Cleanup(func() { _ = conn.Close() })
	return conn
}

// A long-running server must not keep redialling a device with a certificate
// that has expired: it reconnects with the login stored now, and when that
// fails it lets go of the connection instead of retrying the expired one.
func TestExpiredDeviceCertificateConnectionIsReplaced(t *testing.T) {
	now := time.Now()
	target := commandTarget{Device: "mesh-1.local:50051", Transport: "direct"}
	for _, tc := range []struct {
		name       string
		notAfter   time.Time
		connectErr error
		wantDials  int
		want       string // "old", "new" or "none"
	}{
		{"renewed login reconnects", now.Add(-time.Hour), nil, 1, "new"},
		{"no usable login lets go", now.Add(-time.Hour), errors.New("remote error: tls: bad certificate"), 1, "none"},
		{"valid certificate is left alone", now.Add(time.Hour), nil, 0, "old"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			old := certificateTestConn(t, tc.notAfter)
			renewed := certificateTestConn(t, now.Add(24*time.Hour))
			dials := 0
			s := New(&config.Config{}, func(_ context.Context, address string) (*grpcclient.AgentConnection, error) {
				dials++
				if address != target.Device {
					t.Fatalf("reconnected to %q, want the same target %q", address, target.Device)
				}
				if tc.connectErr != nil {
					return nil, tc.connectErr
				}
				return renewed, nil
			})
			s.setConnection(old, "direct", target)

			s.replaceExpiredDeviceConnection(context.Background(), now)

			if dials != tc.wantDials {
				t.Fatalf("dials = %d, want %d", dials, tc.wantDials)
			}
			conn, connType, gotTarget := s.connectionSnapshot()
			switch tc.want {
			case "old":
				if conn != old || old.Conn.GetState() == connectivity.Shutdown {
					t.Fatalf("a connection with a valid certificate was replaced")
				}
			case "new":
				if conn != renewed || connType != "direct" || gotTarget != target {
					t.Fatalf("conn=%p type=%q target=%+v, want the renewed connection to the same target", conn, connType, gotTarget)
				}
			case "none":
				if conn != nil {
					t.Fatalf("kept a connection that can only present an expired certificate")
				}
				diags := s.proxyDiagnostics()
				if len(diags) != 1 || !strings.Contains(diags[0].Error, "operator certificate for this connection expired at") {
					t.Fatalf("diagnostics = %+v", diags)
				}
			}
			if tc.want != "old" && old.Conn.GetState() != connectivity.Shutdown {
				t.Fatalf("the expired connection was not closed, so gRPC keeps redialling with it")
			}
		})
	}
}

// A cloud connection captured its login in conn.Reconnect, so reconnecting
// through it would present the expired certificate again. The watch dials the
// cloud selector through connectFn instead, which reloads the stored login and
// renews it. A cloud target recorded only by device name (cloud_connect's)
// cannot be dialled that way and keeps conn.Reconnect.
func TestExpiredCloudConnectionReconnectsThroughConnectFn(t *testing.T) {
	now := time.Now()
	const selector = "cloud://api.wendy.dev:443/tenant/5c9e14bb-49c0-4178-9cc5-493a151f9885/asset/3701f33a-3617-4317-8f14-cca2e744a8e2"
	for _, tc := range []struct {
		name          string
		target        commandTarget
		wantConnectFn int
		wantReconnect int
	}{
		{"cloud selector", commandTarget{Device: selector, Selector: selector, Transport: "cloud", CloudGRPC: "api.wendy.dev:443"}, 1, 0},
		{"cloud device name", commandTarget{Device: "mesh-1", Selector: selector, Transport: "cloud", CloudGRPC: "api.wendy.dev:443"}, 0, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			old := certificateTestConn(t, now.Add(-time.Hour))
			renewed := certificateTestConn(t, now.Add(24*time.Hour))
			reconnects := 0
			old.Reconnect = func(context.Context) (*grpcclient.AgentConnection, error) {
				reconnects++
				return renewed, nil
			}
			dials := 0
			s := New(&config.Config{}, func(_ context.Context, address string) (*grpcclient.AgentConnection, error) {
				dials++
				if address != selector {
					t.Fatalf("connectFn(%q), want the cloud selector", address)
				}
				return renewed, nil
			})
			s.setConnection(old, "cloud", tc.target)

			s.replaceExpiredDeviceConnection(context.Background(), now)

			if dials != tc.wantConnectFn || reconnects != tc.wantReconnect {
				t.Fatalf("connectFn=%d Reconnect=%d, want %d and %d", dials, reconnects, tc.wantConnectFn, tc.wantReconnect)
			}
			if conn, connType, target := s.connectionSnapshot(); conn != renewed || connType != "cloud" || target != tc.target {
				t.Fatalf("conn=%p type=%q target=%+v, want the new connection to the same target", conn, connType, target)
			}
		})
	}
}

// Losing the transport usually means the device is rebooting, so a reconnect
// that nothing answered keeps the connection and its target, and a later
// check connects once the device is back. A refusal still lets go.
func TestExpiredDeviceCertificateReconnectRetriesWhileDeviceIsDown(t *testing.T) {
	now := time.Now()
	target := commandTarget{Device: "mesh-1.local:50051", Transport: "direct"}
	refused := errors.New("dial tcp 192.168.1.20:50052: connect: connection refused")
	old := certificateTestConn(t, now.Add(-time.Hour))
	renewed := certificateTestConn(t, now.Add(24*time.Hour))
	plaintext := certificateTestConn(t, now.Add(24*time.Hour))
	plaintext.IsMTLS = false
	// Down, then answered only by the lazy plaintext rung an unpinned dial
	// falls back to, then back.
	attempts := []func() (*grpcclient.AgentConnection, error){
		func() (*grpcclient.AgentConnection, error) { return nil, refused },
		func() (*grpcclient.AgentConnection, error) { return plaintext, nil },
		func() (*grpcclient.AgentConnection, error) { return renewed, nil },
	}
	dials := 0
	s := New(&config.Config{}, func(_ context.Context, address string) (*grpcclient.AgentConnection, error) {
		if address != target.Device {
			t.Fatalf("reconnected to %q, want %q", address, target.Device)
		}
		dials++
		return attempts[dials-1]()
	})
	s.SetTransientConnectError(func(err error) bool { return errors.Is(err, refused) })
	s.setConnection(old, "direct", target)

	for tick := 1; tick <= 2; tick++ {
		s.replaceExpiredDeviceConnection(context.Background(), now)
		if conn, _, got := s.connectionSnapshot(); conn != old || got != target {
			t.Fatalf("tick %d: conn=%p target=%+v, want the connection and target kept", tick, conn, got)
		}
	}
	if plaintext.Conn.GetState() != connectivity.Shutdown {
		t.Fatal("the plaintext connection was not closed")
	}
	if diags := s.proxyDiagnostics(); len(diags) != 1 || !strings.Contains(diags[0].Error, "trying again at each check") {
		t.Fatalf("diagnostics = %+v, want one note for the retries", diags)
	}

	s.replaceExpiredDeviceConnection(context.Background(), now)
	if conn, _, got := s.connectionSnapshot(); conn != renewed || got != target || dials != 3 {
		t.Fatalf("conn=%p target=%+v dials=%d, want the renewed connection on the third check", conn, got, dials)
	}
	if old.Conn.GetState() != connectivity.Shutdown {
		t.Fatal("the expired connection was not closed")
	}

	// A refusal is not transient: the connection is let go.
	expired := certificateTestConn(t, now.Add(-time.Hour))
	s.setConnection(expired, "direct", target)
	attempts = append(attempts, func() (*grpcclient.AgentConnection, error) {
		return nil, errors.New("Your Wendy login for Wendy Labs, Inc. expired at 2026-10-28 11:33 UTC.")
	})
	s.replaceExpiredDeviceConnection(context.Background(), now)
	if conn, _, got := s.connectionSnapshot(); conn != nil || got != (commandTarget{}) {
		t.Fatalf("conn=%p target=%+v, want the refused connection dropped", conn, got)
	}
}

// Before reconnecting a direct target, the watch renews the login that issued
// the expired certificate, once per connection: the device may stay down for
// many checks, and each renewal spends the session's refresh token. A cloud
// selector's dial renews its own login.
func TestExpiredDeviceCertificateRenewsTheLoginBeforeReconnecting(t *testing.T) {
	now := time.Now()
	refused := errors.New("dial tcp 192.168.1.20:50052: connect: connection refused")
	for _, tc := range []struct {
		name       string
		target     commandTarget
		renewErr   error
		wantRenews int
	}{
		{"direct target", commandTarget{Device: "mesh-1.local:50051", Transport: "direct"}, nil, 1},
		{"renewal fails", commandTarget{Device: "mesh-1.local:50051", Transport: "direct"}, errors.New("refresh token expired"), 1},
		{"cloud selector", commandTarget{Device: "cloud://api.wendy.dev:443/tenant/t/asset/a", Transport: "cloud"}, nil, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			old := certificateTestConn(t, now.Add(-time.Hour))
			renewed := certificateTestConn(t, now.Add(24*time.Hour))
			var events []string
			dials := 0
			s := New(&config.Config{}, func(context.Context, string) (*grpcclient.AgentConnection, error) {
				events = append(events, "dial")
				dials++
				if dials == 1 {
					return nil, refused
				}
				return renewed, nil
			})
			s.SetTransientConnectError(func(err error) bool { return errors.Is(err, refused) })
			s.SetLoginRenewer(func(_ context.Context, cert config.CertificateInfo) error {
				if cert.PemCertificate != old.CertInfo.PemCertificate {
					t.Fatal("renewed a login other than the one that issued the expired certificate")
				}
				events = append(events, "renew")
				return tc.renewErr
			})
			s.setConnection(old, "direct", tc.target)

			s.replaceExpiredDeviceConnection(context.Background(), now) // the device is still down
			s.replaceExpiredDeviceConnection(context.Background(), now) // back

			want := []string{"dial", "dial"}
			if tc.wantRenews == 1 {
				want = []string{"renew", "dial", "dial"}
			}
			if strings.Join(events, ",") != strings.Join(want, ",") {
				t.Fatalf("events = %v, want %v", events, want)
			}
			if conn, _, _ := s.connectionSnapshot(); conn != renewed {
				t.Fatalf("conn = %p, want the reconnected one", conn)
			}
			diags := s.proxyDiagnostics()
			renewNoted := len(diags) > 0 && strings.Contains(diags[0].Error, "could not renew the stored login")
			if renewNoted != (tc.renewErr != nil) {
				t.Fatalf("diagnostics = %+v", diags)
			}
		})
	}
}

// Connecting to vm:NAME boots the simulator, so the watch never reconnects one:
// it must not start a simulator that was stopped outside this server.
func TestExpiredSimulatorConnectionIsNotBooted(t *testing.T) {
	now := time.Now()
	for name, setup := range map[string]func(*grpcclient.AgentConnection) commandTarget{
		"vm target": func(*grpcclient.AgentConnection) commandTarget {
			return commandTarget{Device: "vm:go2", Transport: "direct"}
		},
		"simulator connection": func(conn *grpcclient.AgentConnection) commandTarget {
			conn.SimulatorName = "go2"
			return commandTarget{Device: "127.0.0.1:50061", Transport: "direct"}
		},
	} {
		t.Run(name, func(t *testing.T) {
			old := certificateTestConn(t, now.Add(-time.Hour))
			target := setup(old)
			s := New(&config.Config{}, func(context.Context, string) (*grpcclient.AgentConnection, error) {
				t.Fatal("the watch connected to a simulator target")
				return nil, nil
			})
			s.setConnection(old, "direct", target)

			s.replaceExpiredDeviceConnection(context.Background(), now)

			if conn, _, got := s.connectionSnapshot(); conn != old || got != target {
				t.Fatalf("conn=%p target=%+v, want the simulator connection left alone", conn, got)
			}
		})
	}
}

// A connection a tool chose while the reconnect was in flight wins over it.
func TestExpiredDeviceCertificateReplacementYieldsToNewerConnection(t *testing.T) {
	now := time.Now()
	old := certificateTestConn(t, now.Add(-time.Hour))
	chosen := certificateTestConn(t, now.Add(time.Hour))
	replacement := certificateTestConn(t, now.Add(time.Hour))
	var s *mcpServer
	s = New(&config.Config{}, func(context.Context, string) (*grpcclient.AgentConnection, error) {
		s.setConnection(chosen, "direct", commandTarget{Device: "other:50051", Transport: "direct"})
		return replacement, nil
	})
	s.setConnection(old, "direct", commandTarget{Device: "mesh-1.local:50051", Transport: "direct"})

	s.replaceExpiredDeviceConnection(context.Background(), now)

	if conn, _, _ := s.connectionSnapshot(); conn != chosen {
		t.Fatalf("the tool's connection was replaced")
	}
	if replacement.Conn.GetState() != connectivity.Shutdown {
		t.Fatalf("the unused replacement was left open")
	}
}
