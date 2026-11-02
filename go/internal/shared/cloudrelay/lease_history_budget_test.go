package cloudrelay

import (
	"context"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc"
)

// Start with a current lease that has already been verified and an archived
// lease that still needs key lookup. Restoration must never trust the latter
// merely because its verification budget elapsed.
func historyBudgetFixture(t *testing.T) (*Agent, claims, string) {
	t.Helper()
	a, signer, current := leaseHistoryFixture(t)
	a.lease, a.leaseClaims = historyLease(signer, current), current
	a.remember(current)
	prior := current
	prior.JTI = "unverified-prior"
	if err := a.archivePresenceLease(historyLease(signer, prior), prior); err != nil {
		t.Fatal(err)
	}
	a.Verifier.fetched = time.Now().Add(-6 * time.Minute)
	return a, current, filepath.Join(a.presenceLeaseHistoryDir(), presenceLeaseName(prior.JTI))
}

func assertHistoryRemainsUnverified(t *testing.T, a *Agent, current claims, path string) {
	t.Helper()
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("unverified history lost: %v", err)
	}
	if _, ok := a.leases["unverified-prior"]; ok {
		t.Fatal("unverified archived lease admitted")
	}
	if a.leases[current.JTI] != current {
		t.Fatal("already verified current lease lost")
	}
}

func TestHistoryRestoreBudgetDoesNotBlockVerifiedPresence(t *testing.T) {
	a, current, path := historyBudgetFixture(t)
	a.Verifier.HTTP = &http.Client{Transport: historyKeyTransport(func(req *http.Request) (*http.Response, error) {
		<-req.Context().Done()
		return nil, req.Context().Err()
	})}
	dialed := false
	reached := errors.New("reached public broker fixture")
	a.Verifier.relayDial = func(string) (*grpc.ClientConn, error) { dialed = true; return nil, reached }
	err := a.runOnce(context.Background())
	assertHistoryRemainsUnverified(t, a, current, path)
	if !dialed || !errors.Is(err, reached) {
		t.Fatalf("private restore budget blocked current presence: dialed=%t err=%v", dialed, err)
	}
}

func TestHistoryRestoreParentCancellationStillStopsPresence(t *testing.T) {
	for _, deadline := range []bool{false, true} {
		name := "cancel"
		if deadline {
			name = "deadline"
		}
		t.Run(name, func(t *testing.T) {
			a, current, path := historyBudgetFixture(t)
			started := make(chan struct{})
			a.Verifier.HTTP = &http.Client{Transport: historyKeyTransport(func(req *http.Request) (*http.Response, error) {
				close(started)
				<-req.Context().Done()
				return nil, req.Context().Err()
			})}
			dialed := false
			a.Verifier.relayDial = func(string) (*grpc.ClientConn, error) {
				dialed = true
				return nil, errors.New("unexpected broker dial")
			}
			ctx, cancel := context.WithCancel(context.Background())
			expected := context.Canceled
			if deadline {
				ctx, cancel = context.WithTimeout(context.Background(), 100*time.Millisecond)
				expected = context.DeadlineExceeded
			}
			defer cancel()
			done := make(chan error, 1)
			go func() { done <- a.runOnce(ctx) }()
			select {
			case <-started:
			case <-time.After(time.Second):
				t.Fatal("history lookup did not start")
			}
			if !deadline {
				cancel()
			}
			select {
			case err := <-done:
				if !errors.Is(err, expected) {
					t.Fatalf("wrong parent failure: %v", err)
				}
			case <-time.After(time.Second):
				t.Fatal("parent did not stop history lookup")
			}
			if dialed {
				t.Fatal("parent cancellation reached broker dial")
			}
			assertHistoryRemainsUnverified(t, a, current, path)
		})
	}
}

func TestHistoryRestoreUnknownKeyDoesNotAdmitArchive(t *testing.T) {
	a, current, path := historyBudgetFixture(t)
	a.Verifier.HTTP = &http.Client{Transport: historyKeyTransport(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(`{"keys":[]}`)), Header: make(http.Header)}, nil
	})}
	reached := errors.New("reached broker without trusting unknown archive key")
	a.Verifier.relayDial = func(string) (*grpc.ClientConn, error) { return nil, reached }
	if err := a.runOnce(context.Background()); !errors.Is(err, reached) {
		t.Fatal(err)
	}
	assertHistoryRemainsUnverified(t, a, current, path)
}
