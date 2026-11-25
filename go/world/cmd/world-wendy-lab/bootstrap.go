package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
)

type seedFrame struct {
	ID       string `json:"id"`
	Revision string `json:"revision"`
}
type seedConfig struct {
	Listen                  string          `json:"listen"`
	DataPath                string          `json:"data_path"`
	ProofPath               string          `json:"proof_path"`
	RevocationPath          string          `json:"revocation_path"`
	ReplayPath              string          `json:"replay_path"`
	Tenant                  string          `json:"tenant"`
	Site                    string          `json:"site"`
	ClockDomain             string          `json:"clock_domain"`
	Audience                string          `json:"audience"`
	Frames                  []seedFrame     `json:"frames"`
	RootPublicKey           []byte          `json:"root_public_key"`
	Revocations             json.RawMessage `json:"revocations"`
	AllowInsecureSimulation bool            `json:"allow_insecure_simulation"`
	MaxRecords              int             `json:"max_records"`
}

// canonicalSeed permits no task, relay, peer, evidence or credential fields.
func canonicalSeed(data []byte) ([]byte, error) {
	if len(data) > 64<<10 {
		return nil, errors.New("node seed exceeds bound")
	}
	var cfg seedConfig
	d := json.NewDecoder(bytes.NewReader(data))
	d.DisallowUnknownFields()
	if err := d.Decode(&cfg); err != nil {
		return nil, errors.New("invalid simulation node seed")
	}
	if d.Decode(new(any)) != io.EOF {
		return nil, errors.New("trailing node seed data")
	}
	host, portText, err := net.SplitHostPort(cfg.Listen)
	port, _ := strconv.Atoi(portText)
	if err != nil || host != "0.0.0.0" || port < 1024 || port > 65535 || cfg.Tenant != "lab" || cfg.Site != "synthetic-office" || cfg.Audience != "wendy-sim-node" || !strings.HasPrefix(cfg.ClockDomain, "simulation:") || len(cfg.ClockDomain) <= 11 || !cfg.AllowInsecureSimulation || cfg.MaxRecords != 512 || len(cfg.RootPublicKey) != 32 || len(cfg.Revocations) == 0 || len(cfg.Frames) != 1 || cfg.Frames[0] != (seedFrame{"office-floor", "office-frame-v1"}) {
		return nil, errors.New("node seed must use bounded simulation observation scope")
	}
	if cfg.DataPath != "/data/world.state" || cfg.ProofPath != "/data/world.proofs" || cfg.RevocationPath != "/data/world.revocations" || cfg.ReplayPath != "/data/world.replay" {
		return nil, errors.New("node state must remain in /data")
	}
	return json.Marshal(cfg)
}

func readRegular(path string, limit int64) ([]byte, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Size() > limit {
		return nil, errors.New("expected bounded regular file")
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err == nil && int64(len(b)) > limit {
		err = errors.New("file exceeds bound")
	}
	return b, err
}

// prepareNode persists a canonical public seed before executing the node. An
// existing volume cannot silently acquire another run, root or expiry policy.
func prepareNode(seedPath, dataDir string) (string, error) {
	b, err := readRegular(seedPath, 64<<10)
	if err != nil {
		return "", err
	}
	canonical, err := canonicalSeed(b)
	if err != nil {
		return "", err
	}
	if err = os.MkdirAll(dataDir, 0700); err != nil {
		return "", err
	}
	info, err := os.Lstat(dataDir)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return "", errors.New("data directory must be a real private volume")
	}
	path := filepath.Join(dataDir, "node.json")
	old, err := readRegular(path, 64<<10)
	if err == nil {
		prior, e := canonicalSeed(old)
		if e != nil || !bytes.Equal(prior, canonical) {
			return "", errors.New("persistent node configuration differs from image seed; use a distinct run and volume")
		}
		return path, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	lock, err := os.OpenFile(filepath.Join(dataDir, "bootstrap.lock"), os.O_CREATE|os.O_RDWR|syscall.O_NOFOLLOW, 0600)
	if err != nil {
		return "", err
	}
	defer lock.Close()
	if info, err := lock.Stat(); err != nil || !info.Mode().IsRegular() {
		return "", errors.New("bootstrap lock must be a regular file")
	}
	if err = syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		return "", fmt.Errorf("bootstrap already running: %w", err)
	}
	defer syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)
	// Check again after acquiring the initialization lock.
	if old, err = readRegular(path, 64<<10); err == nil {
		prior, e := canonicalSeed(old)
		if e != nil || !bytes.Equal(prior, canonical) {
			return "", errors.New("persistent node configuration differs from image seed")
		}
		return path, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	f, err := os.CreateTemp(dataDir, ".node-seed-")
	if err != nil {
		return "", err
	}
	tmp := f.Name()
	defer os.Remove(tmp)
	if _, err = f.Write(canonical); err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err == nil {
		err = closeErr
	}
	if err != nil {
		return "", err
	}
	if err = os.Rename(tmp, path); err != nil {
		return "", err
	}
	dir, err := os.Open(dataDir)
	if err != nil {
		return "", err
	}
	defer dir.Close()
	if err = dir.Sync(); err != nil {
		return "", err
	}
	return path, nil
}
