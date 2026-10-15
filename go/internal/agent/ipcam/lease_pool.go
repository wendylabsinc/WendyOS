package ipcam

import (
	"errors"
	"fmt"
	"net"
	"sort"
	"sync"
	"time"
)

var ErrPoolExhausted = errors.New("no free addresses in the lease pool")

var (
	errUnknownLease  = errors.New("no reservation for this client")
	errLeaseMismatch = errors.New("requested address does not match reservation")
)

// LeasePool reserves addresses until their advertised lease expires. A pool is
// shared across claims of the same segment, including overlapping shutdown and
// startup of server goroutines. Its lock also protects diagnostic readers.
type LeasePool struct {
	mu          sync.Mutex
	base        net.IP
	count       int
	byMAC       map[string]net.IP
	byAddr      map[string]string
	expires     map[string]time.Duration
	quarantined map[string]time.Duration // ambiguous historical addresses, with no owner
	now         func() (time.Duration, error)
	save        func([]savedLease) error
}

type savedLease struct {
	// Only read when migrating version 1, whose wall-clock expiry is untrusted.
	LegacyExpiresAt  *time.Time    `json:"expiresAt,omitempty"`
	MAC              string        `json:"mac,omitempty"`
	Address          string        `json:"address"`
	ExpiresAfterBoot time.Duration `json:"expiresAfterBootNanos"`
	Quarantined      bool          `json:"quarantined,omitempty"`
}

func NewLeasePool(base net.IP, count int) *LeasePool {
	// In-memory pools need only elapsed time. Persisted pools replace this
	// with the system boot clock before use.
	started := time.Now()
	return &LeasePool{
		base: append(net.IP(nil), base.To4()...), count: count,
		byMAC: make(map[string]net.IP), byAddr: make(map[string]string),
		expires: make(map[string]time.Duration), quarantined: make(map[string]time.Duration),
		now: func() (time.Duration, error) { return time.Since(started), nil },
	}
}

// Lease durably reserves an address before the caller sends an OFFER.
// Offers are conservatively reserved for the full advertised lease as well.
// An unsuccessful write never extends or creates an in-memory reservation.
func (p *LeasePool) Lease(mac net.HardwareAddr, preferred net.IP) (net.IP, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(mac) != 6 {
		return nil, fmt.Errorf("camera lease requires an Ethernet MAC")
	}
	now, err := p.now()
	if err != nil {
		return nil, fmt.Errorf("reading DHCP lease clock: %w", err)
	}
	p.expireLocked(now)
	key := mac.String()
	addr := p.byMAC[key]
	if addr == nil && preferred != nil && p.contains(preferred) {
		if _, taken := p.byAddr[preferred.String()]; !taken {
			addr = preferred.To4()
		}
	}
	for i := 0; addr == nil && i < p.count; i++ {
		candidate := p.addrAt(i)
		if _, taken := p.byAddr[candidate.String()]; !taken {
			addr = candidate
		}
	}
	if addr == nil {
		return nil, ErrPoolExhausted
	}
	return p.reserveLocked(key, addr, now)
}

// confirmLease renews only the exact reservation requested by the client.
// Validation and persistence share the pool lock so a release or expiration
// cannot let another client take the address between checking and committing.
func (p *LeasePool) confirmLease(mac net.HardwareAddr, requested net.IP) (net.IP, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	now, err := p.now()
	if err != nil {
		return nil, fmt.Errorf("reading DHCP lease clock: %w", err)
	}
	p.expireLocked(now)
	key := mac.String()
	addr := p.byMAC[key]
	if addr == nil {
		return nil, errUnknownLease
	}
	if !addr.Equal(requested) {
		return nil, errLeaseMismatch
	}
	return p.reserveLocked(key, addr, now)
}

func (p *LeasePool) reserveLocked(key string, addr net.IP, now time.Duration) (net.IP, error) {
	expiry := now + LeaseWindow
	if p.save != nil {
		records := p.recordsLocked(key)
		records = append(records, savedLease{MAC: key, Address: addr.String(), ExpiresAfterBoot: expiry})
		if err := p.save(records); err != nil {
			return nil, fmt.Errorf("saving camera DHCP lease: %w", err)
		}
	}
	p.byMAC[key] = append(net.IP(nil), addr...)
	p.byAddr[addr.String()] = key
	p.expires[key] = expiry
	return append(net.IP(nil), addr...), nil
}

func (p *LeasePool) Holder(addr net.IP) (string, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if now, err := p.now(); err == nil {
		p.expireLocked(now)
	}
	mac, ok := p.byAddr[addr.String()]
	return mac, ok && mac != ""
}

// remaining avoids advertising time spent saving the reservation as additional
// lease lifetime. A slow disk write must not make clients outlive the record.
func (p *LeasePool) remaining(mac net.HardwareAddr) time.Duration {
	p.mu.Lock()
	defer p.mu.Unlock()
	now, err := p.now()
	if err != nil {
		return 0
	}
	remaining := p.expires[mac.String()] - now
	return remaining.Truncate(time.Second)
}

func (p *LeasePool) Leases() map[string]net.IP {
	p.mu.Lock()
	defer p.mu.Unlock()
	if now, err := p.now(); err == nil {
		p.expireLocked(now)
	}
	out := make(map[string]net.IP, len(p.byMAC))
	for mac, addr := range p.byMAC {
		out[mac] = append(net.IP(nil), addr...)
	}
	return out
}

// Release persists removal before making the address reusable. A failed write
// retains the reservation; losing capacity temporarily is safer than a conflict.
func (p *LeasePool) Release(mac net.HardwareAddr) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	now, err := p.now()
	if err != nil {
		return fmt.Errorf("reading DHCP lease clock: %w", err)
	}
	p.expireLocked(now)
	key := mac.String()
	if _, exists := p.byMAC[key]; !exists {
		return nil
	}
	if p.save != nil {
		if err := p.save(p.recordsLocked(key)); err != nil {
			return fmt.Errorf("saving camera DHCP release: %w", err)
		}
	}
	p.removeLocked(key)
	return nil
}

func (p *LeasePool) expireLocked(now time.Duration) {
	for addr, expiry := range p.quarantined {
		if expiry <= now {
			delete(p.quarantined, addr)
			delete(p.byAddr, addr)
		}
	}
	for mac, expiry := range p.expires {
		if expiry <= now {
			p.removeLocked(mac)
		}
	}
}

func (p *LeasePool) removeLocked(mac string) {
	delete(p.byAddr, p.byMAC[mac].String())
	delete(p.byMAC, mac)
	delete(p.expires, mac)
}

func (p *LeasePool) recordsLocked(except string) []savedLease {
	records := make([]savedLease, 0, len(p.byMAC))
	for mac, ip := range p.byMAC {
		if mac != except {
			records = append(records, savedLease{MAC: mac, Address: ip.String(), ExpiresAfterBoot: p.expires[mac]})
		}
	}
	for addr, expiry := range p.quarantined {
		records = append(records, savedLease{Address: addr, ExpiresAfterBoot: expiry, Quarantined: true})
	}
	sort.Slice(records, func(i, j int) bool { return records[i].Address < records[j].Address })
	return records
}

func (p *LeasePool) addrAt(i int) net.IP {
	addr := append(net.IP(nil), p.base...)
	addr[3] += byte(i)
	return addr
}

func (p *LeasePool) contains(ip net.IP) bool {
	v4 := ip.To4()
	return v4 != nil && v4[0] == p.base[0] && v4[1] == p.base[1] && v4[2] == p.base[2] &&
		int(v4[3]) >= int(p.base[3]) && int(v4[3]) < int(p.base[3])+p.count
}
