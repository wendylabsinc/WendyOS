package robotprobe

import (
	"context"
	"fmt"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/shared/robotinspect"
	"github.com/wendylabsinc/wendy/go/internal/shared/rosmsg"
)

// jointsTopic is where a Unitree humanoid publishes its whole body state.
const jointsTopic = "/lowstate"

// jointsWindow bounds the wait. The topic runs at hundreds of hertz, so a handful of
// messages arrive immediately or the publisher is not there.
const jointsWindow = 2 * time.Second

// jointsSamples is how many messages to read. More than one so a temperature is not a
// single reading, few enough that this stays a glance rather than a recording.
const jointsSamples = 5

// Joints reports the robot's body: every motor's position, torque and temperature, plus
// the IMU and the robot's own mode words.
//
// It decodes the wire format itself rather than asking the robot's tooling, because the
// agent's ROS 2 sidecar is a stock image without Unitree's message definitions — asking
// it for this topic fails with "the message type 'unitree_hg/msg/LowState' is invalid".
// Decoding the bytes needs no vendor software installed anywhere.
//
// What it cannot report is a joint *limit*. Nothing in Unitree's SDK or message set
// exposes one, so the figure an operator wants — the angle the safety gate refuses past
// — is not readable here or anywhere else on the robot. It has to be measured by moving
// the joint, which is a different command.
type Joints struct {
	// Topic overrides the topic to read, for a robot that publishes this elsewhere.
	Topic string
}

func (Joints) ID() string                { return "joints" }
func (Joints) Class() robotinspect.Class { return robotinspect.ClassPassive }
func (Joints) Requires() []robotinspect.Requirement {
	return []robotinspect.Requirement{robotinspect.RequirementDDSDomain}
}
func (Joints) Provides() []string {
	return []string{"joints.slots", "joints.live", "joints.temperature.max", "imu.temperature", "robot.mode_machine"}
}

func (p Joints) Observe(ctx context.Context, env *robotinspect.Env) ([]robotinspect.Property, error) {
	handle, ok := env.Handle(robotinspect.RequirementDDSDomain)
	if !ok {
		return nil, fmt.Errorf("joints: no DDS handle in the environment")
	}
	reader, ok := handle.(TopicReader)
	if !ok {
		return nil, fmt.Errorf("joints: DDS handle is %T, not a TopicReader", handle)
	}

	topic := p.Topic
	if topic == "" {
		topic = jointsTopic
	}

	payloads, err := reader.Sample(ctx, topic, rosmsg.TypeHGLowState, jointsWindow, jointsSamples)
	if err != nil {
		return nil, fmt.Errorf("joints: sampling %s: %w", topic, err)
	}
	if len(payloads) == 0 {
		// A robot that is not a Unitree humanoid simply does not publish this, which
		// is not a failure — it is the answer, and it has to be distinguishable from
		// "we did not look".
		unknown := robotinspect.NewUnknown(robotinspect.ReasonSourceAbsent,
			fmt.Sprintf("nothing published %s as %s within %s", topic, rosmsg.TypeHGLowState, jointsWindow))
		return []robotinspect.Property{
			{ID: "joints.slots", Unknown: &unknown},
			{ID: "joints.live", Unknown: &unknown},
		}, nil
	}

	states := make([]*rosmsg.HGLowState, 0, len(payloads))
	for _, payload := range payloads {
		state, err := rosmsg.DecodeHGLowState(payload)
		if err != nil {
			// A layout this decoder does not recognise is worth reporting as such
			// rather than as an absence: the topic is there and we could not read it.
			unknown := robotinspect.NewUnknown(robotinspect.ReasonProbeFailed, err.Error())
			return []robotinspect.Property{
				{ID: "joints.slots", Unknown: &unknown},
				{ID: "joints.live", Unknown: &unknown},
			}, nil
		}
		states = append(states, state)
	}

	source := robotinspect.Source{Probe: p.ID(), Origin: "topic:" + topic}
	sampling := robotinspect.WithSampling(jointsWindow, len(states))
	latest := states[len(states)-1]

	properties := []robotinspect.Property{}
	if slots, ok := declared(p.ID(), "topic:"+topic, "joints.slots",
		robotinspect.MustQuantity(float64(len(latest.Motors)), robotinspect.Count)); ok {
		properties = append(properties, slots)
	}

	live := liveMotors(latest)
	if count, ok := declared(p.ID(), "topic:"+topic, "joints.live",
		robotinspect.MustQuantity(float64(len(live)), robotinspect.Count)); ok {
		properties = append(properties, count)
	}

	// The robot's own mode words, uninterpreted. Not the execution FSM id — that is
	// larger than these fields can hold and is not published here.
	properties = append(properties,
		textProperty(p.ID(), "topic:"+topic, "robot.mode_machine", fmt.Sprintf("%d", latest.ModeMachine)),
		textProperty(p.ID(), "topic:"+topic, "robot.mode_pr", fmt.Sprintf("%d", latest.ModePR)))

	if temperature, err := robotinspect.NewQuantity(float64(latest.IMU.TemperatureC), robotinspect.Celsius); err == nil {
		if observation, err := robotinspect.NewObservation(temperature, robotinspect.Measured, source,
			robotinspect.WithInstantReading()); err == nil {
			properties = append(properties, robotinspect.Property{
				ID: "imu.temperature", Observations: []robotinspect.Observation{observation},
			})
		}
	}

	// The hottest motor is the number an operator acts on, so it gets its own row
	// rather than being buried among the per-joint ones.
	if hottest, index, ok := hottestMotor(latest, live); ok {
		quantity, err := robotinspect.NewQuantity(float64(hottest), robotinspect.Celsius)
		if err != nil {
			return properties, err
		}
		observation, err := robotinspect.NewObservation(quantity, robotinspect.Measured, source,
			robotinspect.WithInstantReading(),
			robotinspect.WithConditions(map[string]string{"joint": fmt.Sprintf("%d", index)}))
		if err != nil {
			return properties, err
		}
		properties = append(properties, robotinspect.Property{
			ID: "joints.temperature.max", Observations: []robotinspect.Observation{observation},
		})
	}

	for _, index := range live {
		properties = append(properties, p.motorProperties(latest.Motors[index], index, source, sampling)...)
	}
	return properties, nil
}

// motorProperties reports one motor. Positions are radians as the robot publishes them;
// no conversion, because a converted number invites a comparison against a spec quoted
// in the other unit.
func (p Joints) motorProperties(motor rosmsg.HGMotor, index int, source robotinspect.Source, sampling robotinspect.ObservationOption) []robotinspect.Property {
	prefix := fmt.Sprintf("joint.%d", index)
	var properties []robotinspect.Property

	add := func(id string, value float64, unit robotinspect.Unit, opts ...robotinspect.QuantityOption) {
		quantity, err := robotinspect.NewQuantity(value, unit, opts...)
		if err != nil {
			return
		}
		observation, err := robotinspect.NewObservation(quantity, robotinspect.Measured, source,
			robotinspect.WithInstantReading())
		if err != nil {
			return
		}
		properties = append(properties, robotinspect.Property{
			ID: id, Observations: []robotinspect.Observation{observation},
		})
	}

	// A joint angle needs its axis, and the axis here is the joint itself: the robot
	// publishes one number per motor and no frame, so the joint index is the axis and
	// saying otherwise would invent precision.
	add(prefix+".position", float64(motor.Position), robotinspect.Radians,
		robotinspect.WithAxis(fmt.Sprintf("joint%d", index)))
	add(prefix+".torque", float64(motor.Torque), robotinspect.NewtonMetres)
	add(prefix+".voltage", float64(motor.Voltage), robotinspect.Volts)

	// Both sensors, under their wire index. Which one is the winding and which the
	// driver board is not documented in the message, so naming them would be a guess.
	for sensor, celsius := range motor.TemperatureC {
		add(fmt.Sprintf("%s.temperature.%d", prefix, sensor), float64(celsius), robotinspect.Celsius)
	}

	// A non-zero error word is the motor reporting a fault. The bits are Unitree's, so
	// the value travels verbatim and is not decoded into a story.
	if motor.State != 0 {
		properties = append(properties, textProperty(p.ID(), source.Origin,
			prefix+".fault", fmt.Sprintf("0x%08x", motor.State)))
	}
	return properties
}

// liveMotors picks out the slots that are actually driving something.
//
// The rule itself is rosmsg's, next to the wire layout it is a fact about, because the
// agent applies the same one when it streams joints for a calibration sweep and the two
// must not drift. Both counts are reported here, so nothing is hidden by the heuristic: a
// reader can see 35 slots and 29 live.
func liveMotors(state *rosmsg.HGLowState) []int {
	return state.LiveMotors()
}

// hottestMotor returns the highest temperature any live motor reports, from either of
// its two sensors, and which joint it was.
func hottestMotor(state *rosmsg.HGLowState, live []int) (int16, int, bool) {
	var hottest int16
	index, found := 0, false
	for _, i := range live {
		for _, celsius := range state.Motors[i].TemperatureC {
			if !found || celsius > hottest {
				hottest, index, found = celsius, i, true
			}
		}
	}
	return hottest, index, found
}
