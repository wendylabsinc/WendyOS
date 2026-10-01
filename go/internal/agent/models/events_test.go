package models

import (
	"slices"
	"testing"
)

func TestFilterMatch(t *testing.T) {
	person := Event{Type: EventEntered, Class: "person", Confidence: 0.8}
	cases := []struct {
		name   string
		filter Filter
		want   bool
	}{
		{"empty filter", Filter{}, true},
		{"class listed", Filter{Classes: []string{"car", "person"}}, true},
		{"class not listed", Filter{Classes: []string{"car"}}, false},
		{"confident enough", Filter{MinConfidence: 0.8}, true},
		{"not confident enough", Filter{MinConfidence: 0.9}, false},
		{"type listed", Filter{Types: []string{EventEntered}}, true},
		{"type not listed", Filter{Types: []string{EventLeft}}, false},
	}
	for _, tc := range cases {
		if got := tc.filter.Match(person); got != tc.want {
			t.Errorf("%s: Match = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestRingNumbersEvictsAndReportsGaps(t *testing.T) {
	r := newRing(3)
	for i := 0; i < 5; i++ {
		if e := r.append(Event{Class: "person"}); e.Sequence != uint64(i+1) {
			t.Fatalf("append %d numbered %d", i, e.Sequence)
		}
	}
	seqs := func(events []Event) []uint64 {
		var out []uint64
		for _, e := range events {
			out = append(out, e.Sequence)
		}
		return out
	}
	events, gap := r.since(1)
	if !slices.Equal(seqs(events), []uint64{3, 4, 5}) || gap == nil || *gap != (Gap{FirstMissing: 2, LastMissing: 2}) {
		t.Fatalf("since(1) = %v, %+v", seqs(events), gap)
	}
	events, gap = r.since(4)
	if !slices.Equal(seqs(events), []uint64{5}) || gap != nil {
		t.Fatalf("since(4) = %v, %+v", seqs(events), gap)
	}
	if events, gap = r.since(5); events != nil || gap != nil {
		t.Fatalf("since(last) = %v, %+v", events, gap)
	}
}
