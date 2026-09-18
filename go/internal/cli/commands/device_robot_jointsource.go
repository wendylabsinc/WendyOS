package commands

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strconv"
	"sync"
	"time"

	"google.golang.org/grpc"
	"gopkg.in/yaml.v3"

	"github.com/wendylabsinc/wendy/go/internal/agent/robotjoints"
	"github.com/wendylabsinc/wendy/go/internal/cli/robotwizard"
	"github.com/wendylabsinc/wendy/go/internal/shared/robotcal"
	agentpbv2 "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
)

// jointSourceBackends are the joint-source backends this build can open.
//
// The set belongs to the platform, exactly as the set of calibration methods
// does: a profile names one and parameterises it, and cannot describe a new one.
const (
	// backendROS2JointStates reads sensor_msgs/JointState off a ROS 2 topic. It
	// is the generic one — the message carries joint names, so nothing here has
	// to know which robot is publishing, and it works for any robot whose
	// driver speaks the ROS 2 convention.
	backendROS2JointStates = "ros2-joint-states"
	// backendUnitreeLowState reads a Unitree humanoid's whole body off
	// unitree_hg/msg/LowState, through the agent. It is the vendor one, needed
	// because a G1 publishes no sensor_msgs/JointState at all: its joints are a
	// positionally indexed array with no names in it, on a type the agent's
	// stock ROS 2 sidecar cannot deserialise.
	backendUnitreeLowState = robotjoints.BackendUnitreeLowState
)

func availableJointSourceBackends() []string {
	return []string{backendROS2JointStates, backendUnitreeLowState}
}

// openJointSource resolves the backend a profile selected, over the agent
// connection the caller already dialled.
//
// One case arm per backend this build can actually open, and a single refusal
// for everything else. A backend a profile names but this build cannot open is
// refused by name, carrying whatever the profile had to say about it — not
// silently degraded to another source, and not given a case arm of its own. A
// switch on vendor names is one refactor away from a switch that changes
// behaviour, and then the platform knows which robot it is talking to.
func openJointSource(conn *grpc.ClientConn) robotwizard.JointSourceOpener {
	return func(ctx context.Context, joints robotcal.Joints) (robotwizard.JointSource, error) {
		spec := joints.Source
		switch spec.Backend {
		case backendROS2JointStates:
			return newROS2JointSource(ctx, conn, spec.Params)
		case backendUnitreeLowState:
			return newUnitreeLowStateJointSource(ctx, conn, joints)
		default:
			return nil, &robotwizard.UnsupportedBackendError{
				Backend:   spec.Backend,
				Available: availableJointSourceBackends(),
				Detail:    spec.Note,
			}
		}
	}
}

// ros2JointSource reads sensor_msgs/JointState from a topic through the agent's
// ROS 2 service, so it works over a cloud tunnel and needs no ROS installation
// on this machine.
//
// It is read-only by construction: EchoTopic is the only RPC it holds, and
// there is no path from here to a publisher.
type ros2JointSource struct {
	topic  string
	cancel context.CancelFunc

	mu      sync.Mutex
	latest  robotwizard.JointReading
	err     error
	haveOne chan struct{}
	once    sync.Once
}

// ros2JointState is the part of sensor_msgs/JointState a sweep needs. The agent
// hands back the message as YAML, which is what `ros2 topic echo` produces.
type ros2JointState struct {
	Name     []string  `yaml:"name"`
	Position []float64 `yaml:"position"`
}

const defaultJointStatesTopic = "/joint_states"

func newROS2JointSource(ctx context.Context, conn *grpc.ClientConn, params map[string]string) (robotwizard.JointSource, error) {
	topic := params["topic"]
	if topic == "" {
		topic = defaultJointStatesTopic
	}
	var domain *int32
	if raw, ok := params["domain_id"]; ok && raw != "" {
		d, err := strconv.ParseInt(raw, 10, 32)
		if err != nil {
			return nil, fmt.Errorf("joint source parameter domain_id: %w", err)
		}
		v := int32(d)
		domain = &v
	}

	streamCtx, cancel := context.WithCancel(ctx)
	stream, err := agentpbv2.NewROS2ServiceClient(conn).EchoTopic(streamCtx, &agentpbv2.EchoROS2TopicRequest{
		DomainId: domain,
		Topic:    topic,
	})
	if err != nil {
		cancel()
		return nil, ros2RPCError(err)
	}

	s := &ros2JointSource{topic: topic, cancel: cancel, haveOne: make(chan struct{})}
	go s.pump(stream)
	return s, nil
}

func (s *ros2JointSource) pump(stream grpc.ServerStreamingClient[agentpbv2.ROS2Message]) {
	for {
		msg, err := stream.Recv()
		if err != nil {
			s.mu.Lock()
			if !errors.Is(err, io.EOF) {
				s.err = err
			} else if s.err == nil {
				s.err = fmt.Errorf("the %s stream ended", s.topic)
			}
			s.mu.Unlock()
			s.once.Do(func() { close(s.haveOne) })
			return
		}
		var state ros2JointState
		if err := yaml.Unmarshal([]byte(msg.GetYaml()), &state); err != nil {
			continue
		}
		if len(state.Name) == 0 || len(state.Name) != len(state.Position) {
			// A JointState with no names, or with a position array that does
			// not line up with them, cannot be zipped without guessing — and
			// guessing which joint a number belongs to is the exact failure the
			// joint map exists to catch.
			continue
		}
		reading := robotwizard.JointReading{
			At:        time.Now(),
			Positions: make(map[string]float64, len(state.Name)),
			Order:     append([]string(nil), state.Name...),
		}
		for i, name := range state.Name {
			reading.Positions[name] = state.Position[i]
		}
		s.mu.Lock()
		s.latest, s.err = reading, nil
		s.mu.Unlock()
		s.once.Do(func() { close(s.haveOne) })
	}
}

func (s *ros2JointSource) Read(ctx context.Context) (robotwizard.JointReading, error) {
	select {
	case <-s.haveOne:
	case <-ctx.Done():
		return robotwizard.JointReading{}, ctx.Err()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.latest.Positions == nil {
		if s.err != nil {
			return robotwizard.JointReading{}, s.err
		}
		return robotwizard.JointReading{}, fmt.Errorf("no sensor_msgs/JointState message arrived on %s", s.topic)
	}
	return s.latest, nil
}

func (s *ros2JointSource) Describe() string {
	return fmt.Sprintf("%s on %s", backendROS2JointStates, s.topic)
}

func (s *ros2JointSource) Close() error {
	s.cancel()
	return nil
}
