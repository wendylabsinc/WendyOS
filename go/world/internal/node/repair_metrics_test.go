package node

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/world/internal/authority"
	"github.com/wendylabsinc/wendy/go/world/internal/runtimeobs"
	"github.com/wendylabsinc/wendy/go/world/internal/world"
)

func observedCounter(t *testing.T, m *runtimeobs.Collector, name string) uint64 {
	t.Helper()
	for _, value := range m.Snapshot().Counters {
		if value.Name == name {
			return value.Value
		}
	}
	t.Fatal("missing counter", name)
	return 0
}

func observedDurationCount(t *testing.T, m *runtimeobs.Collector, name string) uint64 {
	t.Helper()
	for _, value := range m.Snapshot().Durations {
		if value.Name == name {
			return value.Count
		}
	}
	t.Fatal("missing duration", name)
	return 0
}

func TestProofMetricsCountActualSerializedAttempts(t *testing.T) {
	m := runtimeobs.New()
	n := &Node{cfg: Config{Metrics: m, ProofPath: filepath.Join(t.TempDir(), "proofs")}}
	proofs := map[string]authority.SignedRecord{}
	if err := n.saveProofs(proofs); err != nil {
		t.Fatal(err)
	}
	written, err := os.ReadFile(n.cfg.ProofPath)
	if err != nil {
		t.Fatal(err)
	}
	if observedDurationCount(t, m, "proof_store.commit") != 1 || observedCounter(t, m, "proof_store.serialized_bytes") != uint64(len(written)) {
		t.Fatal("successful attempt or final envelope bytes not measured")
	}
	malformed := map[string]authority.SignedRecord{"invalid": {Component: world.Component{Payload: json.RawMessage(`{`)}}}
	if err := n.saveProofs(malformed); err == nil {
		t.Fatal("invalid payload unexpectedly serialized")
	}
	if observedDurationCount(t, m, "proof_store.commit") != 1 {
		t.Fatal("serialization refusal counted as a disk attempt")
	}
	n.cfg.ProofPath = t.TempDir() // Renaming a file onto a directory must fail.
	if err := n.saveProofs(proofs); err == nil {
		t.Fatal("expected proof write failure")
	}
	if observedDurationCount(t, m, "proof_store.commit") != 2 || observedCounter(t, m, "proof_store.commit_failures") != 1 || observedCounter(t, m, "proof_store.serialized_bytes") != 2*uint64(len(written)) {
		t.Fatal("failed attempt or candidate bytes not measured")
	}
}

func TestRepairMetricsDistinguishOutcomesAndUnattemptedRecords(t *testing.T) {
	a := newFixture(t, nil, "origin")
	b := newFixture(t, a.root, "receiver")
	m := runtimeobs.New()
	b.n.cfg.Metrics = m
	epoch := allocate(t, a)
	for i := 0; i < 3; i++ {
		c := component(a, epoch, 1)
		c.Key.Entity = fmt.Sprintf("gas-%d", i)
		publish(t, a, c)
	}
	peer := Peer{URL: a.server.URL, Audience: "origin"}
	if err := b.n.syncWorld(context.Background(), peer, b.credentials); err != nil {
		t.Fatal(err)
	}
	commits := observedDurationCount(t, m, "proof_store.commit")
	if err := b.n.syncWorld(context.Background(), peer, b.credentials); err != nil {
		t.Fatal(err)
	}
	if observedCounter(t, m, "repair.records_fetched") != 3 || observedDurationCount(t, m, "proof_store.commit") != commits {
		t.Fatal("unchanged repair fetched or rewrote proofs")
	}
	inventory, err := a.n.localRepairInventory()
	if err != nil {
		t.Fatal(err)
	}
	var entry RepairEntry
	for _, e := range inventory {
		entry = e
		break
	}
	record := a.n.proofs[entry.ComponentSHA256]
	record.Component.Timing.AgeHint = record.Component.TTL - time.Second
	if _, err = a.n.apply(record, false); err != nil {
		t.Fatal(err)
	}
	if err = b.n.syncWorld(context.Background(), peer, b.credentials); err != nil {
		t.Fatal(err)
	}
	forged := record
	forged.Component.Version.Sequence++
	forged.Signature = append([]byte(nil), record.Signature...)
	forged.Signature[0] ^= 1
	entry.Version = forged.Component.Version
	entry.ComponentSHA256, _ = recordDigest(forged.Component)
	entry.ProofSHA256, _ = repairProofDigest(forged)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/repair/inventory" {
			writeJSON(w, 200, RepairInventoryResponse{Version: RepairVersion, Entries: []RepairEntry{entry}})
			return
		}
		writeJSON(w, 200, RepairFetchResponse{Version: RepairVersion, Records: []RepairRecord{{Entry: entry, Record: forged}}, Unavailable: []string{}})
	}))
	defer server.Close()
	if err = b.n.syncWorld(context.Background(), Peer{URL: server.URL, Audience: "origin"}, b.credentials); err == nil {
		t.Fatal("forged origin accepted")
	}
	for name, want := range map[string]uint64{"repair.rounds": 4, "repair.round_failures": 1, "repair.records_fetched": 5, "repair.applied": 3, "repair.duplicate": 1, "repair.obsolete": 0, "repair.rejected": 1} {
		if got := observedCounter(t, m, name); got != want {
			t.Fatalf("%s=%d,want%d", name, got, want)
		}
	}
	if err = b.n.syncWorld(context.Background(), Peer{URL: "invalid"}, b.credentials); err == nil {
		t.Fatal("invalid peer accepted")
	}
	if observedCounter(t, m, "repair.rounds") != 5 || observedCounter(t, m, "repair.round_failures") != 2 || observedCounter(t, m, "repair.records_fetched") != 5 {
		t.Fatal("prefetch failure invented fetched records")
	}
}
