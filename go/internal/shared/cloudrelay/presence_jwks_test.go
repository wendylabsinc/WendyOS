package cloudrelay

import (
	"context"
	"crypto/ecdsa"
	"crypto/rand"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	pb "github.com/wendylabsinc/wendy/go/proto/gen/relaypb"
)

type renewalJWKSRedirect struct{ url string }

func (p renewalJWKSRedirect) RoundTrip(req *http.Request) (*http.Response, error) {
	local, err := http.NewRequestWithContext(req.Context(), req.Method, p.url, nil)
	if err != nil {
		return nil, err
	}
	return http.DefaultTransport.RoundTrip(local)
}

func artifactWithKid(t *testing.T, key *ecdsa.PrivateKey, compact, kid, typ string) string {
	t.Helper()
	parts := strings.Split(compact, ".")
	header, err := canonicalJSON(map[string]string{"alg": "ES256", "kid": kid, "typ": typ})
	if err != nil || len(parts) != 3 {
		t.Fatal("invalid signed fixture")
	}
	input := b64.EncodeToString(header) + "." + parts[1]
	x, y, err := ecdsa.Sign(rand.Reader, key, digest([]byte(input)))
	if err != nil {
		t.Fatal(err)
	}
	sig := make([]byte, 64)
	x.FillBytes(sig[:32])
	y.FillBytes(sig[32:])
	return input + "." + b64.EncodeToString(sig)
}

func waitPresenceExpiry(t *testing.T, done <-chan error, exp int64) bool {
	t.Helper()
	timeout := time.NewTimer(max(time.Until(time.Unix(exp, 0))+350*time.Millisecond, time.Millisecond))
	defer timeout.Stop()
	select {
	case err := <-done:
		if err == nil || err.Error() != "Cloud presence lease expired" {
			t.Fatalf("wrong expiry: %v", err)
		}
		return true
	case <-timeout.C:
		t.Error("key lookup held presence past its original signed expiry")
		return false
	}
}

// A rotated renewal key must not hold up a valid old cached key, and the
// renewal's verification request must be canceled and joined at signed expiry.
func TestPresenceRenewalJWKSRefreshKeepsCachedOffersAndExpiryResponsive(t *testing.T) {
	fetching, stopped, release := make(chan struct{}), make(chan struct{}), make(chan struct{})
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if calls.Add(1) == 1 {
			close(fetching)
			defer close(stopped)
		}
		select {
		case <-release:
		case <-req.Context().Done():
			return
		}
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()
	defer func() {
		select {
		case <-release:
		default:
			close(release)
		}
	}()
	relayReady := make(chan *renewalRelay, 1)
	r, a, _, done := startRenewalRelay(t, func(_ context.Context, call int, l *pb.IssuePresenceLeaseResponse) (*pb.IssuePresenceLeaseResponse, error) {
		if call == 2 && l != nil {
			relay := <-relayReady
			l.PresenceLeaseJws = artifactWithKid(t, relay.cloudKey, l.PresenceLeaseJws, "rotated", leaseType)
		}
		return l, nil
	}, renewalOptions{lifetime: 4 * time.Second, prepare: func(a *Agent) {
		a.Verifier.HTTP = &http.Client{Transport: renewalJWKSRedirect{srv.URL}}
		a.Verifier.fetched = time.Now().Add(-6 * time.Second)
	}})
	relayReady <- r
	renewalWait(t, fetching)
	_, initial := r.initialLeaseSnapshot()
	offer := r.sessionOffer("old-cached-key-during-rotation", nil, func(g *claims) { g.Presence = initial.JTI })
	renewalSend(t, r, offerMessage(offer))
	select {
	case request := <-r.requests:
		if ack := request.GetOfferAcknowledgement(); ack == nil || ack.OfferId != offer.OfferId {
			t.Error("wrong old-key offer acknowledgement")
		}
	case <-time.After(600 * time.Millisecond):
		t.Error("valid cached old-key offer blocked by renewal JWKS refresh")
	}
	finished := waitPresenceExpiry(t, done, initial.Exp)
	if !finished {
		close(release)
		renewalWait(t, done)
	}
	// Always release after checking the timing boundary, including on a negative
	// control, so the fixture cannot leave its HTTP handler or agent behind.
	select {
	case <-stopped:
	case <-time.After(time.Second):
		t.Error("expiry did not cancel renewal JWKS request")
	}
	select {
	case request := <-r.requests:
		if request.GetOfferAcknowledgement() != nil {
			t.Error("offer acknowledged after signed expiry")
		}
	default:
	}
	if calls.Load() != 1 {
		t.Error("overlapping JWKS refreshes")
	}
	if !t.Failed() && a.lease != nil {
		t.Error("expired presence lease survived")
	}
}

func TestPresenceUncachedOfferLookupStopsAtExpiryOrParentCancel(t *testing.T) {
	for _, parentCancel := range []bool{false, true} {
		name := "signed expiry"
		if parentCancel {
			name = "parent cancellation"
		}
		t.Run(name, func(t *testing.T) {
			fetching, stopped, release := make(chan struct{}), make(chan struct{}), make(chan struct{})
			var calls atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				if calls.Add(1) == 1 {
					close(fetching)
					defer close(stopped)
				}
				select {
				case <-release:
				case <-req.Context().Done():
					return
				}
				w.WriteHeader(http.StatusServiceUnavailable)
			}))
			defer srv.Close()
			defer func() {
				select {
				case <-release:
				default:
					close(release)
				}
			}()
			r, a, cancel, done := startRenewalRelay(t, func(ctx context.Context, call int, l *pb.IssuePresenceLeaseResponse) (*pb.IssuePresenceLeaseResponse, error) {
				if call == 2 && l == nil {
					<-ctx.Done()
					return nil, ctx.Err()
				}
				return l, nil
			}, renewalOptions{lifetime: 4 * time.Second, prepare: func(a *Agent) {
				a.Verifier.HTTP = &http.Client{Transport: renewalJWKSRedirect{srv.URL}}
				a.Verifier.fetched = time.Now().Add(-6 * time.Second)
			}})
			_, initial := r.initialLeaseSnapshot()
			offer := r.sessionOffer("uncached-offer-key", nil, nil)
			offer.SessionGrantJws = artifactWithKid(t, r.cloudKey, offer.SessionGrantJws, "uncached", grantType)
			renewalSend(t, r, offerMessage(offer))
			renewalWait(t, fetching)
			if parentCancel {
				cancel()
				select {
				case err := <-done:
					if !errors.Is(err, context.Canceled) {
						t.Errorf("wrong parent cancellation: %v", err)
					}
				case <-time.After(350 * time.Millisecond):
					t.Error("uncached offer lookup held parent cancellation")
				}
			} else {
				if !waitPresenceExpiry(t, done, initial.Exp) {
					close(release)
					renewalWait(t, done)
				}
			}
			select {
			case <-stopped:
			case <-time.After(time.Second):
				t.Error("offer verification lookup was not canceled")
			}
			select {
			case request := <-r.requests:
				if request.GetOfferAcknowledgement() != nil {
					t.Error("unverified offer acknowledged")
				}
			default:
			}
			if _, err := os.Stat(filepath.Join(a.StateDir, "offer-"+binding([]byte(offer.OfferId))+".pb")); !errors.Is(err, os.ErrNotExist) {
				t.Error("blocked or expired offer persisted")
			}
		})
	}
}

func TestVerifierRefreshSharesFetchWithoutBlockingCachedKeys(t *testing.T) {
	a, _, _ := leaseHistoryFixture(t)
	old := a.Verifier.keys["test"]
	rotated := old
	rotated.Kid = "rotated"
	fetching, release := make(chan struct{}), make(chan struct{})
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if calls.Add(1) == 1 {
			close(fetching)
		}
		select {
		case <-release:
		case <-req.Context().Done():
			return
		}
		_ = json.NewEncoder(w).Encode(struct {
			Keys []jwk `json:"keys"`
		}{[]jwk{old, rotated}})
	}))
	defer srv.Close()
	defer func() {
		select {
		case <-release:
		default:
			close(release)
		}
	}()
	v := &Verifier{Issuer: srv.URL, keys: map[string]jwk{"test": old}, fetched: time.Now().Add(-6 * time.Second)}
	owner := make(chan error, 1)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	go func() { _, err := v.key(ctx, "rotated"); owner <- err }()
	renewalWait(t, fetching)
	const count = 16
	waiters := make(chan error, count)
	for i := 0; i < count; i++ {
		go func() {
			k, err := v.key(ctx, "rotated")
			if err == nil && k.Kid != "rotated" {
				err = errors.New("wrong refreshed key")
			}
			waiters <- err
		}()
	}
	cached := make(chan error, 1)
	go func() {
		k, err := v.key(ctx, "test")
		if err == nil && k != old {
			err = errors.New("cached key changed")
		}
		cached <- err
	}()
	select {
	case err := <-cached:
		if err != nil {
			t.Error(err)
		}
	case <-time.After(200 * time.Millisecond):
		t.Error("valid cached key waited for network")
	}
	waiterCtx, stopWaiter := context.WithCancel(ctx)
	canceled := make(chan error, 1)
	go func() { _, err := v.key(waiterCtx, "another-unknown"); canceled <- err }()
	stopWaiter()
	select {
	case err := <-canceled:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("wrong waiter cancellation: %v", err)
		}
	case <-time.After(200 * time.Millisecond):
		t.Error("waiter ignored its context")
	}
	if calls.Load() != 1 {
		t.Error("concurrent unknown keys started multiple refreshes")
	}
	close(release)
	if err := renewalWait(t, owner); err != nil {
		t.Error(err)
	}
	for i := 0; i < count; i++ {
		if err := renewalWait(t, waiters); err != nil {
			t.Error(err)
		}
	}
	if _, err := v.key(ctx, "absent"); err == nil {
		t.Error("unknown kid accepted after refresh")
	}
	if calls.Load() != 1 {
		t.Error("fresh unknown kid bypassed refresh-frequency bound")
	}
}

func TestVerifierFailedRefreshDoesNotTrustExpiredOrUnknownKeys(t *testing.T) {
	for _, expired := range []bool{false, true} {
		name := "valid cached key"
		age := 6 * time.Second
		if expired {
			name = "expired cached key"
			age = 6 * time.Minute
		}
		t.Run(name, func(t *testing.T) {
			a, _, _ := leaseHistoryFixture(t)
			old := a.Verifier.keys["test"]
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusServiceUnavailable) }))
			defer srv.Close()
			v := &Verifier{Issuer: srv.URL, keys: map[string]jwk{"test": old}, fetched: time.Now().Add(-age)}
			if _, err := v.key(context.Background(), "unknown"); err == nil {
				t.Fatal("failed refresh trusted unknown key")
			}
			k, err := v.key(context.Background(), "test")
			if expired && err == nil {
				t.Error("failed refresh admitted an expired cached key")
			}
			if !expired && (err != nil || k != old) {
				t.Errorf("valid cached key lost after failed refresh: %v", err)
			}
		})
	}
}
