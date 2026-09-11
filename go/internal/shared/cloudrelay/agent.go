package cloudrelay

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	pb "github.com/wendylabsinc/wendy/go/proto/gen/relaypb"
	"go.uber.org/zap"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
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
	signing, encryption *ecdsa.PrivateKey
	lease               *pb.IssuePresenceLeaseResponse
	leaseClaims         claims
	slots               chan struct{}
}

func (a *Agent) Run(ctx context.Context) {
	backoff := time.Second
	for ctx.Err() == nil {
		started := time.Now()
		err := a.runOnce(ctx)
		if ctx.Err() != nil {
			return
		}
		if err != nil {
			a.Logger.Warn("cloud relay connection failed, reconnecting", zap.Error(err), zap.Duration("backoff", backoff))
		}
		if time.Since(started) > time.Minute {
			backoff = time.Second
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
func (a *Agent) issue(ctx context.Context) (*pb.IssuePresenceLeaseResponse, claims, error) {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
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
		return nil, claims{}, fmt.Errorf("requesting presence lease from %s (%s): %w", a.Endpoint, pb.TunnelAuthorizationService_IssuePresenceLease_FullMethodName, err)
	}
	c, err := a.validateLease(ctx, l)
	if err != nil {
		return nil, c, err
	}
	raw, err := proto.Marshal(l)
	if err != nil {
		return nil, c, err
	}
	if err = writeState(filepath.Join(a.StateDir, "lease.pb"), raw); err != nil {
		return nil, c, err
	}
	return l, c, nil
}
func renewalDelay(l *pb.IssuePresenceLeaseResponse, c claims) time.Duration {
	when := time.Unix(c.Exp, 0).Add(-30 * time.Second)
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
					a.lease = &l
					a.leaseClaims = c
				}
			}
		}
	}
	if a.lease == nil || time.Until(time.Unix(a.leaseClaims.Exp, 0)) < 30*time.Second {
		l, c, e := a.issue(ctx)
		if e != nil {
			return e
		}
		a.lease = l
		a.leaseClaims = c
	}
	lease, c := a.lease, a.leaseClaims
	conn, err := a.Verifier.dialRelay(lease.Broker.Endpoint)
	if err != nil {
		return err
	}
	defer conn.Close()
	stream, err := pb.NewTunnelBrokerV2ServiceClient(conn).RegisterPresence(metadata.NewOutgoingContext(ctx, metadata.MD{}))
	if err != nil {
		return err
	}
	if err = stream.Send(&pb.RegisterPresenceRequest{Message: &pb.RegisterPresenceRequest_Open{Open: &pb.PresenceOpen{PresenceLeaseJws: lease.PresenceLeaseJws, AgentSigningPublicKeySpkiDer: publicDER(a.signing)}}}); err != nil {
		return err
	}
	admission := time.AfterFunc(30*time.Second, cancel)
	msg, err := stream.Recv()
	if err != nil {
		admission.Stop()
		return fmt.Errorf("opening v2 presence: %w", err)
	}
	sig, err := proof(a.signing, "presence", c.Aud, c.Route, c.JTI, lease.PresenceLeaseJws, "agent-presence", msg.GetChallenge())
	if err != nil {
		admission.Stop()
		return err
	}
	err = stream.Send(&pb.RegisterPresenceRequest{Message: &pb.RegisterPresenceRequest_Proof{Proof: &pb.PresenceProof{ChallengeId: msg.GetChallenge().ChallengeId, Signature: sig}}})
	if err != nil {
		admission.Stop()
		return err
	}
	msg, err = stream.Recv()
	admission.Stop()
	if err != nil {
		return err
	}
	accepted := msg.GetAccepted()
	if accepted == nil || accepted.LeaseExpiresAt == nil || accepted.LeaseExpiresAt.CheckValid() != nil || accepted.LeaseExpiresAt.AsTime().Unix() != c.Exp {
		return fmt.Errorf("invalid presence acceptance")
	}
	a.Logger.Info("registered presence with cloud relay", zap.String("endpoint", lease.Broker.Endpoint))
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
	type offerState struct {
		o       *pb.SessionOffer
		c       claims
		d       *pb.DialInstruction
		started bool
	}
	pending := map[string]*offerState{}
	// Keep earlier lease bindings until they expire, so in-flight offers remain
	// verifiable when a renewal changes the presence jti.
	leases := map[string]claims{c.JTI: c}
	var renewing *pb.IssuePresenceLeaseResponse
	var renewedClaims claims
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-expiry.C:
			return fmt.Errorf("Cloud presence lease expired")
		case <-renewal.C:
			l, n, e := a.issue(ctx)
			if e != nil {
				return e
			}
			if n.Aud != c.Aud || n.Route != c.Route || l.Broker.Endpoint != lease.Broker.Endpoint {
				a.lease = l
				a.leaseClaims = n
				return fmt.Errorf("Cloud selected a new presence route; reconnecting")
			}
			renewing = l
			renewedClaims = n
			if e = stream.Send(&pb.RegisterPresenceRequest{Message: &pb.RegisterPresenceRequest_Renewal{Renewal: &pb.PresenceRenewal{PresenceLeaseJws: l.PresenceLeaseJws}}}); e != nil {
				return e
			}
		case r := <-ch:
			if r.e != nil {
				return r.e
			}
			switch m := r.m.Message.(type) {
			case *pb.RegisterPresenceResponse_Renewed:
				if renewing == nil || m.Renewed.LeaseExpiresAt == nil || m.Renewed.LeaseExpiresAt.AsTime().Unix() != renewedClaims.Exp {
					return fmt.Errorf("invalid presence renewal acceptance")
				}
				lease = renewing
				c = renewedClaims
				a.lease = lease
				a.leaseClaims = c
				leases[c.JTI] = c
				renewing = nil
				renewal.Reset(renewalDelay(lease, c))
				expiry.Reset(time.Until(time.Unix(c.Exp, 0)))
			case *pb.RegisterPresenceResponse_SessionOffer:
				o := m.SessionOffer
				if !bounded(o.OfferId, 128) || !bounded(o.SessionId, 128) {
					return fmt.Errorf("invalid relay offer")
				}
				now := time.Now()
				for id, s := range pending {
					if s.c.Exp <= now.Unix() {
						delete(pending, id)
						_ = os.Remove(filepath.Join(a.StateDir, "offer-"+binding([]byte(id))+".pb"))
					}
				}
				for id, l := range leases {
					if l.Exp <= now.Unix() {
						delete(leases, id)
					}
				}
				if prior := pending[o.OfferId]; prior != nil {
					if !proto.Equal(prior.o, o) {
						return fmt.Errorf("conflicting relay offer")
					}
				} else {
					if len(pending) >= 64 {
						return status.Error(codes.ResourceExhausted, "too many relay offers")
					}
					g, e := a.Verifier.verify(ctx, o.SessionGrantJws, grantType, c.Aud, now)
					if e != nil {
						return e
					}
					bound, ok := leases[g.Presence]
					if !ok || g.Route != c.Route || g.Session != o.SessionId || g.Agent != binding(publicDER(a.signing)) {
						return fmt.Errorf("relay offer presence binding mismatch")
					}
					d, e := decryptInstruction(o.EncryptedDialInstruction, g, bound, a.encryption)
					if e == nil {
						e = validateService(d)
					}
					if e != nil {
						return e
					}
					for _, prior := range pending {
						if prior.c.Session == g.Session {
							return fmt.Errorf("duplicate relay session offer")
						}
					}
					raw, e := proto.Marshal(o)
					if e != nil {
						return e
					}
					path := filepath.Join(a.StateDir, "offer-"+binding([]byte(o.OfferId))+".pb")
					if prev, e := os.ReadFile(path); e == nil && !bytes.Equal(prev, raw) {
						return fmt.Errorf("conflicting persisted relay offer")
					}
					if e = writeState(path, raw); e != nil {
						return e
					}
					pending[o.OfferId] = &offerState{o: o, c: g, d: d}
				}
				if err = stream.Send(&pb.RegisterPresenceRequest{Message: &pb.RegisterPresenceRequest_OfferAcknowledgement{OfferAcknowledgement: &pb.PresenceOfferAcknowledgement{OfferId: o.OfferId, SessionId: o.SessionId}}}); err != nil {
					return err
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
						return status.Error(codes.ResourceExhausted, "too many active relay sessions")
					}
					s.started = true
					broker := lease.Broker
					go func() {
						defer func() { <-a.slots }()
						if e := a.serve(ctx, broker, s.o, s.c, s.d); e != nil && ctx.Err() == nil {
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
func (a *Agent) serve(ctx context.Context, broker *pb.BrokerInstance, o *pb.SessionOffer, c claims, d *pb.DialInstruction) error {
	session, err := join(ctx, broker, o.SessionGrantJws, c, a.signing, pb.JoinRole_JOIN_ROLE_AGENT, a.Verifier.dialRelay)
	if err != nil {
		return err
	}
	defer session.Close()
	port := int(d.Port)
	if string(d.ServiceDescriptor) == "wendy-agent" && a.MTLSPort > 0 {
		port = a.MTLSPort
	}
	dialer := net.Dialer{Timeout: 5 * time.Second}
	local, err := dialer.DialContext(ctx, "tcp", net.JoinHostPort(d.Host, strconv.Itoa(port)))
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
