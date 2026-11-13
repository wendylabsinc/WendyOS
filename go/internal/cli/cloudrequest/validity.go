package cloudrequest

import (
	"fmt"
	"time"

	cloudpbv2 "github.com/wendylabsinc/wendy/go/proto/gen/cloudpb/v2"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// Instruction validity (WDY-4015, sem 2026-10-23 22:27): a postbox-relayed
// instruction carries valid_until (DeviceAssignment: expires_at) under the
// operator's signature, because a device may be offline for weeks. The device
// refuses one without it and caps it at 365 days; cloud caps it at the
// tenant's setting. It is not signatureTTL, which only covers the call to cloud.
const (
	DefaultValidity = 60 * 24 * time.Hour
	MaxValidity     = 365 * 24 * time.Hour
)

// ValidUntil is now + validFor, or now + DefaultValidity when validFor is 0.
// A negative validFor, or one above MaxValidity, is refused.
func ValidUntil(now time.Time, validFor time.Duration) (time.Time, error) {
	if validFor == 0 {
		validFor = DefaultValidity
	}
	if validFor < 0 || validFor > MaxValidity {
		return time.Time{}, fmt.Errorf("instruction validity %s is outside 0 to %s (365 days)", validFor, MaxValidity)
	}
	return now.Add(validFor), nil
}

// stampValidity gives a postbox-relayed request without a validity the
// default one, and refuses one valid beyond MaxValidity from now. Other
// requests are left alone.
func stampValidity(req proto.Message, now time.Time) error {
	var field **timestamppb.Timestamp
	switch r := req.(type) {
	case *cloudpbv2.CreateDeploymentRequest:
		field = &r.ValidUntil
	case *cloudpbv2.ControlContainerRequest:
		field = &r.ValidUntil
	case *cloudpbv2.UnassignDeviceRequest:
		field = &r.ValidUntil
	case *cloudpbv2.DeviceAssignment:
		field = &r.ExpiresAt
	default:
		return nil
	}
	if *field == nil {
		until, _ := ValidUntil(now, 0)
		*field = timestamppb.New(until)
		return nil
	}
	if (*field).AsTime().After(now.Add(MaxValidity)) {
		return fmt.Errorf("instruction validity ends %s, more than 365 days from now", (*field).AsTime().UTC().Format(time.RFC3339))
	}
	return nil
}
