package browserauth

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/shared/certs"
	"github.com/wendylabsinc/wendy/go/internal/shared/litetopology"
	pb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protojson"
)

// TopologyObservation describes one authenticated reporter, not global health.
// Expected unsupported/offline results remain distinct from empty healthy graphs.
type TopologyObservation struct {
	Status     string          `json:"status"`
	Topology   json.RawMessage `json:"topology,omitempty"`
	Error      string          `json:"error,omitempty"`
	ObservedAt time.Time       `json:"observedAt"`
	Platform   string          `json:"platform,omitempty"`
}

// TopologyFailure classifies a failed observation without inferring device health.
func TopologyFailure(err error) TopologyObservation {
	state := "error"
	switch status.Code(err) {
	case codes.Unimplemented:
		state = "unsupported"
	case codes.PermissionDenied, codes.Unauthenticated:
		state = "unauthorized"
	case codes.Unavailable, codes.DeadlineExceeded, codes.Canceled:
		state = "unavailable"
	}
	var networkError net.Error
	if errors.As(err, &networkError) && networkError.Timeout() {
		state = "unavailable"
	}
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) || errors.Is(err, io.EOF) || errors.Is(err, net.ErrClosed) {
		state = "unavailable"
	}
	return TopologyObservation{Status: state, Error: err.Error(), ObservedAt: time.Now().UTC()}
}

// MeshTopology uses a separate, bounded connection. It never replaces a live
// dashboard connection, starts a device, changes its clock, or changes mesh policy.
var browserTopologyTimeout = 12 * time.Second
var browserLiteSetupBudget = litetopology.SetupBudget
var browserLiteHandshakeBudget = litetopology.HandshakeBudget
var browserLiteReadBudget = litetopology.AggregateBudget
var browserTopologyTotalBudget = 150 * time.Second

type topologyFlight struct {
	done   chan struct{}
	result TopologyObservation
}

func (s *Session) MeshTopology(ctx context.Context, asset string, dial func(context.Context, string) (net.Conn, error), client *http.Client) TopologyObservation {
	ctx, cancel := context.WithTimeout(ctx, browserTopologyTotalBudget)
	defer cancel()
	s.topologyMu.Lock()
	if s.topologyFlights == nil {
		s.topologyFlights = make(map[string]*topologyFlight)
	}
	if pending := s.topologyFlights[asset]; pending != nil {
		s.topologyMu.Unlock()
		select {
		case <-pending.done:
			return pending.result
		case <-ctx.Done():
			return TopologyFailure(ctx.Err())
		}
	}
	if s.topologyGate == nil {
		s.topologyGate = make(chan struct{}, 3)
	}
	gate := s.topologyGate
	pending := &topologyFlight{done: make(chan struct{})}
	s.topologyFlights[asset] = pending
	s.topologyMu.Unlock()
	var result TopologyObservation
	defer func() {
		s.topologyMu.Lock()
		pending.result = result
		delete(s.topologyFlights, asset)
		close(pending.done)
		s.topologyMu.Unlock()
	}()
	select {
	case gate <- struct{}{}:
		defer func() { <-gate }()
	case <-ctx.Done():
		result = TopologyFailure(ctx.Err())
		return result
	}
	result = s.meshTopology(ctx, asset, dial, client)
	return result
}

func (s *Session) meshTopology(ctx context.Context, asset string, dial func(context.Context, string) (net.Conn, error), client *http.Client) TopologyObservation {
	ctx, cancel := context.WithTimeout(ctx, browserTopologyTotalBudget)
	defer cancel()
	conn, lite, readContext, err := s.connectReporter(ctx, asset, dial, client, true)
	if err != nil {
		return TopologyFailure(err)
	}
	if lite != nil {
		defer lite.Close()
		readCtx, stopRead := context.WithTimeout(ctx, browserLiteReadBudget)
		defer stopRead()
		snapshot, err := litetopology.Read(readCtx, lite.Read)
		if err != nil {
			return TopologyFailure(err)
		}
		if err := ValidateTopologyReporter(snapshot, lite.Peer); err != nil {
			return TopologyFailure(err)
		}
		data, err := protojson.Marshal(snapshot)
		if err != nil {
			return TopologyFailure(err)
		}
		return TopologyObservation{Status: "ok", Topology: data, ObservedAt: time.Now().UTC(), Platform: "wendy-lite"}
	}
	defer conn.Close()
	snapshot, err := pb.NewWendyLocalMeshAdminServiceClient(conn.Conn).GetLocalMeshTopology(readContext, &pb.GetLocalMeshTopologyRequest{})
	if err != nil {
		return TopologyFailure(err)
	}
	peer, verified := conn.ObservedServerIdentity()
	if !verified {
		return TopologyFailure(status.Error(codes.Unauthenticated, "topology reporter identity was not verified"))
	}
	if err := ValidateTopologyReporter(snapshot, peer); err != nil {
		return TopologyFailure(err)
	}
	data, err := protojson.Marshal(snapshot)
	if err != nil {
		return TopologyFailure(err)
	}
	return TopologyObservation{Status: "ok", Topology: data, ObservedAt: time.Now().UTC(), Platform: "wendyos"}
}

// ValidateTopologyReporter binds the claimed local observation to the TLS peer.
func ValidateTopologyReporter(snapshot *pb.LocalMeshTopology, peer certs.WendyIdentity) error {
	return litetopology.ValidateReporter(snapshot, peer)
}
