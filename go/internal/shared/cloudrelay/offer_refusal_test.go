package cloudrelay

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strings"
	"testing"
	"time"

	pb "github.com/wendylabsinc/wendy/go/proto/gen/relaypb"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// takesRefusals makes the test broker advertise and accept offer refusals.
func takesRefusals(r *offerRelay) { r.refusals = make(chan *pb.PresenceOfferRefusal, 8) }

func (r *offerRelay) nextRefusal(t *testing.T, ctx context.Context, done <-chan error) *pb.PresenceOfferRefusal {
	t.Helper()
	select {
	case refusal := <-r.refusals:
		return refusal
	case err := <-done:
		t.Fatalf("presence ended: %v", err)
	case <-ctx.Done():
		t.Fatal("no offer refusal")
	}
	return nil
}

func wantRefusal(t *testing.T, got *pb.PresenceOfferRefusal, offer *pb.SessionOffer, reason pb.SessionRefusalReason) {
	t.Helper()
	if got.OfferId != offer.OfferId || got.SessionId != offer.SessionId || got.Reason != reason {
		t.Fatalf("refusal = %v, want %s for offer %q", got, reason, offer.OfferId)
	}
}

func tcpOffer(r *offerRelay, offerID string, port int) *pb.SessionOffer {
	return r.sessionOffer(offerID, &pb.DialInstruction{Transport: pb.DialTransport_DIAL_TRANSPORT_TCP, Host: "127.0.0.1", Port: uint32(port), ServiceDescriptor: []byte(fmt.Sprintf("tcp-%d", port))}, nil)
}

// closedPort is a loopback port nothing listens on.
func closedPort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := l.Addr().(*net.TCPAddr).Port
	l.Close()
	return port
}

// An offer the agent will not serve is refused with its reason, so the caller
// does not wait out the grant to learn nothing (WDY-4295). An offer that fails
// verification may not be Cloud's and still gets no answer. A broker that
// predates refusals is sent none: TestRejectedOfferKeepsPresence runs the same
// offers against one, and it ends presence on any message it does not know.
func TestRefusedOffersTellTheCaller(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	r, _, done, _ := startOfferRelay(t, ctx, nil, takesRefusals)

	badSignature := r.sessionOffer("bad-signature", nil, nil)
	badSignature.SessionGrantJws = badSignature.SessionGrantJws[:len(badSignature.SessionGrantJws)-8] + "AAAAAAAA"
	unsupported := r.sessionOffer("unsupported-service", &pb.DialInstruction{Transport: pb.DialTransport_DIAL_TRANSPORT_TCP, Host: "127.0.0.1", Port: 5900, ServiceDescriptor: []byte("vnc")}, nil)
	future := r.sessionOffer("iat-beyond-skew", nil, func(g *claims) {
		g.Iat += int64(cloudClockSkew/time.Second) + 30
		g.Exp, g.RelayExp = g.Iat+60, g.Iat+300
	})
	good := r.sessionOffer("good", nil, nil)
	for _, o := range []*pb.SessionOffer{badSignature, unsupported, future, unsupported, good} {
		r.queue(t, ctx, done, offerMessage(o))
	}
	wantRefusal(t, r.nextRefusal(t, ctx, done), unsupported, pb.SessionRefusalReason_SESSION_REFUSAL_REASON_UNSUPPORTED)
	wantRefusal(t, r.nextRefusal(t, ctx, done), future, pb.SessionRefusalReason_SESSION_REFUSAL_REASON_CLOCK)
	if ack := r.nextAck(t, ctx, done); ack.OfferId != good.OfferId {
		t.Fatalf("acknowledged %q, want %q", ack.OfferId, good.OfferId)
	}
	// Offers are handled in order, so any further refusal has been sent.
	select {
	case extra := <-r.refusals:
		t.Fatalf("unexpected refusal %v", extra)
	case err := <-done:
		t.Fatalf("presence ended: %v", err)
	case <-time.After(100 * time.Millisecond):
	}
}

// The offer past the waiting limit, and the session past the slot limit, are
// refused as busy. A session refused for lack of a slot gives its waiting
// place back.
func TestBusyAgentRefusesOffers(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	r, a, done, _ := startOfferRelay(t, ctx, func(a *Agent) {
		a.slots = make(chan struct{}, 1)
		a.slots <- struct{}{}
	}, takesRefusals)

	noSlot := r.sessionOffer("no-slot", nil, nil)
	r.queue(t, ctx, done, offerMessage(noSlot))
	r.queue(t, ctx, done, acknowledgedMessage(r.nextAck(t, ctx, done)))
	wantRefusal(t, r.nextRefusal(t, ctx, done), noSlot, pb.SessionRefusalReason_SESSION_REFUSAL_REASON_BUSY)

	for i := 0; i < maxWaitingOffers; i++ {
		r.queue(t, ctx, done, offerMessage(r.sessionOffer(fmt.Sprintf("waiting-%d", i), nil, nil)))
		r.nextAck(t, ctx, done)
	}
	tooMany := r.sessionOffer("one-too-many", nil, nil)
	r.queue(t, ctx, done, offerMessage(tooMany))
	wantRefusal(t, r.nextRefusal(t, ctx, done), tooMany, pb.SessionRefusalReason_SESSION_REFUSAL_REASON_BUSY)
	<-a.slots
}

// A target nothing answers on refuses the offer before the agent joins, and
// the caller is told. Against a broker that predates refusals the agent joins
// first, as before, which closes the caller's stream at once.
func TestUnansweredTargetRefusesTheOffer(t *testing.T) {
	for _, tc := range []struct {
		name      string
		configure []func(*offerRelay)
	}{
		{"broker takes refusals", []func(*offerRelay){takesRefusals}},
		{"broker predates refusals", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			r, _, done, _ := startOfferRelay(t, ctx, nil, tc.configure...)
			offer := tcpOffer(r, "closed-port", closedPort(t))
			r.queue(t, ctx, done, offerMessage(offer))
			r.queue(t, ctx, done, acknowledgedMessage(r.nextAck(t, ctx, done)))
			if r.refusals == nil {
				if grant := r.nextJoin(t, ctx, done); grant != offer.SessionGrantJws {
					t.Fatal("joined with the wrong grant")
				}
				return
			}
			wantRefusal(t, r.nextRefusal(t, ctx, done), offer, pb.SessionRefusalReason_SESSION_REFUSAL_REASON_SERVICE_UNREACHABLE)
			select {
			case <-r.joins:
				t.Fatal("joined a session whose target does not answer")
			case err := <-done:
				t.Fatalf("presence ended: %v", err)
			case <-time.After(100 * time.Millisecond):
			}
		})
	}
}

// A target that answers is reached first and the session then joins.
func TestAnsweringTargetIsReachedBeforeJoining(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	r, _, done, _ := startOfferRelay(t, ctx, nil, takesRefusals)
	target, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer target.Close()
	reached := make(chan struct{})
	go func() {
		if c, err := target.Accept(); err == nil {
			close(reached)
			c.Close()
		}
	}()
	offer := tcpOffer(r, "open-port", target.Addr().(*net.TCPAddr).Port)
	r.queue(t, ctx, done, offerMessage(offer))
	r.queue(t, ctx, done, acknowledgedMessage(r.nextAck(t, ctx, done)))
	// The join is reported only after the target was reached.
	if grant := r.nextJoin(t, ctx, done); grant != offer.SessionGrantJws {
		t.Fatal("joined with the wrong grant")
	}
	select {
	case <-reached:
	default:
		t.Fatal("joined before reaching the target")
	}
	select {
	case refusal := <-r.refusals:
		t.Fatalf("unexpected refusal %v", refusal)
	case <-time.After(100 * time.Millisecond):
	}
}

// A device whose clock is behind Cloud's used to drop out of presence at its
// next lease, leaving no way to say why it refuses sessions. It now stays
// registered, says so in its log, and refuses each session for its clock.
func TestClockBehindCloudKeepsPresenceAndRefusesSessions(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	r, _, done, logs := startOfferRelay(t, ctx, nil, takesRefusals, func(r *offerRelay) { r.cloudAhead = 2 * time.Minute })

	offer := r.sessionOffer("issued-ahead", nil, nil)
	r.queue(t, ctx, done, offerMessage(offer))
	wantRefusal(t, r.nextRefusal(t, ctx, done), offer, pb.SessionRefusalReason_SESSION_REFUSAL_REASON_CLOCK)
	// The agent logs on registering, which it had done before it refused.
	if n := logs.FilterMessageSnippet("clock is behind Wendy Cloud's").Len(); n != 1 {
		t.Fatalf("%d log entries about the clock, want 1", n)
	}
	select {
	case err := <-done:
		t.Fatalf("presence ended: %v", err)
	default:
	}
}

// Only the issue time is waived for a lease. One that has expired on this
// clock, or is otherwise invalid, is still refused.
func TestLeaseIssuedAheadMustStillBeValid(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	r, a, _, _ := startOfferRelay(t, ctx, nil)
	lease := func(edit func(*claims)) *pb.IssuePresenceLeaseResponse {
		r.mu.Lock()
		defer r.mu.Unlock()
		c := r.lease
		c.JTI = "another-lease"
		edit(&c)
		return &pb.IssuePresenceLeaseResponse{PresenceLeaseJws: r.artifact(c, leaseType), Broker: r.broker()}
	}
	now := time.Now().Unix()
	if _, err := a.validateLease(ctx, lease(func(c *claims) { c.Iat, c.Exp = now+120, now+420 })); err != nil {
		t.Fatalf("lease issued ahead of the clock: %v", err)
	}
	for name, edit := range map[string]func(*claims){
		"expired":           func(c *claims) { c.Iat, c.Exp = now-600, now-300 },
		"too long a life":   func(c *claims) { c.Iat, c.Exp = now+120, now+120+901 },
		"other agent's key": func(c *claims) { c.Iat, c.Exp, c.Signing = now+120, now+420, binding([]byte("another key")) },
	} {
		if _, err := a.validateLease(ctx, lease(edit)); err == nil {
			t.Errorf("%s: lease accepted", name)
		}
	}
	bad := lease(func(c *claims) { c.Iat, c.Exp = now+120, now+420 })
	bad.PresenceLeaseJws = bad.PresenceLeaseJws[:len(bad.PresenceLeaseJws)-8] + "AAAAAAAA"
	if _, err := a.validateLease(ctx, bad); err == nil {
		t.Error("bad signature: lease accepted")
	}
}

// The caller reads the broker's refusal and reports the device's reason.
func TestCallerLearnsWhyTheDeviceRefused(t *testing.T) {
	for _, tc := range []struct {
		reason pb.SessionRefusalReason
		says   string
	}{
		{pb.SessionRefusalReason_SESSION_REFUSAL_REASON_CLOCK, "its clock disagrees"},
		{pb.SessionRefusalReason_SESSION_REFUSAL_REASON_BUSY, "too many connections"},
		{pb.SessionRefusalReason_SESSION_REFUSAL_REASON_SERVICE_UNREACHABLE, "nothing answered on that port"},
		{pb.SessionRefusalReason_SESSION_REFUSAL_REASON_UNSUPPORTED, "does not serve that service"},
		{pb.SessionRefusalReason(99), "refused the Cloud session"},
	} {
		t.Run(tc.reason.String(), func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			verifier, cloudConn, r := absentAgentRelayWith(t, 60)
			r.refuseCaller = &pb.SessionRefused{Reason: tc.reason}

			started := time.Now()
			err := openAbsentAgent(ctx, verifier, cloudConn)
			var refused *RefusedError
			if !errors.As(err, &refused) || refused.Reason != tc.reason {
				t.Fatalf("OpenTCP error = %v, want a RefusedError with %s", err, tc.reason)
			}
			if !strings.Contains(err.Error(), tc.says) {
				t.Fatalf("message %q does not say %q", err.Error(), tc.says)
			}
			if waited := time.Since(started); waited > 5*time.Second {
				t.Fatalf("waited %s for a refusal the broker sent at once", waited)
			}
		})
	}
}

func TestRefusalReason(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		want pb.SessionRefusalReason
	}{
		{"grant outside its window", errArtifactValidity, pb.SessionRefusalReason_SESSION_REFUSAL_REASON_CLOCK},
		{"too many offers", status.Error(codes.ResourceExhausted, "too many relay offers"), pb.SessionRefusalReason_SESSION_REFUSAL_REASON_BUSY},
		{"target does not answer", &serviceUnreachableError{errors.New("connection refused")}, pb.SessionRefusalReason_SESSION_REFUSAL_REASON_SERVICE_UNREACHABLE},
		{"unknown service", errUnsupportedService, pb.SessionRefusalReason_SESSION_REFUSAL_REASON_UNSUPPORTED},
		{"unknown transport", fmt.Errorf("%w: dial transport", errUnsupportedService), pb.SessionRefusalReason_SESSION_REFUSAL_REASON_UNSUPPORTED},
		{"bad signature", errors.New("invalid Cloud artifact signature"), pb.SessionRefusalReason_SESSION_REFUSAL_REASON_UNSPECIFIED},
		{"expired while admitting", errors.New("relay offer or presence expired"), pb.SessionRefusalReason_SESSION_REFUSAL_REASON_UNSPECIFIED},
		{"lease not yet known", errUnknownPresenceLease, pb.SessionRefusalReason_SESSION_REFUSAL_REASON_UNSPECIFIED},
	} {
		if got := refusalReason(tc.err); got != tc.want {
			t.Errorf("%s: reason = %s, want %s", tc.name, got, tc.want)
		}
	}
}
