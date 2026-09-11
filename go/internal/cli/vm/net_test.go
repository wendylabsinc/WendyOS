package vm

import (
	"errors"
	"fmt"
	"io/fs"
	"net"
	"os"
	"strings"
	"testing"
)

func TestUserModeArgsForwardTheAgentPort(t *testing.T) {
	args, err := NetConfig{Mode: NetUser, AgentPort: 50051, MAC: MACFor("dev")}.Args()
	if err != nil {
		t.Fatalf("Args() error = %v", err)
	}
	joined := strings.Join(args, " ")
	if !strings.Contains(joined, "hostfwd=tcp:127.0.0.1:50051-:50051") {
		t.Errorf("user-mode args should forward the agent port; got %q", joined)
	}
	if !strings.Contains(joined, "virtio-net-pci") {
		t.Errorf("user-mode args should attach a virtio NIC; got %q", joined)
	}
}

func TestUserModeHonoursAnAlternatePort(t *testing.T) {
	args, err := NetConfig{Mode: NetUser, AgentPort: 51111, MAC: MACFor("dev")}.Args()
	if err != nil {
		t.Fatal(err)
	}
	// The guest port is fixed -- it is what the avahi service record advertises
	// -- so only the host side moves.
	if !strings.Contains(strings.Join(args, " "), "hostfwd=tcp:127.0.0.1:51111-:50051") {
		t.Errorf("expected host port 51111 forwarded to guest 50051; got %v", args)
	}
}

func TestMACForIsStablePerNameAndDistinctBetweenNames(t *testing.T) {
	// The MAC drives the DHCP lease, which drives the guest's address and the
	// hostname it announces over mDNS. An unstable MAC makes `wendy discover`
	// show a different device after every restart; a shared one makes two VMs
	// fight over one lease.
	if MACFor("dev") != MACFor("dev") {
		t.Error("MACFor must be deterministic for a given name")
	}
	if MACFor("dev") == MACFor("test") {
		t.Error("different VM names must get different MACs")
	}
}

func TestMACForIsAUnicastLocallyAdministeredAddress(t *testing.T) {
	mac := MACFor("dev")
	var first int
	if _, err := fmt.Sscanf(mac, "%02x", &first); err != nil {
		t.Fatalf("MACFor(%q) = %q, not parseable: %v", "dev", mac, err)
	}
	// Bit 0 clear = unicast (a multicast MAC is not a valid source address);
	// bit 1 set = locally administered, so we never collide with a real vendor.
	if first&0x01 != 0 {
		t.Errorf("MAC %q is multicast", mac)
	}
	if first&0x02 == 0 {
		t.Errorf("MAC %q is not locally administered", mac)
	}
	if len(mac) != len("52:54:00:12:34:56") {
		t.Errorf("MAC %q is not six colon-separated octets", mac)
	}
}

func TestArgsAlwaysSetTheMAC(t *testing.T) {
	for _, n := range []NetConfig{
		{Mode: NetUser, MAC: MACFor("dev")},
		{Mode: NetShared, SocketPath: "/var/run/socket_vmnet", MAC: MACFor("dev")},
	} {
		args, err := n.Args()
		if err != nil {
			t.Fatalf("Args() error = %v", err)
		}
		if !strings.Contains(strings.Join(args, " "), "mac=") {
			t.Errorf("%s mode did not set a MAC: %v", n.Mode, args)
		}
	}
}

func TestArgsRejectAMissingMAC(t *testing.T) {
	// Falling back to QEMU's default would silently give every VM on the host
	// the same address.
	if _, err := (NetConfig{Mode: NetUser}).Args(); err == nil {
		t.Fatal("Args() accepted a config with no MAC")
	}
}

func TestSharedModeUsesTheSocketVMNetEndpoint(t *testing.T) {
	args, err := NetConfig{Mode: NetShared, SocketPath: "/var/run/socket_vmnet", MAC: MACFor("dev")}.Args()
	if err != nil {
		t.Fatalf("Args() error = %v", err)
	}
	joined := strings.Join(args, " ")
	if !strings.Contains(joined, "/var/run/socket_vmnet") {
		t.Errorf("shared args should name the socket; got %q", joined)
	}
	if strings.Contains(joined, "hostfwd") {
		t.Errorf("shared mode needs no port forward; got %q", joined)
	}
}

func TestSharedModeRequiresASocketPath(t *testing.T) {
	if _, err := (NetConfig{Mode: NetShared, MAC: MACFor("dev")}).Args(); err == nil {
		t.Fatal("shared mode without a socket path should be an error, not a silent fallback")
	}
}

func TestUnknownModeIsRejected(t *testing.T) {
	if _, err := (NetConfig{Mode: "bridged", MAC: MACFor("dev")}).Args(); err == nil {
		t.Fatal("unknown mode should be rejected")
	}
}

func TestSupportsDiscovery(t *testing.T) {
	// SLIRP does not carry multicast, so mDNS never reaches the host and
	// `wendy discover` cannot see a user-mode VM. This is the fact the CLI has
	// to tell the user, so it is asserted rather than assumed.
	if (NetConfig{Mode: NetUser}).SupportsDiscovery() {
		t.Error("user mode must not claim discovery support")
	}
	if !(NetConfig{Mode: NetShared}).SupportsDiscovery() {
		t.Error("shared mode should support discovery")
	}
}

func TestFindSocketVMNetPrefersTheBrewPrefix(t *testing.T) {
	want := "/opt/homebrew/var/run/socket_vmnet"
	orig := vmStat
	t.Cleanup(func() { vmStat = orig })
	vmStat = func(name string) (os.FileInfo, error) {
		if name == want {
			return nil, nil
		}
		return nil, fs.ErrNotExist
	}

	got, err := FindSocketVMNet("darwin", "/opt/homebrew")
	if err != nil {
		t.Fatalf("FindSocketVMNet() error = %v", err)
	}
	if got != want {
		t.Errorf("FindSocketVMNet() = %q, want %q", got, want)
	}
}

func TestFindSocketVMNetErrorExplainsTheOneTimeSetup(t *testing.T) {
	orig := vmStat
	t.Cleanup(func() { vmStat = orig })
	vmStat = func(string) (os.FileInfo, error) { return nil, fs.ErrNotExist }

	_, err := FindSocketVMNet("darwin", "/opt/homebrew")
	if err == nil {
		t.Fatal("expected an error when the helper is not running")
	}
	if !strings.Contains(err.Error(), "socket_vmnet") {
		t.Errorf("error should name the helper; got %q", err)
	}
	if !strings.Contains(err.Error(), "--net user") {
		t.Errorf("error should point at the tier that needs no setup; got %q", err)
	}
}

func TestCheckHostPortAcceptsAFreePort(t *testing.T) {
	// Bind and release to find a port that is genuinely free right now.
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := l.Addr().(*net.TCPAddr).Port
	l.Close()

	if err := CheckHostPort(port); err != nil {
		t.Errorf("CheckHostPort(%d) on a free port = %v, want nil", port, err)
	}
}

func TestCheckHostPortReportsAConflictAndNamesTheFlag(t *testing.T) {
	// The whole point: QEMU's own message for this is "Could not set up host
	// forwarding rule", which says nothing about what to do next.
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	port := l.Addr().(*net.TCPAddr).Port

	err = CheckHostPort(port)
	if err == nil {
		t.Fatal("CheckHostPort on an occupied port returned nil")
	}
	if !errors.Is(err, ErrPortInUse) {
		t.Errorf("error should wrap ErrPortInUse; got %#v", err)
	}
	if !strings.Contains(err.Error(), "--port") {
		t.Errorf("error should name the flag that fixes it; got %q", err)
	}
}

func TestUserModeForwardsOnLoopbackOnly(t *testing.T) {
	// QEMU forwards on the wildcard when given no host address, which would put
	// the guest's agent on the LAN. It is unauthenticated until the device is
	// provisioned, which every fresh VM is.
	args, err := NetConfig{Mode: NetUser, AgentPort: 50051, MAC: MACFor("dev")}.Args()
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(args, " ")
	if !strings.Contains(joined, "hostfwd=tcp:127.0.0.1:") {
		t.Errorf("agent forward must bind loopback; got %q", joined)
	}
	if strings.Contains(joined, "hostfwd=tcp::") {
		t.Errorf("agent forward binds the wildcard; got %q", joined)
	}
}

func TestUserModeForwardsTheWholePortSpan(t *testing.T) {
	// A provisioned agent serves mTLS one port up, so a VM that forwards only
	// the plaintext port becomes unreachable the moment it is provisioned.
	args, err := NetConfig{Mode: NetUser, AgentPort: 51111, MAC: MACFor("vm")}.Args()
	if err != nil {
		t.Fatalf("Args() = %v", err)
	}
	joined := strings.Join(args, " ")
	for i := range AgentPortSpan {
		want := fmt.Sprintf("hostfwd=tcp:127.0.0.1:%d-:%d", 51111+i, DefaultAgentPort+i)
		if !strings.Contains(joined, want) {
			t.Errorf("Args() = %q, want it to contain %q", joined, want)
		}
	}
}

func TestCheckHostPortRejectsAConflictAnywhereInTheSpan(t *testing.T) {
	// The second port of the span is the one a naive check would miss.
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer l.Close()
	busy := l.Addr().(*net.TCPAddr).Port

	if err := CheckHostPort(busy - 1); !errors.Is(err, ErrPortInUse) {
		t.Errorf("CheckHostPort(%d) with %d busy = %v, want ErrPortInUse", busy-1, busy, err)
	}
}

func TestPickHostPortSkipsAnOccupiedSpan(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer l.Close()
	busy := l.Addr().(*net.TCPAddr).Port

	got, err := PickHostPort(busy, 8)
	if err != nil {
		t.Fatalf("PickHostPort(%d) = %v", busy, err)
	}
	if got == busy {
		t.Errorf("PickHostPort(%d) = %d, want it to skip the occupied span", busy, got)
	}
	if (got-busy)%AgentPortSpan != 0 {
		t.Errorf("PickHostPort(%d) = %d, want a span-aligned port", busy, got)
	}
}

func TestFindSocketVMNetSaysSharedNetIsMacOnly(t *testing.T) {
	// The error used to recommend Homebrew commands on hosts with no Homebrew.
	for _, goos := range []string{"linux", "windows"} {
		_, err := FindSocketVMNet(goos, "")
		if !errors.Is(err, ErrSharedNetUnsupported) {
			t.Errorf("FindSocketVMNet(%q) = %v, want ErrSharedNetUnsupported", goos, err)
		}
		if strings.Contains(err.Error(), "brew") {
			t.Errorf("FindSocketVMNet(%q) = %v, want no Homebrew advice off macOS", goos, err)
		}
	}
}

func TestUserModeForwardsTheDeviceRegistry(t *testing.T) {
	// `wendy run` falls back to pushing to the device's registry when chunked
	// deploy is unavailable, and it addresses it as <device host>:5000. Without
	// this forward that fallback cannot reach the guest at all.
	n := NetConfig{Mode: NetUser, AgentPort: 50051, RegistryPort: DeviceRegistryPort, MAC: MACFor("dev")}
	args, err := n.Args()
	if err != nil {
		t.Fatalf("args() = %v", err)
	}
	joined := strings.Join(args, " ")
	want := fmt.Sprintf("hostfwd=tcp:127.0.0.1:%d-:%d", DeviceRegistryPort, DeviceRegistryPort)
	if !strings.Contains(joined, want) {
		t.Errorf("args() = %q, want it to contain %q", joined, want)
	}
}

func TestABusyRegistryPortStillBootsTheVM(t *testing.T) {
	// A developer running their own registry on 5000 loses the fallback, not
	// the ability to start a VM.
	n := NetConfig{Mode: NetUser, AgentPort: 50051, MAC: MACFor("dev")}
	args, err := n.Args()
	if err != nil {
		t.Fatalf("args() = %v", err)
	}
	joined := strings.Join(args, " ")
	if strings.Contains(joined, fmt.Sprintf("-:%d", DeviceRegistryPort)) {
		t.Errorf("args() = %q, want no registry forward when the port was not claimed", joined)
	}
	if !strings.Contains(joined, "hostfwd=tcp:127.0.0.1:50051-:50051") {
		t.Errorf("args() = %q, want the agent forward regardless", joined)
	}
}

func TestCheckHostPortSeparatesImpossibleFromTaken(t *testing.T) {
	// An impossible port reported as "already in use" sends the user hunting
	// for a process that was never there.
	for _, port := range []int{0, -1, 99999, 65535} {
		err := CheckHostPort(port)
		if err == nil {
			t.Errorf("CheckHostPort(%d) = nil, want an error", port)
			continue
		}
		if errors.Is(err, ErrPortInUse) {
			t.Errorf("CheckHostPort(%d) = %v, want out-of-range, not in-use", port, err)
		}
		if !strings.Contains(err.Error(), "out of range") {
			t.Errorf("CheckHostPort(%d) = %v, want it to say out of range", port, err)
		}
	}
	// 65535 is rejected for the SPAN, not the base port: a VM occupies two
	// consecutive ports, so the last usable base is 65534.
	if err := CheckHostPort(65534); err != nil && !errors.Is(err, ErrPortInUse) {
		t.Errorf("CheckHostPort(65534) = %v, want it accepted on range (65534-65535 both fit)", err)
	}
}
