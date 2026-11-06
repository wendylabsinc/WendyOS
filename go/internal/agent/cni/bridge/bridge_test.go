//go:build linux

package bridge

import (
	"encoding/json"
	"fmt"
	"github.com/wendylabsinc/wendy/go/internal/agent/hostnetwork"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/containernetworking/cni/pkg/skel"
	"github.com/containernetworking/plugins/pkg/ns"
	"github.com/containernetworking/plugins/pkg/testutils"
	"github.com/vishvananda/netlink"
	"github.com/wendylabsinc/wendy/go/internal/agent/cni/hostlocal"
)

func TestDeleteQuarantinesLeaseUntilInternetGrantRevoked(t *testing.T) {
	if os.Getenv("WENDY_MESH_NAMESPACE_TEST") != "1" {
		t.Skip("disposable privileged container only")
	}
	// Use a real host-local store and fail firewall inspection at the command
	// boundary. Cleanup also holds the host's cross-process forwarding lock;
	// keep this integration test in the same isolated environment as CNI ADD.
	bin := t.TempDir()
	firewall := filepath.Join(bin, "iptables")
	if err := os.WriteFile(firewall, []byte("#!/bin/sh\nexit 4\n"), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	for _, netns := range []string{"", filepath.Join(t.TempDir(), "missing-netns")} {
		t.Run(strings.ReplaceAll(netns, "/", "_"), func(t *testing.T) {
			if err := os.WriteFile(firewall, []byte("#!/bin/sh\nexit 4\n"), 0700); err != nil {
				t.Fatal(err)
			}
			cfg, err := json.Marshal(map[string]any{
				"cniVersion": "0.4.0", "name": "quarantine-test", "type": "bridge", "bridge": "app0",
				"ipam": map[string]any{"type": "host-local", "subnet": "10.42.1.0/28", "dataDir": t.TempDir()},
			})
			if err != nil {
				t.Fatal(err)
			}
			args := &skel.CmdArgs{ContainerID: "previous-app", IfName: "eth0", Netns: netns, StdinData: cfg}
			if _, _, err := hostlocal.Allocate(args); err != nil {
				t.Fatal(err)
			}
			if err := CmdDel(args); err == nil {
				t.Fatal("firewall failure was hidden")
			}
			if err := hostlocal.CmdCheck(args); err != nil {
				t.Fatalf("unsafe address released despite stale Internet grant: %v", err)
			}
			// An absent chain means every grant is gone. Retried DEL can now release
			// the quarantine even when the original namespace has disappeared.
			if err := os.WriteFile(firewall, []byte("#!/bin/sh\nexit 1\n"), 0700); err != nil {
				t.Fatal(err)
			}
			if err := CmdDel(args); err != nil {
				t.Fatal(err)
			}
			if err := hostlocal.CmdCheck(args); err == nil {
				t.Fatal("recovered cleanup retained the address")
			}
		})
	}
}

func TestAddQuarantinesLeaseWhenInternetRollbackFails(t *testing.T) {
	if os.Getenv("WENDY_MESH_NAMESPACE_TEST") != "1" {
		t.Skip("disposable privileged container only")
	}
	netns, err := testutils.NewNS()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { netns.Close(); testutils.UnmountNS(netns) })
	bin := t.TempDir()
	firewall := filepath.Join(bin, "iptables")
	// Chain initialization/inspection succeeds until the first attempted grant;
	// both that write and subsequent rollback inspection fail.
	t.Setenv("WENDY_CNI_FAILURE_MARKER", filepath.Join(bin, "failed"))
	script := `#!/bin/sh
case " $* " in
*' -A WENDY-BRIDGE-INTERNET '*) touch "$WENDY_CNI_FAILURE_MARKER"; exit 4;;
*' -S WENDY-BRIDGE-INTERNET '*) if test -e "$WENDY_CNI_FAILURE_MARKER"; then exit 4; fi;;
esac
exit 0
`
	if err := os.WriteFile(firewall, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bin, "ip"), []byte("#!/bin/sh\necho '[]'\n"), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	cfg, err := json.Marshal(map[string]any{
		"cniVersion": "0.4.0", "name": "add-quarantine-test", "type": "bridge", "bridge": "qbridge0", "isDefaultGateway": true,
		"ipam": map[string]any{"type": "host-local", "subnet": "10.42.2.0/28", "dataDir": t.TempDir()},
	})
	if err != nil {
		t.Fatal(err)
	}
	args := &skel.CmdArgs{ContainerID: "failed-add-app", IfName: "eth0", Netns: netns.Path(), StdinData: cfg}
	t.Cleanup(func() {
		if link, err := netlink.LinkByName("qbridge0"); err == nil {
			_ = netlink.LinkDel(link)
		}
	})
	if err := CmdAdd(args); err == nil {
		t.Fatal("grant installation failure was hidden")
	}
	if err := hostlocal.CmdCheck(args); err != nil {
		t.Fatalf("failed ADD released a potentially granted address: %v", err)
	}
	if err := CmdDel(args); err == nil {
		t.Fatal("DEL hid failed firewall cleanup")
	}
	if err := hostlocal.CmdCheck(args); err != nil {
		t.Fatalf("DEL released quarantined lease: %v", err)
	}
	if err := netns.Do(func(_ ns.NetNS) error {
		if _, err := netlink.LinkByName("eth0"); err == nil {
			return fmt.Errorf("failed grant revocation prevented veth cleanup")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(firewall, []byte("#!/bin/sh\nexit 1\n"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := CmdDel(args); err != nil {
		t.Fatal(err)
	}
	if err := hostlocal.CmdCheck(args); err == nil {
		t.Fatal("recovered DEL retained the address")
	}
}

// The helper is a real separate CNI process, sharing only kernel state and the
// ownership journal with the agent. Its process-local forwarding map is empty.
func TestBridgeForwardingCNIProcess(t *testing.T) {
	phase := os.Getenv("WENDY_BRIDGE_FORWARDING_CHILD")
	if phase == "" {
		t.Skip("CNI subprocess")
	}
	if phase == "restart" {
		if err := hostnetwork.ReleaseForwarding("old-agent"); err != nil {
			t.Fatal(err)
		}
		return
	}
	data, err := os.ReadFile(os.Getenv("WENDY_BRIDGE_FORWARDING_ARGS"))
	if err != nil {
		t.Fatal(err)
	}
	var args skel.CmdArgs
	if err = json.Unmarshal(data, &args); err != nil {
		t.Fatal(err)
	}
	if phase == "add" {
		err = CmdAdd(&args)
	} else {
		err = CmdDel(&args)
	}
	if phase == "del-fail" {
		if err == nil {
			t.Fatal("failed firewall inspection was hidden")
		}
		if err := hostlocal.CmdCheck(&args); err != nil {
			t.Fatalf("failed revoke released quarantined address: %v", err)
		}
		return
	}
	if err != nil {
		t.Fatal(err)
	}
}

func TestInternetCNIProcessesRetainForwardingAcrossAgentLifetimes(t *testing.T) {
	if os.Getenv("WENDY_MESH_NAMESPACE_TEST") != "1" {
		t.Skip("disposable privileged container only")
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	child := func(t *testing.T, phase, file string) {
		t.Helper()
		cmd := exec.Command(exe, "-test.run=^TestBridgeForwardingCNIProcess$", "-test.v")
		cmd.Env = append(os.Environ(), "WENDY_BRIDGE_FORWARDING_CHILD="+phase, "WENDY_BRIDGE_FORWARDING_ARGS="+file)
		if phase == "del-fail" {
			bin := t.TempDir()
			if err := os.WriteFile(filepath.Join(bin, "iptables"), []byte("#!/bin/sh\nexit 4\n"), 0700); err != nil {
				t.Fatal(err)
			}
			cmd.Env = append(cmd.Env, "PATH="+bin+string(os.PathListSeparator)+os.Getenv("PATH"))
		}
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("CNI %s: %v\n%s", phase, err, out)
		}
	}
	value := func(want string) {
		t.Helper()
		b, err := os.ReadFile("/proc/sys/net/ipv4/ip_forward")
		if err != nil || strings.TrimSpace(string(b)) != want {
			t.Fatalf("forwarding=%q error=%v want %s", b, err, want)
		}
	}
	for _, scenario := range []string{"0-final-cni", "0-new-agent", "1-final-cni", "1-new-agent"} {
		baseline := scenario[:1]
		t.Run(scenario, func(t *testing.T) {
			if err := os.WriteFile("/proc/sys/net/ipv4/ip_forward", []byte(baseline+"\n"), 0600); err != nil {
				t.Fatal(err)
			}
			if err := hostnetwork.AcquireForwarding("mesh-cni-lifetime"); err != nil {
				t.Fatal(err)
			}
			var files []string
			ipamDir := t.TempDir()
			for i := 0; i < 2; i++ {
				n, err := testutils.NewNS()
				if err != nil {
					t.Fatal(err)
				}
				defer func() { n.Close(); testutils.UnmountNS(n) }()
				cfg, err := json.Marshal(map[string]any{"cniVersion": "0.4.0", "name": "cni-lifetime", "type": "bridge", "bridge": "clifetime0", "isDefaultGateway": true, "ipMasq": true, "ipam": map[string]any{"type": "host-local", "subnet": "10.42.8.0/28", "dataDir": ipamDir}})
				if err != nil {
					t.Fatal(err)
				}
				args := skel.CmdArgs{ContainerID: fmt.Sprintf("app-%s-%d", baseline, i), IfName: "eth0", Netns: n.Path(), StdinData: cfg}
				data, err := json.Marshal(args)
				if err != nil {
					t.Fatal(err)
				}
				file := filepath.Join(t.TempDir(), "args.json")
				if err := os.WriteFile(file, data, 0600); err != nil {
					t.Fatal(err)
				}
				files = append(files, file)
				child(t, "add", file)
			}
			defer func() {
				for _, file := range files {
					child(t, "del", file)
				}
				hostnetwork.ReleaseForwarding("mesh-cni-lifetime")
				hostnetwork.ReleaseForwarding("new-agent")
				if link, err := netlink.LinkByName("clifetime0"); err == nil {
					netlink.LinkDel(link)
				}
			}()
			if err := hostnetwork.ReleaseForwarding("mesh-cni-lifetime"); err != nil {
				t.Fatal(err)
			}
			value("1")
			child(t, "restart", "") // Fresh agent's empty map must preserve both CNI owners.
			value("1")
			child(t, "del-fail", files[0])
			value("1")
			child(t, "del", files[0])
			value("1")
			if strings.HasSuffix(scenario, "new-agent") {
				if err := hostnetwork.AcquireForwarding("new-agent"); err != nil {
					t.Fatal(err)
				}
				child(t, "del", files[1])
				value("1") // CNI must not restore beneath a newer agent.
				if err := hostnetwork.ReleaseForwarding("new-agent"); err != nil {
					t.Fatal(err)
				}
			} else {
				child(t, "del", files[1])
			}
			value(baseline)
			if scenario == "0-final-cni" {
				// Model restoration winning after CNI's early gateway enable
				// but before its grant publication enters the shared lock.
				if err := hostnetwork.EnsureBridgeInternetAccess("early-enable-race", "clifetime0", []net.IP{net.ParseIP("10.42.8.14")}); err != nil {
					t.Fatal(err)
				}
				value("1")
				if err := hostnetwork.RevokeBridgeInternetAccess("early-enable-race"); err != nil {
					t.Fatal(err)
				}
				value("0")
			}
		})
	}
}
