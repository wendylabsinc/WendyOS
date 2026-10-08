package data

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/wendylabsinc/wendy/go/internal/shared/atomicfile"
)

// CampaignNotification is an emitted Wendy Data notification, not a raw
// prediction or a claim that an external notification receiver accepted it.
// The occurrence UUID and time survive delivery retries and gateway restarts.
type CampaignNotification struct {
	ID         string `json:"id"`
	Event      string `json:"event"`
	Campaign   string `json:"campaign"`
	SourceID   string `json:"source_id"`
	Model      string `json:"model"`
	Revision   string `json:"model_revision"`
	Count      int    `json:"count"`
	OccurredAt string `json:"occurred_at"`
	Sequence   uint64 `json:"sequence,omitempty"`
}

type notificationJournal struct {
	Epoch         string                 `json:"epoch"`
	Sequence      uint64                 `json:"sequence"`
	Notifications []CampaignNotification `json:"notifications"`
}

func (m *Manager) readNotificationJournal() (notificationJournal, error) {
	var journal notificationJournal
	raw, err := os.ReadFile(filepath.Join(m.root, ".data-notifications.json"))
	if os.IsNotExist(err) {
		return journal, nil
	}
	if err != nil {
		return journal, err
	}
	if len(raw) > 4<<20 {
		return journal, fmt.Errorf("notification journal exceeds limit")
	}
	err = json.Unmarshal(raw, &journal)
	return journal, err
}

// RecordNotification durably retains notification intent before either Cloud
// or a direct webhook is contacted. It is independent of the raw event journal,
// so existing Events readers neither see duplicates nor change their cursors.
func (m *Manager) RecordNotification(notification CampaignNotification) error {
	if _, err := uuid.Parse(notification.ID); err != nil || notification.Campaign == "" || notification.Event == "" {
		return fmt.Errorf("invalid notification identity")
	}
	if _, err := time.Parse(time.RFC3339Nano, notification.OccurredAt); err != nil {
		return fmt.Errorf("invalid notification occurrence time")
	}
	raw, err := json.Marshal(notification)
	if err != nil || len(raw) > 4096 {
		return fmt.Errorf("notification exceeds limit")
	}
	m.eventMu.Lock()
	defer m.eventMu.Unlock()
	journal, err := m.readNotificationJournal()
	if err != nil {
		return err
	}
	for _, old := range journal.Notifications {
		if old.ID == notification.ID {
			return nil
		}
	}
	if journal.Epoch == "" {
		journal.Epoch = uuid.NewString()
	}
	journal.Sequence++
	notification.Sequence = journal.Sequence
	journal.Notifications = append(journal.Notifications, notification)
	if len(journal.Notifications) > 512 {
		journal.Notifications = journal.Notifications[len(journal.Notifications)-512:]
	}
	raw, err = json.Marshal(journal)
	if err != nil {
		return err
	}
	return atomicfile.Write(filepath.Join(m.root, ".data-notifications.json"), raw, 0600)
}

// Notifications has the same explicit gap/replay semantics as DeviceEvents.
// appID selects the campaign's managed app identity, sh.wendy.campaign.NAME.
func (m *Manager) Notifications(appID, event, cursor string, replay bool) ([]CampaignNotification, string, bool, error) {
	m.eventMu.Lock()
	defer m.eventMu.Unlock()
	journal, err := m.readNotificationJournal()
	if err != nil {
		return nil, "", false, err
	}
	out := []CampaignNotification{}
	tail := fmt.Sprintf("%s:%d", journal.Epoch, journal.Sequence)
	if cursor == "" && !replay {
		return out, tail, false, nil
	}
	var after uint64
	gap := false
	if cursor != "" {
		epoch, number, ok := strings.Cut(cursor, ":")
		parsed, err := strconv.ParseUint(number, 10, 64)
		if !ok || err != nil {
			return nil, "", false, fmt.Errorf("invalid notification cursor")
		}
		after = parsed
		if epoch != journal.Epoch {
			gap = epoch != "" || after != 0
			after = 0
		}
		if after > journal.Sequence {
			return nil, "", false, fmt.Errorf("notification cursor is ahead of this device")
		}
	}
	if len(journal.Notifications) > 0 && after < journal.Notifications[0].Sequence-1 {
		gap = true
	}
	for _, notification := range journal.Notifications {
		if notification.Sequence > after && (appID == "" || appID == "sh.wendy.campaign."+notification.Campaign) && (event == "" || event == notification.Event) {
			out = append(out, notification)
		}
	}
	return out, tail, gap, nil
}
