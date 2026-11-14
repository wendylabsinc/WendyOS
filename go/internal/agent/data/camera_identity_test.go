package data

import (
	"context"
	"strings"
	"testing"
)

func TestCameraSelectorStableIdentityAndLegacyAlias(t *testing.T) {
	sources := []Source{{ID: "sensorlink:one:0", Kind: "camera", Healthy: true, Aliases: []string{"v4l2:/dev/video96"}}, {ID: "sensorlink:two:0", Kind: "camera", Healthy: true, Aliases: []string{"v4l2:/dev/video97"}}}
	for _, selector := range []string{"sensorlink:one:0", "v4l2:/dev/video96", "/dev/video96"} {
		id, err := resolveCameraSelector(sources, selector)
		if err != nil || id != "sensorlink:one:0" {
			t.Fatalf("%s: %s %v", selector, id, err)
		}
	}
	sources[0].Aliases, sources[1].Aliases = sources[1].Aliases, sources[0].Aliases
	if id, err := resolveCameraSelector(sources, "sensorlink:one:0"); err != nil || id != sources[0].ID {
		t.Fatalf("identity followed slot: %s %v", id, err)
	}
	sources[0].Healthy = false
	sources[1].Detail = "sensorlink:one:0"
	if _, err := resolveCameraSelector(sources, "sensorlink:one:0"); err == nil {
		t.Fatal("missing stable source fell back to display name")
	}
}

func TestCameraSelectorLegacyWarningRequiresVerifiedMigration(t *testing.T) {
	m, err := NewManager(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	m.SetSourceProvider(func(context.Context) []Source {
		return []Source{{ID: "sensorlink:one:0", Kind: "camera", Healthy: true, Aliases: []string{"v4l2:/dev/video96"}}}
	})
	campaign := Campaign{Sources: []CampaignSource{{Camera: "v4l2:/dev/video96"}}}
	warnings := m.legacyCameraSelectorWarnings(campaign)
	if len(warnings) != 1 || !strings.Contains(warnings[0], "verify the physical camera") || !strings.Contains(warnings[0], "sensorlink:one:0") {
		t.Fatalf("warnings: %v", warnings)
	}
	if campaign.Sources[0].Camera != "v4l2:/dev/video96" {
		t.Fatal("silently migrated ambiguous legacy selector")
	}
	campaign.Sources[0].Camera = "sensorlink:one:0"
	if warnings := m.legacyCameraSelectorWarnings(campaign); len(warnings) != 0 {
		t.Fatalf("stable ID warning: %v", warnings)
	}
}
