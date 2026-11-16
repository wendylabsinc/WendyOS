package meshsession

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/wendylabsinc/WendyOS/go/babel"
	"github.com/wendylabsinc/wendy/go/internal/agent/localmesh"
	"github.com/wendylabsinc/wendy/go/internal/shared/directstream"
	"github.com/wendylabsinc/wendy/go/internal/shared/meshid"
)

// ErrDirectUseUpper means this new request was never assigned a direct flow ID
// because its peer epoch has promoted. Only this result permits opening a fresh
// upper application stream; an admitted OPEN error never permits such a retry.
var ErrNoDirectSession = errors.New("no retained direct session for peer")

var ErrDirectUseUpper = errors.New("new application open belongs to promoted transport")

var errDirectEpochRetired = errors.New("promoted direct epoch drained at both peers")

// DirectBackend must authorize the exact caller/service before opening a local
// endpoint, honor cancellation, and return promptly. Carrier authentication alone
// does not grant app-port, WendyCom or administration privileges.
type DirectBackend func(context.Context, meshid.Device, directstream.OpenRequest) (net.Conn, error)
type DirectSessionConfig struct {
	Peer           meshid.Device
	IdentityDigest [32]byte
	Role           byte
	// Original verified hard deadline, never renewed on carrier replacement.
	Deadline    time.Time
	PolicyCheck func() error
	// Revalidated security lifetime may shorten the original deadline.
	CheckLifetime    func() (time.Time, error)
	Backend          DirectBackend
	Slots            int
	TXBytes, RXBytes int
	Window, Initial  uint64
	RecoveryTimeout  time.Duration
	// Zero disables UDP. Separate from reliable replay/control reservations.
	DatagramBytes   int
	DatagramMaximum uint16
}
type directReply struct {
	conn net.Conn
	err  error
}
type directRequest struct {
	ctx                 context.Context
	request             directstream.OpenRequest
	id                  uint32
	mu                  sync.Mutex
	done                chan struct{}
	reply               directReply
	finished, abandoned bool
}

func (r *directRequest) finish(c net.Conn, err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.abandoned {
		if c != nil {
			c.Close()
		}
		return
	}
	if r.finished {
		panic("direct request completed twice")
	}
	r.reply = directReply{c, err}
	r.finished = true
	close(r.done)
}
func (r *directRequest) abandon() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.abandoned = true
	if r.finished && r.reply.conn != nil {
		r.reply.conn.Close()
	}
}

type directAttachRequest struct {
	mode      directstream.Mode
	openFlow  DirectPromotionOpener
	ctx       context.Context
	admission localmesh.DirectCarrierAdmission
	done      chan error
}
type directRead struct {
	data []byte
	err  error
}
type directBackendResult struct {
	request directstream.OpenRequest
	id      uint32
	conn    net.Conn
	err     error
}
type directCandidate struct {
	request           directAttachRequest
	ctx               context.Context
	cancel            context.CancelFunc
	read              chan directRead
	joined            chan struct{}
	exchange          *directstream.AttachmentExchange
	token, generation uint64
	alias             uint32
	nonce             [16]byte
	active            bool
	promotionClaims   map[uint32]bool
	promotionCancels  map[uint32]context.CancelFunc
	deadline          time.Time
}

// DirectSession is a single peer/role's runtime owner. It serializes flow and
// attachment state while application sockets and backend relays run separately.
// One carrier is active at a time; duplicate admissions cannot steal its state.
// The enclosing manager must Close on trust/config epoch changes and must not
// register direct capability until all selected promotion paths are installed.
// DirectSessionStatus is a coherent actor-published observation for scheduling.
// It is not admission authority: the actor still fences and validates cutover.
// Last BLE identity/cost survives an outage, without exposing replay capability.
// DirectAvailable includes an attachment still reconciling; Established requires
// a completed first exchange. Busy includes pending opens and unretired state.
// Promoted means upper control committed, not that every flow binding finished.
// Closed denotes cancellation; only Close returning proves workers are joined.
type DirectSessionStatus struct {
	Established, Busy, DirectAvailable, Promoting, Promoted, Closed bool
	// UpperOwned remains true between precommit upper retries.
	UpperOwned bool
	BLELink    babel.LinkID
	BLECost    uint16
	BLEAlias   uint32 // Carrier-local epoch alias; survives promotion for diagnostics.
}

func (s *DirectSession) Status() DirectSessionStatus {
	var status DirectSessionStatus
	if value := s.status.Load(); value != nil {
		status = value.(DirectSessionStatus)
	}
	if s.ctx.Err() != nil {
		status.Closed = true
		status.DirectAvailable = false
	}
	return status
}

type DirectSession struct {
	status   atomic.Value // DirectSessionStatus; single actor writer.
	cfg      DirectSessionConfig
	ctx      context.Context
	cancel   context.CancelCauseFunc
	done     chan struct{}
	attach   chan directAttachRequest
	dial     chan *directRequest
	wake     chan struct{}
	backend  chan directBackendResult
	promoted chan directPromotedResult
	workers  sync.WaitGroup
}

func NewDirectSession(parent context.Context, cfg DirectSessionConfig) (*DirectSession, error) {
	if !cfg.Peer.IsValid() || cfg.Role > 1 || cfg.IdentityDigest == ([32]byte{}) || cfg.Deadline.IsZero() || !time.Now().Before(cfg.Deadline) || cfg.Backend == nil {
		return nil, errors.New("invalid direct session configuration")
	}
	if cfg.Slots == 0 {
		cfg.Slots = 16
	}
	if cfg.TXBytes == 0 {
		cfg.TXBytes = 64 << 10
	}
	if cfg.RXBytes == 0 {
		cfg.RXBytes = 64 << 10
	}
	if cfg.Window == 0 {
		cfg.Window = 32 << 10
	}
	if cfg.Initial == 0 {
		cfg.Initial = 4 << 10
	}
	if cfg.RecoveryTimeout == 0 {
		cfg.RecoveryTimeout = 120 * time.Second
	}
	if cfg.RecoveryTimeout <= 0 {
		return nil, errors.New("invalid direct recovery deadline")
	}
	flows, err := directstream.NewReliableFlows(cfg.Role, cfg.Slots, cfg.TXBytes, cfg.RXBytes, cfg.Window, cfg.Initial)
	if err != nil {
		return nil, err
	}
	if cfg.DatagramBytes != 0 {
		if err := flows.EnableDatagrams(cfg.DatagramBytes, cfg.DatagramMaximum); err != nil {
			flows.Close(err)
			return nil, err
		}
	}
	ctx, cancel := context.WithCancelCause(parent)
	s := &DirectSession{cfg: cfg, ctx: ctx, cancel: cancel, done: make(chan struct{}), attach: make(chan directAttachRequest), dial: make(chan *directRequest), wake: make(chan struct{}, 1), backend: make(chan directBackendResult, 2*cfg.Slots), promoted: make(chan directPromotedResult, 2*cfg.Slots)}
	go s.run(flows)
	return s, nil
}
func (s *DirectSession) Close() { s.cancel(net.ErrClosed); <-s.done }
func (s *DirectSession) Attach(ctx context.Context, a localmesh.DirectCarrierAdmission) error {
	if a.Peer != s.cfg.Peer || a.IdentityDigest != s.cfg.IdentityDigest || a.Role != s.cfg.Role || !a.TLS.HandshakeComplete || a.Records == nil {
		return errors.New("direct admission identity mismatch")
	}
	if directstream.SelectedTransport(a.Selected) == 0 {
		return errors.New("missing promoted transport agreement")
	}
	return s.attachCandidate(ctx, a, directstream.DirectBLE, nil)
}
func (s *DirectSession) attachCandidate(ctx context.Context, a localmesh.DirectCarrierAdmission, mode directstream.Mode, open DirectPromotionOpener) error {
	r := directAttachRequest{ctx: ctx, admission: a, mode: mode, openFlow: open, done: make(chan error, 1)}
	select {
	case s.attach <- r:
	case <-ctx.Done():
		return ctx.Err()
	case <-s.done:
		return context.Cause(s.ctx)
	}
	select {
	case err := <-r.done:
		return err
	case <-s.done:
		return context.Cause(s.ctx)
	}
}
func (s *DirectSession) Dial(ctx context.Context, r directstream.OpenRequest) (net.Conn, error) {
	request := &directRequest{ctx: ctx, request: r, done: make(chan struct{})}
	select {
	case s.dial <- request:
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-s.done:
		err := context.Cause(s.ctx)
		if errors.Is(err, errDirectEpochRetired) {
			// No flow ID was allocated for this request. The caller may select
			// the current normal path while a fresh BLE epoch is admitted.
			err = ErrNoDirectSession
		}
		return nil, err
	}
	reply := func() (net.Conn, error) {
		request.mu.Lock()
		defer request.mu.Unlock()
		return request.reply.conn, request.reply.err
	}
	select {
	case <-request.done:
		return reply()
	case <-ctx.Done():
		request.abandon()
		select {
		case s.wake <- struct{}{}:
		default:
		}
		return nil, ctx.Err()
	case <-s.done:
		// The actor can finish a never-admitted request with UseUpper and
		// retire the old epoch before this goroutine runs. Preserve that
		// completed decision instead of exposing the retirement as an app
		// error. Incomplete requests retain the ordinary shutdown behavior.
		select {
		case <-request.done:
			return reply()
		default:
		}
		request.abandon()
		return nil, context.Cause(s.ctx)
	}
}
func (s *DirectSession) startRead(r directAttachRequest, token uint64) *directCandidate {
	ctx, cancel := context.WithCancel(r.ctx)
	alias := r.admission.AttachmentAlias
	if alias == 0 {
		alias = 1
	}
	c := &directCandidate{request: r, ctx: ctx, cancel: cancel, token: token, alias: alias, read: make(chan directRead, 1), joined: make(chan struct{}), deadline: time.Now().Add(60 * time.Second)}
	go func() {
		defer close(c.joined)
		for {
			data, err := r.admission.Records.ReceiveDirectRecord(ctx)
			select {
			case c.read <- directRead{data, err}:
			case <-ctx.Done():
				return
			}
			if err != nil {
				return
			}
		}
	}()
	return c
}
func (s *DirectSession) dialBackend(ctx context.Context, id uint32, r directstream.OpenRequest) {
	s.workers.Add(1)
	go func() {
		defer s.workers.Done()
		conn, err := s.cfg.Backend(ctx, s.cfg.Peer, r)
		select {
		case s.backend <- directBackendResult{request: r, id: id, conn: conn, err: err}:
		case <-s.ctx.Done():
			if conn != nil {
				conn.Close()
			}
		}
	}()
}
func (s *DirectSession) relay(app net.Conn, backend net.Conn, maximum uint16) {
	s.workers.Add(1)
	go func() {
		defer s.workers.Done()
		defer backend.Close()
		// App registries can revoke an owned endpoint without returning EOF.
		// Carry the same ownership signals as the upper-layer stream bridge.
		var revoked, closed <-chan struct{}
		if owned, ok := backend.(interface{ Done() <-chan struct{} }); ok {
			revoked = owned.Done()
		}
		if owned, ok := backend.(interface{ Closed() <-chan struct{} }); ok {
			closed = owned.Closed()
		}
		relayDone, watched := make(chan struct{}), make(chan struct{})
		go func() {
			defer close(watched)
			select {
			case <-relayDone:
				return
			case <-s.ctx.Done():
			case <-revoked:
			case <-closed:
			}
			backend.Close()
			app.Close()
		}()
		defer func() { close(relayDone); <-watched }()
		if maximum != 0 {
			done := make(chan error, 2)
			go func() { done <- relayDirectDatagrams(backend, app, int(maximum)) }()
			go func() { done <- relayDirectDatagrams(app, backend, int(maximum)) }()
			<-done
			backend.Close()
			app.Close()
			<-done
			return
		}
		done := make(chan error, 2)
		go func() {
			_, err := io.Copy(backend, app)
			if err == nil {
				if half, ok := backend.(interface{ CloseWrite() error }); ok {
					err = half.CloseWrite()
				}
			}
			done <- err
		}()
		go func() {
			_, err := io.Copy(app, backend)
			if err == nil {
				err = app.(interface{ CloseWrite() error }).CloseWrite()
			}
			done <- err
		}()
		first := <-done
		if first != nil {
			backend.Close()
			app.Close()
		}
		second := <-done
		if second != nil {
			app.Close()
		}
	}()
}
func (s *DirectSession) run(flows *directstream.ReliableFlows) {
	defer close(s.done)
	started := time.Now()
	nowUS := func() int64 { return time.Since(started).Microseconds() }
	binding := directstream.SessionBinding{IdentityDigest: s.cfg.IdentityDigest, Role: s.cfg.Role}
	gate := directstream.NewPendingGenerationGate()
	var current *directCandidate
	var token uint64
	var agreement uint32
	peerKnown := false
	var waiting []*directRequest
	dialCancels := map[uint32]context.CancelFunc{}
	completed := map[uint32]directBackendResult{}
	var outage, promotionDeadline time.Time
	var promotionStarted bool
	nextPolicy := time.Now()
	securityDeadline := s.cfg.Deadline
	terminal := error(net.ErrClosed)
	var established bool
	var bleLink babel.LinkID
	var bleCost uint16
	var bleAlias uint32
	var reported DirectSessionStatus
	publish := func() {
		status := DirectSessionStatus{UpperOwned: promotionStarted, Established: established, Busy: !flows.Quiescent() || len(waiting) != 0 || len(dialCancels) != 0, BLELink: bleLink, BLECost: bleCost, BLEAlias: bleAlias, Closed: s.ctx.Err() != nil}
		if current != nil {
			status.DirectAvailable = current.request.mode == directstream.DirectBLE && current.ctx.Err() == nil && !status.Closed
			status.Promoting = current.request.mode == directstream.Promoted && !current.active
			status.Promoted = current.request.mode == directstream.Promoted && current.active
		}
		if status != reported {
			s.status.Store(status)
			reported = status
		}
	}
	promotionCommitted := func() bool {
		return current != nil && (current.active || gate.Phase() >= directstream.AttachmentCommitting)
	}
	detach := func(err error) {
		if current == nil {
			return
		}
		c := current
		current = nil
		publish()
		c.cancel()
		<-c.joined
		if c.exchange != nil {
			c.exchange.Lost()
		} else {
			gate.Lost(c.token)
		}
		flows.Suspend()
		c.request.done <- err
		if outage.IsZero() {
			outage = time.Now().Add(s.cfg.RecoveryTimeout)
		}
	}
	defer func() {
		if (errors.Is(terminal, io.EOF) || errors.Is(terminal, net.ErrClosed)) && flows.RetirementReady() {
			terminal = errDirectEpochRetired
		}
		s.cancel(terminal)
		detach(terminal)
		gate.Close()
		flows.Close(terminal)
		for _, cancel := range dialCancels {
			cancel()
		}
		for _, r := range completed {
			if r.conn != nil {
				r.conn.Close()
			}
		}
		s.workers.Wait()
		for len(s.promoted) > 0 {
			r := <-s.promoted
			r.close()
		}
		for {
			select {
			case r := <-s.backend:
				if r.conn != nil {
					r.conn.Close()
				}
			default:
				for _, r := range waiting {
					r.finish(nil, terminal)
				}
				return
			}
		}
	}()
	activate := func() error {
		if current != nil && !current.active && current.exchange != nil && current.exchange.Active() {
			if current.request.mode == directstream.Promoted {
				if err := flows.BindPromotedControl(current.alias); err != nil {
					return err
				}
			} else if err := flows.Bind(current.alias); err != nil {
				return err
			}
			current.active = true
			established = true
			outage = time.Time{}
			if current.request.mode == directstream.Promoted {
				promotionDeadline = time.Time{}
			}
			publish()
		}
		return nil
	}
	prepare := func(c *directCandidate, offer *directstream.ControlRecord) error {
		view, err := flows.Snapshot()
		if err != nil {
			return err
		}
		cfg := directstream.ExchangeConfig{Binding: binding, Gate: gate, Token: c.token, Nonce: c.nonce, Local: view, MaxRemoteRecords: uint16(2 * s.cfg.Slots), Export: c.request.admission.TLS.ExportKeyingMaterial}
		cfg.Apply = func() error {
			remote, err := c.exchange.PeerView()
			if err != nil {
				return err
			}
			return flows.ApplyRecovery(remote, nowUS())
		}
		if offer == nil {
			generation, ok := gate.NextGeneration()
			if !ok {
				return directstream.ErrBounds
			}
			c.generation = generation
			c.exchange, err = directstream.NewCoordinatorExchange(cfg, c.alias, generation, c.request.mode, !peerKnown)
		} else {
			if c.request.admission.AttachmentAlias != 0 && offer.AttachmentID != c.alias {
				return directstream.ErrBounds
			}
			c.alias = offer.AttachmentID
			c.generation = offer.Generation
			c.exchange, err = directstream.NewParticipantExchange(cfg, *offer)
		}
		return err
	}
	for {
		publish()
		// Capture before checking readiness: a concurrent application write must
		// close this generation even if it races the output scan.
		changed := flows.Changed()
		datagramsChanged := flows.DatagramsChanged()
		now := time.Now()
		if !now.Before(securityDeadline) {
			terminal = errors.New("direct session credentials expired")
			return
		}
		if !promotionDeadline.IsZero() && !now.Before(promotionDeadline) {
			terminal = errors.New("promotion setup timed out")
			return
		}
		if !outage.IsZero() && !now.Before(outage) {
			terminal = errors.New("direct session recovery deadline expired")
			return
		}
		if !now.Before(nextPolicy) {
			if s.cfg.CheckLifetime != nil {
				deadline, err := s.cfg.CheckLifetime()
				if err != nil {
					terminal = err
					return
				}
				securityDeadline = minTime(securityDeadline, deadline)
				if !now.Before(securityDeadline) {
					terminal = errors.New("direct session credentials expired")
					return
				}
			}
			if s.cfg.PolicyCheck != nil {
				if err := s.cfg.PolicyCheck(); err != nil {
					terminal = err
					return
				}
			}
			nextPolicy = now.Add(time.Second)
		}
		active := current != nil && current.active
		upper := promotionStarted
		// Hold backend completions during snapshot/proof exchange. No endpoint is
		// redialled because a carrier closes while its authorized dial is pending.
		if current == nil || active {
			for id, r := range completed {
				delete(completed, id)
				if cancel := dialCancels[id]; cancel != nil {
					cancel()
					delete(dialCancels, id)
				}
				code := uint16(0)
				if r.err != nil || r.conn == nil {
					code = directstream.ResultDenied
				}
				var app net.Conn
				var err error
				if r.request.Service == directstream.ServiceUDP {
					app, err = flows.CompleteIncomingDatagram(id, code)
				} else {
					app, err = flows.CompleteIncoming(id, code)
				}
				if err != nil || code != 0 {
					if r.conn != nil {
						r.conn.Close()
					}
				} else {
					s.relay(app, r.conn, r.request.DatagramSize)
				}
			}
		}
		if active {
			kept := waiting[:0]
			for _, r := range waiting {
				if err := r.ctx.Err(); err != nil {
					if r.id != 0 {
						_ = flows.Reset(r.id, directstream.ResultCancelled)
					}
					r.finish(nil, err)
					continue
				}
				if r.id == 0 {
					if upper {
						r.finish(nil, ErrDirectUseUpper)
						continue
					}
					if r.request.Service == directstream.ServiceUDP {
						maximum, known := flows.PeerDatagramLimit()
						if !known {
							kept = append(kept, r)
							continue
						}
						if maximum == 0 || s.cfg.DatagramMaximum == 0 {
							r.finish(nil, ErrDirectUseUpper)
							continue
						}
					}
					id, err := flows.Begin(r.request, nowUS())
					if errors.Is(err, directstream.ErrCapacity) {
						kept = append(kept, r)
						continue
					}
					if err != nil {
						r.finish(nil, err)
						continue
					}
					r.id = id
				}
				var app net.Conn
				var err error
				if r.request.Service == directstream.ServiceUDP {
					app, err = flows.DatagramConnection(r.id)
				} else {
					app, err = flows.Connection(r.id)
				}
				if errors.Is(err, directstream.ErrOpenPending) {
					kept = append(kept, r)
					continue
				}
				if err != nil {
					r.finish(nil, err)
				} else {
					r.finish(app, nil)
				}
			}
			waiting = kept
		}
		if active && upper {
			ids, err := flows.PromotionIDs()
			if err != nil {
				terminal = fmt.Errorf("promotion IDs: %w", err)
				return
			}
			transcript, err := current.exchange.CommittedAttachment()
			if err != nil {
				terminal = err
				return
			}
			if current.promotionClaims == nil {
				current.promotionClaims = make(map[uint32]bool)
				current.promotionCancels = make(map[uint32]context.CancelFunc)
			}
			for _, id := range ids {
				if current.promotionClaims[id] {
					continue
				}
				request, err := flows.PromotionRequest(id)
				if err != nil {
					terminal = fmt.Errorf("promotion request: %w", err)
					return
				}
				current.promotionClaims[id] = true
				current.promotionCancels[id] = s.startPromotedFlow(current, binding, transcript, id, request)
			}
		}

		blocked, burstExhausted := false, false
		if current != nil {
			c := current
			for burst := 0; burst < 32; burst++ {
				var wire [directstream.MaxFlowFrame]byte
				var n int
				var err error
				control := false
				flowID := uint32(0)
				kind := directstream.FrameKind(0)
				if c.exchange != nil {
					if r, ok := c.exchange.Next(); ok {
						control = true
						n, err = r.Encode(wire[:])
					}
				}
				if n == 0 && err == nil && c.active {
					var r directstream.FlowFrame
					var ready bool
					r, ready, err = flows.Prepare(nowUS())
					if ready {
						n, err = r.Encode(wire[:])
						flowID = r.FlowID
						kind = r.Kind
					}
				}
				if err != nil {
					terminal = fmt.Errorf("prepare frame: %w", err)
					return
				}
				if n == 0 {
					break
				}
				accepted, err := c.request.admission.Records.TrySendDirectRecord(wire[:n])
				if err != nil {
					if upper && promotionCommitted() {
						terminal = err
						return
					}
					detach(err)
					break
				}
				if !accepted {
					blocked = true
					break
				}
				burstExhausted = burst == 31
				if control {
					err = c.exchange.Accepted()
				} else {
					err = flows.Accepted()
					if kind == directstream.FrameReset {
						if cancel := current.promotionCancels[flowID]; cancel != nil {
							cancel()
						}
						if cancel := dialCancels[flowID]; cancel != nil {
							cancel()
						}
					}
				}
				if err != nil {
					terminal = fmt.Errorf("accepted frame %d: %w", kind, err)
					return
				}
				if err = activate(); err != nil {
					terminal = err
					return
				}
			}
		}
		// Sending COMMIT_ACK can activate the participant without a socket wake.
		// Revisit pending dials/completions before waiting for more peer input.
		if active && upper && current != nil && flows.RetirementReady() && len(waiting) == 0 && len(dialCancels) == 0 && len(completed) == 0 && len(current.promotionCancels) == 0 {
			if drain, ok := current.request.admission.Records.(interface{ DirectRecordsDrained() bool }); ok && drain.DirectRecordsDrained() {
				terminal = errDirectEpochRetired
				return
			}
		}
		if !active && current != nil && current.active {
			continue
		}
		deadline := minTime(securityDeadline, nextPolicy)
		if !promotionDeadline.IsZero() {
			deadline = minTime(deadline, promotionDeadline)
		}
		if !outage.IsZero() {
			deadline = minTime(deadline, outage)
		}
		var input <-chan directRead
		var carrierDone <-chan struct{}
		if current != nil {
			input = current.read
			carrierDone = current.ctx.Done()
			if !current.active {
				deadline = minTime(deadline, current.deadline)
			} else if !blocked {
				if wake, ok := flows.NextWake(nowUS()); ok {
					deadline = minTime(deadline, started.Add(time.Duration(wake)*time.Microsecond))
				}
			}
		}
		if burstExhausted {
			// Keep draining ready bytes, but still give input/cancellation a turn.
			deadline = time.Now()
		}
		if blocked {
			deadline = minTime(deadline, time.Now().Add(2*time.Millisecond))
		}
		timer := time.NewTimer(max(time.Until(deadline), 0))
		publish()
		select {
		case <-s.ctx.Done():
			timer.Stop()
			terminal = context.Cause(s.ctx)
			return
		case <-timer.C:
			if current != nil && !current.active && !time.Now().Before(current.deadline) {
				if upper {
					terminal = errors.New("promotion setup timed out")
					return
				}
				detach(errors.New("direct attachment setup timed out"))
			}
		case <-changed:
			timer.Stop()
		case <-datagramsChanged:
			timer.Stop()
		case <-s.wake:
			timer.Stop()
			nextPolicy = time.Now()
		case r := <-s.dial:
			timer.Stop()
			if upper {
				r.finish(nil, ErrDirectUseUpper)
				continue
			}
			if len(waiting) >= 2*s.cfg.Slots {
				r.finish(nil, directstream.ErrCapacity)
			} else {
				waiting = append(waiting, r)
			}
		case r := <-s.backend:
			timer.Stop()
			completed[r.id] = r
		case r := <-s.promoted:
			timer.Stop()
			if current == nil || current.token != r.token || current.request.mode != directstream.Promoted {
				r.close()
				continue
			}
			if cancel := current.promotionCancels[r.id]; cancel != nil {
				cancel()
				delete(current.promotionCancels, r.id)
			}
			// A backend/app may have closed while the candidate was negotiating. A
			// late result belongs only to that flow, never a replacement backend.
			request, requestErr := flows.PromotionRequest(r.id)
			if errors.Is(requestErr, directstream.ErrRetired) {
				r.close()
				continue
			}
			if r.err == nil {
				r.err = requestErr
			}
			if r.err == nil && request.Service == directstream.ServiceUDP {
				if r.packets == nil {
					r.err = errors.New("missing bound UDP association")
				} else {
					_, r.err = flows.CommitPromotedDatagram(r.id, r.packets)
				}
			} else if r.err == nil {
				_, r.err = flows.CommitPromotedFlow(r.id, r.stream)
			}
			if r.err != nil {
				r.close()
				// Even an ambiguous binding commit terminates just this stream. RESET on
				// the still-authenticated upper control lane releases its peer's state.
				if err := flows.Reset(r.id, directstream.ResultIO); err != nil && !errors.Is(err, directstream.ErrRetired) {
					terminal = err
					return
				}
			}
		case r := <-s.attach:
			timer.Stop()
			if promotionStarted && r.mode != directstream.Promoted {
				r.done <- directstream.ErrPromoted
				continue
			}
			if agreement != 0 && agreement != r.admission.Selected {
				r.done <- errors.New("promotion agreement changed")
				continue
			}
			if r.mode == directstream.Promoted {
				if binding.SessionID == ([32]byte{}) || !peerKnown && s.cfg.Role == 0 {
					r.done <- directstream.ErrOpenPending
					continue
				}
				if r.ctx.Err() != nil {
					r.done <- r.ctx.Err()
					continue
				}
				if current != nil && current.request.mode == directstream.Promoted {
					// Crossed dials converge on the logical coordinator's dial.
					// A single direction remains valid regardless of role. Never
					// replace a connection once COMMIT may have reached its peer.
					preferred := r.admission.PromotionInitiator == (s.cfg.Role == 0)
					incumbentPreferred := current.request.admission.PromotionInitiator == (s.cfg.Role == 0)
					if !preferred || incumbentPreferred || current.active || gate.Phase() >= directstream.AttachmentCommitting {
						r.done <- directstream.ErrPromoted
						continue
					}
					detach(directstream.ErrPromoted)
				}
				// Release the direct handler without killing its routed underlay. Old
				// actor reads are joined by detach before taking the promotion snapshot.
				detach(localmesh.ErrDirectLaneRetired)
			}
			if current != nil {
				r.done <- errors.New("direct peer already has an active carrier")
				continue
			}
			if r.ctx.Err() != nil {
				r.done <- r.ctx.Err()
				continue
			}
			token++
			if token == 0 {
				terminal = directstream.ErrBounds
				return
			}
			agreement = r.admission.Selected
			current = s.startRead(r, token)
			if r.mode == directstream.Promoted {
				promotionStarted = true
				if promotionDeadline.IsZero() {
					promotionDeadline = current.deadline
				}
				current.deadline = promotionDeadline
			}
			if r.mode == directstream.DirectBLE {
				bleLink, bleCost = r.admission.Link, r.admission.Cost
				bleAlias = current.alias
			}
			if _, err := rand.Read(current.nonce[:]); err != nil {
				terminal = err
				return
			}
			if s.cfg.Role == 0 {
				if binding.SessionID == ([32]byte{}) {
					if _, err := rand.Read(binding.SessionID[:]); err != nil {
						terminal = err
						return
					}
					if _, err := rand.Read(binding.Capability[:]); err != nil {
						terminal = err
						return
					}
				}
				if err := prepare(current, nil); err != nil {
					terminal = err
					return
				}
			}
		case <-carrierDone:
			timer.Stop()
			if upper && promotionCommitted() {
				terminal = current.ctx.Err()
				return
			}
			detach(current.ctx.Err())
		case read := <-input:
			timer.Stop()
			if read.err != nil {
				if upper && promotionCommitted() {
					terminal = read.err
					return
				}
				detach(read.err)
				continue
			}
			if len(read.data) == 0 {
				terminal = directstream.ErrBounds
				return
			}
			c := current
			if read.data[0] >= directstream.ControlOffer {
				record, err := directstream.DecodeControlRecord(read.data)
				if err != nil {
					terminal = err
					return
				}
				if c.exchange == nil {
					if s.cfg.Role != 1 || record.Kind != directstream.ControlOffer || record.Mode != c.request.mode {
						terminal = directstream.ErrBounds
						return
					}
					if binding.SessionID == ([32]byte{}) {
						if !record.Create {
							terminal = errors.New("direct session not retained")
							return
						}
						binding.SessionID = record.SessionID
						binding.Capability = record.Capability
					}
					if err = prepare(c, &record); err != nil {
						terminal = err
						return
					}
				} else {
					if err = c.exchange.Receive(record); err != nil {
						terminal = err
						return
					}
					if s.cfg.Role == 0 && record.Kind == directstream.ControlReady {
						peerKnown = true
					}
				}
				if err = activate(); err != nil {
					terminal = err
					return
				}
			} else {
				if !c.active || !gate.Allows(c.token, c.generation) {
					terminal = errors.New("direct payload before activation")
					return
				}
				frame, err := directstream.DecodeFlowFrame(read.data)
				if err != nil {
					terminal = err
					return
				}
				event, err := flows.Receive(frame, nowUS())
				if err != nil {
					terminal = fmt.Errorf("receive frame %d: %w", frame.Kind, err)
					return
				}
				if frame.Kind == directstream.FrameReset {
					if cancel := current.promotionCancels[frame.FlowID]; cancel != nil {
						cancel()
					}
					if cancel := dialCancels[frame.FlowID]; cancel != nil {
						cancel()
					}
				}
				if event != nil {
					ctx, cancel := context.WithCancel(s.ctx)
					dialCancels[event.ID] = cancel
					s.dialBackend(ctx, event.ID, event.Request)
				}
			}
		}
	}
}
func minTime(a, b time.Time) time.Time {
	if a.Before(b) {
		return a
	}
	return b
}
