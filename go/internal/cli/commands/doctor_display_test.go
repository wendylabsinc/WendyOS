package commands

import (
	"bytes"
	"context"
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

// Every check doctor emits needs a readable label; the underscore fallback
// prints internal IDs such as "wendy agent".
func TestDoctorDisplayLabelsEveryEmittedCheck(t *testing.T) {
	d := doctorTestDeps(t)
	d.probe = func(context.Context, string) doctorTargetEvidence {
		return doctorTargetEvidence{Reachable: true, AgentResponding: true}
	}
	report, _, _ := doctorExecute(t, d, "--device", "192.168.1.20")
	if doctorFindCheck(t, report, "wendy_agent").Status != "pass" {
		t.Fatalf("agent check missing from %+v", report.Checks)
	}
	for _, check := range report.Checks {
		if label := doctorCheckLabel(check.ID); label == strings.ReplaceAll(check.ID, "_", " ") {
			t.Errorf("check %q has no label; doctor prints %q", check.ID, label)
		}
	}
	if got := doctorCheckLabel("wendy_agent"); got != "Wendy Agent" {
		t.Errorf("agent label = %q", got)
	}
}
