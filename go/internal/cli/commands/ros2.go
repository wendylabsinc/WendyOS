package commands

import (
	"archive/tar"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/wendylabsinc/wendy/go/internal/cli/tui"
	agentpbv2 "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
)

// newROS2Cmd builds the `wendy device ros2` command group: live ROS 2
// inspection of a device with zero SSH (WDY-1333).
func newROS2Cmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "ros2",
		Short: "Inspect and debug live ROS 2 systems on the device",
		Long: `Inspect and debug live ROS 2 systems on a device running Wendy Agent.

The agent runs ros2 commands in CLI sidecars for running apps configured with
"frameworks.ros2". When no ROS 2 app is running, it automatically uses the
device's system DDS graph, including Unitree robots, on domain 0. Use --domain
to select another domain. No ROS 2 app deployment or SSH is required.

System inspection uses a cached Humble/Fast DDS CLI image with pinned unitree_go
and unitree_api types, downloaded on first use. Other custom message types still
require their ROS 2 packages. Use lidar sample for bounded geometry observations.

To add ROS 2 to a project: wendy project frameworks add ros2
For the full "frameworks.ros2" config shape (domainId, rmw, distro,
discoveryScope) and how it's used: wendy docs ros2`,
	}

	cmd.AddCommand(
		newROS2NodesCmd(),
		newROS2TopicsCmd(),
		newROS2TopicCmd(),
		newROS2ServicesCmd(),
		newROS2CallCmd(),
		newROS2ParamsCmd(),
		newROS2ParamCmd(),
		newROS2EchoCmd(),
		newROS2HzCmd(),
		newROS2LidarCmd(),
		newROS2GraphCmd(),
		newROS2BagCmd(),
		newROS2DoctorCmd(),
		newROS2ActionCmd(),
		newROS2LifecycleCmd(),
		newROS2ComponentCmd(),
		newROS2ExecCmd(),
	)
	return cmd
}

// ── connection plumbing ─────────────────────────────────────────────

// ros2Client wraps the typed gRPC client plus the resolved device target.
type ros2Client struct {
	client agentpbv2.ROS2ServiceClient
	target *SelectedDevice
}

func newROS2Client(ctx context.Context) (*ros2Client, error) {
	target, err := resolveTarget(ctx, ExcludeProviders("local", "docker"))
	if err != nil {
		return nil, err
	}
	if target.Agent == nil {
		target.Close()
		return nil, fmt.Errorf("ROS 2 inspection requires a WendyOS agent connection (Bluetooth and external devices are not supported)")
	}
	return &ros2Client{
		client: agentpbv2.NewROS2ServiceClient(target.Agent.Conn),
		target: target,
	}, nil
}

func (c *ros2Client) Close() { c.target.Close() }

// ros2RPCError translates transport errors into actionable messages.
func ros2RPCError(err error) error {
	switch status.Code(err) {
	case codes.Unimplemented:
		return fmt.Errorf("this device's agent does not support ROS 2 inspection; update it with `wendy device update`")
	case codes.FailedPrecondition:
		return errors.New(status.Convert(err).Message())
	case codes.DeadlineExceeded:
		return errROS2Timeout
	case codes.ResourceExhausted:
		// Currently raised when one echoed message exceeds the streaming limit;
		// the server-side message already explains the remedy.
		return errors.New(status.Convert(err).Message())
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return errROS2Timeout
	}
	return err
}

// errROS2Timeout replaces a bare "context deadline exceeded" with something the
// user can act on. The unary RPCs are deadline-bounded (see ros2UnaryTimeout);
// before that a sidecar stuck on DDS discovery hung the CLI indefinitely with
// nothing on screen.
var errROS2Timeout = errors.New("timed out preparing the ROS 2 CLI or waiting for the device's graph; " +
	"first-use setup needs an image download — check the device's network connection and retry, " +
	"or try `wendy device ros2 doctor` and check --domain")

// Client-side deadlines for the non-streaming ROS 2 RPCs.
//
// There were none: every call rode the ambient context, so a stalled DDS
// discovery inside the sidecar hung the CLI indefinitely with nothing on screen.
// Each agent-side exec costs roughly a second (container exec dispatch + sourcing
// the ROS environment + a fresh rclpy process doing discovery), which sets the
// scale here.
//
// Streaming commands — echo, hz, bag record, exec, action send_goal — deliberately
// get no deadline: they run until the user stops them.
const (
	// ros2UnaryTimeout includes first-use system CLI image preparation as well
	// as discovery. Robots without app containers may need to download the
	// image; the former 45-second deadline could abort that initial setup.
	ros2UnaryTimeout = 5 * time.Minute
	// ros2FanOutTimeout covers the commands that fan out one exec per node or per
	// topic (graph, topics --all).
	ros2FanOutTimeout = 5 * time.Minute
)

// ros2Ctx derives a deadline-bounded context for a unary RPC. The caller must
// defer the returned cancel.
func ros2Ctx(parent context.Context, timeout time.Duration) (context.Context, context.CancelFunc) {
	return context.WithTimeout(parent, timeout)
}

// ros2DomainFlag registers the shared --domain override flag.
func ros2DomainFlag(cmd *cobra.Command, domain *int32) {
	cmd.Flags().Int32Var(domain, "domain", -1, "ROS_DOMAIN_ID override (default: app config, or 0 for the system graph)")
}

func ros2DomainPtr(domain int32) *int32 {
	if domain < 0 {
		return nil
	}
	return &domain
}

func printROS2JSON(v any) error {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

// ── inspection commands ─────────────────────────────────────────────

// ros2RMWShort renders an RMW identifier compactly for display, e.g.
// "rmw_cyclonedds_cpp" -> "cyclonedds".
func ros2RMWShort(rmw string) string {
	return strings.TrimSuffix(strings.TrimPrefix(rmw, "rmw_"), "_cpp")
}

// ros2ShowRMWTags reports whether the results span more than one RMW, so the
// per-result "[rmw]" tag is shown only on mixed-RMW devices and never clutters
// the common single-RMW case (WDY-1594).
func ros2ShowRMWTags(rmws ...string) bool {
	seen := map[string]struct{}{}
	for _, r := range rmws {
		if r != "" {
			seen[r] = struct{}{}
		}
	}
	return len(seen) > 1
}

func newROS2NodesCmd() *cobra.Command {
	var domain int32
	cmd := &cobra.Command{
		Use:   "nodes",
		Short: "List running ROS 2 nodes",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			client, err := newROS2Client(cmd.Context())
			if err != nil {
				return err
			}
			defer client.Close()

			rpcCtx, rpcCancel := ros2Ctx(cmd.Context(), ros2UnaryTimeout)
			defer rpcCancel()
			resp, err := client.client.ListNodes(rpcCtx, &agentpbv2.ListROS2NodesRequest{DomainId: ros2DomainPtr(domain)})
			if err != nil {
				return ros2RPCError(err)
			}
			if jsonOutput {
				type node struct {
					Name      string `json:"name"`
					Namespace string `json:"namespace"`
					RMW       string `json:"rmw,omitempty"`
				}
				nodes := make([]node, 0, len(resp.GetNodes()))
				for _, n := range resp.GetNodes() {
					nodes = append(nodes, node{Name: n.GetName(), Namespace: n.GetNamespace(), RMW: n.GetRmw()})
				}
				return printROS2JSON(nodes)
			}
			if len(resp.GetNodes()) == 0 {
				cliNotice("No ROS 2 nodes found.")
				return nil
			}
			rmws := make([]string, 0, len(resp.GetNodes()))
			for _, n := range resp.GetNodes() {
				rmws = append(rmws, n.GetRmw())
			}
			showTags := ros2ShowRMWTags(rmws...)
			for _, n := range resp.GetNodes() {
				line := ros2GraphNodeFQN(n)
				if showTags && n.GetRmw() != "" {
					line += "  [" + ros2RMWShort(n.GetRmw()) + "]"
				}
				fmt.Println(line)
			}
			return nil
		},
	}
	ros2DomainFlag(cmd, &domain)
	return cmd
}

func newROS2TopicsCmd() *cobra.Command {
	var domain int32
	var all bool
	cmd := &cobra.Command{
		Use:   "topics",
		Short: "List ROS 2 topics",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			client, err := newROS2Client(cmd.Context())
			if err != nil {
				return err
			}
			defer client.Close()

			rpcCtx, rpcCancel := ros2Ctx(cmd.Context(), ros2FanOutTimeout)
			defer rpcCancel()
			resp, err := client.client.ListTopics(rpcCtx, &agentpbv2.ListROS2TopicsRequest{
				DomainId:      ros2DomainPtr(domain),
				IncludeCounts: all,
			})
			if err != nil {
				return ros2RPCError(err)
			}
			if jsonOutput {
				type topic struct {
					Name        string   `json:"name"`
					Types       []string `json:"types"`
					Publishers  int32    `json:"publishers,omitempty"`
					Subscribers int32    `json:"subscribers,omitempty"`
					RMW         string   `json:"rmw,omitempty"`
				}
				topics := make([]topic, 0, len(resp.GetTopics()))
				for _, t := range resp.GetTopics() {
					topics = append(topics, topic{
						Name:        t.GetName(),
						Types:       t.GetTypes(),
						Publishers:  t.GetPublisherCount(),
						Subscribers: t.GetSubscriberCount(),
						RMW:         t.GetRmw(),
					})
				}
				return printROS2JSON(topics)
			}
			if len(resp.GetTopics()) == 0 {
				cliNotice("No ROS 2 topics found.")
				return nil
			}
			rmws := make([]string, 0, len(resp.GetTopics()))
			for _, t := range resp.GetTopics() {
				rmws = append(rmws, t.GetRmw())
			}
			rmwCol := ros2ShowRMWTags(rmws...)
			w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
			switch {
			case all && rmwCol:
				fmt.Fprintln(w, "TOPIC\tTYPE\tPUBS\tSUBS\tRMW")
				for _, t := range resp.GetTopics() {
					fmt.Fprintf(w, "%s\t%s\t%d\t%d\t%s\n", t.GetName(), strings.Join(t.GetTypes(), ", "), t.GetPublisherCount(), t.GetSubscriberCount(), ros2RMWShort(t.GetRmw()))
				}
			case all:
				fmt.Fprintln(w, "TOPIC\tTYPE\tPUBS\tSUBS")
				for _, t := range resp.GetTopics() {
					fmt.Fprintf(w, "%s\t%s\t%d\t%d\n", t.GetName(), strings.Join(t.GetTypes(), ", "), t.GetPublisherCount(), t.GetSubscriberCount())
				}
			case rmwCol:
				fmt.Fprintln(w, "TOPIC\tTYPE\tRMW")
				for _, t := range resp.GetTopics() {
					fmt.Fprintf(w, "%s\t%s\t%s\n", t.GetName(), strings.Join(t.GetTypes(), ", "), ros2RMWShort(t.GetRmw()))
				}
			default:
				fmt.Fprintln(w, "TOPIC\tTYPE")
				for _, t := range resp.GetTopics() {
					fmt.Fprintf(w, "%s\t%s\n", t.GetName(), strings.Join(t.GetTypes(), ", "))
				}
			}
			return w.Flush()
		},
	}
	ros2DomainFlag(cmd, &domain)
	cmd.Flags().BoolVar(&all, "all", false, "Include publisher/subscriber counts")
	return cmd
}

func newROS2TopicCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "topic",
		Short: "Topic subcommands",
	}
	var domain int32
	info := &cobra.Command{
		Use:   "info <topic>",
		Short: "Show type, endpoints, and QoS for a topic",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			client, err := newROS2Client(cmd.Context())
			if err != nil {
				return err
			}
			defer client.Close()

			rpcCtx, rpcCancel := ros2Ctx(cmd.Context(), ros2UnaryTimeout)
			defer rpcCancel()
			resp, err := client.client.GetTopicInfo(rpcCtx, &agentpbv2.GetROS2TopicInfoRequest{
				DomainId: ros2DomainPtr(domain),
				Topic:    args[0],
			})
			if err != nil {
				return ros2RPCError(err)
			}
			if jsonOutput {
				return printROS2JSON(map[string]any{
					"name":        resp.GetTopic().GetName(),
					"types":       resp.GetTopic().GetTypes(),
					"publishers":  resp.GetTopic().GetPublisherCount(),
					"subscribers": resp.GetTopic().GetSubscriberCount(),
					"verbose":     resp.GetVerbose(),
				})
			}
			fmt.Print(resp.GetVerbose())
			return nil
		},
	}
	ros2DomainFlag(info, &domain)
	cmd.AddCommand(info)
	return cmd
}

func newROS2ServicesCmd() *cobra.Command {
	var domain int32
	cmd := &cobra.Command{
		Use:   "services",
		Short: "List ROS 2 services",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			client, err := newROS2Client(cmd.Context())
			if err != nil {
				return err
			}
			defer client.Close()

			rpcCtx, rpcCancel := ros2Ctx(cmd.Context(), ros2UnaryTimeout)
			defer rpcCancel()
			resp, err := client.client.ListServices(rpcCtx, &agentpbv2.ListROS2ServicesRequest{DomainId: ros2DomainPtr(domain)})
			if err != nil {
				return ros2RPCError(err)
			}
			if jsonOutput {
				type svc struct {
					Name  string   `json:"name"`
					Types []string `json:"types"`
					RMW   string   `json:"rmw,omitempty"`
				}
				svcs := make([]svc, 0, len(resp.GetServices()))
				for _, s := range resp.GetServices() {
					svcs = append(svcs, svc{Name: s.GetName(), Types: s.GetTypes(), RMW: s.GetRmw()})
				}
				return printROS2JSON(svcs)
			}
			if len(resp.GetServices()) == 0 {
				cliNotice("No ROS 2 services found.")
				return nil
			}
			rmws := make([]string, 0, len(resp.GetServices()))
			for _, s := range resp.GetServices() {
				rmws = append(rmws, s.GetRmw())
			}
			rmwCol := ros2ShowRMWTags(rmws...)
			w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
			if rmwCol {
				fmt.Fprintln(w, "SERVICE\tTYPE\tRMW")
				for _, s := range resp.GetServices() {
					fmt.Fprintf(w, "%s\t%s\t%s\n", s.GetName(), strings.Join(s.GetTypes(), ", "), ros2RMWShort(s.GetRmw()))
				}
			} else {
				fmt.Fprintln(w, "SERVICE\tTYPE")
				for _, s := range resp.GetServices() {
					fmt.Fprintf(w, "%s\t%s\n", s.GetName(), strings.Join(s.GetTypes(), ", "))
				}
			}
			return w.Flush()
		},
	}
	ros2DomainFlag(cmd, &domain)
	return cmd
}

func newROS2CallCmd() *cobra.Command {
	var domain int32
	cmd := &cobra.Command{
		Use:   "call <service> <type> [request]",
		Short: "Call a ROS 2 service",
		Long:  "Call a ROS 2 service, e.g.\n  wendy device ros2 call /reset std_srvs/srv/Empty\n  wendy device ros2 call /set_speed my_msgs/srv/SetSpeed '{speed: 1.5}'",
		Args:  cobra.RangeArgs(2, 3),
		RunE: func(cmd *cobra.Command, args []string) error {
			client, err := newROS2Client(cmd.Context())
			if err != nil {
				return err
			}
			defer client.Close()

			req := &agentpbv2.CallROS2ServiceRequest{
				DomainId: ros2DomainPtr(domain),
				Service:  args[0],
				Type:     args[1],
			}
			if len(args) == 3 {
				req.Request = args[2]
			}
			rpcCtx, rpcCancel := ros2Ctx(cmd.Context(), ros2UnaryTimeout)
			defer rpcCancel()
			resp, err := client.client.CallService(rpcCtx, req)
			if err != nil {
				return ros2RPCError(err)
			}
			if jsonOutput {
				return printROS2JSON(map[string]any{"success": resp.GetSuccess(), "response": resp.GetResponse()})
			}
			fmt.Println(resp.GetResponse())
			if !resp.GetSuccess() {
				return fmt.Errorf("service call failed")
			}
			return nil
		},
	}
	ros2DomainFlag(cmd, &domain)
	return cmd
}

func newROS2ParamsCmd() *cobra.Command {
	var domain int32
	var node string
	cmd := &cobra.Command{
		Use:   "params",
		Short: "List parameters across all nodes (or one node)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			client, err := newROS2Client(cmd.Context())
			if err != nil {
				return err
			}
			defer client.Close()

			rpcCtx, rpcCancel := ros2Ctx(cmd.Context(), ros2UnaryTimeout)
			defer rpcCancel()
			resp, err := client.client.ListParams(rpcCtx, &agentpbv2.ListROS2ParamsRequest{
				DomainId: ros2DomainPtr(domain),
				Node:     node,
			})
			if err != nil {
				return ros2RPCError(err)
			}
			if jsonOutput {
				params := make(map[string][]string, len(resp.GetNodes()))
				for _, n := range resp.GetNodes() {
					params[n.GetNode()] = n.GetParams()
				}
				return printROS2JSON(params)
			}
			for _, n := range resp.GetNodes() {
				fmt.Printf("%s:\n", n.GetNode())
				for _, p := range n.GetParams() {
					fmt.Printf("  %s\n", p)
				}
			}
			return nil
		},
	}
	ros2DomainFlag(cmd, &domain)
	cmd.Flags().StringVar(&node, "node", "", "Only list parameters of this node")
	return cmd
}

func newROS2ParamCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "param",
		Short: "Get or set node parameters",
	}

	var getDomain int32
	get := &cobra.Command{
		Use:   "get <node> <param>",
		Short: "Get a parameter value",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			client, err := newROS2Client(cmd.Context())
			if err != nil {
				return err
			}
			defer client.Close()

			rpcCtx, rpcCancel := ros2Ctx(cmd.Context(), ros2UnaryTimeout)
			defer rpcCancel()
			resp, err := client.client.GetParam(rpcCtx, &agentpbv2.GetROS2ParamRequest{
				DomainId: ros2DomainPtr(getDomain),
				Node:     args[0],
				Param:    args[1],
			})
			if err != nil {
				return ros2RPCError(err)
			}
			if jsonOutput {
				return printROS2JSON(map[string]string{"node": args[0], "param": args[1], "value": resp.GetValue()})
			}
			fmt.Println(resp.GetValue())
			return nil
		},
	}
	ros2DomainFlag(get, &getDomain)

	var setDomain int32
	set := &cobra.Command{
		Use:   "set <node> <param> <value>",
		Short: "Set a parameter value live — no restart needed",
		Args:  cobra.ExactArgs(3),
		RunE: func(cmd *cobra.Command, args []string) error {
			client, err := newROS2Client(cmd.Context())
			if err != nil {
				return err
			}
			defer client.Close()

			rpcCtx, rpcCancel := ros2Ctx(cmd.Context(), ros2UnaryTimeout)
			defer rpcCancel()
			resp, err := client.client.SetParam(rpcCtx, &agentpbv2.SetROS2ParamRequest{
				DomainId: ros2DomainPtr(setDomain),
				Node:     args[0],
				Param:    args[1],
				Value:    args[2],
			})
			if err != nil {
				return ros2RPCError(err)
			}
			if jsonOutput {
				return printROS2JSON(map[string]any{"success": resp.GetSuccess(), "message": resp.GetMessage()})
			}
			if !resp.GetSuccess() {
				return fmt.Errorf("setting parameter: %s", resp.GetMessage())
			}
			cliSuccess("%s", resp.GetMessage())
			return nil
		},
	}
	ros2DomainFlag(set, &setDomain)

	cmd.AddCommand(get, set)
	return cmd
}

func newROS2DoctorCmd() *cobra.Command {
	var domain int32
	cmd := &cobra.Command{
		Use:   "doctor",
		Short: "Run ros2 doctor health checks on the device",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			client, err := newROS2Client(cmd.Context())
			if err != nil {
				return err
			}
			defer client.Close()

			rpcCtx, rpcCancel := ros2Ctx(cmd.Context(), ros2UnaryTimeout)
			defer rpcCancel()
			resp, err := client.client.Doctor(rpcCtx, &agentpbv2.ROS2DoctorRequest{DomainId: ros2DomainPtr(domain)})
			if err != nil {
				return ros2RPCError(err)
			}
			if jsonOutput {
				return printROS2JSON(map[string]string{"report": resp.GetReport()})
			}
			fmt.Print(resp.GetReport())
			return nil
		},
	}
	ros2DomainFlag(cmd, &domain)
	return cmd
}

// ── actions / lifecycle / components (WDY-1722) ─────────────────────

func newROS2ActionCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "action",
		Short: "Inspect ROS 2 actions and dispatch goals",
	}

	var listDomain int32
	list := &cobra.Command{
		Use:   "list",
		Short: "List ROS 2 actions",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			client, err := newROS2Client(cmd.Context())
			if err != nil {
				return err
			}
			defer client.Close()

			rpcCtx, rpcCancel := ros2Ctx(cmd.Context(), ros2UnaryTimeout)
			defer rpcCancel()
			resp, err := client.client.ListActions(rpcCtx, &agentpbv2.ListROS2ActionsRequest{DomainId: ros2DomainPtr(listDomain)})
			if err != nil {
				return ros2RPCError(err)
			}
			if jsonOutput {
				type action struct {
					Name  string   `json:"name"`
					Types []string `json:"types"`
					RMW   string   `json:"rmw,omitempty"`
				}
				actions := make([]action, 0, len(resp.GetActions()))
				for _, a := range resp.GetActions() {
					actions = append(actions, action{Name: a.GetName(), Types: a.GetTypes(), RMW: a.GetRmw()})
				}
				return printROS2JSON(actions)
			}
			if len(resp.GetActions()) == 0 {
				cliNotice("No ROS 2 actions found.")
				return nil
			}
			rmws := make([]string, 0, len(resp.GetActions()))
			for _, a := range resp.GetActions() {
				rmws = append(rmws, a.GetRmw())
			}
			rmwCol := ros2ShowRMWTags(rmws...)
			w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
			if rmwCol {
				fmt.Fprintln(w, "ACTION\tTYPE\tRMW")
				for _, a := range resp.GetActions() {
					fmt.Fprintf(w, "%s\t%s\t%s\n", a.GetName(), strings.Join(a.GetTypes(), ", "), ros2RMWShort(a.GetRmw()))
				}
			} else {
				fmt.Fprintln(w, "ACTION\tTYPE")
				for _, a := range resp.GetActions() {
					fmt.Fprintf(w, "%s\t%s\n", a.GetName(), strings.Join(a.GetTypes(), ", "))
				}
			}
			return w.Flush()
		},
	}
	ros2DomainFlag(list, &listDomain)

	var infoDomain int32
	info := &cobra.Command{
		Use:   "info <action>",
		Short: "Show clients and servers for an action",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			client, err := newROS2Client(cmd.Context())
			if err != nil {
				return err
			}
			defer client.Close()

			rpcCtx, rpcCancel := ros2Ctx(cmd.Context(), ros2UnaryTimeout)
			defer rpcCancel()
			resp, err := client.client.GetActionInfo(rpcCtx, &agentpbv2.GetROS2ActionInfoRequest{
				DomainId: ros2DomainPtr(infoDomain),
				Action:   args[0],
			})
			if err != nil {
				return ros2RPCError(err)
			}
			if jsonOutput {
				return printROS2JSON(map[string]any{
					"name":    resp.GetName(),
					"clients": resp.GetActionClients(),
					"servers": resp.GetActionServers(),
					"verbose": resp.GetVerbose(),
				})
			}
			fmt.Print(resp.GetVerbose())
			return nil
		},
	}
	ros2DomainFlag(info, &infoDomain)

	var sendDomain int32
	var feedback bool
	sendGoal := &cobra.Command{
		Use:   "send_goal <action> <type> <goal>",
		Short: "Send a goal to an action and stream feedback/result (ctrl-c to cancel)",
		Args:  cobra.ExactArgs(3),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
			defer stop()

			client, err := newROS2Client(ctx)
			if err != nil {
				return err
			}
			defer client.Close()

			stream, err := client.client.SendActionGoal(ctx, &agentpbv2.SendROS2ActionGoalRequest{
				DomainId:   ros2DomainPtr(sendDomain),
				Action:     args[0],
				ActionType: args[1],
				Goal:       args[2],
				Feedback:   feedback,
			})
			if err != nil {
				return ros2RPCError(err)
			}
			return drainExecStream(ctx, stream, []string{"action", "send_goal", args[0]}, os.Stdout, os.Stderr)
		},
	}
	ros2DomainFlag(sendGoal, &sendDomain)
	sendGoal.Flags().BoolVar(&feedback, "feedback", false, "Stream feedback messages as the goal executes")

	var cancelDomain int32
	cancel := &cobra.Command{
		Use:   "cancel <action>",
		Short: "Cancel all in-flight goals on an action (wendy-specific, best-effort)",
		Long: `Cancel all in-flight goals on an action.

Upstream ros2 has no "action cancel" verb; this is a wendy convenience that
calls the action's _action/cancel_goal service with an all-zero goal id, which
the action server interprets as "cancel all active goals". To cancel a single
goal interactively, ctrl-c its "send_goal" instead.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
			defer stop()

			client, err := newROS2Client(ctx)
			if err != nil {
				return err
			}
			defer client.Close()

			// Route through the generic Exec passthrough: cancel-all via the
			// action's cancel_goal service (zero goal id + zero stamp).
			fwdArgs := []string{
				"service", "call",
				strings.TrimRight(args[0], "/") + "/_action/cancel_goal",
				"action_msgs/srv/CancelGoal",
				"{goal_info: {goal_id: {uuid: [0,0,0,0,0,0,0,0,0,0,0,0,0,0,0,0]}, stamp: {sec: 0, nanosec: 0}}}",
			}
			stream, err := client.client.Exec(ctx, &agentpbv2.ROS2ExecRequest{
				DomainId: ros2DomainPtr(cancelDomain),
				Args:     fwdArgs,
			})
			if err != nil {
				return ros2RPCError(err)
			}
			return drainExecStream(ctx, stream, fwdArgs, os.Stdout, os.Stderr)
		},
	}
	ros2DomainFlag(cancel, &cancelDomain)

	cmd.AddCommand(list, info, sendGoal, cancel)
	return cmd
}

func newROS2LifecycleCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "lifecycle",
		Short: "Inspect and transition managed (lifecycle) nodes",
	}

	var nodesDomain int32
	nodes := &cobra.Command{
		Use:   "nodes",
		Short: "List managed (lifecycle) nodes",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			client, err := newROS2Client(cmd.Context())
			if err != nil {
				return err
			}
			defer client.Close()

			rpcCtx, rpcCancel := ros2Ctx(cmd.Context(), ros2UnaryTimeout)
			defer rpcCancel()
			resp, err := client.client.ListLifecycleNodes(rpcCtx, &agentpbv2.ListROS2LifecycleNodesRequest{DomainId: ros2DomainPtr(nodesDomain)})
			if err != nil {
				return ros2RPCError(err)
			}
			if jsonOutput {
				type node struct {
					Name      string `json:"name"`
					Namespace string `json:"namespace"`
					RMW       string `json:"rmw,omitempty"`
				}
				ns := make([]node, 0, len(resp.GetNodes()))
				for _, n := range resp.GetNodes() {
					ns = append(ns, node{Name: n.GetName(), Namespace: n.GetNamespace(), RMW: n.GetRmw()})
				}
				return printROS2JSON(ns)
			}
			if len(resp.GetNodes()) == 0 {
				cliNotice("No managed (lifecycle) nodes found.")
				return nil
			}
			rmws := make([]string, 0, len(resp.GetNodes()))
			for _, n := range resp.GetNodes() {
				rmws = append(rmws, n.GetRmw())
			}
			showTags := ros2ShowRMWTags(rmws...)
			for _, n := range resp.GetNodes() {
				line := ros2GraphNodeFQN(n)
				if showTags && n.GetRmw() != "" {
					line += "  [" + ros2RMWShort(n.GetRmw()) + "]"
				}
				fmt.Println(line)
			}
			return nil
		},
	}
	ros2DomainFlag(nodes, &nodesDomain)

	var getDomain int32
	get := &cobra.Command{
		Use:   "get <node>",
		Short: "Get the current lifecycle state of a node",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			client, err := newROS2Client(cmd.Context())
			if err != nil {
				return err
			}
			defer client.Close()

			rpcCtx, rpcCancel := ros2Ctx(cmd.Context(), ros2UnaryTimeout)
			defer rpcCancel()
			resp, err := client.client.GetLifecycleState(rpcCtx, &agentpbv2.GetROS2LifecycleStateRequest{
				DomainId: ros2DomainPtr(getDomain),
				Node:     args[0],
			})
			if err != nil {
				return ros2RPCError(err)
			}
			if jsonOutput {
				return printROS2JSON(map[string]any{"node": args[0], "state": resp.GetState(), "stateId": resp.GetStateId()})
			}
			fmt.Printf("%s [%d]\n", resp.GetState(), resp.GetStateId())
			return nil
		},
	}
	ros2DomainFlag(get, &getDomain)

	var listDomain int32
	list := &cobra.Command{
		Use:   "list <node>",
		Short: "List the available lifecycle transitions for a node",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			client, err := newROS2Client(cmd.Context())
			if err != nil {
				return err
			}
			defer client.Close()

			rpcCtx, rpcCancel := ros2Ctx(cmd.Context(), ros2UnaryTimeout)
			defer rpcCancel()
			resp, err := client.client.ListLifecycleTransitions(rpcCtx, &agentpbv2.ListROS2LifecycleTransitionsRequest{
				DomainId: ros2DomainPtr(listDomain),
				Node:     args[0],
			})
			if err != nil {
				return ros2RPCError(err)
			}
			if jsonOutput {
				type transition struct {
					Label string `json:"label"`
					ID    uint32 `json:"id"`
					Start string `json:"start"`
					Goal  string `json:"goal"`
				}
				ts := make([]transition, 0, len(resp.GetTransitions()))
				for _, t := range resp.GetTransitions() {
					ts = append(ts, transition{Label: t.GetLabel(), ID: t.GetId(), Start: t.GetStartState(), Goal: t.GetGoalState()})
				}
				return printROS2JSON(ts)
			}
			if len(resp.GetTransitions()) == 0 {
				cliNotice("No lifecycle transitions available.")
				return nil
			}
			w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
			fmt.Fprintln(w, "TRANSITION\tID\tSTART\tGOAL")
			for _, t := range resp.GetTransitions() {
				fmt.Fprintf(w, "%s\t%d\t%s\t%s\n", t.GetLabel(), t.GetId(), t.GetStartState(), t.GetGoalState())
			}
			return w.Flush()
		},
	}
	ros2DomainFlag(list, &listDomain)

	var setDomain int32
	set := &cobra.Command{
		Use:   "set <node> <transition>",
		Short: "Trigger a lifecycle transition (e.g. configure, activate)",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			client, err := newROS2Client(cmd.Context())
			if err != nil {
				return err
			}
			defer client.Close()

			rpcCtx, rpcCancel := ros2Ctx(cmd.Context(), ros2UnaryTimeout)
			defer rpcCancel()
			resp, err := client.client.SetLifecycleState(rpcCtx, &agentpbv2.SetROS2LifecycleStateRequest{
				DomainId:   ros2DomainPtr(setDomain),
				Node:       args[0],
				Transition: args[1],
			})
			if err != nil {
				return ros2RPCError(err)
			}
			if jsonOutput {
				return printROS2JSON(map[string]any{"success": resp.GetSuccess(), "message": resp.GetMessage()})
			}
			if !resp.GetSuccess() {
				return fmt.Errorf("transition failed: %s", resp.GetMessage())
			}
			cliSuccess("%s", resp.GetMessage())
			return nil
		},
	}
	ros2DomainFlag(set, &setDomain)

	cmd.AddCommand(nodes, get, list, set)
	return cmd
}

func newROS2ComponentCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "component",
		Short: "Manage composable-node component containers",
	}

	var listDomain int32
	list := &cobra.Command{
		Use:   "list",
		Short: "List component containers and their loaded components",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			client, err := newROS2Client(cmd.Context())
			if err != nil {
				return err
			}
			defer client.Close()

			rpcCtx, rpcCancel := ros2Ctx(cmd.Context(), ros2UnaryTimeout)
			defer rpcCancel()
			resp, err := client.client.ListComponents(rpcCtx, &agentpbv2.ListROS2ComponentsRequest{DomainId: ros2DomainPtr(listDomain)})
			if err != nil {
				return ros2RPCError(err)
			}
			if jsonOutput {
				type component struct {
					UID  uint32 `json:"uid"`
					Name string `json:"name"`
				}
				type container struct {
					Name       string      `json:"name"`
					RMW        string      `json:"rmw,omitempty"`
					Components []component `json:"components"`
				}
				containers := make([]container, 0, len(resp.GetContainers()))
				for _, c := range resp.GetContainers() {
					comps := make([]component, 0, len(c.GetComponents()))
					for _, comp := range c.GetComponents() {
						comps = append(comps, component{UID: comp.GetUid(), Name: comp.GetName()})
					}
					containers = append(containers, container{Name: c.GetName(), RMW: c.GetRmw(), Components: comps})
				}
				return printROS2JSON(containers)
			}
			if len(resp.GetContainers()) == 0 {
				cliNotice("No component containers found.")
				return nil
			}
			rmws := make([]string, 0, len(resp.GetContainers()))
			for _, c := range resp.GetContainers() {
				rmws = append(rmws, c.GetRmw())
			}
			showTags := ros2ShowRMWTags(rmws...)
			for _, c := range resp.GetContainers() {
				header := c.GetName()
				if showTags && c.GetRmw() != "" {
					header += "  [" + ros2RMWShort(c.GetRmw()) + "]"
				}
				fmt.Println(header)
				for _, comp := range c.GetComponents() {
					fmt.Printf("  %d  %s\n", comp.GetUid(), comp.GetName())
				}
			}
			return nil
		},
	}
	ros2DomainFlag(list, &listDomain)

	var loadDomain int32
	load := &cobra.Command{
		Use:   "load <container> <package> <plugin>",
		Short: "Load a component into a running container",
		Args:  cobra.ExactArgs(3),
		RunE: func(cmd *cobra.Command, args []string) error {
			client, err := newROS2Client(cmd.Context())
			if err != nil {
				return err
			}
			defer client.Close()

			rpcCtx, rpcCancel := ros2Ctx(cmd.Context(), ros2UnaryTimeout)
			defer rpcCancel()
			resp, err := client.client.LoadComponent(rpcCtx, &agentpbv2.LoadROS2ComponentRequest{
				DomainId:  ros2DomainPtr(loadDomain),
				Container: args[0],
				Package:   args[1],
				Plugin:    args[2],
			})
			if err != nil {
				return ros2RPCError(err)
			}
			if jsonOutput {
				return printROS2JSON(map[string]any{"uid": resp.GetUid(), "nodeName": resp.GetNodeName(), "message": resp.GetMessage()})
			}
			cliSuccess("%s", resp.GetMessage())
			return nil
		},
	}
	ros2DomainFlag(load, &loadDomain)

	var unloadDomain int32
	unload := &cobra.Command{
		Use:   "unload <container> <uid>",
		Short: "Unload a component from a running container",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			uid, err := strconv.ParseUint(args[1], 10, 32)
			if err != nil {
				return fmt.Errorf("invalid component uid %q: must be a number", args[1])
			}
			client, err := newROS2Client(cmd.Context())
			if err != nil {
				return err
			}
			defer client.Close()

			rpcCtx, rpcCancel := ros2Ctx(cmd.Context(), ros2UnaryTimeout)
			defer rpcCancel()
			resp, err := client.client.UnloadComponent(rpcCtx, &agentpbv2.UnloadROS2ComponentRequest{
				DomainId:  ros2DomainPtr(unloadDomain),
				Container: args[0],
				Uid:       uint32(uid),
			})
			if err != nil {
				return ros2RPCError(err)
			}
			if jsonOutput {
				return printROS2JSON(map[string]any{"message": resp.GetMessage()})
			}
			cliSuccess("%s", resp.GetMessage())
			return nil
		},
	}
	ros2DomainFlag(unload, &unloadDomain)

	cmd.AddCommand(list, load, unload)
	return cmd
}

// ── streaming commands ──────────────────────────────────────────────

// echoRecvStream is the minimal interface consumed by drainEchoStream,
// satisfied by grpc.ServerStreamingClient[agentpbv2.ROS2Message].
type echoRecvStream interface {
	Recv() (*agentpbv2.ROS2Message, error)
}

// drainEchoStream reads messages from stream until io.EOF or ctx cancellation,
// printing each message to stdout.  When the stream ends having delivered zero
// messages, it writes a stderr notice so the user isn't left with silent output.
//
// WDY-1708 (claim b): exit 0 is intentional — in ROS 2, "no active publishers
// yet" is not a hard error; topics are dynamic and a streaming echo on an idle
// or absent topic legitimately produces no output.  However, silence without
// feedback is confusing, so we emit a single notice on stderr.  At least one
// message received → no notice.
func drainEchoStream(ctx context.Context, stream echoRecvStream, topic string, useJSON bool, stderr io.Writer) error {
	received := 0
	for {
		msg, rerr := stream.Recv()
		if rerr != nil {
			if rerr == io.EOF || ctx.Err() != nil {
				// WDY-1708: keep exit 0, but surface a notice when no messages
				// arrived so the user knows the topic had no active publishers.
				if received == 0 {
					fmt.Fprintf(stderr, "Notice: No messages received on %s — the topic may have no active publishers.\n", topic)
				}
				return nil
			}
			return ros2RPCError(rerr)
		}
		received++
		if useJSON {
			if jerr := printROS2JSON(map[string]string{"topic": msg.GetTopic(), "yaml": msg.GetYaml()}); jerr != nil {
				return jerr
			}
			continue
		}
		fmt.Print(msg.GetYaml())
		fmt.Println("---")
	}
}

func newROS2EchoCmd() *cobra.Command {
	var domain int32
	var count int32
	cmd := &cobra.Command{
		Use:   "echo <topic>",
		Short: "Stream deserialized messages from a topic (ctrl-c to stop)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
			defer stop()

			client, err := newROS2Client(ctx)
			if err != nil {
				return err
			}
			defer client.Close()

			stream, err := client.client.EchoTopic(ctx, &agentpbv2.EchoROS2TopicRequest{
				DomainId: ros2DomainPtr(domain),
				Topic:    args[0],
				Count:    count,
			})
			if err != nil {
				return ros2RPCError(err)
			}
			return drainEchoStream(ctx, stream, args[0], jsonOutput, os.Stderr)
		},
	}
	ros2DomainFlag(cmd, &domain)
	cmd.Flags().Int32Var(&count, "count", 0, "Stop after N messages (0 = until ctrl-c)")
	return cmd
}

func newROS2HzCmd() *cobra.Command {
	var domain int32
	cmd := &cobra.Command{
		Use:   "hz <topic>",
		Short: "Monitor the live publish rate of a topic (ctrl-c to stop)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
			defer stop()

			client, err := newROS2Client(ctx)
			if err != nil {
				return err
			}
			defer client.Close()

			stream, err := client.client.MonitorHz(ctx, &agentpbv2.MonitorROS2HzRequest{
				DomainId: ros2DomainPtr(domain),
				Topic:    args[0],
			})
			if err != nil {
				return ros2RPCError(err)
			}
			for {
				sample, rerr := stream.Recv()
				if rerr != nil {
					if rerr == io.EOF || ctx.Err() != nil {
						return nil
					}
					return ros2RPCError(rerr)
				}
				if jsonOutput {
					if jerr := printROS2JSON(map[string]any{
						"hz": sample.GetHz(), "minDelta": sample.GetMinDelta(),
						"maxDelta": sample.GetMaxDelta(), "stdDev": sample.GetStdDev(),
						"window": sample.GetWindow(),
					}); jerr != nil {
						return jerr
					}
					continue
				}
				fmt.Printf("average rate: %.3f  min: %.3fs  max: %.3fs  std dev: %.5fs  window: %d\n",
					sample.GetHz(), sample.GetMinDelta(), sample.GetMaxDelta(), sample.GetStdDev(), sample.GetWindow())
			}
		},
	}
	ros2DomainFlag(cmd, &domain)
	return cmd
}

// ── graph ───────────────────────────────────────────────────────────

func newROS2GraphCmd() *cobra.Command {
	var domain int32
	var format string
	cmd := &cobra.Command{
		Use:   "graph",
		Short: "Render the node/topic connectivity graph",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			client, err := newROS2Client(cmd.Context())
			if err != nil {
				return err
			}
			defer client.Close()

			rpcCtx, rpcCancel := ros2Ctx(cmd.Context(), ros2FanOutTimeout)
			defer rpcCancel()
			resp, err := client.client.GetGraph(rpcCtx, &agentpbv2.GetROS2GraphRequest{DomainId: ros2DomainPtr(domain)})
			if err != nil {
				return ros2RPCError(err)
			}
			switch format {
			case "ascii", "":
				fmt.Print(renderROS2GraphASCII(resp))
			case "dot":
				fmt.Print(renderROS2GraphDOT(resp))
			default:
				return fmt.Errorf("unknown graph format %q (supported: ascii, dot)", format)
			}
			return nil
		},
	}
	ros2DomainFlag(cmd, &domain)
	cmd.Flags().StringVar(&format, "format", "ascii", "Output format: ascii or dot (Graphviz)")
	return cmd
}

// ── bag commands ────────────────────────────────────────────────────

func newROS2BagCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "bag",
		Short: "Record, list, and download rosbag2 recordings",
	}
	cmd.AddCommand(
		newROS2BagRecordCmd(),
		newROS2BagListCmd(),
		newROS2BagDownloadCmd(),
	)
	return cmd
}

func newROS2BagRecordCmd() *cobra.Command {
	var domain int32
	var output string
	cmd := &cobra.Command{
		Use:   "record [topics...]",
		Short: "Record topics to a bag on the device (ctrl-c to stop and finalize)",
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
			defer stop()

			client, err := newROS2Client(ctx)
			if err != nil {
				return err
			}
			defer client.Close()

			// Use the parent context for the stream: the bidi session must
			// outlive ctrl-c so the explicit stop command and the final
			// STOPPED response can travel after the signal fires.
			stream, err := client.client.RecordBag(cmd.Context())
			if err != nil {
				return ros2RPCError(err)
			}
			err = stream.Send(&agentpbv2.RecordROS2BagRequest{
				Command: &agentpbv2.RecordROS2BagRequest_Start{
					Start: &agentpbv2.RecordROS2BagRequest_RecordStart{
						DomainId:   ros2DomainPtr(domain),
						Topics:     args,
						OutputName: output,
					},
				},
			})
			if err != nil {
				return ros2RPCError(err)
			}

			first, err := stream.Recv()
			if err != nil {
				return ros2RPCError(err)
			}
			if first.GetState() == agentpbv2.RecordROS2BagResponse_STATE_ERROR {
				return fmt.Errorf("recording failed: %s", first.GetMessage())
			}
			topicsDesc := "all topics"
			if len(args) > 0 {
				topicsDesc = strings.Join(args, ", ")
			}
			cliLogln("Recording %s to bag %q on the device. Press ctrl-c to stop.", topicsDesc, first.GetBagName())
			if m := first.GetMessage(); m != "" {
				cliLogln("Note: %s", m)
			}

			// Wait for ctrl-c or an unsolicited terminal message (recorder error).
			recvCh := make(chan *agentpbv2.RecordROS2BagResponse, 1)
			recvErr := make(chan error, 1)
			go func() {
				msg, rerr := stream.Recv()
				if rerr != nil {
					recvErr <- rerr
					return
				}
				recvCh <- msg
			}()

			select {
			case <-ctx.Done():
				if serr := stream.Send(&agentpbv2.RecordROS2BagRequest{
					Command: &agentpbv2.RecordROS2BagRequest_Stop{Stop: &agentpbv2.RecordROS2BagRequest_RecordStop{}},
				}); serr != nil {
					return ros2RPCError(serr)
				}
				_ = stream.CloseSend()
				select {
				case msg := <-recvCh:
					if msg.GetState() == agentpbv2.RecordROS2BagResponse_STATE_ERROR {
						return fmt.Errorf("recording failed: %s", msg.GetMessage())
					}
					cliSuccess("Recording stopped. Download it with: wendy device ros2 bag download %s", msg.GetBagName())
				case rerr := <-recvErr:
					if rerr != io.EOF {
						return ros2RPCError(rerr)
					}
				}
				return nil
			case msg := <-recvCh:
				if msg.GetState() == agentpbv2.RecordROS2BagResponse_STATE_ERROR {
					return fmt.Errorf("recording failed: %s", msg.GetMessage())
				}
				cliSuccess("Recording finished: %s", msg.GetBagName())
				return nil
			case rerr := <-recvErr:
				return ros2RPCError(rerr)
			}
		},
	}
	ros2DomainFlag(cmd, &domain)
	cmd.Flags().StringVar(&output, "output", "", "Bag name on the device (default: auto-named with timestamp)")
	return cmd
}

func newROS2BagListCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List bags recorded on the device",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			client, err := newROS2Client(cmd.Context())
			if err != nil {
				return err
			}
			defer client.Close()

			rpcCtx, rpcCancel := ros2Ctx(cmd.Context(), ros2UnaryTimeout)
			defer rpcCancel()
			resp, err := client.client.ListBags(rpcCtx, &agentpbv2.ListROS2BagsRequest{})
			if err != nil {
				return ros2RPCError(err)
			}
			if jsonOutput {
				type bag struct {
					Name            string  `json:"name"`
					SizeBytes       int64   `json:"sizeBytes"`
					CreatedUnix     int64   `json:"createdUnix"`
					DurationSeconds float64 `json:"durationSeconds"`
				}
				bags := make([]bag, 0, len(resp.GetBags()))
				for _, b := range resp.GetBags() {
					bags = append(bags, bag{Name: b.GetName(), SizeBytes: b.GetSizeBytes(), CreatedUnix: b.GetCreatedUnix(), DurationSeconds: b.GetDurationSeconds()})
				}
				return printROS2JSON(bags)
			}
			if len(resp.GetBags()) == 0 {
				cliNotice("No bags recorded on the device.")
				return nil
			}
			w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
			fmt.Fprintln(w, "NAME\tSIZE\tDURATION")
			for _, b := range resp.GetBags() {
				duration := "-"
				if b.GetDurationSeconds() > 0 {
					duration = fmt.Sprintf("%.1fs", b.GetDurationSeconds())
				}
				fmt.Fprintf(w, "%s\t%s\t%s\n", b.GetName(), formatBytes(b.GetSizeBytes()), duration)
			}
			return w.Flush()
		},
	}
}

// bagRecvStream is the minimal interface consumed by downloadAndExtractBag,
// satisfied by the gRPC grpc.ServerStreamingClient[agentpbv2.ROS2BagChunk]
// returned by client.DownloadBag.
type bagRecvStream interface {
	Recv() (*agentpbv2.ROS2BagChunk, error)
}

// chunkResult carries the result of a single stream.Recv call.
type chunkResult struct {
	chunk *agentpbv2.ROS2BagChunk
	err   error
}

// downloadAndExtractBag pumps chunks from stream into a pipe, extracts the
// resulting tar archive to a temporary directory inside dest, then atomically
// renames it into place. If extraction fails the pump goroutine is unblocked
// via pr.CloseWithError and the context cancellation, and the temporary
// directory is removed.
func downloadAndExtractBag(ctx context.Context, stream bagRecvStream, dest string) error {
	// Use a child context so we can signal the pump goroutine on extract error.
	dlCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	pr, pw := io.Pipe()

	go func() {
		for {
			// Run Recv in its own goroutine so we can also select on dlCtx.Done,
			// allowing the extract-error path to unblock a streaming Recv call that
			// may not itself inspect the parent context (e.g. in tests or when the
			// underlying transport is slow to react to cancellation).
			recv := make(chan chunkResult, 1)
			go func() {
				chunk, err := stream.Recv()
				recv <- chunkResult{chunk, err}
			}()

			select {
			case <-dlCtx.Done():
				pw.CloseWithError(dlCtx.Err())
				return
			case r := <-recv:
				if r.err == io.EOF {
					pw.Close()
					return
				}
				if r.err != nil {
					pw.CloseWithError(r.err)
					return
				}
				if _, werr := pw.Write(r.chunk.GetData()); werr != nil {
					pw.CloseWithError(werr)
					return
				}
			}
		}
	}()

	// Extract to a temp directory inside dest so os.Rename is on the same FS.
	tmpDir, err := os.MkdirTemp(dest, ".bag-download-*")
	if err != nil {
		pr.CloseWithError(err)
		cancel()
		return err
	}

	if err := extractROS2BagArchive(pr, tmpDir); err != nil {
		// Unblock the pump goroutine and clean up the partial temp dir.
		pr.CloseWithError(err)
		cancel()
		_ = os.RemoveAll(tmpDir)
		return err
	}

	// Find the single top-level entry (bag directory) written by the extract.
	entries, err := os.ReadDir(tmpDir)
	if err != nil {
		_ = os.RemoveAll(tmpDir)
		return fmt.Errorf("reading temp extract dir: %w", err)
	}
	if len(entries) != 1 {
		_ = os.RemoveAll(tmpDir)
		return fmt.Errorf("bag archive must contain exactly one top-level directory, got %d entries", len(entries))
	}
	bagName := entries[0].Name()
	finalPath := filepath.Join(dest, bagName)

	// Atomic rename: tmpDir/bagName → dest/bagName.
	if err := os.Rename(filepath.Join(tmpDir, bagName), finalPath); err != nil {
		_ = os.RemoveAll(tmpDir)
		return fmt.Errorf("renaming bag into place: %w", err)
	}
	_ = os.Remove(tmpDir) // remove now-empty temp wrapper dir; ignore error
	return nil
}

func newROS2BagDownloadCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "download [name] [dest]",
		Short: "Download a bag from the device to the local machine",
		Long: `Download a bag from the device to the local machine.

With no arguments, lists the bags on the device and lets you pick one
interactively.`,
		Args: cobra.MaximumNArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) == 0 && !isInteractiveTerminal() {
				return errors.New("missing bag name (interactive selection needs a terminal); run `wendy device ros2 bag list`, then `wendy device ros2 bag download <name>`")
			}

			dest := "."
			if len(args) == 2 {
				dest = args[1]
			}

			client, err := newROS2Client(cmd.Context())
			if err != nil {
				return err
			}
			defer client.Close()

			name := ""
			if len(args) > 0 {
				name = args[0]
			} else {
				name, err = pickROS2Bag(cmd.Context(), client)
				if err != nil {
					return err
				}
			}

			dlCtx, cancel := context.WithCancel(cmd.Context())
			defer cancel()

			stream, err := client.client.DownloadBag(dlCtx, &agentpbv2.DownloadROS2BagRequest{Name: name})
			if err != nil {
				return ros2RPCError(err)
			}

			if err := downloadAndExtractBag(dlCtx, stream, dest); err != nil {
				return ros2RPCError(err)
			}
			cliSuccess("Downloaded bag %q to %s", name, filepath.Join(dest, name))
			return nil
		},
	}
}

// pickROS2Bag lists the bags recorded on the device and asks the user to
// select one interactively, returning its name.
func pickROS2Bag(ctx context.Context, client *ros2Client) (string, error) {
	resp, err := client.client.ListBags(ctx, &agentpbv2.ListROS2BagsRequest{})
	if err != nil {
		return "", ros2RPCError(err)
	}
	bags := resp.GetBags()
	if len(bags) == 0 {
		return "", errors.New("no bags recorded on the device; record one with `wendy device ros2 bag record`")
	}

	items := make([]tui.PickerItem, 0, len(bags))
	for _, b := range bags {
		duration := "-"
		if b.GetDurationSeconds() > 0 {
			duration = fmt.Sprintf("%.1fs", b.GetDurationSeconds())
		}
		created := "-"
		if b.GetCreatedUnix() > 0 {
			created = time.Unix(b.GetCreatedUnix(), 0).Format("2006-01-02 15:04:05")
		}
		items = append(items, tui.PickerItem{
			Name:       b.GetName(),
			Size:       formatBytes(b.GetSizeBytes()),
			Parameters: duration,
			Comments:   created,
			// Newest bag first: invert the creation time so the picker's
			// ascending SortKey order shows the most recent recording on top.
			SortKey: fmt.Sprintf("%020d", int64(1)<<62-b.GetCreatedUnix()),
			Value:   b.GetName(),
		})
	}
	return pickFromItemsWithColumns("Select a bag to download", items, []tui.PickerColumn{
		{Title: "Name", MinWidth: 18, Required: true, Value: func(i tui.PickerItem) string { return i.Name }},
		{Title: "Size", MinWidth: 8, Value: func(i tui.PickerItem) string { return i.Size }},
		{Title: "Duration", MinWidth: 10, Value: func(i tui.PickerItem) string { return i.Parameters }},
		{Title: "Created", MinWidth: 12, Value: func(i tui.PickerItem) string { return i.Comments }},
	})
}

// extractROS2BagArchive unpacks the tar stream produced by DownloadBag into
// dest, rejecting entries that would escape it (SOC2-CC6, NIST-SI-10).
func extractROS2BagArchive(r io.Reader, dest string) error {
	tr := tar.NewReader(r)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return fmt.Errorf("reading bag archive: %w", err)
		}
		name := filepath.Clean(hdr.Name)
		if name == "" || filepath.IsAbs(name) || strings.HasPrefix(name, "..") {
			return fmt.Errorf("bag archive contains unsafe path %q", hdr.Name)
		}
		path := filepath.Join(dest, name)
		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(path, 0o755); err != nil {
				return err
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
				return err
			}
			f, err := os.OpenFile(path, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o644)
			if err != nil {
				return err
			}
			if _, err := io.Copy(f, tr); err != nil {
				if closeErr := f.Close(); closeErr != nil {
					return errors.Join(err, closeErr)
				}
				return err
			}
			if err := f.Close(); err != nil {
				return err
			}
		default:
			// Skip symlinks and special files; bags only contain regular files.
		}
	}
}

// ── escape hatch ────────────────────────────────────────────────────

// stripWendyExecGlobals scans args (the post-positional slice captured by
// SetInterspersed(false)) for --device and --json flags that belong to wendy,
// removes them from the slice, and returns their values alongside the
// remaining args that should be forwarded to the remote ros2 process.
//
// Because SetInterspersed(false) stops cobra's flag parser at the first
// positional, any --device/--json placed after the ros2 command word lands in
// args rather than being parsed by the persistent root flags.  This function
// peels those flags out so they can select the target device without being
// forwarded verbatim to ros2 (which would reject them as unknown flags).
//
// Only the exact forms --device <value>, --device=<value>, --json, and
// --json=<ignored> are recognised; anything else is left in the forwarded
// slice unchanged (WDY-1553 passthrough).
func stripWendyExecGlobals(args []string) (device string, jsonFlag bool, forwarded []string) {
	forwarded = make([]string, 0, len(args))
	for i := 0; i < len(args); i++ {
		switch {
		case args[i] == "--":
			// Honor the -- escape: forward the rest verbatim
			forwarded = append(forwarded, args[i:]...)
			return
		case strings.HasPrefix(args[i], "--device="):
			device = strings.TrimPrefix(args[i], "--device=")
		case args[i] == "--device":
			if i+1 < len(args) {
				device = args[i+1]
				i++ // consume the value
			} else {
				forwarded = append(forwarded, args[i]) // no value — don't silently drop
			}
		case args[i] == "--json" || strings.HasPrefix(args[i], "--json="):
			jsonFlag = true
		default:
			forwarded = append(forwarded, args[i])
		}
	}
	return device, jsonFlag, forwarded
}

// execRecvStream is the minimal interface consumed by drainExecStream, matching
// grpc.ServerStreamingClient[agentpbv2.ROS2ExecOutput].
type execRecvStream interface {
	Recv() (*agentpbv2.ROS2ExecOutput, error)
}

// drainExecStream reads all messages from stream until io.EOF, forwarding
// stdout chunks to stdout and stderr chunks to stderr.  It returns an error if:
//   - the stream closes without a terminal ExitCode frame ("stream ended before
//     exit status"), which indicates a truncated or aborted stream; or
//   - the last ExitCode frame reports a non-zero code.
//
// It never early-returns on a non-zero code — trailing output is drained first.
// If ctx is cancelled (e.g. ctrl-c) and Recv returns any error, drainExecStream
// returns nil — the same clean-stop behaviour as EchoTopic and MonitorHz.
func drainExecStream(ctx context.Context, stream execRecvStream, args []string, stdout, stderr io.Writer) error {
	var (
		sawExitFrame bool
		lastCode     int32
	)
	for {
		msg, rerr := stream.Recv()
		if rerr != nil {
			if ctx.Err() != nil {
				return nil // user cancelled (ctrl-c) — clean stop, like echo/hz
			}
			if rerr == io.EOF {
				break // normal end — evaluate the exit-frame contract below
			}
			return ros2RPCError(rerr)
		}
		if len(msg.GetStdout()) > 0 {
			stdout.Write(msg.GetStdout())
		}
		if len(msg.GetStderr()) > 0 {
			stderr.Write(msg.GetStderr())
		}
		if msg.ExitCode != nil {
			sawExitFrame = true
			lastCode = *msg.ExitCode
		}
	}
	if !sawExitFrame {
		return fmt.Errorf("ros2 %s: stream ended before exit status", strings.Join(args, " "))
	}
	if lastCode != 0 {
		return fmt.Errorf("ros2 %s exited with code %d", strings.Join(args, " "), lastCode)
	}
	return nil
}

func newROS2ExecCmd() *cobra.Command {
	var domain int32
	cmd := &cobra.Command{
		Use:   "exec <args>...",
		Short: "Run a raw ros2 CLI command on the device",
		Long: `Run a raw ros2 CLI command inside the device's ROS 2 sidecar.

Everything after the ros2 command word is forwarded verbatim, so --flags
work without escaping. Any wendy flags (--device, --domain) must come before
the ros2 command; use -- to force the rest of the line through unparsed.`,
		Example: `  wendy device ros2 exec topic echo /chatter --once
  wendy device ros2 exec topic pub /cmd_vel geometry_msgs/msg/Twist --rate 10
  wendy device ros2 exec --domain 5 node list`,
		Args: cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
			defer stop()

			// SetInterspersed(false) stops cobra's flag parser at the first
			// positional, so --device/--json placed after the ros2 command word
			// (e.g. `exec node info /talker --device host.local`) land in args
			// instead of being parsed by the root persistent flags.  Strip them
			// here and propagate into the package globals before resolveTarget
			// runs inside newROS2Client (WDY-1707).
			localDevice, localJSON, fwdArgs := stripWendyExecGlobals(args)
			if localDevice != "" {
				deviceFlag = localDevice
			}
			if localJSON {
				jsonOutput = true
			}

			client, err := newROS2Client(ctx)
			if err != nil {
				return err
			}
			defer client.Close()

			stream, err := client.client.Exec(ctx, &agentpbv2.ROS2ExecRequest{
				DomainId: ros2DomainPtr(domain),
				Args:     fwdArgs,
			})
			if err != nil {
				return ros2RPCError(err)
			}
			return drainExecStream(ctx, stream, fwdArgs, os.Stdout, os.Stderr)
		},
	}
	ros2DomainFlag(cmd, &domain)
	// Stop flag parsing at the first positional (the ros2 command word) so that
	// --flags meant for ros2 (e.g. `topic echo /chatter --once`) forward verbatim
	// instead of being rejected as unknown flags of this command (WDY-1553).
	cmd.Flags().SetInterspersed(false)
	return cmd
}
