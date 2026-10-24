package devices

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/wendylabsinc/WendyOS/go/wendy-core/platform"
)

func TestPrimaryRejectedOurCertIgnoresOtherCandidates(t *testing.T) {
	cases := []struct {
		name string
		walk mtlsWalk
		want bool
	}{
		{"nothing failed", mtlsWalk{}, false},
		{"primary's own port rejected our cert", mtlsWalk{primaryOwnPortCertReject: true}, true},
		{"every primary mTLS-port failure was a cert rejection", mtlsWalk{primaryMTLSPortCertFails: 2}, true},
		{"primary was merely unreachable", mtlsWalk{primaryMTLSPortNonCertFails: 2}, false},
		{
			"a primary that was partly unreachable is not a rejection",
			mtlsWalk{primaryMTLSPortCertFails: 1, primaryMTLSPortNonCertFails: 1},
			false,
		},
		{
			"another candidate's cert rejection does not suppress the rung",
			mtlsWalk{anyCertRejection: true},
			false,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := c.walk.primaryRejectedOurCert(); got != c.want {
				t.Fatalf("primaryRejectedOurCert() = %v, want %v", got, c.want)
			}
		})
	}
}
func TestDialAddrScopesBucketsToThePrimary(t *testing.T) {

	dead := "127.0.0.1:50051"
	newWalk := func() *mtlsWalk {
		return &mtlsWalk{
			target: Target{PinKey: "orin.local", Addr: dead},
			connector: fakeConnector(func(context.Context, platform.DeviceConnectRequest) (platform.DeviceConnection, error) {
				return failingConnection(errors.New("connection refused")), nil
			}),
			allCerts:   []Credential{{Reference: "0", OrganizationID: 7}},
			probeOrder: []int{0},
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	asPrimary := newWalk()
	if conn, refusal := asPrimary.dialAddr(ctx, dead, true); conn != nil || refusal != nil {
		t.Fatalf("dialAddr(primary) = (%v, %v), want nothing answered", conn, refusal)
	}
	notPrimary := newWalk()
	if conn, refusal := notPrimary.dialAddr(ctx, dead, false); conn != nil || refusal != nil {
		t.Fatalf("dialAddr(non-primary) = (%v, %v), want nothing answered", conn, refusal)
	}

	// The primary run accounts for its failures and records its own last error.
	if asPrimary.primaryMTLSPortCertFails+asPrimary.primaryMTLSPortNonCertFails == 0 {
		t.Error("the primary's failures were not accounted for in its own buckets")
	}
	if asPrimary.primaryLastErr == nil {
		t.Error("primaryLastErr = nil, want the primary's failure kept for chooseRejectionError")
	}
	// The non-primary run must touch none of it, while still logging the attempt
	// so the unreachable message can name the address.
	if notPrimary.primaryOwnPortCertReject ||
		notPrimary.primaryMTLSPortCertFails != 0 ||
		notPrimary.primaryMTLSPortNonCertFails != 0 ||
		notPrimary.primaryLastErr != nil {
		t.Errorf("a non-primary candidate voted on the primary's verdict: %+v", notPrimary)
	}
	if len(notPrimary.attempts) == 0 {
		t.Error("a non-primary candidate logged no attempt, so the failure could not name it")
	}
	if notPrimary.lastMTLSErr == nil {
		t.Error("lastMTLSErr = nil, want every candidate's failure kept for diagnostics")
	}
}
