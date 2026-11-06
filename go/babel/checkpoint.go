package babel

import (
	"net/netip"
	"slices"
	"time"
)

// Checkpoint contains safety history, not live links or learnt forwarding routes.
// Persist it durably BEFORE sending returned datagrams to survive a crash without
// forgetting advertised feasibility distances. If storage is unavailable, stop
// advertising. Checkpoint is an owned value and may be encoded by the application.
type Checkpoint struct {
	Version    int
	RouterID   RouterID
	Seqno      uint16
	LastLink   LinkID
	SourceHold time.Duration
	DropHold   time.Duration
	Sources    []SourceHistory
	Drops      []netip.Prefix
}
type SourceHistory struct {
	Prefix        netip.Prefix
	RouterID      RouterID
	Seqno, Metric uint16
}

func (e *Engine) Checkpoint() Checkpoint {
	c := Checkpoint{Version: 1, RouterID: e.cfg.RouterID, Seqno: e.seq, LastLink: e.lastLink, SourceHold: e.cfg.SourceGC, DropHold: hold(e.cfg.UpdateInterval)}
	for _, until := range e.drops {
		c.DropHold = max(c.DropHold, until-e.now)
	}
	for sk, s := range e.sources {
		c.Sources = append(c.Sources, SourceHistory{sk.prefix, sk.id, s.seq, s.metric})
	}
	slices.SortFunc(c.Sources, func(a, b SourceHistory) int {
		if a.Prefix != b.Prefix {
			if prefixLess(a.Prefix, b.Prefix) {
				return -1
			}
			return 1
		}
		if a.RouterID < b.RouterID {
			return -1
		}
		if a.RouterID > b.RouterID {
			return 1
		}
		return 0
	})
	// Neighbours may still point at us for selected prefixes or prefixes already
	// being suppressed. Include drops even before the first Step after Restore.
	// Deduplicate because selected unreachable routes also appear in e.drops.
	prefixes := make(map[netip.Prefix]struct{}, len(e.drops)+len(e.selected))
	for p := range e.drops {
		prefixes[p] = struct{}{}
	}
	for p := range e.selected {
		prefixes[p] = struct{}{}
	}
	for p := range prefixes {
		c.Drops = append(c.Drops, p)
	}
	slices.SortFunc(c.Drops, func(a, b netip.Prefix) int {
		if a == b {
			return 0
		}
		if prefixLess(a, b) {
			return -1
		}
		return 1
	})
	return c
}

// Restore restarts at elapsed time zero. History lifetimes restart conservatively
// in full; wall-clock downtime cannot shorten a safety hold. Origination policy
// must be reapplied explicitly. Add links with IDs above checkpoint.LastLink.
func Restore(c Config, state Checkpoint) (*Engine, error) {
	if state.Version != 1 || c.RouterID != state.RouterID {
		return nil, ErrConfig
	}
	if state.SourceHold < 3*time.Minute || state.SourceHold > 24*time.Hour || state.DropHold <= 0 || state.DropHold > 24*time.Hour {
		return nil, ErrConfig
	}
	c.SourceGC = max(c.SourceGC, state.SourceHold)
	c.InitialSeqno = state.Seqno
	e, err := New(c)
	if err != nil {
		return nil, err
	}
	if len(state.Sources) > e.cfg.MaxSources || len(state.Drops) > e.cfg.MaxRoutes {
		return nil, ErrLimit
	}
	e.lastLink = state.LastLink
	for _, s := range state.Sources {
		if !validPrefix(s.Prefix) || s.RouterID == 0 || s.RouterID == RouterID(^uint64(0)) || s.Metric == Infinity {
			return nil, ErrConfig
		}
		sk := sourceKey{s.Prefix, s.RouterID}
		if _, ok := e.sources[sk]; ok {
			return nil, ErrConfig
		}
		e.sources[sk] = &source{seq: s.Seqno, metric: s.Metric, expires: e.cfg.SourceGC}
	}
	for _, p := range state.Drops {
		if !validPrefix(p) {
			return nil, ErrConfig
		}
		e.drops[p] = max(hold(e.cfg.UpdateInterval), state.DropHold)
	}
	return e, nil
}
