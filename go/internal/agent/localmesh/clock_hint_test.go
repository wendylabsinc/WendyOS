package localmesh

import (
	"testing"
	"time"
)

func TestClockRollbackHintUsesOnlyLocalReceiptTime(t *testing.T) {
	now := time.Now().Round(0)
	for _, tc := range []struct {
		name  string
		floor time.Time
		want  bool
	}{
		{"absent", time.Time{}, false},
		{"stale", now.Add(-time.Hour), false},
		{"current", now, false},
		{"tolerance", now.Add(5 * time.Second), false},
		{"past-tolerance", now.Add(5*time.Second + time.Nanosecond), true},
		{"reported-restart", now.Add(598 * time.Second), true},
		{"corrupt-future-timestamp", time.Date(2200, 1, 1, 0, 0, 0, 0, time.UTC), true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var receipts []DirectoryReceipt
			if !tc.floor.IsZero() {
				receipts = []DirectoryReceipt{{Until: tc.floor.Add(MaxLease + 5*time.Second)}}
			}
			before := append([]DirectoryReceipt(nil), receipts...)
			hint := clockRollbackHint(receipts, now)
			if (!hint.IsZero()) != tc.want || tc.want && !hint.Equal(tc.floor) {
				t.Fatalf("query hint=%v want=%v floor=%v", hint, tc.want, tc.floor)
			}
			for i := range receipts {
				if receipts[i] != before[i] {
					t.Fatal("query hint changed a replay receipt")
				}
			}
		})
	}
}
