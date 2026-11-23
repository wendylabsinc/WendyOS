package backend

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"math/big"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/cli/grpcclient"
	"github.com/wendylabsinc/wendy/go/internal/shared/config"
)

func appTunnelTestConnection(t *testing.T, expiry time.Time) *grpcclient.AgentConnection {
	t.Helper()
	pub, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{SerialNumber: big.NewInt(1), NotBefore: time.Now().Add(-24 * time.Hour), NotAfter: expiry}
	der, err := x509.CreateCertificate(rand.Reader, template, template, pub, key)
	if err != nil {
		t.Fatal(err)
	}
	return &grpcclient.AgentConnection{CertInfo: &config.CertificateInfo{PemCertificate: string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))}}
}

func appTunnelTestSocket() net.Conn {
	conn, peer := net.Pipe()
	_ = peer.Close()
	return conn
}

func TestAppTunnelRenewsOnceForConcurrentThermalRequests(t *testing.T) {
	type scopeKey struct{}
	ctx, cancel := context.WithCancel(context.WithValue(t.Context(), scopeKey{}, "original"))
	defer cancel()
	old := appTunnelTestConnection(t, time.Now().Add(-time.Hour))
	var stale, refresh, fresh atomic.Int32
	old.RegistryDialer = func(context.Context, int) (net.Conn, error) { stale.Add(1); return appTunnelTestSocket(), nil }
	next := appTunnelTestConnection(t, time.Now().Add(time.Hour))
	next.RegistryDialer = func(context.Context, int) (net.Conn, error) { fresh.Add(1); return appTunnelTestSocket(), nil }
	dial := renewingAppTunnel(ctx, "cloud://same-device", old, func(c context.Context, s string) (*grpcclient.AgentConnection, error) {
		if c.Value(scopeKey{}) != "original" || s != "cloud://same-device" {
			t.Error("lost original scope or device")
		}
		if deadline, ok := c.Deadline(); !ok || time.Until(deadline) > appTunnelSetupTimeout {
			t.Error("renewal setup is missing a bounded deadline")
		}
		refresh.Add(1)
		return next, nil
	}, time.Now)
	var wg sync.WaitGroup
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if conn, err := dial(ctx, 18842); err != nil {
				t.Error(err)
			} else {
				_ = conn.Close()
			}
		}()
	}
	wg.Wait()
	if stale.Load() != 0 || refresh.Load() != 1 || fresh.Load() != 4 {
		t.Fatalf("stale=%d refresh=%d fresh=%d", stale.Load(), refresh.Load(), fresh.Load())
	}
	cancel()
	if _, err := dial(context.Background(), 18842); !errors.Is(err, context.Canceled) {
		t.Fatalf("closed forward: %v", err)
	}
}

func TestAppTunnelKeepsValidConnectionAndPropagatesSignInFailure(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	valid := appTunnelTestConnection(t, time.Now().Add(time.Hour))
	var calls atomic.Int32
	valid.RegistryDialer = func(context.Context, int) (net.Conn, error) { calls.Add(1); return appTunnelTestSocket(), nil }
	refusal := errors.New("invalid_grant: sign in again")
	var renewals atomic.Int32
	reconnect := func(context.Context, string) (*grpcclient.AgentConnection, error) {
		renewals.Add(1)
		return nil, refusal
	}
	dial := renewingAppTunnel(ctx, "cloud://same-device", valid, reconnect, time.Now)
	if conn, err := dial(ctx, 18842); err != nil {
		t.Fatal(err)
	} else {
		_ = conn.Close()
	}
	if renewals.Load() != 0 || calls.Load() != 1 {
		t.Fatal("reconnected a valid tunnel")
	}
	expired := appTunnelTestConnection(t, time.Now().Add(-time.Minute))
	expired.RegistryDialer = valid.RegistryDialer
	dial = renewingAppTunnel(ctx, "cloud://same-device", expired, reconnect, time.Now)
	if _, err := dial(ctx, 18842); !errors.Is(err, refusal) {
		t.Fatalf("hid sign-in failure: %v", err)
	}
	if calls.Load() != 1 {
		t.Fatal("used stale tunnel after renewal refusal")
	}
	if !appTunnelCertificateDue(appTunnelExpiry(appTunnelTestConnection(t, time.Now().Add(30*time.Second))), time.Now()) {
		t.Fatal("missed near expiry")
	}
}

func TestAppTunnelCertificateDueWithTrailingDER(t *testing.T) {
	now := time.Now()
	conn := appTunnelTestConnection(t, now.Add(-time.Minute))
	block, _ := pem.Decode([]byte(conn.CertInfo.PemCertificate))
	block.Bytes = append(block.Bytes, 0x05, 0x00)
	conn.CertInfo.PemCertificate = string(pem.EncodeToMemory(block))
	if !appTunnelCertificateDue(appTunnelExpiry(conn), now) {
		t.Fatal("missed expiry on a stored leaf with trailing ASN.1 data")
	}
}

func TestAppTunnelCanceledWaiterLeavesSharedRenewalRunning(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	old := appTunnelTestConnection(t, time.Now().Add(-time.Hour))
	next := appTunnelTestConnection(t, time.Now().Add(time.Hour))
	next.RegistryDialer = func(context.Context, int) (net.Conn, error) { return appTunnelTestSocket(), nil }
	started := make(chan struct{})
	release := make(chan struct{})
	var attempts atomic.Int32
	dial := renewingAppTunnel(ctx, "cloud://same-device", old, func(setup context.Context, _ string) (*grpcclient.AgentConnection, error) {
		if attempts.Add(1) == 1 {
			close(started)
		}
		select {
		case <-setup.Done():
			return nil, setup.Err()
		case <-release:
			return next, nil
		}
	}, time.Now)
	request, stop := context.WithCancel(ctx)
	defer stop()
	first := make(chan error, 1)
	go func() {
		_, err := dial(request, 18842)
		first <- err
	}()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("renewal did not start")
	}
	stop()
	select {
	case err := <-first:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("canceled waiter returned %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("canceled waiter blocked on shared renewal")
	}
	close(release)
	if conn, err := dial(ctx, 18842); err != nil {
		t.Fatal(err)
	} else {
		_ = conn.Close()
	}
	if got := attempts.Load(); got != 1 {
		t.Fatalf("made %d attempts, want one shared renewal", got)
	}
}

func TestAppTunnelRetriesUnchangedLeafAtExpiry(t *testing.T) {
	now := time.Now().Truncate(time.Second)
	expiry := now.Add(2 * time.Second)
	old := appTunnelTestConnection(t, expiry)
	var usedOld atomic.Int32
	old.RegistryDialer = func(context.Context, int) (net.Conn, error) {
		usedOld.Add(1)
		return appTunnelTestSocket(), nil
	}
	unchanged := &grpcclient.AgentConnection{CertInfo: old.CertInfo, RegistryDialer: old.RegistryDialer}
	unusedClosed := appTunnelCloseSignal(unchanged)
	next := appTunnelTestConnection(t, now.Add(time.Hour))
	next.RegistryDialer = func(context.Context, int) (net.Conn, error) { return appTunnelTestSocket(), nil }
	var attempts atomic.Int32
	dial := renewingAppTunnel(t.Context(), "cloud://same-device", old, func(context.Context, string) (*grpcclient.AgentConnection, error) {
		if attempts.Add(1) == 1 {
			return unchanged, nil
		}
		return next, nil
	}, func() time.Time { return now })
	for range 5 {
		if conn, err := dial(t.Context(), 18842); err != nil {
			t.Fatal(err)
		} else {
			_ = conn.Close()
		}
	}
	waitForAppTunnelClose(t, unusedClosed)
	if got := attempts.Load(); got != 1 {
		t.Fatalf("made %d setup attempts with an unchanged leaf, want one", got)
	}
	now = expiry
	if conn, err := dial(t.Context(), 18842); err != nil {
		t.Fatal(err)
	} else {
		_ = conn.Close()
	}
	if attempts.Load() != 2 || usedOld.Load() != 5 {
		t.Fatal("did not retry at expiry or used the expired transport")
	}
}

func TestAppTunnelClosesSupersededTransportAfterStreamsDrain(t *testing.T) {
	now := time.Now().Truncate(time.Second)
	old := appTunnelTestConnection(t, now.Add(-time.Hour))
	oldClosed := appTunnelCloseSignal(old)
	first := appTunnelTestConnection(t, now.Add(30*time.Second))
	firstClosed := appTunnelCloseSignal(first)
	pipe, peer := net.Pipe()
	defer peer.Close()
	firstSocket := &appTunnelHalfCloseSocket{Conn: pipe}
	first.RegistryDialer = func(context.Context, int) (net.Conn, error) { return firstSocket, nil }
	second := appTunnelTestConnection(t, now.Add(time.Hour))
	secondClosed := appTunnelCloseSignal(second)
	o, p := net.Pipe()
	defer p.Close()
	second.RegistryDialer = func(context.Context, int) (net.Conn, error) { return o, nil }

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	var attempts atomic.Int32
	dial := renewingAppTunnel(ctx, "cloud://same-device", old, func(context.Context, string) (*grpcclient.AgentConnection, error) {
		if attempts.Add(1) == 1 {
			return first, nil
		}
		return second, nil
	}, func() time.Time { return now })
	one, err := dial(ctx, 18842)
	if err != nil {
		t.Fatal(err)
	}
	defer one.Close()
	now = now.Add(appTunnelRetryDelay)
	two, err := dial(ctx, 18842)
	if err != nil {
		t.Fatal(err)
	}
	defer two.Close()
	if _, ok := two.(interface{ CloseWrite() error }); ok {
		t.Fatal("advertised half-close for a transport that does not support it")
	}
	select {
	case <-firstClosed:
		t.Fatal("closed a superseded transport with an open stream")
	default:
	}
	half, ok := one.(interface{ CloseWrite() error })
	if !ok {
		t.Fatal("lost transport's write half-close")
	}
	if err := half.CloseWrite(); err != nil || !firstSocket.halfClosed {
		t.Fatalf("did not pass through write half-close: %v", err)
	}
	select {
	case <-firstClosed:
		t.Fatal("released transport before a stream's full close")
	default:
	}
	if err := one.Close(); err != nil {
		t.Fatal(err)
	}
	waitForAppTunnelClose(t, firstClosed)
	cancel()
	waitForAppTunnelClose(t, secondClosed)
	select {
	case <-oldClosed:
		t.Fatal("closed the original borrowed transport")
	default:
	}
}

type appTunnelHalfCloseSocket struct {
	net.Conn
	halfClosed bool
}

func (c *appTunnelHalfCloseSocket) CloseWrite() error {
	c.halfClosed = true
	return nil
}

type appTunnelCloser func() error

func (f appTunnelCloser) Close() error { return f() }

func appTunnelCloseSignal(conn *grpcclient.AgentConnection) <-chan struct{} {
	closed := make(chan struct{})
	conn.AddCloser(appTunnelCloser(func() error {
		close(closed)
		return nil
	}))
	return closed
}

func waitForAppTunnelClose(t *testing.T, closed <-chan struct{}) {
	t.Helper()
	select {
	case <-closed:
	case <-time.After(time.Second):
		t.Fatal("did not close app tunnel transport")
	}
}

func TestAppTunnelRejectsMissingSocket(t *testing.T) {
	old := appTunnelTestConnection(t, time.Now().Add(time.Hour))
	old.RegistryDialer = func(context.Context, int) (net.Conn, error) { return nil, nil }
	dial := renewingAppTunnel(t.Context(), "cloud://same-device", old, nil, time.Now)
	if conn, err := dial(t.Context(), 18842); conn != nil || err == nil {
		t.Fatalf("dial returned %v, %v for a missing remote socket", conn, err)
	}
}
