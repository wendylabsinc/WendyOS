package services

import (
	"context"
	"errors"
	"net"
	"sync"
	"time"

	"go.uber.org/zap"

	cloudpb "github.com/wendylabsinc/wendy/go/proto/gen/cloudpb"
)

const (
	// datagramFlowIdleTimeout is the agent-side safety expiry for UDP flow
	// sockets; the client edge owns the primary (60s) flow lifetime.
	datagramFlowIdleTimeout = 2 * time.Minute
	maxUDPPayload           = 65507

	// maxFlowsPerSession bounds how many concurrent UDP sockets (each backed
	// by its own goroutine) one datagram session will open. flow_id is
	// entirely client-assigned and unauthenticated beyond session membership,
	// so without a cap a buggy or hostile same-org client could walk flow_id
	// and exhaust the device's file descriptors / goroutines. 256 comfortably
	// covers the multi-flow use cases this design targets (a handful of UDP
	// streams plus ICMP per session) while bounding worst-case resource use
	// per session to a few hundred sockets.
	maxFlowsPerSession = 256

	// rateLimitLogInterval bounds how often a single client can force a log
	// line for the same recurring condition (oversize frame, invalid port,
	// flow-table cap, dial failure).
	rateLimitLogInterval = 10 * time.Second
)

// errFlowCapReached is returned by flow() when a brand-new flow_id would push
// the session over maxFlowsPerSession. It never applies to an already-open
// flow_id (that path is just a write to an existing socket, not a new fd).
var errFlowCapReached = errors.New("datagram flow-table cap reached")

// datagramRelay serves one DATAGRAM tunnel session: a flow table of connected
// loopback UDP sockets keyed by client-assigned flow_id, plus inline ICMP echo
// replies (the agent IS the pinged host; no ICMP socket is involved).
type datagramTunnelStream interface {
	Send(*cloudpb.TunnelData) error
	Recv() (*cloudpb.TunnelData, error)
}

type datagramRelay struct {
	logger      *zap.Logger
	stream      datagramTunnelStream
	idleTimeout time.Duration

	sendMu sync.Mutex // gRPC streams do not allow concurrent Send

	mu    sync.Mutex
	flows map[uint32]*datagramFlow

	lastOversizeLog     time.Time
	lastOversizeEchoLog time.Time
	lastFlowCapLog      time.Time
	lastDialFailLog     time.Time
	lastInvalidPortLog  time.Time
}

type datagramFlow struct {
	conn       *net.UDPConn
	port       uint32
	lastActive time.Time // guarded by datagramRelay.mu
}

func newDatagramRelay(logger *zap.Logger, stream datagramTunnelStream, idleTimeout time.Duration) *datagramRelay {
	return &datagramRelay{
		logger:      logger,
		stream:      stream,
		idleTimeout: idleTimeout,
		flows:       make(map[uint32]*datagramFlow),
	}
}

func (r *datagramRelay) activeFlows() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.flows)
}

func (r *datagramRelay) send(msg *cloudpb.TunnelData) error {
	r.sendMu.Lock()
	defer r.sendMu.Unlock()
	return r.stream.Send(msg)
}

// run serves the session until the stream ends or ctx is cancelled.
func (r *datagramRelay) run(ctx context.Context) {
	sweep := time.NewTicker(r.idleTimeout / 4)
	defer sweep.Stop()
	defer r.closeAll()

	frames := make(chan *cloudpb.TunnelData)
	recvErr := make(chan error, 1)
	go func() {
		for {
			msg, err := r.stream.Recv()
			if err != nil {
				recvErr <- err
				return
			}
			select {
			case frames <- msg:
			case <-ctx.Done():
				return
			}
		}
	}()

	for {
		select {
		case msg := <-frames:
			switch {
			case msg.GetDatagram() != nil:
				r.handleDatagram(ctx, msg.GetDatagram())
			case msg.GetIcmpRequest() != nil:
				r.handleEcho(msg.GetIcmpRequest())
			}
		case <-sweep.C:
			r.expireIdle()
		case <-recvErr:
			return
		case <-ctx.Done():
			return
		}
	}
}

func (r *datagramRelay) handleEcho(req *cloudpb.IcmpEchoRequest) {
	if len(req.GetPayload()) > maxUDPPayload {
		r.mu.Lock()
		if time.Since(r.lastOversizeEchoLog) > rateLimitLogInterval {
			r.lastOversizeEchoLog = time.Now()
			r.logger.Warn("dropping oversized tunnel echo request", zap.Int("size", len(req.GetPayload())))
		}
		r.mu.Unlock()
		return
	}

	err := r.send(&cloudpb.TunnelData{IcmpReply: &cloudpb.IcmpEchoReply{
		Identifier:      req.GetIdentifier(),
		Sequence:        req.GetSequence(),
		Payload:         req.GetPayload(),
		OriginateUnixNs: req.GetOriginateUnixNs(),
		AgentUnixNs:     uint64(time.Now().UnixNano()),
	}})
	if err != nil {
		r.logger.Warn("failed to send icmp echo reply", zap.Error(err))
	}
}

func (r *datagramRelay) handleDatagram(ctx context.Context, d *cloudpb.TunnelDatagram) {
	if len(d.GetPayload()) > maxUDPPayload {
		r.mu.Lock()
		if time.Since(r.lastOversizeLog) > rateLimitLogInterval {
			r.lastOversizeLog = time.Now()
			r.logger.Warn("dropping oversized tunnel datagram",
				zap.Uint32("flow_id", d.GetFlowId()), zap.Int("size", len(d.GetPayload())))
		}
		r.mu.Unlock()
		return
	}

	if d.GetPort() == 0 || d.GetPort() > 65535 {
		r.mu.Lock()
		if time.Since(r.lastInvalidPortLog) > rateLimitLogInterval {
			r.lastInvalidPortLog = time.Now()
			r.logger.Warn("dropping datagram: invalid port",
				zap.Uint32("flow_id", d.GetFlowId()), zap.Uint32("port", d.GetPort()))
		}
		r.mu.Unlock()
		return
	}

	flow, err := r.flow(ctx, d.GetFlowId(), d.GetPort())
	if err != nil {
		if errors.Is(err, errFlowCapReached) {
			r.mu.Lock()
			if time.Since(r.lastFlowCapLog) > rateLimitLogInterval {
				r.lastFlowCapLog = time.Now()
				r.logger.Warn("dropping datagram: session flow-table cap reached",
					zap.Uint32("flow_id", d.GetFlowId()), zap.Int("max_flows", maxFlowsPerSession))
			}
			r.mu.Unlock()
			return
		}
		r.mu.Lock()
		if time.Since(r.lastDialFailLog) > rateLimitLogInterval {
			r.lastDialFailLog = time.Now()
			r.logger.Warn("failed to open UDP flow",
				zap.Uint32("flow_id", d.GetFlowId()), zap.Uint32("port", d.GetPort()), zap.Error(err))
		}
		r.mu.Unlock()
		return
	}
	if _, err := flow.conn.Write(d.GetPayload()); err != nil {
		r.closeFlow(d.GetFlowId())
	}
}

func (r *datagramRelay) flow(ctx context.Context, flowID, port uint32) (*datagramFlow, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if f, ok := r.flows[flowID]; ok {
		f.lastActive = time.Now()
		return f, nil
	}
	if len(r.flows) >= maxFlowsPerSession {
		return nil, errFlowCapReached
	}
	// SECURITY: authenticated clients intentionally select any valid UDP port,
	// but never the destination host. Keeping that policy server-side prevents
	// a modified client from turning this diagnostic tunnel into a LAN relay.
	conn, err := net.DialUDP("udp", nil, datagramLoopbackAddr(port))
	if err != nil {
		return nil, err
	}
	f := &datagramFlow{conn: conn, port: port, lastActive: time.Now()}
	r.flows[flowID] = f
	go r.readFlow(ctx, flowID, f)
	return f, nil
}

func datagramLoopbackAddr(port uint32) *net.UDPAddr {
	return &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: int(port)}
}

// readFlow pumps device→client datagrams for one flow until its socket closes.
func (r *datagramRelay) readFlow(ctx context.Context, flowID uint32, f *datagramFlow) {
	buf := make([]byte, maxUDPPayload)
	for {
		n, err := f.conn.Read(buf)
		if err != nil {
			r.closeFlow(flowID)
			return
		}
		payload := make([]byte, n)
		copy(payload, buf[:n])
		r.mu.Lock()
		f.lastActive = time.Now()
		r.mu.Unlock()
		if err := r.send(&cloudpb.TunnelData{Datagram: &cloudpb.TunnelDatagram{
			FlowId: flowID, Port: f.port, Payload: payload,
		}}); err != nil {
			r.closeFlow(flowID)
			return
		}
		if ctx.Err() != nil {
			return
		}
	}
}

func (r *datagramRelay) closeFlow(flowID uint32) {
	r.mu.Lock()
	f, ok := r.flows[flowID]
	delete(r.flows, flowID)
	r.mu.Unlock()
	if ok {
		_ = f.conn.Close()
	}
}

func (r *datagramRelay) expireIdle() {
	r.mu.Lock()
	var expired []uint32
	for id, f := range r.flows {
		if time.Since(f.lastActive) > r.idleTimeout {
			expired = append(expired, id)
		}
	}
	r.mu.Unlock()
	for _, id := range expired {
		r.closeFlow(id)
	}
}

func (r *datagramRelay) closeAll() {
	r.mu.Lock()
	flows := r.flows
	r.flows = make(map[uint32]*datagramFlow)
	r.mu.Unlock()
	for _, f := range flows {
		_ = f.conn.Close()
	}
}
