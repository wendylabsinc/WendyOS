package robotprobe

import (
	"context"
	"fmt"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/shared/robotinspect"
	"github.com/wendylabsinc/wendy/go/internal/shared/rosmsg"
)

// Hands reports each hand: its finger motors, its pressure sensors and its power rails.
//
// The hands are separate from the body on the wire and separate here, because they fail
// differently. A hand browns out under load, and it loses travel as it warms — the
// measurement campaign recorded travel falling from 97% to 36% within a session. Motor
// temperature and supply voltage are the two readings that show that happening, and
// neither is visible in the body's LowState.
type Hands struct {
	// Topics maps a hand's name to the topic carrying its state.
	Topics map[string]string
}

// DefaultHandTopics is where a G1's Dex3 hands publish.
var DefaultHandTopics = map[string]string{
	"left":  "/dex3/left/state",
	"right": "/dex3/right/state",
}

func (Hands) ID() string                { return "hands" }
func (Hands) Class() robotinspect.Class { return robotinspect.ClassPassive }
func (Hands) Requires() []robotinspect.Requirement {
	return []robotinspect.Requirement{robotinspect.RequirementDDSDomain}
}

func (p Hands) Provides() []string {
	ids := make([]string, 0, len(p.topics())*2)
	for name := range p.topics() {
		ids = append(ids, fmt.Sprintf("hand.%s.motors", name), fmt.Sprintf("hand.%s.temperature.max", name))
	}
	return ids
}

func (p Hands) topics() map[string]string {
	if len(p.Topics) > 0 {
		return p.Topics
	}
	return DefaultHandTopics
}

func (p Hands) Observe(ctx context.Context, env *robotinspect.Env) ([]robotinspect.Property, error) {
	reader, err := topicReader(env, p.ID())
	if err != nil {
		return nil, err
	}

	var properties []robotinspect.Property
	var failures error
	for name, topic := range p.topics() {
		found, err := p.observeHand(ctx, reader, name, topic)
		properties = append(properties, found...)
		if err != nil {
			failures = joinErrors(failures, err)
		}
	}
	return properties, failures
}

func (p Hands) observeHand(ctx context.Context, reader TopicReader, name, topic string) ([]robotinspect.Property, error) {
	prefix := "hand." + name
	payloads, err := reader.Sample(ctx, topic, rosmsg.TypeHGHandState, 2*time.Second, 1)
	if err != nil {
		return nil, fmt.Errorf("hands: sampling %s: %w", topic, err)
	}
	if len(payloads) == 0 {
		// A robot with no hands fitted, or hands that are not powered. Both are
		// answers, and distinguishing them is not something this probe can do.
		unknown := robotinspect.NewUnknown(robotinspect.ReasonSourceAbsent,
			fmt.Sprintf("nothing published %s within 2s", topic))
		return unknownAll(unknown, []string{prefix + ".motors", prefix + ".temperature.max"}), nil
	}
	hand, err := rosmsg.DecodeHGHandState(payloads[0])
	if err != nil {
		unknown := robotinspect.NewUnknown(robotinspect.ReasonProbeFailed, err.Error())
		return unknownAll(unknown, []string{prefix + ".motors", prefix + ".temperature.max"}), nil
	}

	source := robotinspect.Source{Probe: p.ID(), Origin: "topic:" + topic}
	properties := []robotinspect.Property{
		instantProperty(prefix+".motors", float64(len(hand.Motors)), robotinspect.Count, source, nil),
		instantProperty(prefix+".pressure_sensors", float64(len(hand.PressSensors)), robotinspect.Count, source, nil),
		// The supply rails: where a hand browning out under load shows up.
		instantProperty(prefix+".power.volts", float64(hand.PowerVolts), robotinspect.Volts, source, nil),
		instantProperty(prefix+".power.amps", float64(hand.PowerAmps), robotinspect.Amps, source, nil),
	}

	if hottest, index, ok := hand.HottestMotorC(); ok {
		properties = append(properties, instantProperty(prefix+".temperature.max",
			float64(hottest), robotinspect.Celsius, source,
			map[string]string{"motor": fmt.Sprintf("%d", index)}))
	} else {
		unknown := robotinspect.NewUnknown(robotinspect.ReasonSourceAbsent, "the hand reports no motors")
		properties = append(properties, robotinspect.Property{ID: prefix + ".temperature.max", Unknown: &unknown})
	}

	for i, motor := range hand.Motors {
		id := fmt.Sprintf("%s.motor.%d", prefix, i)
		properties = append(properties,
			instantProperty(id+".position", float64(motor.Position), robotinspect.Radians, source, nil,
				robotinspect.WithAxis(fmt.Sprintf("%s%d", name, i))),
			instantProperty(id+".torque", float64(motor.Torque), robotinspect.NewtonMetres, source, nil))
	}

	// The pads' own count of readings they know they dropped. On the G1 this reads
	// around 14,700 per hand with the control stack down, which is almost certainly a
	// lifetime tally rather than a fault: one sample cannot tell a counter that has
	// been climbing since boot from one climbing now. It is reported as the count it
	// is, and two passes apart are what would turn it into a rate.
	var lost uint32
	for _, sensor := range hand.PressSensors {
		lost += sensor.Lost
	}
	if len(hand.PressSensors) > 0 {
		properties = append(properties, instantProperty(prefix+".pressure.lost",
			float64(lost), robotinspect.Count, source, nil))
	}
	return properties, nil
}
