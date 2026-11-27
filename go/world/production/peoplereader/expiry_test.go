package peoplereader

import (
	"bytes"
	"context"
	"testing"
	"time"
)

func TestExpiryDuringReadOmitsOriginalAndFreshResultRecovers(t *testing.T) {
	for _, mode := range []string{"sample", "physical", "empty-authority-query"} {
		t.Run(mode, func(t *testing.T) {
			f, api := readerFixture(t)
			s, err := New(f.cfg, api)
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			first, err := s.Step(context.Background())
			if err != nil || len(first.Tracks) != 1 {
				t.Fatalf("initial result count=%d err=%v", len(first.Tracks), err)
			}
			f.mu.Lock()
			at := f.queries + 3 // Second batch authorization Query.
			f.queryHook = func(n int) {
				if n != at {
					return
				}
				if mode == "sample" {
					f.now = f.now.Add(2 * time.Second)
				} else {
					f.w.Earliest = f.w.Earliest.Add(6 * time.Second)
					f.w.Latest = f.w.Latest.Add(6 * time.Second)
					f.w.Monotonic += 6 * time.Second
				}
				if mode == "empty-authority-query" {
					f.raw = nil
				}
			}
			f.mu.Unlock()
			empty, err := s.Step(context.Background())
			if err != nil || empty.Status != "ready" || len(empty.Tracks) != 0 || len(empty.AbsentTrackIDs) != 1 || empty.AbsentTrackIDs[0] != first.Tracks[0].Payload.TrackID {
				t.Fatalf("expired original was not omitted: status=%s tracks=%d absent=%v err=%v", empty.Status, len(empty.Tracks), empty.AbsentTrackIDs, err)
			}
			// A repeated old result cannot revive or extend the old track.
			empty, err = s.Step(context.Background())
			if err != nil || empty.Status != "ready" || len(empty.Tracks) != 0 || len(empty.AbsentTrackIDs) != 0 {
				t.Fatalf("old result revived: status=%s tracks=%d err=%v", empty.Status, len(empty.Tracks), err)
			}
			f.fresh()
			fresh, err := s.Step(context.Background())
			if err != nil || fresh.Status != "ready" || len(fresh.Tracks) != 1 || fresh.Tracks[0].Version.Sequence != 2 || !bytes.Equal(fresh.Tracks[0].EnvelopeJSON, f.raw) {
				t.Fatalf("fresh result did not recover: status=%s tracks=%d err=%v", fresh.Status, len(fresh.Tracks), err)
			}
		})
	}
}

func TestEmptyAuthorityQueryWithUnexpiredOriginalRemainsTerminal(t *testing.T) {
	f, api := readerFixture(t)
	f.queryHook = func(n int) {
		if n == 3 {
			f.raw = nil
		}
	}
	s, err := New(f.cfg, api)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	r, err := s.Step(context.Background())
	if err == nil || r.Status != "failed" || len(r.Tracks) != 0 || len(r.AbsentTrackIDs) != 0 {
		t.Fatal("missing unexpired authority became successful absence")
	}
	if _, err = s.Step(context.Background()); err == nil {
		t.Fatal("terminal authority refusal was retried")
	}
}
