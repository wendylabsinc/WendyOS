package config

import "testing"

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
