//go:build windows

package winusb

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"

	"golang.org/x/sys/windows"
)

// DriverProfile describes one independently upgradeable driver package. The
// complete ID set includes devices that only appear after a recovery handoff.
// Profiles have separate INF/catalog/interface identities; adding Qualcomm
// support must never replace a Jetson's driver or remove its staged gadget ID.
type DriverProfile struct {
	Name                        string
	guid, inf, catalog, version string
	models                      []struct{ desc, id string }
}

func JetsonDriver() DriverProfile {
	return DriverProfile{"Jetson", DeviceInterfaceGUID, infFileName, catalogFileName, "1.1.0.0", jetsonModels()}
}

func DragonwingDriver() DriverProfile {
	return DriverProfile{
		Name: "Qualcomm EDL",
		guid: "{91C4F453-3E14-4D3A-AB84-65D5EB532A89}",
		inf:  "wendy_qualcomm_edl.inf", catalog: "wendy_qualcomm_edl.cat", version: "1.0.0.0",
		models: []struct{ desc, id string }{{"Qualcomm EDL (Wendy)", `USB\VID_05C6&PID_9008`}},
	}
}

func (p DriverProfile) hardwareIDs() []string {
	ids := make([]string, 0, len(p.models))
	for _, m := range p.models {
		ids = append(ids, m.id)
	}
	return ids
}

func (p DriverProfile) supports(d Device) bool {
	for _, m := range p.models {
		if strings.EqualFold(m.id, d.HardwareID()) {
			return true
		}
	}
	return false
}

// DriverReady checks the actual selected devnode, not any interface on the
// host. Reading its installed INF also checks coverage for future gadget IDs.
// A newer compatible package is accepted, avoiding downgrades by older CLIs.
func DriverReady(d Device, p DriverProfile) (bool, error) {
	if !p.supports(d) {
		return false, fmt.Errorf("%s does not support %s", p.Name, d.HardwareID())
	}
	if !compatibleBinding(d, p) || profileInterfacePath(d.InstanceID, p) == "" {
		return false, nil
	}
	if d.DriverINF == "" || filepath.Base(d.DriverINF) != d.DriverINF {
		return false, nil
	}
	windir, err := windows.GetWindowsDirectory()
	if err != nil {
		return false, err
	}
	data, err := os.ReadFile(filepath.Join(windir, "INF", d.DriverINF))
	if os.IsNotExist(err) {
		return false, nil // incomplete/removed package needs repair
	}
	if err != nil {
		return false, fmt.Errorf("reading installed driver package %s: %w", d.DriverINF, err)
	}
	return infCoversIDs(string(data), runtime.GOARCH, p.hardwareIDs()), nil
}

func compatibleBinding(d Device, p DriverProfile) bool {
	return p.supports(d) && d.InstanceID != "" && d.Bound && d.Problem == 0 &&
		strings.EqualFold(d.Service, "WinUSB") && versionAtLeast(d.DriverVersion, p.version)
}

func versionAtLeast(got, minimum string) bool {
	parse := func(s string) ([4]uint64, bool) {
		var out [4]uint64
		parts := strings.Split(s, ".")
		if len(parts) != len(out) {
			return out, false
		}
		for i, part := range parts {
			n, err := strconv.ParseUint(part, 10, 16)
			if err != nil {
				return out, false
			}
			out[i] = n
		}
		return out, true
	}
	a, ok := parse(got)
	if !ok {
		return false
	}
	b, ok := parse(minimum)
	if !ok {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return a[i] > b[i]
		}
	}
	return true
}

// Our generated packages use Standard.NT<arch> model sections. Ignore comments
// and other architectures: an ID mentioned elsewhere is not driver coverage.
func infCoversIDs(inf, arch string, ids []string) bool {
	// Windows may preserve an INF as UTF-16LE in the driver store.
	if strings.HasPrefix(inf, "\xff\xfe") {
		b := []byte(inf[2:])
		u := make([]uint16, len(b)/2)
		for i := range u {
			u[i] = uint16(b[2*i]) | uint16(b[2*i+1])<<8
		}
		inf = windows.UTF16ToString(u)
	}
	wantSection := "[standard.nt" + arch + "]"
	active := false
	found := map[string]bool{}
	for _, line := range strings.Split(strings.ToLower(inf), "\n") {
		line, _, _ = strings.Cut(line, ";")
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "[") {
			active = line == wantSection
			continue
		}
		if !active {
			continue
		}
		_, value, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		parts := strings.Split(value, ",")
		if len(parts) >= 2 && strings.TrimSpace(parts[0]) == "usb_install" {
			found[strings.TrimSpace(parts[1])] = true
		}
	}
	for _, id := range ids {
		if !found[strings.ToLower(id)] {
			return false
		}
	}
	return true
}

func profileInterfacePath(instance string, p DriverProfile) string {
	guid := mustGUID(p.guid)
	paths, err := windows.CM_Get_Device_Interface_List(instance, &guid, windows.CM_GET_DEVICE_INTERFACE_LIST_PRESENT)
	if err != nil || len(paths) != 1 {
		return ""
	}
	return paths[0]
}

// OpenProfileInstance opens only the selected PnP instance and expected family.
func OpenProfileInstance(d Device, p DriverProfile) (*USBDevice, error) {
	if d.InstanceID == "" || !p.supports(d) {
		return nil, fmt.Errorf("invalid %s target %q", p.Name, d.InstanceID)
	}
	vid, pid, ok := ParseVIDPID(d.InstanceID)
	if !ok || vid != d.VID || pid != d.PID {
		return nil, fmt.Errorf("USB identity changed for %s", d.InstanceID)
	}
	path := profileInterfacePath(d.InstanceID, p)
	if path == "" {
		return nil, fmt.Errorf("%s has no %s WinUSB interface", d.InstanceID, p.Name)
	}
	return openPath(path)
}
