package commands

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/net/http2"
	"google.golang.org/grpc"

	"github.com/wendylabsinc/wendy/go/internal/cli/grpcclient"
	"github.com/wendylabsinc/wendy/go/internal/shared/cloudrelay"
	"github.com/wendylabsinc/wendy/go/internal/shared/config"
	"github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
	cloudpbv2 "github.com/wendylabsinc/wendy/go/proto/gen/cloudpb/v2"
)

// fakeMuxTunnel is one multiplexed tunnel served as the agent serves it after
// the endpoint handshake: every stream echoes what it receives. closed ends
// when the caller closes the tunnel.
func fakeMuxTunnel(t *testing.T, expires time.Time) (c *cloudrelay.MuxClient, closed <-chan struct{}) {
	t.Helper()
	served := make(chan struct{})
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		defer close(served)
		device, err := ln.Accept()
		if err != nil {
			return
		}
		t.Cleanup(func() { device.Close() })
		new(http2.Server).ServeConn(device, &http2.ServeConnOpts{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			rc := http.NewResponseController(w)
			w.WriteHeader(http.StatusOK)
			_ = rc.Flush()
			buf := make([]byte, 1024)
			for {
				n, err := r.Body.Read(buf)
				if n > 0 {
					_, _ = w.Write(buf[:n])
					_ = rc.Flush()
				}
				if err != nil {
					return
				}
			}
		})})
	}()
	caller, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	c, err = cloudrelay.NewMuxClient(caller, expires)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	return c, served
}

// startForward serves a forward on a fresh listener until the test ends. A
// failed dial is a user-visible error and fails the test.
func startForward(t *testing.T, dial func(context.Context) (net.Conn, error)) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	served := make(chan error, 1)
	go func() { served <- serveTCPForward(ctx, ln, dial, func(err error) { t.Errorf("forwarded connection failed: %v", err) }, nil) }()
	t.Cleanup(func() {
		cancel()
		if err := <-served; err != nil {
			t.Errorf("forward: %v", err)
		}
	})
	return ln.Addr().String()
}

// echoes sends one line on conn and checks it comes back.
func echoes(t *testing.T, conn net.Conn, line string) {
	t.Helper()
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
	if _, err := io.WriteString(conn, line); err != nil {
		t.Fatal(err)
	}
	got := make([]byte, len(line))
	if _, err := io.ReadFull(conn, got); err != nil || string(got) != line {
		t.Fatalf("echo %q: got %q, %v", line, got, err)
	}
}

// Many connections over one forward share one tunnel, and so one attestation.
func TestCloudTunnelForwardCarriesConnectionsInOneTunnel(t *testing.T) {
	var opens atomic.Int32
	mux := &forwardMux{
		open: func() (*cloudrelay.MuxClient, error) {
			opens.Add(1)
			c, _ := fakeMuxTunnel(t, time.Now().Add(30*time.Minute))
			return c, nil
		},
		fallback: func(context.Context) (net.Conn, error) {
			return nil, errors.New("a connection opened a tunnel of its own")
		},
	}
	addr := startForward(t, mux.dial)
	var wg sync.WaitGroup
	for i := range 5 {
		wg.Go(func() {
			conn, err := net.Dial("tcp", addr)
			if err != nil {
				t.Error(err)
				return
			}
			defer conn.Close()
			echoes(t, conn, fmt.Sprintf("connection %d\n", i))
		})
	}
	wg.Wait()
	if n := opens.Load(); n != 1 {
		t.Fatalf("5 connections opened %d tunnels, want 1", n)
	}
}

// Near its relay's end the forward opens a new tunnel for new connections; it
// keeps listening, a connection on the old tunnel keeps working, and the old
// tunnel closes once that connection ends.
func TestCloudTunnelForwardSurvivesRenewal(t *testing.T) {
	var opens atomic.Int32
	var firstClosed <-chan struct{}
	mux := &forwardMux{
		open: func() (*cloudrelay.MuxClient, error) {
			if opens.Add(1) == 1 {
				c, closed := fakeMuxTunnel(t, time.Now().Add(muxRenewBefore-time.Minute))
				firstClosed = closed
				return c, nil
			}
			c, _ := fakeMuxTunnel(t, time.Now().Add(30*time.Minute))
			return c, nil
		},
		fallback: func(context.Context) (net.Conn, error) {
			return nil, errors.New("a connection opened a tunnel of its own")
		},
	}
	addr := startForward(t, mux.dial)
	first, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	echoes(t, first, "on the first tunnel\n")
	for i := range 2 {
		conn, err := net.Dial("tcp", addr)
		if err != nil {
			t.Fatal(err)
		}
		echoes(t, conn, fmt.Sprintf("after renewal %d\n", i))
		conn.Close()
	}
	echoes(t, first, "still on the first tunnel\n")
	first.Close()
	select {
	case <-firstClosed:
	case <-time.After(5 * time.Second):
		t.Fatal("the replaced tunnel stayed open after its last connection ended")
	}
	if n := opens.Load(); n != 2 {
		t.Fatalf("opened %d tunnels, want 2: the first and its renewal", n)
	}
}

// Renewal opens the next tunnel in the background: while it opens, new
// connections keep using the current tunnel rather than wait for it.
func TestCloudTunnelForwardRenewsInBackground(t *testing.T) {
	release, renewed := make(chan struct{}), make(chan struct{})
	var opens atomic.Int32
	mux := &forwardMux{
		open: func() (*cloudrelay.MuxClient, error) {
			if opens.Add(1) > 1 {
				defer close(renewed)
				<-release
				return nil, errors.New("renewal failed")
			}
			c, _ := fakeMuxTunnel(t, time.Now().Add(muxRenewBefore-time.Minute))
			return c, nil
		},
		fallback: func(context.Context) (net.Conn, error) {
			return nil, errors.New("a connection opened a tunnel of its own")
		},
	}
	addr := startForward(t, mux.dial)
	for i := range 3 {
		conn, err := net.Dial("tcp", addr)
		if err != nil {
			t.Fatal(err)
		}
		echoes(t, conn, fmt.Sprintf("while renewing %d\n", i))
		conn.Close()
	}
	close(release)
	<-renewed
}

// oldAgent answers GetAgentVersion without TCPMuxFeature.
type oldAgent struct{ agentpb.WendyAgentServiceClient }

func (oldAgent) GetAgentVersion(context.Context, *agentpb.GetAgentVersionRequest, ...grpc.CallOption) (*agentpb.GetAgentVersionResponse, error) {
	return &agentpb.GetAgentVersionResponse{Featureset: []string{cloudrelay.TCPHalfCloseFeature}}, nil
}

// An agent without the feature gets today's tunnel per connection, and the
// user sees no error.
func TestCloudTunnelForwardFallsBackForOldAgent(t *testing.T) {
	echo, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer echo.Close()
	go func() {
		for {
			c, err := echo.Accept()
			if err != nil {
				return
			}
			go func() { defer c.Close(); _, _ = io.Copy(c, c) }()
		}
	}()
	id := "00000000-0000-4000-8000-000000000042"
	asset := &cloudpbv2.Asset{Id: id}
	d := cloudDiscoveryDevice{cloudAssetMetadata: asset, v2: asset, key: id}
	agent := &grpcclient.AgentConnection{AgentService: oldAgent{}}
	var openErr error
	var fallbacks atomic.Int32
	mux := &forwardMux{
		open: func() (*cloudrelay.MuxClient, error) {
			c, err := d.openForwardMux(t.Context(), &config.AuthConfig{}, agent, 3000)
			openErr = err
			return c, err
		},
		fallback: func(ctx context.Context) (net.Conn, error) {
			fallbacks.Add(1)
			return (&net.Dialer{}).DialContext(ctx, "tcp", echo.Addr().String())
		},
	}
	conn, err := net.Dial("tcp", startForward(t, mux.dial))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	echoes(t, conn, "through a tunnel of its own\n")
	if !errors.Is(openErr, errMuxUnsupported) {
		t.Fatalf("opening the shared tunnel: %v, want %v", openErr, errMuxUnsupported)
	}
	if n := fallbacks.Load(); n != 1 {
		t.Fatalf("%d connections used their own tunnel, want 1", n)
	}
}
