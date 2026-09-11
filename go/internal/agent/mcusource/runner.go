package mcusource

import (
	"context"
	"github.com/wendylabsinc/wendy/go/internal/shared/models"
	"net"
	"strconv"
	"sync"

	"github.com/wendylabsinc/wendy/go/internal/agent/sensorlink"
	"github.com/wendylabsinc/wendy/go/internal/shared/discovery"
	"go.uber.org/zap"
)

// resolveLANAddrs is a seam over discovery.Discover so tests can stub LAN
// resolution without a real mDNS browse.
//
// transport selects which port to dial: for "grpc" pairings (agent-hosted
// sources) the source is reached through its own mTLS agent gRPC port, i.e.
// the mDNS-advertised d.Port. For "tcp"/empty pairings (MCU raw-TCP sources)
// d.Port is NOT the right port — it's the agent's own gRPC port, not the
// sensorlink port the source's SensorPairing service listens on — so dial
// the well-known sensorlink.Port instead, agreeing with the address the CLI
// builds on `device pair`.
// discoverFn is a seam over discovery.Discover so resolveLANAddrs's own
// transport→port selection logic can be exercised with a fake device list,
// without a real mDNS browse.
var discoverFn = discovery.Discover

var resolveLANAddrs = func(ctx context.Context, sourceAssetID int32, transport string) ([]string, bool) {
	devices, err := discoverFn(ctx, discovery.DiscoveryOptions{Types: []models.InterfaceType{models.InterfaceLAN}})
	if err != nil {
		return nil, false
	}
	var addresses []string
	seen := make(map[string]bool)
	for _, d := range devices.LANDevices {
		if d.AssetID != sourceAssetID || !d.IsMTLS {
			continue
		}
		port := sensorlink.Port
		if transport == "grpc" {
			port = d.Port
		}
		if port <= 0 {
			continue
		}
		for _, host := range append([]string{d.IPAddress}, d.Addresses...) {
			if host == "" {
				continue
			}
			addr := net.JoinHostPort(host, strconv.Itoa(port))
			if !seen[addr] {
				addresses = append(addresses, addr)
				seen[addr] = true
			}
		}
	}
	return addresses, len(addresses) > 0
}

// Runner owns one cancelable goroutine per active pairing, all driven through
// the single shared Supervisor (its node-id allocator must stay shared across
// every pairing on the agent).
type Runner struct {
	logger *zap.Logger
	sup    *Supervisor

	mu   sync.Mutex
	runs map[int32]pairingRun
}

func NewRunner(logger *zap.Logger, sup *Supervisor) *Runner {
	return &Runner{logger: logger, sup: sup, runs: make(map[int32]pairingRun)}
}

// Start (re)launches the supervisor goroutine for p. If a goroutine is
// already running for this source asset id, it is stopped first. addr == ""
// means the source's LAN address is resolved by asset id (used on boot-resume
// and the common `device pair` path, where the pairing store has no address
// on file); RunPairing then owns that resolution and RE-resolves on every
// reconnect so a source that changes IP is still found. A non-empty addr is a
// pinned target RunPairing reuses unchanged.
func (r *Runner) Start(p SensorPairing, addr string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	// A restart retains allocations but must not overlap the previous writers.
	r.stopLocked(p.SourceAssetID)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	r.runs[p.SourceAssetID] = pairingRun{cancel: cancel, done: done}
	go func() {
		defer close(done)
		if err := r.sup.RunPairing(ctx, p, addr); err != nil && ctx.Err() == nil {
			r.logger.Warn("sensor pairing supervisor exited", zap.Int32("source", p.SourceAssetID), zap.Error(err))
		}
	}()
}

type pairingRun struct {
	cancel context.CancelFunc
	done   <-chan struct{}
}

// Stop cancels a pairing and waits for its writers to close before making its
// slots available to other sources. Start and Stop are serialized so a new
// generation cannot race cleanup of the old one.
func (r *Runner) Stop(sourceAssetID int32) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.stopLocked(sourceAssetID)
	r.sup.releaseSource(sourceAssetID)
}

func (r *Runner) stopLocked(sourceAssetID int32) {
	if run, ok := r.runs[sourceAssetID]; ok {
		run.cancel()
		<-run.done
		delete(r.runs, sourceAssetID)
	}
}
