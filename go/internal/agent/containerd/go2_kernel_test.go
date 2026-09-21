package containerd

import (
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/containerd/containerd/v2/pkg/oci"
	specs "github.com/opencontainers/runtime-spec/specs-go"
	"github.com/wendylabsinc/wendy/go/internal/shared/appconfig"
)

func go2KernelFixture(t *testing.T) (map[string]string, *oci.Spec, go2KernelDeps, *[][]string) {
	t.Helper()
	labels := wendyLabels(go2RuntimeAppID, "", "0.1.0-"+strings.Repeat("a", 12), nil,
		[]appconfig.Entitlement{{Type: appconfig.EntitlementNetwork, Mode: "host-admin"}}, "", nil)
	spec := &oci.Spec{
		Process: &specs.Process{Env: []string{"GO2_VM_NAME=robot", "GO2_SOURCE_DIGEST=sha256:" + strings.Repeat("a", 64)},
			Capabilities: &specs.LinuxCapabilities{Bounding: []string{"CAP_NET_ADMIN"}, Effective: []string{"CAP_NET_ADMIN"}, Permitted: []string{"CAP_NET_ADMIN"}}},
		Linux: &specs.Linux{},
	}
	var calls [][]string
	deps := go2KernelDeps{
		goos: "linux",
		readFile: func(path string) ([]byte, error) {
			switch path {
			case "/etc/wendyos/device-type":
				return []byte("BOARD=vm-arm64\nMACHINE=vm-arm64-wendyos\nSTORAGE=disk\n"), nil
			case "/proc/config.gz":
				return go2KernelGzip(t, "# CONFIG_NF_TABLES is not set\nCONFIG_IP6_NF_FILTER=m\n"), nil
			default:
				t.Fatalf("unexpected host read: %s", path)
				return nil, os.ErrNotExist
			}
		},
		modprobe: func(ctx context.Context, modules []string) error {
			deadline, ok := ctx.Deadline()
			if !ok || time.Until(deadline) > 30*time.Second {
				t.Fatal("module loading was not bounded")
			}
			calls = append(calls, append([]string(nil), modules...))
			return nil
		},
	}
	return labels, spec, deps, &calls
}

func go2KernelGzip(t *testing.T, config string) []byte {
	t.Helper()
	var data bytes.Buffer
	writer := gzip.NewWriter(&data)
	if _, err := writer.Write([]byte(config)); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	return data.Bytes()
}

func TestGo2KernelLegacyPreloadUsesOnlyFixedModulesOnEveryStart(t *testing.T) {
	labels, spec, deps, calls := go2KernelFixture(t)
	for range 2 {
		if err := prepareGo2KernelModules(context.Background(), labels, spec, deps); err != nil {
			t.Fatal(err)
		}
	}
	want := []string{"ip_tables", "iptable_filter", "ip6_tables", "ip6table_filter", "xt_string", "xt_comment", "xt_tcpudp"}
	if !reflect.DeepEqual(*calls, [][]string{want, want}) {
		t.Fatalf("start/reboot module preparation = %v", *calls)
	}
}

func TestGo2KernelUnmanagedContainersNeverProbeTheHost(t *testing.T) {
	for _, scenario := range []string{"other app", "service", "unmanaged", "other platform"} {
		t.Run(scenario, func(t *testing.T) {
			labels, spec, deps, calls := go2KernelFixture(t)
			switch scenario {
			case "other app":
				labels[labelKeyAppID] = "example.app"
			case "service":
				labels[labelKeyServiceName] = "other"
			case "unmanaged":
				spec.Process.Env = nil
			case "other platform":
				deps.goos = "darwin"
			}
			deps.readFile = func(string) ([]byte, error) { t.Fatal("unrelated app probed the host"); return nil, nil }
			if err := prepareGo2KernelModules(context.Background(), labels, spec, deps); err != nil || len(*calls) != 0 {
				t.Fatalf("unrelated app prepared host modules: %v %v", err, *calls)
			}
		})
	}
}

func TestGo2KernelRejectsInvalidIdentityAndInsufficientEntitlement(t *testing.T) {
	for _, scenario := range []string{"VM name", "source", "version", "host visibility only", "private namespace", "capability", "non-VM host", "missing identity", "broken config"} {
		t.Run(scenario, func(t *testing.T) {
			labels, spec, deps, calls := go2KernelFixture(t)
			switch scenario {
			case "VM name":
				spec.Process.Env = append(spec.Process.Env, "GO2_VM_NAME=../physical")
			case "source":
				spec.Process.Env = append(spec.Process.Env, "GO2_SOURCE_DIGEST=untrusted")
			case "version":
				labels[labelKeyAppVersion] = "0.1.0"
			case "host visibility only":
				for key, value := range appconfig.BuildEntitlementAnnotations([]appconfig.Entitlement{{Type: appconfig.EntitlementNetwork, Mode: "host"}}) {
					labels[key] = value
				}
			case "private namespace":
				spec.Linux.Namespaces = []specs.LinuxNamespace{{Type: specs.NetworkNamespace}}
			case "capability":
				spec.Process.Capabilities.Effective = nil
			case "non-VM host":
				deps.readFile = func(string) ([]byte, error) { return []byte("BOARD=jetson-orin-nano\nMACHINE=vm-arm64-wendyos\n"), nil }
			case "missing identity":
				deps.readFile = func(string) ([]byte, error) { return nil, os.ErrNotExist }
			case "broken config":
				read := deps.readFile
				deps.readFile = func(path string) ([]byte, error) {
					if path == "/proc/config.gz" {
						return []byte("not gzip"), nil
					}
					return read(path)
				}
			}
			if err := prepareGo2KernelModules(context.Background(), labels, spec, deps); err == nil || len(*calls) != 0 {
				t.Fatalf("invalid managed preparation was accepted: %v %v", err, *calls)
			}
		})
	}
}

func TestGo2KernelNFTSupportDoesNotLoadLegacyModules(t *testing.T) {
	for _, config := range []string{"CONFIG_NF_TABLES=y\n", "CONFIG_NF_TABLES=m\n"} {
		labels, spec, deps, calls := go2KernelFixture(t)
		read := deps.readFile
		deps.readFile = func(path string) ([]byte, error) {
			if path == "/proc/config.gz" {
				return go2KernelGzip(t, config), nil
			}
			return read(path)
		}
		if err := prepareGo2KernelModules(context.Background(), labels, spec, deps); err != nil || len(*calls) != 0 {
			t.Fatalf("nft-capable kernel prepared legacy modules: %v %v", err, *calls)
		}
	}
}

func TestGo2KernelMissingModuleIsActionableAndCannotBeIgnored(t *testing.T) {
	labels, spec, deps, _ := go2KernelFixture(t)
	expected := errors.New("modprobe: FATAL: Module ip6table_filter not found")
	deps.modprobe = func(context.Context, []string) error { return expected }
	err := prepareGo2KernelModules(context.Background(), labels, spec, deps)
	if !errors.Is(err, expected) || !strings.Contains(err.Error(), "VM image must include its kernel modules") {
		t.Fatalf("missing IPv6 support was suppressed: %v", err)
	}
}

func TestGo2HostIdentityUsesAuthoritativeBoardBeforeMachine(t *testing.T) {
	for content, want := range map[string]bool{
		"vm-arm64\n": true, "BOARD=vm-arm64\n": true, "MACHINE=vm-arm64-wendyos\n": true,
		"BOARD=raspberrypi5\nMACHINE=vm-arm64-wendyos\n": false,
		"BOARD=unitree-go2\n":                            false, "vm-arm64-untrusted": false, "": false,
		"BOARD=unitree-g1\nMACHINE=vm-arm64-wendyos\n": false,
	} {
		if got := go2HostIsVM(content); got != want {
			t.Fatalf("host identity %q: got %t, want %t", content, got, want)
		}
	}
}

func TestG1KernelPreparationPinsRobotIdentity(t *testing.T) {
	for _, scenario := range []string{"valid", "wrong app", "mixed identity", "missing VM", "missing source", "version", "physical G1", "physical Go2"} {
		t.Run(scenario, func(t *testing.T) {
			labels, spec, deps, calls := go2KernelFixture(t)
			labels[labelKeyAppID] = g1RuntimeAppID
			for i, value := range spec.Process.Env {
				spec.Process.Env[i] = strings.Replace(value, "GO2_", "G1_", 1)
			}
			switch scenario {
			case "wrong app":
				labels[labelKeyAppID] = go2RuntimeAppID
			case "mixed identity":
				spec.Process.Env = append(spec.Process.Env, "GO2_VM_NAME=another-robot")
			case "missing VM":
				spec.Process.Env = append(spec.Process.Env, "G1_VM_NAME=")
			case "missing source":
				spec.Process.Env = append(spec.Process.Env, "G1_SOURCE_DIGEST=")
			case "version":
				labels[labelKeyAppVersion] = "0.1.0-" + strings.Repeat("b", 12)
			case "physical G1", "physical Go2":
				board := "unitree-g1"
				if scenario == "physical Go2" {
					board = "unitree-go2"
				}
				deps.readFile = func(string) ([]byte, error) {
					return []byte("BOARD=" + board + "\nMACHINE=vm-arm64-wendyos\n"), nil
				}
			}
			err := prepareGo2KernelModules(context.Background(), labels, spec, deps)
			if scenario == "valid" {
				if err != nil || len(*calls) != 1 || !reflect.DeepEqual((*calls)[0], go2LegacyFirewallModules) {
					t.Fatalf("G1 VM did not prepare the fixed firewall modules: %v, %v", err, *calls)
				}
			} else if err == nil || len(*calls) != 0 {
				t.Fatalf("invalid G1 identity prepared host modules: %v, %v", err, *calls)
			}
		})
	}
}
