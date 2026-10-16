package commands

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/wendylabsinc/wendy/go/internal/cli/grpcclient"
	"github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
	agentpbv2 "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

type artifactWiFiClient struct {
	agentpb.WendyAgentServiceClient
	calls     int
	connected bool
}

func (c *artifactWiFiClient) GetWiFiStatus(context.Context, *agentpb.GetWiFiStatusRequest, ...grpc.CallOption) (*agentpb.GetWiFiStatusResponse, error) {
	c.calls++
	return &agentpb.GetWiFiStatusResponse{Connected: c.connected}, nil
}

func artifactPreparationFixture(t *testing.T) (*grpcclient.AgentConnection, *atomic.Int32, string) {
	t.Helper()
	isolateArtifactDownload(t, false, false)
	stubLinkLocalSourceIP(t, func(context.Context, string, ...int) (string, bool) {
		return "127.0.0.1", true
	})
	originalListen := artifactListenFn
	t.Cleanup(func() { artifactListenFn = originalListen })
	artifactListenFn = func(ctx context.Context, addr string) (net.Listener, error) {
		var lc net.ListenConfig
		return lc.Listen(ctx, "tcp", addr)
	}
	requests := new(atomic.Int32)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		fmt.Fprint(w, "artifact bytes")
	}))
	t.Cleanup(server.Close)
	return &grpcclient.AgentConnection{
		Host: "192.0.2.1", AgentService: &artifactWiFiClient{},
		DriverService: fakeDriverClient{list: &agentpbv2.ListDriversResponse{}},
	}, requests, server.URL + "/os.wendy"
}

func requireArtifactCacheSize(t *testing.T, want int) {
	t.Helper()
	dir, err := osCacheDir()
	if err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != want {
		t.Fatalf("artifact cache contains %d entries, want %d (err=%v)", len(entries), want, err)
	}
}

func TestPrepareOSUpdateArtifactDriverFailureBeforeTransfer(t *testing.T) {
	for _, code := range []codes.Code{codes.Unimplemented, codes.DeadlineExceeded, codes.PermissionDenied} {
		t.Run(code.String(), func(t *testing.T) {
			conn, requests, url := artifactPreparationFixture(t)
			conn.DriverService = fakeDriverClient{listErr: status.Error(code, "driver list unavailable")}
			_, _, err := prepareOSUpdateArtifact(context.Background(), conn, &agentpb.GetAgentVersionResponse{}, osUpdateArtifactOptions{url: url})
			if err == nil || !strings.Contains(err.Error(), "installed set could not be read") {
				t.Fatalf("unreadable driver set must still abort: %v", err)
			}
			if requests.Load() != 0 || conn.AgentService.(*artifactWiFiClient).calls != 0 {
				t.Fatal("driver refusal happened only after preparing/downloading the OS image")
			}
			requireArtifactCacheSize(t, 0)
		})
	}
}

func TestPrepareOSUpdateArtifactCleanup(t *testing.T) {
	for _, override := range []bool{false, true} {
		t.Run(fmt.Sprintf("no-drivers=%v", override), func(t *testing.T) {
			conn, requests, url := artifactPreparationFixture(t)
			if override {
				conn.DriverService = nil
			}
			served, cleanup, err := prepareOSUpdateArtifact(context.Background(), conn, &agentpb.GetAgentVersionResponse{}, osUpdateArtifactOptions{url: url, noDrivers: override})
			if err != nil {
				t.Fatal(err)
			}
			defer cleanup()
			if requests.Load() != 1 {
				t.Fatalf("download requests = %d, want 1", requests.Load())
			}
			resp, err := http.Get(served)
			if err != nil {
				t.Fatal(err)
			}
			data, err := io.ReadAll(resp.Body)
			resp.Body.Close()
			if err != nil || string(data) != "artifact bytes" {
				t.Fatalf("served artifact = %q, err=%v", data, err)
			}
			requireArtifactCacheSize(t, 1)
			cleanup()
			requireArtifactCacheSize(t, 0)
		})
	}
}

func TestPrepareOSUpdateArtifactServerFailureCleanup(t *testing.T) {
	conn, _, url := artifactPreparationFixture(t)
	artifactListenFn = func(context.Context, string) (net.Listener, error) {
		return nil, fmt.Errorf("test listener refused")
	}
	_, _, err := prepareOSUpdateArtifact(context.Background(), conn, &agentpb.GetAgentVersionResponse{}, osUpdateArtifactOptions{url: url})
	if err == nil || !strings.Contains(err.Error(), "listener refused") {
		t.Fatalf("expected server error: %v", err)
	}
	requireArtifactCacheSize(t, 0)
}

func TestPrepareOSUpdateArtifactSafetyChecksBeforeTransfer(t *testing.T) {
	for _, reason := range []string{"unsupported stack", "empty drivers directory", "local file driver refusal"} {
		t.Run(reason, func(t *testing.T) {
			conn, requests, url := artifactPreparationFixture(t)
			opts := osUpdateArtifactOptions{url: url}
			version := &agentpb.GetAgentVersionResponse{}
			switch reason {
			case "unsupported stack":
				version.Featureset = []string{"mender"}
			case "empty drivers directory":
				opts.driversDir = t.TempDir()
			case "local file driver refusal":
				opts.url = ""
				opts.path = filepath.Join(t.TempDir(), "caller.wendy")
				if err := os.WriteFile(opts.path, []byte("caller file"), 0o600); err != nil {
					t.Fatal(err)
				}
				conn.DriverService = nil
			}
			artifactListenFn = func(context.Context, string) (net.Listener, error) {
				t.Error("started an artifact server before the safety refusal")
				return nil, fmt.Errorf("unexpected listener")
			}
			_, _, err := prepareOSUpdateArtifact(context.Background(), conn, version, opts)
			if err == nil || requests.Load() != 0 || conn.AgentService.(*artifactWiFiClient).calls != 0 {
				t.Fatalf("safety refusal must precede artifact preparation: err=%v, downloads=%d", err, requests.Load())
			}
			requireArtifactCacheSize(t, 0)
		})
	}
}

func TestPrepareOSUpdateArtifactDirectURL(t *testing.T) {
	for _, loopback := range []bool{false, true} {
		t.Run(fmt.Sprintf("loopback=%v", loopback), func(t *testing.T) {
			conn, requests, url := artifactPreparationFixture(t)
			conn.AgentService.(*artifactWiFiClient).connected = true
			if loopback {
				conn.Host = "127.0.0.1"
			}
			got, cleanup, err := prepareOSUpdateArtifact(context.Background(), conn, &agentpb.GetAgentVersionResponse{}, osUpdateArtifactOptions{url: url})
			if err != nil || got != url || requests.Load() != 0 {
				t.Fatalf("direct URL changed or downloaded: got=%q, err=%v, downloads=%d", got, err, requests.Load())
			}
			cleanup()
			if loopback && conn.AgentService.(*artifactWiFiClient).calls != 0 {
				t.Error("queried WiFi on a loopback port-forwarded device")
			}
			requireArtifactCacheSize(t, 0)
		})
	}
}

func TestPrepareOSUpdateArtifactPreservesCallerFile(t *testing.T) {
	conn, requests, _ := artifactPreparationFixture(t)
	path := filepath.Join(t.TempDir(), "caller.wendy")
	if err := os.WriteFile(path, []byte("caller file"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, cleanup, err := prepareOSUpdateArtifact(context.Background(), conn, &agentpb.GetAgentVersionResponse{}, osUpdateArtifactOptions{path: path})
	if err != nil {
		t.Fatal(err)
	}
	cleanup()
	if data, err := os.ReadFile(path); err != nil || string(data) != "caller file" {
		t.Fatalf("caller-owned artifact changed/removed: %q, %v", data, err)
	}
	if requests.Load() != 0 {
		t.Fatal("downloaded an artifact despite a caller-supplied local file")
	}
}
