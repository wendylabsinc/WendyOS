package replication

import (
	"time"

	"github.com/wendylabsinc/wendy/go/world/internal/fusion"
	"github.com/wendylabsinc/wendy/go/world/production/fusioncapture"
	"github.com/wendylabsinc/wendy/go/world/production/resultrecord"
	"github.com/wendylabsinc/wendy/go/world/production/sensorrecord"
	"github.com/wendylabsinc/wendy/go/world/production/serviceorigin"
	"github.com/wendylabsinc/wendy/go/world/production/trackcapture"
)

// SupportedComponent is a transport selector check, not payload admission.
// Generic result envelopes are admitted only for the strict person-track and
// fusion-hazard schemas. Typed capture envelopes may reference camera/audio
// media; the media bytes are never transferred through this record path.
func SupportedComponent(component string) bool {
	return component == ScalarComponent || component == resultrecord.ComponentType || component == sensorrecord.ComponentType
}

// RecordInterpretationUntil validates the closed remote payload vocabulary.
// A nonzero result is the original physical interpretation deadline, not a
// receipt-time TTL. Expired originals may still repair durable version floors;
// callers must apply this cap before exposing a live value. The origin's camera
// signatures, media and inference are not independently reexecuted here.
func RecordInterpretationUntil(record serviceorigin.Record) (time.Time, error) {
	p := record.Publication
	if !SupportedComponent(p.ComponentType) {
		return time.Time{}, ErrProtocol
	}
	if p.Tombstone {
		if len(p.Payload) != 0 {
			return time.Time{}, ErrProtocol
		}
		return time.Time{}, nil
	}
	if p.ComponentType == ScalarComponent {
		return time.Time{}, nil // Scalar schema admission stays in the host store.
	}
	if p.ComponentType == sensorrecord.ComponentType {
		component := (serviceorigin.VerifiedRecord{Record: record}).Component()
		capture, err := sensorrecord.FromComponent(component)
		if err != nil || record.Scope.ExecutionDomain != "simulation" {
			return time.Time{}, ErrProtocol
		}
		sample, err := capture.Sample()
		if err != nil || sample.Kind != "gas" && sample.Kind != "lidar" && sample.Kind != "camera" && sample.Kind != "audio" {
			return time.Time{}, ErrProtocol
		}
		// Evidence selects the original media reference, including the native
		// JPEG for SensorLink cameras. Validation does not fetch those bytes
		// or claim that a receiver has decoded them.
		if _, err := capture.Evidence(); err != nil {
			return time.Time{}, ErrProtocol
		}
		_, until, err := sensorrecord.LiveDeadlines(component)
		if err != nil {
			return time.Time{}, ErrProtocol
		}
		return until, nil
	}
	result, err := resultrecord.Decode(p.Payload)
	if err != nil {
		return time.Time{}, ErrProtocol
	}
	var until time.Time
	switch result.ResultSchema {
	case trackcapture.ResultSchema:
		track, err := trackcapture.ValidatePublication(p)
		if err != nil {
			return time.Time{}, ErrProtocol
		}
		until = track.PhysicalValidUntil()
	case fusion.HazardType:
		hazard, err := fusioncapture.ValidateReplicaPublication(p)
		if err != nil {
			return time.Time{}, ErrProtocol
		}
		until = hazard.PhysicalValidUntil
	default:
		return time.Time{}, ErrProtocol
	}
	// Each typed validator binds its members to this exact ordered generic
	// reference set. Hazard fusion may use raw observations from other devices
	// in the same tenant/site; the origin's local admission verifies those exact
	// inputs. A remote receiver still consumes the signer's attested inference.
	// Person-track lineage retains its existing own-device restriction.
	for _, input := range result.Inputs {
		key := input.Key
		if key.Tenant != record.Scope.Tenant || key.Site != record.Scope.Site || result.ResultSchema != fusion.HazardType && key.Source.Device != record.Scope.Device {
			return time.Time{}, ErrProtocol
		}
	}
	return until, nil
}
