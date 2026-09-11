//go:build windows

package winusb

import (
	"strings"
	"testing"
)

func TestDriverProfilesCoexist(t *testing.T) {
	j, q := JetsonDriver(), DragonwingDriver()
	if j.inf == q.inf || j.catalog == q.catalog || j.guid == q.guid {
		t.Fatal("driver families share package or interface identity")
	}
	for _, p := range []DriverProfile{j, q} {
		inf := generateProfileINF(p)
		for _, arch := range []string{"amd64", "arm64"} {
			if !infCoversIDs(inf, arch, p.hardwareIDs()) {
				t.Fatalf("%s missing %s IDs", p.Name, arch)
			}
		}
		if strings.Contains(inf, "VID_1D6B&PID_0104") {
			t.Fatal("mass storage gadget must retain inbox driver")
		}
	}
	if infCoversIDs(generateProfileINF(j), "amd64", q.hardwareIDs()) || infCoversIDs(generateProfileINF(q), "amd64", j.hardwareIDs()) {
		t.Fatal("one family must not establish another family's readiness")
	}
}

func TestDriverCoverageRequiresFutureGadgetAndCorrectArchitecture(t *testing.T) {
	p := JetsonDriver()
	inf := generateProfileINF(p)
	// A Thor-era package supports the currently attached recovery device but
	// cannot satisfy an operation requiring newer Orin IDs or its ADB handoff.
	for _, id := range []string{`USB\VID_0955&PID_7100`, `USB\VID_0955&PID_7523`} {
		broken := strings.ReplaceAll(inf, id, "USB\\VID_0000&PID_0000")
		if broken == inf {
			t.Fatalf("fixture ID missing: %s", id)
		}
		if infCoversIDs(broken+"\n; "+id, "amd64", p.hardwareIDs()) {
			t.Fatalf("accepted package missing %s", id)
		}
	}
	if infCoversIDs(strings.ReplaceAll(inf, "[Standard.NTamd64]", "[Other]"), "amd64", p.hardwareIDs()) {
		t.Fatal("ARM64 model coverage must not satisfy amd64")
	}
	if infCoversIDs(strings.ReplaceAll(inf, "= USB_Install,", "= Other_Install,"), "amd64", p.hardwareIDs()) {
		t.Fatal("IDs bound to another install section must not satisfy WinUSB coverage")
	}
}

func TestCompatibleDriverBinding(t *testing.T) {
	p := JetsonDriver()
	good := Device{InstanceID: `USB\VID_0955&PID_7026\A`, VID: VendorNVIDIA, PID: ProductThor,
		Bound: true, Service: "WinUSB", DriverVersion: "1.1.0.0"}
	if !compatibleBinding(good, p) {
		t.Fatal("current package rejected")
	}
	for _, version := range []string{"1.2.0.0", "2.0.0.0"} {
		d := good
		d.DriverVersion = version
		if !compatibleBinding(d, p) {
			t.Fatal("newer package would be downgraded")
		}
	}
	for name, alter := range map[string]func(*Device){
		"old":             func(d *Device) { d.DriverVersion = "1.0.0.0" },
		"unknown version": func(d *Device) { d.DriverVersion = "" },
		"wrong service":   func(d *Device) { d.Service = "qcusbser" },
		"problem":         func(d *Device) { d.Problem = 28 },
		"unbound":         func(d *Device) { d.Bound = false },
		"wrong family":    func(d *Device) { d.VID = 0x05c6; d.PID = 0x9008 },
		"no selection":    func(d *Device) { d.InstanceID = "" },
	} {
		t.Run(name, func(t *testing.T) {
			d := good
			alter(&d)
			if compatibleBinding(d, p) {
				t.Fatal("incompatible binding accepted")
			}
		})
	}
}

func TestDriverBindOnlySelectedHardwareID(t *testing.T) {
	target := Device{InstanceID: "qualcomm-a", VID: 0x05c6, PID: 0x9008}
	jetson := Device{InstanceID: "thor", VID: VendorNVIDIA, PID: ProductThor}
	if err := soleBindingTarget([]Device{target, jetson}, target); err != nil {
		t.Fatal(err)
	}
	other := target
	other.InstanceID = "qualcomm-b"
	if err := soleBindingTarget([]Device{target, other, jetson}, target); err == nil {
		t.Fatal("ID-wide bind would affect another board")
	}
	if err := soleBindingTarget([]Device{other, jetson}, target); err == nil {
		t.Fatal("substituted a different board")
	}
}

func TestVersionComparison(t *testing.T) {
	for _, v := range []string{"", "1.1", "x.1.0.0", "1.1.0.65536", "1.0.9.9"} {
		if versionAtLeast(v, "1.1.0.0") {
			t.Fatalf("accepted %q", v)
		}
	}
	if !versionAtLeast("1.10.0.0", "1.2.0.0") {
		t.Fatal("version comparison must be numeric")
	}
}
