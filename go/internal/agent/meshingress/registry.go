// Package meshingress tracks the host TCP ports a running, isolated mesh app
// has explicitly published. Peer sessions consult this registry before
// connecting to a loopback port on the host.
package meshingress

import (
	"errors"
	"fmt"
	"net"
	"net/netip"
	"sync"
)

var ErrPortDenied = errors.New("mesh ingress port is not published by a running app")

// Registry is an in-memory authorization table. Each host port has at most
// one container owner, and an owner can publish several ports. The zero value
// is usable. It starts empty, so access fails closed across agent restarts.
type Registry struct {
	flowMu         sync.Mutex // acquired after mu; connection Close never acquires mu
	tcpByOwner     map[string]map[*authorizedConn]struct{}
	mu             sync.RWMutex
	byPort         map[uint16]string
	appByPort      map[uint16]string
	byOwner        map[string]map[uint16]struct{}
	udpByPort      map[uint16]string
	udpAppByPort   map[uint16]string
	udpByOwner     map[string]map[uint16]struct{}
	udpTokenByPort map[uint16]uint64
	udpNext        uint64
	sources        map[netip.Addr]sourceClaim
	sourceByOwner  map[string]netip.Addr
	sourceNext     uint64
}

type sourceClaim struct {
	owner   string
	appID   string
	cidr    netip.Prefix
	ifindex int
	token   uint64
}

// ClaimSource attributes a live isolated mesh app's CNI address and granted
// service CIDR. The source is installed only after egress wiring succeeds.
func (r *Registry) ClaimSource(containerID, appID, rawIP, rawCIDR string, ifindex int) error {
	if r == nil || containerID == "" || appID == "" || ifindex <= 0 {
		return ErrPortDenied
	}
	ip, err := netip.ParseAddr(rawIP)
	if err != nil || !ip.Is4() {
		return ErrPortDenied
	}
	cidr, err := netip.ParsePrefix(rawCIDR)
	if err != nil || !cidr.Addr().Is4() {
		return ErrPortDenied
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if prior := r.sources[ip]; prior.owner != "" && prior.owner != containerID {
		return ErrPortDenied
	}
	if old := r.sourceByOwner[containerID]; old.IsValid() && old != ip {
		delete(r.sources, old)
	}
	if r.sources == nil {
		r.sources = make(map[netip.Addr]sourceClaim)
		r.sourceByOwner = make(map[string]netip.Addr)
	}
	r.sourceNext++
	r.sources[ip] = sourceClaim{containerID, appID, cidr.Masked(), ifindex, r.sourceNext}
	r.sourceByOwner[containerID] = ip
	return nil
}

func (r *Registry) SourceAllowed(rawIP, dst netip.Addr, ifindex int) bool {
	return r.SourceToken(rawIP, dst, ifindex) != 0
}

// SourceIP returns the currently attributed app address for host-rule cleanup
// when the container client's IP cache is unavailable after recovery.
func (r *Registry) SourceIP(containerID string) string {
	if r == nil || containerID == "" {
		return ""
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	ip := r.sourceByOwner[containerID]
	if !ip.IsValid() || r.sources[ip].owner != containerID {
		return ""
	}
	return ip.String()
}

// SourceToken is a claim incarnation, not merely an IP. A restarted or
// replacement app cannot inherit an old app's UDP reply mapping.
func (r *Registry) SourceToken(rawIP, dst netip.Addr, ifindex int) uint64 {
	if r == nil || !rawIP.IsValid() || !dst.IsValid() || ifindex <= 0 {
		return 0
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	claim := r.sources[rawIP]
	if claim.owner == "" || claim.ifindex != ifindex || !claim.cidr.Contains(dst) {
		return 0
	}
	return claim.token
}

// WithSourceToken holds the same claim across a bounded UDP send. Release
// waits for an in-flight send, then makes every old token unusable.
func (r *Registry) WithSourceToken(rawIP, dst netip.Addr, ifindex int, token uint64, send func() error) error {
	if r == nil || token == 0 || send == nil || ifindex <= 0 {
		return ErrPortDenied
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	claim := r.sources[rawIP]
	if claim.token != token || claim.owner == "" || claim.ifindex != ifindex || !claim.cidr.Contains(dst) {
		return ErrPortDenied
	}
	return send()
}

func NewRegistry() *Registry { return &Registry{} }

// Allowed reports whether a running container has claimed port. Port zero is
// never valid. The caller must still authenticate the remote mesh peer.
func (r *Registry) Allowed(port uint16) bool {
	if r == nil || port == 0 {
		return false
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.byPort[port] != ""
}

// OwnedBy reports whether this exact live container claimed the host port.
// Recovery uses it to distinguish a successful forward from a sibling app
// container that may publish a different port under the same app ID.
func (r *Registry) OwnedBy(containerID string, port uint16) bool {
	if r == nil || containerID == "" || port == 0 {
		return false
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.byPort[port] == containerID
}

// AllowedApp binds a catalog publication to the app whose running container
// owns the forwarded host port. A service cannot claim another app's ingress.
func (r *Registry) AllowedApp(appID string, port uint16) bool {
	if r == nil || appID == "" || port == 0 {
		return false
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.byPort[port] != "" && r.appByPort[port] == appID
}

// AllowedUDPApp checks the separately entitled UDP endpoint. A TCP claim
// never authorizes the same numbered UDP port.
func (r *Registry) AllowedUDPApp(appID string, port uint16) bool {
	if r == nil || appID == "" || port == 0 {
		return false
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.udpByPort[port] != "" && r.udpAppByPort[port] == appID
}

func (r *Registry) AllowedUDP(port uint16) bool {
	if r == nil || port == 0 {
		return false
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.udpByPort[port] != ""
}

// WithAuthorizedUDP holds the admission lock across a single datagram write.
// A stopped app cannot receive a datagram after its claim is revoked.
func (r *Registry) WithAuthorizedUDP(port uint16, write func() error) error {
	if r == nil || port == 0 || write == nil {
		return ErrPortDenied
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	if r.udpByPort[port] == "" {
		return ErrPortDenied
	}
	return write()
}

func (r *Registry) UDPToken(port uint16) uint64 {
	if r == nil || port == 0 {
		return 0
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.udpTokenByPort[port]
}

func (r *Registry) WithAuthorizedUDPToken(port uint16, token uint64, write func() error) error {
	if r == nil || port == 0 || token == 0 || write == nil {
		return ErrPortDenied
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	if r.udpByPort[port] == "" || r.udpTokenByPort[port] != token {
		return ErrPortDenied
	}
	return write()
}

func (r *Registry) CheckUDPAvailable(containerID string, port uint16) error {
	if r == nil || containerID == "" || port == 0 {
		return ErrPortDenied
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	return checkOwner(r.udpByPort[port], containerID, port)
}

func (r *Registry) ClaimUDPForApp(containerID, appID string, port uint16) error {
	if r == nil || containerID == "" || port == 0 {
		return ErrPortDenied
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := checkOwner(r.udpByPort[port], containerID, port); err != nil {
		return err
	}
	if r.udpByPort[port] != "" && r.udpAppByPort[port] != appID {
		return ErrPortDenied
	}
	if r.udpByPort == nil {
		r.udpByPort = make(map[uint16]string)
		r.udpAppByPort = make(map[uint16]string)
		r.udpByOwner = make(map[string]map[uint16]struct{})
		r.udpTokenByPort = make(map[uint16]uint64)
	}
	if r.udpByOwner[containerID] == nil {
		r.udpByOwner[containerID] = make(map[uint16]struct{})
	}
	r.udpByPort[port], r.udpAppByPort[port] = containerID, appID
	r.udpNext++
	r.udpTokenByPort[port] = r.udpNext
	r.udpByOwner[containerID][port] = struct{}{}
	return nil
}

func (r *Registry) OwnedUDPBy(containerID string, port uint16) bool {
	if r == nil || containerID == "" || port == 0 {
		return false
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.udpByPort[port] == containerID
}

// DialAuthorized holds the authorization read lock until the local TCP dial
// finishes. Without this, a stop could revoke the port and remove its DNAT
// rule between an Allowed check and the dial, exposing an unrelated localhost
// listener to the peer. The callback must dial only 127.0.0.1:port and must
// have a bounded timeout; it runs while Release waits for the read lock.
func (r *Registry) DialAuthorized(port uint16, dial func() (net.Conn, error)) (net.Conn, error) {
	if r == nil || port == 0 || dial == nil {
		return nil, ErrPortDenied
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	if r.byPort[port] == "" {
		return nil, ErrPortDenied
	}
	conn, err := dial()
	if err != nil {
		return conn, err
	}
	if conn == nil {
		return nil, errors.New("mesh ingress dial returned no connection")
	}
	owned := &authorizedConn{Conn: conn, registry: r, owner: r.byPort[port], done: make(chan struct{})}
	r.flowMu.Lock()
	if r.tcpByOwner == nil {
		r.tcpByOwner = make(map[string]map[*authorizedConn]struct{})
	}
	if r.tcpByOwner[owned.owner] == nil {
		r.tcpByOwner[owned.owner] = make(map[*authorizedConn]struct{})
	}
	r.tcpByOwner[owned.owner][owned] = struct{}{}
	r.flowMu.Unlock()
	return owned, nil
}

// authorizedConn retains the exact admission incarnation. Release revokes live
// flows as well as future dials; a late Close cannot revoke a replacement app.
type authorizedConn struct {
	net.Conn
	registry *Registry
	owner    string
	done     chan struct{}
	once     sync.Once
	err      error
}

func (c *authorizedConn) Done() <-chan struct{} { return c.done }
func (c *authorizedConn) Close() error {
	c.once.Do(func() {
		c.registry.flowMu.Lock()
		delete(c.registry.tcpByOwner[c.owner], c)
		if len(c.registry.tcpByOwner[c.owner]) == 0 {
			delete(c.registry.tcpByOwner, c.owner)
		}
		c.registry.flowMu.Unlock()
		close(c.done)
		c.err = c.Conn.Close()
	})
	return c.err
}
func (c *authorizedConn) CloseWrite() error {
	if half, ok := c.Conn.(interface{ CloseWrite() error }); ok {
		return half.CloseWrite()
	}
	return errors.New("mesh ingress connection does not support half-close")
}

// CheckAvailable rejects a port already owned by another container. Callers
// must serialize this check with forward installation and Claim; Claim repeats
// the check so a caller that forgets that rule still fails closed.
func (r *Registry) CheckAvailable(containerID string, port uint16) error {
	if r == nil {
		return fmt.Errorf("mesh ingress registry is unavailable")
	}
	if containerID == "" || port == 0 {
		return fmt.Errorf("mesh ingress needs a container ID and nonzero host port")
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	return checkOwner(r.byPort[port], containerID, port)
}

// Claim grants access only after the caller has successfully installed the
// matching host port forward. Repeating a claim by the same owner is safe.
func (r *Registry) Claim(containerID string, port uint16) error {
	return r.ClaimForApp(containerID, "", port)
}

// ClaimForApp records trusted app ownership after its host forward is ready.
func (r *Registry) ClaimForApp(containerID, appID string, port uint16) error {
	if r == nil {
		return fmt.Errorf("mesh ingress registry is unavailable")
	}
	if containerID == "" || port == 0 {
		return fmt.Errorf("mesh ingress needs a container ID and nonzero host port")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := checkOwner(r.byPort[port], containerID, port); err != nil {
		return err
	}
	if r.byPort[port] != "" && r.appByPort[port] != appID {
		return fmt.Errorf("mesh ingress host port %d cannot change app owner", port)
	}
	if r.byPort == nil {
		r.byPort = make(map[uint16]string)
		r.appByPort = make(map[uint16]string)
		r.byOwner = make(map[string]map[uint16]struct{})
	}
	if r.byOwner[containerID] == nil {
		r.byOwner[containerID] = make(map[uint16]struct{})
	}
	r.byPort[port] = containerID
	r.appByPort[port] = appID
	r.byOwner[containerID][port] = struct{}{}
	return nil
}

func checkOwner(owner, containerID string, port uint16) error {
	if owner != "" && owner != containerID {
		return fmt.Errorf("mesh ingress host port %d is already published by container %q", port, owner)
	}
	return nil
}

// Release revokes all ports owned by containerID. It is safe to call after a
// partial start, stop, delete, or again during teardown.
func (r *Registry) Release(containerID string) {
	if r == nil || containerID == "" {
		return
	}
	r.mu.Lock()
	if ip := r.sourceByOwner[containerID]; ip.IsValid() {
		if r.sources[ip].owner == containerID {
			delete(r.sources, ip)
		}
		delete(r.sourceByOwner, containerID)
	}
	for port := range r.byOwner[containerID] {
		if r.byPort[port] == containerID {
			delete(r.byPort, port)
			delete(r.appByPort, port)
		}
	}
	delete(r.byOwner, containerID)
	for port := range r.udpByOwner[containerID] {
		if r.udpByPort[port] == containerID {
			delete(r.udpByPort, port)
			delete(r.udpAppByPort, port)
			delete(r.udpTokenByPort, port)
		}
	}
	delete(r.udpByOwner, containerID)
	r.flowMu.Lock()
	flows := r.tcpByOwner[containerID]
	delete(r.tcpByOwner, containerID)
	r.flowMu.Unlock()
	r.mu.Unlock()
	// Never hold the admission lock while closing sockets. Their callbacks
	// may complete teardown, and unrelated apps must remain available.
	for flow := range flows {
		_ = flow.Close()
	}
}
