package directstream

import "testing"

func TestACKSchedule(t *testing.T) {
	a, err := NewACKSchedule(Receipt{Received: 0, Limit: 4096}, 0)
	if err != nil {
		t.Fatal(err)
	}
	flush := func(now int64, want bool) Receipt {
		t.Helper()
		s, due := a.Due(now)
		if due != want {
			t.Fatalf("due=%v want=%v at %d", due, want, now)
		}
		return s
	}
	initial := flush(0, true)
	if err = a.Written(initial); err != nil {
		t.Fatal(err)
	}
	flush(1000000, false)
	if err = a.Update(Receipt{Received: 100, Limit: 4096}, 1000000); err != nil {
		t.Fatal(err)
	}
	flush(1249999, false)
	if err = a.Update(Receipt{Received: 200, Limit: 4096}, 1080000); err != nil {
		t.Fatal(err)
	}
	queued := flush(1250000, true)
	// Failure to queue does not suppress it. New progress while a write is in
	// flight remains pending when that older snapshot is acknowledged locally.
	flush(1250001, true)
	if err = a.Update(Receipt{Received: 300, Limit: 4096}, 1250001); err != nil {
		t.Fatal(err)
	}
	if err = a.Written(queued); err != nil {
		t.Fatal(err)
	}
	latest := flush(1250002, true)
	if err = a.Written(latest); err != nil {
		t.Fatal(err)
	}
	flush(2000000, false)
	if err = a.Update(Receipt{Received: 1324, Limit: 4096}, 2000000); err != nil {
		t.Fatal(err)
	}
	latest = flush(2000000, true)
	a.Written(latest)
	if err = a.Update(Receipt{Received: 1324, Limit: 5120}, 2000001); err != nil {
		t.Fatal(err)
	}
	latest = flush(2000001, true)
	a.Written(latest)
	if err = a.Update(Receipt{Received: 1324, Limit: 5376}, 2000002); err != nil {
		t.Fatal(err)
	}
	flush(2250001, false)
	latest = flush(2250002, true)
	a.Written(latest)
	if err = a.Update(Receipt{Received: 1323, Limit: 5376}, 2250003); err == nil {
		t.Fatal("regression accepted")
	}
	if err = a.Written(Receipt{Received: 5000, Limit: 5000}); err == nil {
		t.Fatal("unowned bytes accepted")
	}
	// A tiny window cannot wait for an unreachable ACK byte threshold.
	a, _ = NewACKSchedule(Receipt{Received: 0, Limit: 256}, 0)
	s, _ := a.Due(0)
	a.Written(s)
	a.Update(Receipt{Received: 256, Limit: 256}, 1)
	flush(1, true)
}

func TestFINReceiptIsImmediateAndMonotonic(t *testing.T) {
	a, _ := NewACKSchedule(Receipt{Limit: 4096}, 0)
	old, _ := a.Due(0)
	a.Written(old)
	if err := a.Update(Receipt{Limit: 4096, FINSeen: true}, 1); err != nil {
		t.Fatal(err)
	}
	fin, due := a.Due(1)
	if !due || !fin.FINSeen {
		t.Fatal("FIN not immediately acknowledged")
	}
	if err := a.Written(old); err != nil {
		t.Fatal(err)
	}
	if _, due = a.Due(1); !due {
		t.Fatal("old queued state erased FIN")
	}
	if err := a.Update(old, 2); err == nil {
		t.Fatal("FIN retracted")
	}
	if err := a.Written(fin); err != nil {
		t.Fatal(err)
	}
	if err := a.Written(old); err == nil {
		t.Fatal("sent FIN retracted")
	}
	if _, due = a.Due(2); due {
		t.Fatal("FIN still pending")
	}
	b, _ := NewACKSchedule(old, 0)
	if err := b.Written(fin); err == nil {
		t.Fatal("unobserved FIN acknowledged")
	}
}

func TestACKImmediatelyReopensExhaustedAdvertisedWindow(t *testing.T) {
	for _, ackBeforeDelivery := range []bool{false, true} {
		a, _ := NewACKSchedule(Receipt{Limit: 256}, 0)
		initial, _ := a.Due(0)
		a.Written(initial)
		if ackBeforeDelivery {
			a.Update(Receipt{Received: 256, Limit: 256}, 1)
			full, _ := a.Due(1)
			a.Written(full)
		}
		// Local delivery reserved fresh storage, so the current receive limit is
		// no longer full. The sender still only knows the exhausted old limit.
		a.Update(Receipt{Received: 256, Limit: 512}, 2)
		state, due := a.Due(2)
		if !due || state.Limit != 512 {
			t.Fatalf("delivery waited for ACK timer (previous ACK=%v): %+v", ackBeforeDelivery, state)
		}
	}
}

func TestACKCoalescesHalfKilobyteDelivery(t *testing.T) {
	a, _ := NewACKSchedule(Receipt{Limit: 4096}, 0)
	initial, _ := a.Due(0)
	a.Written(initial)
	a.Update(Receipt{Received: 512, Limit: 4608}, 10)
	if _, due := a.Due(90010); due {
		t.Fatal("half-kilobyte delivery emitted a standalone STATE before coalescing")
	}
	a.Update(Receipt{Received: 1024, Limit: 5120}, 120000)
	if _, due := a.Due(120000); !due {
		t.Fatal("kilobyte progress was not acknowledged promptly")
	}
}
