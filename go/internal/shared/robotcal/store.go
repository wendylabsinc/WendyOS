package robotcal

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"sync"
	"time"
)

// DefaultRoot is where a device keeps what its robots know about themselves.
//
// On the disk-backed /var/lib tree, beside the camera registries the agent
// already owns, and for the same reasons: a calibration is a fact about the
// machine, it has to survive a reboot, and it has to be readable by the apps
// that run on it. It deliberately does NOT live in the CLI's $HOME — a
// per-robot fact stored per-laptop means the robot stops knowing its own
// calibration the moment someone else connects to it, and it fails silently.
const DefaultRoot = "/var/lib/wendy/robot"

// UnitRecord is everything one physical robot knows about itself.
//
// Keyed per unit rather than per device on purpose: an SO-101 rig is two arms
// on one Jetson, a leader and a follower, with different measured travel on
// every joint. Keying by device would give them one shared calibration, which
// would be wrong for both.
type UnitRecord struct {
	Unit string `json:"unit"`
	// ProfileKind is the model this unit is. Part 1's `robot configure` is what
	// records it properly; until then the wizard writes whatever --profile
	// selected, and refuses to run a procedure against a different model than
	// the records were taken with.
	ProfileKind string `json:"profile_kind"`
	// StableID binds this unit to the physical device behind it, using the
	// platform's existing stable-id scheme (by-id:… / by-path:…) rather than a
	// raw serial or a device node. A servo bus on /dev/ttyACM1 renumbers across
	// boots exactly as /dev/videoN does.
	StableID     string            `json:"stable_id,omitempty"`
	Calibrations map[string]Record `json:"calibrations,omitempty"`
	// Sessions are procedures that were interrupted part-way. A seven-joint
	// sweep stopped at joint four does not start over.
	Sessions  map[string]Session `json:"sessions,omitempty"`
	UpdatedAt time.Time          `json:"updated_at"`
}

// Store is where calibration records live. An interface because the transport
// differs by where the CLI is running: on the device the store is a file the
// agent owns, and from a laptop it has to be reached over gRPC.
type Store interface {
	// Load returns the unit's record, or a zero-valued one when the unit has
	// never been calibrated. A unit that does not exist yet is not an error.
	Load(ctx context.Context, unit string) (UnitRecord, error)
	// PutRecord stores one calibration result, whatever its verdict. A failed
	// calibration is recorded too: "measured and it does not fit" is a fact
	// worth keeping, and it is not the same as never having run.
	PutRecord(ctx context.Context, unit string, rec Record) error
	// ClearRecord invalidates a calibration. After a hand is replaced, the old
	// calibration is worse than none.
	ClearRecord(ctx context.Context, unit, id string) error
	// PutSession checkpoints an in-progress procedure.
	PutSession(ctx context.Context, unit string, s Session) error
	// ClearSession drops a checkpoint once the procedure has finished.
	ClearSession(ctx context.Context, unit, procedureID string) error
	// SetProfileKind and SetStableID record what this unit is and what backs it.
	SetProfileKind(ctx context.Context, unit, kind string) error
	SetStableID(ctx context.Context, unit, stableID string) error
	// Describe says where the store physically is, for the operator and for
	// error messages.
	Describe() string
}

// unitNamePattern keeps a unit id usable as a directory name. A unit is named
// by a person ("leader", "follower"), so this is a validation rather than an
// escaping problem: a name that needs escaping is a name that will be typed
// wrong.
var unitNamePattern = regexp.MustCompile(`^[a-z0-9]([a-z0-9._-]{0,62}[a-z0-9])?$`)

// NoSuchCalibrationError is what ClearRecord returns when there is nothing to
// clear.
//
// A type rather than a bare message because a second implementation of Store
// has to classify it across a process boundary — the agent turns it into a gRPC
// NOT_FOUND — and matching on message text would make a reworded sentence a
// silent behaviour change. Clearing a calibration that is not there must never
// read as success: `clear` is what an operator runs after a repair, and a typo
// reporting "done" would leave the old calibration in place and trusted.
type NoSuchCalibrationError struct {
	Unit string
	ID   string
}

func (e *NoSuchCalibrationError) Error() string {
	return fmt.Sprintf("unit %q has no calibration %q to clear", e.Unit, e.ID)
}

// ValidUnit refuses a unit name that could not be a directory.
func ValidUnit(unit string) error {
	if !unitNamePattern.MatchString(unit) {
		return fmt.Errorf("invalid robot unit name %q: use lowercase letters, digits, '.', '_' and '-' "+
			"(a unit is one physical robot on this device, such as 'leader' or 'follower')", unit)
	}
	return nil
}

// FileStore is the device-local store: one JSON file per unit under Root.
//
// This is the same shape the agent already uses for its camera registries, and
// it is what the agent will serve once a RobotService RPC exists to reach it
// from off-device.
type FileStore struct {
	Root string
	// Now is indirected so a test can pin the timestamps it writes.
	Now func() time.Time

	mu sync.Mutex
}

// NewFileStore returns a store rooted at root, or at DefaultRoot when empty.
func NewFileStore(root string) *FileStore {
	if root == "" {
		root = DefaultRoot
	}
	return &FileStore{Root: root, Now: time.Now}
}

func (s *FileStore) Describe() string { return s.Root }

func (s *FileStore) path(unit string) string {
	return filepath.Join(s.Root, "units", unit, "calibration.json")
}

func (s *FileStore) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

// Load reads a unit's record. A missing file is the normal state of a robot
// nobody has calibrated yet, so it is not an error.
func (s *FileStore) Load(_ context.Context, unit string) (UnitRecord, error) {
	if err := ValidUnit(unit); err != nil {
		return UnitRecord{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.loadLocked(unit)
}

func (s *FileStore) loadLocked(unit string) (UnitRecord, error) {
	data, err := os.ReadFile(s.path(unit))
	if errors.Is(err, os.ErrNotExist) {
		return UnitRecord{Unit: unit}, nil
	}
	if err != nil {
		return UnitRecord{}, fmt.Errorf("reading the calibration store for unit %q: %w", unit, err)
	}
	var rec UnitRecord
	if err := json.Unmarshal(data, &rec); err != nil {
		return UnitRecord{}, fmt.Errorf("parsing the calibration store for unit %q (%s): %w", unit, s.path(unit), err)
	}
	rec.Unit = unit
	return rec, nil
}

// update applies a mutation to a unit's record and writes it back atomically.
func (s *FileStore) update(unit string, mutate func(*UnitRecord) error) error {
	if err := ValidUnit(unit); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	rec, err := s.loadLocked(unit)
	if err != nil {
		return err
	}
	if err := mutate(&rec); err != nil {
		return err
	}
	rec.Unit = unit
	rec.UpdatedAt = s.now()
	return s.saveLocked(rec)
}

func (s *FileStore) saveLocked(rec UnitRecord) error {
	data, err := json.MarshalIndent(rec, "", "  ")
	if err != nil {
		return err
	}
	path := s.path(rec.Unit)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("creating the calibration store directory: %w", err)
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return fmt.Errorf("writing the calibration store: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("installing the calibration store: %w", err)
	}
	return nil
}

func (s *FileStore) PutRecord(_ context.Context, unit string, rec Record) error {
	return s.update(unit, func(u *UnitRecord) error {
		if u.Calibrations == nil {
			u.Calibrations = make(map[string]Record)
		}
		u.Calibrations[rec.ID] = rec
		return nil
	})
}

func (s *FileStore) ClearRecord(_ context.Context, unit, id string) error {
	return s.update(unit, func(u *UnitRecord) error {
		if _, ok := u.Calibrations[id]; !ok {
			return &NoSuchCalibrationError{Unit: unit, ID: id}
		}
		delete(u.Calibrations, id)
		// A cleared calibration invalidates any half-finished attempt at it too.
		delete(u.Sessions, id)
		return nil
	})
}

func (s *FileStore) PutSession(_ context.Context, unit string, sess Session) error {
	return s.update(unit, func(u *UnitRecord) error {
		if u.Sessions == nil {
			u.Sessions = make(map[string]Session)
		}
		u.Sessions[sess.ProcedureID] = sess
		return nil
	})
}

func (s *FileStore) ClearSession(_ context.Context, unit, procedureID string) error {
	return s.update(unit, func(u *UnitRecord) error {
		delete(u.Sessions, procedureID)
		return nil
	})
}

func (s *FileStore) SetProfileKind(_ context.Context, unit, kind string) error {
	return s.update(unit, func(u *UnitRecord) error {
		u.ProfileKind = kind
		return nil
	})
}

func (s *FileStore) SetStableID(_ context.Context, unit, stableID string) error {
	return s.update(unit, func(u *UnitRecord) error {
		u.StableID = stableID
		return nil
	})
}

// Units lists the units this store holds records for.
func (s *FileStore) Units() ([]string, error) {
	entries, err := os.ReadDir(filepath.Join(s.Root, "units"))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var units []string
	for _, e := range entries {
		if e.IsDir() {
			units = append(units, e.Name())
		}
	}
	sort.Strings(units)
	return units, nil
}

// Statuses joins what the profile asks for with what the store has, so a
// listing always shows every calibration the robot needs — including the ones
// nobody has ever run, which are the interesting ones.
func Statuses(p *Profile, unit UnitRecord) []Status {
	out := make([]Status, 0, len(p.RequiresCalibration))
	for _, proc := range p.RequiresCalibration {
		st := Status{ID: proc.ID, Method: string(proc.Method), Verdict: VerdictNeverRun}
		desc, err := Method(proc.Method)
		if err != nil {
			st.Reason = err.Error()
			out = append(out, st)
			continue
		}
		class, err := Resolve(desc.ClassFloor, proc.Class)
		if err != nil {
			st.Reason = err.Error()
			out = append(out, st)
			continue
		}
		st.Class = string(class)
		st.Runnable = desc.Implemented
		if !desc.Implemented {
			st.Reason = desc.Unavailable
		}
		if rec, ok := unit.Calibrations[proc.ID]; ok {
			copied := rec
			st.Record = &copied
			st.Verdict = rec.Verdict()
			// A record taken against a different budget than the profile now
			// declares was judged by a gate that no longer applies.
			if !rec.Budget.comparableWith(proc.Budget) || rec.Budget.Value != proc.Budget.Value {
				st.Verdict = VerdictStale
				st.Reason = fmt.Sprintf("measured against a budget of %s, the profile now asks for %s",
					rec.Budget, proc.Budget)
			}
		}
		out = append(out, st)
	}
	return out
}
