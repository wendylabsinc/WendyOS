package ipcam

import (
	"context"
	"fmt"
	"net"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"go.uber.org/zap"
)

// probeTimeout bounds a discovery round. Cameras answer a WS-Discovery probe
// within a few hundred milliseconds, so two seconds is generous and keeps the
// periodic scan cheap.
const probeTimeout = 2 * time.Second

// maxProbeResponse caps a single discovery datagram. Real ProbeMatches are a
// couple of kilobytes; the bound keeps a hostile responder from driving agent
// allocation.
const maxProbeResponse = 8192

// Discoverer finds network cameras and records them in a Registry.
type Discoverer struct {
	reg    *Registry
	logger *zap.Logger

	// Injection seams: unit tests replace these so no socket is opened and no
	// /proc read happens, matching the style used in internal/agent/camera.
	probe      func(ctx context.Context) ([][]byte, error)
	arpTable   func() (string, error)
	localIPv4s func() []string
	reachable  func(address string) bool
	probeOne   func(source string, deadline time.Time) ([][]byte, error)
	// resolveStreams asks a camera for its RTSP paths (ResolveStreamPaths in
	// production). Only cameras whose registry entry has no paths yet are
	// asked, once per round, so a camera that never answers costs one bounded
	// call a round and not a stall.
	resolveStreams func(ctx context.Context, cam Camera, cred Credential) (StreamPaths, error)

	// Credentials, when set, supplies the stored login for a camera by MAC so
	// ONVIF calls can authenticate. Nil means every call goes out without one,
	// which is enough for cameras that serve ONVIF open.
	Credentials func(mac string) (Credential, bool)
}

// NewDiscoverer returns a discoverer writing into reg.
func NewDiscoverer(reg *Registry, logger *zap.Logger) *Discoverer {
	d := &Discoverer{reg: reg, logger: logger}
	d.probe = d.multicastProbe
	d.arpTable = func() (string, error) {
		b, err := os.ReadFile("/proc/net/arp")
		return string(b), err
	}
	d.localIPv4s = listLocalIPv4s
	d.probeOne = probeFrom
	d.resolveStreams = ResolveStreamPaths
	d.reachable = func(address string) bool {
		conn, err := net.DialTimeout("tcp",
			net.JoinHostPort(address, strconv.Itoa(cameraHTTPPort)), livenessTimeout)
		if err != nil {
			return false
		}
		conn.Close() //nolint:errcheck
		return true
	}
	return d
}

// cameraHTTPPort is the port a liveness check dials. Every IP camera serves its
// own web interface, so an open port 80 is a reliable "still there" signal.
const cameraHTTPPort = 80

// livenessTimeout bounds a single liveness dial.
const livenessTimeout = 2 * time.Second

// Once runs a single discovery round and returns the cameras it registered or
// refreshed.
//
// Malformed responses are skipped rather than fatal: port 3702 carries traffic
// from plenty of things that are not cameras.
func (d *Discoverer) Once(ctx context.Context) ([]Camera, error) {
	payloads, err := d.probe(ctx)
	if err != nil {
		return nil, err
	}
	arp, arpErr := d.arpTable()
	if arpErr != nil {
		// /proc/net/arp is absent off Linux. Without it no MAC resolves, so the
		// round finds nothing, which is better than failing the agent.
		d.logger.Debug("reading ARP table failed", zap.Error(arpErr))
	}

	var found []Camera
	seen := make(map[string]bool)
	for _, payload := range payloads {
		match, err := ParseProbeMatch(payload)
		if err != nil {
			d.logger.Debug("ignoring discovery response", zap.Error(err))
			continue
		}
		host := HostFromXAddrs(match.XAddrs)
		if host == "" {
			continue
		}
		mac := match.MAC
		if mac == "" {
			mac = MACFromARP(arp, host)
		}
		if mac == "" {
			// The registry is MAC-keyed; without one we would allocate a new ID
			// every round. Skip and pick the camera up once ARP resolves.
			d.logger.Debug("camera discovered but MAC unknown", zap.String("address", host))
			continue
		}
		if seen[mac] {
			continue
		}
		seen[mac] = true

		cam, err := d.reg.Upsert(Camera{
			MAC:       mac,
			Address:   host,
			Model:     match.Model,
			ONVIFAddr: match.XAddrs[0],
		})
		if err != nil {
			d.logger.Warn("registering camera failed",
				zap.String("mac", mac), zap.Error(err))
			continue
		}
		cam = d.learnStreamPaths(ctx, cam)
		found = append(found, cam)
	}

	// Cameras that answered no probe may still be there, so check the ones we
	// already know about directly.
	d.refreshLiveness()
	return found, nil
}

// learnStreamPaths fills a camera's RTSP paths from ONVIF the first time it is
// seen without them. Until 2026-09-13 nothing did, so every camera that was
// not a Reolink was streamed at the Reolink default paths and answered 404;
// `camera test` said "the stream path may be wrong" and `camera view` failed
// its pipeline. A failure here is logged and leaves the entry as it was: the
// camera is still registered, and the next round asks again.
func (d *Discoverer) learnStreamPaths(ctx context.Context, cam Camera) Camera {
	if cam.StreamSub != "" && cam.StreamMain != "" {
		return cam
	}
	if cam.ONVIFAddr == "" || d.resolveStreams == nil {
		return cam
	}
	var cred Credential
	if d.Credentials != nil {
		cred, _ = d.Credentials(cam.MAC)
	}
	paths, err := d.resolveStreams(ctx, cam, cred)
	if err != nil {
		d.logger.Debug("camera did not report its stream paths",
			zap.String("mac", cam.MAC), zap.String("address", cam.Address), zap.Error(err))
		return cam
	}
	updated, err := d.reg.Upsert(Camera{MAC: cam.MAC, StreamSub: paths.Sub, StreamMain: paths.Main})
	if err != nil {
		d.logger.Warn("recording camera stream paths failed",
			zap.String("mac", cam.MAC), zap.Error(err))
		return cam
	}
	d.logger.Info("learned camera stream paths from ONVIF",
		zap.String("mac", cam.MAC), zap.String("address", cam.Address),
		zap.String("sub", paths.Sub), zap.String("main", paths.Main))
	return updated
}

// multicastProbe sends a WS-Discovery Probe out of every local IPv4 address and
// collects the replies. It is the production implementation of d.probe.
//
// Probing per address matters on exactly the setup this package exists for: a
// socket not bound to a source address sends multicast out of the default route
// only, which is the uplink, so a camera on its own link never sees the probe.
// Binding the source address makes the kernel pick that interface.
func (d *Discoverer) multicastProbe(ctx context.Context) ([][]byte, error) {
	sources := d.localIPv4s()
	if len(sources) == 0 {
		// Nothing to bind to: fall back to the default route rather than probing
		// nothing at all.
		sources = []string{""}
	}

	deadline := time.Now().Add(probeTimeout)
	if ctxDeadline, ok := ctx.Deadline(); ok && ctxDeadline.Before(deadline) {
		deadline = ctxDeadline
	}

	var (
		mu       sync.Mutex
		payloads [][]byte
		lastErr  error
		wg       sync.WaitGroup
	)
	for _, source := range sources {
		wg.Add(1)
		go func(source string) {
			defer wg.Done()
			got, err := d.probeOne(source, deadline)
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				lastErr = err
				return
			}
			payloads = append(payloads, got...)
		}(source)
	}
	wg.Wait()

	// One interface failing, say a link going down mid-probe, must not discard the
	// replies the others collected.
	if len(payloads) == 0 && lastErr != nil {
		return nil, lastErr
	}
	return payloads, nil
}

// probeFrom sends one Probe from a single source address and reads replies until
// the deadline. An empty source means the default route.
func probeFrom(source string, deadline time.Time) ([][]byte, error) {
	conn, err := net.ListenPacket("udp4", net.JoinHostPort(source, "0"))
	if err != nil {
		return nil, fmt.Errorf("opening discovery socket on %q: %w", source, err)
	}
	defer conn.Close() //nolint:errcheck

	dst, err := net.ResolveUDPAddr("udp4", DiscoveryMulticastAddr)
	if err != nil {
		return nil, fmt.Errorf("resolving discovery address: %w", err)
	}
	messageID := "uuid:" + strconv.FormatInt(time.Now().UnixNano(), 16)
	if _, err := conn.WriteTo(BuildProbe(messageID), dst); err != nil {
		return nil, fmt.Errorf("sending discovery probe from %q: %w", source, err)
	}
	if err := conn.SetReadDeadline(deadline); err != nil {
		return nil, fmt.Errorf("setting discovery deadline: %w", err)
	}

	var payloads [][]byte
	buf := make([]byte, maxProbeResponse)
	for {
		n, _, err := conn.ReadFrom(buf)
		if err != nil {
			// A read timeout is the normal way a probe round ends.
			return payloads, nil
		}
		payload := make([]byte, n)
		copy(payload, buf[:n])
		payloads = append(payloads, payload)
	}
}

// listLocalIPv4s returns every usable local IPv4 address, which is the set of
// interfaces a probe should go out of.
func listLocalIPv4s() []string {
	ifaces, err := net.Interfaces()
	if err != nil {
		return nil
	}
	var out []string
	for _, iface := range ifaces {
		if iface.Flags&net.FlagUp == 0 || iface.Flags&net.FlagLoopback != 0 {
			continue
		}
		addrs, err := iface.Addrs()
		if err != nil {
			continue
		}
		for _, addr := range addrs {
			ipnet, ok := addr.(*net.IPNet)
			if !ok {
				continue
			}
			if v4 := ipnet.IP.To4(); v4 != nil {
				out = append(out, v4.String())
			}
		}
	}
	return out
}

// refreshLiveness updates the online flag for cameras already known.
//
// Discovery alone cannot do this: a camera still holding a lease from an earlier
// session answers no probe, because it needs nothing. Without this a known camera
// would sit at online=false forever.
func (d *Discoverer) refreshLiveness() {
	for _, cam := range d.reg.List() {
		if cam.Address == "" {
			continue
		}
		d.reg.MarkSeen(cam.MAC, "", d.reachable(cam.Address))
	}
}

// HostFromXAddrs returns the host of the first usable ONVIF service address.
func HostFromXAddrs(xaddrs []string) string {
	for _, raw := range xaddrs {
		u, err := url.Parse(raw)
		if err != nil {
			continue
		}
		if host := u.Hostname(); host != "" {
			return host
		}
	}
	return ""
}

// MACFromARP finds the hardware address for ip in the contents of /proc/net/arp.
// Incomplete entries, which carry flags 0x0 and an all-zero address, are treated
// as unknown.
func MACFromARP(table, ip string) string {
	const incompleteMAC = "00:00:00:00:00:00"
	for line := range strings.SplitSeq(table, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 4 || fields[0] != ip {
			continue
		}
		mac := strings.ToLower(fields[3])
		if mac == incompleteMAC {
			return ""
		}
		if _, err := net.ParseMAC(mac); err != nil {
			return ""
		}
		return mac
	}
	return ""
}
