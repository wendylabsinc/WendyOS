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
	"math/big"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cloudflare/circl/hpke"
	"github.com/google/uuid"
	pb "github.com/wendylabsinc/wendy/go/proto/gen/relaypb"
	"go.uber.org/zap"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

type testRelay struct {
	renewOnly bool
	renewed   chan struct{}
	pb.UnimplementedTunnelAuthorizationServiceServer
	pb.UnimplementedTunnelBrokerV2ServiceServer
	t                   *testing.T
	endpoint            string
	cloudKey            *ecdsa.PrivateKey
	verifier            *Verifier
	mu                  sync.Mutex
	lease               claims
	leaseJWS            string
	signing, encryption []byte
	requested           chan *pb.RequestTunnelRequest
	offers              chan *pb.SessionOffer
	present             chan struct{}
	ack                 chan struct{}
	paired              chan struct{}
	joins               int
	joined              map[pb.JoinRole]bool
	grant               claims
	callerPub           *ecdsa.PublicKey
	grantJWS            string
	toCaller, toAgent   chan *pb.TunnelFrame
}

func (r *testRelay) artifact(c claims, typ string) string {
	raw, _ := json.Marshal(c)
	var obj map[string]any
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	_ = decoder.Decode(&obj)
	payload, _ := canonicalJSON(obj)
	header, _ := canonicalJSON(map[string]string{"alg": "ES256", "kid": "test", "typ": typ})
	input := b64.EncodeToString(header) + "." + b64.EncodeToString(payload)
	x, y, err := ecdsa.Sign(rand.Reader, r.cloudKey, digest([]byte(input)))
	if err != nil {
		r.t.Fatal(err)
	}
	sig := make([]byte, 64)
	x.FillBytes(sig[:32])
	y.FillBytes(sig[32:])
	return input + "." + b64.EncodeToString(sig)
}
func (r *testRelay) broker() *pb.BrokerInstance {
	return &pb.BrokerInstance{Endpoint: r.endpoint, Audience: "test-relay/incarnation/AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"}
}
func (r *testRelay) IssuePresenceLease(ctx context.Context, req *pb.IssuePresenceLeaseRequest) (*pb.IssuePresenceLeaseResponse, error) {
	if bytes.Equal(req.AgentSigningPublicKeySpkiDer, req.AgentKeyAgreementPublicKeySpkiDer) {
		return nil, fmt.Errorf("reused signing/encryption keys")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.signing = bytes.Clone(req.AgentSigningPublicKeySpkiDer)
	r.encryption = bytes.Clone(req.AgentKeyAgreementPublicKeySpkiDer)
	now := time.Now().Unix()
	r.lease = claims{Iss: r.verifier.Issuer, Aud: r.broker().Audience, Iat: now, Exp: now + 300, JTI: uuid.NewString(), Correlation: req.CorrelationId, Route: "opaque-route", Signing: binding(r.signing), Encryption: binding(r.encryption)}
	r.leaseJWS = r.artifact(r.lease, leaseType)
	renewAfter := time.Now().Add(2 * time.Minute)
	if r.renewOnly {
		renewAfter = time.Now().Add(time.Millisecond)
	}
	return &pb.IssuePresenceLeaseResponse{PresenceLeaseJws: r.leaseJWS, Broker: r.broker(), RenewAfter: timestamppb.New(renewAfter)}, nil
}
func assertAnonymous(t *testing.T, ctx context.Context) {
	t.Helper()
	md, _ := metadata.FromIncomingContext(ctx)
	for _, key := range []string{"authorization", "x-forwarded-client-cert", "x-wendy-client-cert", "x-wendy-request-signature"} {
		if len(md.Get(key)) != 0 {
			t.Errorf("identity leaked to broker via %s", key)
		}
	}
}
func challenge() *pb.BrokerChallenge {
	id := make([]byte, 16)
	nonce := make([]byte, 32)
	_, _ = rand.Read(id)
	_, _ = rand.Read(nonce)
	return &pb.BrokerChallenge{ChallengeId: id, Nonce: nonce, ExpiresAt: timestamppb.New(time.Now().Add(20 * time.Second))}
}
func verifyProof(pub *ecdsa.PublicKey, domain string, c claims, artifact, role string, ch *pb.BrokerChallenge, sig []byte) bool {
	id := c.Session
	if domain == "presence" {
		id = c.Route
	}
	data := canonical([]byte("wendycloud.tunnel.v1/"+domain+"-proof/1"), []byte(c.Aud), []byte(id), []byte(c.JTI), digest([]byte(artifact)), []byte(role), ch.ChallengeId, ch.Nonce)
	return len(sig) == 64 && ecdsa.Verify(pub, digest(data), new(big.Int).SetBytes(sig[:32]), new(big.Int).SetBytes(sig[32:]))
}
func (r *testRelay) RegisterPresence(s grpc.BidiStreamingServer[pb.RegisterPresenceRequest, pb.RegisterPresenceResponse]) error {
	assertAnonymous(r.t, s.Context())
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
	pub := p.(*ecdsa.PublicKey)
	ch := challenge()
	if e = s.Send(&pb.RegisterPresenceResponse{Message: &pb.RegisterPresenceResponse_Challenge{Challenge: ch}}); e != nil {
		return e
	}
	m, e = s.Recv()
	if e != nil {
		return e
	}
	pmsg := m.GetProof()
	if pmsg == nil || !bytes.Equal(pmsg.ChallengeId, ch.ChallengeId) || !verifyProof(pub, "presence", c, o.PresenceLeaseJws, "agent-presence", ch, pmsg.Signature) {
		return fmt.Errorf("invalid presence holder proof")
	}
	if e = s.Send(&pb.RegisterPresenceResponse{Message: &pb.RegisterPresenceResponse_Accepted{Accepted: &pb.PresenceAccepted{LeaseExpiresAt: timestamppb.New(time.Unix(c.Exp, 0))}}}); e != nil {
		return e
	}
	close(r.present)
	if r.renewOnly {
		m, e := s.Recv()
		if e != nil {
			return e
		}
		renewal := m.GetRenewal()
		if renewal == nil {
			return fmt.Errorf("expected renewal")
		}
		renewed, e := r.verifier.verify(s.Context(), renewal.PresenceLeaseJws, leaseType, c.Aud, time.Now())
		if e != nil {
			return e
		}
		if renewed.JTI == c.JTI || renewed.Route != c.Route || renewed.Signing != c.Signing || renewed.Encryption != c.Encryption {
			return fmt.Errorf("invalid renewed lease bindings")
		}
		if e = s.Send(&pb.RegisterPresenceResponse{Message: &pb.RegisterPresenceResponse_Renewed{Renewed: &pb.PresenceRenewed{LeaseExpiresAt: timestamppb.New(time.Unix(renewed.Exp, 0))}}}); e != nil {
			return e
		}
		close(r.renewed)
		<-s.Context().Done()
		return s.Context().Err()
	}
	var offer *pb.SessionOffer
	select {
	case offer = <-r.offers:
	case <-s.Context().Done():
		return s.Context().Err()
	}
	if e = s.Send(&pb.RegisterPresenceResponse{Message: &pb.RegisterPresenceResponse_SessionOffer{SessionOffer: offer}}); e != nil {
		return e
	}
	m, e = s.Recv()
	if e != nil {
		return e
	}
	ack := m.GetOfferAcknowledgement()
	if ack == nil || ack.SessionId != offer.SessionId || ack.OfferId != offer.OfferId {
		return fmt.Errorf("invalid offer acknowledgement")
	}
	close(r.ack)
	if e = s.Send(&pb.RegisterPresenceResponse{Message: &pb.RegisterPresenceResponse_OfferAcknowledged{OfferAcknowledged: &pb.PresenceOfferAcknowledged{OfferId: ack.OfferId, SessionId: ack.SessionId}}}); e != nil {
		return e
	}
	<-s.Context().Done()
	return s.Context().Err()
}
func (r *testRelay) RequestTunnel(ctx context.Context, req *pb.RequestTunnelRequest) (*pb.RequestTunnelResponse, error) {
	r.requested <- req
	md, _ := metadata.FromIncomingContext(ctx)
	if md.Get("authorization")[0] != "Bearer operator-test" {
		return nil, fmt.Errorf("missing operator auth")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	var descriptor map[string]any
	if e := json.Unmarshal(req.PrincipalRequest.Value, &descriptor); e != nil {
		return nil, e
	}
	body, _ := proto.MarshalOptions{Deterministic: true}.Marshal(req.RequestBody)
	target := descriptor["target"].(map[string]any)
	if req.RequestBody.TargetAssetId != "00000000-0000-4000-8000-000000000042" || target["asset_id"] != req.RequestBody.TargetAssetId || descriptor["body_sha256"] != binding(body) || target["service"] != "wendy-agent" {
		return nil, fmt.Errorf("UUID or signed request body mismatch")
	}
	now := time.Now().Unix()
	g := claims{Iss: r.verifier.Issuer, Aud: r.broker().Audience, Iat: now, Exp: now + 60, JTI: uuid.NewString(), Correlation: "opaque-correlation", Route: r.lease.Route, Session: "opaque-session", Presence: r.lease.JTI, Request: binding(req.PrincipalRequest.Value), Attestation: binding([]byte("attestation")), Caller: binding(req.RequestBody.CallerSigningPublicKeySpkiDer), Agent: r.lease.Signing, RelayExp: now + 300}
	envelope, e := sealTestInstruction(r.encryption, g)
	if e != nil {
		return nil, e
	}
	g.Dial = binding(canonical([]byte("wendycloud.tunnel.v1/hpke-envelope/1"), []byte("DHKEM(P-256,HKDF-SHA256)+HKDF-SHA256+AES-128-GCM"), envelope.EncapsulatedKey, envelope.Info, envelope.Aad, envelope.Ciphertext))
	r.grant = g
	r.grantJWS = r.artifact(g, grantType)
	pub, e := x509.ParsePKIXPublicKey(req.RequestBody.CallerSigningPublicKeySpkiDer)
	if e != nil {
		return nil, e
	}
	r.callerPub = pub.(*ecdsa.PublicKey)
	r.offers <- &pb.SessionOffer{SessionGrantJws: r.grantJWS, EncryptedDialInstruction: envelope, OfferId: "opaque-offer", SessionId: g.Session}
	return &pb.RequestTunnelResponse{SessionGrantJws: r.grantJWS, Broker: r.broker()}, nil
}
func sealTestInstruction(spki []byte, g claims) (*pb.EncryptedDialInstructionEnvelope, error) {
	pub, e := x509.ParsePKIXPublicKey(spki)
	if e != nil {
		return nil, e
	}
	p := pub.(*ecdsa.PublicKey)
	kemPub, e := hpke.KEM_P256_HKDF_SHA256.Scheme().UnmarshalBinaryPublicKey(elliptic.Marshal(elliptic.P256(), p.X, p.Y))
	if e != nil {
		return nil, e
	}
	info := canonical([]byte("wendycloud.tunnel.v1/hpke-info/1"), []byte(g.Session), []byte(g.Route), digest(spki))
	request, _ := decode64(g.Request)
	aad := canonical([]byte("wendycloud.tunnel.v1/hpke-aad/1"), []byte(g.Session), request)
	d := &pb.DialInstruction{SessionId: g.Session, Transport: pb.DialTransport_DIAL_TRANSPORT_TCP, Host: "127.0.0.1", Port: 50052, RelayExpiresAtUnixSeconds: g.RelayExp, ServiceDescriptor: []byte("wendy-agent")}
	raw, _ := proto.Marshal(d)
	d.Padding = make([]byte, 4096-len(raw)-3)
	_, _ = rand.Read(d.Padding)
	raw, e = proto.MarshalOptions{Deterministic: true}.Marshal(d)
	if e != nil || len(raw) != 4096 {
		return nil, fmt.Errorf("padding size")
	}
	suite := hpke.NewSuite(hpke.KEM_P256_HKDF_SHA256, hpke.KDF_HKDF_SHA256, hpke.AEAD_AES128GCM)
	sender, e := suite.NewSender(kemPub, info)
	if e != nil {
		return nil, e
	}
	enc, sealer, e := sender.Setup(rand.Reader)
	if e != nil {
		return nil, e
	}
	ciphertext, e := sealer.Seal(raw, aad)
	if e != nil {
		return nil, e
	}
	return &pb.EncryptedDialInstructionEnvelope{Suite: 1, EncapsulatedKey: enc, Info: info, Aad: aad, Ciphertext: ciphertext}, nil
}
func (r *testRelay) JoinSession(s grpc.BidiStreamingServer[pb.JoinSessionRequest, pb.JoinSessionResponse]) error {
	assertAnonymous(r.t, s.Context())
	m, e := s.Recv()
	if e != nil {
		return e
	}
	o := m.GetOpen()
	if o == nil {
		return fmt.Errorf("expected join open")
	}
	r.mu.Lock()
	c := r.grant
	artifact := r.grantJWS
	pub := r.callerPub
	r.mu.Unlock()
	label := "caller"
	if o.Role == pb.JoinRole_JOIN_ROLE_AGENT {
		select {
		case <-r.ack:
		default:
			return fmt.Errorf("agent joined before acknowledging")
		}
		r.mu.Lock()
		p, e := x509.ParsePKIXPublicKey(r.signing)
		r.mu.Unlock()
		if e != nil {
			return e
		}
		pub = p.(*ecdsa.PublicKey)
		label = "agent"
	}
	if o.SessionGrantJws != artifact {
		return fmt.Errorf("wrong session grant")
	}
	ch := challenge()
	if e = s.Send(&pb.JoinSessionResponse{Message: &pb.JoinSessionResponse_Challenge{Challenge: ch}}); e != nil {
		return e
	}
	m, e = s.Recv()
	if e != nil {
		return e
	}
	p := m.GetProof()
	if p == nil || !bytes.Equal(p.ChallengeId, ch.ChallengeId) || !verifyProof(pub, "join", c, artifact, label, ch, p.Signature) {
		return fmt.Errorf("invalid join holder proof")
	}
	r.mu.Lock()
	if r.joined[o.Role] {
		r.mu.Unlock()
		return fmt.Errorf("duplicate role")
	}
	r.joined[o.Role] = true
	r.joins++
	if r.joins == 2 {
		close(r.paired)
	}
	r.mu.Unlock()
	select {
	case <-r.paired:
	case <-s.Context().Done():
		return s.Context().Err()
	}
	if e = s.Send(&pb.JoinSessionResponse{Message: &pb.JoinSessionResponse_Accepted{Accepted: &pb.JoinAccepted{Role: o.Role, RelayExpiresAt: timestamppb.New(time.Unix(c.RelayExp, 0))}}}); e != nil {
		return e
	}
	incoming, outgoing := r.toAgent, r.toCaller
	if o.Role == pb.JoinRole_JOIN_ROLE_AGENT {
		incoming, outgoing = r.toCaller, r.toAgent
	}
	errs := make(chan error, 1)
	go func() {
		for {
			m, e := s.Recv()
			if e != nil {
				errs <- e
				return
			}
			if m.GetFrame() == nil {
				errs <- fmt.Errorf("expected frame")
				return
			}
			select {
			case incoming <- m.GetFrame():
			case <-s.Context().Done():
				return
			}
		}
	}()
	for {
		select {
		case f := <-outgoing:
			if e = s.Send(&pb.JoinSessionResponse{Message: &pb.JoinSessionResponse_Frame{Frame: f}}); e != nil {
				return e
			}
		case e := <-errs:
			return e
		case <-s.Context().Done():
			return s.Context().Err()
		}
	}
}
func TestAuthorizedRelayEndToEnd(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	key, e := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if e != nil {
		t.Fatal(e)
	}
	verifier := &Verifier{Issuer: "https://cloud.test", fetched: time.Now(), keys: map[string]jwk{"test": {Kid: "test", Kty: "EC", Alg: "ES256", Crv: "P-256", X: b64.EncodeToString(key.X.FillBytes(make([]byte, 32))), Y: b64.EncodeToString(key.Y.FillBytes(make([]byte, 32)))}}}
	listener, e := net.Listen("tcp", "127.0.0.1:0")
	if e != nil {
		t.Fatal(e)
	}
	r := &testRelay{t: t, endpoint: listener.Addr().String(), cloudKey: key, verifier: verifier, requested: make(chan *pb.RequestTunnelRequest, 1), offers: make(chan *pb.SessionOffer, 1), present: make(chan struct{}), ack: make(chan struct{}), paired: make(chan struct{}), joined: map[pb.JoinRole]bool{}, toCaller: make(chan *pb.TunnelFrame, 8), toAgent: make(chan *pb.TunnelFrame, 8)}
	srv := grpc.NewServer()
	pb.RegisterTunnelAuthorizationServiceServer(srv, r)
	pb.RegisterTunnelBrokerV2ServiceServer(srv, r)
	go srv.Serve(listener)
	defer srv.Stop()
	dial := func(endpoint string) (*grpc.ClientConn, error) {
		return grpc.NewClient(endpoint, grpc.WithTransportCredentials(insecure.NewCredentials()))
	}
	verifier.relayDial = dial
	echo, e := net.Listen("tcp", "127.0.0.1:0")
	if e != nil {
		t.Fatal(e)
	}
	defer echo.Close()
	go func() {
		c, e := echo.Accept()
		if e != nil {
			return
		}
		defer c.Close()
		_, _ = io.Copy(c, c)
	}()
	agent := &Agent{Endpoint: r.endpoint, Verifier: verifier, StateDir: t.TempDir(), Credentials: func() (string, string, []byte) { return "", "", nil }, Logger: zap.NewNop(), MTLSPort: echo.Addr().(*net.TCPAddr).Port, dialCloud: func(endpoint, _, _ string, _ []byte) (*grpc.ClientConn, error) { return dial(endpoint) }}
	agentDone := make(chan error, 1)
	go func() {
		agentDone <- agent.runOnce(metadata.NewOutgoingContext(ctx, metadata.Pairs("authorization", "Bearer device-test")))
	}()
	select {
	case <-r.present:
	case err := <-agentDone:
		t.Fatalf("presence failed: %v", err)
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	cloudConn, e := dial(r.endpoint)
	if e != nil {
		t.Fatal(e)
	}
	defer cloudConn.Close()
	authCtx := metadata.NewOutgoingContext(ctx, metadata.Pairs("authorization", "Bearer operator-test"))
	// Signing is covered separately. This seam lets the test Cloud inspect the
	// exact descriptor/body while exercising actual gRPC admission and relay.
	conn, e := OpenTCP(authCtx, authCtx, cloudConn, verifier, "00000000-0000-4000-8000-000000000042", "wendy-agent", func(b []byte) ([]byte, error) { return b, nil })
	if e != nil {
		select {
		case err := <-agentDone:
			t.Fatalf("tunnel: %v; agent: %v", e, err)
		default:
			t.Fatal(e)
		}
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(3 * time.Second))
	payload := bytes.Repeat([]byte("relay"), 30000)
	sent := make(chan error, 1)
	go func() { _, e := conn.Write(payload); sent <- e }()
	got := make([]byte, len(payload))
	if _, e = io.ReadFull(conn, got); e != nil {
		t.Fatal(e)
	}
	if !bytes.Equal(got, payload) {
		t.Fatal("relay corrupted payload")
	}
	if e = <-sent; e != nil {
		t.Fatal(e)
	}
	// The offer was persisted before its receipt acknowledgement permitted join.
	entries, e := os.ReadDir(agent.StateDir)
	if e != nil {
		t.Fatal(e)
	}
	found := false
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), "offer-") {
			found = true
			info, _ := entry.Info()
			if info.Mode().Perm() != 0600 {
				t.Fatal("offer state permissions")
			}
		}
	}
	if !found {
		t.Fatal("offer not retained before admission")
	}
	cancel()
	select {
	case <-agentDone:
	case <-time.After(time.Second):
		t.Fatal("presence did not stop on cancellation")
	}
}

func TestAgentRenewsProvenPresence(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	verifier := &Verifier{Issuer: "https://cloud.test", fetched: time.Now(), keys: map[string]jwk{"test": {Kid: "test", Kty: "EC", Alg: "ES256", Crv: "P-256", X: b64.EncodeToString(key.X.FillBytes(make([]byte, 32))), Y: b64.EncodeToString(key.Y.FillBytes(make([]byte, 32)))}}}
	listener, e := net.Listen("tcp", "127.0.0.1:0")
	if e != nil {
		t.Fatal(e)
	}
	r := &testRelay{t: t, endpoint: listener.Addr().String(), cloudKey: key, verifier: verifier, present: make(chan struct{}), renewOnly: true, renewed: make(chan struct{})}
	srv := grpc.NewServer()
	pb.RegisterTunnelAuthorizationServiceServer(srv, r)
	pb.RegisterTunnelBrokerV2ServiceServer(srv, r)
	go srv.Serve(listener)
	defer srv.Stop()
	dial := func(endpoint string) (*grpc.ClientConn, error) {
		return grpc.NewClient(endpoint, grpc.WithTransportCredentials(insecure.NewCredentials()))
	}
	verifier.relayDial = dial
	agent := &Agent{Endpoint: r.endpoint, Verifier: verifier, StateDir: t.TempDir(), Credentials: func() (string, string, []byte) { return "", "", nil }, Logger: zap.NewNop(), dialCloud: func(endpoint, _, _ string, _ []byte) (*grpc.ClientConn, error) { return dial(endpoint) }}
	done := make(chan error, 1)
	go func() { done <- agent.runOnce(ctx) }()
	select {
	case <-r.renewed:
	case err := <-done:
		t.Fatalf("presence failed before renewal: %v", err)
	case <-ctx.Done():
		t.Fatal("no renewal")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("renewing presence leaked")
	}
	raw, e := os.ReadFile(filepath.Join(agent.StateDir, "lease.pb"))
	if e != nil {
		t.Fatal(e)
	}
	var stored pb.IssuePresenceLeaseResponse
	if proto.Unmarshal(raw, &stored) != nil {
		t.Fatal("bad persisted lease")
	}
	if stored.PresenceLeaseJws != r.leaseJWS {
		t.Fatal("renewal was not persisted for reconnect")
	}
}
