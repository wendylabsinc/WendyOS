//go:build darwin || linux

package commands

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"testing"
)

type orinSeedFixture struct {
	Device    string `json:"device"`
	Storage   string `json:"storage"`
	Version   string `json:"version"`
	URL       string `json:"url"`
	SHA256    string `json:"sha256"`
	SizeBytes int64  `json:"size_bytes"`
}

func readOrinSeedFixture(path string) (*orinSeedFixture, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var fixture orinSeedFixture
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&fixture); err != nil {
		return nil, err
	}
	if err := decoder.Decode(new(any)); !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("Orin fixture must contain exactly one JSON value")
	}
	if (fixture.Device != "jetson-orin-nano" && fixture.Device != "jetson-agx-orin") ||
		(fixture.Storage != "nvme" && fixture.Storage != "emmc") || fixture.Version == "" ||
		!regexp.MustCompile(`^[a-zA-Z0-9._-]+$`).MatchString(fixture.Version) ||
		!regexp.MustCompile(`^[a-f0-9]{64}$`).MatchString(fixture.SHA256) || fixture.SizeBytes <= 0 {
		return nil, fmt.Errorf("invalid pinned Orin seed fixture")
	}
	return &fixture, nil
}

func TestOrinSeedProducerArchiveRemainsUnchanged(t *testing.T) {
	dir := t.TempDir()
	source := filepath.Join(dir, "producer.tar.zst")
	payload := []byte("producer-owned archive bytes")
	if err := os.WriteFile(source, payload, 0444); err != nil {
		t.Fatal(err)
	}
	info := &recoveryFlashpackInfo{SizeBytes: int64(len(payload)), Checksum: fmt.Sprintf("%x", sha256.Sum256(payload))}
	target := filepath.Join(dir, "private", "cache.tar.zst")
	if err := stageOrinSeedArchive(source, target, info); err != nil {
		t.Fatal(err)
	}
	if err := stageOrinSeedArchive(source, target, info); err == nil {
		t.Fatal("accepted replacement of an existing cache target")
	}
	if err := os.Remove(target); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(source)
	if err != nil || !bytes.Equal(got, payload) {
		t.Fatalf("cache removal changed producer artifact: %q, %v", got, err)
	}
	info.Checksum = fmt.Sprintf("%064x", 0)
	if err := stageOrinSeedArchive(source, target, info); err == nil {
		t.Fatal("accepted wrong producer archive digest")
	}
	if _, err := os.Lstat(target); !os.IsNotExist(err) {
		t.Fatalf("bad archive created a cache entry: %v", err)
	}
}

func TestOrinSeedFixtureRejectsTrailingOrUnknownInput(t *testing.T) {
	path := filepath.Join(t.TempDir(), "fixture.json")
	valid := `{"device":"jetson-orin-nano","storage":"nvme","version":"0.20.2","url":"https://example.invalid/producer","sha256":"` + fmt.Sprintf("%064x", 1) + `","size_bytes":123}`
	for _, input := range []string{valid, valid + " {}", valid[:len(valid)-1] + `,"unknown":1}`} {
		if err := os.WriteFile(path, []byte(input), 0600); err != nil {
			t.Fatal(err)
		}
		_, err := readOrinSeedFixture(path)
		if (err == nil) != (input == valid) {
			t.Fatalf("input %q: %v", input, err)
		}
	}
}

// Reuse an existing producer artifact without editing or deleting its bytes.
// ResolveRecovery validates every consumed file and only unlinks our cache link.
func stageOrinSeedArchive(source, target string, info *recoveryFlashpackInfo) error {
	stat, err := os.Stat(source)
	if err != nil {
		return err
	}
	if !stat.Mode().IsRegular() || stat.Size() != info.SizeBytes {
		return fmt.Errorf("pinned Orin archive size or type mismatch")
	}
	if err := verifySHA256(source, info.Checksum); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(target), 0755); err != nil {
		return err
	}
	if _, err := os.Lstat(target); err == nil || !os.IsNotExist(err) {
		return fmt.Errorf("Orin fixture archive cache target must be new")
	}
	if err := os.Link(source, target); err != nil {
		return copyRegularFile(source, target, 0644)
	}
	return nil
}

func verifyOrinSeedAgent(t *testing.T, image string, expected []byte) {
	t.Helper()
	data, err := readFATFile(t, image, "wendy-agent")
	if err != nil || !bytes.Equal(data, expected) {
		t.Errorf("Orin seed Agent bytes differ after injection/write: %v", err)
	}
}

func verifyOrinSeedCacheUnchanged(t *testing.T, image string, expected []byte) {
	t.Helper()
	data, err := os.ReadFile(image)
	if err != nil || !bytes.Equal(data, expected) {
		t.Errorf("Orin rehearsal modified immutable cached config image: %v", err)
	}
}
