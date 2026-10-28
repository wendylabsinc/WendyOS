package commands

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/spf13/cobra"
	"github.com/wendylabsinc/wendy/go/internal/cli/browserauth"
	"github.com/wendylabsinc/wendy/go/internal/cli/grpcclient"
	"github.com/wendylabsinc/wendy/go/internal/shared/certs"
	pb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protojson"
)

var connectLocalMeshTopology = connectToAgent
var observedLocalMeshTopologyIdentity = func(conn *grpcclient.AgentConnection) (certs.WendyIdentity, bool) {
	return conn.ObservedServerIdentity()
}

const localMeshTopologyTimeout = 15 * time.Second

// Register this constructor under the device command's manage group.
func newDeviceLocalMeshCmd() *cobra.Command {
	root := &cobra.Command{Use: "wendy-net", Aliases: []string{"local-mesh", "nan-mesh"}, Short: "Configure Wendy Net carriers and Internet sharing"}
	configure := &cobra.Command{Use: "configure", Short: "Update selected Wendy Net settings", Args: cobra.NoArgs}
	configure.Flags().Bool("participate", false, "Join and relay Wendy Net (false stops all carriers; saved carrier choices are retained)")
	configure.Flags().Bool("roam", false, "Use Wendy Net Internet when an independent uplink is unavailable")
	configure.Flags().Bool("share-uplink", false, "Offer an independent uplink to the net")
	configure.Flags().Bool("nan", false, "Enable Wi-Fi Aware NAN as a net carrier")
	configure.Flags().Bool("ble", false, "Enable BLE L2CAP as a net carrier")
	configure.Flags().Bool("ethernet", false, "Discover local net peers over Ethernet")
	configure.Flags().Bool("infrastructure-wifi", false, "Discover local net peers over infrastructure Wi-Fi")
	configure.Flags().Uint32("max-links", 0, "Lite: limit simultaneous mesh carriers")
	configure.Flags().Uint32("ble-links", 0, "Lite: limit simultaneous BLE mesh carriers")
	configure.Flags().Uint32("wifi-links", 0, "Lite: limit simultaneous infrastructure Wi-Fi mesh carriers")
	configure.Flags().Uint32("nan-links", 0, "Lite: limit simultaneous NAN mesh carriers")
	configure.RunE = func(cmd *cobra.Command, _ []string) error {
		changed := false
		for _, name := range append([]string{"participate", "roam", "share-uplink", "nan", "ble", "ethernet", "infrastructure-wifi"}, liteNetLimitFlags...) {
			changed = changed || cmd.Flags().Changed(name)
		}
		if !changed {
			return fmt.Errorf("specify a Wendy Net setting; omitted flags preserve saved settings")
		}
		target, err := resolveTarget(cmd.Context(), SuppressUpdateCheck())
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
		req, err := localMeshRequest(cmd)
		if err != nil {
			return err
		}
		if target.Agent == nil || target.Agent.Conn == nil {
			return fmt.Errorf("Wendy Net settings require a WendyOS agent")
		}
		client := pb.NewWendyLocalMeshAdminServiceClient(target.Agent.Conn)
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
	root.AddCommand(configure, inspect, newDeviceLocalMeshTopologyCmd())
	return root
}

func newDeviceLocalMeshTopologyCmd() *cobra.Command {
	var observation bool
	cmd := &cobra.Command{
		Use:   "topology",
		Short: "Show the selected device's observed local net peers, routes and route proofs",
		Long:  "Reads one live topology snapshot from the selected agent. Observations are local to that device; they do not prove fleet-wide reachability. Use --json for the complete snapshot.",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if observation && !jsonOutput {
				return fmt.Errorf("--observation requires --json")
			}
			topology, err := readLocalMeshTopology(cmd.Context())
			if observation {
				return printLocalMeshObservation(cmd, topology, err)
			}
			if err != nil {
				return err
			}
			return printLocalMeshTopology(cmd, topology)
		},
	}
	cmd.Flags().BoolVar(&observation, "observation", false, "With --json, emit a typed observation result including unsupported, unavailable and unauthorized states")
	return cmd
}

func readLocalMeshTopology(parent context.Context) (*pb.LocalMeshTopology, error) {
	ctx, cancel := context.WithTimeout(parent, localMeshTopologyTimeout)
	defer cancel()
	conn, err := connectLocalMeshTopology(ctx, ReadOnlyMonitoring())
	if err != nil {
		return nil, err
	}
	if conn == nil {
		return nil, fmt.Errorf("local topology requires a WendyOS agent")
	}
	defer conn.Close()
	if conn.Conn == nil {
		return nil, fmt.Errorf("local topology requires a WendyOS agent")
	}
	client := pb.NewWendyLocalMeshAdminServiceClient(conn.Conn)
	topology, err := client.GetLocalMeshTopology(ctx, &pb.GetLocalMeshTopologyRequest{})
	if err != nil {
		return nil, fmt.Errorf("reading local topology: %w", err)
	}
	peer, verified := observedLocalMeshTopologyIdentity(conn)
	if !verified {
		return nil, status.Error(codes.Unauthenticated, "topology reporter identity was not verified")
	}
	if err := browserauth.ValidateTopologyReporter(topology, peer); err != nil {
		return nil, err
	}
	return topology, nil
}

func printLocalMeshObservation(cmd *cobra.Command, topology *pb.LocalMeshTopology, readErr error) error {
	var observation browserauth.TopologyObservation
	if readErr != nil {
		observation = browserauth.TopologyFailure(readErr)
	} else {
		data, err := (protojson.MarshalOptions{EmitUnpopulated: true}).Marshal(topology)
		if err != nil {
			observation = browserauth.TopologyFailure(err)
		} else {
			observation = browserauth.TopologyObservation{Status: "ok", Topology: data, ObservedAt: time.Now().UTC()}
		}
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
	_, err := fmt.Fprintf(cmd.OutOrStdout(), "Device: %s\nNet address: %s\nCaptured: %s\nRuntime available: %t\nState: %s\n%s\nObserved peers: %d\nRoutes: %d\nRoute announcements: %d\nSnapshot truncated: %t\nUse --json for peer, route and proof details.\n",
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
		return nil, fmt.Errorf("specify --participate, --roam, --share-uplink, --nan, --ble, --ethernet or --infrastructure-wifi; omitted flags preserve saved settings")
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
