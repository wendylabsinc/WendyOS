package cloudrelay

import (
	"context"
	"fmt"
	"net"
	"slices"
	"sync"
	"time"

	pb "github.com/wendylabsinc/wendy/go/proto/gen/relaypb"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

const (
	maxDatagramPayload = 65507
	datagramIdle       = 2 * time.Minute
	maxDatagramFlows   = 64
	maxFlowCreations   = 16
)

// DatagramSession forwards one authorized UDP port. The relay sees destination
// ID 1, never the port; each local peer has its own flow ID and connected socket.
type DatagramSession struct {
	session *Session
}

func OpenUDP(ctx, authCtx context.Context, cloudConn *grpc.ClientConn, verifier *Verifier, assetID string, port uint32, signRequest func([]byte) ([]byte, error)) (*DatagramSession, error) {
	service, err := PortService("udp", port)
	if err != nil {
		return nil, err
	}
	s, err := openPortSession(ctx, authCtx, cloudConn, verifier, assetID, service, pb.DialTransport_DIAL_TRANSPORT_DATAGRAM, signRequest)
	if err != nil {
		return nil, err
	}
	return &DatagramSession{session: s}, nil
}

func (s *DatagramSession) Close() error { return s.session.Close() }

func (s *DatagramSession) Send(flowID uint32, payload []byte) error {
	return s.session.sendDatagram(&pb.TunnelDatagram{FlowId: flowID, DestinationId: 1, Payload: payload})
}

func (s *DatagramSession) Recv() (*pb.TunnelDatagram, error) {
	return s.session.recvDatagram()
}

func validDatagram(d *pb.TunnelDatagram) bool {
	return d != nil && d.DestinationId == 1 && len(d.Payload) <= maxDatagramPayload
}

func (s *Session) sendDatagram(d *pb.TunnelDatagram) error {
	if !validDatagram(d) {
		return status.Error(codes.InvalidArgument, "invalid authorized datagram")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.stream.Send(&pb.JoinSessionRequest{Message: &pb.JoinSessionRequest_Frame{
		Frame: &pb.TunnelFrame{Frame: &pb.TunnelFrame_Datagram{Datagram: d}},
	}})
}

func (s *Session) recvDatagram() (*pb.TunnelDatagram, error) {
	msg, err := s.stream.Recv()
	if err != nil {
		return nil, err
	}
	d := msg.GetFrame().GetDatagram()
	if !validDatagram(d) {
		return nil, status.Error(codes.InvalidArgument, "invalid authorized datagram")
	}
	return d, nil
}

type udpFlow struct {
	conn     *net.UDPConn
	lastSeen time.Time
}

type udpRelay struct {
	session   *Session
	port      uint32
	mu        sync.Mutex
	flows     map[uint32]*udpFlow
	creations []time.Time
	readers   sync.WaitGroup
}

func serveDatagrams(session *Session, instruction *pb.DialInstruction) error {
	if err := validateService(instruction); err != nil {
		return err
	}
	r := &udpRelay{
		session: session,
		port:    instruction.AuthorizedDatagramDestinations[0].LoopbackPort,
		flows:   make(map[uint32]*udpFlow),
	}
	defer r.close()
	for {
		d, err := session.recvDatagram()
		if err != nil {
			return err
		}
		f, err := r.flow(d.FlowId, time.Now())
		if err != nil {
			return err
		}
		if _, err := f.conn.Write(d.Payload); err != nil {
			return fmt.Errorf("writing tunnel datagram: %w", err)
		}
	}
}

// The agent independently applies the v2 protocol's limits even if the broker
// is compromised. Socket deadlines reclaim idle flows without a sweep goroutine.
func (r *udpRelay) flow(id uint32, now time.Time) (*udpFlow, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for flowID, f := range r.flows {
		if now.Sub(f.lastSeen) >= datagramIdle {
			_ = f.conn.Close()
			delete(r.flows, flowID)
		}
	}
	if f := r.flows[id]; f != nil {
		f.lastSeen = now
		return f, f.conn.SetReadDeadline(now.Add(datagramIdle))
	}
	r.creations = slices.DeleteFunc(r.creations, func(t time.Time) bool { return now.Sub(t) >= time.Second })
	if len(r.flows) >= maxDatagramFlows || len(r.creations) >= maxFlowCreations {
		return nil, status.Error(codes.ResourceExhausted, "UDP flow limit exceeded")
	}
	// Any valid port is intentional, like an SSH local forward. Authorization
	// binds this one port; neither the caller nor Cloud can select another host.
	conn, err := net.DialUDP("udp4", nil, &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: int(r.port)})
	if err != nil {
		return nil, err
	}
	if err := conn.SetWriteBuffer(maxDatagramPayload); err != nil {
		_ = conn.Close()
		return nil, err
	}
	if err := conn.SetReadDeadline(now.Add(datagramIdle)); err != nil {
		_ = conn.Close()
		return nil, err
	}
	f := &udpFlow{conn: conn, lastSeen: now}
	r.flows[id] = f
	r.creations = append(r.creations, now)
	r.readers.Go(func() { r.read(id, f) })
	return f, nil
}

func (r *udpRelay) read(id uint32, f *udpFlow) {
	defer func() {
		r.mu.Lock()
		if r.flows[id] == f {
			delete(r.flows, id)
		}
		r.mu.Unlock()
		_ = f.conn.Close()
	}()
	// One extra byte detects truncated oversized packets, including a reply
	// from a local service that ignores the protocol's maximum payload size.
	buf := make([]byte, maxDatagramPayload+1)
	for {
		n, err := f.conn.Read(buf)
		if err != nil {
			return
		}
		if n > maxDatagramPayload {
			continue
		}
		r.mu.Lock()
		current := r.flows[id] == f
		if current {
			f.lastSeen = time.Now()
			err = f.conn.SetReadDeadline(f.lastSeen.Add(datagramIdle))
		}
		r.mu.Unlock()
		if !current || err != nil {
			return
		}
		if err := r.session.sendDatagram(&pb.TunnelDatagram{FlowId: id, DestinationId: 1, Payload: buf[:n]}); err != nil {
			_ = r.session.Close()
			return
		}
	}
}

func (r *udpRelay) close() {
	// Cancel blocked stream sends before waiting for socket reader goroutines.
	_ = r.session.Close()
	r.mu.Lock()
	for id, f := range r.flows {
		_ = f.conn.Close()
		delete(r.flows, id)
	}
	r.mu.Unlock()
	r.readers.Wait()
}
