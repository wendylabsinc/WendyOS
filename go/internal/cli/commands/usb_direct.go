package commands

import (
	"context"
	"net"
	"strings"
	"sync"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/cli/grpcclient"
	"github.com/wendylabsinc/wendy/go/internal/shared/discovery"
	"github.com/wendylabsinc/wendy/go/internal/shared/models"
	"github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
)

// usbDirectProbeBudget bounds well-known-address probing. A dead candidate
// fails within one NDP neighbor-resolution timeout (~3s); a live agent answers
// the TCP SYN immediately but its ML-DSA mTLS handshake can take several
// seconds on Jetson-class hardware (see mtlsProbeTimeout), hence the headroom.
//
// Its scope differs by caller: batch-wide in probeUSBDirectDevices, which
// probes every candidate concurrently under one deadline so discovery never
// stalls longer than this; per-candidate in usbDirectFallback, which walks
// candidates in sequence and must give each its own full handshake window.
const usbDirectProbeBudget = 8 * time.Second

// usbDirectCandidatesFn is a seam for tests.
var usbDirectCandidatesFn = discovery.USBDirectCandidates

// advertisedAgentPort returns the port a discovered LANDevice must carry for
// the connection convention lanAgentAddresses implements: a provisioned device
// advertises its mTLS port, from which lanAgentAddresses subtracts
// agentMTLSPortOffset to recover the plaintext port connectWithAutoTLS dials.
// Storing the plaintext port on an IsMTLS device would make that subtraction
// undershoot and every later connection attempt miss the agent entirely.
func advertisedAgentPort(isMTLS bool) int {
	if isMTLS {
		return defaultAgentPort + agentMTLSPortOffset
	}
	return defaultAgentPort
}

// usbDirectPreDialTimeout bounds the liveness gate below. A live agent on a USB
// link completes the TCP handshake in milliseconds; a dead peer costs one NDP
// neighbor-resolution attempt instead of the full probe budget.
const usbDirectPreDialTimeout = time.Second

// usbDirectPreDialFn is a seam for tests.
var usbDirectPreDialFn = usbDirectPreDial

// usbDirectPreDial reports whether anything answers at the well-known address
// on this candidate. Every USB ethernet dongle (enx…) qualifies as a candidate,
// and a dead one otherwise burns the whole usbDirectProbeBudget on mTLS
// handshake attempts — one per stored org cert, across two ports — inflating
// both `wendy discover` and every usbDirectFallback attempt. A raw TCP connect
// is a sufficient gate before that expensive path.
//
// Both agent ports are dialed, concurrently: an unprovisioned agent serves
// plaintext gRPC on defaultAgentPort, while a provisioned one shuts that
// listener down and serves only mTLS on port+1, so neither port alone proves
// liveness.
func usbDirectPreDial(ctx context.Context, cand discovery.USBDirectCandidate) bool {
	return anyAgentPortAnswers(ctx, cand.HostPort)
}

// anyAgentPortAnswers reports whether a TCP connect succeeds on either agent
// port at the addresses addrForPort builds. Both are dialed concurrently so a
// dead peer costs one timeout rather than two.
func anyAgentPortAnswers(ctx context.Context, addrForPort func(port int) string) bool {
	ports := [...]int{defaultAgentPort, defaultAgentPort + agentMTLSPortOffset}
	dialCtx, cancel := context.WithTimeout(ctx, usbDirectPreDialTimeout)
	defer cancel()

	answered := make(chan bool, len(ports))
	for _, port := range ports {
		go func(port int) {
			conn, err := (&net.Dialer{}).DialContext(dialCtx, "tcp", addrForPort(port))
			if err != nil {
				answered <- false
				return
			}
			conn.Close()
			answered <- true
		}(port)
	}
	for range ports {
		if <-answered {
			return true
		}
	}
	return false
}

// probeUSBDirectDevices dials the well-known USB link-local address on every
// USB-backed host interface in parallel and returns a LANDevice for each Wendy
// agent that answers. Devices on WendyOS images without the well-known address
// (or with no agent listening) simply never answer; the caller's mDNS path
// still covers those. Identity comes from GetAgentVersion instead of mDNS TXT
// records, so no multicast needs to work on the link.
//
// Each candidate is dialed at the PLAINTEXT port and handed to the shared
// auto-TLS path, so on a provisioned device the first attempt is mTLS against
// the plaintext port — which no longer listens once provisioning completes, so
// it is refused immediately — before the same handshake is retried at port+1
// where it succeeds. That wasted round trip is deliberate: reusing
// connectWithAutoTLS keeps cert selection, pinning and the plaintext fallback
// in one place, and the failed attempt costs a refused TCP connect over USB.
func probeUSBDirectDevices(ctx context.Context) []models.LANDevice {
	candidates := usbDirectCandidatesFn()
	if len(candidates) == 0 {
		return nil
	}
	pctx, cancel := context.WithTimeout(ctx, usbDirectProbeBudget)
	defer cancel()

	results := make([]*models.LANDevice, len(candidates))
	var wg sync.WaitGroup
	for i := range candidates {
		wg.Add(1)
		go func(i int, cand discovery.USBDirectCandidate) {
			defer wg.Done()
			if !usbDirectPreDialFn(pctx, cand) {
				return
			}
			isMTLS, resp, err := getAgentVersionAtAddress(pctx, cand.HostPort(defaultAgentPort))
			if err != nil || resp == nil {
				return
			}
			hostname := discovery.SanitiseDisplayName(resp.GetHostname())
			dev := models.LANDevice{
				DisplayName:      hostname,
				IPAddress:        discovery.WellKnownUSBAddr + "%" + cand.Zone,
				Port:             advertisedAgentPort(isMTLS),
				IsMTLS:           isMTLS,
				InterfaceType:    string(models.InterfaceLAN),
				NetworkInterface: cand.Interface,
				USB:              cand.Interface,
				IsWendyDevice:    true,
				AgentVersion:     resp.GetVersion(),
				OS:               resp.GetOs(),
				OSVersion:        resp.GetOsVersion(),
				CPUArchitecture:  resp.GetCpuArchitecture(),
				DeviceType:       resp.GetDeviceType(),
			}
			if hostname != "" {
				dev.Hostname = hostname + ".local"
			} else {
				// Pre-hostname-field agent: still show it, identified by port.
				dev.DisplayName = "USB device (" + cand.Interface + ")"
			}
			results[i] = &dev
		}(i, candidates[i])
	}
	wg.Wait()

	var out []models.LANDevice
	for _, r := range results {
		if r != nil {
			out = append(out, *r)
		}
	}
	return out
}

// shouldProbeUSBDirect mirrors Discover's type filter: the probe produces
// LAN-type devices, so it runs whenever LAN discovery would.
func shouldProbeUSBDirect(opts discovery.DiscoveryOptions) bool {
	if len(opts.Types) == 0 {
		return true
	}
	for _, t := range opts.Types {
		if t == models.InterfaceLAN {
			return true
		}
	}
	return false
}

// discoverWithUSBDirect runs standard discovery and the USB well-known-address
// probe concurrently, then merges the probe results into the LAN device list.
// Drop-in replacement for discovery.Discover at command call sites.
func discoverWithUSBDirect(ctx context.Context, opts discovery.DiscoveryOptions) (*models.DevicesCollection, error) {
	var probed []models.LANDevice
	var wg sync.WaitGroup
	if shouldProbeUSBDirect(opts) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			probed = probeUSBDirectDevices(ctx)
		}()
	}
	collection, err := discovery.Discover(ctx, opts)
	wg.Wait()
	if err != nil {
		// Discovery failing outright (no multicast route, avahi down) is the
		// very situation the direct probe exists for, and anything it reached
		// is a verified live agent. Report those instead of throwing them away
		// along with the error.
		if len(probed) > 0 {
			return &models.DevicesCollection{LANDevices: probed}, nil
		}
		return collection, err
	}
	if len(probed) > 0 {
		collection.LANDevices = mergeUSBDirectDevices(collection.LANDevices, probed)
	}
	return collection, nil
}

// mergeUSBDirectDevices folds direct-probe results into an mDNS-discovered LAN
// device list. A probed device whose hostname matches an existing entry
// enriches it in place (USB annotation, interface); identity fields and the
// mDNS-advertised address are preserved, since preferDiscoveredLANDevice
// scoring and lanAgentAddresses ordering already know how to dial USB-annotated
// devices. A probed device with no counterpart is appended — that is the case
// where mDNS is broken and the direct probe is the only path.
func mergeUSBDirectDevices(devices, probed []models.LANDevice) []models.LANDevice {
	for _, p := range probed {
		matched := false
		for i := range devices {
			sameHost := p.Hostname != "" && normalizeMDNSHost(devices[i].Hostname) == normalizeMDNSHost(p.Hostname)
			sameIface := devices[i].NetworkInterface != "" && devices[i].NetworkInterface == p.NetworkInterface
			if !sameHost && !sameIface {
				continue
			}
			matched = true
			if devices[i].USB == "" {
				devices[i].USB = p.USB
			}
			if devices[i].NetworkInterface == "" {
				devices[i].NetworkInterface = p.NetworkInterface
			}
			if devices[i].IPAddress == "" {
				devices[i].IPAddress = p.IPAddress
			}
			if devices[i].AgentVersion == "" {
				devices[i].AgentVersion = p.AgentVersion
			}
			break
		}
		if !matched {
			devices = append(devices, p)
		}
	}
	return devices
}

// usbDirectConnectFn is a seam for tests.
var usbDirectConnectFn = connectWithAutoTLS

// usbDirectFallback attempts to reach the requested device over the USB
// well-known address after normal resolution/connection failed (e.g. mDNS
// broken on this host, stale stored address). It returns a live connection
// ONLY when the device's reported hostname matches the requested one; an
// empty hostname (agent predating the field) never matches — connecting to
// whichever device happens to be plugged in would silently target the wrong
// machine.
//
// Never for a VM's vm:<name> key: a VM is never on USB, and a USB agent that
// merely reports that name would be judged under a key that consults no
// loopback pin — the forwarded port the key is trusted for was never touched.
func usbDirectFallback(ctx context.Context, wantHost string) (*grpcclient.AgentConnection, bool) {
	want := normalizeMDNSHost(wantHost)
	if want == "" || strings.HasPrefix(want, vmDeviceIDPrefix) {
		return nil, false
	}
	for _, cand := range usbDirectCandidatesFn() {
		pctx, cancel := context.WithTimeout(ctx, usbDirectProbeBudget)
		if !usbDirectPreDialFn(pctx, cand) {
			cancel()
			continue
		}
		conn, err := usbDirectConnectFn(pctx, cand.HostPort(defaultAgentPort))
		if err != nil {
			cancel()
			continue
		}
		resp, verr := conn.AgentService.GetAgentVersion(pctx, &agentpb.GetAgentVersionRequest{})
		cancel()
		if verr != nil || resp.GetHostname() == "" || normalizeMDNSHost(resp.GetHostname()) != want {
			conn.Close()
			continue
		}
		conn.CacheAgentVersion(resp)
		return conn, true
	}
	return nil, false
}
