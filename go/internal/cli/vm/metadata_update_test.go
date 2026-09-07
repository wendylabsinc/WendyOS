package vm

import (
	"errors"
	"os"
	"testing"
)

func TestMetadataUpdateExcludesOtherWritersAndLifecycleOperations(t *testing.T) {
	s := newTestStore(t)
	createTestVM(t, s, "dev", Meta{ImageVersion: "original"})
	other := &Store{Root: s.Root}
	err := s.updateMeta("dev", func(m *Meta) bool {
		// The callback runs between reading and replacing the record. Separate
		// lock handles model competing CLI processes at the dangerous instant.
		for label, op := range map[string]func() error{
			"hostname": func() error { return other.RecordHostname("dev", "competing") },
			"port":     func() error { return other.RecordAgentPort("dev", 50100) },
			"remove":   func() error { return other.Remove("dev") },
			"start": func() error {
				lock, err := other.acquireRunLock("dev")
				if lock != nil {
					lock.Close()
				}
				return err
			},
		} {
			if err := op(); !errors.Is(err, ErrLifecycleBusy) {
				t.Errorf("%s during metadata update = %v", label, err)
			}
		}
		m.Hostname = "learned"
		return true
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := other.RecordAgentPort("dev", 50100); err != nil {
		t.Fatal(err)
	}
	if err := other.RecordHostname("dev", "renamed"); err != nil {
		t.Fatal(err)
	}
	m, ok := s.ReadMeta("dev")
	if !ok || m.Hostname != "renamed" || m.AgentPort != 50100 || m.ImageVersion != "original" || m.MAC == "" {
		t.Fatalf("metadata fields lost: %+v", m)
	}
}

func TestMetadataUpdatesRespectLifecycleLockBeforeReading(t *testing.T) {
	s := newTestStore(t)
	createTestVM(t, s, "dev", Meta{})
	lock, err := s.acquireLifecycleLock("dev")
	if err != nil {
		t.Fatal(err)
	}
	for _, op := range []func() error{
		func() error { return s.RecordHostname("dev", "learned") },
		func() error { return s.RecordAgentPort("dev", 50100) },
	} {
		if err := op(); !errors.Is(err, ErrLifecycleBusy) {
			t.Errorf("update while lifecycle busy = %v", err)
		}
	}
	lock.Close()
	if err := s.Remove("dev"); err != nil {
		t.Fatal(err)
	}
	if err := s.RecordHostname("dev", "stale"); err == nil {
		t.Fatal("updated removed VM")
	}
	if _, err := os.Stat(s.Dir("dev")); !os.IsNotExist(err) {
		t.Fatalf("update recreated removed directory: %v", err)
	}
	createTestVM(t, s, "dev", Meta{ImageVersion: "replacement"})
	if err := s.RecordAgentPort("dev", 50102); err != nil {
		t.Fatal(err)
	}
	m, _ := s.ReadMeta("dev")
	if m.ImageVersion != "replacement" || m.Hostname != "" || m.AgentPort != 50102 {
		t.Fatalf("replacement clobbered: %+v", m)
	}
}
