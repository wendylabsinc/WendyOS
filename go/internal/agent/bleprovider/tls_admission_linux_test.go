//go:build linux

package bleprovider

import (
	"context"
	"crypto/tls"
	"errors"
	"net"
	goruntime "runtime"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"
)

type admissionDelayedFirstWrite struct {
	net.Conn
	once  sync.Once
	delay time.Duration
}

func (c *admissionDelayedFirstWrite) Write(p []byte) (int, error) {
	c.once.Do(func() { time.Sleep(c.delay) })
	return c.Conn.Write(p)
}

// The remote adapter can be finishing another peer's authentication before
// admitting this one. Its wait and certificate-flight budgets are sequential.
func TestTLSOutboundAllowsRemoteAdmissionAndCertificateFlight(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		serverConfig, clientConfig := metricsTLSConfigs(t)
		serverConfig.SessionTicketsDisabled = true
		clientConfig.SessionTicketsDisabled = true
		clientRaw, serverRaw := net.Pipe()
		defer clientRaw.Close()
		defer serverRaw.Close()
		gate := newTLSHandshakeAdmission()
		gate.slot <- struct{}{} // Another peer currently owns admission.
		admissionDelay := meshTLSAdmissionWait - time.Second
		flightDelay := meshTLSHandshakeTimeout - time.Second
		go func() {
			time.Sleep(admissionDelay)
			<-gate.slot
		}()
		serverDone := make(chan error, 1)
		go func() {
			server := tls.Server(&admissionDelayedFirstWrite{Conn: serverRaw, delay: flightDelay}, serverConfig)
			_, _, err := gate.run(context.Background(), meshTLSAdmissionWait, meshTLSHandshakeTimeout, server.HandshakeContext)
			serverDone <- err
		}()
		client := tls.Client(clientRaw, clientConfig)
		var r runtime
		elapsed, err := r.handshakeOutbound(context.Background(), client)
		if err != nil {
			// Drain the simulated peer before failing the bubble. Its delayed
			// write must finish even when the client has already timed out.
			clientRaw.Close()
			serverRaw.Close()
			<-serverDone
			t.Fatalf("remote admission plus progressing flight rejected after %s: %v", elapsed, err)
		}
		if elapsed < admissionDelay+flightDelay || !client.ConnectionState().HandshakeComplete {
			t.Fatalf("incomplete delayed authentication: elapsed=%s state=%+v", elapsed, client.ConnectionState())
		}
		if err := <-serverDone; err != nil {
			t.Fatal(err)
		}
	})
}

func TestTLSOutboundBudgetAndParentCancellationRemainBounded(t *testing.T) {
	for _, parentDeadline := range []bool{false, true} {
		t.Run(map[bool]string{false: "authentication_bound", true: "parent_bound"}[parentDeadline], func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				_, clientConfig := metricsTLSConfigs(t)
				clientRaw, silentPeer := net.Pipe()
				defer clientRaw.Close()
				defer silentPeer.Close()
				ctx := context.Background()
				want := meshTLSAdmissionWait + meshTLSHandshakeTimeout
				if parentDeadline {
					var cancel context.CancelFunc
					ctx, cancel = context.WithTimeout(ctx, 2*time.Second)
					defer cancel()
					want = 2 * time.Second
				}
				var r runtime
				elapsed, err := r.handshakeOutbound(ctx, tls.Client(clientRaw, clientConfig))
				if !errors.Is(err, context.DeadlineExceeded) || elapsed != want {
					t.Fatalf("silent peer: elapsed=%s err=%v, want deadline at %s", elapsed, err, want)
				}
			})
		})
	}
}

func TestTLSAdmissionConcurrentZeroValue(t *testing.T) {
	var gate tlsHandshakeAdmission
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	start := make(chan struct{})
	var workers sync.WaitGroup
	var active atomic.Int32
	for range 32 {
		workers.Go(func() {
			<-start
			_, _, err := gate.run(ctx, 5*time.Second, time.Second, func(context.Context) error {
				if active.Add(1) != 1 {
					t.Error("concurrent first uses bypassed adapter admission")
				}
				goruntime.Gosched()
				active.Add(-1)
				return nil
			})
			if err != nil {
				t.Errorf("first-use handshake: %v", err)
			}
		})
	}
	close(start)
	workers.Wait()
}

func TestTLSAdmissionSerializesAcceptedHandshakesAndStartsDeadlineAfterAdmission(t *testing.T) {
	gate := newTLSHandshakeAdmission()
	firstEntered := make(chan struct{})
	releaseFirst := make(chan struct{})
	firstDone := make(chan error, 1)
	var concurrent atomic.Int32
	go func() {
		_, _, err := gate.run(context.Background(), time.Second, time.Second, func(context.Context) error {
			if concurrent.Add(1) != 1 {
				t.Errorf("two adapter TLS handshakes ran together")
			}
			close(firstEntered)
			<-releaseFirst
			concurrent.Add(-1)
			return nil
		})
		firstDone <- err
	}()
	<-firstEntered
	secondEntered := make(chan struct{})
	secondDone := make(chan error, 1)
	go func() {
		_, _, err := gate.run(context.Background(), time.Second, 400*time.Millisecond, func(ctx context.Context) error {
			if concurrent.Add(1) != 1 {
				t.Errorf("two adapter TLS handshakes ran together")
			}
			defer concurrent.Add(-1)
			deadline, ok := ctx.Deadline()
			if !ok || time.Until(deadline) < 300*time.Millisecond {
				t.Errorf("handshake deadline was consumed while waiting for admission: %v", deadline)
			}
			close(secondEntered)
			return nil
		})
		secondDone <- err
	}()
	select {
	case <-secondEntered:
		t.Fatal("second handshake bypassed adapter admission")
	case <-time.After(50 * time.Millisecond):
	}
	close(releaseFirst)
	if err := <-firstDone; err != nil {
		t.Fatal(err)
	}
	if err := <-secondDone; err != nil {
		t.Fatal(err)
	}
}

func TestTLSAdmissionWaitBoundedWithoutStartingHandshake(t *testing.T) {
	gate := newTLSHandshakeAdmission()
	gate.slot <- struct{}{}
	called := false
	wait, duration, err := gate.run(context.Background(), 30*time.Millisecond, time.Second, func(context.Context) error {
		called = true
		return nil
	})
	if !errors.Is(err, context.DeadlineExceeded) || called || wait < 25*time.Millisecond || duration != 0 {
		t.Fatalf("wait=%v duration=%v err=%v called=%v", wait, duration, err, called)
	}
	<-gate.slot
	if _, _, err := gate.run(context.Background(), time.Second, time.Second, func(context.Context) error { return nil }); err != nil {
		t.Fatalf("timed-out waiter retained gate: %v", err)
	}
}

func TestTLSAdmissionHandshakeDeadlineAndCancellationRelease(t *testing.T) {
	gate := newTLSHandshakeAdmission()
	wait, duration, err := gate.run(context.Background(), time.Second, 30*time.Millisecond, func(ctx context.Context) error {
		<-ctx.Done()
		return ctx.Err()
	})
	if !errors.Is(err, context.DeadlineExceeded) || wait > 20*time.Millisecond || duration < 25*time.Millisecond {
		t.Fatalf("wait=%v duration=%v err=%v", wait, duration, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, _, err = gate.run(ctx, time.Second, time.Second, func(context.Context) error {
		t.Fatal("canceled handshake started")
		return nil
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled admission: %v", err)
	}
}
