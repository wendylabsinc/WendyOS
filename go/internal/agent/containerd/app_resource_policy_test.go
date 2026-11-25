package containerd

import (
	"errors"
	"strings"
	"testing"

	specs "github.com/opencontainers/runtime-spec/specs-go"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"

	"github.com/wendylabsinc/wendy/go/internal/agent/appcgroup"
)

// legacyAppSpec is a spec saved before apps moved under the budget: the old
// placement and the score inherited from the containerd shim.
func legacyAppSpec(appID, serviceName string) *specs.Spec {
	inherited := -998
	return &specs.Spec{
		Linux:   &specs.Linux{CgroupsPath: appcgroup.ContainerPath(appID, serviceName, false)},
		Process: &specs.Process{OOMScoreAdj: &inherited},
	}
}

func TestRefreshAppResourcePolicyMigratesLegacySpec(t *testing.T) {
	spec := legacyAppSpec("com.example.app", "worker")
	changed, err := refreshAppResourcePolicy(spec, "com.example.app", "worker", true)
	if err != nil || !changed {
		t.Fatalf("changed %v error %v", changed, err)
	}
	if !strings.HasPrefix(spec.Linux.CgroupsPath, "/wendy-apps/system.slice:") || !strings.HasSuffix(spec.Linux.CgroupsPath, ":com.example.app@worker") {
		t.Fatalf("unexpected path %s", spec.Linux.CgroupsPath)
	}
	if spec.Process.OOMScoreAdj == nil || *spec.Process.OOMScoreAdj != 0 {
		t.Fatal("legacy inherited OOM score was not reset")
	}
	changed, err = refreshAppResourcePolicy(spec, "com.example.app", "worker", true)
	if err != nil || changed {
		t.Fatalf("unchanged spec = changed %v error %v", changed, err)
	}
	// A service and an app with a similar name must remain separate leaves.
	if appcgroup.ContainerPath("com.example.app", "worker", true) == appcgroup.ContainerPath("com.example.app-worker", "", true) {
		t.Fatal("cgroup identity collision")
	}
}

func TestRefreshAppResourcePolicyKeepsV1Placement(t *testing.T) {
	legacy := appcgroup.ContainerPath("com.example.app", "", false)
	spec := &specs.Spec{Linux: &specs.Linux{CgroupsPath: legacy}, Process: &specs.Process{}}
	changed, err := refreshAppResourcePolicy(spec, "com.example.app", "", false)
	if err != nil || !changed || spec.Linux.CgroupsPath != legacy || spec.Process.OOMScoreAdj == nil || *spec.Process.OOMScoreAdj != 0 {
		t.Fatalf("policy changed %v spec %+v err %v", changed, spec, err)
	}
	if _, err := refreshAppResourcePolicy(&specs.Spec{}, "com.example.app", "", true); err == nil {
		t.Fatal("incomplete spec must fail")
	}
}

func TestApplyAppResourcePolicyStartsAppsWhenTheBudgetFails(t *testing.T) {
	for _, tt := range []struct {
		name   string
		status appcgroup.Status
	}{
		{"cgroup write fails", appcgroup.Status{Unified: true, Err: errors.New("permission denied")}},
		{"no memory controller", appcgroup.Status{Unified: true, Note: "memory controller unavailable"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			core, logs := observer.New(zap.WarnLevel)
			c := &Client{logger: zap.New(core)}
			c.SetAppMemoryBudget(func() appcgroup.Status { return tt.status })

			spec := legacyAppSpec("com.example.app", "worker")
			if !c.applyAppResourcePolicy(spec, "com.example.app", "worker") {
				t.Fatal("legacy spec was not migrated")
			}
			if want := appcgroup.ContainerPath("com.example.app", "worker", true); spec.Linux.CgroupsPath != want {
				t.Fatalf("path %q, want %q", spec.Linux.CgroupsPath, want)
			}
			if *spec.Process.OOMScoreAdj != 0 {
				t.Fatalf("score %d, want 0", *spec.Process.OOMScoreAdj)
			}
			if logs.Len() != 1 {
				t.Fatalf("got %d warnings, want 1: %+v", logs.Len(), logs.All())
			}
			// An incomplete spec is logged, never a panic or a failed start.
			if c.applyAppResourcePolicy(&specs.Spec{}, "com.example.app", "worker") {
				t.Fatal("incomplete spec reported as changed")
			}
		})
	}
}

func TestApplyAppResourcePolicyLogsABudgetChangeOnce(t *testing.T) {
	core, logs := observer.New(zap.InfoLevel)
	c := &Client{logger: zap.New(core)}
	failing := false
	c.SetAppMemoryBudget(func() appcgroup.Status {
		if failing {
			// Each call yields a fresh error value, as a real write failure does.
			return appcgroup.Status{Unified: true, Err: errors.New("setting cgroup control: permission denied")}
		}
		return appcgroup.Status{Unified: true, Limited: true, Reserve: 1 << 30, Max: 7 << 30}
	})
	start := func() { c.applyAppResourcePolicy(legacyAppSpec("com.example.app", ""), "com.example.app", "") }

	start()
	start()
	failing = true
	start()
	start()
	failing = false
	start()

	var levels []zapcore.Level
	for _, entry := range logs.All() {
		levels = append(levels, entry.Level)
	}
	want := []zapcore.Level{zapcore.InfoLevel, zapcore.WarnLevel, zapcore.InfoLevel}
	if len(levels) != len(want) {
		t.Fatalf("logged %v, want %v: %+v", levels, want, logs.All())
	}
	for i := range want {
		if levels[i] != want[i] {
			t.Fatalf("logged %v, want %v", levels, want)
		}
	}
}

func TestEnsureAppMemoryBudgetWarnsAboutABadOverride(t *testing.T) {
	core, logs := observer.New(zap.InfoLevel)
	c := &Client{logger: zap.New(core)}
	c.SetAppMemoryBudget(func() appcgroup.Status {
		return appcgroup.Status{Unified: true, Limited: true, Reserve: 1 << 30, Max: 7 << 30,
			Note: "/etc/wendy-agent/host-memory-reserve: invalid; using the default reserve"}
	})
	c.EnsureAppMemoryBudget()
	if logs.Len() != 1 || logs.All()[0].Level != zapcore.WarnLevel || logs.All()[0].ContextMap()["note"] == nil {
		t.Fatalf("want one warning with the note, got %+v", logs.All())
	}
}

func TestApplyAppResourcePolicyWithoutBudgetUsesHostPlacement(t *testing.T) {
	core, logs := observer.New(zap.DebugLevel)
	c := &Client{logger: zap.New(core)}
	spec := legacyAppSpec("com.example.app", "")
	c.applyAppResourcePolicy(spec, "com.example.app", "")
	if want := appcgroup.ContainerPath("com.example.app", "", appcgroup.Unified()); spec.Linux.CgroupsPath != want {
		t.Fatalf("path %q, want %q", spec.Linux.CgroupsPath, want)
	}
	if logs.Len() != 0 {
		t.Fatalf("logged without a budget: %+v", logs.All())
	}
}

func TestWarnIfOutsideAppMemoryBudget(t *testing.T) {
	const outside = "0::/system.slice/wendy-agent.service\n"
	inside := "0::/" + appcgroup.Parent + "/system.slice:wendy:app\n"
	cases := []struct {
		name       string
		st         appcgroup.Status
		membership string
		wantWarn   bool
	}{
		{"limited and outside", appcgroup.Status{Unified: true, Limited: true, Max: 7 << 30}, outside, true},
		{"limited and inside", appcgroup.Status{Unified: true, Limited: true, Max: 7 << 30}, inside, false},
		{"not limited and outside", appcgroup.Status{Unified: true, Note: "no memory controller"}, outside, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			core, logs := observer.New(zap.InfoLevel)
			c := &Client{logger: zap.New(core)}
			c.SetAppMemoryBudget(func() appcgroup.Status { return tc.st })
			old := readProcCgroup
			readProcCgroup = func(string) ([]byte, error) { return []byte(tc.membership), nil }
			defer func() { readProcCgroup = old }()

			c.warnIfOutsideAppMemoryBudget("com.example.app", 42)

			n := 0
			for _, e := range logs.All() {
				if strings.Contains(e.Message, "outside the app memory budget") {
					n++
				}
			}
			if want := map[bool]int{true: 1, false: 0}[tc.wantWarn]; n != want {
				t.Fatalf("outside-budget warnings = %d, want %d: %+v", n, want, logs.All())
			}
		})
	}
}
