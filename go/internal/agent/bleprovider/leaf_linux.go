//go:build linux

package bleprovider

import (
	"context"
	"fmt"
	"net"
	"sync"
	"time"

	"github.com/godbus/dbus/v5"
	"github.com/wendylabsinc/wendy/go/internal/agent/localmesh"
	"github.com/wendylabsinc/wendy/go/internal/shared/meshid"
	"go.uber.org/zap"
)

// RunLeaves schedules configured leaves within the router's physical adapter
// budget. The leaf PSM and TLS protocol remain independent of router streams.
func RunLeaves(ctx context.Context, node *localmesh.Node, peers []localmesh.LeafBLEPeer,
	timeURL string, policy *localmesh.ConnectionPolicy, candidates *localmesh.CandidateQueue, pacer *DialPacer,
	logger *zap.Logger, status func(localmesh.CarrierStatus)) error {
	if node == nil || policy == nil || len(peers) == 0 || len(peers) > 8 {
		return fmt.Errorf("configure 1..8 BLE leaves with a shared adapter policy")
	}
	if candidates == nil {
		candidates = &localmesh.CandidateQueue{}
	}
	r := &leafRuntime{policy: policy, candidates: candidates, pacer: pacer, logger: logger, serve: func(ctx context.Context, peer meshid.Device, conn net.Conn) error {
		return node.ServeLeafRadio(ctx, peer, conn, timeURL)
	}}
	for _, config := range peers {
		peer, err := meshid.ParseDevice(node.Credentials.Device.Tenant(), config.Device)
		if err != nil {
			return err
		}
		r.peers = append(r.peers, leafCandidate{config: config, peer: peer})
	}
	local, err := leafAdapterAddress(ctx)
	if err != nil {
		return err
	}
	r.dial = func(ctx context.Context, config localmesh.LeafBLEPeer) (net.Conn, error) {
		return dialL2CAP(ctx, local, config.Address, config.AddressType, config.PSM)
	}
	ctx, cancel := context.WithCancel(ctx)
	defer r.workers.Wait()
	defer cancel()
	if status != nil {
		status(localmesh.CarrierStatus{Ready: true})
	}
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		r.startCandidates(ctx, time.Now())
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}
	}
}

type leafCandidate struct {
	config localmesh.LeafBLEPeer
	peer   meshid.Device
}

func (p leafCandidate) key() string {
	return (candidate{address: p.config.Address, addressType: p.config.AddressType}).key()
}

type leafRuntime struct {
	policy     *localmesh.ConnectionPolicy
	candidates *localmesh.CandidateQueue
	pacer      *DialPacer
	peers      []leafCandidate
	dial       func(context.Context, localmesh.LeafBLEPeer) (net.Conn, error)
	serve      func(context.Context, meshid.Device, net.Conn) error
	logger     *zap.Logger
	workers    sync.WaitGroup
	nextDial   time.Time // owned by the scheduling loop
}

func (r *leafRuntime) startCandidates(ctx context.Context, now time.Time) {
	if ctx.Err() != nil || now.Before(r.nextDial) {
		return
	}
	byKey := make(map[string]leafCandidate, len(r.peers))
	keys := make([]string, 0, len(r.peers))
	for _, peer := range r.peers {
		key := peer.key()
		if _, ok := byKey[key]; !ok {
			keys = append(keys, key)
		}
		byKey[key] = peer
	}
	for _, key := range r.candidates.Order(keys, now) {
		peer := byKey[key]
		lease := r.policy.Reserve(key, peer.peer, true)
		if lease == nil {
			continue
		}
		if r.pacer != nil && !r.pacer.Admit(now) {
			lease.Release()
			return
		}
		r.nextDial = now.Add(globalDialPace)
		lease.ObserveReady(func() { r.candidates.Succeeded(key) })
		r.workers.Add(1)
		go func() {
			defer r.workers.Done()
			defer lease.Release()
			dialCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
			conn, err := r.dial(dialCtx, peer.config)
			cancel()
			if err == nil {
				stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
				err = r.serve(localmesh.WithCarrier(localmesh.WithConnectionLease(ctx, lease), "ble"), peer.peer, conn)
				stop()
				_ = conn.Close()
			}
			if ctx.Err() == nil && !lease.Retired() {
				r.candidates.Failed(key, time.Now())
				if r.logger != nil {
					r.logger.Warn("BLE leaf disconnected", zap.String("device", peer.config.Device), zap.Error(err))
				}
			}
		}()
		return // One actual start per pacing interval; skipped peers are unpenalized.
	}
}

// Leaf sockets also bind a concrete local controller. The leaf PSM is a remote
// endpoint; the local dynamic-PSM bind uses the selected adapter's identity.
func leafAdapterAddress(ctx context.Context) (l2addr, error) {
	query, cancel := context.WithTimeout(ctx, 6*time.Second)
	defer cancel()
	bus, closeTransport, err := connectProviderBus(query)
	if err != nil {
		return l2addr{}, err
	}
	stop := guardProviderBusShutdown(query, 0, closeTransport)
	defer func() { _ = closeTransport(); _ = bus.Close(); stop() }()
	adapter, err := selectAdapter(query, bus, "")
	if err != nil {
		return l2addr{}, err
	}
	// Leaf-only configurations do not run router discovery setup, which
	// normally powers the controller. Fail setup if BlueZ cannot enable it.
	if err := bus.Object(bluezName, adapter).CallWithContext(query, "org.freedesktop.DBus.Properties.Set", 0, adapterInterface, "Powered", dbus.MakeVariant(true)).Err; err != nil {
		return l2addr{}, fmt.Errorf("powering BLE leaf adapter: %w", err)
	}
	return adapterL2Address(query, bus, adapter, DefaultPSM)
}
