package mcusource

import (
	"context"
	"math/rand/v2"
	"net"
	"net/netip"
	"slices"
	"strconv"
	"sync"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/agent/localmesh"
	"github.com/wendylabsinc/wendy/go/internal/shared/discovery"
	"github.com/wendylabsinc/wendy/go/internal/shared/meshid"
	"github.com/wendylabsinc/wendy/go/internal/shared/models"
)

const (
	// meshProbeWorkers bounds the probes one scan runs at a time.
	meshProbeWorkers = 4
	// meshLANWindow bounds the LAN lookup once a live route reaches the
	// source. A board on this network answers mDNS well within it; a board
	// only on Wendy Net would otherwise hold every reconnect for the whole
	// resolve budget before its routed address is tried.
	meshLANWindow = time.Second
)

// LiteMeshSources discovers WendyCom sensors using signed device presence and
// live routes. The directory has no sensor capability field, so the WendyCom
// port is only a candidate hint: discovery authenticates and fetches the sensor
// manifest before offering a source. No application catalogue or multicast
// relay is needed.
type LiteMeshSources struct {
	snapshot     func() localmesh.NodeSnapshot
	transportFor TransportFactory
	browseLAN    func(context.Context, time.Duration, func(models.LANDevice) bool) ([]models.LANDevice, error)
	resolveLAN   func(context.Context, SensorPairing) ([]string, bool)

	mu       sync.Mutex
	inflight map[string]*meshProbe // by source principal
}

// meshProbe is one board's authentication probe, shared by concurrent scans.
// ok is valid once done is closed.
type meshProbe struct {
	done chan struct{}
	ok   bool
}

func NewLiteMeshSources(snapshot func() localmesh.NodeSnapshot, transportFor TransportFactory) *LiteMeshSources {
	return &LiteMeshSources{snapshot: snapshot, transportFor: transportFor,
		browseLAN: discovery.DiscoverSensorSources, resolveLAN: resolvePairingLANAddrs,
		inflight: make(map[string]*meshProbe)}
}

func (m *LiteMeshSources) candidates() []models.LANDevice {
	s := m.snapshot()
	if !s.Self.IsValid() {
		return nil
	}
	now := time.Now().UnixMilli()
	var out []models.LANDevice
	for _, manifest := range s.Devices {
		device := meshid.NewDevice(s.Self.Tenant(), manifest.Device)
		if device == s.Self || manifest.Withdraw || manifest.Expires <= now || manifest.AgentPort != discovery.WendyLitePort ||
			manifest.Route == nil || !s.RouteAuthorizations[*manifest.Route] {
			continue
		}
		prefix := netip.PrefixFrom(device.MeshAddress(), 128)
		if manifest.Route.Prefix != prefix {
			continue
		}
		for _, r := range s.Routes {
			if r.Prefix != prefix || r.RouterID != manifest.Route.RouterID || r.Unreachable || r.Local {
				continue
			}
			out = append(out, models.LANDevice{ID: device.UUID().String(), DisplayName: manifest.Name,
				IPAddress: device.MeshAddress().String(), Port: int(manifest.AgentPort), IsMTLS: true,
				IsWendyDevice: true, WendyLite: true, TenantUUID: device.Tenant().String(), PrincipalURI: device.Principal()})
			break
		}
	}
	return out
}

func meshSourceAddress(d models.LANDevice) string {
	return net.JoinHostPort(d.IPAddress, strconv.Itoa(d.Port))
}

// Resolve keeps LAN preference, then adds a fresh routed endpoint. A failed LAN
// TLS handshake falls through to the mesh in the supervisor's address loop.
// With a live route the LAN lookup gets only meshLANWindow, since a source
// missing from this network never answers it.
func (m *LiteMeshSources) Resolve(ctx context.Context, p SensorPairing) ([]string, bool) {
	var routed []string
	if p.Transport == "wendycom" && p.SourcePrincipalURI != "" {
		for _, d := range m.candidates() {
			if d.PrincipalURI == p.SourcePrincipalURI {
				routed = append(routed, meshSourceAddress(d))
			}
		}
	}
	lanCtx := ctx
	if len(routed) != 0 {
		var cancel context.CancelFunc
		lanCtx, cancel = context.WithTimeout(ctx, meshLANWindow)
		defer cancel()
	}
	addresses, _ := m.resolveLAN(lanCtx, p)
	for _, addr := range routed {
		if !slices.Contains(addresses, addr) {
			addresses = append(addresses, addr)
		}
	}
	return addresses, len(addresses) > 0
}

// Browse shares the caller's scan budget across LAN discovery and bounded mesh
// probes. A broken LAN browse does not hide authenticated mesh sources.
func (m *LiteMeshSources) Browse(ctx context.Context, timeout time.Duration, exclude func(models.LANDevice) bool) ([]models.LANDevice, error) {
	if timeout <= 0 {
		timeout = 5 * time.Second
	}
	probeCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	var lan, mesh []models.LANDevice
	var lanErr error
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		lan, lanErr = m.browseLAN(ctx, timeout, exclude)
	}()
	// Each scan has its own workers, so one scan's stalled probes cannot hold
	// back another's. A board another scan is already probing is not dialed
	// twice: this scan takes that probe's result, which ends with the scan that
	// started it. Each probe closes its connection without subscribing or
	// disturbing an existing pairing.
	candidates := m.candidates()
	// A few slow/offline candidates must not consume every refresh's budget
	// ahead of the same later devices in the signed directory.
	rand.Shuffle(len(candidates), func(i, j int) { candidates[i], candidates[j] = candidates[j], candidates[i] })
	type sharedProbe struct {
		device models.LANDevice
		probe  *meshProbe
	}
	var shared []sharedProbe
	var mu sync.Mutex
	for range min(meshProbeWorkers, len(candidates)) {
		wg.Go(func() {
			for {
				if probeCtx.Err() != nil {
					return
				}
				mu.Lock()
				if len(candidates) == 0 {
					mu.Unlock()
					return
				}
				d := candidates[0]
				candidates = candidates[1:]
				mu.Unlock()
				if exclude != nil && exclude(d) {
					continue
				}
				probe, owner := m.claimProbe(d.PrincipalURI)
				if !owner {
					mu.Lock()
					shared = append(shared, sharedProbe{d, probe})
					mu.Unlock()
					continue
				}
				ok := m.probe(probeCtx, d)
				m.finishProbe(d.PrincipalURI, probe, ok)
				if ok {
					mu.Lock()
					mesh = append(mesh, d)
					mu.Unlock()
				}
			}
		})
	}
	wg.Wait()
	for _, s := range shared {
		if s.probe.wait(probeCtx) {
			mesh = append(mesh, s.device)
		}
	}
	// Prefer the authenticated mesh entry when LAN and mesh name the same
	// principal. An unsigned LAN hint must not shadow a verified source, but a
	// board on this network keeps its LAN addresses.
	for _, d := range mesh {
		d.Sensorlink, d.Caps = true, []string{"sensors"}
		if merged := withLANAddresses(d, lan); exclude == nil || !exclude(merged) {
			d = merged
		}
		lan = slices.DeleteFunc(lan, func(l models.LANDevice) bool { return l.PrincipalURI == d.PrincipalURI })
		lan = append(lan, d)
	}
	if ctx.Err() != nil {
		return lan, ctx.Err()
	}
	if len(lan) != 0 {
		return lan, nil
	}
	return nil, lanErr
}

// withLANAddresses keeps verified source d's signed identity and lists the
// addresses of LAN sightings claiming its principal first, matching the order
// Resolve dials. Addresses stay reachability hints; pairing pins the principal.
func withLANAddresses(d models.LANDevice, lan []models.LANDevice) models.LANDevice {
	var addresses []string
	for _, l := range lan {
		if l.PrincipalURI != d.PrincipalURI || l.IPAddress == "" {
			continue
		}
		if d.Hostname == "" {
			d.Hostname = l.Hostname
		}
		for _, addr := range append([]string{l.IPAddress}, l.Addresses...) {
			if addr != "" && !slices.Contains(addresses, addr) {
				addresses = append(addresses, addr)
			}
		}
	}
	if len(addresses) == 0 {
		return d
	}
	d.Addresses = append(addresses, d.IPAddress)
	d.IPAddress = addresses[0]
	return d
}

// claimProbe returns the probe already running for principal, or registers a
// new one that the caller runs and then passes to finishProbe.
func (m *LiteMeshSources) claimProbe(principal string) (*meshProbe, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if p := m.inflight[principal]; p != nil {
		return p, false
	}
	p := &meshProbe{done: make(chan struct{})}
	m.inflight[principal] = p
	return p, true
}

func (m *LiteMeshSources) finishProbe(principal string, p *meshProbe, ok bool) {
	m.mu.Lock()
	delete(m.inflight, principal)
	m.mu.Unlock()
	p.ok = ok
	close(p.done)
}

// wait reports the probe's result, or false if ctx ends first. A result that
// is already in counts even when ctx has ended too.
func (p *meshProbe) wait(ctx context.Context) bool {
	select {
	case <-p.done:
		return p.ok
	default:
	}
	select {
	case <-p.done:
		return p.ok
	case <-ctx.Done():
		return false
	}
}

func (m *LiteMeshSources) probe(ctx context.Context, d models.LANDevice) bool {
	if ctx.Err() != nil {
		return false
	}
	p := SensorPairing{SourcePrincipalURI: d.PrincipalURI, Transport: "wendycom"}
	tr, err := m.transportFor(p, meshSourceAddress(d))
	if err != nil {
		return false
	}
	defer tr.Close()
	manifest, err := tr.FetchManifest(ctx)
	return err == nil && len(manifest.GetSensors()) != 0
}
