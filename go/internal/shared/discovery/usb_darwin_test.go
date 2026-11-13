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
