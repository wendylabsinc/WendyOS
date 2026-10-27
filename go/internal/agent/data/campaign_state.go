package data

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/shared/atomicfile"
)

// Campaign states. Deployment stores a campaign armed. Disarming keeps the
// plan, its revision and everything it recorded, but stops what the campaign
// runs on the device: its triggers, its inference and its camera pre-roll.
// Arming restores them.
//
// The state is a field of the stored plan, so it survives agent restarts and
// reboots, and it is not part of the revision: switching a campaign off and on
// is not an edit to the plan.
const (
	CampaignStateArmed    = "armed"
	CampaignStateDisarmed = "disarmed"
)

// SetCampaignState persists a deployed campaign's state and reports whether it
// changed. It returns os.ErrNotExist for a campaign that is not deployed.
//
// Only the state field of the stored plan is rewritten. The plan is patched as
// raw JSON rather than decoded into Campaign and encoded again, so a field this
// agent does not know, written by a newer agent before a rollback, survives
// the switch instead of silently leaving the plan.
func (m *Manager) SetCampaignState(name, state string) (Campaign, bool, error) {
	if state != CampaignStateArmed && state != CampaignStateDisarmed {
		return Campaign{}, false, fmt.Errorf("campaign state %q is not %s or %s", state, CampaignStateArmed, CampaignStateDisarmed)
	}
	m.campaignMu.Lock()
	defer m.campaignMu.Unlock()
	campaign, err := m.Campaign(name)
	if err != nil {
		return Campaign{}, false, err
	}
	if campaign.State == state {
		return campaign, false, nil
	}
	path := filepath.Join(m.campaignDir(), name+".json")
	raw, err := os.ReadFile(path)
	if err != nil {
		return Campaign{}, false, err
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return Campaign{}, false, err
	}
	fields["state"], _ = json.Marshal(state)
	b, err := json.MarshalIndent(fields, "", "  ")
	if err != nil {
		return Campaign{}, false, err
	}
	if err := atomicfile.Write(path, append(b, '\n'), 0o640); err != nil {
		return Campaign{}, false, err
	}
	campaign.State = state
	return campaign, true, nil
}

// removedCampaign is what RemoveCampaign keeps of a plan: the policy the
// episodes it recorded are still uploaded under.
type removedCampaign struct {
	Name             string         `json:"name"`
	Revision         string         `json:"revision"`
	Fleet            string         `json:"fleet,omitempty"`
	Upload           CampaignUpload `json:"upload"`
	RemovedUnixNanos int64          `json:"removed_unix_nanos"`
}

func (m *Manager) removedCampaignPath(name string) string {
	return filepath.Join(m.campaignDir(), "removed", name+".json")
}

// RemoveCampaign deletes a deployed campaign's stored plan, so it is no longer
// listed, triggered or restored at boot, and returns the plan it removed. It
// never touches an episode: everything the campaign recorded stays in the
// store, inspectable, downloadable and in the upload queue.
//
// It keeps one thing besides the episodes: the plan's upload policy, in a small
// record under removed/. The transfer worker reads upload.when and
// upload.max_rate by campaign name when an episode's turn comes, and without
// that record a removed campaign's episodes fell back to "always, unlimited":
// removing a campaign whose plan said manual would have uploaded everything it
// had recorded. CampaignForUpload reads the record.
//
// The caller stops the campaign first. This only changes what is stored.
func (m *Manager) RemoveCampaign(name string) (Campaign, error) {
	m.campaignMu.Lock()
	defer m.campaignMu.Unlock()
	campaign, err := m.Campaign(name)
	if err != nil {
		return Campaign{}, err
	}
	record, err := json.MarshalIndent(removedCampaign{Name: campaign.Name, Revision: campaign.Revision, Fleet: campaign.Fleet, Upload: campaign.Upload, RemovedUnixNanos: time.Now().UnixNano()}, "", "  ")
	if err != nil {
		return Campaign{}, err
	}
	// The record is written before the plan goes. A failure in between leaves
	// the campaign deployed, so a retry finds it and completes the removal.
	if err := os.MkdirAll(filepath.Dir(m.removedCampaignPath(name)), 0o750); err != nil {
		return Campaign{}, err
	}
	if err := atomicfile.Write(m.removedCampaignPath(name), append(record, '\n'), 0o640); err != nil {
		return Campaign{}, err
	}
	if err := os.Remove(filepath.Join(m.campaignDir(), name+".json")); err != nil {
		return Campaign{}, err
	}
	// Without this a power cut can bring the plan back at the next boot.
	if err := syncDirectory(m.campaignDir()); err != nil {
		m.Warnf("campaign %q is removed, but syncing the campaign directory failed; a power cut now could restore it: %v", name, err)
	}
	return campaign, nil
}

// CampaignForUpload returns the plan an episode the named campaign recorded is
// uploaded under: the deployed plan, or, once the campaign has been removed,
// what RemoveCampaign kept of it, which carries the name, revision, fleet and
// upload policy and nothing else. A campaign that was never deployed, or whose
// removal predates the record, returns os.ErrNotExist.
func (m *Manager) CampaignForUpload(name string) (Campaign, error) {
	campaign, err := m.Campaign(name)
	if !errors.Is(err, os.ErrNotExist) {
		return campaign, err
	}
	raw, readErr := os.ReadFile(m.removedCampaignPath(name))
	if readErr != nil {
		return Campaign{}, err
	}
	var removed removedCampaign
	if decodeErr := json.Unmarshal(raw, &removed); decodeErr != nil {
		return Campaign{}, fmt.Errorf("reading what was kept of removed campaign %q: %w", name, decodeErr)
	}
	return Campaign{Name: removed.Name, Revision: removed.Revision, Fleet: removed.Fleet, Upload: removed.Upload}, nil
}
