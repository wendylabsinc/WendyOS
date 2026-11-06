package framesource

import (
	"context"
	"errors"

	agentpbv2 "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
)

// UnavailableSource is a camera this device can SEE but cannot capture from —
// a RealSense whose factory calibration cannot be read, an unrecognised model
// whose depth is deliberately disabled.
//
// It is listed rather than hidden on purpose. Omitting it would tell an
// operator their camera does not exist, and the failure this whole service
// exists to prevent is precisely a missing capability that produces no error.
type UnavailableSource struct{ desc *agentpbv2.CalibratedSource }

// NewUnavailableSource describes a camera that is present but not capturable.
// reason is shown to the operator and should name the fix.
func NewUnavailableSource(name, kind, description, reason string) *UnavailableSource {
	return &UnavailableSource{desc: &agentpbv2.CalibratedSource{
		Source:            name,
		Kind:              kind,
		Description:       description,
		Available:         false,
		UnavailableReason: reason,
	}}
}

func (s *UnavailableSource) Describe(context.Context) (*agentpbv2.CalibratedSource, error) {
	return s.desc, nil
}

func (s *UnavailableSource) Open(context.Context, Options) (Stream, error) {
	return nil, errors.New(s.desc.GetUnavailableReason())
}
