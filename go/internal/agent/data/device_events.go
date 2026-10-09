package data

import (
	"encoding/json"
	"fmt"
	"github.com/google/uuid"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// DeviceEvent records detection-time facts. Receipt UTC is observation time,
// not a claim about camera timestamp accuracy. Images are never stored here.
type DeviceEvent struct {
	ID               string         `json:"id"`
	Sequence         uint64         `json:"sequence"`
	AppID            string         `json:"app_id"`
	Name             string         `json:"name"`
	Model            string         `json:"model,omitempty"`
	ObservedAt       string         `json:"observed_at"`
	BootID           string         `json:"boot_id"`
	ReceiptBootNanos int64          `json:"receipt_boottime_nanos"`
	Attributes       map[string]any `json:"attributes,omitempty"`
	Inputs           []SampleRef    `json:"inputs,omitempty"`
}
type deviceEventJournal struct {
	Epoch    string        `json:"epoch"`
	Sequence uint64        `json:"sequence"`
	Events   []DeviceEvent `json:"events"`
}

func (m *Manager) readDeviceEvents() (deviceEventJournal, error) {
	var j deviceEventJournal
	b, err := os.ReadFile(filepath.Join(m.root, ".device-events.json"))
	if os.IsNotExist(err) {
		return j, nil
	}
	if err != nil {
		return j, err
	}
	if len(b) > 4<<20 {
		return j, fmt.Errorf("device event journal exceeds limit")
	}
	err = json.Unmarshal(b, &j)
	return j, err
}

func (m *Manager) appendDeviceEvent(appID string, r ApplicationRecord, bootID string, receipt int64) error {
	// Keep the bounded inbox independent of episode recording and its lock.
	m.eventMu.Lock()
	defer m.eventMu.Unlock()
	j, err := m.readDeviceEvents()
	if err != nil {
		return err
	}
	if j.Epoch == "" {
		j.Epoch = uuid.NewString()
	}
	j.Sequence++
	e := DeviceEvent{ID: fmt.Sprintf("%s:%d", j.Epoch, j.Sequence), Sequence: j.Sequence, AppID: appID, Name: r.Name, Model: r.Model, ObservedAt: time.Now().UTC().Format(time.RFC3339Nano), BootID: bootID, ReceiptBootNanos: receipt, Attributes: r.Attributes, Inputs: r.Inputs}
	b, err := json.Marshal(e)
	if err != nil {
		return err
	}
	// Preserve the occurrence even if an app supplies an oversized payload.
	if len(b) > 4096 {
		e.Attributes = nil
		e.Inputs = nil
	}
	j.Events = append(j.Events, e)
	if len(j.Events) > 512 {
		j.Events = j.Events[len(j.Events)-512:]
	}
	b, err = json.Marshal(j)
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(m.root, ".device-events-*")
	if err != nil {
		return err
	}
	path := f.Name()
	defer os.Remove(path)
	if _, err = f.Write(b); err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	if err = os.Rename(path, filepath.Join(m.root, ".device-events.json")); err != nil {
		return err
	}
	dir, err := os.Open(m.root)
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}

// RecordDeviceEvent appends an event to the device-event journal and nowhere
// else. Leased campaigns use it: they are notify-only, so their records reach
// no open episode and no pre-roll ring, where another campaign's next episode
// would pick them up.
func (m *Manager) RecordDeviceEvent(appID string, record ApplicationRecord) error {
	receipt, err := readBootTime()
	if err != nil {
		return err
	}
	return m.appendDeviceEvent(appID, record, bootID(), receipt)
}

// DeviceEvents advances across nonmatching records as well, preventing replay
// storms. The cursor identifies this journal generation and never uses UTC order.
func (m *Manager) DeviceEvents(appID, event, cursor string, replay bool) ([]DeviceEvent, string, bool, error) {
	m.eventMu.Lock()
	defer m.eventMu.Unlock()
	j, err := m.readDeviceEvents()
	if err != nil {
		return nil, "", false, err
	}
	out := []DeviceEvent{}
	tail := fmt.Sprintf("%s:%d", j.Epoch, j.Sequence)
	if cursor == "" && !replay {
		return out, tail, false, nil
	}
	var after uint64
	gap := false
	if cursor != "" {
		epoch, n, ok := strings.Cut(cursor, ":")
		v, e := strconv.ParseUint(n, 10, 64)
		if !ok || e != nil {
			return nil, "", false, fmt.Errorf("invalid event cursor")
		}
		after = v
		if epoch != j.Epoch {
			gap = epoch != "" || after != 0
			after = 0
		}
		if after > j.Sequence {
			return nil, "", false, fmt.Errorf("event cursor is ahead of this device")
		}
	}
	if len(j.Events) > 0 && after < j.Events[0].Sequence-1 {
		gap = true
	}
	for _, e := range j.Events {
		if e.Sequence > after && (appID == "" || e.AppID == appID) && (event == "" || e.Name == event) {
			out = append(out, e)
		}
	}
	return out, tail, gap, nil
}
