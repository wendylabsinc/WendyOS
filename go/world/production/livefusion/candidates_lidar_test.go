package livefusion

import (
	"fmt"
	"reflect"
	"testing"
	"time"
)

// The exhaustive reference is deliberately small; production selection must
// support eight sources without enumerating the 8^8 retained combinations.
func exhaustiveTuple(w *candidateWindow) []candidate {
	var best []candidate
	var bestStart time.Time
	tuple := make([]candidate, len(w.order))
	var visit func(int)
	visit = func(at int) {
		if at < len(tuple) {
			for _, c := range w.state[w.order[at]].records {
				tuple[at] = c
				visit(at + 1)
			}
			return
		}
		var key [16]string
		lo, hi := tuple[0].start, tuple[0].end
		for i, c := range tuple {
			key[i] = c.hash
			if c.start.Before(lo) {
				lo = c.start
			}
			if c.end.After(hi) {
				hi = c.end
			}
		}
		if hi.Sub(lo) > w.skew {
			return
		}
		if _, retired := w.retired[key]; retired {
			return
		}
		better := best == nil || lo.After(bestStart)
		if !better && lo.Equal(bestStart) {
			for i, c := range tuple {
				if c.sequence != best[i].sequence {
					better = c.sequence > best[i].sequence
					break
				}
			}
		}
		if better {
			best = append([]candidate(nil), tuple...)
			bestStart = lo
		}
	}
	visit(0)
	return best
}

func TestCandidateWindowSelectionMatchesExhaustiveOrderWithRetirement(t *testing.T) {
	for variant := 0; variant < 24; variant++ {
		w := &candidateWindow{state: map[string]candidateSource{}, retired: map[[16]string]struct{}{}, skew: 350 * time.Millisecond}
		for i := 0; i < 4; i++ {
			id := fmt.Sprint(i)
			w.order = append(w.order, id)
			var records []candidate
			for j := 0; j < 3; j++ {
				start := time.Unix(1, 0).Add(time.Duration((variant*(i+1)+j*5+i*3)%11) * 50 * time.Millisecond)
				span := time.Duration(0)
				if i == 0 {
					span = 250 * time.Millisecond
				}
				records = append(records, candidate{start: start, end: start.Add(span), sequence: uint64(j + 1), hash: fmt.Sprintf("%d/%d", i, j)})
			}
			w.state[id] = candidateSource{records: records}
		}
		for retired := 0; retired < 16; retired++ {
			got, want := w.selectTuple(), exhaustiveTuple(w)
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("variant%d retirement%d: got%+v want%+v", variant, retired, got, want)
			}
			if got == nil {
				break
			}
			var key [16]string
			for i, c := range got {
				key[i] = c.hash
			}
			w.retired[key] = struct{}{}
		}
	}
}

func TestCandidateWindowEightSourcesFullHistoryAndRetirement(t *testing.T) {
	w := &candidateWindow{state: map[string]candidateSource{}, retired: map[[16]string]struct{}{}, skew: 350 * time.Millisecond}
	for i := 0; i < 8; i++ {
		id := fmt.Sprint(i)
		w.order = append(w.order, id)
		var records []candidate
		for j := 0; j < 8; j++ {
			start := time.Unix(1, 0).Add(time.Duration(j) * time.Millisecond)
			records = append(records, candidate{start: start, end: start.Add(250 * time.Millisecond), sequence: uint64(j + 1), hash: fmt.Sprintf("%d/%d", i, j)})
		}
		w.state[id] = candidateSource{records: records}
	}
	for n := 0; n < 17; n++ {
		selected := w.selectTuple()
		if len(selected) != 8 {
			t.Fatal("lost complete configured set")
		}
		var key [16]string
		for i, c := range selected {
			key[i] = c.hash
		}
		if _, exists := w.retired[key]; exists {
			t.Fatal("reselected evaluated tuple")
		}
		if n < 16 {
			w.retired[key] = struct{}{}
		}
	}
}
