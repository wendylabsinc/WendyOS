package meshcatalog

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"testing"
	"time"
)

// Freeze the initial writer after the old snapshot is taken, then admit newer
// live/withdraw records through the actual session broadcast channel first.
func TestLegacyInitialSnapshotConcurrentNewerRecords(t *testing.T) {
	for _, marker := range []bool{false, true} {
		t.Run(fmt.Sprint(marker), func(t *testing.T) {
			legacySession(t, new(Runtime), marker)
			f := newFixture(t)
			c, _ := f.newCatalog(t, 533, "default", nil, nil)
			recv, _ := f.newCatalog(t, 534, "default", nil, nil)
			for i := 0; i < 24; i++ {
				s := testSpec()
				s.ServiceID = fmt.Sprintf("service-%02d", i)
				if _, err := c.Publish(s, f.now); err != nil {
					t.Fatal(err)
				}
			}
			original := c.Records(f.now)
			r := legacyRuntime(t, c)
			a, b := net.Pipe()
			blocked := &legacyBlockedConn{Conn: a, started: make(chan struct{})}
			ctx, cancel := context.WithCancel(context.Background())
			done := make(chan struct{})
			go func() { defer close(done); legacySession(t, r, marker)(ctx, 534, blocked) }()
			defer func() {
				cancel()
				b.Close()
				select {
				case <-done:
				case <-time.After(time.Second):
					t.Error("session did not join")
				}
			}()
			select {
			case <-blocked.started:
			case <-time.After(time.Second):
				t.Fatal("writer never blocked")
			}
			p := legacyPeer(t, r, 534)
			s := testSpec()
			s.ServiceID = "service-22"
			s.TXT = []string{"new=1"}
			newer, err := c.Publish(s, f.now)
			if err != nil {
				t.Fatal(err)
			}
			withdrawn, err := c.Remove(s.AppID, "service-23", f.now)
			if err != nil {
				t.Fatal(err)
			}
			p.out <- newer
			p.out <- withdrawn
			end := time.Now().Add(time.Second)
			for len(p.out) != 0 && time.Now().Before(end) {
				time.Sleep(time.Millisecond)
			}
			if len(p.out) != 0 {
				t.Fatal("broadcasts were not consumed while initial writer blocked")
			}
			b.SetReadDeadline(time.Now().Add(3 * time.Second))
			syncer := NewSynchronizer(recv, recv.cache)
			count := 0
			seen := map[string]Record{}
			for {
				m, err := ReadMessage(b)
				if err != nil {
					t.Fatal(err)
				}
				if m.Kind == "snapshot-done" {
					if !marker || count != 24 {
						t.Fatalf("marker before all records: %d", count)
					}
					break
				}
				if _, _, err := syncer.Receive(m, f.now); err != nil {
					t.Fatal(err)
				}
				if m.Kind == "record" {
					var rec Record
					if err := json.Unmarshal(m.Record.Body, &rec); err != nil {
						t.Fatal(err)
					}
					count++
					seen[rec.Key.ServiceID] = rec
					if rec.Key.ServiceID == "service-22" || rec.Key.ServiceID == "service-23" {
						if rec.Generation != 2 {
							t.Fatalf("old snapshot generation leaked after newer broadcast: %+v", rec)
						}
					}
				}
				if !marker && count == 24 {
					break
				}
			}
			if len(seen) != 24 || seen["service-22"].Generation != 2 || seen["service-22"].Withdraw || !seen["service-23"].Withdraw {
				t.Fatalf("bad final newer records: %+v", seen)
			}
			// Origin receipt rejects stale publishes even if another relay sends them.
			for _, w := range original {
				var rec Record
				json.Unmarshal(w.Body, &rec)
				if rec.Key.ServiceID == "service-22" || rec.Key.ServiceID == "service-23" {
					_, changed, err := syncer.Receive(Message{Kind: "record", Record: &w}, f.now)
					if err != nil || len(changed) != 0 {
						t.Fatalf("receiver accepted stale replay: changed=%d err=%v", len(changed), err)
					}
				}
			}
			for _, w := range recv.Records(f.now) {
				var rec Record
				json.Unmarshal(w.Body, &rec)
				if rec.Key.ServiceID == "service-23" && !rec.Withdraw {
					t.Fatal("withdrawn record resurrected")
				}
			}
		})
	}
}
