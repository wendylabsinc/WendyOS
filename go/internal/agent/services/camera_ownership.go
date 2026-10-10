package services

import (
	"sync"
)

// The camera ownership table is the authoritative answer to "which agent-run
// capture holds this V4L2 node", for the refusals that today can only guess
// from sysfs. The calibrated-frame service claims a camera's nodes for the
// life of a capture; errCameraInUse consults the table so a StreamVideo caller
// is told exactly which source holds the device rather than "another
// application". It is the first piece of the single-owner design
// (specs/2026-10-10-realsense-single-owner-design.md): the redirection that
// *serves* StreamVideo from the owning capture reads the same table.
//
// Package-level, like the cameraHolderHint seam it feeds: both services live
// in this package as process singletons, and threading one table through two
// constructors buys nothing but churn. Tests claim and release around
// themselves.

// cameraOwner names the capture holding a node.
type cameraOwner struct {
	// Source is the calibrated source the capture serves, as
	// CalibratedSource.source spells it.
	Source string
	// Kind is the capture technology (framesource.KindRealSense today).
	Kind string
}

type cameraOwnerTable struct {
	mu sync.Mutex
	// owners is keyed by node path (/dev/videoN). A node has one owner: a
	// later claim of the same node replaces the earlier one, because the
	// kernel will have refused one of the two captures anyway and the table
	// must describe whoever actually holds the device now.
	owners map[string]cameraOwner
	// nodesBySource remembers what each source claimed, so release removes
	// exactly those nodes -- and only where the source still owns them.
	nodesBySource map[string][]string
	// redirects maps a claimed COLOUR node to the v4l2loopback bridge node
	// re-exposing the owning capture's colour plane, which is where
	// resolveSource serves StreamVideo from while the claim stands. Only the
	// colour node redirects: a depth or IR node has no bridge to point at and
	// keeps the named refusal instead. The writing source is kept so a clear
	// removes only its own entry, same as owners above.
	redirects map[string]bridgeRedirect
	// redirectBySource mirrors redirects per source, so release and a replaced
	// bridge clear exactly their own entry.
	redirectBySource map[string]string
}

type bridgeRedirect struct {
	Source string
	Bridge string
}

// cameraOwners is the one table in the agent process.
var cameraOwners = newCameraOwnerTable()

func newCameraOwnerTable() *cameraOwnerTable {
	return &cameraOwnerTable{
		owners:           map[string]cameraOwner{},
		nodesBySource:    map[string][]string{},
		redirects:        map[string]bridgeRedirect{},
		redirectBySource: map[string]string{},
	}
}

// claim records source as the owner of nodes for as long as its capture runs.
// Claiming again replaces the source's previous claim.
func (t *cameraOwnerTable) claim(source, kind string, nodes []string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.releaseLocked(source)
	owner := cameraOwner{Source: source, Kind: kind}
	for _, n := range nodes {
		t.owners[n] = owner
	}
	t.nodesBySource[source] = append([]string(nil), nodes...)
}

// release forgets every node source claimed, leaving nodes another source has
// since claimed over it untouched.
func (t *cameraOwnerTable) release(source string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.releaseLocked(source)
}

func (t *cameraOwnerTable) releaseLocked(source string) {
	for _, n := range t.nodesBySource[source] {
		if t.owners[n].Source == source {
			delete(t.owners, n)
		}
	}
	delete(t.nodesBySource, source)
	t.clearBridgeLocked(source)
}

// setBridge records that source's colour node is served from bridgePath while
// the claim stands. Replaces the source's previous bridge, if any.
func (t *cameraOwnerTable) setBridge(source, colourNode, bridgePath string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.clearBridgeLocked(source)
	t.redirects[colourNode] = bridgeRedirect{Source: source, Bridge: bridgePath}
	t.redirectBySource[source] = colourNode
}

// clearBridge removes source's redirect, leaving a redirect another source has
// since written over the same node untouched.
func (t *cameraOwnerTable) clearBridge(source string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.clearBridgeLocked(source)
}

func (t *cameraOwnerTable) clearBridgeLocked(source string) {
	if colour, ok := t.redirectBySource[source]; ok {
		if t.redirects[colour].Source == source {
			delete(t.redirects, colour)
		}
		delete(t.redirectBySource, source)
	}
}

// redirect reports the bridge node serving nodePath, when one stands.
func (t *cameraOwnerTable) redirect(nodePath string) (string, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	r, ok := t.redirects[nodePath]
	return r.Bridge, ok
}

// holder reports which capture owns nodePath, if any.
func (t *cameraOwnerTable) holder(nodePath string) (cameraOwner, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	owner, ok := t.owners[nodePath]
	return owner, ok
}
