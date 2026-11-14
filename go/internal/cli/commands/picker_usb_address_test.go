package commands

import (
	"slices"
	"testing"

	"github.com/wendylabsinc/wendy/go/internal/cli/tui"
	"github.com/wendylabsinc/wendy/go/internal/shared/discovery"
	"github.com/wendylabsinc/wendy/go/internal/shared/models"
)

// usbPickerSightings returns the two ways the picker hears of one USB board:
// the discovery stream, which here resolved the interface's own link-local
// address, and the USB probe, which dials the fixed USB address.
func usbPickerSightings() (stream, probe models.LANDevice, fixed string) {
	fixed = discovery.WellKnownUSBAddr + "%en7"
	stream = models.LANDevice{
		ID: "board-1", DisplayName: "Brave Dolphin", Hostname: "brave-dolphin.local",
		IPAddress: "fe80::a1b2%en7", Addresses: []string{"fe80::a1b2%en7"},
		Port: defaultAgentPort, NetworkInterface: "en7", USB: "en7",
	}
	probe = models.LANDevice{
		DisplayName: "brave-dolphin", Hostname: "brave-dolphin.local",
		IPAddress: fixed, Port: defaultAgentPort, NetworkInterface: "en7", USB: "en7",
	}
	return stream, probe, fixed
}

// A USB board answers at the fixed USB address and at its interface's own
// link-local address. The row shows the fixed one whichever sighting reaches
// the picker first.
func TestPickerUSBRowShowsTheFixedUSBAddressInEitherOrder(t *testing.T) {
	stream, probe, fixed := usbPickerSightings()
	want := hostPort(fixed, defaultAgentPort)

	for _, order := range []struct {
		name   string
		first  models.LANDevice
		second models.LANDevice
	}{
		{"stream first", stream, probe},
		{"USB probe first", probe, stream},
	} {
		t.Run(order.name, func(t *testing.T) {
			row := lanPickerItem(order.first, false, tui.ProbeOK)
			mergePickerItem(&row, lanPickerItem(order.second, false, tui.ProbeOK))
			if row.Address != want {
				t.Errorf("row address = %q, want the fixed USB address %q", row.Address, want)
			}
		})
	}
}

// The stream reports every address it has heard a device at. A row built from
// one event shows the fixed USB address when it is among them, not whichever
// address was resolved first.
func TestLANPickerItemShowsTheFixedUSBAddressAmongItsAddresses(t *testing.T) {
	stream, _, fixed := usbPickerSightings()
	stream.Addresses = []string{stream.IPAddress, fixed}

	if got, want := lanPickerItem(stream, false, tui.ProbeOK).Address, hostPort(fixed, defaultAgentPort); got != want {
		t.Errorf("row address = %q, want the fixed USB address %q", got, want)
	}
}

// Only the fixed USB address outranks another address. A board that is not on
// USB keeps the address its row first showed, and nothing replaces the fixed
// USB address once a row shows it.
func TestPickerAddressRankLeavesOtherRowsAlone(t *testing.T) {
	t.Run("a later ordinary address does not replace the first", func(t *testing.T) {
		first := models.LANDevice{ID: "pi", Hostname: "pi.local", IPAddress: "192.168.1.20", Port: defaultAgentPort}
		later := first
		later.IPAddress = "10.0.0.9"
		row := lanPickerItem(first, false, tui.ProbeOK)
		mergePickerItem(&row, lanPickerItem(later, false, tui.ProbeOK))
		if want := hostPort("192.168.1.20", defaultAgentPort); row.Address != want {
			t.Errorf("row address = %q, want the first address %q", row.Address, want)
		}
	})
	t.Run("a board on USB without the fixed address shows what it did before", func(t *testing.T) {
		dev := models.LANDevice{ID: "pi", Hostname: "pi.local", IPAddress: "192.168.1.20", Addresses: []string{"192.168.1.20", "fe80::a1b2%en7"}, Port: defaultAgentPort, USB: "en7"}
		if got, want := lanPickerItem(dev, false, tui.ProbeOK).Address, preferredLANAddress(dev); got != want {
			t.Errorf("row address = %q, want %q", got, want)
		}
	})
	t.Run("the fixed USB address is never replaced", func(t *testing.T) {
		stream, probe, fixed := usbPickerSightings()
		row := lanPickerItem(probe, false, tui.ProbeOK)
		for range 2 {
			mergePickerItem(&row, lanPickerItem(stream, false, tui.ProbeOK))
		}
		if want := hostPort(fixed, defaultAgentPort); row.Address != want {
			t.Errorf("row address = %q, want the fixed USB address %q", row.Address, want)
		}
	})
}

// The address a row shows is display only. The row keeps the device record it
// already had, so selecting it dials exactly what it dialled before.
func TestPickerAddressRankDoesNotChangeWhatIsDialled(t *testing.T) {
	stream, probe, _ := usbPickerSightings()
	row := lanPickerItem(stream, false, tui.ProbeOK)
	before := lanAgentAddresses(*row.Value.(*pickerEntry).mergedDevice.LAN)

	mergePickerItem(&row, lanPickerItem(probe, false, tui.ProbeOK))

	lan := row.Value.(*pickerEntry).mergedDevice.LAN
	if after := lanAgentAddresses(*lan); !slices.Equal(after, before) {
		t.Errorf("dial addresses = %v after the USB probe merged, want %v", after, before)
	}
	if lan.ID != stream.ID || lan.IPAddress != stream.IPAddress || !slices.Equal(lan.Addresses, stream.Addresses) {
		t.Errorf("row device = %+v after the USB probe merged, want the stream's record kept", *lan)
	}
}
