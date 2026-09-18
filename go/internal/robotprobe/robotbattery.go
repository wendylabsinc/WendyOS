package robotprobe

import (
	"context"
	"fmt"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/shared/robotinspect"
	"github.com/wendylabsinc/wendy/go/internal/shared/rosmsg"
)

// robotBatteryTopic is where a Unitree humanoid publishes its pack state.
const robotBatteryTopic = "/lf/bmsstate"

// RobotBattery reports the battery the robot runs on — distinct from the battery of the
// computer inside it, which the host-battery probe covers and which a G1 does not have.
//
// Three things here are not in a charge percentage and are the reason this is worth its
// own probe: the pack's health, its cycle count, and the spread between its cells. A pack
// at 78% that is 400 cycles old with 90 mV between its best and worst cell is a different
// machine from a new one at 78%, and nothing else on the robot says so.
type RobotBattery struct {
	// Topic overrides where to read, for a robot that publishes this elsewhere.
	Topic string
}

func (RobotBattery) ID() string                { return "robot-battery" }
func (RobotBattery) Class() robotinspect.Class { return robotinspect.ClassPassive }
func (RobotBattery) Requires() []robotinspect.Requirement {
	return []robotinspect.Requirement{robotinspect.RequirementDDSDomain}
}
func (RobotBattery) Provides() []string {
	return []string{"battery.robot.charge", "battery.robot.health", "battery.robot.cycles"}
}

func (p RobotBattery) Observe(ctx context.Context, env *robotinspect.Env) ([]robotinspect.Property, error) {
	reader, err := topicReader(env, p.ID())
	if err != nil {
		return nil, err
	}
	topic := p.Topic
	if topic == "" {
		topic = robotBatteryTopic
	}

	payloads, err := reader.Sample(ctx, topic, rosmsg.TypeHGBmsState, 2*time.Second, 1)
	if err != nil {
		return nil, fmt.Errorf("robot-battery: sampling %s: %w", topic, err)
	}
	if len(payloads) == 0 {
		return unknownAll(robotinspect.NewUnknown(robotinspect.ReasonSourceAbsent,
			fmt.Sprintf("nothing published %s within 2s", topic)), p.Provides()), nil
	}
	bms, err := rosmsg.DecodeHGBmsState(payloads[0])
	if err != nil {
		return unknownAll(robotinspect.NewUnknown(robotinspect.ReasonProbeFailed, err.Error()), p.Provides()), nil
	}

	source := robotinspect.Source{Probe: p.ID(), Origin: "topic:" + topic}
	level := func(id string, value float64, unit robotinspect.Unit, conditions map[string]string) robotinspect.Property {
		return instantProperty(id, value, unit, source, conditions)
	}

	properties := []robotinspect.Property{
		level("battery.robot.charge", float64(bms.ChargePercent), robotinspect.Percent, nil),
		level("battery.robot.health", float64(bms.HealthPercent), robotinspect.Percent, nil),
		level("battery.robot.cycles", float64(bms.Cycles), robotinspect.Count, nil),
		level("battery.robot.current", float64(bms.CurrentMilliamps)/1000, robotinspect.Amps, nil),
		textProperty(p.ID(), "topic:"+topic, "battery.robot.firmware",
			fmt.Sprintf("%d.%d", bms.VersionHigh, bms.VersionLow)),
	}

	// Cell spread. An unfitted slot reads zero and is excluded, so this is the spread
	// across cells that are actually there.
	highest, haveHigh := bms.HighestCellMillivolts()
	lowest, haveLow := bms.LowestCellMillivolts()
	if haveHigh && haveLow {
		properties = append(properties,
			level("battery.robot.cell.highest", float64(highest)/1000, robotinspect.Volts, nil),
			level("battery.robot.cell.lowest", float64(lowest)/1000, robotinspect.Volts, nil),
			level("battery.robot.cell.imbalance", float64(highest-lowest)/1000, robotinspect.Volts, nil))
	} else {
		unknown := robotinspect.NewUnknown(robotinspect.ReasonSourceAbsent,
			"the pack reports no cell voltages")
		properties = append(properties, robotinspect.Property{ID: "battery.robot.cell.imbalance", Unknown: &unknown})
	}

	if hottest, ok := bms.HottestCelsius(); ok {
		properties = append(properties, level("battery.robot.temperature.max", float64(hottest), robotinspect.Celsius, nil))
	} else {
		unknown := robotinspect.NewUnknown(robotinspect.ReasonSourceAbsent,
			"the pack reports no temperatures")
		properties = append(properties, robotinspect.Property{ID: "battery.robot.temperature.max", Unknown: &unknown})
	}
	return properties, nil
}
