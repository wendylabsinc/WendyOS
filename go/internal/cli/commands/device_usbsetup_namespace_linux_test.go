//go:build linux && regression_usb_profile

package commands

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

// This is the production setup path against private, real NetworkManager and
// D-Bus daemons. veth devices supply stable MACs and port-dependent names; the
// physical USB/udev event boundary is explicitly outside this fixture.
type usbFixtureProcess struct {
	cancel context.CancelFunc
	done   chan error
	log    *os.File
}

func startUSBFixtureProcess(t *testing.T, label string, command ...string) *usbFixtureProcess {
	t.Helper()
	log, err := os.Create(filepath.Join(os.Getenv("WENDY_USB_PROFILE_EVIDENCE"), label+".log"))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cmd := exec.CommandContext(ctx, command[0], command[1:]...)
	cmd.Stdout, cmd.Stderr = log, log
	if err = cmd.Start(); err != nil {
		cancel()
		log.Close()
		t.Fatal(err)
	}
	p := &usbFixtureProcess{cancel, make(chan error, 1), log}
	go func() { p.done <- cmd.Wait(); close(p.done) }()
	t.Cleanup(func() { p.stop(t) })
	return p
}
func (p *usbFixtureProcess) stop(t *testing.T) {
	t.Helper()
	if p.cancel == nil {
		return
	}
	p.cancel() // CommandContext retains the owned process until Wait completes.
	select {
	case <-p.done:
	case <-time.After(4 * time.Second):
		t.Error("owned USB fixture process did not drain")
	}
	if err := p.log.Close(); err != nil {
		t.Error(err)
	}
	p.cancel = nil
}
func usbFixtureCommand(args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, args[0], args[1:]...)
	cmd.Env = append(os.Environ(), "LC_ALL=C.UTF-8")
	out, err := cmd.CombinedOutput()
	if err != nil {
		return string(out), fmt.Errorf("%v: %w: %s", args, err, out)
	}
	return strings.TrimSpace(string(out)), nil
}
func usbFixtureMust(t *testing.T, args ...string) string {
	t.Helper()
	out, err := usbFixtureCommand(args...)
	if err != nil {
		t.Fatal(err)
	}
	return out
}
func usbFixtureAwait(t *testing.T, duration time.Duration, failure string, check func() bool) {
	t.Helper()
	until := time.Now().Add(duration)
	for time.Now().Before(until) {
		if check() {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatal(failure)
}
func usbFixtureProfileActive(iface, uuid string) bool {
	out, err := usbFixtureCommand("nmcli", "-g", "GENERAL.CON-UUID", "device", "show", iface)
	if err != nil || out != uuid {
		return false
	}
	link, err := net.InterfaceByName(iface)
	if err != nil {
		return false
	}
	addresses, err := link.Addrs()
	if err != nil {
		return false
	}
	v4, v6 := false, false
	for _, address := range addresses {
		ip, _, e := net.ParseCIDR(address.String())
		if e == nil && ip.IsLinkLocalUnicast() {
			if ip.To4() != nil {
				v4 = true
			} else {
				v6 = true
			}
		}
	}
	return v4 && v6
}
func usbFixtureEcho(t *testing.T, marker string) {
	t.Helper()
	conn, err := net.DialTimeout("tcp", "169.254.200.2:18474", 3*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if !conn.LocalAddr().(*net.TCPAddr).IP.IsLinkLocalUnicast() {
		t.Fatal("USB traffic did not originate on assigned link-local address")
	}
	conn.SetDeadline(time.Now().Add(3 * time.Second))
	if _, err = fmt.Fprintln(conn, marker); err != nil {
		t.Fatal(err)
	}
	got, err := bufio.NewReader(conn).ReadString('\n')
	if err != nil || got != "same-usb-device:"+marker+"\n" {
		t.Fatalf("USB device lost exact TCP response: %q %v", got, err)
	}
}
func TestUSBProfileNamespacePeer(t *testing.T) {
	if os.Getenv("WENDY_USB_PROFILE_PEER") != "1" {
		t.Skip("owned peer namespace only")
	}
	listener, err := net.Listen("tcp", "0.0.0.0:18474")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	// Only three short sequential requests are expected; the socket stays open
	// while its device address disappears and returns on the replacement veth.
	for range 3 {
		listener.(*net.TCPListener).SetDeadline(time.Now().Add(100 * time.Second))
		conn, e := listener.Accept()
		if e != nil {
			t.Fatal(e)
		}
		conn.SetDeadline(time.Now().Add(4 * time.Second))
		line, e := bufio.NewReader(conn).ReadString('\n')
		if e == nil {
			_, e = fmt.Fprint(conn, "same-usb-device:"+line)
		}
		conn.Close()
		if e != nil {
			t.Fatal(e)
		}
	}
}
func TestUSBProfileFollowsDeviceAcrossInterfaceRecreation(t *testing.T) {
	if os.Getenv("WENDY_USB_PROFILE_NAMESPACE") != "1" {
		t.Skip("requires private hosted NetworkManager namespace")
	}
	if os.Geteuid() != 0 || os.Getenv("DBUS_SYSTEM_BUS_ADDRESS") != "" {
		t.Fatal("invalid private namespace entry")
	}
	evidence := os.Getenv("WENDY_USB_PROFILE_EVIDENCE")
	if !filepath.IsAbs(evidence) {
		t.Fatal("absolute evidence path required")
	}
	write := func(path, content string) {
		t.Helper()
		mode := os.FileMode(0600)
		if strings.HasPrefix(path, evidence+string(os.PathSeparator)) {
			mode = 0644
		}
		if err := os.WriteFile(path, []byte(content), mode); err != nil {
			t.Fatal(err)
		}
	}
	write("/run/usb-profile/dbus.conf", `<busconfig><type>system</type><listen>unix:path=/run/dbus/system_bus_socket</listen><auth>EXTERNAL</auth><policy context="default"><allow user="root"/><allow own="*"/><allow send_destination="*"/><allow receive_sender="*"/></policy></busconfig>`)
	write("/run/usb-profile/NetworkManager.conf", `[main]
plugins=keyfile
auth-polkit=false
no-auto-default=*
dns=none
rc-manager=unmanaged
hostname-mode=none
[connectivity]
enabled=false
[device-fixture]
match-device=interface-name:usbport*;interface-name:unrelated0
managed=1
[logging]
level=DEBUG
`)
	t.Setenv("DBUS_SYSTEM_BUS_ADDRESS", "unix:path=/run/dbus/system_bus_socket")
	startUSBFixtureProcess(t, "dbus", "dbus-daemon", "--nofork", "--nopidfile", "--config-file=/run/usb-profile/dbus.conf")
	usbFixtureAwait(t, 5*time.Second, "private D-Bus socket did not appear", func() bool {
		info, e := os.Stat("/run/dbus/system_bus_socket")
		return e == nil && info.Mode()&os.ModeSocket != 0
	})
	startNM := func(label string) *usbFixtureProcess {
		p := startUSBFixtureProcess(t, label, "NetworkManager", "--no-daemon", "--config=/run/usb-profile/NetworkManager.conf", "--config-dir=/run/usb-profile/conf.d", "--system-config-dir=/run/usb-profile/conf.d", "--intern-config=/run/usb-profile/internal.conf", "--pid-file=/run/usb-profile/NetworkManager.pid", "--state-file=/var/lib/NetworkManager/NetworkManager.state")
		usbFixtureAwait(t, 8*time.Second, "private NetworkManager did not become ready", func() bool { _, e := usbFixtureCommand("nmcli", "general", "status"); return e == nil })
		return p
	}
	nm := startNM("networkmanager-initial")
	usbFixtureMust(t, "nmcli", "connection", "add", "type", "ethernet", "con-name", "wendy-usb", "ifname", "usbport0", "connection.autoconnect", "no", "ipv4.method", "shared", "ipv6.method", "disabled")
	var setup bytes.Buffer
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
	err := runUSBSetup(ctx, "usbport0", &setup)
	cancel()
	t.Log(setup.String())
	write(filepath.Join(evidence, "production-setup.log"), setup.String())
	if err != nil {
		t.Fatalf("production USB setup failed: %v", err)
	}
	if _, err = usbFixtureCommand("nmcli", "connection", "show", "wendy-usb"); err == nil {
		t.Fatal("legacy name-bound USB profile survived setup")
	}
	mac, _ := net.ParseMAC("02:00:00:00:21:24")
	name := usbSetupConnName(mac)
	uuid := usbFixtureMust(t, "nmcli", "-g", "connection.uuid", "connection", "show", name)
	if len(uuid) != 36 {
		t.Fatalf("invalid saved USB UUID: %q", uuid)
	}
	for _, field := range []string{"ipv4.method", "ipv6.method"} {
		if got := usbFixtureMust(t, "nmcli", "-g", field, "connection", "show", uuid); got != "link-local" {
			t.Fatalf("saved USB %s is %q", field, got)
		}
	}
	usbFixtureAwait(t, 8*time.Second, "initial USB profile not active with both link-local address families", func() bool { return usbFixtureProfileActive("usbport0", uuid) })
	if got := usbFixtureMust(t, "nmcli", "-g", "GENERAL.CON-UUID", "device", "show", "unrelated0"); got == uuid {
		t.Fatal("USB profile attached to unrelated MAC")
	}
	rules, err := os.ReadFile(usbSetupUdevPath)
	if err != nil || string(rules) != usbSetupUdevRule {
		t.Fatal("production udev rule was not written into private mount")
	}
	boundary, err := os.ReadFile("/run/usb-profile/udev-boundary.log")
	if err != nil || !strings.HasSuffix(string(boundary), "control --reload-rules\ntrigger\n") {
		t.Fatal("unexpected udev boundary calls")
	}
	// Ubuntu24.04 persists NM changes as Netplan YAML and derives a runtime
	// keyfile. Other NM builds persist the keyfile directly. Require a durable
	// file containing this exact UUID; a runtime-only connection cannot pass.
	netplanPath := "/etc/netplan/90-NM-" + uuid + ".yaml"
	netplanBacked := false
	persisted := false
	files, _ := filepath.Glob("/etc/NetworkManager/system-connections/*")
	files = append(files, netplanPath)
	for _, file := range files {
		body, e := os.ReadFile(file)
		if e != nil {
			continue
		}
		matches := bytes.Contains(body, []byte("uuid="+uuid))
		if file == netplanPath {
			var saved struct {
				Network struct {
					Ethernets map[string]struct {
						Match struct {
							MAC string `yaml:"macaddress"`
						}
						NetworkManager struct {
							UUID string `yaml:"uuid"`
						} `yaml:"networkmanager"`
					} `yaml:"ethernets"`
				} `yaml:"network"`
			}
			if e = yaml.Unmarshal(body, &saved); e != nil {
				t.Fatal(e)
			}
			for _, entry := range saved.Network.Ethernets {
				if entry.NetworkManager.UUID == uuid && strings.EqualFold(entry.Match.MAC, mac.String()) {
					matches = true
					netplanBacked = true
				}
			}
		}
		if !matches {
			continue
		}
		info, e := os.Stat(file)
		if e != nil || info.Mode().Perm() != 0600 {
			t.Fatal("saved USB profile permissions are not 0600")
		}
		persisted = true
		write(filepath.Join(evidence, "initial-profile"+filepath.Ext(file)), string(body))
	}
	if !persisted {
		t.Fatal("USB profile was not persisted by NetworkManager")
	}
	t.Setenv("WENDY_USB_PROFILE_PEER", "1")
	startUSBFixtureProcess(t, "gadget", "ip", "netns", "exec", "gadget", os.Args[0], "-test.run=^TestUSBProfileNamespacePeer$", "-test.v", "-test.timeout=120s")
	// The first TCP probe is also peer readiness. It sends application bytes only
	// once a connection succeeds; failed connect attempts do not consume requests.
	usbFixtureAwait(t, 4*time.Second, "gadget listener absent", func() bool {
		out, e := usbFixtureCommand("ip", "netns", "exec", "gadget", "ss", "-ltn", "sport = :18474")
		return e == nil && strings.Contains(out, "18474")
	})
	usbFixtureEcho(t, "before-port-change")
	old, err := net.InterfaceByName("usbport0")
	if err != nil {
		t.Fatal(err)
	}
	usbFixtureMust(t, "ip", "link", "delete", "usbport0")
	usbFixtureMust(t, "ip", "link", "add", "usbport1", "address", mac.String(), "type", "veth", "peer", "name", "gadget1")
	usbFixtureMust(t, "ip", "link", "set", "gadget1", "netns", "gadget")
	usbFixtureMust(t, "ip", "link", "set", "usbport1", "up")
	usbFixtureMust(t, "ip", "netns", "exec", "gadget", "ip", "address", "add", "169.254.200.2/16", "dev", "gadget1")
	usbFixtureMust(t, "ip", "netns", "exec", "gadget", "ip", "link", "set", "gadget1", "up")
	changed, err := net.InterfaceByName("usbport1")
	if err != nil || changed.Index == old.Index {
		t.Fatal("fixture did not recreate a new kernel interface")
	}
	// No setup, connection up, reload or profile edit after replug: the exact
	// durable UUID must autoconnect solely through its stable MAC match.
	usbFixtureAwait(t, 25*time.Second, "saved USB profile did not reconnect after port/interface change", func() bool { return usbFixtureProfileActive("usbport1", uuid) })
	usbFixtureEcho(t, "after-port-change")
	if pin := usbFixtureMust(t, "nmcli", "-g", "connection.interface-name", "connection", "show", uuid); pin != "" && pin != "--" {
		t.Fatalf("saved profile remains interface-name pinned: %q", pin)
	}
	nm.stop(t)
	if netplanBacked {
		// Exercise reconstruction from the saved YAML, rather than accidentally
		// accepting only a runtime keyfile left behind by the old daemon.
		runtimeProfile := "/run/NetworkManager/system-connections/netplan-NM-" + uuid + ".nmconnection"
		if err := os.Remove(runtimeProfile); err != nil {
			t.Fatal(err)
		}
		usbFixtureMust(t, "netplan", "generate")
		body, err := os.ReadFile(runtimeProfile)
		if err != nil || !bytes.Contains(body, []byte("uuid="+uuid)) {
			t.Fatal("durable Netplan profile did not regenerate the same UUID")
		}
	}
	startNM("networkmanager-restarted")
	usbFixtureAwait(t, 25*time.Second, "saved USB profile did not survive NetworkManager restart", func() bool { return usbFixtureProfileActive("usbport1", uuid) })
	usbFixtureEcho(t, "after-daemon-restart")
	if got := usbFixtureMust(t, "nmcli", "-g", "connection.uuid", "connection", "show", name); got != uuid {
		t.Fatal("port migration recreated rather than reused the saved USB profile")
	}
	if got := usbFixtureMust(t, "nmcli", "-g", "GENERAL.CON-UUID", "device", "show", "unrelated0"); got == uuid {
		t.Fatal("restarted USB profile attached to unrelated MAC")
	}
	write(filepath.Join(evidence, "final-profile.txt"), usbFixtureMust(t, "nmcli", "connection", "show", uuid))
	t.Log("same saved USB profile reconnected across new interface and daemon restart with exact TCP bytes")
}
