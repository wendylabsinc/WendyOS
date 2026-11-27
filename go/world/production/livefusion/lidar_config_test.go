package livefusion

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/world/internal/world"
)

func lidarConfigFixture() Config {
	tenant := "11111111-1111-4111-8111-111111111111"
	c := Config{Version: 1, Tenant: tenant, Site: "office", Device: "spiffe://wendy.sh/tenant/" + tenant + "/device/22222222-2222-4222-8222-222222222222", App: "com.example.fusion", Service: "fusion", RootsDER: [][]byte{[]byte("public-root-shape-only")}, RunID: "run", SceneBootID: "scene-boot", PackageSHA256: strings.Repeat("a", 64), SimulationAnchorUnixNS: time.Unix(1700000000, 0).UnixNano(), Frame: world.FrameRef{ID: "floor", Revision: "v1"}, ScenarioVersion: "mujoco-live-v1", ResultStream: "hazards", DurationMS: 10000}
	for _, id := range []string{"camera-a", "camera-b", "gas-1", "audio-1"} {
		c.Sources = append(c.Sources, Source{SensorID: id, App: c.App, Service: "reader-" + id})
	}
	return c
}

func TestLidarConfigKeepsFourSourceWorkflowAndOptionalScans(t *testing.T) {
	for _, total := range []int{4, 5, 8} {
		c := lidarConfigFixture()
		for i := 4; i < total; i++ {
			id := fmt.Sprintf("lidar-%d", i)
			c.Sources = append(c.Sources, Source{SensorID: id, App: c.App, Service: id})
		}
		got, err := c.normalized()
		if err != nil || len(got.Sources) != total || got.MaxSkewMS != 350 {
			t.Fatalf("%d source configuration: %v", total, err)
		}
		c.Sources[0].Service = "changed"
		if got.Sources[0].Service == "changed" {
			t.Fatal("normalized sources alias caller")
		}
	}
}

func TestLidarConfigRefusesMissingBaselineAndAmbiguousSources(t *testing.T) {
	for name, change := range map[string]func(*Config){
		"missing audio": func(c *Config) { c.Sources[3] = Source{SensorID: "lidar-1", App: c.App, Service: "lidar-1"} },
		"unknown": func(c *Config) {
			c.Sources = append(c.Sources, Source{SensorID: "radar-1", App: c.App, Service: "radar"})
		},
		"empty suffix": func(c *Config) {
			c.Sources = append(c.Sources, Source{SensorID: "lidar-", App: c.App, Service: "lidar"})
		},
		"invalid identifier": func(c *Config) {
			c.Sources = append(c.Sources, Source{SensorID: "lidar-a/b", App: c.App, Service: "lidar"})
		},
		"duplicate sensor": func(c *Config) {
			c.Sources = append(c.Sources, Source{SensorID: "camera-a", App: c.App, Service: "other"})
		},
		"duplicate service": func(c *Config) {
			c.Sources = append(c.Sources, Source{SensorID: "lidar-1", App: c.App, Service: c.Sources[0].Service})
		},
		"result service": func(c *Config) {
			c.Sources = append(c.Sources, Source{SensorID: "lidar-1", App: c.App, Service: c.Service})
		},
		"over source bound": func(c *Config) {
			for i := 4; i < 9; i++ {
				id := fmt.Sprintf("lidar-%d", i)
				c.Sources = append(c.Sources, Source{SensorID: id, App: c.App, Service: id})
			}
		},
	} {
		t.Run(name, func(t *testing.T) {
			c := lidarConfigFixture()
			change(&c)
			if _, err := c.normalized(); err == nil {
				t.Fatal("accepted invalid source set")
			}
		})
	}
}
