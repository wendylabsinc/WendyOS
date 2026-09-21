package services

import (
	"archive/tar"
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"go.uber.org/zap"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	"github.com/wendylabsinc/wendy/go/internal/shared/appconfig"
	"github.com/wendylabsinc/wendy/go/internal/shared/ros2inspection"
	agentpbv2 "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
)

// ros2BagNamePattern restricts bag names to a single safe path segment so a
// crafted name can never escape the bag directory (SOC2-CC6, ISO27001-A.8,
// NIST-SI-10).
var ros2BagNamePattern = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._-]*$`)

// ros2BagChunkSize is the payload size for DownloadBag stream messages.
const ros2BagChunkSize = 64 * 1024

// Scanner token caps for the two streaming commands. Exceeding one is reported as
// an error rather than an end-of-stream — see ros2ScanError.
const (
	// ros2EchoMaxMessageBytes bounds one `ros2 topic echo` YAML document. Large
	// enough for a VGA Image or a modest PointCloud2; a full-res point cloud will
	// exceed it, which is now an actionable error instead of silence.
	ros2EchoMaxMessageBytes = 4 * 1024 * 1024
	// ros2HzMaxLineBytes bounds one `ros2 topic hz` output line. The lines are
	// tiny; the headroom is for a node that logs to stdout.
	ros2HzMaxLineBytes = 1024 * 1024
)

// ros2ScanError turns a bufio.Scanner failure into a gRPC status. bufio.ErrTooLong
// is the interesting case and gets ResourceExhausted with the cap named, because
// the caller's only recourse is to echo a smaller topic (or use a field selector).
func ros2ScanError(command, topic string, err error, cap int) error {
	if errors.Is(err, bufio.ErrTooLong) {
		return status.Errorf(codes.ResourceExhausted,
			"ros2 %s %s: a single message exceeded the %d MiB streaming limit; "+
				"echo a smaller topic or narrow it with a field selector",
			command, topic, cap/(1024*1024))
	}
	return status.Errorf(codes.Internal, "ros2 %s %s: reading output: %v", command, topic, err)
}

// ROS2Service implements agentpbv2.ROS2ServiceServer by exec-ing `ros2`
// commands inside the CLI sidecar managed by the ROS2Runtime (WDY-1332).
type ROS2Service struct {
	agentpbv2.UnimplementedROS2ServiceServer
	logger  *zap.Logger
	runtime ROS2Runtime
	// bagDir is the host directory holding rosbag2 recordings. Variable for
	// tests; defaults to the containerd package's ROS2BagDir.
	bagDir string
}

// NewROS2Service creates a new ROS2Service backed by the given runtime.
// bagDir is the host directory where bag recordings are stored.
func NewROS2Service(logger *zap.Logger, runtime ROS2Runtime, bagDir string) *ROS2Service {
	return &ROS2Service{logger: logger, runtime: runtime, bagDir: bagDir}
}

// ros2SC is a resolved per-RMW sidecar plus the DDS domain to use for a call.
type ros2SC struct {
	name     string
	rmw      string
	domainID int
}

// resolveSidecars ensures one sidecar per running RMW (WDY-1594) and returns
// them with the effective domain: the --domain override when set, else each
// sidecar's own default. Unscoped requests fall back to the system graph when no
// ROS 2 app is running. Discovery commands run in all and merge; targeted commands
// route to one.
func (s *ROS2Service) resolveSidecars(ctx context.Context, override *int32) ([]ros2SC, error) {
	scope, scopeErr := requestedROS2Scope(ctx)
	if scopeErr != nil {
		return nil, scopeErr
	}
	if scope == ros2inspection.HostScope {
		return nil, status.Error(codes.InvalidArgument, "host scope is restricted to topic listing, endpoint information, samples and rates")
	}
	ovr := -1
	if override != nil {
		id := int(*override)
		if id < appconfig.ROS2DomainIDMin || id > appconfig.ROS2DomainIDMax {
			return nil, status.Errorf(codes.InvalidArgument, "domain ID %d out of range [%d,%d]", id, appconfig.ROS2DomainIDMin, appconfig.ROS2DomainIDMax)
		}
		ovr = id
	}
	sidecars, err := s.runtime.EnsureROS2Sidecars(ctx)
	if errors.Is(err, ErrNoRunningROS2Containers) {
		md, _ := metadata.FromIncomingContext(ctx)
		_, explicitScope := md[ros2inspection.ScopeMetadata]
		if runtime, ok := s.runtime.(ROS2SystemRuntime); ok && !explicitScope {
			var sidecar ROS2Sidecar
			sidecar, err = runtime.EnsureSystemROS2Sidecar(ctx)
			if err == nil {
				sidecars = []ROS2Sidecar{sidecar}
			}
		}
	}
	if err != nil {
		return nil, status.Error(codes.FailedPrecondition, err.Error())
	}
	out := make([]ros2SC, 0, len(sidecars))
	for _, sc := range sidecars {
		d := sc.DomainID
		if ovr >= 0 {
			d = ovr
		}
		out = append(out, ros2SC{name: sc.Name, rmw: sc.RMW, domainID: d})
	}
	// Sort by sidecar name so `scs[0]` — the fallback for commands that cannot be
	// RMW-routed (raw Exec, `bag record -a`) — is actually deterministic. It used
	// to inherit containerd's Containers() ordering, which containerd does not
	// specify; in practice it is a bolt bucket walk, so the previous "first-running
	// RMW deterministically wins" claim held by accident rather than by contract.
	sort.Slice(out, func(i, j int) bool { return out[i].name < out[j].name })
	return out, nil
}

// runIn executes `ros2 <args>` in a specific sidecar and returns its stdout. A
// non-zero exit code is reported as an error carrying stderr.
func (s *ROS2Service) runIn(ctx context.Context, sc ros2SC, args ...string) (string, error) {
	var stdout, stderr bytes.Buffer
	code, err := s.runtime.ExecROS2(ctx, ROS2ExecOptions{DomainID: sc.domainID, Args: args, SidecarName: sc.name}, &stdout, &stderr)
	if err != nil {
		return "", status.Errorf(codes.Internal, "ros2 %s: %v", strings.Join(args, " "), err)
	}
	if code != 0 {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = strings.TrimSpace(stdout.String())
		}
		return "", status.Errorf(codes.Internal, "ros2 %s failed (exit %d): %s", strings.Join(args, " "), code, msg)
	}
	return stdout.String(), nil
}

// ros2Out is one sidecar's stdout for a merged command, tagged with its RMW.
type ros2Out struct {
	rmw string
	out string
}

// runMerged runs args in every sidecar and returns each one's tagged output.
// A sidecar that errors is skipped so one broken RMW graph doesn't hide the
// others; if every sidecar errors, the last error is returned.
func (s *ROS2Service) runMerged(ctx context.Context, scs []ros2SC, args ...string) ([]ros2Out, error) {
	var outs []ros2Out
	var lastErr error
	for _, sc := range scs {
		out, err := s.runIn(ctx, sc, args...)
		if err != nil {
			lastErr = err
			continue
		}
		outs = append(outs, ros2Out{rmw: sc.rmw, out: out})
	}
	if len(outs) == 0 && lastErr != nil {
		return nil, lastErr
	}
	return outs, nil
}

// pickSidecarOwning routes a target-specific command to the sidecar whose graph
// carries `target`, found by matching a line of `ros2 <listKind> list` (a
// topic/node/service lives in exactly one RMW graph). This avoids running the
// command in the wrong sidecar — where a node/service-targeted call (param get,
// service call) would block on DDS discovery until timeout before failing.
// Falls back to the first sidecar (lowest name — see resolveSidecars) when
// ownership can't be determined; if the same name exists in more than one RMW
// graph (genuinely ambiguous), the lowest-named sidecar wins.
//
// Short-circuits when there is only one sidecar. Routing costs one extra
// `ros2 <kind> list` exec per sidecar, and an exec is ~1s (container exec
// dispatch + sourcing setup.bash + a fresh rclpy process doing DDS discovery).
// On the overwhelmingly common single-RMW device that was a 100% overhead tax on
// every targeted call — GetParam, SetParam, CallService, GetTopicInfo, every
// lifecycle/component/action call, EchoTopic, MonitorHz, RecordBag — to choose
// between one candidate.
func (s *ROS2Service) pickSidecarOwning(ctx context.Context, scs []ros2SC, listKind, target string) ros2SC {
	if len(scs) == 1 {
		return scs[0]
	}
	for _, sc := range scs {
		out, err := s.runIn(ctx, sc, listKind, "list")
		if err != nil {
			continue
		}
		for _, line := range strings.Split(out, "\n") {
			if strings.TrimSpace(line) == target {
				return sc
			}
		}
	}
	return scs[0]
}

func (s *ROS2Service) pickSidecarForTopic(ctx context.Context, scs []ros2SC, topic string) ros2SC {
	return s.pickSidecarOwning(ctx, scs, "topic", topic)
}

func (s *ROS2Service) pickSidecarForNode(ctx context.Context, scs []ros2SC, node string) ros2SC {
	return s.pickSidecarOwning(ctx, scs, "node", node)
}

func (s *ROS2Service) pickSidecarForService(ctx context.Context, scs []ros2SC, service string) ros2SC {
	return s.pickSidecarOwning(ctx, scs, "service", service)
}

func (s *ROS2Service) pickSidecarForAction(ctx context.Context, scs []ros2SC, action string) ros2SC {
	return s.pickSidecarOwning(ctx, scs, "action", action)
}

func (s *ROS2Service) pickSidecarForComponent(ctx context.Context, scs []ros2SC, container string) ros2SC {
	return s.pickSidecarOwning(ctx, scs, "component", container)
}

func (s *ROS2Service) ListNodes(ctx context.Context, req *agentpbv2.ListROS2NodesRequest) (*agentpbv2.ListROS2NodesResponse, error) {
	scs, err := s.resolveSidecars(ctx, req.DomainId)
	if err != nil {
		return nil, err
	}
	outs, err := s.runMerged(ctx, scs, "node", "list")
	if err != nil {
		return nil, err
	}
	resp := &agentpbv2.ListROS2NodesResponse{}
	seen := map[string]bool{}
	for _, o := range outs {
		for _, n := range parseROS2NodeList(o.out) {
			n.Rmw = o.rmw
			// ros2NodeFQN, not manual concatenation: the hand-rolled version
			// produced "//talker" for a root-namespace node while every other
			// dedup site used ros2NodeFQN's "/talker". Harmless as long as it was
			// only ever a map key, but two spellings of the same identity is how
			// a real dedup bug gets introduced later.
			key := ros2NodeFQN(n) + "\x00" + o.rmw
			if seen[key] {
				continue
			}
			seen[key] = true
			resp.Nodes = append(resp.Nodes, n)
		}
	}
	return resp, nil
}

func (s *ROS2Service) ListTopics(ctx context.Context, req *agentpbv2.ListROS2TopicsRequest) (*agentpbv2.ListROS2TopicsResponse, error) {
	scs, err := s.resolveInspectionSidecars(ctx, req.DomainId)
	if err != nil {
		return nil, err
	}
	resp := &agentpbv2.ListROS2TopicsResponse{}
	var lastErr error
	any := false
	for _, sc := range scs {
		out, rerr := s.runIn(ctx, sc, "topic", "list", "-t")
		if rerr != nil {
			lastErr = rerr
			continue
		}
		any = true
		topics := parseROS2TopicList(out)
		for _, t := range topics {
			t.Rmw = sc.rmw
		}
		if req.GetIncludeCounts() {
			s.fillROS2TopicCounts(ctx, sc, topics)
		}
		resp.Topics = append(resp.Topics, topics...)
	}
	if !any && lastErr != nil {
		return nil, lastErr
	}
	return resp, nil
}

// ros2TopicInfoConcurrency bounds how many `ros2 topic info` execs
// fillROS2TopicCounts runs at once. Each exec pays a fixed cost (container
// exec dispatch + sourcing setup.bash + a fresh rclpy process doing DDS
// discovery, roughly ~1s), so fetching counts one topic at a time turns
// `--all` into a multi-minute wait on devices with many topics. Kept well
// under the containerd client's per-sidecar cap (ros2MaxConcurrentExecs=16)
// so this fan-out can't starve other concurrent ROS 2 RPCs on the sidecar.
const ros2TopicInfoConcurrency = 8

// fillROS2TopicCounts fetches publisher/subscriber counts for each topic
// concurrently and fills them in in place. A topic whose `topic info` exec
// fails is left with zero counts, matching the prior sequential behavior.
func (s *ROS2Service) fillROS2TopicCounts(ctx context.Context, sc ros2SC, topics []*agentpbv2.ROS2Topic) {
	sem := make(chan struct{}, ros2TopicInfoConcurrency)
	var wg sync.WaitGroup
LOOP:
	for _, t := range topics {
		select {
		case sem <- struct{}{}:
		case <-ctx.Done():
			break LOOP
		}
		wg.Add(1)
		go func(t *agentpbv2.ROS2Topic) {
			defer wg.Done()
			defer func() { <-sem }()
			info, ierr := s.runIn(ctx, sc, "topic", "info", t.GetName())
			if ierr != nil {
				return
			}
			_, pubs, subs := parseROS2TopicInfo(info)
			t.PublisherCount = pubs
			t.SubscriberCount = subs
		}(t)
	}
	wg.Wait()
}

func (s *ROS2Service) GetTopicInfo(ctx context.Context, req *agentpbv2.GetROS2TopicInfoRequest) (*agentpbv2.GetROS2TopicInfoResponse, error) {
	scs, err := s.resolveInspectionSidecars(ctx, req.DomainId)
	if err != nil {
		return nil, err
	}
	if err := validateROS2GraphName(req.GetTopic()); err != nil {
		return nil, err
	}
	sc := s.pickSidecarForTopic(ctx, scs, req.GetTopic())
	out, err := s.runIn(ctx, sc, "topic", "info", "-v", req.GetTopic())
	if err != nil {
		return nil, err
	}
	types, pubs, subs := parseROS2TopicInfo(out)
	return &agentpbv2.GetROS2TopicInfoResponse{
		Topic: &agentpbv2.ROS2Topic{
			Name:            req.GetTopic(),
			Types:           types,
			PublisherCount:  pubs,
			SubscriberCount: subs,
			Rmw:             sc.rmw,
		},
		Verbose: out,
	}, nil
}

func (s *ROS2Service) ListServices(ctx context.Context, req *agentpbv2.ListROS2ServicesRequest) (*agentpbv2.ListROS2ServicesResponse, error) {
	scs, err := s.resolveSidecars(ctx, req.DomainId)
	if err != nil {
		return nil, err
	}
	outs, err := s.runMerged(ctx, scs, "service", "list", "-t")
	if err != nil {
		return nil, err
	}
	resp := &agentpbv2.ListROS2ServicesResponse{}
	seen := map[string]bool{}
	for _, o := range outs {
		for _, t := range parseROS2TopicList(o.out) {
			key := t.GetName() + "\x00" + o.rmw
			if seen[key] {
				continue
			}
			seen[key] = true
			resp.Services = append(resp.Services, &agentpbv2.ListROS2ServicesResponse_Service{
				Name:  t.GetName(),
				Types: t.GetTypes(),
				Rmw:   o.rmw,
			})
		}
	}
	return resp, nil
}

func (s *ROS2Service) ListParams(ctx context.Context, req *agentpbv2.ListROS2ParamsRequest) (*agentpbv2.ListROS2ParamsResponse, error) {
	scs, err := s.resolveSidecars(ctx, req.DomainId)
	if err != nil {
		return nil, err
	}
	args := []string{"param", "list"}
	if req.GetNode() != "" {
		if err := validateROS2GraphName(req.GetNode()); err != nil {
			return nil, err
		}
		args = append(args, req.GetNode())
	}
	resp := &agentpbv2.ListROS2ParamsResponse{}
	if req.GetNode() != "" {
		// Node-targeted: it lives in one RMW graph; route to the sidecar that has it.
		sc := s.pickSidecarForNode(ctx, scs, req.GetNode())
		out, err := s.runIn(ctx, sc, args...)
		if err != nil {
			return nil, err
		}
		resp.Nodes = parseROS2ParamList(out, req.GetNode())
		return resp, nil
	}
	// All-nodes: merge across every RMW graph.
	outs, err := s.runMerged(ctx, scs, args...)
	if err != nil {
		return nil, err
	}
	for _, o := range outs {
		resp.Nodes = append(resp.Nodes, parseROS2ParamList(o.out, "")...)
	}
	return resp, nil
}

func (s *ROS2Service) GetParam(ctx context.Context, req *agentpbv2.GetROS2ParamRequest) (*agentpbv2.GetROS2ParamResponse, error) {
	scs, err := s.resolveSidecars(ctx, req.DomainId)
	if err != nil {
		return nil, err
	}
	if err := validateROS2GraphName(req.GetNode()); err != nil {
		return nil, err
	}
	if err := validateROS2ParamName(req.GetParam()); err != nil {
		return nil, err
	}
	sc := s.pickSidecarForNode(ctx, scs, req.GetNode())
	out, err := s.runIn(ctx, sc, "param", "get", req.GetNode(), req.GetParam())
	if err != nil {
		return nil, err
	}
	return &agentpbv2.GetROS2ParamResponse{Value: strings.TrimSpace(out)}, nil
}

func (s *ROS2Service) SetParam(ctx context.Context, req *agentpbv2.SetROS2ParamRequest) (*agentpbv2.SetROS2ParamResponse, error) {
	scs, err := s.resolveSidecars(ctx, req.DomainId)
	if err != nil {
		return nil, err
	}
	if err := validateROS2GraphName(req.GetNode()); err != nil {
		return nil, err
	}
	if err := validateROS2ParamName(req.GetParam()); err != nil {
		return nil, err
	}
	sc := s.pickSidecarForNode(ctx, scs, req.GetNode())
	out, err := s.runIn(ctx, sc, "param", "set", req.GetNode(), req.GetParam(), req.GetValue())
	if err != nil {
		// `ros2 param set` reports failures both via exit code and text.
		return &agentpbv2.SetROS2ParamResponse{Success: false, Message: err.Error()}, nil
	}
	return &agentpbv2.SetROS2ParamResponse{Success: true, Message: strings.TrimSpace(out)}, nil
}

func (s *ROS2Service) CallService(ctx context.Context, req *agentpbv2.CallROS2ServiceRequest) (*agentpbv2.CallROS2ServiceResponse, error) {
	scs, err := s.resolveSidecars(ctx, req.DomainId)
	if err != nil {
		return nil, err
	}
	if err := validateROS2GraphName(req.GetService()); err != nil {
		return nil, err
	}
	args := []string{"service", "call", req.GetService(), req.GetType()}
	if req.GetRequest() != "" {
		args = append(args, req.GetRequest())
	}
	sc := s.pickSidecarForService(ctx, scs, req.GetService())
	out, err := s.runIn(ctx, sc, args...)
	if err != nil {
		return &agentpbv2.CallROS2ServiceResponse{Success: false, Response: err.Error()}, nil
	}
	return &agentpbv2.CallROS2ServiceResponse{Success: true, Response: strings.TrimSpace(out)}, nil
}

func (s *ROS2Service) GetGraph(ctx context.Context, req *agentpbv2.GetROS2GraphRequest) (*agentpbv2.GetROS2GraphResponse, error) {
	scs, err := s.resolveSidecars(ctx, req.DomainId)
	if err != nil {
		return nil, err
	}
	resp := &agentpbv2.GetROS2GraphResponse{}
	var lastErr error
	any := false
	seenNodes := map[string]bool{}
	seenEdges := map[string]bool{}
	for _, sc := range scs {
		out, rerr := s.runIn(ctx, sc, "node", "list")
		if rerr != nil {
			lastErr = rerr
			continue
		}
		any = true
		nodes := parseROS2NodeList(out)
		for _, node := range nodes {
			node.Rmw = sc.rmw
			nodeKey := ros2NodeFQN(node) + "\x00" + sc.rmw
			if !seenNodes[nodeKey] {
				seenNodes[nodeKey] = true
				resp.Nodes = append(resp.Nodes, node)
			}
		}
		// `node info` per node used to run serially. Each exec is ~1s (see
		// ros2TopicInfoConcurrency), so a 40-node robot took 40s+ to answer one
		// `wendy device ros2 graph`. Fan out with the same bound ListTopics uses,
		// then fold results in listing order so the response stays deterministic.
		for _, r := range s.fetchROS2NodeInfo(ctx, sc, nodes) {
			if !r.ok {
				continue // node may have exited between list and info
			}
			for _, topic := range r.publishes {
				edgeKey := r.fqn + "\x00" + topic + "\x00" + sc.rmw + "\x00pub"
				if !seenEdges[edgeKey] {
					seenEdges[edgeKey] = true
					resp.Publishes = append(resp.Publishes, &agentpbv2.GetROS2GraphResponse_Edge{Node: r.fqn, Topic: topic, Rmw: sc.rmw})
				}
			}
			for _, topic := range r.subscribes {
				edgeKey := r.fqn + "\x00" + topic + "\x00" + sc.rmw + "\x00sub"
				if !seenEdges[edgeKey] {
					seenEdges[edgeKey] = true
					resp.Subscribes = append(resp.Subscribes, &agentpbv2.GetROS2GraphResponse_Edge{Node: r.fqn, Topic: topic, Rmw: sc.rmw})
				}
			}
		}
	}
	if !any && lastErr != nil {
		return nil, lastErr
	}
	return resp, nil
}

// ros2NodeInfoResult is one node's parsed `node info`, carrying its index so the
// concurrent fan-out can be folded back in deterministic listing order.
type ros2NodeInfoResult struct {
	fqn        string
	publishes  []string
	subscribes []string
	ok         bool
}

// fetchROS2NodeInfo runs `ros2 node info <fqn>` for every node concurrently,
// bounded by ros2TopicInfoConcurrency, and returns results in `nodes` order.
func (s *ROS2Service) fetchROS2NodeInfo(ctx context.Context, sc ros2SC, nodes []*agentpbv2.ROS2Node) []ros2NodeInfoResult {
	results := make([]ros2NodeInfoResult, len(nodes))
	sem := make(chan struct{}, ros2TopicInfoConcurrency)
	var wg sync.WaitGroup
LOOP:
	for i, node := range nodes {
		select {
		case sem <- struct{}{}:
		case <-ctx.Done():
			break LOOP
		}
		wg.Add(1)
		go func(i int, fqn string) {
			defer wg.Done()
			defer func() { <-sem }()
			results[i].fqn = fqn
			info, ierr := s.runIn(ctx, sc, "node", "info", fqn)
			if ierr != nil {
				return
			}
			results[i].publishes, results[i].subscribes = parseROS2NodeInfo(info)
			results[i].ok = true
		}(i, ros2NodeFQN(node))
	}
	wg.Wait()
	return results
}

func (s *ROS2Service) Doctor(ctx context.Context, req *agentpbv2.ROS2DoctorRequest) (*agentpbv2.ROS2DoctorResponse, error) {
	scs, err := s.resolveSidecars(ctx, req.DomainId)
	if err != nil {
		return nil, err
	}
	// One report per RMW graph, each headed by its RMW when more than one runs.
	var sb strings.Builder
	var lastErr error
	any := false
	for _, sc := range scs {
		// `ros2 doctor` exits non-zero when checks fail, but the report is still
		// the useful output — capture both streams and return whatever we got. A
		// sidecar whose exec genuinely fails is skipped so one broken RMW graph
		// doesn't hide the others (matches the other merge commands).
		var stdout, stderr bytes.Buffer
		_, execErr := s.runtime.ExecROS2(ctx, ROS2ExecOptions{DomainID: sc.domainID, Args: []string{"doctor", "--report"}, SidecarName: sc.name}, &stdout, &stderr)
		if execErr != nil {
			lastErr = status.Errorf(codes.Internal, "ros2 doctor: %v", execErr)
			continue
		}
		any = true
		report := stdout.String()
		if strings.TrimSpace(report) == "" {
			report = stderr.String()
		}
		if len(scs) > 1 {
			label := sc.rmw
			if label == "" {
				label = "default"
			}
			sb.WriteString("=== RMW: " + label + " ===\n")
		}
		sb.WriteString(report)
		if !strings.HasSuffix(report, "\n") {
			sb.WriteString("\n")
		}
	}
	if !any && lastErr != nil {
		return nil, lastErr
	}
	return &agentpbv2.ROS2DoctorResponse{Report: sb.String()}, nil
}

func (s *ROS2Service) EchoTopic(req *agentpbv2.EchoROS2TopicRequest, stream grpc.ServerStreamingServer[agentpbv2.ROS2Message]) error {
	ctx := stream.Context()
	if opts, err := requestedLidarOptions(ctx, req); err != nil {
		return err
	} else if opts != nil {
		return s.echoLidar(req, stream, opts)
	}
	scs, err := s.resolveInspectionSidecars(ctx, req.DomainId)
	if err != nil {
		return err
	}
	if err := validateROS2GraphName(req.GetTopic()); err != nil {
		return err
	}
	sc := s.pickSidecarForTopic(ctx, scs, req.GetTopic())

	execCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	pr, pw := io.Pipe()
	execDone := make(chan error, 1)
	go func() {
		stderr := &ros2StderrTail{}
		code, execErr := s.runtime.ExecROS2(execCtx, ROS2ExecOptions{
			DomainID:    sc.domainID,
			SidecarName: sc.name,
			Args:        []string{"topic", "echo", req.GetTopic()},
		}, pw, stderr)
		execErr = ros2StreamExitError(code, execErr, stderr)
		pw.CloseWithError(execErr)
		execDone <- execErr
	}()

	// Release the exec goroutine and wait for it. Cancelling alone is NOT enough:
	// the goroutine can be blocked mid-Write into pw, so the read end has to be
	// drained and closed or <-execDone wedges forever (WDY-1698). Every exit path
	// goes through here — including the scanner-error path, which previously did
	// not and deadlocked the whole RPC (holding one of the sidecar's 16 exec slots
	// indefinitely) whenever a message exceeded the token cap.
	drainAndWait := func() error {
		cancel()
		go func() { _, _ = io.Copy(io.Discard, pr) }()
		pr.CloseWithError(context.Canceled)
		return <-execDone
	}

	// `ros2 topic echo` separates YAML documents with bare "---" lines.
	var doc strings.Builder
	sent := int32(0)
	scanner := bufio.NewScanner(pr)
	scanner.Buffer(make([]byte, 0, 64*1024), ros2EchoMaxMessageBytes)
	for scanner.Scan() {
		line := scanner.Text()
		if strings.TrimSpace(line) != "---" {
			// Scanner bounds each line, but an array may span many lines.
			// Bound the complete document before retaining another line.
			if len(line)+1 > ros2EchoMaxMessageBytes-doc.Len() {
				_ = drainAndWait()
				return ros2ScanError("topic echo", req.GetTopic(), bufio.ErrTooLong, ros2EchoMaxMessageBytes)
			}
			doc.WriteString(line)
			doc.WriteString("\n")
			continue
		}
		if strings.TrimSpace(doc.String()) == "" {
			continue
		}
		if serr := stream.Send(&agentpbv2.ROS2Message{Topic: req.GetTopic(), Yaml: doc.String()}); serr != nil {
			_ = drainAndWait()
			return serr
		}
		doc.Reset()
		sent++
		if req.GetCount() > 0 && sent >= req.GetCount() {
			_ = drainAndWait()
			return nil
		}
	}
	// scanner.Err() must be checked before treating loop exit as end-of-stream. A
	// single YAML document over the token cap — a PointCloud2, an Image, an
	// occupancy grid — ends Scan() with bufio.ErrTooLong while execErr stays nil,
	// so this used to return nil and the client saw a clean EOF with no output and
	// no error: silent truncation on exactly the topics echo exists to inspect.
	scanErr := scanner.Err()
	execErr := drainAndWait()
	if ctx.Err() != nil {
		return nil // client cancelled; not an error
	}
	if scanErr != nil {
		return ros2ScanError("topic echo", req.GetTopic(), scanErr, ros2EchoMaxMessageBytes)
	}
	if execErr != nil {
		return status.Errorf(codes.Internal, "ros2 topic echo: %v", execErr)
	}
	return nil
}

func (s *ROS2Service) MonitorHz(req *agentpbv2.MonitorROS2HzRequest, stream grpc.ServerStreamingServer[agentpbv2.ROS2HzSample]) error {
	ctx := stream.Context()
	scs, err := s.resolveInspectionSidecars(ctx, req.DomainId)
	if err != nil {
		return err
	}
	if err := validateROS2GraphName(req.GetTopic()); err != nil {
		return err
	}
	sc := s.pickSidecarForTopic(ctx, scs, req.GetTopic())

	execCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	pr, pw := io.Pipe()
	execDone := make(chan error, 1)
	go func() {
		stderr := &ros2StderrTail{}
		code, execErr := s.runtime.ExecROS2(execCtx, ROS2ExecOptions{
			DomainID:    sc.domainID,
			SidecarName: sc.name,
			Args:        []string{"topic", "hz", req.GetTopic()},
		}, pw, stderr)
		execErr = ros2StreamExitError(code, execErr, stderr)
		pw.CloseWithError(execErr)
		execDone <- execErr
	}()

	// Same teardown contract as EchoTopic: cancel, drain, close, then wait.
	// Cancelling without draining left the exec goroutine blocked mid-Write.
	drainAndWait := func() error {
		cancel()
		go func() { _, _ = io.Copy(io.Discard, pr) }()
		pr.CloseWithError(context.Canceled)
		return <-execDone
	}

	var avgLine string
	scanner := bufio.NewScanner(pr)
	// `ros2 topic hz` lines are short, but a node logging to stdout is not, and
	// the default 64 KiB cap turned that into a silently-ended stream.
	scanner.Buffer(make([]byte, 0, 64*1024), ros2HzMaxLineBytes)
	for scanner.Scan() {
		line := scanner.Text()
		if strings.Contains(line, "average rate:") {
			avgLine = line
			continue
		}
		if avgLine == "" {
			continue
		}
		sample, ok := parseROS2HzBlock(avgLine, line)
		avgLine = ""
		if !ok {
			continue
		}
		if serr := stream.Send(sample); serr != nil {
			_ = drainAndWait()
			return serr
		}
	}
	scanErr := scanner.Err()
	execErr := drainAndWait()
	if ctx.Err() != nil {
		return nil
	}
	if scanErr != nil {
		return ros2ScanError("topic hz", req.GetTopic(), scanErr, ros2HzMaxLineBytes)
	}
	if execErr != nil {
		return status.Errorf(codes.Internal, "ros2 topic hz: %v", execErr)
	}
	return nil
}

func (s *ROS2Service) RecordBag(stream grpc.BidiStreamingServer[agentpbv2.RecordROS2BagRequest, agentpbv2.RecordROS2BagResponse]) error {
	ctx := stream.Context()

	first, err := stream.Recv()
	if err != nil {
		return err
	}
	start := first.GetStart()
	if start == nil {
		return status.Error(codes.InvalidArgument, "first RecordBag message must be a start command")
	}

	scs, err := s.resolveSidecars(ctx, start.DomainId)
	if err != nil {
		return err
	}

	bagName := start.GetOutputName()
	if bagName == "" {
		bagName = "bag_" + time.Now().UTC().Format("20060102-150405")
	}
	if !ros2BagNamePattern.MatchString(bagName) {
		return status.Errorf(codes.InvalidArgument, "invalid bag name %q", bagName)
	}
	for _, topic := range start.GetTopics() {
		if err := validateROS2GraphName(topic); err != nil {
			return err
		}
	}

	args := []string{"bag", "record", "-o", filepath.Join(s.bagDir, bagName)}
	if len(start.GetTopics()) > 0 {
		args = append(args, start.GetTopics()...)
	} else {
		args = append(args, "-a")
	}

	// Recording captures a single RMW graph; route to the sidecar that owns the
	// first requested topic, else the first sidecar.
	sc := scs[0]
	if len(start.GetTopics()) > 0 {
		sc = s.pickSidecarForTopic(ctx, scs, start.GetTopics()[0])
	}

	// A rosbag can't span DDS implementations: on a mixed-RMW device, `-a`
	// records only this sidecar's RMW graph. Warn so missing topics from other
	// RMWs aren't a surprise (WDY-1594).
	var startMsg string
	if len(start.GetTopics()) == 0 && len(scs) > 1 {
		startMsg = fmt.Sprintf("recording the %s graph only; -a does not span RMWs (this device also runs other RMWs)", sc.rmw)
	}

	execCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	type execResult struct {
		code int
		err  error
	}
	var output bytes.Buffer
	execDone := make(chan execResult, 1)
	go func() {
		code, execErr := s.runtime.ExecROS2(execCtx, ROS2ExecOptions{DomainID: sc.domainID, SidecarName: sc.name, Args: args}, &output, &output)
		execDone <- execResult{code: code, err: execErr}
	}()

	if err := stream.Send(&agentpbv2.RecordROS2BagResponse{
		State:   agentpbv2.RecordROS2BagResponse_STATE_RECORDING,
		BagName: bagName,
		Message: startMsg,
	}); err != nil {
		cancel()
		<-execDone
		return err
	}

	// Wait for a stop command, client disconnect, or recorder exit.
	recvDone := make(chan error, 1)
	go func() {
		for {
			msg, rerr := stream.Recv()
			if rerr != nil {
				recvDone <- rerr
				return
			}
			if msg.GetStop() != nil {
				recvDone <- nil
				return
			}
		}
	}()

	var recorder execResult
	select {
	case <-recvDone:
		// Stop requested (or client went away): SIGINT the recorder so
		// rosbag2 finalizes the bag, then wait for it to exit.
		cancel()
		recorder = <-execDone
	case recorder = <-execDone:
		// Recorder exited on its own — diagnose and surface an error state.
		if ctx.Err() == nil {
			_ = stream.Send(&agentpbv2.RecordROS2BagResponse{
				State:   agentpbv2.RecordROS2BagResponse_STATE_ERROR,
				BagName: bagName,
				Message: s.diagnoseRecorderExit(ctx, sc.name, recorder.code, recorder.err, output.String()),
			})
			return nil
		}
	}

	if ctx.Err() != nil {
		return nil
	}
	// Cancellation propagates through ExecROS2 as ctx.Err(); that is the
	// expected stop path, not a failure.
	msg := ""
	if recorder.err != nil && !strings.Contains(recorder.err.Error(), context.Canceled.Error()) {
		msg = recorder.err.Error()
	}
	return stream.Send(&agentpbv2.RecordROS2BagResponse{
		State:   agentpbv2.RecordROS2BagResponse_STATE_STOPPED,
		BagName: bagName,
		Message: msg,
	})
}

// diagnoseRecorderExit builds the error message for a recorder that exited
// without a stop request. The most common cause is the ROS 2 app being
// stopped or redeployed mid-recording: that tears down the network namespace
// the sidecar (and recorder) joined, killing the DDS session. Raw recorder
// logs alone are misleading there, so check the anchor first and lead with
// the actual cause.
func (s *ROS2Service) diagnoseRecorderExit(ctx context.Context, sidecarName string, exitCode int, execErr error, output string) string {
	var verifyErr error
	if runtime, ok := s.runtime.(ROS2NamedSidecarVerifier); ok {
		verifyErr = runtime.VerifyROS2SidecarNamed(ctx, sidecarName)
	} else {
		verifyErr = s.runtime.VerifyROS2Sidecar(ctx)
	}
	var b strings.Builder
	if verifyErr != nil {
		b.WriteString("the ROS 2 sidecar or its app was stopped or redeployed while recording; ")
		b.WriteString("restart the recording once the ROS 2 graph is available (")
		b.WriteString(verifyErr.Error())
		b.WriteString(")")
	} else {
		fmt.Fprintf(&b, "recorder exited unexpectedly (exit code %d)", exitCode)
		if execErr != nil {
			b.WriteString(": ")
			b.WriteString(execErr.Error())
		}
	}
	if tail := tailLines(output, 10); tail != "" {
		b.WriteString("\nrecorder output:\n")
		b.WriteString(tail)
	}
	return b.String()
}

// tailLines returns the last n non-empty lines of s.
func tailLines(s string, n int) string {
	var lines []string
	for _, line := range strings.Split(s, "\n") {
		if strings.TrimSpace(line) != "" {
			lines = append(lines, strings.TrimRight(line, " \r"))
		}
	}
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}

func (s *ROS2Service) ListBags(ctx context.Context, _ *agentpbv2.ListROS2BagsRequest) (*agentpbv2.ListROS2BagsResponse, error) {
	entries, err := os.ReadDir(s.bagDir)
	if err != nil {
		if os.IsNotExist(err) {
			return &agentpbv2.ListROS2BagsResponse{}, nil
		}
		return nil, status.Errorf(codes.Internal, "reading bag directory: %v", err)
	}
	resp := &agentpbv2.ListROS2BagsResponse{}
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		bagPath := filepath.Join(s.bagDir, entry.Name())
		var size int64
		_ = filepath.Walk(bagPath, func(_ string, info os.FileInfo, werr error) error {
			if werr == nil && !info.IsDir() {
				size += info.Size()
			}
			return nil
		})
		bag := &agentpbv2.ListROS2BagsResponse_Bag{
			Name:      entry.Name(),
			SizeBytes: size,
		}
		if info, ierr := entry.Info(); ierr == nil {
			bag.CreatedUnix = info.ModTime().Unix()
		}
		if metadata, merr := os.ReadFile(filepath.Join(bagPath, "metadata.yaml")); merr == nil {
			if nanos, ok := parseROS2BagDurationNanos(string(metadata)); ok {
				bag.DurationSeconds = float64(nanos) / 1e9
			}
		}
		resp.Bags = append(resp.Bags, bag)
	}
	sort.Slice(resp.Bags, func(i, j int) bool { return resp.Bags[i].CreatedUnix < resp.Bags[j].CreatedUnix })
	return resp, nil
}

func (s *ROS2Service) DownloadBag(req *agentpbv2.DownloadROS2BagRequest, stream grpc.ServerStreamingServer[agentpbv2.ROS2BagChunk]) error {
	name := req.GetName()
	if !ros2BagNamePattern.MatchString(name) {
		return status.Errorf(codes.InvalidArgument, "invalid bag name %q", name)
	}
	bagPath := filepath.Join(s.bagDir, name)
	info, err := os.Stat(bagPath)
	if err != nil || !info.IsDir() {
		return status.Errorf(codes.NotFound, "bag %q not found", name)
	}

	chunker := &ros2ChunkWriter{stream: stream}
	tw := tar.NewWriter(chunker)
	walkErr := filepath.Walk(bagPath, func(path string, fi os.FileInfo, werr error) error {
		if werr != nil {
			return werr
		}
		rel, rerr := filepath.Rel(s.bagDir, path)
		if rerr != nil {
			return rerr
		}
		// Bags are plain directories of regular files (sqlite3/mcap +
		// metadata.yaml); skip anything else, including symlinks, so the
		// archive can never reference content outside the bag (SOC2-CC6).
		if !fi.Mode().IsRegular() && !fi.IsDir() {
			return nil
		}
		hdr, herr := tar.FileInfoHeader(fi, "")
		if herr != nil {
			return herr
		}
		hdr.Name = filepath.ToSlash(rel)
		if err := tw.WriteHeader(hdr); err != nil {
			return err
		}
		if fi.IsDir() {
			return nil
		}
		f, ferr := os.Open(path)
		if ferr != nil {
			return ferr
		}
		defer f.Close()
		_, cerr := io.Copy(tw, f)
		return cerr
	})
	if walkErr != nil {
		return status.Errorf(codes.Internal, "archiving bag: %v", walkErr)
	}
	if err := tw.Close(); err != nil {
		return status.Errorf(codes.Internal, "finalizing bag archive: %v", err)
	}
	return chunker.Flush()
}

// ros2ChunkWriter adapts a DownloadBag stream into an io.Writer that batches
// tar output into fixed-size chunks.
type ros2ChunkWriter struct {
	stream grpc.ServerStreamingServer[agentpbv2.ROS2BagChunk]
	buf    bytes.Buffer
}

func (w *ros2ChunkWriter) Write(p []byte) (int, error) {
	w.buf.Write(p)
	for w.buf.Len() >= ros2BagChunkSize {
		if err := w.send(w.buf.Next(ros2BagChunkSize)); err != nil {
			return 0, err
		}
	}
	return len(p), nil
}

func (w *ros2ChunkWriter) Flush() error {
	if w.buf.Len() == 0 {
		return nil
	}
	return w.send(w.buf.Next(w.buf.Len()))
}

func (w *ros2ChunkWriter) send(data []byte) error {
	chunk := make([]byte, len(data))
	copy(chunk, data)
	return w.stream.Send(&agentpbv2.ROS2BagChunk{Data: chunk})
}

func (s *ROS2Service) Exec(req *agentpbv2.ROS2ExecRequest, stream grpc.ServerStreamingServer[agentpbv2.ROS2ExecOutput]) error {
	ctx := stream.Context()
	scs, err := s.resolveSidecars(ctx, req.DomainId)
	if err != nil {
		return err
	}
	if len(req.GetArgs()) == 0 {
		return status.Error(codes.InvalidArgument, "ros2 exec requires at least one argument")
	}

	// Raw passthrough can't be RMW-routed (the args are opaque); run it in the
	// first sidecar. Use `--rmw`-aware commands for per-graph inspection.
	sc := scs[0]
	stdout := &ros2ExecStreamWriter{stream: stream, stderr: false}
	stderr := &ros2ExecStreamWriter{stream: stream, stderr: true}
	code, execErr := s.runtime.ExecROS2(ctx, ROS2ExecOptions{DomainID: sc.domainID, SidecarName: sc.name, Args: req.GetArgs()}, stdout, stderr)
	if ctx.Err() != nil {
		return nil
	}
	if execErr != nil {
		return status.Errorf(codes.Internal, "ros2 %s: %v", strings.Join(req.GetArgs(), " "), execErr)
	}
	exitCode := int32(code)
	return stream.Send(&agentpbv2.ROS2ExecOutput{ExitCode: &exitCode})
}

// ros2ExecStreamWriter forwards raw output chunks to an Exec stream.
type ros2ExecStreamWriter struct {
	stream grpc.ServerStreamingServer[agentpbv2.ROS2ExecOutput]
	stderr bool
}

func (w *ros2ExecStreamWriter) Write(p []byte) (int, error) {
	chunk := make([]byte, len(p))
	copy(chunk, p)
	msg := &agentpbv2.ROS2ExecOutput{}
	if w.stderr {
		msg.Stderr = chunk
	} else {
		msg.Stdout = chunk
	}
	if err := w.stream.Send(msg); err != nil {
		return 0, err
	}
	return len(p), nil
}

// --- Actions (WDY-1722) ---

func (s *ROS2Service) ListActions(ctx context.Context, req *agentpbv2.ListROS2ActionsRequest) (*agentpbv2.ListROS2ActionsResponse, error) {
	scs, err := s.resolveSidecars(ctx, req.DomainId)
	if err != nil {
		return nil, err
	}
	outs, err := s.runMerged(ctx, scs, "action", "list", "-t")
	if err != nil {
		return nil, err
	}
	resp := &agentpbv2.ListROS2ActionsResponse{}
	seen := map[string]bool{}
	for _, o := range outs {
		for _, a := range parseROS2ActionList(o.out) {
			a.Rmw = o.rmw
			key := a.GetName() + "\x00" + o.rmw
			if seen[key] {
				continue
			}
			seen[key] = true
			resp.Actions = append(resp.Actions, a)
		}
	}
	return resp, nil
}

func (s *ROS2Service) GetActionInfo(ctx context.Context, req *agentpbv2.GetROS2ActionInfoRequest) (*agentpbv2.GetROS2ActionInfoResponse, error) {
	scs, err := s.resolveSidecars(ctx, req.DomainId)
	if err != nil {
		return nil, err
	}
	if err := validateROS2GraphName(req.GetAction()); err != nil {
		return nil, err
	}
	sc := s.pickSidecarForAction(ctx, scs, req.GetAction())
	out, err := s.runIn(ctx, sc, "action", "info", req.GetAction())
	if err != nil {
		return nil, err
	}
	name, clients, servers := parseROS2ActionInfo(out)
	if name == "" {
		name = req.GetAction()
	}
	return &agentpbv2.GetROS2ActionInfoResponse{
		Name:          name,
		ActionClients: clients,
		ActionServers: servers,
		Verbose:       out,
	}, nil
}

func (s *ROS2Service) SendActionGoal(req *agentpbv2.SendROS2ActionGoalRequest, stream grpc.ServerStreamingServer[agentpbv2.ROS2ExecOutput]) error {
	ctx := stream.Context()
	scs, err := s.resolveSidecars(ctx, req.DomainId)
	if err != nil {
		return err
	}
	if err := validateROS2GraphName(req.GetAction()); err != nil {
		return err
	}
	if req.GetActionType() == "" {
		return status.Error(codes.InvalidArgument, "action type must not be empty")
	}
	args := []string{"action", "send_goal", req.GetAction(), req.GetActionType(), req.GetGoal()}
	if req.GetFeedback() {
		args = append(args, "--feedback")
	}
	sc := s.pickSidecarForAction(ctx, scs, req.GetAction())
	stdout := &ros2ExecStreamWriter{stream: stream, stderr: false}
	stderr := &ros2ExecStreamWriter{stream: stream, stderr: true}
	code, execErr := s.runtime.ExecROS2(ctx, ROS2ExecOptions{DomainID: sc.domainID, SidecarName: sc.name, Args: args}, stdout, stderr)
	if ctx.Err() != nil {
		return nil
	}
	if execErr != nil {
		return status.Errorf(codes.Internal, "ros2 %s: %v", strings.Join(args, " "), execErr)
	}
	exitCode := int32(code)
	return stream.Send(&agentpbv2.ROS2ExecOutput{ExitCode: &exitCode})
}

// --- Lifecycle / managed nodes (WDY-1722) ---

func (s *ROS2Service) ListLifecycleNodes(ctx context.Context, req *agentpbv2.ListROS2LifecycleNodesRequest) (*agentpbv2.ListROS2LifecycleNodesResponse, error) {
	scs, err := s.resolveSidecars(ctx, req.DomainId)
	if err != nil {
		return nil, err
	}
	outs, err := s.runMerged(ctx, scs, "lifecycle", "nodes")
	if err != nil {
		return nil, err
	}
	resp := &agentpbv2.ListROS2LifecycleNodesResponse{}
	seen := map[string]bool{}
	for _, o := range outs {
		for _, n := range parseROS2NodeList(o.out) {
			n.Rmw = o.rmw
			key := ros2NodeFQN(n) + "\x00" + o.rmw
			if seen[key] {
				continue
			}
			seen[key] = true
			resp.Nodes = append(resp.Nodes, n)
		}
	}
	return resp, nil
}

func (s *ROS2Service) GetLifecycleState(ctx context.Context, req *agentpbv2.GetROS2LifecycleStateRequest) (*agentpbv2.GetROS2LifecycleStateResponse, error) {
	scs, err := s.resolveSidecars(ctx, req.DomainId)
	if err != nil {
		return nil, err
	}
	if err := validateROS2GraphName(req.GetNode()); err != nil {
		return nil, err
	}
	sc := s.pickSidecarForNode(ctx, scs, req.GetNode())
	out, err := s.runIn(ctx, sc, "lifecycle", "get", req.GetNode())
	if err != nil {
		return nil, err
	}
	state, id, ok := parseROS2LifecycleState(out)
	if !ok {
		return nil, status.Errorf(codes.Internal, "could not parse lifecycle state: %q", strings.TrimSpace(out))
	}
	return &agentpbv2.GetROS2LifecycleStateResponse{State: state, StateId: id}, nil
}

func (s *ROS2Service) ListLifecycleTransitions(ctx context.Context, req *agentpbv2.ListROS2LifecycleTransitionsRequest) (*agentpbv2.ListROS2LifecycleTransitionsResponse, error) {
	scs, err := s.resolveSidecars(ctx, req.DomainId)
	if err != nil {
		return nil, err
	}
	if err := validateROS2GraphName(req.GetNode()); err != nil {
		return nil, err
	}
	sc := s.pickSidecarForNode(ctx, scs, req.GetNode())
	out, err := s.runIn(ctx, sc, "lifecycle", "list", req.GetNode())
	if err != nil {
		return nil, err
	}
	return &agentpbv2.ListROS2LifecycleTransitionsResponse{
		Transitions: parseROS2LifecycleTransitions(out),
	}, nil
}

func (s *ROS2Service) SetLifecycleState(ctx context.Context, req *agentpbv2.SetROS2LifecycleStateRequest) (*agentpbv2.SetROS2LifecycleStateResponse, error) {
	scs, err := s.resolveSidecars(ctx, req.DomainId)
	if err != nil {
		return nil, err
	}
	if err := validateROS2GraphName(req.GetNode()); err != nil {
		return nil, err
	}
	if req.GetTransition() == "" {
		return nil, status.Error(codes.InvalidArgument, "transition must not be empty")
	}
	sc := s.pickSidecarForNode(ctx, scs, req.GetNode())
	out, err := s.runIn(ctx, sc, "lifecycle", "set", req.GetNode(), req.GetTransition())
	if err != nil {
		// `ros2 lifecycle set` reports failures both via exit code and text.
		return &agentpbv2.SetROS2LifecycleStateResponse{Success: false, Message: err.Error()}, nil
	}
	msg := strings.TrimSpace(out)
	return &agentpbv2.SetROS2LifecycleStateResponse{
		Success: strings.Contains(msg, "Transitioning successful"),
		Message: msg,
	}, nil
}

// --- Components / composable nodes (WDY-1722) ---

func (s *ROS2Service) ListComponents(ctx context.Context, req *agentpbv2.ListROS2ComponentsRequest) (*agentpbv2.ListROS2ComponentsResponse, error) {
	scs, err := s.resolveSidecars(ctx, req.DomainId)
	if err != nil {
		return nil, err
	}
	outs, err := s.runMerged(ctx, scs, "component", "list")
	if err != nil {
		return nil, err
	}
	resp := &agentpbv2.ListROS2ComponentsResponse{}
	seen := map[string]bool{}
	for _, o := range outs {
		for _, c := range parseROS2ComponentList(o.out) {
			c.Rmw = o.rmw
			key := c.GetName() + "\x00" + o.rmw
			if seen[key] {
				continue
			}
			seen[key] = true
			resp.Containers = append(resp.Containers, c)
		}
	}
	return resp, nil
}

func (s *ROS2Service) LoadComponent(ctx context.Context, req *agentpbv2.LoadROS2ComponentRequest) (*agentpbv2.LoadROS2ComponentResponse, error) {
	scs, err := s.resolveSidecars(ctx, req.DomainId)
	if err != nil {
		return nil, err
	}
	if err := validateROS2GraphName(req.GetContainer()); err != nil {
		return nil, err
	}
	if req.GetPackage() == "" || req.GetPlugin() == "" {
		return nil, status.Error(codes.InvalidArgument, "package and plugin must not be empty")
	}
	sc := s.pickSidecarForComponent(ctx, scs, req.GetContainer())
	out, err := s.runIn(ctx, sc, "component", "load", req.GetContainer(), req.GetPackage(), req.GetPlugin())
	if err != nil {
		return nil, err
	}
	uid, nodeName, _ := parseROS2ComponentLoad(out)
	return &agentpbv2.LoadROS2ComponentResponse{
		Uid:      uid,
		NodeName: nodeName,
		Message:  strings.TrimSpace(out),
	}, nil
}

func (s *ROS2Service) UnloadComponent(ctx context.Context, req *agentpbv2.UnloadROS2ComponentRequest) (*agentpbv2.UnloadROS2ComponentResponse, error) {
	scs, err := s.resolveSidecars(ctx, req.DomainId)
	if err != nil {
		return nil, err
	}
	if err := validateROS2GraphName(req.GetContainer()); err != nil {
		return nil, err
	}
	sc := s.pickSidecarForComponent(ctx, scs, req.GetContainer())
	out, err := s.runIn(ctx, sc, "component", "unload", req.GetContainer(), strconv.Itoa(int(req.GetUid())))
	if err != nil {
		return nil, err
	}
	return &agentpbv2.UnloadROS2ComponentResponse{Message: strings.TrimSpace(out)}, nil
}

// validateROS2GraphName accepts ROS 2 graph names (topics, nodes, services):
// slash-separated identifiers, optionally with a leading slash or `~/`. The
// character set excludes whitespace and shell metacharacters, providing
// defence-in-depth on top of the sidecar's no-shell-interpretation exec
// (SOC2-CC6, ISO27001-A.8, NIST-SI-10).
//
// Each segment must start with a letter or underscore (hidden topics start with
// `_`) and may then contain letters, digits and underscores — which is ROS 2's
// own rule. The previous pattern (`^~?/?[a-zA-Z0-9_][a-zA-Z0-9_/]*$`) let through
// `//foo`, `/foo/` and `/1camera`: all illegal ROS 2 names that were forwarded to
// the CLI and came back as an rclpy traceback rather than an InvalidArgument.
var ros2GraphNamePattern = regexp.MustCompile(`^(~/|/)?[a-zA-Z_][a-zA-Z0-9_]*(/[a-zA-Z_][a-zA-Z0-9_]*)*$`)

func validateROS2GraphName(name string) error {
	if name == "" {
		return status.Error(codes.InvalidArgument, "name must not be empty")
	}
	if !ros2GraphNamePattern.MatchString(name) {
		return status.Errorf(codes.InvalidArgument, "invalid ROS 2 graph name %q", name)
	}
	return nil
}

// validateROS2ParamName accepts ROS 2 parameter names, which use dots as
// hierarchy separators (e.g. "robot.wheel.radius"). Each dot-separated segment
// follows the same identifier rule as a graph-name segment, so a leading digit,
// a trailing dot, or an empty segment (`a..b`) is rejected rather than passed to
// the CLI.
var ros2ParamNamePattern = regexp.MustCompile(`^[a-zA-Z_][a-zA-Z0-9_]*(\.[a-zA-Z_][a-zA-Z0-9_]*)*$`)

func validateROS2ParamName(name string) error {
	if name == "" {
		return status.Error(codes.InvalidArgument, "parameter name must not be empty")
	}
	if !ros2ParamNamePattern.MatchString(name) {
		return status.Errorf(codes.InvalidArgument, "invalid ROS 2 parameter name %q", name)
	}
	return nil
}
