//go:build linux

package nanprovider

import (
	"errors"
	"fmt"
	"time"

	"github.com/vishvananda/netlink"
)

var errSoftNANRecovery = errors.New("NAN repeatedly connected without an authenticated data path")
var errNoRXNANRecovery = errors.New("NAN NDI transmitted but received no packets")

type ndiTraffic struct {
	rx, tx uint64
}

func ndiTrafficSnapshot() (ndiTraffic, error) {
	link, err := netlink.LinkByName(ndiName)
	if err != nil {
		return ndiTraffic{}, err
	}
	stats := link.Attrs().Statistics
	if stats == nil {
		return ndiTraffic{}, fmt.Errorf("NAN interface %s has no traffic counters", ndiName)
	}
	return ndiTraffic{rx: stats.RxPackets, tx: stats.TxPackets}, nil
}

// coldNDPRecovery requires actual outbound NDI packets and three NDP
// incarnations before considering an in-place data-interface reset. If any
// packets arrive, a new window starts and requires three later NDPs plus a
// longer deadline. Signaling and untrusted discovery alone are insufficient.
type coldNDPRecovery struct {
	windowStart time.Time
	baseline    ndiTraffic
	last        ndiTraffic
	paths       int
	txAdvanced  bool
	hadRX       bool
	triggered   bool
}

func (r *coldNDPRecovery) connected(now time.Time, traffic ndiTraffic) {
	if r.windowStart.IsZero() {
		r.windowStart = now
		r.baseline = traffic
		r.last = traffic
	} else {
		r.observe(now, traffic)
		if r.windowStart.IsZero() {
			r.windowStart = now
			r.baseline = traffic
			r.last = traffic
		}
	}
	r.paths++
}

func (r *coldNDPRecovery) observe(now time.Time, traffic ndiTraffic) {
	if r.windowStart.IsZero() {
		return
	}
	if traffic.rx < r.last.rx || traffic.tx < r.last.tx {
		*r = coldNDPRecovery{}
		return
	}
	if traffic.rx > r.last.rx {
		r.hadRX = true
		r.windowStart = now
		r.baseline = traffic
		r.paths = 0
		r.txAdvanced = false
		r.triggered = false
	}
	r.txAdvanced = r.txAdvanced || traffic.tx > r.baseline.tx
	r.last = traffic
}

func (r *coldNDPRecovery) ready(now time.Time, healthy bool) bool {
	if healthy {
		*r = coldNDPRecovery{}
		return false
	}
	deadline := 90 * time.Second
	if r.hadRX {
		deadline = 270 * time.Second
	}
	if r.triggered || r.windowStart.IsZero() || r.paths < 3 || !r.txAdvanced || now.Sub(r.windowStart) < deadline {
		return false
	}
	r.triggered = true
	return true
}
