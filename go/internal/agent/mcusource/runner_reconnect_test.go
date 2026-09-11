package mcusource

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	sensorlinkpb "github.com/wendylabsinc/wendy/go/proto/gen/sensorlinkpb"
	"go.uber.org/zap"
)

// TestRunPairingReResolvesChangedAddress proves the dynamic (empty-addr) path
// re-resolves the source on every reconnect: the first resolve yields addr A
// (which fails to dial), the source then reappears at a DIFFERENT addr B, and
// a later reconnect must dial B rather than forever redialing the stale A.
func TestRunPairingReResolvesChangedAddress(t *testing.T) {
	orig := resolveLANAddrs
	t.Cleanup(func() { resolveLANAddrs = orig })

	var calls int32
	resolveLANAddrs = func(context.Context, int32, string) ([]string, bool) {
		if atomic.AddInt32(&calls, 1) == 1 {
			return []string{"10.0.0.1:9000"}, true // addr A: first sighting
		}
		return []string{"10.0.0.2:9000"}, true // addr B: source moved (new IP)
	}

	dialed := make(chan string, 8)
	transportFor := func(_ SensorPairing, addr string) (SensorTransport, error) {
		select {
		case dialed <- addr:
		default:
		}
		return nil, errors.New("dial failed") // force streamOnce to fail and back off
	}
	sup := NewSupervisor(zap.NewNop(), nil, transportFor, nil, nil)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = sup.RunPairing(ctx, SensorPairing{SourceAssetID: 7}, "") }()

	if got := <-dialed; got != "10.0.0.1:9000" {
		t.Fatalf("first dial = %q, want 10.0.0.1:9000", got)
	}
	deadline := time.After(5 * time.Second)
	for {
		select {
		case got := <-dialed:
			if got == "10.0.0.2:9000" {
				return // re-resolved to the changed address
			}
		case <-deadline:
			t.Fatal("supervisor never re-resolved to the changed source address")
		}
	}
}

// TestRunPairingPinnedAddressNeverReResolves proves a non-empty (pinned) addr
// is reused unchanged across reconnects and never triggers a re-resolve.
func TestRunPairingPinnedAddressNeverReResolves(t *testing.T) {
	orig := resolveLANAddrs
	t.Cleanup(func() { resolveLANAddrs = orig })
	resolveLANAddrs = func(context.Context, int32, string) ([]string, bool) {
		t.Error("pinned address must not be re-resolved")
		return nil, false
	}

	dialed := make(chan string, 4)
	sup := NewSupervisor(zap.NewNop(), nil, func(_ SensorPairing, addr string) (SensorTransport, error) {
		select {
		case dialed <- addr:
		default:
		}
		return nil, errors.New("dial failed")
	}, nil, nil)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = sup.RunPairing(ctx, SensorPairing{SourceAssetID: 8}, "192.168.1.50:9000") }()

	if got := <-dialed; got != "192.168.1.50:9000" {
		t.Fatalf("pinned dial = %q, want 192.168.1.50:9000", got)
	}
}

func TestRunPairingTriesSiblingBeforeBackingOff(t *testing.T) {
	orig := resolveLANAddrs
	t.Cleanup(func() { resolveLANAddrs = orig })
	resolveLANAddrs = func(context.Context, int32, string) ([]string, bool) { return []string{"wifi", "usb"}, true }
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var dialed []string
	sup := NewSupervisor(zap.NewNop(), nil, func(_ SensorPairing, addr string) (SensorTransport, error) {
		dialed = append(dialed, addr)
		if addr == "usb" {
			cancel()
		}
		return nil, errors.New("unavailable")
	}, nil, nil)
	done := make(chan struct{})
	go func() { defer close(done); _ = sup.RunPairing(ctx, SensorPairing{SourceAssetID: 1}, "") }()
	select {
	case <-done:
	case <-time.After(500 * time.Millisecond):
		t.Fatal("backed off before trying sibling address")
	}
	if len(dialed) != 2 || dialed[0] != "wifi" || dialed[1] != "usb" {
		t.Fatalf("dialed %v", dialed)
	}
}

type silentManifestTransport struct{ cleanupTransport }

func (*silentManifestTransport) FetchManifest(ctx context.Context) (*sensorlinkpb.SensorManifest, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}
func TestRunPairingTimesOutSilentManifestAndTriesSibling(t *testing.T) {
	orig := resolveLANAddrs
	t.Cleanup(func() { resolveLANAddrs = orig })
	resolveLANAddrs = func(context.Context, int32, string) ([]string, bool) { return []string{"silent", "reachable"}, true }
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	tr := &silentManifestTransport{}
	reached := false
	sup := NewSupervisor(zap.NewNop(), nil, func(_ SensorPairing, addr string) (SensorTransport, error) {
		if addr == "silent" {
			return tr, nil
		}
		reached = true
		cancel()
		return nil, errors.New("test finished")
	}, nil, nil)
	done := make(chan struct{})
	go func() { defer close(done); _ = sup.RunPairing(ctx, SensorPairing{SourceAssetID: 1}, "") }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("silent source blocked fallback")
	}
	if !reached || tr.closes != 1 {
		t.Fatalf("fallback=%v, closes=%d", reached, tr.closes)
	}
}
