package meshcatalog

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
)

// ReceiptStore is an atomic, owner-only on-disk generation high-water file.
// It contains no presence or private identity material. The enclosing agent
// config directory must itself be private and tied to the enrolled identity.
type ReceiptStore struct {
	mu    sync.Mutex
	path  string
	limit int
}

func NewReceiptStore(path string, limit int) (*ReceiptStore, error) {
	if path == "" || limit < 1 || limit > 8192 {
		return nil, errors.New("invalid mesh receipt store")
	}
	return &ReceiptStore{path: path, limit: limit}, nil
}

func (s *ReceiptStore) Load() ([]Receipt, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	f, err := os.Open(s.path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	max := int64(s.limit)*1024 + 1024
	data, err := io.ReadAll(io.LimitReader(f, max+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > max {
		return nil, errors.New("oversized mesh receipt file")
	}
	var disk struct {
		Version  int       `json:"version"`
		Receipts []Receipt `json:"receipts"`
	}
	if err := json.Unmarshal(data, &disk); err != nil {
		return nil, err
	}
	if disk.Version != 1 || len(disk.Receipts) > s.limit {
		return nil, errors.New("unsupported or oversized mesh receipt file")
	}
	return disk.Receipts, nil
}

func (s *ReceiptStore) Save(receipts []Receipt) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(receipts) > s.limit {
		return errors.New("too many mesh receipts")
	}
	data, err := json.Marshal(struct {
		Version  int       `json:"version"`
		Receipts []Receipt `json:"receipts"`
	}{1, receipts})
	if err != nil {
		return err
	}
	if len(data) > s.limit*1024+1024 {
		return errors.New("oversized mesh receipts")
	}
	dir := filepath.Dir(s.path)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, ".mesh-receipts-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer os.Remove(tmp)
	if err := f.Chmod(0600); err != nil {
		f.Close()
		return err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp, s.path); err != nil {
		return fmt.Errorf("replace mesh receipts: %w", err)
	}
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}
