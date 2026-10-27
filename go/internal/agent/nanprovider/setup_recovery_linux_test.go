//go:build linux

package nanprovider

import (
	"testing"
	"time"
)

func TestInitialNDPSetupRecoveryIsBoundedAndCannotResetManagement(t *testing.T) {
	now := time.Unix(1000, 0)
	pending := &radioLink{}
	failed := map[string]string{"failure": "1"}
	r := ndpSetupRecovery{}
	for i := 0; i < 3; i++ {
		r.setupFailed(pending, failed, now.Add(time.Duration(i)*31*time.Second))
	}
	if r.ready(now.Add(89*time.Second), false, false) || !r.ready(now.Add(90*time.Second), false, false) {
		t.Fatal("three explicit setup failures did not use the bounded private recovery deadline")
	}
	if r.ready(now.Add(time.Hour), false, false) {
		t.Fatal("one outage repeatedly requested recovery")
	}
	budget := nanRecovery{ownsSession: true}
	if budget.next(false) != resetNDI || budget.canReset(false) || budget.next(false) != resetNone {
		t.Fatal("setup failures escalated beyond the single private NDI allowance")
	}
}

func TestInitialNDPSetupRecoveryDoesNotUseDiscoveryOrEstablishedDisconnects(t *testing.T) {
	now := time.Unix(1000, 0)
	for _, tc := range []struct {
		name  string
		link  *radioLink
		event map[string]string
	}{
		{"unmatched event", nil, map[string]string{"failure": "1"}},
		{"ordinary disconnect", &radioLink{}, map[string]string{"failure": "0"}},
		{"established path", &radioLink{peer: radioPeer{NDI: "02:00:00:00:00:01"}}, map[string]string{"failure": "1"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := ndpSetupRecovery{}
			for i := 0; i < 4; i++ {
				r.setupFailed(tc.link, tc.event, now)
			}
			if r.ready(now.Add(time.Hour), false, false) || r.failures != 0 {
				t.Fatal("non-setup evidence requested recovery")
			}
		})
	}
	r := ndpSetupRecovery{}
	for i := 0; i < 2; i++ {
		r.setupFailed(&radioLink{}, map[string]string{"failure": "1,"}, now)
	}
	if r.ready(now.Add(time.Hour), false, false) {
		t.Fatal("fewer than three owned setup failures requested recovery")
	}
}

func TestInitialNDPSetupRecoveryPreservesHealthyAndWarmEpochs(t *testing.T) {
	now := time.Unix(1000, 0)
	for _, healthy := range []bool{false, true} {
		for _, warm := range []bool{false, true} {
			if !healthy && !warm {
				continue
			}
			r := ndpSetupRecovery{}
			for i := 0; i < 3; i++ {
				r.setupFailed(&radioLink{}, map[string]string{"failure": "1"}, now)
			}
			if r.ready(now.Add(time.Hour), healthy, warm) || r.failures != 0 {
				t.Fatal("initial-setup detector overrode a healthy or authenticated epoch")
			}
		}
	}
}
