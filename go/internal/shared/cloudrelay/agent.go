package cloudrelay

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/wendylabsinc/wendy/go/internal/shared/asyncdiag"
	pb "github.com/wendylabsinc/wendy/go/proto/gen/relaypb"
	"go.uber.org/zap"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

// Agent owns the authenticated Cloud control connection and an identity-free
// broker presence stream. Credentials are reloaded on each lease request.
type Agent struct {
	dialCloud           func(string, string, string, []byte) (*grpc.ClientConn, error)
	Endpoint            string
	Verifier            *Verifier
	StateDir            string
	Credentials         func() (string, string, []byte)
	Logger              *zap.Logger
	MTLSPort            int
	OnRegistered        func() // the device is reachable through the relay again
	signing, encryption *ecdsa.PrivateKey
	lease               *pb.IssuePresenceLeaseResponse
	leaseClaims         claims
	// leases holds each unexpired lease Cloud issued to these keys, by jti.
	// Cloud binds a session grant to the newest lease it issued, whether or
	// not a broker admitted it, and the broker redelivers an unacknowledged
	// offer on the next presence stream for the route. An offer can therefore
	// name a lease this agent has already replaced, by renewal or by
	// reconnecting.
	leases              map[string]claims
	slots               chan struct{}
	sourceAddresses     func() ([]net.Addr, error)
	sourceCheckInterval time.Duration
	preferredSource     func(*net.TCPAddr) (net.IP, error)
	pathReachable       func(context.Context, string) bool
	presenceMaxAge      time.Duration
}

var errPresenceRefresh = errors.New("refreshing Cloud presence before transport timeout")

func (a *Agent) Run(ctx context.Context) {
	backoff := time.Second
	for ctx.Err() == nil {
		started := time.Now()
		err := a.runOnce(ctx)
		if ctx.Err() != nil {
			return
		}
		if errors.Is(err, errPresenceRefresh) || errors.Is(err, errPresencePathMoved) || errors.Is(err, errPresenceSourceRemoved) {
			a.Logger.Info("reconnecting cloud presence", zap.Error(err))
			backoff = time.Second
			continue
		}
		if time.Since(started) > time.Minute {
			backoff = time.Second
		}
		if err != nil {
			a.Logger.Warn("cloud relay connection failed, reconnecting", zap.Error(err), zap.Duration("backoff", backoff))
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		backoff = min(backoff*2, time.Minute)
	}
}
func writeState(path string, data []byte) error {
	f, err := os.CreateTemp(filepath.Dir(path), ".relay-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if err = f.Chmod(0600); err == nil {
		_, err = f.Write(data)
	}
	if err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	if err = os.Rename(f.Name(), path); err != nil {
		return err
	}
	dir, err := os.Open(filepath.Dir(path))
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}
func (a *Agent) keys() error {
	if a.slots == nil {
		a.slots = make(chan struct{}, 64)
	}
	if a.signing != nil {
		return nil
	}
	if err := os.MkdirAll(a.StateDir, 0700); err != nil {
		return err
	}
	path := filepath.Join(a.StateDir, "keys.json")
	raw, err := os.ReadFile(path)
	var record struct{ Signing, Encryption []byte }
	if err == nil {
		if json.Unmarshal(raw, &record) != nil {
			return fmt.Errorf("invalid saved relay keys")
		}
		a.signing, err = x509.ParseECPrivateKey(record.Signing)
		if err != nil {
			return err
		}
		a.encryption, err = x509.ParseECPrivateKey(record.Encryption)
		if err != nil || a.signing.Curve != elliptic.P256() || a.encryption.Curve != elliptic.P256() || bytes.Equal(publicDER(a.signing), publicDER(a.encryption)) {
			a.signing = nil
			return fmt.Errorf("invalid or reused relay key material")
		}
		return nil
	}
	if !os.IsNotExist(err) {
		return err
	}
	signing, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return err
	}
	encryption, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return err
	}
	record.Signing, _ = x509.MarshalECPrivateKey(signing)
	record.Encryption, _ = x509.MarshalECPrivateKey(encryption)
	raw, _ = json.Marshal(record)
	if err = writeState(path, raw); err != nil {
		return err
	}
	a.signing = signing
	a.encryption = encryption
	return nil
}
func (a *Agent) validateLease(ctx context.Context, l *pb.IssuePresenceLeaseResponse) (claims, error) {
	if l == nil || l.Broker == nil {
		return claims{}, fmt.Errorf("Cloud returned no relay instance")
	}
	if _, err := target(l.Broker.Endpoint); err != nil {
		return claims{}, err
	}
	c, err := a.Verifier.verify(ctx, l.PresenceLeaseJws, leaseType, l.Broker.Audience, time.Now())
	if err != nil {
		return c, err
	}
	if c.Signing != binding(publicDER(a.signing)) || c.Encryption != binding(publicDER(a.encryption)) {
		return c, fmt.Errorf("Cloud lease key binding mismatch")
	}
	return c, nil
}

// remember keeps a lease Cloud issued to these keys until it expires.
func (a *Agent) remember(c claims) {
	if a.leases == nil {
		a.leases = map[string]claims{}
	}
	now := time.Now().Unix()
	for id, l := range a.leases {
		if l.Exp <= now {
			delete(a.leases, id)
		}
	}
	a.leases[c.JTI] = c
	for len(a.leases) > presenceLeaseHistoryLimit {
		var oldest claims
		for id, candidate := range a.leases {
			if id == c.JTI {
				continue
			}
			if oldest.JTI == "" || candidate.Iat < oldest.Iat || candidate.Iat == oldest.Iat && candidate.JTI < oldest.JTI {
				oldest = candidate
			}
		}
		delete(a.leases, oldest.JTI)
	}
}

// presenceLeaseRequestError marks only failures of the issuance RPC. Validation,
// credentials and saved-state errors must not be mistaken for transient RPCs.
type presenceLeaseRequestError struct{ error }

func (e *presenceLeaseRequestError) Unwrap() error { return e.error }

func presenceDeadline(err error) bool {
	return errors.Is(err, context.DeadlineExceeded) || status.Code(err) == codes.DeadlineExceeded
}

func retryPresenceLease(err error) bool {
	var request *presenceLeaseRequestError
	return errors.As(err, &request) && presenceDeadline(request.error)
}

// presenceBrokerError identifies only the public presence RPC and its phase.
// Device-mTLS issuance errors have different authentication semantics.
type presenceBrokerError struct {
	phase string
	err   error
}

func (e *presenceBrokerError) Error() string {
	return fmt.Sprintf("public broker presence %s: %v", e.phase, e.err)
}
func (e *presenceBrokerError) Unwrap() error { return e.err }

// requestLease reads immutable agent keys and verifies Cloud's response. Only
// the presence loop may remember, archive or apply the returned lease.
func (a *Agent) requestLease(ctx context.Context) (*pb.IssuePresenceLeaseResponse, claims, error) {
	ctx, cancel := context.WithTimeout(ctx, ConnectionSetupTimeout)
	defer cancel()
	cert, chain, key := a.Credentials()
	defer clear(key)
	dial := a.dialCloud
	if dial == nil {
		dial = DialCloud
	}
	conn, err := dial(a.Endpoint, cert, chain, key)
	if err != nil {
		return nil, claims{}, err
	}
	defer conn.Close()
	l, err := pb.NewTunnelAuthorizationServiceClient(conn).IssuePresenceLease(ctx, &pb.IssuePresenceLeaseRequest{AgentSigningPublicKeySpkiDer: publicDER(a.signing), AgentKeyAgreementPublicKeySpkiDer: publicDER(a.encryption), CorrelationId: uuid.NewString()})
	if err != nil {
		return nil, claims{}, &presenceLeaseRequestError{fmt.Errorf("requesting presence lease from %s (%s): %w", a.Endpoint, pb.TunnelAuthorizationService_IssuePresenceLease_FullMethodName, err)}
	}
	c, err := a.validateLease(ctx, l)
	if err != nil {
		return nil, c, err
	}
	return l, c, nil
}

func (a *Agent) recordIssuedLease(l *pb.IssuePresenceLeaseResponse, c claims) error {
	// Cloud already binds new grants to this lease, whatever happens to it next.
	a.remember(c)
	return a.archivePresenceLease(l, c)
}

func (a *Agent) saveResumeLease(l *pb.IssuePresenceLeaseResponse) error {
	raw, err := proto.Marshal(l)
	if err != nil {
		return err
	}
	return writeState(filepath.Join(a.StateDir, "lease.pb"), raw)
}

func (a *Agent) issue(ctx context.Context) (*pb.IssuePresenceLeaseResponse, claims, error) {
	l, c, err := a.requestLease(ctx)
	if err == nil {
		err = a.recordIssuedLease(l, c)
	}
	if err == nil {
		err = a.saveResumeLease(l)
	}
	return l, c, err
}
func renewalDelay(l *pb.IssuePresenceLeaseResponse, c claims) time.Duration {
	// Leave room for a slow request and a retry, without turning short-lived
	// leases into a continuous renewal loop. Cloud may request an earlier time.
	lifetime := time.Unix(c.Exp, 0).Sub(time.Unix(c.Iat, 0))
	lead := min(2*ConnectionSetupTimeout+15*time.Second,
		max(30*time.Second, lifetime/2))
	when := time.Unix(c.Exp, 0).Add(-lead)
	if l.RenewAfter != nil && l.RenewAfter.CheckValid() == nil && l.RenewAfter.AsTime().Before(when) {
		when = l.RenewAfter.AsTime()
	}
	return max(time.Until(when), time.Second)
}
func (a *Agent) runOnce(parent context.Context) error {
	if err := a.keys(); err != nil {
		return err
	}
	a.expireSavedOffers()
	ctx, cancel := context.WithCancel(parent)
	defer cancel()
	if a.lease == nil {
		if raw, e := os.ReadFile(filepath.Join(a.StateDir, "lease.pb")); e == nil && len(raw) < 32768 {
			var l pb.IssuePresenceLeaseResponse
			if proto.Unmarshal(raw, &l) == nil {
				if c, e := a.validateLease(ctx, &l); e == nil {
					if e = a.archivePresenceLease(&l, c); e != nil {
						return e
					}
					a.lease = &l
					a.leaseClaims = c
					a.remember(c)
				}
			}
		}
	}
	// A cached lease must cover connection setup and the broker's admission
	// exchange. Otherwise a slow reconnect can become live just before expiry,
	// with too little time left to issue and apply its first renewal.
	if a.lease == nil || time.Until(time.Unix(a.leaseClaims.Exp, 0)) < ConnectionSetupTimeout+30*time.Second {
		l, c, e := a.issue(ctx)
		if e != nil {
			return e
		}
		a.lease = l
		a.leaseClaims = c
	}
	lease, c := a.lease, a.leaseClaims
	if err := a.restorePresenceLeases(ctx, c); err != nil {
		return err
	}
	conn, err := a.Verifier.dialRelay(lease.Broker.Endpoint)
	if err != nil {
		return err
	}
	defer conn.Close()
	// A broker refusal invalidates this resume lease (a restart changes the
	// broker audience). Public transport unavailability says nothing about a
	// verified lease: reconnect with fresh TLS and holder proof until its expiry.
	keepLease := false
	defer func() {
		// A cancelled parent is the agent shutting down, not the broker
		// refusing anything: the lease is still good and is what lets the next
		// start resume presence without a round trip to Cloud.
		if keepLease || parent.Err() != nil {
			return
		}
		a.lease, a.leaseClaims = nil, claims{}
		// The same dead lease is otherwise reloaded after an agent restart:
		// validateLease checks Cloud's signature, which a stale lease still
		// carries, not whether the broker still knows it.
		_ = os.Remove(filepath.Join(a.StateDir, "lease.pb"))
	}()
	brokerError := func(phase string, err error) error {
		// gRPC can report a failed public TCP/TLS connection as Unavailable.
		// It cannot reliably distinguish that from a server's Unavailable,
		// but neither proves this signed lease invalid. Explicit auth/policy
		// refusals remain terminal. This does not classify Cloud issuance.
		keepLease = (presenceDeadline(err) || status.Code(err) == codes.Unavailable) && time.Now().Before(time.Unix(a.leaseClaims.Exp, 0))
		return &presenceBrokerError{phase: phase, err: err}
	}
	stream, err := pb.NewTunnelBrokerV2ServiceClient(conn).RegisterPresence(metadata.NewOutgoingContext(ctx, metadata.MD{}))
	if err != nil {
		return brokerError("opening stream", err)
	}
	if err = stream.Send(&pb.RegisterPresenceRequest{Message: &pb.RegisterPresenceRequest_Open{Open: &pb.PresenceOpen{PresenceLeaseJws: lease.PresenceLeaseJws, AgentSigningPublicKeySpkiDer: publicDER(a.signing)}}}); err != nil {
		return brokerError("sending open", err)
	}
	admission := time.AfterFunc(30*time.Second, cancel)
	msg, err := stream.Recv()
	if err != nil {
		admission.Stop()
		return brokerError("receiving challenge", err)
	}
	sig, err := proof(a.signing, "presence", c.Aud, c.Route, c.JTI, lease.PresenceLeaseJws, "agent-presence", msg.GetChallenge())
	if err != nil {
		admission.Stop()
		return err
	}
	err = stream.Send(&pb.RegisterPresenceRequest{Message: &pb.RegisterPresenceRequest_Proof{Proof: &pb.PresenceProof{ChallengeId: msg.GetChallenge().ChallengeId, Signature: sig}}})
	if err != nil {
		admission.Stop()
		return brokerError("sending holder proof", err)
	}
	msg, err = stream.Recv()
	admission.Stop()
	if err != nil {
		// The broker reports ownership conflict here only after validating
		// this lease and its fresh holder proof. A stale old stream must
		// drain; another lease cannot release it and would repeat the Cloud
		// mTLS exchange over a potentially slow borrowed path.
		wrapped := brokerError("receiving admission", err)
		keepLease = keepLease || status.Code(err) == codes.AlreadyExists
		return wrapped
	}
	accepted := msg.GetAccepted()
	if accepted == nil || accepted.LeaseExpiresAt == nil || accepted.LeaseExpiresAt.CheckValid() != nil || accepted.LeaseExpiresAt.AsTime().Unix() != c.Exp {
		return fmt.Errorf("invalid presence acceptance")
	}
	a.Logger.Info("registered presence with cloud relay", zap.String("endpoint", lease.Broker.Endpoint))
	if a.OnRegistered != nil {
		a.OnRegistered()
	}
	// Cloud Run closes even active streaming requests at one hour. Rotate
	// presence deliberately before that deadline, keeping its lease and the
	// independently owned data sessions rather than treating it as a failure.
	maxAge := a.presenceMaxAge
	if maxAge <= 0 {
		maxAge = 55 * time.Minute
	}
	refresh := time.NewTimer(maxAge)
	defer refresh.Stop()
	presencePeer, _ := peer.FromContext(stream.Context())
	pathChanged := make(chan error, 1)
	go func(endpoint string) {
		pathChanged <- a.watchPresencePath(ctx, presencePeer, endpoint)
	}(lease.Broker.Endpoint)
	renewal := time.NewTimer(renewalDelay(lease, c))
	defer renewal.Stop()
	expiry := time.NewTimer(time.Until(time.Unix(c.Exp, 0)))
	defer expiry.Stop()
	type received struct {
		m *pb.RegisterPresenceResponse
		e error
	}
	ch := make(chan received, 1)
	go func() {
		for {
			m, e := stream.Recv()
			select {
			case ch <- received{m, e}:
			case <-ctx.Done():
				return
			}
			if e != nil {
				return
			}
		}
	}()
	pending := map[string]*offerState{}
	// Offers refused on this stream, until the broker stops redelivering them.
	rejected := map[string]int64{}
	var renewing *pb.IssuePresenceLeaseResponse
	var renewedClaims claims
	type renewalResult struct {
		lease  *pb.IssuePresenceLeaseResponse
		claims claims
		err    error
	}
	results := make(chan renewalResult, 1)
	var workerDone chan struct{}
	deferred := map[string]*pb.SessionOffer{}
	handleOffer := func(o *pb.SessionOffer) error {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if !time.Now().Before(time.Unix(c.Exp, 0)) {
			return fmt.Errorf("Cloud presence lease expired")
		}
		if !bounded(o.OfferId, 128) || !bounded(o.SessionId, 128) {
			return fmt.Errorf("invalid relay offer")
		}
		if prior := deferred[o.OfferId]; prior != nil {
			if !proto.Equal(prior, o) {
				return fmt.Errorf("conflicting relay offer")
			}
			return nil
		}
		now := time.Now()
		for id, s := range pending {
			if s.c.Exp <= now.Unix() {
				delete(pending, id)
				_ = os.Remove(filepath.Join(a.StateDir, "offer-"+binding([]byte(id))+".pb"))
			}
		}
		for id, l := range a.leases {
			if l.Exp <= now.Unix() {
				delete(a.leases, id)
			}
		}
		for id, until := range rejected {
			if until <= now.Unix() {
				delete(rejected, id)
			}
		}
		if prior := pending[o.OfferId]; prior != nil {
			if !proto.Equal(prior.o, o) {
				return fmt.Errorf("conflicting relay offer")
			}
		} else {
			if _, refused := rejected[o.OfferId]; refused {
				return nil
			}
			// An uncached grant may need JWKS I/O. Its own context must not
			// let that lookup outlive this presence's signed authority.
			offerCtx, stopOffer := context.WithDeadline(ctx, time.Unix(c.Exp, 0))
			s, e := a.admitOffer(offerCtx, o, c, a.leases, pending, now)
			stopOffer()
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if !time.Now().Before(time.Unix(c.Exp, 0)) {
				return fmt.Errorf("Cloud presence lease expired")
			}
			if e != nil {
				// Cloud can send a grant bound to its new lease before the
				// issuance RPC reaches us. Retain only small, already-verified
				// grants until that one worker finishes; no decryption or ack
				// happens until the exact lease binding is known.
				if errors.Is(e, errUnknownPresenceLease) && workerDone != nil && len(deferred) < 64 && proto.Size(o) <= presenceLeaseBytesLimit {
					deferred[o.OfferId] = o
					return nil
				}
				// tunnel/v2 refuses a bad offer with no decryption and
				// no dial. Ending presence instead would drop the lease
				// and every other offer for one bad grant or service.
				// Unacknowledged, it is redelivered until its grant
				// expires, at most a minute away; remember up to 64
				// such offers that long so a redelivery is skipped.
				asyncdiag.Log(a.Logger, zap.WarnLevel, "refused cloud relay offer", zap.String("offer_id", o.OfferId), zap.String("error", e.Error()))
				if len(rejected) < 64 {
					rejected[o.OfferId] = now.Add(time.Minute + cloudClockSkew).Unix()
				}
				return nil
			}
			pending[o.OfferId] = s
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if !time.Now().Before(time.Unix(c.Exp, 0)) {
			return fmt.Errorf("Cloud presence lease expired")
		}
		if err = stream.Send(&pb.RegisterPresenceRequest{Message: &pb.RegisterPresenceRequest_OfferAcknowledgement{OfferAcknowledgement: &pb.PresenceOfferAcknowledgement{OfferId: o.OfferId, SessionId: o.SessionId}}}); err != nil {
			return brokerError("acknowledging offer", err)
		}
		return nil
	}
	retryDeferred := func() error {
		for id, o := range deferred {
			delete(deferred, id)
			if e := handleOffer(o); e != nil {
				return e
			}
		}
		return nil
	}
	// A worker has no agent-state writes and cannot outlive this attempt,
	// including cancellation or signed expiry while its RPC is outstanding.
	defer func() {
		cancel()
		if workerDone != nil {
			<-workerDone
		}
	}()
	retryDelay := time.Second
	for {
		// Cancellation and the current signed expiry win over ready offers,
		// issuance results and renewal acknowledgements alike.
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if !time.Now().Before(time.Unix(c.Exp, 0)) {
			return fmt.Errorf("Cloud presence lease expired")
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-expiry.C:
			return fmt.Errorf("Cloud presence lease expired")
		case <-refresh.C:
			keepLease = true
			return errPresenceRefresh
		case err := <-pathChanged:
			// A local network change, not a broker refusal: the lease binds
			// no source address, so the same broker still admits it over the
			// new path. Re-presenting it saves an IssuePresenceLease round
			// trip; if the broker does refuse it, that attempt drops it.
			keepLease = true
			return err
		case <-renewal.C:
			if workerDone != nil || renewing != nil {
				return fmt.Errorf("presence renewal already pending")
			}
			workerDone = make(chan struct{})
			go func(done chan struct{}) {
				defer close(done)
				l, n, e := a.requestLease(ctx)
				results <- renewalResult{l, n, e}
			}(workerDone)
		case result := <-results:
			<-workerDone
			workerDone = nil
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if !time.Now().Before(time.Unix(c.Exp, 0)) {
				return fmt.Errorf("Cloud presence lease expired")
			}
			if result.err != nil {
				if !retryPresenceLease(result.err) {
					return result.err
				}
				if e := retryDeferred(); e != nil {
					return e
				}
				asyncdiag.Log(a.Logger, zap.WarnLevel, "cloud presence renewal deadline; retaining valid presence", zap.String("error", result.err.Error()), zap.Duration("retry", retryDelay))
				renewal.Reset(retryDelay)
				retryDelay = min(retryDelay*2, 15*time.Second)
				continue
			}
			l, n := result.lease, result.claims
			if !time.Now().Before(time.Unix(n.Exp, 0)) {
				return fmt.Errorf("Cloud returned an expired presence renewal")
			}
			if e := a.recordIssuedLease(l, n); e != nil {
				return e
			}
			if n.Aud != c.Aud || n.Route != c.Route || l.Broker.Endpoint != lease.Broker.Endpoint {
				// Freshly issued against the broker Cloud now names, so it is
				// the one lease that survives this attempt.
				if e := a.saveResumeLease(l); e != nil {
					return e
				}
				a.lease = l
				a.leaseClaims = n
				keepLease = true
				return fmt.Errorf("Cloud selected a new presence route; reconnecting")
			}
			renewing = l
			renewedClaims = n
			if e := stream.Send(&pb.RegisterPresenceRequest{Message: &pb.RegisterPresenceRequest_Renewal{Renewal: &pb.PresenceRenewal{PresenceLeaseJws: l.PresenceLeaseJws}}}); e != nil {
				return brokerError("sending renewal", e)
			}
			if e := retryDeferred(); e != nil {
				return e
			}
		case r := <-ch:
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if !time.Now().Before(time.Unix(c.Exp, 0)) {
				return fmt.Errorf("Cloud presence lease expired")
			}
			if r.e != nil {
				return brokerError("receiving messages", r.e)
			}
			switch m := r.m.Message.(type) {
			case *pb.RegisterPresenceResponse_Renewed:
				if renewing == nil || m.Renewed.LeaseExpiresAt == nil || m.Renewed.LeaseExpiresAt.CheckValid() != nil || m.Renewed.LeaseExpiresAt.AsTime().Unix() != renewedClaims.Exp {
					return fmt.Errorf("invalid presence renewal acceptance")
				}
				if !time.Now().Before(time.Unix(c.Exp, 0)) || !time.Now().Before(time.Unix(renewedClaims.Exp, 0)) {
					return fmt.Errorf("Cloud presence lease expired")
				}
				if e := a.saveResumeLease(renewing); e != nil {
					return e
				}
				lease = renewing
				c = renewedClaims
				a.lease = lease
				a.leaseClaims = c
				renewing = nil
				retryDelay = time.Second
				renewal.Reset(renewalDelay(lease, c))
				expiry.Reset(time.Until(time.Unix(c.Exp, 0)))
			case *pb.RegisterPresenceResponse_SessionOffer:
				if e := handleOffer(m.SessionOffer); e != nil {
					return e
				}
			case *pb.RegisterPresenceResponse_OfferAcknowledged:
				s := pending[m.OfferAcknowledged.OfferId]
				if s == nil || s.c.Session != m.OfferAcknowledged.SessionId {
					return fmt.Errorf("unknown relay offer acknowledgement")
				}
				if !s.started {
					select {
					case a.slots <- struct{}{}:
					default:
						// Only this session waits out its grant; presence and
						// the sessions already running are unaffected.
						a.Logger.Warn("refused cloud relay session", zap.String("offer_id", s.o.OfferId), zap.String("reason", "too many active relay sessions"))
						continue
					}
					s.started = true
					broker := lease.Broker
					go func() {
						defer func() { <-a.slots }()
						// Sessions belong to the agent, so replacing its presence
						// connection must not cancel their independent relay streams.
						if e := a.serve(parent, broker, s.o, s.c, s.d); e != nil && parent.Err() == nil {
							a.Logger.Warn("cloud relay session failed", zap.Error(e))
						}
					}()
				}
			default:
				return fmt.Errorf("unexpected presence message")
			}
		}
	}
}

// offerState is an offer the agent retained and acknowledged.
type offerState struct {
	o       *pb.SessionOffer
	c       claims
	d       *pb.DialInstruction
	started bool
}

var errUnknownPresenceLease = errors.New("relay offer presence lease is not yet known")

// admitOffer verifies an offer's grant against the presence lease, decrypts and
// checks its dial instruction, and retains it durably, as tunnel/v2 requires
// before an acknowledgement. An error refuses this offer only.
func (a *Agent) admitOffer(ctx context.Context, o *pb.SessionOffer, lease claims, leases map[string]claims, pending map[string]*offerState, now time.Time) (*offerState, error) {
	if len(pending) >= 64 {
		return nil, status.Error(codes.ResourceExhausted, "too many relay offers")
	}
	g, err := a.Verifier.verify(ctx, o.SessionGrantJws, grantType, lease.Aud, now)
	if err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	// Verification may have waited for a key refresh: recheck both signed
	// lifetimes before decrypting or retaining this offer.
	if now := time.Now().Unix(); lease.Exp <= now || g.Exp <= now {
		return nil, fmt.Errorf("relay offer or presence expired")
	}
	if g.Route != lease.Route || g.Session != o.SessionId || g.Agent != binding(publicDER(a.signing)) {
		return nil, fmt.Errorf("relay offer presence binding mismatch")
	}
	bound, ok := leases[g.Presence]
	if !ok {
		return nil, errUnknownPresenceLease
	}
	d, err := decryptInstruction(o.EncryptedDialInstruction, g, bound, a.encryption)
	if err == nil {
		err = validateService(d)
	}
	if err != nil {
		return nil, err
	}
	for _, prior := range pending {
		if prior.c.Session == g.Session {
			return nil, fmt.Errorf("duplicate relay session offer")
		}
	}
	raw, err := proto.Marshal(o)
	if err != nil {
		return nil, err
	}
	path := filepath.Join(a.StateDir, "offer-"+binding([]byte(o.OfferId))+".pb")
	if prev, err := os.ReadFile(path); err == nil && !bytes.Equal(prev, raw) {
		return nil, fmt.Errorf("conflicting persisted relay offer")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if now := time.Now().Unix(); lease.Exp <= now || g.Exp <= now {
		return nil, fmt.Errorf("relay offer or presence expired")
	}
	if err = writeState(path, raw); err != nil {
		return nil, err
	}
	return &offerState{o: o, c: g, d: d}, nil
}

func (a *Agent) serve(ctx context.Context, broker *pb.BrokerInstance, o *pb.SessionOffer, c claims, d *pb.DialInstruction) error {
	session, err := join(ctx, broker, o.SessionGrantJws, c, a.signing, pb.JoinRole_JOIN_ROLE_AGENT, a.Verifier.dialRelay)
	if err != nil {
		return err
	}
	defer session.Close()
	if d.Transport == pb.DialTransport_DIAL_TRANSPORT_DATAGRAM {
		return serveDatagrams(session, d)
	}
	port := int(d.Port)
	agentSession := a.isAgentSession(string(d.ServiceDescriptor), port)
	if agentSession {
		port = a.MTLSPort
	}
	local, err := dialLocalService(ctx, d.Host, strconv.Itoa(port), agentSession)
	if err != nil {
		return err
	}
	defer local.Close()
	done := make(chan error, 1)
	go func() {
		buf := make([]byte, 65536)
		for {
			n, e := local.Read(buf)
			if n > 0 {
				if e = session.Send(buf[:n], false); e != nil {
					done <- e
					return
				}
			}
			if e != nil {
				if e == io.EOF {
					e = session.Send(nil, true)
				}
				done <- e
				return
			}
		}
	}()
	incoming := make(chan error, 1)
	go func() {
		for {
			b, half, e := session.Recv()
			if e != nil {
				incoming <- e
				return
			}
			if half {
				if tcp, ok := local.(*net.TCPConn); ok {
					_ = tcp.CloseWrite()
				}
				incoming <- nil
				return
			}
			if _, e = local.Write(b); e != nil {
				incoming <- e
				return
			}
		}
	}()
	// Both half-closes finish independently. Errors or cancellation close the
	// connection and unblock both pumps.
	for i := 0; i < 2; i++ {
		select {
		case e := <-done:
			if e != nil {
				return e
			}
			done = nil
		case e := <-incoming:
			if e != nil {
				return e
			}
			incoming = nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return nil
}

// Admission lasts at most 60 seconds. Files older than the maximum relay
// lifetime no longer participate in deduplication, even across restarts.
func (a *Agent) expireSavedOffers() {
	entries, err := os.ReadDir(a.StateDir)
	if err != nil {
		return
	}
	for _, entry := range entries {
		if !strings.HasPrefix(entry.Name(), "offer-") || !strings.HasSuffix(entry.Name(), ".pb") {
			continue
		}
		info, err := entry.Info()
		if err == nil && time.Since(info.ModTime()) > time.Hour {
			_ = os.Remove(filepath.Join(a.StateDir, entry.Name()))
		}
	}
}

// isAgentSession covers an app tunnel aimed at the agent's own port too, so
// no relay session reaches the agent looking like an on-device caller.
func (a *Agent) isAgentSession(descriptor string, port int) bool {
	return a.MTLSPort > 0 && (descriptor == "wendy-agent" || port == a.MTLSPort)
}

// RelaySourceIP is the loopback address relay sessions to the agent come
// from, so the agent can tell a cloud session from other loopback callers.
var RelaySourceIP = net.IPv4(127, 0, 0, 2)

// dialLocalService dials the agent's own sessions from RelaySourceIP so it can
// tell them apart; app tunnels dial plainly, as their NAT rules match 127.0.0.1.
func dialLocalService(ctx context.Context, host, port string, agentSession bool) (net.Conn, error) {
	addr := net.JoinHostPort(host, port)
	dialer := net.Dialer{Timeout: 5 * time.Second}
	if ip := net.ParseIP(host); agentSession && runtime.GOOS == "linux" && ip != nil && ip.To4() != nil && ip.IsLoopback() {
		tagged := dialer
		tagged.LocalAddr = &net.TCPAddr{IP: RelaySourceIP}
		if c, err := tagged.DialContext(ctx, "tcp", addr); err == nil {
			return c, nil
		}
	}
	return dialer.DialContext(ctx, "tcp", addr)
}
