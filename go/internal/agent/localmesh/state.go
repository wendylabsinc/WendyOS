package localmesh

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"

	"github.com/wendylabsinc/WendyOS/babel"
)

// PersistentState contains safety history, never active peer connections. The
// origin revision advances durably before signing a new directory announcement.
type PersistentState struct {
	Version    int
	Org, Asset int32
	Revision   uint64
	Babel      *babel.Checkpoint
	Receipts   []DirectoryReceipt
}

type StateStore struct {
	path     string
	previous []byte
}

func OpenStateStore(path string, org, asset int32) (*StateStore, PersistentState, error) {
	s := &StateStore{path: path}
	state := PersistentState{Version: 1, Org: org, Asset: asset}
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return s, state, nil
	}
	if err != nil {
		return nil, state, err
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, 4<<20+1))
	if err != nil {
		return nil, state, err
	}
	if len(data) > 4<<20 {
		return nil, state, errors.New("oversized local-mesh safety state")
	}
	if err = json.Unmarshal(data, &state); err != nil {
		return nil, state, err
	}
	if state.Version != 1 || state.Org != org || state.Asset != asset {
		return nil, state, errors.New("local-mesh safety state identity mismatch")
	}
	s.previous = append([]byte(nil), data...)
	return s, state, nil
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
