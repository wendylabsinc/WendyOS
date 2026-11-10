package carriers

import (
	"context"
	"net/netip"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wendylabsinc/WendyOS/go/babel"
)

func TestRunLegacyPeerClosesStandbyWithoutRapidRedialOrLANLoss(t *testing.T) {
	epoch := time.Now()
	var elapsed atomic.Int64
	legacyClose := make(chan context.CancelFunc, 4)
	legacyDials := make(chan time.Duration, 4)
	withdrawn := make(chan struct{}, 8)
	h := newSpareRun(t, Limits{Connections: 2}, func(cfg *Config) {
		cfg.now = func() time.Time { return epoch.Add(time.Duration(elapsed.Load())) }
		cfg.Limits.RetryDelay = 5 * time.Second // The production default, with deterministic elapsed time.
		connect := cfg.Connect
		cfg.Connect = func(ctx context.Context, c Candidate) error {
			if c.Transport == BLE {
				legacyCtx, close := context.WithCancel(ctx)
				defer close()
				legacyClose <- close
				legacyDials <- time.Duration(elapsed.Load())
				return connect(legacyCtx, c)
			}
			return connect(ctx, c)
		}
		source := cfg.Sources[0]
		cfg.Sources[0] = func(ctx context.Context, emit func([]Candidate)) error {
			return source(ctx, func(items []Candidate) {
				emit(items)
				if len(items) == 1 && items[0].Transport == TCP {
					withdrawn <- struct{}{}
				}
			})
		}
	})
	lan, ble := spareTCP(2, 128), spareBLE(2)
	h.start(lan)
	h.expectStart(lan)
	wait(t, withdrawn)
	h.ready(lan.Address, true)
	h.mu.Lock()
	lanID := h.links[lan.Address].ID
	h.installed = []babel.Route{{Prefix: netip.PrefixFrom(lan.Device.MeshAddress(), 128), Link: lanID, Metric: 128}}
	h.mu.Unlock()
	h.start(lan, ble)
	h.expectStart(ble)
	if got := wait(t, legacyDials); got != 0 {
		t.Fatalf("initial legacy probe at %s", got)
	}
	// Old software closes a redundant carrier, and BLE commonly withdraws its
	// advertisement during that connection. This must not erase retry history.
	h.start(lan)
	wait(t, withdrawn)
	wait(t, legacyClose)()
	if got := wait(t, h.closing); got.Address != ble.Address {
		t.Fatalf("legacy closure affected working LAN: %+v", got)
	}
	h.quietFor(60 * time.Millisecond)
	elapsed.Store(int64(5*time.Second - time.Nanosecond))
	for i := 0; i < 3; i++ {
		h.start(lan, ble)
		h.quiet()
	}
	elapsed.Store(int64(5 * time.Second))
	h.start(lan, ble)
	h.expectStart(ble)
	if got := wait(t, legacyDials); got != 5*time.Second {
		t.Fatalf("legacy endpoint redialed without its normal backoff: %s", got)
	}
	h.mu.Lock()
	working := h.links[lan.Address]
	installed := append([]babel.Route(nil), h.installed...)
	h.mu.Unlock()
	if working.ID != lanID || !working.ForwardingReady || len(installed) != 1 || installed[0].Link != lanID {
		t.Fatal("legacy standby churn changed the working LAN or its installed application route")
	}
	h.quiet()
}
