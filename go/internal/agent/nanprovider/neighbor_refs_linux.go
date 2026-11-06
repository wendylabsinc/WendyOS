//go:build linux

package nanprovider

import (
	"errors"
	"sync"

	"github.com/vishvananda/netlink"
)

type radioWorkerKey struct {
	nmi, id  string
	outgoing bool
}

// Crossed NDPs share the peer's underlay neighbour. Retiring one authenticated
// connection must not delete the neighbour still used by its surviving twin.
type nanNeighborRefs struct {
	mu   sync.Mutex
	refs map[string]int
	mac  map[string]string
	set  func(*netlink.Neigh) error
	del  func(*netlink.Neigh) error
}

func (r *nanNeighborRefs) acquire(n *netlink.Neigh) (func(), error) {
	if r == nil {
		return nil, errors.New("NAN neighbour owner missing")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.refs == nil {
		r.refs = make(map[string]int)
		r.mac = make(map[string]string)
	}
	key := n.IP.String()
	if r.refs[key] > 0 && r.mac[key] != n.HardwareAddr.String() {
		return nil, errors.New("NAN peer NDI changed while another path owns its neighbour")
	}
	set := r.set
	if set == nil {
		set = netlink.NeighSet
	}
	del := r.del
	if del == nil {
		del = netlink.NeighDel
	}
	if r.refs[key] == 0 {
		if err := set(n); err != nil {
			return nil, err
		}
		r.mac[key] = n.HardwareAddr.String()
	}
	r.refs[key]++
	var once sync.Once
	return func() {
		once.Do(func() {
			r.mu.Lock()
			defer r.mu.Unlock()
			r.refs[key]--
			if r.refs[key] == 0 {
				delete(r.refs, key)
				delete(r.mac, key)
				_ = del(n)
			}
		})
	}, nil
}
