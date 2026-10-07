package services

import (
	"context"
	"errors"
	"os"
	"sync"
	"time"

	agentpbv2 "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// campaignLeases holds the deadlines of leased campaigns. They live in memory
// only: a leased plan without a deadline counts as expired, so a restarted
// agent removes the leased plans of its previous run before it starts any
// inference (spec §5.3). The zero value is ready to use.
type campaignLeases struct {
	mu sync.Mutex
	// now is the clock; nil means time.Now. Tests set a fake one before
	// StartCampaignInference.
	now       func() time.Time
	deadlines map[string]time.Time
}

func (l *campaignLeases) clock() time.Time {
	if l.now != nil {
		return l.now()
	}
	return time.Now()
}

// extend sets name's deadline to now + lease and returns it.
func (l *campaignLeases) extend(name string, lease time.Duration) time.Time {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.deadlines == nil {
		l.deadlines = map[string]time.Time{}
	}
	deadline := l.clock().Add(lease)
	l.deadlines[name] = deadline
	return deadline
}

// renew extends a lease that has not lapsed. It refuses a lapsed one, even in
// the seconds before the reconcile loop removes its campaign: renewing would
// revive a watch whose client has already lost it.
func (l *campaignLeases) renew(name string, lease time.Duration) (time.Time, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.clock()
	if deadline, ok := l.deadlines[name]; !ok || !now.Before(deadline) {
		return time.Time{}, false
	}
	l.deadlines[name] = now.Add(lease)
	return l.deadlines[name], true
}

func (l *campaignLeases) forget(name string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.deadlines, name)
}

// forgetAll drops every deadline, which makes every leased plan count as lapsed.
func (l *campaignLeases) forgetAll() {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.deadlines = nil
}

// expired reports whether name's lease has lapsed. A name without a deadline
// has lapsed: deadlines live in memory, so it is a plan from before the agent
// restarted.
func (l *campaignLeases) expired(name string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	deadline, ok := l.deadlines[name]
	return !ok || !l.clock().Before(deadline)
}

// CampaignRenew pushes a leased campaign's deadline to now + lease.
func (s *DataService) CampaignRenew(_ context.Context, req *agentpbv2.DataCampaignRenewRequest) (*agentpbv2.DataCampaignRenewResponse, error) {
	// No deploymentMu: renew only extends a live deadline under the table's own
	// lock, and a lapsed lease stays lapsed, so a renew racing expiry or removal
	// cannot revive a campaign. It must not queue behind a capture startup that
	// holds deploymentMu, or an on-time renew could find its lease lapsed.
	campaign, err := s.manager.Campaign(req.GetName())
	if err != nil {
		return nil, dataStatusError(err)
	}
	if !campaign.Leased() {
		return nil, status.Errorf(codes.FailedPrecondition, "campaign %q has no lease", campaign.Name)
	}
	deadline, ok := s.leases.renew(campaign.Name, campaign.LeaseDuration())
	if !ok {
		return nil, status.Errorf(codes.NotFound, "campaign %q lease expired", campaign.Name)
	}
	return &agentpbv2.DataCampaignRenewResponse{ExpiresUnixNanos: deadline.UnixNano()}, nil
}

// expireLeases deletes every leased plan whose lease has lapsed. Reconcile
// passes call it before reading plans, so the same pass retires the deleted
// plan's job. The plan goes first: a detection that races the removal finds no
// current revision and sends nothing.
func (s *DataService) expireLeases() {
	s.deploymentMu.Lock()
	defer s.deploymentMu.Unlock()
	campaigns, err := s.manager.Campaigns()
	if err != nil {
		s.manager.Warnf("reading campaigns to expire leases: %v", err)
		return
	}
	for _, campaign := range campaigns {
		if !campaign.Leased() || !s.leases.expired(campaign.Name) {
			continue
		}
		if err := s.manager.RemoveCampaign(campaign.Name); err != nil && !errors.Is(err, os.ErrNotExist) {
			s.manager.Warnf("removing leased campaign %q after its lease lapsed: %v", campaign.Name, err)
			continue
		}
		s.leases.forget(campaign.Name)
		s.manager.Warnf("leased campaign %q was removed because its lease lapsed or the agent restarted", campaign.Name)
	}
}

// CampaignRemove stops a leased campaign and deletes its plan. It returns once
// the campaign's model process and camera subscriptions have stopped.
func (s *DataService) CampaignRemove(_ context.Context, req *agentpbv2.DataCampaignRemoveRequest) (*agentpbv2.DataCampaignRemoveResponse, error) {
	if err := s.removeLeased(req.GetName()); err != nil {
		return nil, err
	}
	// With the plan gone, a reconcile pass retires the job and waits for it.
	if s.inference != nil {
		s.inference.reconcileNow()
	}
	return &agentpbv2.DataCampaignRemoveResponse{}, nil
}

// removeLeased deletes a leased campaign's plan and forgets its deadline. It
// releases deploymentMu before CampaignRemove waits for the job.
func (s *DataService) removeLeased(name string) error {
	s.deploymentMu.Lock()
	defer s.deploymentMu.Unlock()
	campaign, err := s.manager.Campaign(name)
	if err != nil {
		return dataStatusError(err)
	}
	if !campaign.Leased() {
		return status.Errorf(codes.FailedPrecondition, "campaign %q has no lease; only leased campaigns can be removed", name)
	}
	if err := s.manager.RemoveCampaign(name); err != nil {
		return dataStatusError(err)
	}
	s.leases.forget(name)
	return nil
}
