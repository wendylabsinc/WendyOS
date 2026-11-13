package localmesh

import (
	"net/netip"
	"testing"

	"github.com/wendylabsinc/WendyOS/go/babel"
)

func TestCarrierForwardingReadyRequiresCurrentInstalledAuthorizedDirectRoute(t *testing.T) {
	peer := selectionDevice(10)
	route := babel.Route{Prefix: netip.PrefixFrom(peer.MeshAddress(), 128), RouterID: 10, Seqno: 3, Link: 1, Metric: 128}
	origin := RouteOrigin{Prefix: route.Prefix, RouterID: route.RouterID, Seqno: route.Seqno}
	for _, tc := range []struct {
		name string
		edit func(*PeerLink, *babel.Snapshot, *[]babel.Route, map[RouteOrigin]bool)
		want bool
	}{
		{"ready", func(*PeerLink, *babel.Snapshot, *[]babel.Route, map[RouteOrigin]bool) {}, true},
		{"silent-neighbour", func(_ *PeerLink, s *babel.Snapshot, _ *[]babel.Route, _ map[RouteOrigin]bool) {
			s.Neighbours[0].Reachable = false
		}, false},
		{"missing-neighbour", func(_ *PeerLink, s *babel.Snapshot, _ *[]babel.Route, _ map[RouteOrigin]bool) { s.Neighbours = nil }, false},
		{"uninstalled", func(_ *PeerLink, _ *babel.Snapshot, installed *[]babel.Route, _ map[RouteOrigin]bool) {
			*installed = nil
		}, false},
		{"stale-installed", func(_ *PeerLink, s *babel.Snapshot, _ *[]babel.Route, _ map[RouteOrigin]bool) { s.Routes = nil }, false},
		{"other-carrier", func(_ *PeerLink, s *babel.Snapshot, installed *[]babel.Route, _ map[RouteOrigin]bool) {
			s.Routes[0].Link = 2
			(*installed)[0].Link = 2
		}, false},
		{"other-peer", func(l *PeerLink, _ *babel.Snapshot, _ *[]babel.Route, _ map[RouteOrigin]bool) {
			l.Device = selectionDevice(11)
		}, false},
		{"unsigned-route", func(_ *PeerLink, _ *babel.Snapshot, _ *[]babel.Route, origins map[RouteOrigin]bool) {
			delete(origins, origin)
		}, false},
		{"superseded-sequence", func(_ *PeerLink, _ *babel.Snapshot, _ *[]babel.Route, origins map[RouteOrigin]bool) {
			delete(origins, origin)
			newer := origin
			newer.Seqno++
			origins[newer] = true
		}, false},
		{"unreachable", func(_ *PeerLink, s *babel.Snapshot, installed *[]babel.Route, _ map[RouteOrigin]bool) {
			s.Routes[0].Unreachable = true
			(*installed)[0].Unreachable = true
		}, false},
		{"local", func(_ *PeerLink, s *babel.Snapshot, installed *[]babel.Route, _ map[RouteOrigin]bool) {
			s.Routes[0].Local = true
			(*installed)[0].Local = true
		}, false},
		{"infinity", func(_ *PeerLink, s *babel.Snapshot, installed *[]babel.Route, _ map[RouteOrigin]bool) {
			s.Routes[0].Metric = babel.Infinity
			(*installed)[0].Metric = babel.Infinity
		}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			links := []PeerLink{{ID: 1, Device: peer, Cost: 128, ForwardingReady: true}}
			routing := babel.Snapshot{Neighbours: []babel.Neighbour{{Link: 1, Reachable: true}}, Routes: []babel.Route{route}}
			installed := []babel.Route{route}
			origins := map[RouteOrigin]bool{origin: true}
			tc.edit(&links[0], &routing, &installed, origins)
			markForwardingReady(links, routing, installed, origins)
			if links[0].ForwardingReady != tc.want {
				t.Fatalf("ready=%v want=%v", links[0].ForwardingReady, tc.want)
			}
		})
	}
}
