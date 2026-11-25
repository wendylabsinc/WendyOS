package mcusource

import (
	"context"
	"net"
	"strconv"
	"sync"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/shared/models"

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
// builds on `device pair`. "wendycom" pairings (Wendy Lite boards) are not
// WendyOS agents at all: they are found on their own _wendy-lite._tcp
// service (see discoverWendyLiteLANDevices), whose d.Port is the WendyCom port.
// discoverFn is a seam over discovery.Discover so resolveLANAddrs's own
// transport→port selection logic can be exercised with a fake device list,
// without a real mDNS browse.
var discoverFn = discovery.Discover

// browseContinuousFn is the same seam for the browse behind
// discoverWendyLiteLANDevices, so tests can script the sightings it sees.
var browseContinuousFn = discovery.BrowseMDNSServicesContinuous

// discoverWendyLiteLANDevices browses _wendy-lite._tcp until the board with
// sourceAssetID answers, and returns that sighting. The browse stops right
// there: a one-shot browse would wait for the network to go quiet instead,
// and could conclude before a slow board answers when others answer first.
// ok is false if ctx ends before the board answers.
func discoverWendyLiteLANDevices(ctx context.Context, sourceAssetID int32) (models.LANDevice, bool) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel() // ends the browse once the board has answered
	svcs, err := browseContinuousFn(ctx, discovery.WendyLiteServiceType)
	if err != nil {
		return models.LANDevice{}, false
	}
	for svc := range svcs {
		d := discovery.LANDeviceFromWendyLiteService(svc)
		// A sighting resolveLANAddrs could not dial does not count as found.
		if d.AssetID == sourceAssetID && d.IPAddress != "" && d.Port > 0 {
			return d, true
		}
	}
	return models.LANDevice{}, false
}

var resolveLANAddrs = func(ctx context.Context, sourceAssetID int32, transport string) ([]string, bool) {
	var lanDevices []models.LANDevice
	if transport == "wendycom" {
		d, ok := discoverWendyLiteLANDevices(ctx, sourceAssetID)
		if !ok {
			return nil, false
		}
		lanDevices = []models.LANDevice{d}
	} else {
		devices, err := discoverFn(ctx, discovery.DiscoveryOptions{Types: []models.InterfaceType{models.InterfaceLAN}})
		if err != nil {
			return nil, false
		}
		lanDevices = devices.LANDevices
	}
	var addresses []string
	seen := make(map[string]bool)
	for _, d := range lanDevices {
		if d.AssetID != sourceAssetID {
			continue
		}
		if !d.IsMTLS {
			continue
		}
		port := sensorlink.Port
		if transport == "grpc" || transport == "wendycom" {
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

func resolvePairingLANAddrs(ctx context.Context, p SensorPairing) ([]string, bool) {
	if p.SourcePrincipalURI == "" {
		return resolveLANAddrs(ctx, p.SourceAssetID, p.Transport)
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	svcs, err := browseContinuousFn(ctx, discovery.WendyLiteServiceType)
	if err != nil {
		return nil, false
	}
	for svc := range svcs {
		d := discovery.LANDeviceFromWendyLiteService(svc)
		if d.PrincipalURI != p.SourcePrincipalURI || !d.IsMTLS || d.IPAddress == "" || d.Port <= 0 {
			continue
		}
		return []string{net.JoinHostPort(d.IPAddress, strconv.Itoa(d.Port))}, true
	}
	return nil, false
}

// meshSourceLANWindow bounds discovery, not authenticated source setup. A
// routed-only source must not spend the full browse budget waiting for LAN.
const meshSourceLANWindow = time.Second

// NewSourceResolver resolves LAN first and retains a live signed mesh endpoint
// as a sequential alternative. No TLS connection or manifest probe is opened
// by discovery. Each attempt still authenticates the saved source principal.
func NewSourceResolver(meshSources func() []models.LANDevice) SourceResolver {
	return newSourceResolver(meshSources, resolvePairingLANAddrs)
}

func newSourceResolver(meshSources func() []models.LANDevice, resolveLAN SourceResolver) SourceResolver {
	return func(ctx context.Context, p SensorPairing) ([]string, bool) {
		if p.SourcePrincipalURI == "" || p.Transport != "wendycom" || meshSources == nil {
			return resolveLAN(ctx, p)
		}
		routed := func() []string {
			var addresses []string
			seen := make(map[string]bool)
			for _, d := range meshSources() {
				if d.PrincipalURI != p.SourcePrincipalURI || !d.WendyLite || !d.IsMTLS || !d.Sensorlink || d.Port <= 0 || d.Port > 65535 {
					continue
				}
				for _, host := range append([]string{d.IPAddress}, d.Addresses...) {
					if host == "" {
						continue
					}
					address := net.JoinHostPort(host, strconv.Itoa(d.Port))
					if !seen[address] {
						addresses = append(addresses, address)
						seen[address] = true
					}
				}
			}
			return addresses
		}
		lanCtx := ctx
		cancel := func() {}
		if len(routed()) != 0 {
			lanCtx, cancel = context.WithTimeout(ctx, meshSourceLANWindow)
		}
		addresses, _ := resolveLAN(lanCtx, p)
		cancel()
		if ctx.Err() != nil {
			return nil, false
		}
		seen := make(map[string]bool)
		var candidates []string
		// Read the mesh projection again after browsing, so a withdrawn route
		// or changed signed port cannot leave a stale fallback in this attempt.
		for _, list := range [][]string{addresses, routed()} {
			for _, address := range list {
				if address != "" && !seen[address] {
					candidates = append(candidates, address)
					seen[address] = true
				}
			}
		}
		return candidates, len(candidates) != 0
	}
}

// Runner owns one cancelable goroutine per active pairing, all driven through
// the single shared Supervisor (its node-id allocator must stay shared across
// every pairing on the agent).
type Runner struct {
	logger *zap.Logger
	sup    *Supervisor

	mu   sync.Mutex
	runs map[string]pairingRun
}

func NewRunner(logger *zap.Logger, sup *Supervisor) *Runner {
	return &Runner{logger: logger, sup: sup, runs: make(map[string]pairingRun)}
}

// Start (re)launches the supervisor goroutine for p. If a goroutine is
// already running for this source asset id, it is stopped first. addr == ""
// means the source's LAN address is resolved by asset id (used on boot-resume
// and the common `device pair` path, where the pairing store has no address
// on file); RunPairing then owns that resolution and RE-resolves on every
// reconnect so a source that changes IP is still found. A non-empty addr is a
// pinned target RunPairing reuses unchanged.
func (r *Runner) Start(p SensorPairing, addr string) {
	r.start(p, addr, false)
}

// StartDiscovered treats a principal-based source's picker address as an
// initial hint, then re-resolves its identity on every reconnect. Explicit
// Start callers retain their pinned-address behavior, as do legacy pairings.
func (r *Runner) StartDiscovered(p SensorPairing, addr string) {
	r.start(p, addr, p.SourcePrincipalURI != "")
}

func (r *Runner) start(p SensorPairing, addr string, dynamic bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	// A restart retains allocations but must not overlap the previous writers.
	r.stopLocked(p.Key())
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	r.runs[p.Key()] = pairingRun{cancel: cancel, done: done}
	go func() {
		defer close(done)
		if err := r.sup.runPairing(ctx, p, addr, addr != "" && !dynamic); err != nil && ctx.Err() == nil {
			r.logger.Warn("sensor pairing supervisor exited", zap.String("source", p.Key()), zap.Error(err))
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
	r.StopPairing(SensorPairing{SourceAssetID: sourceAssetID})
}

func (r *Runner) StopPairing(p SensorPairing) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.stopLocked(p.Key())
	r.sup.releasePairing(p)
}

func (r *Runner) stopLocked(key string) {
	if run, ok := r.runs[key]; ok {
		run.cancel()
		<-run.done
		delete(r.runs, key)
	}
}
