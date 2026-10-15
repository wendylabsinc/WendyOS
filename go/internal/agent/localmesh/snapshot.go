package localmesh

import "github.com/wendylabsinc/WendyOS/babel"

// NodeSnapshot reports authenticated device presence and installed routes.
// It is internal to internet-sharing policy, not an app service directory.
type NodeSnapshot struct {
	Peers   int
	Devices []Manifest
	Routes  []babel.Route
}
