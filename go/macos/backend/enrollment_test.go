package backend

import (
	"errors"
	"testing"

	"github.com/wendylabsinc/wendy/go/proto/gen/litepb"
)

type enrollmentLite struct {
	liteConnection
	state *litepb.WendyComEnrollmentChallenge
	err   error
	calls int
}

func (f *enrollmentLite) EnrollmentChallenge(statusOnly bool) (*litepb.WendyComEnrollmentChallenge, error) {
	if !statusOnly {
		panic("status lookup mutated Lite enrollment")
	}
	f.calls++
	return f.state, f.err
}
func TestDesktopEnrollmentSessionAndLiteState(t *testing.T) {
	for _, tc := range []struct {
		name                string
		state               *litepb.WendyComEnrollmentChallenge
		err                 error
		enrolled, wantError bool
	}{
		{name: "unenrolled", state: &litepb.WendyComEnrollmentChallenge{}},
		{name: "enrolled", state: &litepb.WendyComEnrollmentChallenge{Enrolled: true, TenantId: "tenant"}, enrolled: true},
		{name: "pending", state: &litepb.WendyComEnrollmentChallenge{DeviceId: "reserved-device"}, wantError: true},
		{name: "missing", wantError: true},
		{name: "unreachable", err: errors.New("offline"), wantError: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			lite := &enrollmentLite{state: tc.state, err: tc.err}
			b := New()
			b.session = 42
			b.lite = lite
			for _, session := range []uint64{0, 41} {
				if _, err := b.DeviceEnrollment(session); err == nil {
					t.Fatal("accepted invalid session")
				}
				if err := b.EnrollDevice(session, "account", "tenant", "device"); err == nil {
					t.Fatal("enrolled invalid session")
				}
			}
			if lite.calls != 0 {
				t.Fatal("invalid session reached device")
			}
			value, err := b.DeviceEnrollment(42)
			if (err != nil) != tc.wantError {
				t.Fatalf("status = %+v, %v", value, err)
			}
			if err == nil && value.Enrolled != tc.enrolled {
				t.Fatalf("status = %+v", value)
			}
		})
	}
}
