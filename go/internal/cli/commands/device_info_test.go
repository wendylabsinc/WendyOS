package commands

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/x/ansi"
	"github.com/wendylabsinc/wendy/go/internal/cli/grpcclient"
	"github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
	agentpbv2 "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
)

type deviceInfoClient struct {
	agentpb.WendyAgentServiceClient
	versionCalls int
	versionErr   error
	statusCalls  int
	updateStatus func(context.Context, *agentpb.GetOSUpdateStatusRequest) (*agentpb.GetOSUpdateStatusResponse, error)
}

func (c *deviceInfoClient) GetAgentVersion(context.Context, *agentpb.GetAgentVersionRequest, ...grpc.CallOption) (*agentpb.GetAgentVersionResponse, error) {
	c.versionCalls++
	return &agentpb.GetAgentVersionResponse{Version: "live-version", Os: "linux", CpuCount: 4}, c.versionErr
}

func (c *deviceInfoClient) GetOSUpdateStatus(ctx context.Context, req *agentpb.GetOSUpdateStatusRequest, _ ...grpc.CallOption) (*agentpb.GetOSUpdateStatusResponse, error) {
	c.statusCalls++
	return c.updateStatus(ctx, req)
}

func runDeviceInfoWithClient(t *testing.T, client *deviceInfoClient, cached *agentpb.GetAgentVersionResponse, asJSON bool, ctx context.Context) (string, error) {
	t.Helper()
	// Provisioning is unsupported in this fixture. Intercept those RPCs so the
	// command exercises its normal legacy fallback without opening a network.
	transport, err := grpc.NewClient("passthrough:///device-info-test",
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithUnaryInterceptor(func(context.Context, string, any, any, *grpc.ClientConn, grpc.UnaryInvoker, ...grpc.CallOption) error {
			return status.Error(codes.Unimplemented, "legacy agent")
		}))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = transport.Close() })
	conn := &grpcclient.AgentConnection{
		Conn:                transport,
		AgentService:        client,
		ProvisioningService: agentpb.NewWendyProvisioningServiceClient(transport),
	}
	conn.CacheAgentVersion(cached)
	previousResolve, previousJSON := resolveDeviceInfoTargetFn, jsonOutput
	t.Cleanup(func() {
		resolveDeviceInfoTargetFn, jsonOutput = previousResolve, previousJSON
	})
	resolveDeviceInfoTargetFn = func(context.Context, ...resolveOption) (*SelectedDevice, error) {
		return &SelectedDevice{Agent: conn}, nil
	}
	jsonOutput = asJSON
	cmd := newDeviceInfoCmd()
	cmd.SetContext(ctx)
	return captureCommandStdout(t, func() error { return cmd.RunE(cmd, nil) })
}

func TestDeviceInfoUsesPersistedStatusAndConnectionVersion(t *testing.T) {
	for _, asJSON := range []bool{false, true} {
		for _, cached := range []bool{false, true} {
			name := "text"
			if asJSON {
				name = "json"
			}
			if cached {
				name += "/cached-version"
			} else {
				name += "/uncached-version"
			}
			t.Run(name, func(t *testing.T) {
				client := &deviceInfoClient{updateStatus: func(ctx context.Context, req *agentpb.GetOSUpdateStatusRequest) (*agentpb.GetOSUpdateStatusResponse, error) {
					if req.GetIncludeEngineStatus() {
						t.Error("basic device info requested a live updater scan")
					}
					deadline, ok := ctx.Deadline()
					if !ok || time.Until(deadline) > deviceInfoOSUpdateTimeout {
						t.Error("optional update status request has no bounded deadline")
					}
					return &agentpb.GetOSUpdateStatusResponse{
						HasResult: true, Outcome: agentpb.GetOSUpdateStatusResponse_OUTCOME_COMMITTED,
						OldOsVersion: "0.16.0", NewOsVersion: "0.17.0",
					}, nil
				}}
				var version *agentpb.GetAgentVersionResponse
				wantVersion, wantCalls := "live-version", 1
				if cached {
					version = &agentpb.GetAgentVersionResponse{Version: "probe-version", Os: "linux", CpuCount: 4}
					wantVersion, wantCalls = "probe-version", 0
				}
				out, err := runDeviceInfoWithClient(t, client, version, asJSON, context.Background())
				if err != nil {
					t.Fatal(err)
				}
				if client.versionCalls != wantCalls || client.statusCalls != 1 {
					t.Fatalf("version/status RPC calls = %d/%d, want %d/1", client.versionCalls, client.statusCalls, wantCalls)
				}
				if !asJSON {
					out = ansi.Strip(out)
					if !strings.Contains(out, "Agent Version: "+wantVersion) || !strings.Contains(out, "Last update: committed (0.16.0 → 0.17.0)") {
						t.Fatalf("missing device version or persisted update in output: %s", out)
					}
					return
				}
				var data struct {
					Version  string `json:"version"`
					OSUpdate struct {
						LastUpdate struct {
							Outcome      string `json:"outcome"`
							NewOSVersion string `json:"newOsVersion"`
						} `json:"lastUpdate"`
						Engine json.RawMessage `json:"engine"`
					} `json:"osUpdate"`
				}
				if err := json.Unmarshal([]byte(out), &data); err != nil {
					t.Fatal(err)
				}
				if data.Version != wantVersion || data.OSUpdate.LastUpdate.Outcome != "committed" || data.OSUpdate.LastUpdate.NewOSVersion != "0.17.0" || len(data.OSUpdate.Engine) != 0 {
					t.Fatalf("unexpected JSON output: %s", out)
				}
			})
		}
	}
}

func TestDeviceInfoShowsTheOSBuild(t *testing.T) {
	const sha = "4db3514d8971c00739d0831ae04ee6f663e09484"
	for _, tc := range []struct {
		name          string
		build, commit string
		wantOSLine    string
	}{
		{"pull request image", "pr-375", sha, "OS: linux WendyOS-0.21.0 (pr-375, 4db3514)"},
		{"nightly image", "nightly-20261121T165425", sha, "OS: linux WendyOS-0.21.0 (nightly-20261121T165425, 4db3514)"},
		// A release is stamped with its own version: the line stays as it was.
		{"release image", "0.21.0", sha, "OS: linux WendyOS-0.21.0"},
		{"agent that reports no build", "", "", "OS: linux WendyOS-0.21.0"},
	} {
		for _, asJSON := range []bool{false, true} {
			name := tc.name + "/text"
			if asJSON {
				name = tc.name + "/json"
			}
			t.Run(name, func(t *testing.T) {
				osVersion := "WendyOS-0.21.0"
				version := &agentpb.GetAgentVersionResponse{Version: "2026.11.23", Os: "linux", OsVersion: &osVersion}
				if tc.build != "" {
					version.OsBuild = &tc.build
				}
				if tc.commit != "" {
					version.OsCommit = &tc.commit
				}
				client := &deviceInfoClient{updateStatus: func(context.Context, *agentpb.GetOSUpdateStatusRequest) (*agentpb.GetOSUpdateStatusResponse, error) {
					return nil, status.Error(codes.Unimplemented, "no update status")
				}}
				out, err := runDeviceInfoWithClient(t, client, version, asJSON, context.Background())
				if err != nil {
					t.Fatal(err)
				}
				if !asJSON {
					if out = ansi.Strip(out); !strings.Contains(out, tc.wantOSLine+"\n") {
						t.Fatalf("want the line %q in:\n%s", tc.wantOSLine, out)
					}
					return
				}
				var data map[string]any
				if err := json.Unmarshal([]byte(out), &data); err != nil {
					t.Fatal(err)
				}
				if data["osVersion"] != "WendyOS-0.21.0" {
					t.Errorf("osVersion = %v, want it unchanged", data["osVersion"])
				}
				// The values as the device reports them, and no key at all
				// when it reports none.
				for key, want := range map[string]string{"osBuild": tc.build, "osCommit": tc.commit} {
					got, present := data[key]
					if present != (want != "") || (present && got != want) {
						t.Errorf("%s = %v (present: %v), want %q", key, got, present, want)
					}
				}
			})
		}
	}
}

func TestDeviceInfoOptionalStatusFailure(t *testing.T) {
	for _, code := range []codes.Code{codes.Unimplemented, codes.Unavailable, codes.DeadlineExceeded} {
		t.Run(code.String(), func(t *testing.T) {
			client := &deviceInfoClient{updateStatus: func(context.Context, *agentpb.GetOSUpdateStatusRequest) (*agentpb.GetOSUpdateStatusResponse, error) {
				return nil, status.Error(code, "update status unavailable")
			}}
			out, err := runDeviceInfoWithClient(t, client, nil, true, context.Background())
			if err != nil {
				t.Fatalf("optional status error failed device info: %v", err)
			}
			var data map[string]json.RawMessage
			if err := json.Unmarshal([]byte(out), &data); err != nil {
				t.Fatal(err)
			}
			if _, ok := data["osUpdate"]; ok {
				t.Fatalf("unavailable status should be omitted: %s", out)
			}
			if string(data["version"]) != `"live-version"` {
				t.Fatalf("device metadata missing after optional status failure: %s", out)
			}
		})
	}
}

func TestDeviceInfoUncachedVersionFailure(t *testing.T) {
	wantErr := errors.New("agent unavailable")
	client := &deviceInfoClient{versionErr: wantErr, updateStatus: func(ctx context.Context, _ *agentpb.GetOSUpdateStatusRequest) (*agentpb.GetOSUpdateStatusResponse, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	}}
	_, err := runDeviceInfoWithClient(t, client, nil, true, context.Background())
	if !errors.Is(err, wantErr) {
		t.Fatalf("error = %v, want wrapped version RPC error", err)
	}
	if client.statusCalls != 1 {
		t.Fatal("optional request did not finish after mandatory metadata failed")
	}
}

func TestDeviceInfoQueriesOverlap(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	started := make(chan string, 3)
	release := make(chan struct{})
	transport, err := grpc.NewClient("passthrough:///concurrent-device-info",
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithUnaryInterceptor(func(ctx context.Context, method string, _, reply any, _ *grpc.ClientConn, _ grpc.UnaryInvoker, _ ...grpc.CallOption) error {
			started <- method
			select {
			case <-release:
			case <-ctx.Done():
				return ctx.Err()
			}
			switch response := reply.(type) {
			case *agentpb.GetAgentVersionResponse:
				response.Version = "concurrent"
			case *agentpb.GetOSUpdateStatusResponse:
				response.HasResult = true
			case *agentpbv2.IsProvisionedResponse:
				response.ResponseType = &agentpbv2.IsProvisionedResponse_NotProvisioned{NotProvisioned: &agentpbv2.NotProvisionedResponse{}}
			default:
				t.Errorf("unexpected query %s", method)
			}
			return nil
		}))
	if err != nil {
		t.Fatal(err)
	}
	defer transport.Close()
	conn := &grpcclient.AgentConnection{Conn: transport, AgentService: agentpb.NewWendyAgentServiceClient(transport)}
	finished := make(chan error, 1)
	go func() {
		version, update, organization, err := fetchDeviceInfo(ctx, conn)
		if err == nil && (version.GetVersion() != "concurrent" || !update.GetHasResult() || organization == nil || organization.ID != "") {
			err = errors.New("parallel queries lost metadata")
		}
		finished <- err
	}()
	for range 3 {
		select {
		case <-started:
		case <-ctx.Done():
			t.Fatal("queries did not all start before any completed")
		}
	}
	close(release)
	if err := <-finished; err != nil {
		t.Fatal(err)
	}
}

func TestDeviceInfoCachedVersionHonorsCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	client := &deviceInfoClient{}
	_, err := runDeviceInfoWithClient(t, client, &agentpb.GetAgentVersionResponse{Version: "cached"}, true, ctx)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want context cancellation", err)
	}
	if client.versionCalls != 0 || client.statusCalls != 0 {
		t.Fatal("canceled command made an RPC")
	}
}
