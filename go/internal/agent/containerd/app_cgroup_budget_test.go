package containerd

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	specs "github.com/opencontainers/runtime-spec/specs-go"
)

func TestAppMemoryBudget(t *testing.T) {
	const gib = uint64(1 << 30)
	for _, tt := range []struct {
		name      string
		ram, want uint64
	}{
		{"eight GiB board", 8 * gib, 6 * gib},
		{"large board reserves twenty percent", 40 * gib, 32 * gib},
		{"four GiB board", 4 * gib, 2 * gib},
		{"small board reserves half", gib, gib / 2},
	} {
		t.Run(tt.name, func(t *testing.T) {
			high, max, err := appMemoryBudget(tt.ram)
			if err != nil || max != tt.want || high != max-max/10 || high >= max {
				t.Fatalf("budget = high %d max %d err %v; want max %d", high, max, err, tt.want)
			}
		})
	}
	if _, _, err := appMemoryBudget(0); err == nil {
		t.Fatal("zero RAM must fail")
	}
}

func TestHostMemoryTotal(t *testing.T) {
	total, err := hostMemoryTotal([]byte("MemFree: 1 kB\nMemTotal:       8000000 kB\n"))
	if err != nil || total != 8000000*1024 {
		t.Fatalf("total %d error %v", total, err)
	}
	for _, invalid := range []string{"", "MemTotal: 0 kB", "MemTotal: -1 kB", "MemTotal: 5 MB", "MemTotal: 18446744073709551615 kB"} {
		if _, err := hostMemoryTotal([]byte(invalid)); err == nil {
			t.Fatalf("accepted invalid memory total %q", invalid)
		}
	}
}

func appCgroupFixture(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	parent := filepath.Join(root, appCgroupParent)
	if err := os.Mkdir(parent, 0755); err != nil {
		t.Fatal(err)
	}
	for _, dir := range []string{root, parent} {
		for name, value := range map[string]string{"cgroup.controllers": "cpu memory pids", "cgroup.subtree_control": "cpu"} {
			if err := os.WriteFile(filepath.Join(dir, name), []byte(value), 0600); err != nil {
				t.Fatal(err)
			}
		}
	}
	for _, name := range []string{"memory.high", "memory.max", "memory.swap.max"} {
		if err := os.WriteFile(filepath.Join(parent, name), []byte("max"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

func TestConfigureAppMemoryBudget(t *testing.T) {
	root := appCgroupFixture(t)
	if err := configureAppMemoryBudget(root, 8<<30); err != nil {
		t.Fatal(err)
	}
	for path, want := range map[string]string{
		"cgroup.subtree_control":            "+memory",
		"wendy-apps/cgroup.subtree_control": "+memory",
		"wendy-apps/memory.high":            "5798205850",
		"wendy-apps/memory.max":             "6442450944",
		"wendy-apps/memory.swap.max":        "0",
	} {
		got, err := os.ReadFile(filepath.Join(root, path))
		if err != nil || string(got) != want {
			t.Errorf("%s = %q (%v), want %s", path, got, err, want)
		}
	}
	// A second start leaves the existing budget in place.
	if err := configureAppMemoryBudget(root, 8<<30); err != nil {
		t.Fatal(err)
	}
}

func TestConfigureAppMemoryBudgetFailsClosed(t *testing.T) {
	for _, name := range []string{"cgroup.controllers", "wendy-apps/memory.max", "wendy-apps/memory.swap.max", "wendy-apps/cgroup.subtree_control"} {
		t.Run(name, func(t *testing.T) {
			root := appCgroupFixture(t)
			if err := os.Remove(filepath.Join(root, name)); err != nil {
				t.Fatal(err)
			}
			if err := configureAppMemoryBudget(root, 8<<30); err == nil {
				t.Fatal("missing control file must fail")
			}
			if _, err := os.Stat(filepath.Join(root, name)); !os.IsNotExist(err) {
				t.Fatal("missing control file was recreated")
			}
		})
	}
	t.Run("memory controller unavailable", func(t *testing.T) {
		root := appCgroupFixture(t)
		if err := os.WriteFile(filepath.Join(root, "cgroup.controllers"), []byte("cpu pids"), 0600); err != nil {
			t.Fatal(err)
		}
		if err := configureAppMemoryBudget(root, 8<<30); err == nil {
			t.Fatal("unavailable memory controller must fail")
		}
	})
}

func TestRefreshAppResourcePolicyMigratesLegacySpec(t *testing.T) {
	inherited := -998
	spec := &specs.Spec{Linux: &specs.Linux{CgroupsPath: appCgroupPath("com.example.app", "worker", false)}, Process: &specs.Process{OOMScoreAdj: &inherited}}
	changed, err := refreshAppResourcePolicy(spec, "com.example.app", "worker", true)
	if err != nil || !changed {
		t.Fatalf("changed %v error %v", changed, err)
	}
	if !strings.HasPrefix(spec.Linux.CgroupsPath, "/wendy-apps/system.slice:") || !strings.HasSuffix(spec.Linux.CgroupsPath, ":com.example.app@worker") {
		t.Fatalf("unexpected path %s", spec.Linux.CgroupsPath)
	}
	if spec.Process.OOMScoreAdj == nil || *spec.Process.OOMScoreAdj != 0 {
		t.Fatal("legacy protected OOM score was not reset")
	}
	changed, err = refreshAppResourcePolicy(spec, "com.example.app", "worker", true)
	if err != nil || changed {
		t.Fatalf("unchanged spec = changed %v error %v", changed, err)
	}
	// A service and an app with a similar name must remain separate leaves.
	if appCgroupPath("com.example.app", "worker", true) == appCgroupPath("com.example.app-worker", "", true) {
		t.Fatal("cgroup identity collision")
	}
}

func TestRefreshAppResourcePolicyPreservesV1Placement(t *testing.T) {
	legacy := appCgroupPath("com.example.app", "", false)
	spec := &specs.Spec{Linux: &specs.Linux{CgroupsPath: legacy}, Process: &specs.Process{}}
	changed, err := refreshAppResourcePolicy(spec, "com.example.app", "", false)
	if err != nil || !changed || spec.Linux.CgroupsPath != legacy || spec.Process.OOMScoreAdj == nil || *spec.Process.OOMScoreAdj != 0 {
		t.Fatalf("policy changed %v spec %+v err %v", changed, spec, err)
	}
	if _, err := refreshAppResourcePolicy(&specs.Spec{}, "com.example.app", "", true); err == nil {
		t.Fatal("incomplete spec must fail")
	}
}

func TestAppTaskHasBudgetCgroup(t *testing.T) {
	for _, tt := range []struct {
		membership string
		want       bool
	}{
		{"0::/wendy-apps/system.slice:edge-agent:com.example.app\n", true},
		{"0::/wendy-apps/system.slice:edge-agent:com.example.app/child\n", true},
		{"0::/system.slice:edge-agent:com.example.app\n", false},
		{"0::/wendy-apps-other/system.slice:edge-agent:com.example.app\n", false},
		{"0::/wendy-apps\n", false},
		{"1:memory:/wendy-apps/system.slice:edge-agent:com.example.app\n", false},
	} {
		if got := appTaskHasBudgetCgroup(tt.membership); got != tt.want {
			t.Errorf("membership %q covered %v, want %v", tt.membership, got, tt.want)
		}
	}
}
