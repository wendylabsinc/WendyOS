package livefusion

import (
	"context"
	"fmt"
	"testing"
)

func TestEveryWholeSetAuthorityBoundaryRefusesChangeWithoutPublication(t *testing.T) {
	// Query 1 acquires candidates. Queries 2/3 verify before inference, 4/5
	// reverify after media. That final batch also supplies result inputs.
	for query := 2; query <= 5; query++ {
		for _, fault := range []string{"replacement", "missing", "reader-replaced", "rpc", "cursor"} {
			t.Run(fmt.Sprintf("query-%d/%s", query, fault), func(t *testing.T) {
				s, api := setup(t, "complete")
				s.faultQuery, s.faultKind = query, fault
				s.cfg.MaxAttempts = 3
				out, err := Run(context.Background(), s.cfg, api)
				if err == nil || out.Status != "failed" || s.published != 0 || out.LocallyVerified || out.Attempts != 1 {
					t.Fatalf("authority fault retried or published: query=%d actual=%d status=%s attempts=%d err=%v", query, s.queries, out.Status, out.Attempts, err)
				}
				wantQueries := query
				// Signing authority remains valid after capture expiry. The common
				// capture-age guard follows the complete two-query proof round.
				if fault == "expiry" && query%2 == 0 {
					wantQueries++
				}
				if s.queries != wantQueries {
					t.Fatalf("missing or extra authority round: got %d want %d", s.queries, wantQueries)
				}
				if query >= 4 && len(out.Media) != 3 {
					t.Fatal("after-media boundary was not exercised")
				}
			})
		}
	}
}
