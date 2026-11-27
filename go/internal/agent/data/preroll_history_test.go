package data

import (
	"fmt"
	"math/rand"
	"slices"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/shared/appconfig"
	recordingpb "github.com/wendylabsinc/wendy/go/proto/gen/recordingpb"
	"golang.org/x/sync/errgroup"
)

func TestPreRollExpiryReleasesEvictedPayloads(t *testing.T) {
	now := (10 * time.Minute).Nanoseconds()
	old := now - preRollWindow.Nanoseconds() - 1
	for _, reason := range []string{"age", "budget"} {
		t.Run(reason, func(t *testing.T) {
			m := &Manager{preRoll: []bufferedRecord{
				{receiptNanos: old, encoded: []byte("discarded")},
				{receiptNanos: now, encoded: []byte("retained")},
			}}
			backing := m.preRoll
			m.preRollBytes = len(backing[0].encoded) + len(backing[1].encoded)
			if reason == "budget" {
				m.preRoll[0].receiptNanos = now
				m.preRollBytes = preRollLimit + 1
			}
			m.evictPreRoll(now)
			if len(m.preRoll) != 1 || string(m.preRoll[0].encoded) != "retained" {
				t.Fatal("wrong retained record")
			}
			if backing[0].encoded != nil || backing[0].receiptNanos != 0 {
				t.Fatal("evicted record remains reachable in backing storage")
			}
		})
	}
}

// Both entry points can contribute receipts in a different order from their
// arrival. Compare every expiry and episode-window query against a plain list.
func TestPreRollLossHistoryPreservesDisorderedReceipts(t *testing.T) {
	var m Manager
	rng := rand.New(rand.NewSource(4148))
	now := (10 * time.Minute).Nanoseconds()
	var expected []int64
	for i := 0; i < 6000; i++ {
		now += (time.Second / 10).Nanoseconds()
		if i == 2000 || i == 4000 {
			// Resume after an idle period with only part of the loss history
			// expired, exercising the bounded linear rebuild as well.
			now += time.Minute.Nanoseconds()
		}
		receipt := now - int64(rng.Intn(360))*time.Second.Nanoseconds()
		m.preRoll = []bufferedRecord{{receiptNanos: receipt, encoded: []byte("budgeted")}}
		m.preRollBytes = preRollLimit + 1
		m.evictPreRoll(now)
		cutoff := now - preRollWindow.Nanoseconds()
		if receipt >= cutoff {
			expected = append(expected, receipt)
		}
		expected = slices.DeleteFunc(expected, func(v int64) bool { return v < cutoff })
		actual := append([]int64(nil), m.preRollEvicted...)
		slices.Sort(actual)
		want := append([]int64(nil), expected...)
		slices.Sort(want)
		if !slices.Equal(actual, want) {
			t.Fatalf("history differs after receipt %d", i)
		}
		for _, window := range []time.Duration{time.Second, time.Minute, preRollWindow} {
			var count uint64
			for _, v := range expected {
				if v >= now-window.Nanoseconds() && v <= now {
					count++
				}
			}
			if got := m.preRollLostInWindowLocked(now, window); got != count {
				t.Fatalf("window %s: got %d, want %d", window, got, count)
			}
		}
	}
	// The boundary remains inclusive, and history expires entirely afterward.
	m.evictPreRoll(now + preRollWindow.Nanoseconds() + 1)
	if len(m.preRollEvicted) != 0 {
		t.Fatal("expired history remains")
	}
}

func TestPreRollLossHistoryIncludesConcurrentAppAndStreamIntake(t *testing.T) {
	m, err := NewManager(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	now, err := readBootTime()
	if err != nil {
		t.Fatal(err)
	}
	// Fill the logical byte budgets with recent small records so both real
	// intake methods evict on their first operation. Their durable I/O remains
	// outside this test; volatile ingestion still owns payloads and locks.
	m.preRoll = []bufferedRecord{{receiptNanos: now, encoded: []byte("old")}}
	m.preRollBytes = preRollLimit
	m.streamPreRoll = []bufferedStreamRecord{{record: &recordingpb.StoredRecord{ReceiptBoottimeNanos: now}, size: 3}}
	m.streamPreRollSize = streamPreRollBytes
	var workers errgroup.Group
	workers.SetLimit(4)
	for worker := 0; worker < 4; worker++ {
		workers.Go(func() error {
			for i := 0; i < 32; i++ {
				if _, err := m.RecordApplication("test.app", ApplicationRecord{Version: 1, Type: "prediction", Model: "detector"}); err != nil {
					return err
				}
				if _, err := m.RecordStream("test.app", "", "samples", appconfig.RecordingStream{}, &recordingpb.Record{Payload: []byte("sample")}); err != nil {
					return err
				}
			}
			return nil
		})
	}
	if err := workers.Wait(); err != nil {
		t.Fatal(err)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.preRollEvicted) != 4*32*2+2 {
		t.Fatalf("lost concurrent loss receipts: got %d, want 258", len(m.preRollEvicted))
	}
	for i := 1; i < len(m.preRollEvicted); i++ {
		if m.preRollEvicted[(i-1)/2] > m.preRollEvicted[i] {
			t.Fatal("concurrent intake broke receipt ordering")
		}
	}
}

func TestPreRollLossHistoryRestoresAcrossJournals(t *testing.T) {
	m, err := NewManager(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	now, err := readBootTime()
	if err != nil {
		t.Fatal(err)
	}
	// More than the 16 MiB stream budget, distributed across journals whose
	// traversal order differs from receipt order. Write real persisted frames
	// so startup exercises restoreStreamPreRoll rather than a synthetic heap.
	payload := make([]byte, 256<<10)
	const records = 96
	for i := 0; i < records; i++ {
		stream := fmt.Sprintf("samples-%d", i%3)
		j, err := m.recordings.journal("test.app", "", stream)
		if err != nil {
			t.Fatal(err)
		}
		stored := &recordingpb.StoredRecord{
			Record: &recordingpb.Record{Id: fmt.Sprint(i), Payload: payload},
			AppId:  "test.app", Stream: stream,
			StreamDescriptor:     &recordingpb.StreamDescriptor{MediaType: "application/octet-stream"},
			ReceiptBoottimeNanos: now - int64(i+1), ReceiptBootId: bootID(), ReceiptUnixNanos: time.Now().UnixNano(),
		}
		if _, err := j.appendConfigured(stored, nil); err != nil {
			t.Fatal(err)
		}
	}
	fresh, err := NewManager(m.root)
	if err != nil {
		t.Fatal(err)
	}
	if len(fresh.preRollEvicted) == 0 || len(fresh.preRollEvicted)+len(fresh.streamPreRoll) != records {
		t.Fatalf("restored record accounting: retained %d, lost %d", len(fresh.streamPreRoll), len(fresh.preRollEvicted))
	}
	for i := 1; i < len(fresh.preRollEvicted); i++ {
		if fresh.preRollEvicted[(i-1)/2] > fresh.preRollEvicted[i] {
			t.Fatal("restoration did not establish receipt heap order")
		}
	}
	old := append([]int64(nil), fresh.preRollEvicted...)
	slices.Sort(old)
	cutoff := old[len(old)/2]
	fresh.evictPreRoll(cutoff + preRollWindow.Nanoseconds())
	want := len(old) - len(old)/2
	if len(fresh.preRollEvicted) != want || fresh.preRollLostInWindowLocked(cutoff+preRollWindow.Nanoseconds(), preRollWindow) != uint64(want) {
		t.Fatal("restored receipts lost their inclusive expiry boundary")
	}
}
