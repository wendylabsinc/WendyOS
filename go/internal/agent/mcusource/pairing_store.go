package mcusource

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

// SensorPairing binds a source device (by asset id) to this consumer.
type SensorPairing struct {
	SourceAssetID   int32     `json:"sourceAssetId"`
	OrgID           int32     `json:"orgId"`
	Name            string    `json:"name"`
	SensorAllowlist []string  `json:"sensorAllowlist,omitempty"`
	CreatedAt       time.Time `json:"createdAt"`
	// Transport selects how this source's manifest/frames are fetched:
	// "grpc" (agent-hosted source) or "tcp"/empty (MCU raw-TCP, back-compat
	// default for pairings written before this field existed).
	Transport string `json:"transport,omitempty"`
}

// PairingStore persists pairings to a JSON file under the agent state dir.
type PairingStore struct {
	path string
	mu   sync.Mutex
	by   map[int32]SensorPairing
}

func NewPairingStore(path string) *PairingStore {
	return &PairingStore{path: path, by: make(map[int32]SensorPairing)}
}

func (s *PairingStore) Load() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	data, err := os.ReadFile(s.path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	var list []SensorPairing
	if err := json.Unmarshal(data, &list); err != nil {
		return err
	}
	s.by = make(map[int32]SensorPairing, len(list))
	for _, p := range list {
		s.by[p.SourceAssetID] = p
	}
	return nil
}

func (s *PairingStore) List() []SensorPairing {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]SensorPairing, 0, len(s.by))
	for _, p := range s.by {
		out = append(out, p)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].SourceAssetID < out[j].SourceAssetID })
	return out
}

// NameFor returns the friendly name of the pairing for sourceAssetID, and
// whether a pairing with a non-empty name exists. Used by the audio device
// enumeration to name a mounted source's Loopback subdevice.
func (s *PairingStore) NameFor(sourceAssetID int32) (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.by[sourceAssetID]
	if !ok || p.Name == "" {
		return "", false
	}
	return p.Name, true
}

func (s *PairingStore) Add(p SensorPairing) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if p.CreatedAt.IsZero() {
		p.CreatedAt = time.Now().UTC()
	}
	s.by[p.SourceAssetID] = p
	return s.saveLocked()
}

func (s *PairingStore) Remove(sourceAssetID int32) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.by, sourceAssetID)
	return s.saveLocked()
}

func (s *PairingStore) saveLocked() error {
	list := make([]SensorPairing, 0, len(s.by))
	for _, p := range s.by {
		list = append(list, p)
	}
	sort.Slice(list, func(i, j int) bool { return list[i].SourceAssetID < list[j].SourceAssetID })
	data, err := json.MarshalIndent(list, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(s.path), 0o700); err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, s.path)
}
