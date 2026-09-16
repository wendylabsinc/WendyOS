package timesync

import "testing"

func TestLargestIntersectionExcludesOutlier(t *testing.T) {
	evidence := []RoughtimeEvidence{
		{Server: "a", LowerOffsetNanos: 100, UpperOffsetNanos: 200},
		{Server: "b", LowerOffsetNanos: 120, UpperOffsetNanos: 220},
		{Server: "c", LowerOffsetNanos: 110, UpperOffsetNanos: 180},
		{Server: "outlier", LowerOffsetNanos: 900, UpperOffsetNanos: 950},
	}
	idx, lo, hi := largestIntersection(evidence)
	if len(idx) != 3 || lo != 120 || hi != 180 {
		t.Fatalf("intersection = idx=%v [%d,%d], want quorum 3 [120,180]", idx, lo, hi)
	}
}

func TestLargestIntersectionTwoIsDegradedBasis(t *testing.T) {
	evidence := []RoughtimeEvidence{
		{LowerOffsetNanos: 1, UpperOffsetNanos: 5},
		{LowerOffsetNanos: 4, UpperOffsetNanos: 8},
		{LowerOffsetNanos: 20, UpperOffsetNanos: 30},
	}
	idx, lo, hi := largestIntersection(evidence)
	if len(idx) != 2 || lo != 4 || hi != 5 {
		t.Fatalf("got %v [%d,%d]", idx, lo, hi)
	}
}

func TestLargestIntersectionIgnoresInvalidEvidence(t *testing.T) {
	evidence := []RoughtimeEvidence{{LowerOffsetNanos: 1, UpperOffsetNanos: 10}, {LowerOffsetNanos: 2, UpperOffsetNanos: 9, Error: "bad signature"}}
	idx, _, _ := largestIntersection(evidence)
	if len(idx) != 1 || idx[0] != 0 {
		t.Fatalf("got %v", idx)
	}
}
