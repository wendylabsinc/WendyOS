package robotarm

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"sync"

	pb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
)

// Store follows the Agent's JSON state convention, with atomic writes and no
// optimistic in-memory mutation on failed persistence. Corrupt state fails closed.
// Each record is proto JSON with proto field names; fields from a newer Agent
// are dropped so that a rolled-back Agent can still read the file.
type Store struct {
	path string
	mu   sync.Mutex
}

var errInvalidStore = errors.New("invalid robot arm binding store")

func NewStore(path string) *Store { return &Store{path: path} }
func (s *Store) load() ([]*pb.RobotArm, error) {
	data, err := os.ReadFile(s.path)
	if errors.Is(err, os.ErrNotExist) {
		return []*pb.RobotArm{}, nil
	}
	if err != nil {
		return nil, err
	}
	var raw []json.RawMessage
	if err = json.Unmarshal(data, &raw); err != nil {
		return nil, err
	}
	if raw == nil {
		return nil, errInvalidStore
	}
	records := make([]*pb.RobotArm, 0, len(raw))
	seen := map[string]bool{}
	for _, item := range raw {
		r := &pb.RobotArm{}
		if err = (protojson.UnmarshalOptions{DiscardUnknown: true}).Unmarshal(item, r); err != nil {
			return nil, errInvalidStore
		}
		if r.Id == "" || r.Model != "so101" || !r.Configured || seen[r.Id] {
			return nil, errInvalidStore
		}
		seen[r.Id] = true
		records = append(records, r)
	}
	return records, nil
}
func (s *Store) List() ([]*pb.RobotArm, error) { s.mu.Lock(); defer s.mu.Unlock(); return s.load() }
func (s *Store) Put(d *pb.RobotArm) error      { return s.update(d.Id, d) }
func (s *Store) Forget(id string) error        { return s.update(id, nil) }
func (s *Store) update(id string, d *pb.RobotArm) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	records, err := s.load()
	if err != nil {
		return err
	}
	out := make([]*pb.RobotArm, 0, len(records)+1)
	for _, r := range records {
		if r.Id != id {
			out = append(out, r)
		}
	}
	if d != nil {
		r := proto.Clone(d).(*pb.RobotArm)
		r.Connected = false
		r.Verified = false
		r.DevicePath = ""
		r.ConnectionStatus = ""
		r.AccessStatus = ""
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Id < out[j].Id })
	raw := make([]json.RawMessage, 0, len(out))
	for _, r := range out {
		item, err := protojson.MarshalOptions{UseProtoNames: true}.Marshal(r)
		if err != nil {
			return err
		}
		raw = append(raw, item)
	}
	// Marshal compacts each record, so protojson's build-dependent spacing
	// doesn't reach the file.
	data, err := json.MarshalIndent(raw, "", "  ")
	if err != nil {
		return err
	}
	if err = os.MkdirAll(filepath.Dir(s.path), 0700); err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(s.path), ".robot-arm-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err = f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err = f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	return os.Rename(f.Name(), s.path)
}

// Merge includes disconnected bindings and refuses to choose among duplicate
// serial identities. A location-only binding intentionally follows a USB port.
func Merge(live, saved []*pb.RobotArm) []*pb.RobotArm {
	groups := map[string][]*pb.RobotArm{}
	for _, d := range live {
		groups[d.Id] = append(groups[d.Id], proto.Clone(d).(*pb.RobotArm))
	}
	for _, r := range saved {
		matches := groups[r.Id]
		if len(matches) == 0 {
			d := proto.Clone(r).(*pb.RobotArm)
			d.DevicePath = ""
			d.Connected = false
			d.ConnectionStatus = "disconnected"
			d.AccessStatus = "unknown"
			d.Verified = false
			groups[r.Id] = []*pb.RobotArm{d}
			continue
		}
		for _, d := range matches {
			d.Name, d.Model, d.Configured, d.Verified = r.Name, r.Model, true, false
		}
	}
	out := make([]*pb.RobotArm, 0)
	for _, group := range groups {
		for _, d := range group {
			if len(group) > 1 {
				d.Connected = false
				d.ConnectionStatus = "ambiguous"
				d.Note = "Multiple endpoints share this identity; binding is blocked. Disconnect duplicates before binding."
			}
			out = append(out, d)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Id == out[j].Id {
			return out[i].DevicePath < out[j].DevicePath
		}
		return out[i].Id < out[j].Id
	})
	return out
}
