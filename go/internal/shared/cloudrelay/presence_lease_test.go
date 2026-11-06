package cloudrelay

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"fmt"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	pb "github.com/wendylabsinc/wendy/go/proto/gen/relaypb"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// redeliveryRelay is a Cloud and broker that behave as production does across a
// presence reconnect. Cloud keeps the route for the same agent keys, mints a new
// lease jti on every issuance, and binds a session grant to the newest lease it
// issued. The first presence stream is reset before its offer reaches the
// agent, and the broker redelivers the unacknowledged offer on the next proven
// stream for the route.
type redeliveryRelay struct {
	pb.UnimplementedTunnelAuthorizationServiceServer
	pb.UnimplementedTunnelBrokerV2ServiceServer
	cloud *testRelay // signs artifacts and names the broker
	// resetBeforeAcceptance resets the first stream before the broker accepts
	// its lease, so that lease is never admitted anywhere.
	resetBeforeAcceptance bool
	mu                    sync.Mutex
	issued                []claims
	encryption            []byte
	offer                 *pb.SessionOffer
	offerLease            string
	streams               int
	acked                 chan *pb.PresenceOfferAcknowledgement
}

func (r *redeliveryRelay) IssuePresenceLease(_ context.Context, req *pb.IssuePresenceLeaseRequest) (*pb.IssuePresenceLeaseResponse, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	now := time.Now().Unix()
	c := claims{Iss: r.cloud.verifier.Issuer, Aud: r.cloud.broker().Audience, Iat: now, Exp: now + 300, JTI: uuid.NewString(), Correlation: req.CorrelationId, Route: "opaque-route", Signing: binding(req.AgentSigningPublicKeySpkiDer), Encryption: binding(req.AgentKeyAgreementPublicKeySpkiDer)}
	r.issued = append(r.issued, c)
	r.encryption = bytes.Clone(req.AgentKeyAgreementPublicKeySpkiDer)
	return &pb.IssuePresenceLeaseResponse{PresenceLeaseJws: r.cloud.artifact(c, leaseType), Broker: r.cloud.broker(), RenewAfter: timestamppb.New(time.Now().Add(2 * time.Minute))}, nil
}

// sessionOffer is what Cloud's RequestTunnel sends through the broker: a grant
// bound to the newest lease Cloud issued, with the dial instruction sealed to
// the agent's key.
func (r *redeliveryRelay) sessionOffer() (*pb.SessionOffer, string, error) {
	lease := r.issued[len(r.issued)-1]
	now := time.Now().Unix()
	g := claims{Iss: lease.Iss, Aud: lease.Aud, Iat: now, Exp: now + 60, JTI: uuid.NewString(), Correlation: "opaque-correlation", Route: lease.Route, Session: "opaque-session", Presence: lease.JTI, Request: binding([]byte("request")), Attestation: binding([]byte("attestation")), Caller: binding([]byte("caller")), Agent: lease.Signing, RelayExp: now + 300}
	envelope, err := sealTestInstruction(r.encryption, g, nil)
	if err != nil {
		return nil, "", err
	}
	g.Dial = binding(canonical([]byte("wendycloud.tunnel.v1/hpke-envelope/1"), []byte("DHKEM(P-256,HKDF-SHA256)+HKDF-SHA256+AES-128-GCM"), envelope.EncapsulatedKey, envelope.Info, envelope.Aad, envelope.Ciphertext))
	return &pb.SessionOffer{SessionGrantJws: r.cloud.artifact(g, grantType), EncryptedDialInstruction: envelope, OfferId: "opaque-offer", SessionId: g.Session}, lease.JTI, nil
}

func (r *redeliveryRelay) RegisterPresence(s grpc.BidiStreamingServer[pb.RegisterPresenceRequest, pb.RegisterPresenceResponse]) error {
	m, err := s.Recv()
	if err != nil {
		return err
	}
	c, err := r.cloud.verifier.verify(s.Context(), m.GetOpen().GetPresenceLeaseJws(), leaseType, r.cloud.broker().Audience, time.Now())
	if err != nil {
		return err
	}
	r.mu.Lock()
	r.streams++
	first := r.streams == 1
	if first {
		r.offer, r.offerLease, err = r.sessionOffer()
	}
	offer := r.offer
	r.mu.Unlock()
	if err != nil {
		return err
	}
	// The holder proof is covered by TestAuthorizedRelayEndToEnd.
	if err = s.Send(&pb.RegisterPresenceResponse{Message: &pb.RegisterPresenceResponse_Challenge{Challenge: challenge()}}); err != nil {
		return err
	}
	if _, err = s.Recv(); err != nil {
		return err
	}
	if first && r.resetBeforeAcceptance {
		return status.Error(codes.Unavailable, "presence stream reset")
	}
	if err = s.Send(&pb.RegisterPresenceResponse{Message: &pb.RegisterPresenceResponse_Accepted{Accepted: &pb.PresenceAccepted{LeaseExpiresAt: timestamppb.New(time.Unix(c.Exp, 0))}}}); err != nil {
		return err
	}
	if first {
		return status.Error(codes.Unavailable, "presence stream reset")
	}
	if err = s.Send(&pb.RegisterPresenceResponse{Message: &pb.RegisterPresenceResponse_SessionOffer{SessionOffer: offer}}); err != nil {
		return err
	}
	if m, err = s.Recv(); err != nil {
		return err
	}
	r.acked <- m.GetOfferAcknowledgement()
	<-s.Context().Done()
	return s.Context().Err()
}

// Cloud records a lease's jti as the device's presence when it issues the
// lease, and binds every session grant to the newest one. When the presence
// stream drops, the agent reconnects under a new lease, and the broker
// redelivers each unacknowledged offer, still bound to the replaced lease, on
// the new stream. The agent has to accept it: refusing it ends the stream,
// the next reconnect mints another jti, and every offer queued meanwhile is
// refused in turn, so a device whose peers keep dialing it stays unreachable
// while Cloud shows it online.
func TestRedeliveredOfferBindsAReplacedLease(t *testing.T) {
	for _, tc := range []struct {
		name                  string
		resetBeforeAcceptance bool
	}{
		{"after acceptance", false},
		// Cloud binds grants to a lease the moment it issues it, whether or
		// not any broker ever admitted that lease.
		{"before acceptance", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
			if err != nil {
				t.Fatal(err)
			}
			verifier := &Verifier{Issuer: "https://cloud.test", fetched: time.Now(), keys: map[string]jwk{"test": {Kid: "test", Kty: "EC", Alg: "ES256", Crv: "P-256", X: b64.EncodeToString(key.X.FillBytes(make([]byte, 32))), Y: b64.EncodeToString(key.Y.FillBytes(make([]byte, 32)))}}}
			listener, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			r := &redeliveryRelay{cloud: &testRelay{t: t, endpoint: listener.Addr().String(), cloudKey: key, verifier: verifier}, resetBeforeAcceptance: tc.resetBeforeAcceptance, acked: make(chan *pb.PresenceOfferAcknowledgement, 1)}
			srv := grpc.NewServer()
			pb.RegisterTunnelAuthorizationServiceServer(srv, r)
			pb.RegisterTunnelBrokerV2ServiceServer(srv, r)
			go srv.Serve(listener)
			defer srv.Stop()
			dial := func(endpoint string) (*grpc.ClientConn, error) {
				return grpc.NewClient(endpoint, grpc.WithTransportCredentials(insecure.NewCredentials()))
			}
			verifier.relayDial = dial
			core, logs := observer.New(zap.WarnLevel)
			agent := &Agent{Endpoint: r.cloud.endpoint, Verifier: verifier, StateDir: t.TempDir(), Credentials: func() (string, string, []byte) { return "", "", nil }, Logger: zap.New(core), dialCloud: func(endpoint, _, _ string, _ []byte) (*grpc.ClientConn, error) { return dial(endpoint) }}
			done := make(chan struct{})
			go func() {
				defer close(done)
				agent.Run(ctx)
			}()
			defer func() {
				cancel()
				<-done
			}()

			select {
			case ack := <-r.acked:
				if ack.GetOfferId() != "opaque-offer" || ack.GetSessionId() != "opaque-session" {
					t.Fatalf("acknowledged %q/%q, want the redelivered offer", ack.GetOfferId(), ack.GetSessionId())
				}
			case <-ctx.Done():
				var warnings []string
				for _, entry := range logs.All() {
					warnings = append(warnings, fmt.Sprintf("%s: %v", entry.Message, entry.ContextMap()["error"]))
				}
				t.Fatalf("the redelivered offer was never acknowledged; agent warnings: %q", warnings)
			}
			r.mu.Lock()
			defer r.mu.Unlock()
			if len(r.issued) < 2 || r.offerLease != r.issued[0].JTI || r.offerLease == r.issued[len(r.issued)-1].JTI {
				t.Fatal("the offer was not bound to a lease the agent had replaced, so nothing was under test")
			}
		})
	}
}
