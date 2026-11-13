//go:build linux

package nanprovider

import (
	"strings"
	"time"
)

func failedPendingNDP(link *radioLink, event map[string]string) bool {
	return link != nil && link.peer.NDI == "" && strings.TrimSuffix(event["failure"], ",") == "1"
}

// ndpSetupRecovery covers a fresh radio epoch that never authenticates a neighbor.
// Only explicit failures matched to admitted, owned pending paths count.
// Discovery and ordinary established-path disconnects are not evidence.
// A warm epoch keeps radioRecovery's existing authenticated-peer ordering.
// This detector can request only the existing private NDI retry, never a
// management-session reset or an additional recovery allowance.
type ndpSetupRecovery struct {
	first     time.Time
	failures  int
	triggered bool
}

func (r *ndpSetupRecovery) setupFailed(link *radioLink, event map[string]string, now time.Time) {
	if !failedPendingNDP(link, event) {
		return
	}
	if r.first.IsZero() {
		r.first = now
	}
	if r.failures < 3 {
		r.failures++
	}
}

func (r *ndpSetupRecovery) ready(now time.Time, healthy, previouslyAuthenticated bool) bool {
	if healthy || previouslyAuthenticated {
		*r = ndpSetupRecovery{}
		return false
	}
	if r.triggered || r.failures < 3 || r.first.IsZero() || now.Sub(r.first) < 90*time.Second {
		return false
	}
	r.triggered = true
	return true
}
