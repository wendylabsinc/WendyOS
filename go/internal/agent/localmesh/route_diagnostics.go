package localmesh

import (
	"encoding/json"
	"net/netip"
	"slices"
	"time"

	"github.com/wendylabsinc/WendyOS/go/babel"
	"github.com/wendylabsinc/wendy/go/internal/shared/asyncdiag"
)

type routeInput struct {
	Kind string       `json:"kind"`
	Link babel.LinkID `json:"link,omitempty"`
}

func describeRouteInput(event babel.Event) routeInput {
	switch e := event.(type) {
	case babel.Tick:
		return routeInput{Kind: "tick"}
	case babel.Receive:
		return routeInput{Kind: "packet", Link: e.ID}
	case babel.AddLink:
		return routeInput{Kind: "peer-added", Link: e.Link.ID}
	case babel.RemoveLink:
		return routeInput{Kind: "peer-removed", Link: e.ID}
	case babel.SetCost:
		return routeInput{Kind: "peer-cost", Link: e.ID}
	case babel.Originate:
		return routeInput{Kind: "originate"}
	case babel.Withdraw:
		return routeInput{Kind: "withdraw"}
	default:
		return routeInput{Kind: "forwarding-apply"}
	}
}

type routeTransition struct {
	Version                  int           `json:"version"`
	Time                     time.Time     `json:"time"`
	Reason                   string        `json:"reason"`
	Input                    routeInput    `json:"input"`
	Previous                 babel.Route   `json:"previous"`
	Current                  babel.Route   `json:"current"`
	PreviousOriginAuthorized bool          `json:"previous_origin_authorized"`
	CurrentOrigins           []RouteOrigin `json:"current_origins"`
	CurrentOriginsTruncated  bool          `json:"current_origins_truncated,omitempty"`
	Candidates               int           `json:"babel_candidates"`
	PendingRequests          int           `json:"babel_pending_requests"`
	Rejected                 uint64        `json:"babel_rejected"`
}

// reportRouteTransitions stages observations after successful forwarding application.
// It distinguishes loss of the old signed origin from an unavailable Babel
// route without inferring radio failure from a still-attached peer. Initial
// routes and ordinary metric/sequence changes do not emit events.
func reportRouteTransitions(previous, current []babel.Route, origins map[RouteOrigin]bool, input routeInput, snapshot babel.Snapshot, now time.Time, emit func(routeTransition)) {
	if input.Kind == "" {
		input = describeRouteInput(nil)
	}
	before := make(map[netip.Prefix]babel.Route, len(previous))
	after := make(map[netip.Prefix]babel.Route, len(current))
	for _, r := range previous {
		before[r.Prefix] = r
	}
	for _, r := range current {
		after[r.Prefix] = r
	}
	for _, old := range previous {
		if old.Local || old.Unreachable {
			continue
		}
		next, ok := after[old.Prefix]
		if ok && !next.Unreachable {
			continue
		}
		if !ok {
			next = babel.Route{Prefix: old.Prefix, Metric: babel.Infinity, Unreachable: true}
		}
		event := routeTransition{Version: 1, Time: now.UTC(), Input: input, Previous: old, Current: next, Candidates: snapshot.Candidates, PendingRequests: snapshot.PendingRequests, Rejected: snapshot.Counters.Rejected}
		event.PreviousOriginAuthorized = origins[RouteOrigin{Prefix: old.Prefix, RouterID: old.RouterID, Seqno: old.Seqno}]
		event.CurrentOrigins = matchingRouteOrigins(old.Prefix, origins)
		event.Reason = "babel-route-unavailable"
		if !event.PreviousOriginAuthorized {
			event.Reason = "origin-not-authorized"
			if len(event.CurrentOrigins) > 0 {
				event.Reason = "origin-superseded"
			}
		}
		emit(event)
	}
	for _, next := range current {
		old, ok := before[next.Prefix]
		if next.Local || next.Unreachable || !ok || !old.Unreachable {
			continue
		}
		emit(routeTransition{Version: 1, Time: now.UTC(), Reason: "route-recovered", Input: input, Previous: old, Current: next, CurrentOrigins: matchingRouteOrigins(next.Prefix, origins), Candidates: snapshot.Candidates, PendingRequests: snapshot.PendingRequests, Rejected: snapshot.Counters.Rejected})
	}
}

func matchingRouteOrigins(prefix netip.Prefix, origins map[RouteOrigin]bool) []RouteOrigin {
	var matches []RouteOrigin
	for origin, authorized := range origins {
		if authorized && origin.Prefix == prefix {
			matches = append(matches, origin)
		}
	}
	slices.SortFunc(matches, func(a, b RouteOrigin) int {
		if a.RouterID < b.RouterID {
			return -1
		}
		if a.RouterID > b.RouterID {
			return 1
		}
		return int(a.Seqno) - int(b.Seqno)
	})
	return matches
}

func logRouteTransition(event routeTransition) {
	// This diagnostic is a compact snapshot, never an unbounded queued origin set.
	if len(event.CurrentOrigins) > 8 {
		event.CurrentOrigins = event.CurrentOrigins[:8]
		event.CurrentOriginsTruncated = true
	}
	if row, err := json.Marshal(event); err == nil {
		asyncdiag.Print("wendy net route transition=" + string(row))
	}
}
