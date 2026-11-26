package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestOrganizationNameFreshness(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("WENDY_CONFIG_DIR", dir)
	SetOrganizationName("prod:443", "7", "Robotics")
	if got := FreshOrganizationName("prod:443", "7", time.Hour); got != "Robotics" {
		t.Fatalf("fresh persisted name = %q", got)
	}
	if FreshOrganizationName("other:443", "7", time.Hour) != "" || FreshOrganizationName("prod:443", "8", time.Hour) != "" {
		t.Fatal("freshness crossed an endpoint or organization boundary")
	}
	for _, tc := range []struct {
		name    string
		updated time.Time
	}{
		{"expired", time.Now().Add(-2 * time.Hour)},
		{"old cache", time.Time{}},
		{"clock moved backwards", time.Now().Add(time.Hour)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(dir, "organization-names.json")
			cache := readOrganizationNameCache(path)
			cache.UpdatedAt["prod:443"]["7"] = tc.updated
			data, err := json.Marshal(cache)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, data, 0600); err != nil {
				t.Fatal(err)
			}
			if FreshOrganizationName("prod:443", "7", time.Hour) != "" {
				t.Fatal("stale timestamp was considered fresh")
			}
			if OrganizationName("prod:443", "7") != "Robotics" {
				t.Fatal("stale name lost its offline fallback")
			}
		})
	}
}

func TestOrganizationLabelNamesFirstAndFallsBackToTheID(t *testing.T) {
	t.Setenv("WENDY_CONFIG_DIR", t.TempDir())
	tenant := "5b6b85fc-61bf-49a3-a7ec-eb8bfdde82bd"
	if got := OrganizationLabel("prod:443", tenant); got != tenant {
		t.Fatalf("unknown organization = %q, want the bare ID", got)
	}
	SetOrganizationName("prod:443", tenant, "Wendy Labs")
	if got := OrganizationLabel("prod:443", tenant); got != `"Wendy Labs" (`+tenant+")" {
		t.Fatalf("known organization = %q", got)
	}
	// Another endpoint's organization with the same key is not this one.
	if got := OrganizationLabel("dev:443", tenant); got != tenant {
		t.Fatalf("other endpoint = %q, want the bare ID", got)
	}
	// Without an endpoint, a name is used only when no cloud disagrees: legacy
	// numeric IDs repeat across clouds.
	if got := OrganizationLabel("", tenant); got != `"Wendy Labs" (`+tenant+")" {
		t.Fatalf("any endpoint, one name = %q", got)
	}
	SetOrganizationName("prod:443", "7", "Robotics")
	SetOrganizationName("dev:443", "7", "Sandbox")
	if got := OrganizationLabel("", "7"); got != "7" {
		t.Fatalf("any endpoint, conflicting names = %q, want the bare ID", got)
	}
	if got := OrganizationLabel("dev:443", "7"); got != `"Sandbox" (7)` {
		t.Fatalf("scoped lookup = %q", got)
	}
	// A name cannot pass itself off as another organization's label, or reach
	// the terminal as a control sequence.
	SetOrganizationName("prod:443", "8", "Acme\" (9)\n\x1b[2J")
	if got := OrganizationLabel("prod:443", "8"); got != `"Acme\" (9)\n\x1b[2J" (8)` {
		t.Fatalf("hostile name = %q", got)
	}
}
