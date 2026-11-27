package commands

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"regexp"
	"syscall"
	"time"

	"github.com/coder/websocket"
	"github.com/spf13/cobra"

	"github.com/wendylabsinc/wendy/go/internal/cli/grpcclient"
	"github.com/wendylabsinc/wendy/go/internal/shared/appconfig"
	"github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
)

// foxgloveBridgePort is the port foxglove_bridge listens on inside the app.
const foxgloveBridgePort = 8765

// foxgloveBridgeAddress keeps the unauthenticated Foxglove WebSocket reachable
// only through the Wendy tunnel. ROS 2 discovery still uses the device's host
// network independently of this listener address.
const foxgloveBridgeAddress = "127.0.0.1"

// foxgloveAppID is the appId of the generated foxglove_bridge app; used both in
// the generated wendy.json and to remove a prior instance before redeploy.
const foxgloveAppID = "sh.wendy.foxglovebridge"

// Linux interface names are limited to IFNAMSIZ-1 (15) bytes. Keeping the
// accepted character set narrow also makes it safe to embed the name in the
// generated CycloneDDS XML and shell command.
var foxgloveInterfacePattern = regexp.MustCompile(`^[A-Za-z0-9_.:-]{1,15}$`)

// newFoxgloveCmd builds the `wendy device foxglove` command group.
func newFoxgloveCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "foxglove",
		Short: "Bridge the device's ROS 2 graph to Foxglove Studio",
	}
	cmd.AddCommand(newFoxgloveServeCmd())
	return cmd
}

func newFoxgloveServeCmd() *cobra.Command {
	var (
		port    int
		domain  int
		app     string
		rmw     string
		distro  string
		iface   string
		builder string
	)

	cmd := &cobra.Command{
		Use:   "serve",
		Short: "Deploy foxglove_bridge to the device and open a tunnel for Foxglove Studio",
		Long: `Generates a foxglove_bridge app and deploys it to the selected device.
Host networking lets it join the device's ROS 2 graph, including a robot's native
host ROS 2. It then forwards the bridge's WebSocket port to your machine.

Connect Foxglove Studio to the printed ws:// URL. For a robot whose ROS 2 uses a
non-default domain or RMW (e.g. a Unitree Go2 on CycloneDDS), pass --domain and
--rmw so the bridge matches it. The bridge exposes every topic, including hidden
ROS topics, in the selected domain. Its device-side WebSocket listens only on
loopback and is forwarded through the selected Wendy agent connection, over
LAN or cloud. Deployment and forwarding use the same device.

Domain selection matters: a robot's native ROS 2 is usually on domain 0, but a
Wendy-deployed ROS 2 app with no explicit "domainId" gets a stable domain derived
from its appId — essentially never 0. To bridge one of those, pass --app <appId>
and the matching domain is computed for you. With neither flag the bridge sits on
domain 0 and will show an empty graph if that is not where your nodes are.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
			defer stop()
			resolvedDomain, err := resolveFoxgloveDomain(domain, app, cmd.Flags().Changed("domain"))
			if err != nil {
				return err
			}
			return foxgloveServe(ctx, foxgloveServeOpts{
				localPort: port,
				domain:    resolvedDomain,
				rmw:       rmw,
				distro:    distro,
				device:    deviceFlag,
				iface:     iface,
				builder:   builder,
			})
		},
	}

	cmd.Flags().IntVar(&port, "port", foxgloveBridgePort, "Local port to forward foxglove_bridge to")
	cmd.Flags().IntVar(&domain, "domain", 0, "ROS_DOMAIN_ID the device's ROS 2 uses")
	cmd.Flags().StringVar(&app, "app", "", "appId of a Wendy-deployed ROS 2 app to bridge; derives its ROS_DOMAIN_ID")
	cmd.Flags().StringVar(&rmw, "rmw", "rmw_cyclonedds_cpp", "RMW implementation the device's ROS 2 uses")
	cmd.Flags().StringVar(&distro, "distro", "humble", "ROS 2 distro to build foxglove_bridge from")
	cmd.Flags().StringVar(&iface, "interface", "", "CycloneDDS network interface to use (for multi-interface devices)")
	cmd.Flags().StringVar(&builder, "builder", "", "Image builder to force: docker, apple-container, or buildkit")

	return cmd
}

type foxgloveServeOpts struct {
	localPort int
	domain    int
	rmw       string
	distro    string
	device    string // global --device; "" = WENDY_DEVICE or the picker
	iface     string // optional CycloneDDS network interface
	builder   string // optional image builder; "" = normal wendy run selection
}

// resolveFoxgloveDomain decides which ROS_DOMAIN_ID the bridge should join.
//
// `--domain` defaulted to 0, which is right for a robot's native ROS 2 but wrong
// for anything Wendy deployed: an app without an explicit domainId gets
// ROS2AutoDomainID(appID), a stable hash that is essentially never 0. So the
// no-flag invocation bridged an empty graph with nothing explaining why. Passing
// `--app <appId>` now derives the same domain the agent injected — it is a pure
// function of the appId, so no device round-trip is needed — and the bare default
// says out loud what it is doing.
func resolveFoxgloveDomain(domain int, appID string, domainSet bool) (int, error) {
	switch {
	case domainSet && appID != "":
		derived := appconfig.ROS2AutoDomainID(appID)
		if derived != domain {
			return 0, fmt.Errorf("--domain %d conflicts with --app %q, whose ROS_DOMAIN_ID is %d; pass only one",
				domain, appID, derived)
		}
		return domain, nil
	case appID != "":
		derived := appconfig.ROS2AutoDomainID(appID)
		cliLogln("Using ROS_DOMAIN_ID=%d, derived from appId %q.", derived, appID)
		return derived, nil
	case domainSet:
		return domain, nil
	default:
		cliLogln("No --domain or --app given: using ROS_DOMAIN_ID=0 (a robot's native ROS 2). " +
			"Wendy-deployed ROS 2 apps get a domain derived from their appId, so if you " +
			"meant to bridge one of those, re-run with --app <appId>.")
		return 0, nil
	}
}

// Resolve once so deployment and forwarding share the same device and transport.
var resolveFoxgloveTarget = func(ctx context.Context, device string) (*SelectedDevice, error) {
	return resolveWithCloudFallback(ctx, device, SelectDevice(device), ExcludeProviders("local", "docker"))
}

var deployFoxgloveApp = func(ctx context.Context, conn *grpcclient.AgentConnection, dir string, cfg *appconfig.AppConfig, builder string) error {
	return runWithAgent(ctx, conn, dir, cfg, runOptions{detach: true, builder: builder})
}

// foxgloveServe deploys the bridge and carries its loopback WebSocket through
// container exec on the same agent connection. No second device lookup is needed.
func foxgloveServe(ctx context.Context, opts foxgloveServeOpts) error {
	if opts.localPort < 1 || opts.localPort > 65535 {
		return fmt.Errorf("invalid --port %d: must be between 1 and 65535", opts.localPort)
	}
	if _, err := normalizeImageBuilder(opts.builder); err != nil {
		return err
	}
	dir, err := os.MkdirTemp("", "wendy-foxglove-*")
	if err != nil {
		return fmt.Errorf("creating temp app dir: %w", err)
	}
	defer os.RemoveAll(dir)
	if err := writeFoxgloveApp(dir, opts); err != nil {
		return err
	}
	cfg, err := appconfig.LoadFromFile(filepath.Join(dir, "wendy.json"))
	if err != nil {
		return fmt.Errorf("loading foxglove_bridge config: %w", err)
	}

	if err := cfg.Validate(); err != nil {
		return fmt.Errorf("validating foxglove_bridge config: %w", err)
	}

	// Reserve the local port before replacing a running bridge on the device.
	listener, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", fmt.Sprint(opts.localPort)))
	if err != nil {
		return fmt.Errorf("listening for Foxglove Studio: %w", err)
	}
	defer listener.Close()
	target, err := resolveFoxgloveTarget(ctx, opts.device)
	if err != nil {
		return err
	}
	defer target.Close()
	if target.Agent == nil {
		return fmt.Errorf("Foxglove requires a Wendy agent connection")
	}

	// Older CLIs modeled the bridge as a named service. Its container uses a
	// different ID but the same host port, so it cannot run alongside the new
	// single-container app during the one-time migration.
	if err := removeLegacyFoxgloveApp(ctx, target.Agent.ContainerService); err != nil {
		return err
	}

	// Use the normal single-container redeploy path so current bridge versions
	// stay available while the replacement image builds and uploads. The agent
	// swaps the container only once the new deployment is ready to create.
	cliLogln("Deploying foxglove_bridge to the device...")
	if err := deployFoxgloveApp(ctx, target.Agent, dir, cfg, opts.builder); err != nil {
		return fmt.Errorf("deploying foxglove_bridge: %w", err)
	}
	if err := waitForFoxgloveBridge(ctx, target.Agent.ContainerService); err != nil {
		if ctx.Err() != nil {
			return nil
		}
		return err
	}
	cliSuccess("foxglove_bridge ready. Connect Foxglove Studio to ws://localhost:%d", opts.localPort)
	cliLogln("Press Ctrl+C to stop forwarding. The bridge app will keep running.")
	return serveFoxgloveForward(ctx, listener, target.Agent.ContainerService)
}

// removeLegacyFoxgloveApp removes only the named-service container generated by
// older CLIs. Current single-container bridge deployments have no service entry
// and remain running until the normal redeploy replaces them.
func removeLegacyFoxgloveApp(ctx context.Context, client agentpb.WendyContainerServiceClient) error {
	stream, err := client.ListContainers(ctx, &agentpb.ListContainersRequest{})
	if err != nil {
		return fmt.Errorf("checking for a legacy foxglove_bridge deployment: %w", err)
	}
	for {
		resp, err := stream.Recv()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return fmt.Errorf("checking for a legacy foxglove_bridge deployment: %w", err)
		}
		container := resp.GetContainer()
		if container.GetAppName() != foxgloveAppID {
			continue
		}
		for _, service := range container.GetServices() {
			if service.GetName() != "foxglove" {
				continue
			}
			cliNotice("Replacing a foxglove_bridge deployment from an older CLI; the old bridge must stop before its single-container replacement can bind port %d.", foxgloveBridgePort)
			if _, err := client.DeleteContainer(ctx, &agentpb.DeleteContainerRequest{
				AppName:     multiServiceContainerName(foxgloveAppID, "foxglove"),
				DeleteImage: true,
			}); err != nil {
				return fmt.Errorf("removing legacy foxglove_bridge deployment: %w", err)
			}
			return nil
		}
		return nil
	}
}

// Confirm the WebSocket handshake through the same path Studio will use. A
// detached deployment can finish before the bridge starts listening.
func waitForFoxgloveBridge(ctx context.Context, client agentpb.WendyContainerServiceClient) error {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	var lastErr error
	for {
		probeCtx, stop := context.WithTimeout(ctx, 3*time.Second)
		transport := &http.Transport{DialContext: func(context.Context, string, string) (net.Conn, error) {
			return openFoxgloveConnection(probeCtx, client)
		}}
		ws, _, err := websocket.Dial(probeCtx, "ws://127.0.0.1:8765", &websocket.DialOptions{
			HTTPClient: &http.Client{Transport: transport},
			// Current Rust-SDK bridges select foxglove.sdk.v1, while older
			// foxglove_bridge releases select foxglove.websocket.v1.
			Subprotocols: []string{"foxglove.sdk.v1", "foxglove.websocket.v1"},
		})
		if err == nil {
			_ = ws.CloseNow()
		}
		transport.CloseIdleConnections()
		stop()
		if err == nil {
			return nil
		}
		lastErr = err
		select {
		case <-ctx.Done():
			return fmt.Errorf("waiting for foxglove_bridge WebSocket: %w", lastErr)
		case <-time.After(250 * time.Millisecond):
		}
	}
}

// writeFoxgloveApp writes the Dockerfile + wendy.json for a foxglove_bridge app
// into dir, templated for the requested distro/domain/rmw.
func writeFoxgloveApp(dir string, opts foxgloveServeOpts) error {
	if opts.iface != "" && !foxgloveInterfacePattern.MatchString(opts.iface) {
		return fmt.Errorf("invalid network interface %q: use 1-15 letters, digits, '.', '_', ':', or '-'", opts.iface)
	}
	// distro lands in three interpolated positions below: two `apt-get install
	// ros-<distro>-…` lines and the `source /opt/ros/<distro>/setup.bash` inside a
	// `bash -lc` CMD. %q protects the Dockerfile's JSON tokenisation but not the
	// shell semantics *inside* the string, so this is validated for the same reason
	// `iface` is — the comment above foxgloveInterfacePattern already states the
	// invariant, distro just wasn't held to it.
	if !appconfig.ROS2DistroPattern.MatchString(opts.distro) {
		return fmt.Errorf("invalid ROS 2 distro %q: use lowercase letters and digits, "+
			"starting with a letter (e.g. %q)", opts.distro, appconfig.ROS2DefaultDistro)
	}
	if opts.domain < appconfig.ROS2DomainIDMin || opts.domain > appconfig.ROS2DomainIDMax {
		return fmt.Errorf("invalid --domain %d: must be between %d and %d",
			opts.domain, appconfig.ROS2DomainIDMin, appconfig.ROS2DomainIDMax)
	}
	if opts.rmw != "" && !appconfig.IsValidRMWImplementation(opts.rmw) {
		return fmt.Errorf("invalid --rmw %q: expected one of rmw_cyclonedds_cpp, "+
			"rmw_fastrtps_cpp, rmw_connextdds, rmw_gurumdds_cpp", opts.rmw)
	}

	// Keep the explicit discovery overrides in the launch command as a
	// compatibility path for devices whose installed agent predates
	// frameworks.ros2.scope (or discoveryScope). New agents also honor the
	// manifest field.
	launchCommand := fmt.Sprintf("source /opt/ros/%s/setup.bash && export ROS_LOCALHOST_ONLY=0 ROS_AUTOMATIC_DISCOVERY_RANGE=SUBNET", opts.distro)
	if opts.iface != "" {
		// This override runs inside the generated image, after the agent injects
		// its network-scope environment. Keep loopback and localhost discovery
		// while pinning the outward-facing side to the requested interface.
		// SECURITY: opts.iface matched foxgloveInterfacePattern above, so it
		// cannot alter either the XML attribute or single-quoted shell value.
		cycloneURI := fmt.Sprintf(`<CycloneDDS><Domain><General><Interfaces><NetworkInterface name="lo" multicast="true"/><NetworkInterface name="%s"/></Interfaces></General><Discovery><ParticipantIndex>auto</ParticipantIndex><Peers><Peer address="127.0.0.1"/></Peers></Discovery><SharedMemory><Enable>false</Enable></SharedMemory></Domain></CycloneDDS>`, opts.iface)
		launchCommand += " && export CYCLONEDDS_URI='" + cycloneURI + "'"
	}
	launchCommand += fmt.Sprintf(" && exec ros2 launch foxglove_bridge foxglove_bridge_launch.xml port:=%d address:=%s include_hidden:=true", foxgloveBridgePort, foxgloveBridgeAddress)

	dockerfile := fmt.Sprintf(`# Auto-generated by 'wendy device foxglove serve'.
FROM ros:%s
RUN apt-get update && apt-get install -y --no-install-recommends \
      socat \
      ros-%s-foxglove-bridge \
      ros-%s-rmw-cyclonedds-cpp \
    && rm -rf /var/lib/apt/lists/*
CMD ["bash","-lc",%q]
`, opts.distro, opts.distro, opts.distro, launchCommand)

	wendyJSON := fmt.Sprintf(`{
  "appId": %[4]q,
  "platform": "linux",
  "version": "1.0.0",
  "frameworks": {
    "ros2": { "domainId": %[1]d, "rmw": %[2]q, "distro": %[3]q, "scope": "network" }
  },
  "entitlements": [
    { "type": "network", "mode": "host" }
  ]
}
`, opts.domain, opts.rmw, opts.distro, foxgloveAppID)

	if err := os.WriteFile(filepath.Join(dir, "Dockerfile"), []byte(dockerfile), 0o644); err != nil {
		return fmt.Errorf("writing Dockerfile: %w", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "wendy.json"), []byte(wendyJSON), 0o644); err != nil {
		return fmt.Errorf("writing wendy.json: %w", err)
	}
	return nil
}
