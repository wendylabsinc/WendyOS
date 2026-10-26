package cloudrequest

import (
	"testing"
	"time"

	cloudpbv2 "github.com/wendylabsinc/wendy/go/proto/gen/cloudpb/v2"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func TestValidUntil(t *testing.T) {
	now := time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		validFor time.Duration
		want     time.Time
		wantErr  bool
	}{
		{0, now.Add(60 * 24 * time.Hour), false},
		{7 * 24 * time.Hour, now.Add(7 * 24 * time.Hour), false},
		{365 * 24 * time.Hour, now.Add(365 * 24 * time.Hour), false},
		{365*24*time.Hour + time.Second, time.Time{}, true},
		{-time.Second, time.Time{}, true},
	} {
		got, err := ValidUntil(now, tc.validFor)
		if (err != nil) != tc.wantErr || !got.Equal(tc.want) {
			t.Errorf("ValidUntil(%s) = %s, %v; want %s, error %v", tc.validFor, got, err, tc.want, tc.wantErr)
		}
	}
}

func TestStampValidity(t *testing.T) {
	now := time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC)
	def := now.Add(DefaultValidity)
	set := now.Add(time.Hour)

	assignment := &cloudpbv2.DeviceAssignment{}
	deployment := &cloudpbv2.CreateDeploymentRequest{}
	control := &cloudpbv2.ControlContainerRequest{}
	unassign := &cloudpbv2.UnassignDeviceRequest{ValidUntil: timestamppb.New(set)}
	for name, tc := range map[string]struct {
		stamp func() error
		got   func() *timestamppb.Timestamp
		want  time.Time
	}{
		"DeviceAssignment expires_at defaults": {func() error { return stampValidity(assignment, now) }, assignment.GetExpiresAt, def},
		"CreateDeployment defaults":            {func() error { return stampValidity(deployment, now) }, deployment.GetValidUntil, def},
		"ControlContainer defaults":            {func() error { return stampValidity(control, now) }, control.GetValidUntil, def},
		"a set validity is kept":               {func() error { return stampValidity(unassign, now) }, unassign.GetValidUntil, set},
	} {
		if err := tc.stamp(); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if got := tc.got().AsTime(); !got.Equal(tc.want) {
			t.Errorf("%s: validity %s, want %s", name, got, tc.want)
		}
	}

	far := &cloudpbv2.DeviceAssignment{ExpiresAt: timestamppb.New(now.Add(MaxValidity + time.Second))}
	if err := stampValidity(far, now); err == nil {
		t.Error("a validity beyond 365 days was accepted")
	}
	if err := stampValidity(&cloudpbv2.EnrollDeviceRequest{}, now); err != nil {
		t.Errorf("a request that is not relayed to devices: %v", err)
	}
}
