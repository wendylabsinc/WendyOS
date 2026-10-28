package localmesh

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/wendylabsinc/WendyOS/go/babel"
	"github.com/wendylabsinc/wendy/go/internal/shared/meshid"
)

// PersistentState contains safety history, never active peer connections. The
// origin revision advances durably before signing a new directory announcement.
//
// Device is the full device principal and RouterID is the Babel routing identity
// selected for it. They are stored together because the checkpoint's feasibility
// history is only meaningful for the router ID that produced it.
type PersistentState struct {
	Version  int
	Device   string `json:"device" cbor:"d"`
	RouterID uint64 `json:"routerID" cbor:"r"`
	Revision uint64 `json:"revision" cbor:"R"`
	Babel    *babel.Checkpoint
	Receipts []DirectoryReceipt
}

type StateStore struct {
	path     string
	previous []byte
}

// OpenDeviceStateStore separates enrollment identities while retaining the
// safety history of an identity that returns after re-enrollment. The unscoped
// file is only an import source: a valid checkpoint for this exact principal is
// copied durably, never reset. Legacy numeric identities and other principals
// remain untouched and cannot lend their revisions or Babel history to this one.
func OpenDeviceStateStore(path string, device meshid.Device, routerID babel.RouterID) (*StateStore, PersistentState, error) {
	// Stay in the existing state directory: atomicStateWrite fsyncs this
	// directory before acknowledging persistence. Newly created ancestor
	// directories would need their own fsyncs to make that guarantee hold.
	ext := filepath.Ext(path)
	scoped := strings.TrimSuffix(path, ext) + "." + device.Tenant().String() + "." + device.UUID().String() + ext
	if _, err := os.Stat(scoped); !errors.Is(err, os.ErrNotExist) {
		return OpenStateStore(scoped, device, routerID)
	}
	old, data, err := readPersistentState(path)
	if errors.Is(err, os.ErrNotExist) {
		return OpenStateStore(scoped, device, routerID)
	}
	if err != nil {
		return nil, PersistentState{}, err
	}
	if old.Device == device.Principal() {
		store, state, err := OpenStateStore(path, device, routerID)
		if err != nil {
			return nil, state, err
		}
		store.path, store.previous = scoped, nil
		if err := store.Save(state); err != nil {
			return nil, state, err
		}
		return store, state, nil
	}
	// Recognize only the actual old numeric format, not a malformed current
	// checkpoint missing its principal. Never populate missing disk fields from
	// the current credentials: that would relabel another router's history.
	var legacy struct{ Org, Asset int32 }
	_ = json.Unmarshal(data, &legacy)
	_, principalErr := meshid.DeviceFromPrincipal(old.Device)
	if old.Version != 1 || (principalErr != nil && !(old.Device == "" && legacy.Org > 0 && legacy.Asset > 0)) {
		return nil, old, errors.New("wendy net safety state identity mismatch")
	}
	return OpenStateStore(scoped, device, routerID)
}

// OpenStateStore loads the safety state pinned to one device principal.
//
// routerID is a candidate for a new instance. Existing state for the same
// device retains its router ID together with its Babel feasibility history,
// even if hardware selection or the random fallback changes on restart.
// State for another principal or an inconsistent checkpoint is rejected.
func OpenStateStore(path string, device meshid.Device, routerID babel.RouterID) (*StateStore, PersistentState, error) {
	s := &StateStore{path: path}
	principal := device.Principal()
	if principal == "" || len(principal) > maxPrincipalBytes || routerID == 0 || routerID == babel.RouterID(^uint64(0)) {
		return nil, PersistentState{}, errors.New("invalid wendy net instance identity")
	}
	state := PersistentState{Version: 1, Device: principal, RouterID: uint64(routerID)}
	loaded, data, err := readPersistentState(path)
	if errors.Is(err, os.ErrNotExist) {
		return s, state, nil
	}
	if err != nil {
		return nil, state, err
	}
	state = loaded
	if state.Version != 1 || state.Device != principal {
		return nil, state, errors.New("wendy net safety state identity mismatch")
	}
	if _, err := meshid.DeviceFromPrincipal(state.Device); err != nil {
		return nil, state, errors.New("wendy net safety state identity mismatch")
	}
	if state.RouterID == 0 || state.RouterID == uint64(babel.RouterID(^uint64(0))) {
		return nil, state, errors.New("wendy net safety state has no router ID")
	}
	if state.Babel != nil && state.Babel.RouterID != babel.RouterID(state.RouterID) {
		return nil, state, errors.New("wendy net checkpoint belongs to another router ID")
	}
	s.previous = append([]byte(nil), data...)
	return s, state, nil
}

func readPersistentState(path string) (PersistentState, []byte, error) {
	var state PersistentState
	f, err := os.Open(path)
	if err != nil {
		return state, nil, err
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, 4<<20+1))
	if err != nil {
		return state, nil, err
	}
	if len(data) > 4<<20 {
		return state, nil, errors.New("oversized wendy net safety state")
	}
	err = json.Unmarshal(data, &state)
	return state, data, err
}

func (s *StateStore) Save(state PersistentState) error {
	data, err := json.Marshal(state)
	if err != nil {
		return err
	}
	if bytes.Equal(data, s.previous) {
		return nil
	}
	if err = atomicStateWrite(s.path, data); err != nil {
		return err
	}
	s.previous = append(s.previous[:0], data...)
	return nil
}

func atomicStateWrite(path string, data []byte) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, ".local-mesh-state-*")
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
	if err = os.Rename(f.Name(), path); err != nil {
		return err
	}
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}
