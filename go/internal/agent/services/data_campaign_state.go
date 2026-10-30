package services

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/agent/data"
	agentpbv2 "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// campaignStopWait bounds how long CampaignDisarm and CampaignRemove wait for
// the campaign's inference worker to stop, the same bound DocumentRemove uses.
const campaignStopWait = 10 * time.Second

// CampaignArm switches a disarmed campaign back on: its triggers fire again,
// its inference restarts and its camera pre-roll is re-armed.
func (s *DataService) CampaignArm(_ context.Context, req *agentpbv2.DataCampaignArmRequest) (*agentpbv2.DataCampaignArmResponse, error) {
	name := req.GetName()
	// Resolve the name before taking its lock, as disarmCampaign does.
	if _, err := s.manager.Campaign(name); err != nil {
		return nil, campaignLookupError(name, err)
	}
	// The key lock orders this after a disarm or a remove in progress. A
	// remove holds it while a forced recording stops; arming without it let
	// the state flip back to armed in that window, the ring was armed again,
	// and the plan was then deleted with the camera still held for it.
	lock := s.keyLock(name)
	lock.Lock()
	campaign, changed, err := s.manager.SetCampaignState(name, data.CampaignStateArmed)
	lock.Unlock()
	if err != nil {
		return nil, campaignLookupError(name, err)
	}
	if changed {
		// Both subscribe to cameras, so they run off the request path, exactly
		// as they do after a deploy.
		go s.armCampaign(campaign)
		if s.inference != nil {
			s.inference.wakeNow()
		}
	}
	message, err := s.campaignMessage(campaign)
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	return &agentpbv2.DataCampaignArmResponse{Campaign: message, Changed: changed}, nil
}

// CampaignDisarm switches a campaign off without removing it. It returns once
// the campaign's inference has stopped, so the camera is no longer held for it
// when the operator reads the answer.
func (s *DataService) CampaignDisarm(ctx context.Context, req *agentpbv2.DataCampaignDisarmRequest) (*agentpbv2.DataCampaignDisarmResponse, error) {
	campaign, changed, recording, err := s.disarmCampaign(req.GetName())
	if err != nil {
		return nil, campaignLookupError(req.GetName(), err)
	}
	if err := s.awaitCampaignInferenceStopped(ctx, "campaign "+campaign.Name+" is disarmed, but its inference is still stopping"); err != nil {
		return nil, err
	}
	message, err := s.campaignMessage(campaign)
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	response := &agentpbv2.DataCampaignDisarmResponse{Campaign: message, Changed: changed, Recording: recording}
	if recording != nil {
		response.Warnings = append(response.Warnings, fmt.Sprintf("episode %s is still recording; disarming does not cut it short, so it finishes its after_trigger window (%s) and is sealed normally", recording.GetId(), campaign.Capture.AfterTrigger))
	}
	if campaign.Fleet != "" {
		response.Warnings = append(response.Warnings, fmt.Sprintf("campaign %s names fleet %s. It stays disarmed on this device even when the plan is deployed again, until it is armed here", campaign.Name, campaign.Fleet))
	}
	return response, nil
}

// disarmCampaign stores the disarmed state and releases the campaign's camera
// pre-roll. It holds the campaign key's lock, which startCapture holds while it
// checks the state, so a trigger either opened its episode before this ran or
// sees the campaign disarmed. The recording in progress, if any, is returned.
func (s *DataService) disarmCampaign(name string) (data.Campaign, bool, *agentpbv2.DataEpisode, error) {
	// Resolve the name before taking its lock: keyLocks grows by one entry per
	// key, so it must only ever see deployed campaigns.
	if _, err := s.manager.Campaign(name); err != nil {
		return data.Campaign{}, false, nil, err
	}
	lock := s.keyLock(name)
	lock.Lock()
	defer lock.Unlock()
	campaign, changed, err := s.manager.SetCampaignState(name, data.CampaignStateDisarmed)
	if err != nil {
		return data.Campaign{}, false, nil, err
	}
	// Unconditionally, so a ring left behind by any earlier race is released
	// by disarming again.
	if arming := s.arming(); arming != nil {
		arming.Disarm(name)
	}
	var recording *agentpbv2.DataEpisode
	if session, ok := s.manager.ActiveSession(name); ok {
		recording = &agentpbv2.DataEpisode{Id: session.ID, Name: name, State: "recording", BootId: session.BootID}
	}
	return campaign, changed, recording, nil
}

// CampaignRemove deletes a campaign's plan and keeps everything it recorded.
func (s *DataService) CampaignRemove(ctx context.Context, req *agentpbv2.DataCampaignRemoveRequest) (*agentpbv2.DataCampaignRemoveResponse, error) {
	name := req.GetName()
	if _, err := s.manager.Campaign(name); err != nil {
		return nil, campaignLookupError(name, err)
	}
	removed, sealed, err := s.removeCampaign(ctx, name, req.GetForce())
	if err != nil {
		return nil, err
	}
	if err := s.awaitCampaignInferenceStopped(ctx, "campaign "+name+" is removed, but its inference is still stopping"); err != nil {
		return nil, err
	}
	response := &agentpbv2.DataCampaignRemoveResponse{Name: removed.Name, Revision: removed.Revision, Fleet: removed.Fleet, Sealed: sealed}
	if removed.Fleet != "" {
		response.Warnings = append(response.Warnings, fmt.Sprintf("campaign %s names fleet %s. Removing it here does not change that fleet: deploying the plan again brings it back, armed. To keep a fleet campaign off on one device, disarm it there instead of removing it; a disarmed campaign stays disarmed when its plan is deployed again", removed.Name, removed.Fleet))
	}
	return response, nil
}

// removeCampaign stops the campaign and deletes its plan under the campaign
// key's lock.
//
// The campaign is disarmed first, and that state is stored before anything is
// stopped. Every path that could bring it back reads that state: the trigger
// path under this same lock, the inference reconcile, and the re-arm that runs
// when a forced recording stops. If the removal itself then fails, the
// campaign is left disarmed rather than half removed, and running the command
// again completes it.
func (s *DataService) removeCampaign(ctx context.Context, name string, force bool) (data.Campaign, *agentpbv2.DataEpisode, error) {
	lock := s.keyLock(name)
	lock.Lock()
	defer lock.Unlock()
	session, recording := s.manager.ActiveSession(name)
	if recording && !force {
		return data.Campaign{}, nil, status.Errorf(codes.FailedPrecondition, "campaign %s is recording episode %s; wait for it to be sealed, or remove the campaign with --force to stop and seal that recording first", name, session.ID)
	}
	disarmed, _, err := s.manager.SetCampaignState(name, data.CampaignStateDisarmed)
	if err != nil {
		return data.Campaign{}, nil, campaignLookupError(name, err)
	}
	if arming := s.arming(); arming != nil {
		arming.Disarm(name)
	}
	var sealed *agentpbv2.DataEpisode
	if recording {
		episode, err := s.stopCaptureLocked(ctx, name)
		if err != nil {
			return data.Campaign{}, nil, status.Errorf(status.Code(err), "campaign %s is disarmed but not removed: stopping its recording %s failed: %s", name, session.ID, status.Convert(err).Message())
		}
		sealed = episode
	}
	// Remove only the plan that was disarmed above. A deploy doesn't take
	// this lock, so one that landed while a forced recording stopped has
	// stored a new plan and told its caller so; deleting that plan would
	// lose a deploy that reported success.
	removed, err := s.manager.RemoveCampaign(name, disarmed.Revision)
	if errors.Is(err, data.ErrCampaignRedeployed) {
		return data.Campaign{}, nil, status.Errorf(codes.FailedPrecondition, "campaign %s was deployed again while it was being removed, so it is left deployed and disarmed with the new plan; run remove again to remove it", name)
	}
	if err != nil {
		return data.Campaign{}, nil, campaignLookupError(name, err)
	}
	// Release the ring once more, for a re-arm that was already in flight
	// when the plan went.
	if arming := s.arming(); arming != nil {
		arming.Disarm(name)
	}
	return removed, sealed, nil
}

// requireArmed refuses to open an episode for a campaign that is disarmed or
// no longer deployed.
func (s *DataService) requireArmed(name string) error {
	current, err := s.manager.Campaign(name)
	if errors.Is(err, os.ErrNotExist) {
		return status.Errorf(codes.FailedPrecondition, "campaign %s was removed", name)
	}
	if err != nil {
		return dataStatusError(err)
	}
	if current.State != data.CampaignStateArmed {
		return campaignDisarmedError(name)
	}
	return nil
}

func campaignDisarmedError(name string) error {
	return status.Errorf(codes.FailedPrecondition, "campaign %s is disarmed; arm it with `wendy data campaign arm %s` first", name, name)
}

// campaignLookupError reports a campaign that is not deployed in words rather
// than as the path of a missing file.
func campaignLookupError(name string, err error) error {
	if errors.Is(err, os.ErrNotExist) {
		return status.Errorf(codes.NotFound, "campaign %s is not deployed on this device", name)
	}
	return dataStatusError(err)
}

// awaitCampaignInferenceStopped runs an inference reconcile pass and waits for
// it, which returns once every worker of a campaign that is no longer armed
// has stopped and released its cameras. Callers must not hold deploymentMu or
// a campaign key's lock: a stopping worker can be inside triggerInference,
// which takes both.
func (s *DataService) awaitCampaignInferenceStopped(ctx context.Context, pending string) error {
	if s.inference == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, campaignStopWait)
	defer cancel()
	done := make(chan struct{})
	go func() { defer close(done); s.inference.reconcileNow() }()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		// A client that gave up is Canceled, not DeadlineExceeded.
		if errors.Is(ctx.Err(), context.Canceled) {
			return status.Error(codes.Canceled, pending)
		}
		return status.Error(codes.DeadlineExceeded, pending)
	}
}
