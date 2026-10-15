package localmesh

import (
	"errors"
	"testing"
	"time"
)

func TestDirectoryWithdrawalSurvivesRestart(t *testing.T) {
	d, chain, key, now := directoryFixture(t)
	var saved []DirectoryReceipt
	if err := d.SetPersistence(nil, func(r []DirectoryReceipt) error { saved = append([]DirectoryReceipt(nil), r...); return nil }, now); err != nil {
		t.Fatal(err)
	}
	m := Manifest{Version: 1, Org: 64, Asset: 445, Revision: 1, Issued: now.UnixMilli(), Expires: now.Add(time.Minute).UnixMilli(), AgentPort: 50052}
	old, _ := SignManifest(m, chain, key, now)
	if _, err := d.Accept(old, now); err != nil {
		t.Fatal(err)
	}
	m.Revision = 2
	m.Withdraw = true
	m.Expires = now.Add(time.Second).UnixMilli()
	withdrawal, _ := SignManifest(m, chain, key, now)
	if _, err := d.Accept(withdrawal, now); err != nil {
		t.Fatal(err)
	}
	restored, _ := NewDirectory(64, 10, d.cache)
	if err := restored.SetPersistence(saved, func([]DirectoryReceipt) error { return nil }, now.Add(2*time.Second)); err != nil {
		t.Fatal(err)
	}
	if len(restored.Snapshot(now.Add(2*time.Second))) != 0 {
		t.Fatal("history restored as presence")
	}
	if _, err := restored.Accept(old, now.Add(2*time.Second)); !errors.Is(err, ErrStaleManifest) {
		t.Fatal("withdrawal forgotten", err)
	}
}

func TestDirectoryPersistenceFailureDoesNotPublish(t *testing.T) {
	d, chain, key, now := directoryFixture(t)
	d.SetPersistence(nil, func([]DirectoryReceipt) error { return errors.New("disk unavailable") }, now)
	m := Manifest{Version: 1, Org: 64, Asset: 445, Revision: 1, Issued: now.UnixMilli(), Expires: now.Add(time.Minute).UnixMilli(), AgentPort: 50052}
	w, _ := SignManifest(m, chain, key, now)
	if _, err := d.Accept(w, now); err == nil {
		t.Fatal("storage failure ignored")
	}
	if len(d.Snapshot(now)) != 0 {
		t.Fatal("uncommitted manifest published")
	}
}
