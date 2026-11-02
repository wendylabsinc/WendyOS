package timesync

import (
	"context"
	"crypto/ed25519"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/shared/roughtime"
)

func TestConsensusRetainsOnlyObservedVerifiedPathMetadata(t *testing.T) {
	servers := []roughtime.Server{{Name: "one", Address: "one:2002", PublicKey: make(ed25519.PublicKey, 32)}, {Name: "two", Address: "two:2002", PublicKey: make(ed25519.PublicKey, 32)}}
	var boot atomic.Int64
	boot.Store(int64(time.Second))
	c, err := queryConsensus(context.Background(), servers, func(_ context.Context, s roughtime.Server) (roughtime.Result, error) {
		return roughtime.Result{Server: s.Name, Midpoint: time.Unix(1800000000, 0), Radius: time.Second, LocalAddress: "10.88.0.1:1000", RemoteAddress: s.Address}, nil
	}, func() (int64, error) { return boot.Add(1), nil })
	if err != nil || c.Quorum != 2 {
		t.Fatal("fixture did not form quorum", c, err)
	}
	for _, ev := range c.Evidence {
		if !ev.Included || ev.LocalAddress != "10.88.0.1:1000" || ev.RemoteAddress != ev.Address {
			t.Fatal("verified socket observations dropped", ev)
		}
	}
}

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
