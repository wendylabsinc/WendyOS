package robotprobe

import (
	"fmt"

	"github.com/wendylabsinc/wendy/go/internal/shared/robotinspect"
)

// topicReader pulls the DDS handle out of an environment, with the same error shape for
// every probe that needs one.
func topicReader(env *robotinspect.Env, probe string) (TopicReader, error) {
	handle, ok := env.Handle(robotinspect.RequirementDDSDomain)
	if !ok {
		return nil, fmt.Errorf("%s: no DDS handle in the environment", probe)
	}
	reader, ok := handle.(TopicReader)
	if !ok {
		return nil, fmt.Errorf("%s: DDS handle is %T, not a TopicReader", probe, handle)
	}
	return reader, nil
}

// unknownAll marks every promised property with one reason, so a probe that could not
// read anything still keeps every promise it made.
func unknownAll(unknown robotinspect.Unknown, ids []string) []robotinspect.Property {
	properties := make([]robotinspect.Property, 0, len(ids))
	for _, id := range ids {
		reason := unknown
		properties = append(properties, robotinspect.Property{ID: id, Unknown: &reason})
	}
	return properties
}

// instantProperty records a reading taken at a moment rather than sampled over a window
// — a charge level, a temperature, a joint angle. A rate must never use this.
func instantProperty(id string, value float64, unit robotinspect.Unit, source robotinspect.Source, conditions map[string]string, quantityOpts ...robotinspect.QuantityOption) robotinspect.Property {
	quantity, err := robotinspect.NewQuantity(value, unit, quantityOpts...)
	if err != nil {
		unknown := robotinspect.NewUnknown(robotinspect.ReasonProbeFailed, err.Error())
		return robotinspect.Property{ID: id, Unknown: &unknown}
	}
	opts := []robotinspect.ObservationOption{robotinspect.WithInstantReading()}
	if len(conditions) > 0 {
		opts = append(opts, robotinspect.WithConditions(conditions))
	}
	observation, err := robotinspect.NewObservation(quantity, robotinspect.Measured, source, opts...)
	if err != nil {
		unknown := robotinspect.NewUnknown(robotinspect.ReasonProbeFailed, err.Error())
		return robotinspect.Property{ID: id, Unknown: &unknown}
	}
	return robotinspect.Property{ID: id, Observations: []robotinspect.Observation{observation}}
}
