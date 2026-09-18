package rosmsg

import (
	"fmt"

	"github.com/wendylabsinc/wendy/go/internal/rtps/cdr"
)

// TypeHGBmsState is the DDS type name a unitree_hg/msg/BmsState writer advertises. A G1
// publishes this on /lf/bmsstate: the robot's own battery, as opposed to the battery of
// the computer inside it.
const TypeHGBmsState = "unitree_hg::msg::dds_::BmsState_"

// HGBmsCells and HGBmsTemperatures are the fixed array lengths in the message. A pack
// with fewer cells fitted still publishes the full array, so a zero entry means an
// absent cell rather than a flat one.
const (
	HGBmsCells        = 40
	HGBmsTemperatures = 12
)

// HGBmsState is the robot's battery.
type HGBmsState struct {
	// VersionHigh and VersionLow are the pack firmware, which is readable here and
	// nowhere else without the vendor SDK.
	VersionHigh uint8
	VersionLow  uint8

	// ChargePercent and HealthPercent are soc and soh on the wire.
	ChargePercent uint8
	HealthPercent uint8
	// CurrentMilliamps is signed: negative while discharging.
	CurrentMilliamps int32
	// CellMillivolts has one entry per cell slot; a zero is an absent cell.
	CellMillivolts [HGBmsCells]uint16
	// TemperaturesC has one entry per sensor slot, in degrees.
	TemperaturesC [HGBmsTemperatures]int16
	// Cycles is how many charge cycles the pack has been through — the number that
	// says how worn it is, and one nothing else on the robot reports.
	Cycles uint16
	// ManufactureDate is the vendor's packed date word, carried verbatim because its
	// encoding is theirs.
	ManufactureDate uint16
	// State is the pack's status words, uninterpreted for the same reason.
	State [5]uint32
}

// HighestCellMillivolts, LowestCellMillivolts and CellImbalanceMillivolts describe the
// spread across fitted cells. Imbalance is what says a pack is ageing unevenly, and it
// is invisible in a charge percentage.
func (b HGBmsState) HighestCellMillivolts() (uint16, bool) {
	var highest uint16
	for _, mv := range b.CellMillivolts {
		if mv > highest {
			highest = mv
		}
	}
	return highest, highest > 0
}

func (b HGBmsState) LowestCellMillivolts() (uint16, bool) {
	var lowest uint16
	for _, mv := range b.CellMillivolts {
		if mv == 0 {
			continue // an unfitted slot, not a dead cell
		}
		if lowest == 0 || mv < lowest {
			lowest = mv
		}
	}
	return lowest, lowest > 0
}

// HottestCelsius returns the highest reading among fitted sensors.
func (b HGBmsState) HottestCelsius() (int16, bool) {
	var hottest int16
	found := false
	for _, celsius := range b.TemperaturesC {
		if celsius == 0 {
			continue
		}
		if !found || celsius > hottest {
			hottest, found = celsius, true
		}
	}
	return hottest, found
}

// DecodeHGBmsState decodes a unitree_hg/msg/BmsState payload. Like the LowState decoder
// it walks every field, including those nothing reads, and requires the payload to end
// exactly — a layout assumption that is wrong then fails here rather than producing a
// believable charge percentage.
func DecodeHGBmsState(payload []byte) (*HGBmsState, error) {
	d, err := cdr.NewDecoder(payload)
	if err != nil {
		return nil, err
	}

	var bms HGBmsState
	read := func(name string, fn func() error) error {
		if err := fn(); err != nil {
			return fmt.Errorf("%s: %w", name, err)
		}
		return nil
	}

	if err := read("version_high", func() (err error) { bms.VersionHigh, err = d.Uint8(); return }); err != nil {
		return nil, err
	}
	if err := read("version_low", func() (err error) { bms.VersionLow, err = d.Uint8(); return }); err != nil {
		return nil, err
	}
	if err := read("fn", func() error { _, err := d.Uint8(); return err }); err != nil {
		return nil, err
	}
	for i := range bms.CellMillivolts {
		if err := read(fmt.Sprintf("cell_vol[%d]", i), func() (err error) {
			bms.CellMillivolts[i], err = d.Uint16()
			return
		}); err != nil {
			return nil, err
		}
	}
	for i := 0; i < 3; i++ {
		if err := read(fmt.Sprintf("bmsvoltage[%d]", i), func() error { _, err := d.Uint32(); return err }); err != nil {
			return nil, err
		}
	}
	if err := read("current", func() (err error) { bms.CurrentMilliamps, err = d.Int32(); return }); err != nil {
		return nil, err
	}
	if err := read("soc", func() (err error) { bms.ChargePercent, err = d.Uint8(); return }); err != nil {
		return nil, err
	}
	if err := read("soh", func() (err error) { bms.HealthPercent, err = d.Uint8(); return }); err != nil {
		return nil, err
	}
	for i := range bms.TemperaturesC {
		if err := read(fmt.Sprintf("temperature[%d]", i), func() (err error) {
			bms.TemperaturesC[i], err = d.Int16()
			return
		}); err != nil {
			return nil, err
		}
	}
	if err := read("cycle", func() (err error) { bms.Cycles, err = d.Uint16(); return }); err != nil {
		return nil, err
	}
	if err := read("manufacturer_date", func() (err error) { bms.ManufactureDate, err = d.Uint16(); return }); err != nil {
		return nil, err
	}
	for i := range bms.State {
		if err := read(fmt.Sprintf("bmsstate[%d]", i), func() (err error) { bms.State[i], err = d.Uint32(); return }); err != nil {
			return nil, err
		}
	}
	for i := 0; i < 3; i++ {
		if err := read(fmt.Sprintf("reserve[%d]", i), func() error { _, err := d.Uint32(); return err }); err != nil {
			return nil, err
		}
	}
	if remaining := d.Remaining(); remaining != 0 {
		return nil, fmt.Errorf("rosmsg: unitree_hg BmsState left %d bytes unread; the wire layout is not what this decoder expects", remaining)
	}
	return &bms, nil
}
