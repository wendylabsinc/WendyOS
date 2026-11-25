package gateway

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wendylabsinc/wendy/go/world/internal/mission"
	"github.com/wendylabsinc/wendy/go/world/internal/tasking"
	"github.com/wendylabsinc/wendy/go/world/internal/world"
)

func go2ConfigurationFixture(t *testing.T) config {
	t.Helper()
	d := strings.Repeat("a", 64)
	b := tasking.Go2Binding{Schema: "wendy.world.go2-execution.v1", PackageSHA256: d, ConversionSHA256: d, LossSHA256: d,
		ProjectRevision: d, ProjectID: strings.Repeat("b", 32), VMName: "explicit-vm", SourceDigest: "sha256:" + d,
		PolicyBundle: "go2-moe-cts-164k-0.6715-v1", WorldEpoch: 2, RunID: "run", Frame: world.FrameRef{ID: "floor", Revision: "frame1"}, AlignmentSHA256: d, ProfileSHA256: d}
	return config{AllowInsecureSimulation: true, Listen: "127.0.0.1:0", DataPath: filepath.Join(t.TempDir(), "world.json"), Tenant: "lab", Site: "office", Audience: "node", RootPublicKey: make([]byte, 32), ClockDomain: "simulation:run", Frames: []world.FrameRef{b.Frame}, Tasks: &taskConfig{Go2Binding: &b, Assignee: "robot/navigation", ExecutorPrincipal: "executor", Catalog: map[string]mission.SafetyClass{tasking.Go2GoToType: mission.Move}, MaxLeaseMS: 500, MaxTelemetryAgeMS: 250, MaxTasks: 10}}
}

func TestGo2ConfigurationMatchesExplicitFrameRunAndNoSupervision(t *testing.T) {
	if e := validateGo2TaskConfiguration(go2ConfigurationFixture(t)); e != nil {
		t.Fatal("valid bound configuration refused", e)
	}
	cases := map[string]func(*config){
		"missing binding":         func(c *config) { c.Tasks.Go2Binding = nil },
		"different run":           func(c *config) { c.ClockDomain = "simulation:another-run" },
		"different frame":         func(c *config) { c.Frames[0].Revision = "other" },
		"no frame":                func(c *config) { c.Frames = nil },
		"supervision":             func(c *config) { c.Tasks.EnableSupervision = true },
		"mixed one-axis":          func(c *config) { c.Tasks.Catalog["wendy.nav.go_to.v1"] = mission.Move },
		"orphan binding":          func(c *config) { c.Tasks.Catalog = map[string]mission.SafetyClass{"wendy.nav.go_to.v1": mission.Move} },
		"stale telemetry":         func(c *config) { c.Tasks.MaxTelemetryAgeMS = 251 },
		"unsigned binding schema": func(c *config) { c.Tasks.Go2Binding.Schema = "wendy.world.go2-project-binding.v1" },
		"unbound profile":         func(c *config) { c.Tasks.Go2Binding.ProfileSHA256 = "" },
	}
	for name, change := range cases {
		t.Run(name, func(t *testing.T) {
			c := go2ConfigurationFixture(t)
			change(&c)
			if e := validateGo2TaskConfiguration(c); !errors.Is(e, errGo2Binding) {
				t.Fatalf("got %v", e)
			}
		})
	}
}

func TestGo2InvalidRunConfigurationRefusedBeforeOpeningStores(t *testing.T) {
	cases := []struct {
		name   string
		change func(*config)
		want   error
	}{
		{"physical-clock", func(c *config) { c.ClockDomain = "physical" }, errSimulationClock},
		{"wrong-safety-class", func(c *config) { c.Tasks.Catalog[tasking.Go2GoToType] = mission.Observe }, errUnsupportedTaskCatalog},
		{"wrong-type", func(c *config) {
			c.Tasks.Catalog = map[string]mission.SafetyClass{"wendy.go2.nav.missing.v1": mission.Move}
		}, errUnsupportedTaskCatalog},
		{"missing-binding", func(c *config) { c.Tasks.Go2Binding = nil }, errGo2Binding},
		{"run-mismatch", func(c *config) { c.Tasks.Go2Binding.RunID = "other" }, errGo2Binding},
		{"frame-mismatch", func(c *config) { c.Tasks.Go2Binding.Frame.Revision = "other" }, errGo2Binding},
		{"supervision-enabled", func(c *config) { c.Tasks.EnableSupervision = true }, errGo2Binding},
		{"orphan-binding", func(c *config) { c.Tasks.Catalog = map[string]mission.SafetyClass{"wendy.nav.go_to.v1": mission.Move} }, errGo2Binding},
		{"default-telemetry-too-old", func(c *config) { c.Tasks.MaxTelemetryAgeMS = 0 }, errGo2Binding},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := go2ConfigurationFixture(t)
			tc.change(&c)
			path := filepath.Join(t.TempDir(), "config.json")
			body, e := json.Marshal(c)
			if e != nil {
				t.Fatal(e)
			}
			if e = os.WriteFile(path, body, 0600); e != nil {
				t.Fatal(e)
			}
			if e = run(path); !errors.Is(e, tc.want) {
				t.Fatalf("got %v, expected %v", e, tc.want)
			}
			entries, e := os.ReadDir(filepath.Dir(c.DataPath))
			if e != nil {
				t.Fatal(e)
			}
			if len(entries) != 0 {
				t.Fatal("invalid configuration opened storage before refusal")
			}
		})
	}
}

func TestGo2AbsentLeavesExistingCatalogConfigurationUnchanged(t *testing.T) {
	c := go2ConfigurationFixture(t)
	c.Tasks.Go2Binding = nil
	c.Tasks.Catalog = map[string]mission.SafetyClass{"wendy.nav.go_to.v1": mission.Move, tasking.InspectType: mission.Move}
	c.Tasks.EnableSupervision = true
	c.Tasks.MaxTelemetryAgeMS = 500
	if e := validateGo2TaskConfiguration(c); e != nil {
		t.Fatal("Go2 checks changed existing one-axis catalog policy", e)
	}
	c.Tasks = nil
	if e := validateGo2TaskConfiguration(c); e != nil {
		t.Fatal("reader-only configuration affected", e)
	}
}
