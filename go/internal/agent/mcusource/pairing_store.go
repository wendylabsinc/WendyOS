package mcusource

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/shared/certs"
)

// SensorPairing binds a source device by numeric asset ID or PKI principal.
type SensorPairing struct {
	SourceAssetID      int32     `json:"sourceAssetId"`
	SourcePrincipalURI string    `json:"sourcePrincipalUri,omitempty"`
	OrgID              int32     `json:"orgId"`
	Name               string    `json:"name"`
	SensorAllowlist    []string  `json:"sensorAllowlist,omitempty"`
	CreatedAt          time.Time `json:"createdAt"`
	// Transport selects how this source's manifest/frames are fetched:
	// "grpc" (agent-hosted source), "wendycom" (Wendy Lite), or "tcp"/empty
	// (MCU raw-TCP, the default for pairings written before this field existed).
	Transport string `json:"transport,omitempty"`
}

// Key keeps direct PKI identities separate from legacy numeric asset IDs.
func (p SensorPairing) Key() string {
	if p.SourcePrincipalURI != "" {
		return p.SourcePrincipalURI
	}
	return fmt.Sprintf("asset:%d", p.SourceAssetID)
}

func (p SensorPairing) Validate() error {
	if p.SourcePrincipalURI == "" {
		if p.SourceAssetID <= 0 {
			return fmt.Errorf("sensor source identity is required")
		}
		return nil
	}
	id, err := certs.ParseDevicePrincipal(p.SourcePrincipalURI)
	if err != nil || p.SourceAssetID != 0 || p.SourcePrincipalURI != certs.DeviceSPIFFEURI(id.TenantUUID, id.EntityID) {
		return fmt.Errorf("sensor source must have one direct PKI device identity")
	}
	if p.Transport != "wendycom" {
		return fmt.Errorf("PKI sensor sources require WendyCom transport")
	}
	return nil
}

// PairingStore persists pairings to a JSON file under the agent state dir.
type PairingStore struct {
	path string
	mu   sync.Mutex
	by   map[string]SensorPairing
}

func NewPairingStore(path string) *PairingStore {
	return &PairingStore{path: path, by: make(map[string]SensorPairing)}
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
	s.by = make(map[string]SensorPairing, len(list))
	for _, p := range list {
		if err := p.Validate(); err != nil {
			return err
		}
		s.by[p.Key()] = p
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
	sort.Slice(out, func(i, j int) bool { return out[i].Key() < out[j].Key() })
	return out
}

// NameFor returns the friendly name of the pairing for sourceAssetID, and
// whether a pairing with a non-empty name exists. Used by the audio device
// enumeration to name a mounted source's Loopback subdevice.
func (s *PairingStore) NameFor(sourceAssetID int32) (string, bool) {
	return s.NameForKey((SensorPairing{SourceAssetID: sourceAssetID}).Key())
}

func (s *PairingStore) NameForKey(key string) (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.by[key]
	if !ok || p.Name == "" {
		return "", false
	}
	return p.Name, true
}

func (s *PairingStore) Add(p SensorPairing) error {
	if err := p.Validate(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if p.CreatedAt.IsZero() {
		p.CreatedAt = time.Now().UTC()
	}
	s.by[p.Key()] = p
	return s.saveLocked()
}

func (s *PairingStore) Remove(sourceAssetID int32) error {
	return s.RemovePairing(SensorPairing{SourceAssetID: sourceAssetID})
}

func (s *PairingStore) RemovePairing(p SensorPairing) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.by, p.Key())
	return s.saveLocked()
}

func (s *PairingStore) saveLocked() error {
	list := make([]SensorPairing, 0, len(s.by))
	for _, p := range s.by {
		list = append(list, p)
	}
	sort.Slice(list, func(i, j int) bool { return list[i].Key() < list[j].Key() })
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
