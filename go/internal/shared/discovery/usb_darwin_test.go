//go:build darwin

package discovery

import (
	"reflect"
	"testing"

	"github.com/wendylabsinc/wendy/go/internal/shared/models"
)

// macOS 26 answers SPUSBDataType with an empty list and reports USB devices
// under SPUSBHostDataType, with different key names. This is that output for
// one WendyOS board on a built-in port, one behind a hub, and an ESP32 whose
// name also says Wendy.
const systemProfilerUSBHostJSON = `{
  "SPUSBDataType" : [

  ],
  "SPUSBHostDataType" : [
    {
      "_name" : "USB 3.1 Bus",
      "Driver" : "AppleT8132USBXHCI",
      "USBKeyHardwareType" : "Built-in",
      "USBKeyLocationID" : "0x02000000"
    },
    {
      "_items" : [
        {
          "_name" : "WendyOS Device brave-otter",
          "USBDeviceKeyLinkSpeed" : "5 Gb/s",
          "USBDeviceKeyProductID" : "0x0104",
          "USBDeviceKeyProductVersion" : "0x0100",
          "USBDeviceKeySerialNumber" : "1420000000001",
          "USBDeviceKeyVendorID" : "0x1d6b",
          "USBDeviceKeyVendorName" : "Wendy Labs Inc",
          "USBKeyHardwareType" : "Removable",
          "USBKeyLocationID" : "0x00200000"
        },
        {
          "_items" : [
            {
              "_name" : "WendyOS Device calm-heron",
              "USBDeviceKeyLinkSpeed" : "480 Mb/s",
              "USBDeviceKeyProductID" : "0x0104",
              "USBDeviceKeySerialNumber" : "1420000000002",
              "USBDeviceKeyVendorID" : "0x1d6b",
              "USBDeviceKeyVendorName" : "Wendy Labs Inc"
            },
            {
              "_name" : "Wendy Lite",
              "USBDeviceKeyLinkSpeed" : "12 Mb/s",
              "USBDeviceKeyProductID" : "0x1001",
              "USBDeviceKeyVendorID" : "0x303a",
              "USBDeviceKeyVendorName" : "Espressif"
            }
          ],
          "_name" : "USB3.1 Hub",
          "USBDeviceKeyLinkSpeed" : "5 Gb/s",
          "USBDeviceKeyProductID" : "0x0626",
          "USBDeviceKeyVendorID" : "0x05e3"
        }
      ],
      "_name" : "USB 3.1 Bus",
      "Driver" : "AppleT8132USBXHCI",
      "USBKeyHardwareType" : "Built-in",
      "USBKeyLocationID" : "0x00000000"
    }
  ]
}`

// The report macOS 15 and earlier print for the same board.
const systemProfilerUSBLegacyJSON = `{
  "SPUSBDataType" : [
    {
      "_items" : [
        {
          "_name" : "WendyOS Device brave-otter",
          "bus_power" : "900",
          "bus_power_used" : "500",
          "device_speed" : "Up to 5 Gb/s",
          "manufacturer" : "Wendy Labs Inc",
          "product_id" : "0x0104",
          "serial_num" : "1420000000001",
          "vendor_id" : "0x1d6b  (Linux Foundation)"
        }
      ],
      "_name" : "USB31Bus",
      "host_controller" : "AppleT8112USBXHCI"
    }
  ]
}`

func TestParseSystemProfilerUSBReadsTheMacOS26Report(t *testing.T) {
	got, err := parseSystemProfilerUSB([]byte(systemProfilerUSBHostJSON))
	if err != nil {
		t.Fatal(err)
	}
	want := []models.USBDevice{
		{
			Name:          "WendyOS Device brave-otter",
			DisplayName:   "WendyOS Device brave-otter",
			SerialNumber:  "1420000000001",
			VendorID:      "0x1d6b",
			ProductID:     "0x0104",
			USBVersion:    "USB 3.0",
			IsWendyDevice: true,
		},
		{
			Name:          "WendyOS Device calm-heron",
			DisplayName:   "WendyOS Device calm-heron",
			SerialNumber:  "1420000000002",
			VendorID:      "0x1d6b",
			ProductID:     "0x0104",
			USBVersion:    "USB 2.0",
			IsWendyDevice: true,
		},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("devices = %+v\nwant      %+v", got, want)
	}
}

func TestParseSystemProfilerUSBStillReadsTheOlderReport(t *testing.T) {
	got, err := parseSystemProfilerUSB([]byte(systemProfilerUSBLegacyJSON))
	if err != nil {
		t.Fatal(err)
	}
	want := []models.USBDevice{{
		Name:              "WendyOS Device brave-otter",
		DisplayName:       "WendyOS Device brave-otter",
		SerialNumber:      "1420000000001",
		VendorID:          "0x1d6b",
		ProductID:         "0x0104",
		USBVersion:        "USB 3.0",
		MaxPowerMilliamps: 500,
		IsWendyDevice:     true,
	}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("devices = %+v\nwant      %+v", got, want)
	}
}

// A release that answers both reports must not list a board twice.
func TestParseSystemProfilerUSBListsABoardOnceWhenBothReportsHaveIt(t *testing.T) {
	both := `{
  "SPUSBDataType" : [
    {
      "_items" : [
        {
          "_name" : "WendyOS Device brave-otter",
          "device_speed" : "Up to 5 Gb/s",
          "product_id" : "0x0104",
          "serial_num" : "1420000000001",
          "vendor_id" : "0x1d6b"
        }
      ],
      "_name" : "USB31Bus"
    }
  ],
  "SPUSBHostDataType" : [
    {
      "_items" : [
        {
          "_name" : "WendyOS Device brave-otter",
          "USBDeviceKeyLinkSpeed" : "5 Gb/s",
          "USBDeviceKeyProductID" : "0x0104",
          "USBDeviceKeySerialNumber" : "1420000000001",
          "USBDeviceKeyVendorID" : "0x1d6b"
        }
      ],
      "_name" : "USB 3.1 Bus"
    }
  ]
}`
	got, err := parseSystemProfilerUSB([]byte(both))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d devices, want 1: %+v", len(got), got)
	}
}

func TestSetUSBNetworkInterfacesJoinsADeviceToItsHardwarePort(t *testing.T) {
	ports := []darwinHardwarePort{
		{displayName: "Wi-Fi", bsdName: "en0"},
		{displayName: "WendyOS Device brave-otter", bsdName: "en11"},
		{displayName: "WendyOS Device twin", bsdName: "en12"},
		{displayName: "WendyOS Device twin", bsdName: "en13"},
		{displayName: "WendyOS Device shared", bsdName: "en14"},
	}
	devices := []models.USBDevice{
		{Name: "WendyOS Device brave-otter"},
		{Name: "WendyOS Device twin"},    // two ports carry this name
		{Name: "WendyOS Device shared"},  // two devices carry this name
		{Name: "WendyOS Device shared"},  //
		{Name: "WendyOS Device no-port"}, // no network port at all
	}

	setUSBNetworkInterfaces(devices, ports)

	want := []string{"en11", "", "", "", ""}
	for i, dev := range devices {
		if dev.NetworkInterface != want[i] {
			t.Errorf("%s: NetworkInterface = %q, want %q", dev.Name, dev.NetworkInterface, want[i])
		}
	}
}

// checkUSBNetworkInterfaces joins devices to ports and compares the interface
// each device ends up with, in order, against want.
func checkUSBNetworkInterfaces(t *testing.T, devices []models.USBDevice, ports []darwinHardwarePort, want []string) {
	t.Helper()
	setUSBNetworkInterfaces(devices, ports)
	for i, dev := range devices {
		if dev.NetworkInterface != want[i] {
			t.Errorf("%s: NetworkInterface = %q, want %q", dev.Name, dev.NetworkInterface, want[i])
		}
	}
}

// macOS names the network port after the USB product string but keeps only its
// first 31 characters. A board with the product string
// "WendyOS warehouse-robot-station-01" showed up on macOS 26.6.2 as the port
// "WendyOS warehouse-robot-station".
func TestSetUSBNetworkInterfacesJoinsAPortNameMacOSCut(t *testing.T) {
	ports := []darwinHardwarePort{
		{displayName: "Wi-Fi", bsdName: "en0"},
		{displayName: "WendyOS warehouse-robot-station", bsdName: "en11"},
		{displayName: "WendyOS Device hackathon-device", bsdName: "en12"},
		// The cut is counted in characters, so it never splits one that takes
		// several bytes. Product strings are ASCII in practice.
		{displayName: "WendyOS Device warehouse-roboté", bsdName: "en13"},
	}
	devices := []models.USBDevice{
		{Name: "WendyOS warehouse-robot-station-01"},
		{Name: "WendyOS Device hackathon-device-5"},
		{Name: "WendyOS Device warehouse-roboté-01"},
	}

	checkUSBNetworkInterfaces(t, devices, ports, []string{"en11", "en12", "en13"})
}

// A cut name that several boards or several ports share proves nothing, just
// like a whole name. Two boards whose names are equal after the cut cannot be
// told apart by name, whether both of their ports are up or only one is.
func TestSetUSBNetworkInterfacesLeavesASharedCutNameUnjoined(t *testing.T) {
	twoBoards := func() []models.USBDevice {
		return []models.USBDevice{
			{Name: "WendyOS warehouse-robot-station-01"},
			{Name: "WendyOS warehouse-robot-station-02"},
		}
	}
	twoPorts := []darwinHardwarePort{
		{displayName: "WendyOS warehouse-robot-station", bsdName: "en11"},
		{displayName: "WendyOS warehouse-robot-station", bsdName: "en12"},
	}

	t.Run("two boards, two ports", func(t *testing.T) {
		checkUSBNetworkInterfaces(t, twoBoards(), twoPorts, []string{"", ""})
	})
	t.Run("two boards, one port", func(t *testing.T) {
		checkUSBNetworkInterfaces(t, twoBoards(), twoPorts[:1], []string{"", ""})
	})
	t.Run("one board, two ports", func(t *testing.T) {
		checkUSBNetworkInterfaces(t, twoBoards()[:1], twoPorts, []string{""})
	})
}

// Only a port name of exactly 31 characters can be a cut name. Any other port
// name still has to equal the device's name.
func TestSetUSBNetworkInterfacesNeedsEqualNamesBelowTheMacOSCut(t *testing.T) {
	ports := []darwinHardwarePort{
		{displayName: "WendyOS a", bsdName: "en11"},
		{displayName: "WendyOS warehouse-robot-statio", bsdName: "en12"},   // 30 characters
		{displayName: "WendyOS Device hackathon-device-", bsdName: "en13"}, // 32 characters
	}
	devices := []models.USBDevice{
		{Name: "WendyOS ab"},
		{Name: "WendyOS warehouse-robot-station-01"},
		{Name: "WendyOS Device hackathon-device-5"},
	}

	checkUSBNetworkInterfaces(t, devices, ports, []string{"", "", ""})
}

// A device whose whole name is on a port is joined by that name alone, as
// before: the cut name is tried only when no port carries the whole name, and
// a port goes to the device that has its exact name.
func TestSetUSBNetworkInterfacesPrefersAnExactNameOverACutOne(t *testing.T) {
	t.Run("a port with the whole name and one with the cut name", func(t *testing.T) {
		ports := []darwinHardwarePort{
			{displayName: "WendyOS warehouse-robot-station", bsdName: "en11"},
			{displayName: "WendyOS warehouse-robot-station-01", bsdName: "en12"},
		}
		devices := []models.USBDevice{{Name: "WendyOS warehouse-robot-station-01"}}
		checkUSBNetworkInterfaces(t, devices, ports, []string{"en12"})
	})
	t.Run("a device with the port's name and one cut to it", func(t *testing.T) {
		ports := []darwinHardwarePort{
			{displayName: "WendyOS warehouse-robot-station", bsdName: "en11"},
		}
		devices := []models.USBDevice{
			{Name: "WendyOS warehouse-robot-station-01"},
			{Name: "WendyOS warehouse-robot-station"},
		}
		checkUSBNetworkInterfaces(t, devices, ports, []string{"", "en11"})
	})
	t.Run("two ports with the whole name and one with the cut name", func(t *testing.T) {
		ports := []darwinHardwarePort{
			{displayName: "WendyOS warehouse-robot-station", bsdName: "en11"},
			{displayName: "WendyOS warehouse-robot-station-01", bsdName: "en12"},
			{displayName: "WendyOS warehouse-robot-station-01", bsdName: "en13"},
		}
		devices := []models.USBDevice{{Name: "WendyOS warehouse-robot-station-01"}}
		checkUSBNetworkInterfaces(t, devices, ports, []string{""})
	})
}
