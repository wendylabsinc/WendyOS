package rtps

import (
	"bufio"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// Kernel interface names: at most 15 bytes. The names end up in CycloneDDS XML,
// so anything else is skipped rather than escaped.
var interfaceNamePattern = regexp.MustCompile(`^[A-Za-z0-9_.:-]{1,15}$`)

// PeripheralInterfaces returns the interfaces that lead to hardware attached to
// the device rather than to a network: eligible wired interfaces (see
// HostInterfaces) that are backed by a physical network device and do not carry
// a default route. A robot or sensor on its own cable qualifies; the device's
// uplink, VPNs, bridges and container interfaces do not. When the routes cannot
// be read, no interface qualifies, so peripheral discovery never widens to an
// uplink.
func PeripheralInterfaces() ([]string, error) {
	wired, err := HostInterfaces()
	if err != nil {
		return nil, err
	}
	uplinks, err := defaultRouteInterfaces("/proc/net/route", "/proc/net/ipv6_route")
	if err != nil {
		return nil, nil
	}
	return peripheralInterfaces(wired, uplinks, "/sys/class/net"), nil
}

func peripheralInterfaces(wired []string, uplinks map[string]bool, sysClassNet string) []string {
	var names []string
	for _, name := range wired {
		if uplinks[name] || !interfaceNamePattern.MatchString(name) {
			continue
		}
		// Physical network devices have a "device" link in sysfs; virtual
		// interfaces (VPNs, bridges, veth, dummy) do not.
		if _, err := os.Stat(filepath.Join(sysClassNet, name, "device")); err != nil {
			continue
		}
		names = append(names, name)
	}
	return names
}

// defaultRouteInterfaces reads the IPv4 and IPv6 routing tables and returns the
// interfaces that carry a default route. A missing IPv6 table is fine; a
// missing IPv4 table is an error.
func defaultRouteInterfaces(ipv4Path, ipv6Path string) (map[string]bool, error) {
	uplinks := map[string]bool{}
	ipv4, err := os.Open(ipv4Path)
	if err != nil {
		return nil, err
	}
	defer ipv4.Close()
	scanner := bufio.NewScanner(ipv4)
	for scanner.Scan() {
		// Iface Destination Gateway Flags RefCnt Use Metric Mask ...
		fields := strings.Fields(scanner.Text())
		if len(fields) >= 8 && fields[1] == "00000000" && fields[7] == "00000000" {
			uplinks[fields[0]] = true
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	ipv6, err := os.Open(ipv6Path)
	if err != nil {
		if !os.IsNotExist(err) {
			return nil, err
		}
		return uplinks, nil
	}
	defer ipv6.Close()
	scanner = bufio.NewScanner(ipv6)
	for scanner.Scan() {
		// Destination PrefixLen Source SourcePrefixLen NextHop Metric RefCnt Use Flags Iface
		fields := strings.Fields(scanner.Text())
		if len(fields) >= 10 && fields[0] == strings.Repeat("0", 32) && fields[1] == "00" && fields[9] != "lo" {
			uplinks[fields[9]] = true
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	return uplinks, nil
}
