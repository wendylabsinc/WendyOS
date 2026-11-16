//go:build linux

package configpartition

import (
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/miekg/dns"
	"github.com/wendylabsinc/wendy/go/internal/agent/services"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
	"golang.org/x/net/ipv4"
)

func requireHostnameNamespace(t *testing.T) {
	t.Helper()
	if os.Getenv("WENDY_HOSTNAME_NAMESPACE") != "1" {
		t.Skip("requires disposable Linux namespace runner")
	}
	if os.Geteuid() != 0 {
		t.Fatal("namespace runner must be root")
	}
	for _, kind := range []string{"mnt", "uts", "net", "pid"} {
		current, err := os.Readlink("/proc/self/ns/" + kind)
		parent := os.Getenv("WENDY_HOSTNAME_PARENT_" + strings.ToUpper(kind))
		if err != nil || parent == "" || current == parent {
			t.Fatalf("%s namespace not isolated: %q, %v", kind, current, err)
		}
	}
}

func hostnameCommand(t *testing.T, name string, args ...string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, name, args...).CombinedOutput()
	if err != nil {
		t.Fatalf("%s %v: %v\n%s", name, args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// A new process runs the actual OS identity scripts and the agent startup hook.
// The service manager is adapted by the namespace runner; Avahi is a real daemon.
func TestDeviceNameBootProcess(t *testing.T) {
	if os.Getenv("WENDY_HOSTNAME_BOOT_CHILD") != "1" {
		return
	}
	requireHostnameNamespace(t)
	hostnameCommand(t, "/usr/sbin/generate-hostname.sh")
	hostnameCommand(t, "/bin/bash", os.Getenv("WENDY_MDNS_BOOT_SCRIPT"))
	services.ReassertHostnameAdvertisement(zap.NewNop())
}

func assertAdvertisedDeviceName(t *testing.T, hostname, name string) {
	t.Helper()
	if got := hostnameCommand(t, "/usr/bin/hostname"); got != hostname {
		t.Fatalf("running hostname = %q, want %q", got, hostname)
	}
	contents, err := os.ReadFile("/etc/hostname")
	if err != nil || strings.TrimSpace(string(contents)) != hostname {
		t.Fatalf("persisted hostname = %q, %v", contents, err)
	}
	t.Setenv("WENDY_EXPECT_HOSTNAME", hostname)
	t.Setenv("WENDY_EXPECT_ADVERTISED_NAME", name)
	hostnameCommand(t, "ip", "netns", "exec", "probe", os.Args[0], "-test.run", "^TestDeviceNameDNSProbe$", "-test.v")
}

func TestDeviceNameDNSProbe(t *testing.T) {
	hostname := os.Getenv("WENDY_EXPECT_HOSTNAME")
	if hostname == "" {
		return
	}
	requireHostnameNamespace(t)
	name := os.Getenv("WENDY_EXPECT_ADVERTISED_NAME")
	// A legacy multicast DNS query from the probe veth travels to the interface
	// Avahi publishes on. Inspect the actual DNS response, not its service XML.
	iface, err := net.InterfaceByName("probe0")
	if err != nil {
		t.Fatal(err)
	}
	conn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.ParseIP("192.0.2.2")})
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	packet := ipv4.NewPacketConn(conn)
	if err := packet.SetMulticastInterface(iface); err != nil {
		t.Fatal(err)
	}
	if err := packet.SetMulticastTTL(255); err != nil {
		t.Fatal(err)
	}
	instance := hostname + "._wendyos._udp.local."
	query := new(dns.Msg)
	query.SetQuestion(instance, dns.TypeANY)
	data, err := query.Pack()
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(12 * time.Second)
	var last string
	for time.Now().Before(deadline) {
		if _, err := conn.WriteToUDP(data, &net.UDPAddr{IP: net.IPv4(224, 0, 0, 251), Port: 5353}); err != nil {
			t.Fatal(err)
		}
		_ = conn.SetReadDeadline(time.Now().Add(400 * time.Millisecond))
		buffer := make([]byte, 9000)
		n, _, err := conn.ReadFromUDP(buffer)
		if err != nil {
			last = err.Error()
			continue
		}
		var response dns.Msg
		if err := response.Unpack(buffer[:n]); err != nil {
			t.Fatal(err)
		}
		if !response.Response || response.Id != query.Id {
			continue
		}
		last = response.String()
		var targetOK, nameOK, identityOK bool
		for _, rr := range append(response.Answer, response.Extra...) {
			if !strings.EqualFold(rr.Header().Name, instance) {
				continue
			}
			switch r := rr.(type) {
			case *dns.SRV:
				targetOK = r.Target == hostname+".local." && r.Port == 50052
			case *dns.TXT:
				values := make(map[string]bool)
				for _, v := range r.Txt {
					values[v] = true
				}
				nameOK = values["name="+name]
				identityOK = values["id=151bd03a-4c88-465d-b098-8b2a6d9c1339"] && values["tls=true"]
			}
		}
		if targetOK && nameOK && identityOK {
			return
		}
	}
	t.Fatalf("no matching wire advertisement for hostname=%s name=%s; last response: %s", hostname, name, last)
}

func TestDeviceNameRestartIntegration(t *testing.T) {
	requireHostnameNamespace(t)
	for _, blocked := range []bool{false, true} {
		t.Run(fmt.Sprintf("overrideRemovalBlocked=%v", blocked), func(t *testing.T) {
			hostnameCommand(t, "/usr/bin/systemctl", "stop", "avahi-daemon")
			for path, value := range map[string]string{
				"/etc/wendyos/device-name":                 "old-device\n",
				"/etc/wendyos/device-uuid":                 "151bd03a-4c88-465d-b098-8b2a6d9c1339\n",
				"/etc/wendy-agent/hostname":                "old-explicit\n",
				"/etc/hostname":                            "old-explicit\n",
				"/etc/avahi/services/wendyos-mdns.service": `<?xml version="1.0"?><service-group><name replace-wildcards="yes">%h</name><service><type>_wendyos._udp</type><port>50052</port><txt-record>id=151bd03a-4c88-465d-b098-8b2a6d9c1339</txt-record><txt-record>name=old-explicit</txt-record><txt-record>displayname=Old Explicit</txt-record><txt-record>fqdn=sh.wendy.old-explicit</txt-record><txt-record>tls=true</txt-record></service></service-group>`,
			} {
				if err := os.WriteFile(path, []byte(value), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			hostnameCommand(t, "/usr/bin/hostname", "old-explicit")
			hostnameCommand(t, "/usr/bin/systemctl", "restart", "avahi-daemon")
			assertAdvertisedDeviceName(t, "old-explicit", "old-explicit")
			bound := false
			if blocked {
				// Bind-mount the still-readable explicit hostname to force unlink EBUSY.
				hostnameCommand(t, "mount", "--bind", "/etc/wendy-agent/hostname", "/etc/wendy-agent/hostname")
				bound = true
				defer func() {
					if bound {
						hostnameCommand(t, "umount", "/etc/wendy-agent/hostname")
					}
				}()
			}
			wantStored := "new-device\n"
			if blocked {
				wantStored = "old-device\n"
			}
			assertStoredDeviceName := func(when string) {
				t.Helper()
				// A failed rename must not leave the new name on disk to take effect later.
				if got, err := os.ReadFile("/etc/wendyos/device-name"); err != nil || string(got) != wantStored {
					t.Fatalf("stored device name %s = %q, %v; want %q", when, got, err, wantStored)
				}
			}
			core, logs := observer.New(zap.InfoLevel)
			err := applyDeviceName(zap.New(core), "new-device")
			wantHostname, wantName := "wendyos-new-device", "new-device"
			if blocked {
				if err == nil {
					t.Fatal("reported success while an unremovable explicit hostname still wins")
				}
				if logs.FilterMessage("Could not clear explicit hostname; it will keep overriding the device name").Len() != 1 {
					t.Fatal("missing failed-removal warning")
				}
				wantHostname, wantName = "old-explicit", "old-explicit"
			} else if err != nil {
				t.Fatal(err)
			}
			assertStoredDeviceName("after the rename")
			cfgDir, confPath := writeConf(t, "[device]\nname=new-device\n")
			configPath := t.TempDir()
			applyWendyConf(zap.NewNop(), cfgDir, configPath)
			conf, confErr := os.ReadFile(confPath)
			if blocked {
				if confErr != nil || !strings.Contains(string(conf), "new-device") {
					t.Fatalf("failed rename request was discarded: %q, %v", conf, confErr)
				}
			} else if !os.IsNotExist(confErr) {
				t.Fatalf("successful rename request was not consumed: %v", confErr)
			}
			assertStoredDeviceName("after the config-partition retry")
			assertAdvertisedDeviceName(t, wantHostname, wantName)
			reboot := func() {
				t.Helper()
				hostnameCommand(t, "/usr/bin/systemctl", "stop", "avahi-daemon")
				// Reset the volatile hostname and advertisement to model startup, leaving
				// both persistent identity files untouched. The scripts must restore them.
				hostnameCommand(t, "/usr/bin/hostname", "boot-placeholder")
				service, err := os.ReadFile("/etc/avahi/services/wendyos-mdns.service")
				if err != nil {
					t.Fatal(err)
				}
				if err = os.WriteFile("/etc/avahi/services/wendyos-mdns.service", []byte(strings.ReplaceAll(string(service), "name="+wantName, "name=boot-placeholder")), 0o644); err != nil {
					t.Fatal(err)
				}
				t.Setenv("WENDY_HOSTNAME_BOOT_CHILD", "1")
				hostnameCommand(t, os.Args[0], "-test.run", "^TestDeviceNameBootProcess$", "-test.v")
				t.Setenv("WENDY_HOSTNAME_BOOT_CHILD", "")
			}
			reboot()
			assertAdvertisedDeviceName(t, wantHostname, wantName)
			if blocked {
				hostnameCommand(t, "umount", "/etc/wendy-agent/hostname")
				bound = false
				applyWendyConf(zap.NewNop(), cfgDir, configPath)
				if _, err := os.Stat(confPath); !os.IsNotExist(err) {
					t.Fatalf("recovered rename request was not consumed: %v", err)
				}
				wantHostname, wantName, wantStored = "wendyos-new-device", "new-device", "new-device\n"
				assertStoredDeviceName("after the recovered rename")
				assertAdvertisedDeviceName(t, wantHostname, wantName)
				reboot()
				assertAdvertisedDeviceName(t, wantHostname, wantName)
			}
			hostnameCommand(t, "/usr/bin/systemctl", "stop", "avahi-daemon")
		})
	}
}
