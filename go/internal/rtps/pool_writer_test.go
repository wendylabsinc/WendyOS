package rtps

import (
	"testing"
)

func TestPoolKeepsPublishingAndReadOnlyParticipantsApart(t *testing.T) {
	pool, cfg := testPool(t)
	reading := acquire(t, pool, cfg)
	publishing := cfg
	publishing.Publish = true
	first := acquire(t, pool, publishing)
	second := acquire(t, pool, publishing)

	// A participant tells its peers once, when they meet, whether it
	// publishes. One that was created to read cannot start later, and the
	// readers of a graph must not begin to draw subscription traffic because
	// someone else publishes there.
	if pool.Participants() != 2 {
		t.Fatalf("pool holds %d participants, want one that reads and one that publishes", pool.Participants())
	}
	if first.entry.participant != second.entry.participant {
		t.Fatal("two publishing consumers of one graph got separate participants")
	}
	if reading.entry.participant == first.entry.participant {
		t.Fatal("reading and publishing consumers share a participant")
	}
	if _, err := reading.NewWriter(Publication{Topic: testTopic, Type: testType}); err == nil {
		t.Fatal("a reading lease created a writer")
	}
	if _, err := first.NewWriter(Publication{Topic: testTopic, Type: testType}); err != nil {
		t.Fatalf("publishing lease: %v", err)
	}
}

func TestClosingALeaseWithdrawsOnlyItsOwnWriters(t *testing.T) {
	pool, cfg := testPool(t)
	cfg.Publish = true
	mine := acquire(t, pool, cfg)
	theirs := acquire(t, pool, cfg)
	p := mine.entry.participant

	a, err := mine.NewWriter(Publication{Topic: testTopic, Type: testType})
	if err != nil {
		t.Fatal(err)
	}
	closedEarly, err := mine.NewWriter(Publication{Topic: "rt/closed/early", Type: testType})
	if err != nil {
		t.Fatal(err)
	}
	b, err := theirs.NewWriter(Publication{Topic: "rt/theirs", Type: testType})
	if err != nil {
		t.Fatal(err)
	}
	if err := closedEarly.Close(); err != nil {
		t.Fatal(err)
	}

	if err := mine.Close(); err != nil {
		t.Fatal(err)
	}
	sample := []byte{0x00, 0x01, 0x00, 0x00}
	if err := a.Write(sample); err == nil {
		t.Fatal("writer outlived the lease that created it")
	}
	if err := b.Write(sample); err != nil {
		t.Fatalf("another consumer's writer stopped working: %v", err)
	}
	p.mu.Lock()
	open := len(p.writers)
	p.mu.Unlock()
	if open != 1 {
		t.Fatalf("participant has %d open writers, want only the other consumer's", open)
	}
	if _, err := mine.NewWriter(Publication{Topic: testTopic, Type: testType}); err == nil {
		t.Fatal("a closed lease created a writer")
	}
}

func TestLeaseDoesNotAccumulateWritersClosedByTheirOwner(t *testing.T) {
	pool, cfg := testPool(t)
	cfg.Publish = true
	lease := acquire(t, pool, cfg)
	for range 50 {
		w, err := lease.NewWriter(Publication{Topic: testTopic, Type: testType})
		if err != nil {
			t.Fatal(err)
		}
		if err := w.Close(); err != nil {
			t.Fatal(err)
		}
	}
	pool.mu.Lock()
	held := len(lease.writers)
	pool.mu.Unlock()
	if held > 1 {
		t.Fatalf("lease still holds %d closed writers", held)
	}
}
