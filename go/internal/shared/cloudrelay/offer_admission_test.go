package cloudrelay

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"
	pb "github.com/wendylabsinc/wendy/go/proto/gen/relaypb"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// offerRelay is a broker whose presence stream sends whatever the test queues
// on send, reports each offer acknowledgement on acks, and reports the grant of
// each agent JoinOpen on joins before refusing the join.
type offerRelay struct {
	*testRelay
	send  chan *pb.RegisterPresenceResponse
	acks  chan *pb.PresenceOfferAcknowledgement
	joins chan string
}

func (r *offerRelay) RegisterPresence(s grpc.BidiStreamingServer[pb.RegisterPresenceRequest, pb.RegisterPresenceResponse]) error {
	m, e := s.Recv()
	if e != nil {
		return e
	}
	o := m.GetOpen()
	if o == nil {
		return fmt.Errorf("expected presence open")
	}
	c, e := r.verifier.verify(s.Context(), o.PresenceLeaseJws, leaseType, r.broker().Audience, time.Now())
	if e != nil {
		return e
	}
	p, e := x509.ParsePKIXPublicKey(o.AgentSigningPublicKeySpkiDer)
	if e != nil {
		return e
	}
	ch := challenge()
	if e = s.Send(&pb.RegisterPresenceResponse{Message: &pb.RegisterPresenceResponse_Challenge{Challenge: ch}}); e != nil {
		return e
	}
	if m, e = s.Recv(); e != nil {
		return e
	}
	if proof := m.GetProof(); proof == nil || !bytes.Equal(proof.ChallengeId, ch.ChallengeId) || !verifyProof(p.(*ecdsa.PublicKey), "presence", c, o.PresenceLeaseJws, "agent-presence", ch, proof.Signature) {
		return fmt.Errorf("invalid presence holder proof")
	}
	if e = s.Send(&pb.RegisterPresenceResponse{Message: &pb.RegisterPresenceResponse_Accepted{Accepted: &pb.PresenceAccepted{LeaseExpiresAt: timestamppb.New(time.Unix(c.Exp, 0))}}}); e != nil {
		return e
	}
	r.mu.Lock()
	r.registrations++
	if r.registrations == 1 {
		close(r.present)
	}
	r.mu.Unlock()
	// gRPC streams allow one sender at a time, so only this loop sends.
	received := make(chan *pb.PresenceOfferAcknowledgement)
	failed := make(chan error, 1)
	go func() {
		for {
			m, e := s.Recv()
			if e != nil {
				failed <- e
				return
			}
			ack := m.GetOfferAcknowledgement()
			if ack == nil {
				failed <- fmt.Errorf("unexpected presence message")
				return
			}
			select {
			case received <- ack:
			case <-s.Context().Done():
				return
			}
		}
	}()
	for {
		select {
		case m := <-r.send:
			if e = s.Send(m); e != nil {
				return e
			}
		case ack := <-received:
			r.acks <- ack
		case e = <-failed:
			return e
		case <-s.Context().Done():
			return s.Context().Err()
		}
	}
}

func (r *offerRelay) JoinSession(s grpc.BidiStreamingServer[pb.JoinSessionRequest, pb.JoinSessionResponse]) error {
	m, e := s.Recv()
	if e != nil {
		return e
	}
	o := m.GetOpen()
	if o == nil || o.Role != pb.JoinRole_JOIN_ROLE_AGENT {
		return fmt.Errorf("expected agent join")
	}
	r.joins <- o.SessionGrantJws
	return status.Error(codes.FailedPrecondition, "test broker ends every join")
}

// sessionOffer seals d (nil: wendy-agent) for the current lease the way Cloud
// does, after edit adjusts the grant claims.
func (r *testRelay) sessionOffer(offerID string, d *pb.DialInstruction, edit func(*claims)) *pb.SessionOffer {
	r.t.Helper()
	r.mu.Lock()
	defer r.mu.Unlock()
	now := time.Now().Unix()
	g := claims{Iss: r.verifier.Issuer, Aud: r.broker().Audience, Iat: now, Exp: now + 60, JTI: uuid.NewString(), Correlation: "opaque-correlation", Route: r.lease.Route, Session: uuid.NewString(), Presence: r.lease.JTI, Request: binding([]byte(offerID)), Attestation: binding([]byte("attestation")), Caller: binding([]byte("caller")), Agent: r.lease.Signing, RelayExp: now + 300}
	if edit != nil {
		edit(&g)
	}
	envelope, e := sealTestInstruction(r.encryption, g, d)
	if e != nil {
		r.t.Fatal(e)
	}
	g.Dial = binding(canonical([]byte("wendycloud.tunnel.v1/hpke-envelope/1"), []byte("DHKEM(P-256,HKDF-SHA256)+HKDF-SHA256+AES-128-GCM"), envelope.EncapsulatedKey, envelope.Info, envelope.Aad, envelope.Ciphertext))
	return &pb.SessionOffer{SessionGrantJws: r.artifact(g, grantType), EncryptedDialInstruction: envelope, OfferId: offerID, SessionId: g.Session}
}

func offerMessage(o *pb.SessionOffer) *pb.RegisterPresenceResponse {
	return &pb.RegisterPresenceResponse{Message: &pb.RegisterPresenceResponse_SessionOffer{SessionOffer: o}}
}

func acknowledgedMessage(a *pb.PresenceOfferAcknowledgement) *pb.RegisterPresenceResponse {
	return &pb.RegisterPresenceResponse{Message: &pb.RegisterPresenceResponse_OfferAcknowledged{OfferAcknowledged: &pb.PresenceOfferAcknowledged{OfferId: a.OfferId, SessionId: a.SessionId}}}
}

// startOfferRelay runs one presence attempt against an offerRelay and returns
// once presence is registered. done receives runOnce's result.
func startOfferRelay(t *testing.T, ctx context.Context, prepare func(*Agent)) (*offerRelay, *Agent, <-chan error, *observer.ObservedLogs) {
	t.Helper()
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	verifier := &Verifier{Issuer: "https://cloud.test", fetched: time.Now(), keys: map[string]jwk{"test": {Kid: "test", Kty: "EC", Alg: "ES256", Crv: "P-256", X: b64.EncodeToString(key.X.FillBytes(make([]byte, 32))), Y: b64.EncodeToString(key.Y.FillBytes(make([]byte, 32)))}}}
	listener, e := net.Listen("tcp", "127.0.0.1:0")
	if e != nil {
		t.Fatal(e)
	}
	r := &offerRelay{
		testRelay: &testRelay{t: t, endpoint: listener.Addr().String(), cloudKey: key, verifier: verifier, present: make(chan struct{})},
		send:      make(chan *pb.RegisterPresenceResponse),
		acks:      make(chan *pb.PresenceOfferAcknowledgement, 8),
		joins:     make(chan string, 8),
	}
	srv := grpc.NewServer()
	pb.RegisterTunnelAuthorizationServiceServer(srv, r)
	pb.RegisterTunnelBrokerV2ServiceServer(srv, r)
	go srv.Serve(listener)
	t.Cleanup(srv.Stop)
	dial := func(endpoint string) (*grpc.ClientConn, error) {
		return grpc.NewClient(endpoint, grpc.WithTransportCredentials(insecure.NewCredentials()))
	}
	verifier.relayDial = dial
	core, logs := observer.New(zapcore.WarnLevel)
	a := &Agent{Endpoint: r.endpoint, Verifier: verifier, StateDir: t.TempDir(), Credentials: func() (string, string, []byte) { return "", "", nil }, Logger: zap.New(core), dialCloud: func(endpoint, _, _ string, _ []byte) (*grpc.ClientConn, error) { return dial(endpoint) }}
	if prepare != nil {
		prepare(a)
	}
	done := make(chan error, 1)
	go func() { done <- a.runOnce(ctx) }()
	select {
	case <-r.present:
	case err := <-done:
		t.Fatalf("presence failed: %v", err)
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	return r, a, done, logs
}

// queue hands m to the broker's presence stream, failing if presence ended.
func (r *offerRelay) queue(t *testing.T, ctx context.Context, done <-chan error, m *pb.RegisterPresenceResponse) {
	t.Helper()
	select {
	case r.send <- m:
	case err := <-done:
		t.Fatalf("presence ended: %v", err)
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
}

func (r *offerRelay) nextAck(t *testing.T, ctx context.Context, done <-chan error) *pb.PresenceOfferAcknowledgement {
	t.Helper()
	select {
	case ack := <-r.acks:
		return ack
	case err := <-done:
		t.Fatalf("presence ended: %v", err)
	case <-ctx.Done():
		t.Fatal("no offer acknowledgement")
	}
	return nil
}

func (r *offerRelay) nextJoin(t *testing.T, ctx context.Context, done <-chan error) string {
	t.Helper()
	select {
	case grant := <-r.joins:
		return grant
	case err := <-done:
		t.Fatalf("presence ended: %v", err)
	case <-ctx.Done():
		t.Fatal("offer was never served")
	}
	return ""
}

// One offer the agent cannot admit must cost only that offer (tunnel/v2: no
// decryption and no dial), not the presence stream and its lease (WDY-3939
// #16). A grant up to cloudClockSkew ahead of the device clock is admitted
// (WDY-3939 #11). A refused offer redelivered on the same stream is ignored.
func TestRejectedOfferKeepsPresence(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	r, a, done, logs := startOfferRelay(t, ctx, nil)

	badSignature := r.sessionOffer("bad-signature", nil, nil)
	badSignature.SessionGrantJws = badSignature.SessionGrantJws[:len(badSignature.SessionGrantJws)-8] + "AAAAAAAA"
	unsupported := r.sessionOffer("unsupported-service", &pb.DialInstruction{Transport: pb.DialTransport_DIAL_TRANSPORT_TCP, Host: "127.0.0.1", Port: 5900, ServiceDescriptor: []byte("vnc")}, nil)
	future := r.sessionOffer("iat-beyond-skew", nil, func(g *claims) {
		g.Iat += int64(cloudClockSkew/time.Second) + 30
		g.Exp, g.RelayExp = g.Iat+60, g.Iat+300
	})
	skewed := r.sessionOffer("iat-within-skew", nil, func(g *claims) {
		g.Iat += 2
		g.Exp, g.RelayExp = g.Iat+60, g.Iat+300
	})
	good := r.sessionOffer("good", nil, nil)
	for _, o := range []*pb.SessionOffer{badSignature, unsupported, future, badSignature, skewed, good} {
		r.queue(t, ctx, done, offerMessage(o))
	}
	// Offers are handled in order, so an acknowledgement of any refused offer
	// would arrive before these.
	for _, want := range []*pb.SessionOffer{skewed, good} {
		ack := r.nextAck(t, ctx, done)
		if ack.OfferId != want.OfferId || ack.SessionId != want.SessionId {
			t.Fatalf("acknowledged %q, want %q", ack.OfferId, want.OfferId)
		}
		r.queue(t, ctx, done, acknowledgedMessage(ack))
		if grant := r.nextJoin(t, ctx, done); grant != want.SessionGrantJws {
			t.Fatalf("joined with the wrong grant for %q", want.OfferId)
		}
	}
	select {
	case err := <-done:
		t.Fatalf("presence ended: %v", err)
	default:
	}
	for _, o := range []*pb.SessionOffer{badSignature, unsupported, future} {
		if _, err := os.Stat(filepath.Join(a.StateDir, "offer-"+binding([]byte(o.OfferId))+".pb")); !os.IsNotExist(err) {
			t.Fatalf("refused offer %q was retained (stat err = %v)", o.OfferId, err)
		}
	}
	refused := map[string]int{}
	for _, entry := range logs.FilterMessage("refused cloud relay offer").All() {
		refused[entry.ContextMap()["offer_id"].(string)]++
	}
	want := map[string]int{"bad-signature": 1, "unsupported-service": 1, "iat-beyond-skew": 1}
	if fmt.Sprint(refused) != fmt.Sprint(want) {
		t.Fatalf("refusals logged %v, want %v", refused, want)
	}
}

// A session past the agent's concurrent-session limit is refused on its own;
// presence stays up and serves the next session once a slot is free.
func TestFullSessionSlotsKeepPresence(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	r, a, done, logs := startOfferRelay(t, ctx, func(a *Agent) {
		a.slots = make(chan struct{}, 1)
		a.slots <- struct{}{}
	})

	first := r.sessionOffer("first", nil, nil)
	r.queue(t, ctx, done, offerMessage(first))
	r.queue(t, ctx, done, acknowledgedMessage(r.nextAck(t, ctx, done)))
	second := r.sessionOffer("second", nil, nil)
	r.queue(t, ctx, done, offerMessage(second))
	// The second acknowledgement follows the first confirmation, which the
	// agent handled with every slot taken.
	ack := r.nextAck(t, ctx, done)
	if ack.OfferId != second.OfferId {
		t.Fatalf("acknowledged %q, want %q", ack.OfferId, second.OfferId)
	}
	if logs.FilterMessage("refused cloud relay session").Len() != 1 {
		t.Fatal("full session slots were not reported")
	}
	<-a.slots
	r.queue(t, ctx, done, acknowledgedMessage(ack))
	if grant := r.nextJoin(t, ctx, done); grant != second.SessionGrantJws {
		t.Fatal("served the session that was refused for lack of a slot")
	}
	select {
	case err := <-done:
		t.Fatalf("presence ended: %v", err)
	default:
	}
}
