package commands

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/spf13/cobra"
	"github.com/wendylabsinc/wendy/go/internal/cli/browserauth"
	"github.com/wendylabsinc/wendy/go/internal/cli/grpcclient"
	"github.com/wendylabsinc/wendy/go/internal/cli/sessionbroker"
	"github.com/wendylabsinc/wendy/go/internal/shared/certs"
	"github.com/wendylabsinc/wendy/go/internal/shared/litetopology"
	"github.com/wendylabsinc/wendy/go/internal/shared/models"
	pb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protojson"
)

var resolveLocalMeshTopologyTarget = resolveTargetInner
var resolveLocalMeshTarget = resolveTarget
var errNoLocalMeshSettings = errors.New("specify a Wendy Net setting; omitted flags preserve saved settings; run without flags in an interactive terminal to choose settings")
var readLocalOperatorTopology = sessionbroker.ReadOperatorTopology
var observedLocalMeshTopologyIdentity = func(conn *grpcclient.AgentConnection) (certs.WendyIdentity, bool) {
	return conn.ObservedServerIdentity()
}

const localMeshTopologyTimeout = 15 * time.Second

func newNetCmd() *cobra.Command {
	cmd := newDeviceLocalMeshCmd()
	cmd.Use = "net"
	cmd.Aliases = nil
	return cmd
}
func newLegacyDeviceLocalMeshCmd() *cobra.Command {
	cmd := newDeviceLocalMeshCmd()
	cmd.Hidden = true
	return cmd
}

// Register this constructor under the device command's manage group.
func newDeviceLocalMeshCmd() *cobra.Command {
	root := &cobra.Command{Use: "wendy-net", Aliases: []string{"local-mesh", "nan-mesh"}, Short: "Manage Wendy Net connections and Internet sharing",
		Long: "Use connect and disconnect to join or leave Wendy Net from this computer. Use configure and status with --device to manage a device discovered by wendy discover. Omitted device settings are preserved; reading status does not enable mesh participation."}
	configure := &cobra.Command{Use: "configure", Short: "Update selected Wendy Net settings", Args: cobra.NoArgs}
	configure.Flags().Bool("participate", false, "Join and relay Wendy Net (false disconnects the device; saved connection settings are retained)")
	configure.Flags().Bool("roam", false, "Use Wendy Net Internet when an independent uplink is unavailable")
	configure.Flags().Bool("share-uplink", false, "Share this device's own Internet connection with Wendy Net")
	configure.Flags().Bool("nan", false, "Connect over Wi-Fi Aware (NAN)")
	configure.Flags().Bool("ble", false, "Connect over Bluetooth Low Energy")
	configure.Flags().String("ble-mode", "", "BLE radio profile: off, normal, coded-s2 or coded-s8 (Lite profile changes restart the device)")
	configure.Flags().Bool("ethernet", false, "Discover local net peers over Ethernet")
	configure.Flags().Bool("infrastructure-wifi", false, "Discover local net peers over infrastructure Wi-Fi")
	configure.Flags().Uint32("max-links", 0, "Lite: limit simultaneous mesh connections")
	configure.Flags().Uint32("ble-links", 0, "Lite: limit simultaneous Bluetooth mesh connections")
	configure.Flags().Uint32("wifi-links", 0, "Lite: limit simultaneous infrastructure Wi-Fi mesh connections")
	configure.Flags().Uint32("nan-links", 0, "Lite: limit simultaneous Wi-Fi Aware mesh connections")
	configure.RunE = func(cmd *cobra.Command, _ []string) error {
		changed := false
		for _, name := range append([]string{"participate", "roam", "share-uplink", "nan", "ble", "ble-mode", "ethernet", "infrastructure-wifi"}, liteNetLimitFlags...) {
			changed = changed || cmd.Flags().Changed(name)
		}
		interactive := !changed && !jsonOutput && isInteractiveTerminal()
		if !changed && !interactive {
			return errNoLocalMeshSettings
		}
		if cmd.Flags().Changed("ble-mode") {
			if cmd.Flags().Changed("ble") {
				return fmt.Errorf("choose --ble-mode or --ble, not both")
			}
			mode, _ := cmd.Flags().GetString("ble-mode")
			switch mode {
			case "off", "normal", "coded-s2", "coded-s8":
			default:
				return fmt.Errorf("--ble-mode must be off, normal, coded-s2 or coded-s8")
			}
		}
		target, err := resolveLocalMeshTarget(cmd.Context(), SuppressUpdateCheck(), SuppressPickerEnroll())
		if err != nil {
			return err
		}
		defer target.Close()
		if target.External != nil {
			return configureLiteNet(cmd, target)
		}
		for _, name := range liteNetLimitFlags {
			if cmd.Flags().Changed(name) {
				return fmt.Errorf("--%s applies to Wendy Lite peers", name)
			}
		}
		if cmd.Flags().Changed("ble-mode") {
			if cmd.Flags().Changed("ble") {
				return fmt.Errorf("choose --ble-mode or --ble, not both")
			}
			mode, _ := cmd.Flags().GetString("ble-mode")
			if mode != "off" {
				if target.Agent == nil || target.Agent.Conn == nil {
					return fmt.Errorf("BLE settings require an agent connection")
				}
				if _, err := pb.NewWendyBluetoothServiceClient(target.Agent.Conn).ConfigureBluetoothRadio(cmd.Context(), &pb.ConfigureBluetoothRadioRequest{MeshMode: mode}); err != nil {
					return err
				}
			}
			if err := cmd.Flags().Set("ble", fmt.Sprint(mode != "off")); err != nil {
				return err
			}
		}
		req, err := localMeshRequest(cmd)
		if err != nil && !interactive {
			return err
		}
		if target.Agent == nil || target.Agent.Conn == nil {
			return fmt.Errorf("Wendy Net settings require a WendyOS agent")
		}
		client := pb.NewWendyLocalMeshAdminServiceClient(target.Agent.Conn)
		if interactive {
			req, err = interactiveLocalMeshRequest(cmd.Context(), client)
			if err != nil {
				return err
			}
			if req == nil {
				cmd.Println("Wendy Net settings unchanged.")
				return nil
			}
		}
		response, err := client.ConfigureLocalMesh(cmd.Context(), req)
		if err != nil {
			return err
		}
		return printLocalMeshStatus(cmd, response)
	}
	inspect := &cobra.Command{Use: "status", Short: "Show saved Wendy Net settings and runtime state", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		target, err := resolveTarget(cmd.Context(), SuppressUpdateCheck())
		if err != nil {
			return err
		}
		defer target.Close()
		if target.External != nil {
			return inspectLiteNet(cmd, target)
		}
		if target.Agent == nil || target.Agent.Conn == nil {
			return fmt.Errorf("Wendy Net status requires a WendyOS agent")
		}
		client := pb.NewWendyLocalMeshAdminServiceClient(target.Agent.Conn)
		response, err := client.GetLocalMeshStatus(cmd.Context(), &pb.GetLocalMeshStatusRequest{})
		if err != nil {
			return err
		}
		return printLocalMeshStatus(cmd, response)
	}}
	root.AddCommand(configure, inspect, newDeviceLocalMeshTopologyCmd(), newDeviceOperatorMeshConnectCmd(), newDeviceOperatorMeshDisconnectCmd(), newDeviceMeshForwardCmd(), newDeviceMeshServicesCmd())
	return root
}

func newDeviceLocalMeshTopologyCmd() *cobra.Command {
	var observation, localCLI bool
	cmd := &cobra.Command{
		Use:   "topology",
		Short: "Show nearby mesh connections and known routes",
		Long:  "Show the selected WendyOS or Wendy Lite device's view of the mesh, or this computer's running Wendy Net router with --local-cli. Each router has its own view; a listed route does not guarantee the destination is currently reachable. Use --json for complete details.",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if observation && !jsonOutput {
				return fmt.Errorf("--observation requires --json")
			}
			var topology *pb.LocalMeshTopology
			var platform string
			var err error
			if localCLI {
				if cmd.Flags().Changed("device") || cmd.InheritedFlags().Changed("device") {
					return fmt.Errorf("choose --local-cli for this computer or --device for a device, not both")
				}
				topology, err = readCLIReporter(cmd.Context())
				platform = "wendy-cli"
			} else {
				topology, platform, err = readLocalMeshReporter(cmd.Context())
			}
			if observation {
				return printLocalMeshObservation(cmd, topology, err, platform)
			}
			if err != nil {
				return err
			}
			return printLocalMeshTopology(cmd, topology)
		},
	}
	cmd.Flags().BoolVar(&localCLI, "local-cli", false, "Read this computer's running Wendy Net router without starting or enabling it")
	cmd.Flags().BoolVar(&observation, "observation", false, "With --json, include whether topology is supported, available and authorized")
	return cmd
}

// ReadOperatorTopology authenticates the selected private broker and validates
// the actual operator principal/installation binding. There is no target TLS.
func readCLIReporter(parent context.Context) (*pb.LocalMeshTopology, error) {
	ctx, cancel := context.WithTimeout(parent, 3*time.Second)
	defer cancel()
	snapshot, err := readLocalOperatorTopology(ctx)
	if errors.Is(err, sessionbroker.ErrUnavailable) {
		return nil, status.Error(codes.Unavailable, err.Error())
	}
	if err != nil {
		return nil, err
	}
	if snapshot == nil {
		return nil, status.Error(codes.Unavailable, "the existing CLI router returned no topology")
	}
	if snapshot.Status == nil {
		snapshot.Status = &pb.LocalMeshStatus{RuntimeAvailable: snapshot.RuntimeAvailable, Configured: &pb.LocalMeshConfiguration{Participate: snapshot.RuntimeAvailable}, AuthenticatedPeers: uint32(len(snapshot.Peers)), State: "waiting-for-peers", Detail: "Wendy Net router on this computer, using the selected sign-in profile."}
		if snapshot.RuntimeAvailable && len(snapshot.Peers) > 0 {
			snapshot.Status.State = "mesh-active"
		}
	}
	return snapshot, nil
}

func readLocalMeshReporter(parent context.Context) (*pb.LocalMeshTopology, string, error) {
	ctx, cancel := context.WithTimeout(parent, localMeshTopologyTimeout)
	defer cancel()
	target, err := resolveLocalMeshTopologyTarget(ctx, ReadOnlyMonitoring())
	if err != nil {
		return nil, "", err
	}
	if target == nil {
		return nil, "", fmt.Errorf("no device selected for mesh topology; discover a device and select it with --device")
	}
	defer target.Close()
	if target.External != nil {
		provider, ok := target.Provider.(interface {
			NetTopology(context.Context, models.ExternalDevice) (*pb.LocalMeshTopology, error)
		})
		if !ok {
			return nil, "", status.Error(codes.Unimplemented, "mesh topology is not supported for the selected device or connection")
		}
		liteCtx, stopLite := context.WithTimeout(parent, litetopology.SetupBudget+litetopology.AggregateBudget)
		defer stopLite()
		snapshot, err := provider.NetTopology(liteCtx, *target.External)
		return snapshot, "wendy-lite", err
	}
	conn := target.Agent
	if conn == nil {
		return nil, "", fmt.Errorf("mesh topology requires a WendyOS agent or a supported Wendy Lite device")
	}
	if conn.Conn == nil {
		return nil, "", fmt.Errorf("mesh topology requires a WendyOS agent or a supported Wendy Lite device")
	}
	client := pb.NewWendyLocalMeshAdminServiceClient(conn.Conn)
	topology, err := client.GetLocalMeshTopology(ctx, &pb.GetLocalMeshTopologyRequest{})
	if err != nil {
		return nil, "", fmt.Errorf("reading the device's mesh topology: %w", err)
	}
	peer, verified := observedLocalMeshTopologyIdentity(conn)
	if !verified {
		return nil, "", status.Error(codes.Unauthenticated, "the identity of the device returning mesh topology could not be verified")
	}
	if err := browserauth.ValidateTopologyReporter(topology, peer); err != nil {
		return nil, "", err
	}
	return topology, "wendyos", nil
}

func printLocalMeshObservation(cmd *cobra.Command, topology *pb.LocalMeshTopology, readErr error, platform string) error {
	var observation browserauth.TopologyObservation
	if readErr != nil {
		observation = browserauth.TopologyFailure(readErr)
	} else {
		data, err := (protojson.MarshalOptions{EmitUnpopulated: true}).Marshal(topology)
		if err != nil {
			observation = browserauth.TopologyFailure(err)
		} else {
			observation = browserauth.TopologyObservation{Status: "ok", Topology: data, ObservedAt: time.Now().UTC(), Platform: platform}
		}
	}
	if platform != "" {
		observation.Platform = platform
	}
	encoder := json.NewEncoder(cmd.OutOrStdout())
	encoder.SetIndent("", "  ")
	return encoder.Encode(observation)
}

func printLocalMeshTopology(cmd *cobra.Command, topology *pb.LocalMeshTopology) error {
	if topology == nil {
		return fmt.Errorf("agent returned no local topology snapshot")
	}
	if jsonOutput {
		data, err := (protojson.MarshalOptions{Indent: "  ", EmitUnpopulated: true}).Marshal(topology)
		if err != nil {
			return err
		}
		_, err = fmt.Fprintln(cmd.OutOrStdout(), string(data))
		return err
	}
	captured := "unknown"
	if topology.GetCapturedAtUnixMs() > 0 {
		captured = time.UnixMilli(topology.GetCapturedAtUnixMs()).UTC().Format(time.RFC3339Nano)
	}
	_, err := fmt.Fprintf(cmd.OutOrStdout(), "Reporter: %s\nNet address: %s\nCaptured: %s\nRuntime available: %t\nState: %s\n%s\nObserved peers: %d\nRoutes: %d\nRoute announcements: %d\nSnapshot truncated: %t\nUse --json for peer, route and proof details.\n",
		topology.GetSelfPrincipal(), topology.GetSelfMeshAddress(), captured, topology.GetRuntimeAvailable(),
		localNetStateLabel(topology.GetStatus().GetState()), topology.GetStatus().GetDetail(), len(topology.GetPeers()),
		len(topology.GetRoutes()), len(topology.GetAnnouncements()), topology.GetTruncated())
	return err
}

func localMeshRequest(cmd *cobra.Command) (*pb.ConfigureLocalMeshRequest, error) {
	req := &pb.ConfigureLocalMeshRequest{}
	for _, field := range []struct {
		name string
		dest **bool
	}{{"participate", &req.Participate}, {"roam", &req.Roam}, {"share-uplink", &req.ShareUplink}, {"nan", &req.Nan}, {"ble", &req.Ble}, {"ethernet", &req.Ethernet}, {"infrastructure-wifi", &req.InfrastructureWifi}} {
		if cmd.Flags().Changed(field.name) {
			value, err := cmd.Flags().GetBool(field.name)
			if err != nil {
				return nil, err
			}
			*field.dest = &value
		}
	}
	if req.Participate == nil && req.Roam == nil && req.ShareUplink == nil && req.Nan == nil && req.Ble == nil && req.Ethernet == nil && req.InfrastructureWifi == nil {
		return nil, errNoLocalMeshSettings
	}
	return req, nil
}

func localNetStateLabel(state string) string {
	if state == "mesh-active" {
		return "Local peers active"
	}
	return state
}

func printLocalMeshStatus(cmd *cobra.Command, status *pb.LocalMeshStatus) error {
	if jsonOutput {
		data, err := (protojson.MarshalOptions{Indent: "  ", EmitUnpopulated: true}).Marshal(status)
		if err != nil {
			return err
		}
		_, err = fmt.Fprintln(cmd.OutOrStdout(), string(data))
		return err
	}
	configured := status.GetConfigured()
	cmd.Printf("Participate: %t\nRoam: %t\nShare uplink: %t\nNAN: %t\nBLE: %t\nEthernet: %t\nInfrastructure Wi-Fi: %t\nTCP listen: %s\nConfigured TCP peers: %d\nAuthenticated peers: %d\nGateway principal: %s\nState: %s\n%s\n",
		configured.GetParticipate(), configured.GetRoam(), configured.GetShareUplink(), configured.GetNan(), configured.GetBle(), configured.GetEthernet(), configured.GetInfrastructureWifi(), configured.GetTcpListen(), configured.GetConfiguredTcpPeers(),
		status.GetAuthenticatedPeers(), status.GetGatewayPrincipal(), localNetStateLabel(status.GetState()), status.GetDetail())
	return nil
}
