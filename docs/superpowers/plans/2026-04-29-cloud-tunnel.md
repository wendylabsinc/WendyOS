# Cloud Tunnel Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add `wendy cloud run` (run via cloud-tunnelled gRPC) and `wendy cloud tunnel <local>:<remote>` (general TCP port-forward through the cloud broker) under the existing `wendy cloud` command group.

**Architecture:** The CLI opens an mTLS gRPC stream (`ClientTunnel`) to the cloud broker, which in turn asks the agent to dial a local port and relay bytes. A `net.Pipe()` bridges that stream into a standard `net.Conn`, which gRPC uses as its transport for the agent connection. `wendy cloud run` feeds that connection directly into the existing `runWithAgent` function; `wendy cloud tunnel` listens on a local TCP port and opens a fresh broker stream per accepted connection.

**Tech Stack:** Go, Cobra, Bubble Tea (`tui.NewPickerWithTitle`), gRPC with `grpc.WithContextDialer`, `cloudpb.TunnelBrokerServiceClient`, `cloudpb.AssetServiceClient`, `grpcclient.AgentConnection`

---

## File Map

| File | Action | Responsibility |
|---|---|---|
| `go/internal/cli/grpcclient/client.go` | Modify | Add exported `NewFromConn` constructor |
| `go/internal/cli/commands/cloud_tunnel.go` | Create | `dialCloudBroker`, `openBrokerTunnel`, `pickCloudDevice`, `tunnelDialer` |
| `go/internal/cli/commands/cloud_run.go` | Create | `newCloudRunCmd`, `cloudRunCommand` |
| `go/internal/cli/commands/cloud_forward.go` | Create | `newCloudTunnelCmd`, `cloudTunnelCommand`, `parseTunnelArg`, `serveTunnelConn` |
| `go/internal/cli/commands/cloud.go` | Modify | Register the two new subcommands |
| `go/internal/cli/commands/cloud_tunnel_test.go` | Create | Unit tests for `parseTunnelArg` |

---

### Task 1: Export `NewFromConn` in grpcclient

`cloud_run.go` needs to build an `AgentConnection` from a `*grpc.ClientConn` it creates itself. The internal `newAgentConnection` helper already does this; we just export it.

**Files:**
- Modify: `go/internal/cli/grpcclient/client.go`

- [ ] **Step 1: Add `NewFromConn` after `newAgentConnection`**

In `go/internal/cli/grpcclient/client.go`, add immediately after `newAgentConnection`:

```go
// NewFromConn wraps an existing gRPC connection as an AgentConnection.
// Use this when the caller manages its own dialing (e.g. a cloud tunnel).
func NewFromConn(conn *grpc.ClientConn) *AgentConnection {
	return newAgentConnection(conn)
}
```

- [ ] **Step 2: Build to verify**

```bash
cd go && go build ./internal/cli/grpcclient/...
```
Expected: no errors.

- [ ] **Step 3: Commit**

```bash
git add go/internal/cli/grpcclient/client.go
git commit -m "feat(grpcclient): export NewFromConn for custom-dialer connections"
```

---

### Task 2: Core tunnel helpers (`cloud_tunnel.go`)

Three shared functions used by both `wendy cloud run` and `wendy cloud tunnel`:

1. `dialCloudBroker` — mTLS gRPC connection to the broker.
2. `openBrokerTunnel` — opens a `ClientTunnel` stream and returns a `net.Conn` backed by a pipe relay.
3. `pickCloudDevice` — lists `is_compute_device` assets, shows a TUI picker (or matches by name).
4. `tunnelDialer` — wraps a `net.Conn` as a `grpc.DialOption`.

**Files:**
- Create: `go/internal/cli/commands/cloud_tunnel.go`

- [ ] **Step 1: Create `cloud_tunnel.go`**

```go
package commands

import (
	"context"
	"fmt"
	"io"
	"net"
	"strings"
	"sync"

	tea "github.com/charmbracelet/bubbletea"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"

	"github.com/wendylabsinc/wendy/internal/cli/tui"
	"github.com/wendylabsinc/wendy/internal/shared/certs"
	"github.com/wendylabsinc/wendy/internal/shared/config"
	cloudpb "github.com/wendylabsinc/wendy/proto/gen/cloudpb"
)

const defaultBrokerPort = "50053"

// dialCloudBroker opens an mTLS gRPC connection to the tunnel broker.
// brokerURL is host:port; if empty it is derived from auth.CloudGRPC.
func dialCloudBroker(auth *config.AuthConfig, brokerURL string) (*grpc.ClientConn, error) {
	if brokerURL == "" {
		host := auth.CloudGRPC
		if h, _, err := net.SplitHostPort(host); err == nil {
			host = h
		}
		brokerURL = net.JoinHostPort(host, defaultBrokerPort)
	}

	cert := auth.Certificates[0]
	tlsCfg, err := certs.LoadTLSConfig(
		cert.PemCertificate,
		cert.PemCertificateChain,
		cert.PemPrivateKey,
		"",
	)
	if err != nil {
		return nil, fmt.Errorf("loading broker TLS config: %w", err)
	}

	var transportCreds grpc.DialOption
	if strings.HasSuffix(brokerURL, ":443") {
		transportCreds = grpc.WithTransportCredentials(credentials.NewTLS(tlsCfg))
	} else {
		transportCreds = grpc.WithTransportCredentials(insecure.NewCredentials())
	}
	conn, err := grpc.NewClient(brokerURL, transportCreds)
	if err != nil {
		return nil, fmt.Errorf("connecting to broker at %s: %w", brokerURL, err)
	}
	return conn, nil
}

// openBrokerTunnel asks the broker to connect to remotePort on the given asset
// and returns a net.Conn whose reads/writes are relayed through the tunnel stream.
// The caller is responsible for closing the returned conn.
func openBrokerTunnel(ctx context.Context, brokerConn *grpc.ClientConn, auth *config.AuthConfig, assetID int32, remotePort uint32) (net.Conn, error) {
	client := cloudpb.NewTunnelBrokerServiceClient(brokerConn)

	callCtx := ctx
	if auth.APIKey != "" {
		callCtx = metadata.NewOutgoingContext(ctx, metadata.Pairs("authorization", "Bearer "+auth.APIKey))
	}

	stream, err := client.ClientTunnel(callCtx)
	if err != nil {
		return nil, fmt.Errorf("opening tunnel stream: %w", err)
	}

	if err := stream.Send(&cloudpb.ClientTunnelMessage{
		Content: &cloudpb.ClientTunnelMessage_Open{
			Open: &cloudpb.ClientTunnelOpen{
				AssetId: assetID,
				Host:    "localhost",
				Port:    remotePort,
			},
		},
	}); err != nil {
		return nil, fmt.Errorf("sending tunnel open: %w", err)
	}

	// Bridge the gRPC stream into a net.Conn via a synchronous pipe.
	local, remote := net.Pipe()

	go func() {
		defer remote.Close()
		for {
			msg, err := stream.Recv()
			if err != nil {
				break
			}
			if len(msg.Payload) > 0 {
				if _, err := remote.Write(msg.Payload); err != nil {
					break
				}
			}
			if msg.HalfClose {
				break
			}
		}
	}()

	go func() {
		buf := make([]byte, 32*1024)
		for {
			n, readErr := remote.Read(buf)
			if n > 0 {
				payload := make([]byte, n)
				copy(payload, buf[:n])
				if err := stream.Send(&cloudpb.ClientTunnelMessage{
					Content: &cloudpb.ClientTunnelMessage_Data{
						Data: &cloudpb.TunnelData{Payload: payload},
					},
				}); err != nil {
					break
				}
			}
			if readErr != nil {
				if readErr == io.EOF {
					_ = stream.Send(&cloudpb.ClientTunnelMessage{
						Content: &cloudpb.ClientTunnelMessage_Data{
							Data: &cloudpb.TunnelData{HalfClose: true},
						},
					})
				}
				break
			}
		}
		_ = stream.CloseSend()
	}()

	return local, nil
}

// pickCloudDevice lists compute-device assets in the org and shows a TUI
// picker. If deviceName is non-empty and matches exactly one asset name
// (case-insensitive), the picker is skipped.
func pickCloudDevice(ctx context.Context, auth *config.AuthConfig, deviceName string) (*cloudpb.Asset, error) {
	cert := auth.Certificates[0]
	tlsCfg, err := certs.LoadTLSConfig(
		cert.PemCertificate,
		cert.PemCertificateChain,
		cert.PemPrivateKey,
		"",
	)
	if err != nil {
		return nil, fmt.Errorf("loading TLS config: %w", err)
	}

	var transportCreds grpc.DialOption
	if strings.HasSuffix(auth.CloudGRPC, ":443") {
		transportCreds = grpc.WithTransportCredentials(credentials.NewTLS(tlsCfg))
	} else {
		transportCreds = grpc.WithTransportCredentials(insecure.NewCredentials())
	}
	cloudConn, err := grpc.NewClient(auth.CloudGRPC, transportCreds)
	if err != nil {
		return nil, fmt.Errorf("connecting to cloud: %w", err)
	}
	defer cloudConn.Close()

	callCtx := ctx
	if auth.APIKey != "" {
		callCtx = metadata.NewOutgoingContext(ctx, metadata.Pairs("authorization", "Bearer "+auth.APIKey))
	}

	assetClient := cloudpb.NewAssetServiceClient(cloudConn)
	resp, err := assetClient.ListAssets(callCtx, &cloudpb.ListAssetsRequest{
		OrganizationId:  int32(cert.OrganizationID),
		IsComputeDevice: boolPtr(true),
		PageSize:        100,
	})
	if err != nil {
		return nil, fmt.Errorf("listing devices: %w", err)
	}

	assets := resp.GetAssets()
	if len(assets) == 0 {
		return nil, fmt.Errorf("no enrolled devices found for this org; enroll a device with 'wendy device enroll' first")
	}

	if deviceName != "" {
		lower := strings.ToLower(deviceName)
		var matched *cloudpb.Asset
		for _, a := range assets {
			if strings.ToLower(a.GetName()) == lower {
				if matched != nil {
					return nil, fmt.Errorf("multiple devices match %q; use a more specific name", deviceName)
				}
				matched = a
			}
		}
		if matched != nil {
			return matched, nil
		}
		return nil, fmt.Errorf("no device named %q found; omit --device to choose from a list", deviceName)
	}

	if len(assets) == 1 {
		return assets[0], nil
	}

	picker := tui.NewPickerWithTitle("Select a cloud device")
	items := make([]tui.PickerItem, 0, len(assets))
	for _, a := range assets {
		aCopy := a
		items = append(items, tui.PickerItem{
			Name:        a.GetName(),
			Description: fmt.Sprintf("asset %d", a.GetId()),
			Type:        "Cloud",
			Value:       aCopy,
		})
	}
	p := tea.NewProgram(picker)
	p.Send(tui.PickerAddMsg{Items: items})
	p.Send(tui.PickerDoneMsg{})

	finalModel, err := p.Run()
	if err != nil {
		return nil, fmt.Errorf("device picker: %w", err)
	}
	pm := finalModel.(tui.PickerModel)
	if pm.Cancelled() {
		return nil, ErrUserCancelled
	}
	sel := pm.Selected()
	if sel == nil {
		return nil, fmt.Errorf("no device selected")
	}
	asset, ok := sel.Value.(*cloudpb.Asset)
	if !ok {
		return nil, fmt.Errorf("invalid picker selection")
	}
	return asset, nil
}

func boolPtr(b bool) *bool { return &b }

// tunnelDialer returns a grpc.DialOption that routes all dials through the
// given net.Conn (the broker tunnel). The returned closer shuts the conn.
func tunnelDialer(tunnelConn net.Conn) (grpc.DialOption, func()) {
	var once sync.Once
	return grpc.WithContextDialer(func(_ context.Context, _ string) (net.Conn, error) {
		return tunnelConn, nil
	}), func() { once.Do(func() { tunnelConn.Close() }) }
}
```

- [ ] **Step 2: Build to verify**

```bash
cd go && go build ./internal/cli/commands/...
```
Expected: no errors.

- [ ] **Step 3: Commit**

```bash
git add go/internal/cli/commands/cloud_tunnel.go
git commit -m "feat(cloud): add broker tunnel helpers (dialCloudBroker, openBrokerTunnel, pickCloudDevice)"
```

---

### Task 3: `wendy cloud run`

**Files:**
- Create: `go/internal/cli/commands/cloud_run.go`

- [ ] **Step 1: Create `cloud_run.go`**

```go
package commands

import (
	"context"
	"fmt"
	"os"

	"github.com/spf13/cobra"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	"github.com/wendylabsinc/wendy/internal/cli/grpcclient"
)

func newCloudRunCmd() *cobra.Command {
	var opts runOptions
	var cloudGRPC string
	var deviceName string
	var brokerURL string

	cmd := &cobra.Command{
		Use:   "run",
		Short: "Build and run application on a cloud-enrolled device",
		Long:  "Same as 'wendy run' but connects to the device through the Wendy Cloud tunnel broker instead of a direct network path.",
		RunE: func(cmd *cobra.Command, args []string) error {
			return cloudRunCommand(cmd.Context(), opts, cloudGRPC, deviceName, brokerURL)
		},
	}

	cmd.Flags().StringVar(&opts.buildType, "build-type", "", "Build type: docker, swift, or python")
	cmd.Flags().BoolVar(&opts.debug, "debug", false, "Enable debug logging")
	cmd.Flags().BoolVar(&opts.deploy, "deploy", false, "Create container but do not start it")
	cmd.Flags().BoolVar(&opts.detach, "detach", false, "Start container but do not stream logs")
	cmd.Flags().BoolVarP(&opts.yes, "yes", "y", false, "Automatically accept all interactive prompts")
	cmd.Flags().BoolVar(&opts.restartUnlessStopped, "restart-unless-stopped", false, "Restart unless manually stopped")
	cmd.Flags().BoolVar(&opts.restartOnFailure, "restart-on-failure", false, "Restart on failure")
	cmd.Flags().BoolVar(&opts.noRestart, "no-restart", false, "Do not restart on exit")
	cmd.Flags().StringVar(&opts.prefix, "prefix", "", "Project directory instead of current working directory")
	cmd.Flags().StringVar(&opts.product, "product", "", "Swift Package Manager product to build and run")
	cmd.Flags().StringSliceVar(&opts.userArgs, "user-args", nil, "Extra arguments to pass to the container")
	cmd.Flags().StringVar(&cloudGRPC, "cloud-grpc", "", "Cloud gRPC endpoint (required when multiple auth sessions exist)")
	cmd.Flags().StringVar(&deviceName, "device", "", "Device name (skips interactive picker)")
	cmd.Flags().StringVar(&brokerURL, "broker-url", os.Getenv("WENDY_BROKER_URL"), "Tunnel broker host:port (default: <cloud-host>:50053)")

	return cmd
}

func cloudRunCommand(ctx context.Context, opts runOptions, cloudGRPC, deviceName, brokerURL string) error {
	cwd, err := resolveRunWorkingDir(opts)
	if err != nil {
		return fmt.Errorf("resolving working directory: %w", err)
	}

	appCfg, err := ensureAppConfig(cwd+"/wendy.json", opts.yes)
	if err != nil {
		return fmt.Errorf("loading wendy.json: %w", err)
	}
	if err := appCfg.Validate(); err != nil {
		return fmt.Errorf("invalid wendy.json: %w", err)
	}

	auth, err := pickAuthEntry(cloudGRPC)
	if err != nil {
		return err
	}

	cliLogln("Fetching device list from cloud...")
	asset, err := pickCloudDevice(ctx, auth, deviceName)
	if err != nil {
		return err
	}
	cliLogln("Connecting to %s via cloud tunnel...", asset.GetName())

	brokerConn, err := dialCloudBroker(auth, brokerURL)
	if err != nil {
		return err
	}
	defer brokerConn.Close()

	tunnelConn, err := openBrokerTunnel(ctx, brokerConn, auth, asset.GetId(), defaultAgentPort)
	if err != nil {
		return fmt.Errorf("opening cloud tunnel to %s: %w", asset.GetName(), err)
	}

	dialOpt, closeTunnel := tunnelDialer(tunnelConn)
	defer closeTunnel()

	grpcConn, err := grpc.NewClient(
		"passthrough:///cloud-tunnel",
		dialOpt,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		return fmt.Errorf("creating tunnelled gRPC connection: %w", err)
	}
	defer grpcConn.Close()

	agentConn := grpcclient.NewFromConn(grpcConn)
	agentConn.Host = asset.GetName()
	defer agentConn.Close()

	return runWithAgent(ctx, agentConn, cwd, appCfg, opts)
}
```

- [ ] **Step 2: Build to verify**

```bash
cd go && go build ./internal/cli/commands/...
```
Expected: fails on undefined `newCloudTunnelCmd` (added to `cloud.go` next step). That's fine — fix in Step 3.

- [ ] **Step 3: Wire both commands into `cloud.go`**

Replace the body of `newCloudCmd` in `go/internal/cli/commands/cloud.go`:

```go
func newCloudCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "cloud",
		Short: "Manage Wendy Cloud resources",
	}

	cmd.AddCommand(newCloudEnrollDeviceCmd())
	cmd.AddCommand(newCloudRunCmd())
	cmd.AddCommand(newCloudTunnelCmd())
	return cmd
}
```

`newCloudTunnelCmd` is defined in Task 4. Build will succeed after Task 4.

- [ ] **Step 4: Commit**

```bash
git add go/internal/cli/commands/cloud_run.go go/internal/cli/commands/cloud.go
git commit -m "feat(cloud): add 'wendy cloud run' command"
```

---

### Task 4: `wendy cloud tunnel`

**Files:**
- Create: `go/internal/cli/commands/cloud_forward.go`

- [ ] **Step 1: Create `cloud_forward.go`**

```go
package commands

import (
	"context"
	"fmt"
	"io"
	"net"
	"os"
	"strconv"
	"strings"

	"github.com/spf13/cobra"
	"google.golang.org/grpc"

	"github.com/wendylabsinc/wendy/internal/shared/config"
)

func newCloudTunnelCmd() *cobra.Command {
	var cloudGRPC string
	var deviceName string
	var brokerURL string

	cmd := &cobra.Command{
		Use:   "tunnel <local-port>:<remote-port>",
		Short: "Forward a local TCP port to a port on a cloud-enrolled device",
		Long:  "Listens on <local-port> and forwards each connection through the Wendy Cloud tunnel broker to <remote-port> on the target device.",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			localPort, remotePort, err := parseTunnelArg(args[0])
			if err != nil {
				return err
			}
			return cloudTunnelCommand(cmd.Context(), cloudGRPC, deviceName, brokerURL, localPort, remotePort)
		},
	}

	cmd.Flags().StringVar(&cloudGRPC, "cloud-grpc", "", "Cloud gRPC endpoint (required when multiple auth sessions exist)")
	cmd.Flags().StringVar(&deviceName, "device", "", "Device name (skips interactive picker)")
	cmd.Flags().StringVar(&brokerURL, "broker-url", os.Getenv("WENDY_BROKER_URL"), "Tunnel broker host:port (default: <cloud-host>:50053)")

	return cmd
}

// parseTunnelArg parses "localPort:remotePort" or just "port" (same for both sides).
func parseTunnelArg(arg string) (localPort, remotePort uint32, err error) {
	parts := strings.SplitN(arg, ":", 2)
	parse := func(s string) (uint32, error) {
		n, e := strconv.ParseUint(s, 10, 32)
		if e != nil || n == 0 || n > 65535 {
			return 0, fmt.Errorf("invalid port %q", s)
		}
		return uint32(n), nil
	}
	if len(parts) == 1 {
		p, e := parse(parts[0])
		return p, p, e
	}
	lp, e := parse(parts[0])
	if e != nil {
		return 0, 0, e
	}
	rp, e := parse(parts[1])
	return lp, rp, e
}

func cloudTunnelCommand(ctx context.Context, cloudGRPC, deviceName, brokerURL string, localPort, remotePort uint32) error {
	auth, err := pickAuthEntry(cloudGRPC)
	if err != nil {
		return err
	}

	cliLogln("Fetching device list from cloud...")
	asset, err := pickCloudDevice(ctx, auth, deviceName)
	if err != nil {
		return err
	}

	brokerConn, err := dialCloudBroker(auth, brokerURL)
	if err != nil {
		return err
	}
	defer brokerConn.Close()

	listenAddr := fmt.Sprintf("127.0.0.1:%d", localPort)
	ln, err := net.Listen("tcp", listenAddr)
	if err != nil {
		return fmt.Errorf("listening on %s: %w", listenAddr, err)
	}
	defer ln.Close()

	cliSuccess("Forwarding %s → %s (cloud) → localhost:%d", listenAddr, asset.GetName(), remotePort)
	cliLogln("Press Ctrl+C to stop.")

	go func() {
		<-ctx.Done()
		ln.Close()
	}()

	for {
		tcpConn, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return fmt.Errorf("accepting connection: %w", err)
		}
		go serveTunnelConn(ctx, tcpConn, brokerConn, auth, asset.GetId(), remotePort)
	}
}

func serveTunnelConn(ctx context.Context, tcpConn net.Conn, brokerConn *grpc.ClientConn, auth *config.AuthConfig, assetID int32, remotePort uint32) {
	defer tcpConn.Close()

	tunnelConn, err := openBrokerTunnel(ctx, brokerConn, auth, assetID, remotePort)
	if err != nil {
		return
	}
	defer tunnelConn.Close()

	done := make(chan struct{}, 2)
	relay := func(dst io.Writer, src io.Reader) {
		defer func() { done <- struct{}{} }()
		_, _ = io.Copy(dst, src)
	}
	go relay(tunnelConn, tcpConn)
	go relay(tcpConn, tunnelConn)
	<-done
}
```

- [ ] **Step 2: Build to verify**

```bash
cd go && go build ./internal/cli/commands/...
```
Expected: no errors.

- [ ] **Step 3: Commit**

```bash
git add go/internal/cli/commands/cloud_forward.go
git commit -m "feat(cloud): add 'wendy cloud tunnel' TCP port-forward command"
```

---

### Task 5: Tests for `parseTunnelArg`

**Files:**
- Create: `go/internal/cli/commands/cloud_tunnel_test.go`

- [ ] **Step 1: Write tests**

```go
package commands

import (
	"testing"
)

func TestParseTunnelArg(t *testing.T) {
	tests := []struct {
		arg        string
		wantLocal  uint32
		wantRemote uint32
		wantErr    bool
	}{
		{"8080", 8080, 8080, false},
		{"3000:8080", 3000, 8080, false},
		{"0", 0, 0, true},
		{"99999", 0, 0, true},
		{"abc", 0, 0, true},
		{"8080:abc", 0, 0, true},
		{"65535", 65535, 65535, false},
		{"1:65535", 1, 65535, false},
	}

	for _, tt := range tests {
		t.Run(tt.arg, func(t *testing.T) {
			local, remote, err := parseTunnelArg(tt.arg)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("parseTunnelArg(%q) expected error, got none", tt.arg)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseTunnelArg(%q) unexpected error: %v", tt.arg, err)
			}
			if local != tt.wantLocal || remote != tt.wantRemote {
				t.Errorf("parseTunnelArg(%q) = (%d, %d), want (%d, %d)", tt.arg, local, remote, tt.wantLocal, tt.wantRemote)
			}
		})
	}
}
```

- [ ] **Step 2: Run tests**

```bash
cd go && go test ./internal/cli/commands/... -run TestParseTunnelArg -v
```
Expected: all PASS.

- [ ] **Step 3: Commit**

```bash
git add go/internal/cli/commands/cloud_tunnel_test.go
git commit -m "test(cloud): add unit tests for parseTunnelArg"
```

---

### Task 6: Final build and push

- [ ] **Step 1: Full build**

```bash
cd go && go build ./...
```
Expected: no errors (macOS `-lobjc` linker warning is fine).

- [ ] **Step 2: Run existing tests**

```bash
cd go && go test ./internal/cli/commands/... -count=1 2>&1 | tail -20
```
Expected: all pre-existing tests pass.

- [ ] **Step 3: Push to `jo/cli-colors` branch**

```bash
git push origin HEAD:jo/cli-colors
```

---

## Notes

- `boolPtr` helper is defined in `cloud_tunnel.go`. Verify no collision: `grep -r 'func boolPtr' go/internal/cli/commands/` should show only one result.
- `warnAppConfigFile` is called inside `ensureAppConfig`, so no separate call needed in `cloudRunCommand`.
- `pickCloudDevice` sends `PickerDoneMsg{}` immediately since all items are loaded synchronously (no streaming discovery).
- The `net.Pipe()` relay in `openBrokerTunnel` does not implement TCP half-close on the `local` side (`net.Pipe` doesn't support `CloseWrite`). Connections will still close cleanly; half-close is best-effort.
- `defaultAgentPort` is an untyped constant (`50051`) in `helpers.go`. It converts implicitly to `uint32` — no explicit cast needed.
