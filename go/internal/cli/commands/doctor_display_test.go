package commands

import (
	"bytes"
	"strings"
	"testing"
)

func TestDoctorDisplayGroupsActionBeforeHealthyChecks(t *testing.T) {
	report := doctorReport{OK: false, Checks: []doctorCheck{
		{ID: "docker_executable", Status: "pass"},
		{ID: "project", Status: "unknown", Message: "No project recognized.", Remediation: "Run from a project or pass --project."},
		{ID: "docker_buildx", Status: "fail", Required: true, Message: "Buildx is unavailable.", Remediation: "Install Buildx."},
		{ID: "authentication", Status: "unknown", Code: "auth_not_checked"},
	}}
	var b bytes.Buffer
	renderDoctorReport(&b, report)
	got := b.String()
	for _, text := range []string{"Setup needs attention", "Needs attention\n  Docker Buildx", "    Install Buildx.", "Ready\n  OK  Docker CLI", "Not checked\n  Project"} {
		if !strings.Contains(got, text) {
			t.Errorf("missing %q in %s", text, got)
		}
	}
	if strings.Index(got, "Needs attention") > strings.Index(got, "Ready") {
		t.Fatal("action must come first")
	}
	if strings.Contains(got, "optional]") || strings.Contains(got, "docker_buildx") || strings.Contains(got, "Authentication") {
		t.Fatal("internal labels or duplicate missing-target advice escaped")
	}
}

func TestDoctorDisplayDoesNotClaimUnobservedReadiness(t *testing.T) {
	var b bytes.Buffer
	renderDoctorReport(&b, doctorReport{OK: true, Checks: []doctorCheck{{ID: "project", Status: "unknown", Message: "No project selected."}}})
	if !strings.Contains(b.String(), "in the checks performed") || !strings.Contains(b.String(), "Not checked") || strings.Contains(b.String(), "\nReady") {
		t.Fatal(b.String())
	}
}
