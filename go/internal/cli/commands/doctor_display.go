package commands

import (
	"fmt"
	"io"
	"strings"
)

func renderDoctorReport(w io.Writer, report doctorReport) {
	fmt.Fprintln(w, "Wendy setup")
	if report.OK {
		fmt.Fprintln(w, "No blocking issues found in the checks performed.")
	} else {
		fmt.Fprintln(w, "Setup needs attention before you can continue.")
	}
	for _, group := range []struct{ title, kind string }{
		{"Needs attention", "required"}, {"Optional tools", "optional"},
		{"Ready", "ready"}, {"Not checked", "unknown"},
	} {
		shown := false
		for _, check := range report.Checks {
			kind := "unknown"
			switch check.Status {
			case "pass":
				kind = "ready"
			case "not_required":
				continue
			case "fail":
				kind = "optional"
				if check.Required {
					kind = "required"
				}
			}
			if kind != group.kind {
				continue
			}
			// Missing target context already explains why authentication was skipped.
			if check.ID == "authentication" && check.Code == "auth_not_checked" {
				continue
			}
			if !shown {
				fmt.Fprintf(w, "\n%s\n", group.title)
				shown = true
			}
			label := doctorCheckLabel(check.ID)
			if kind == "ready" {
				fmt.Fprintf(w, "  OK  %s\n", label)
				continue
			}
			fmt.Fprintf(w, "  %s: %s\n", label, check.Message)
			if check.Remediation != "" {
				fmt.Fprintf(w, "    %s\n", check.Remediation)
			}
		}
	}
	fmt.Fprintln(w, "\nNothing changed or uploaded. JSON report: wendy doctor --report")
}

func doctorCheckLabel(id string) string {
	labels := map[string]string{
		"cli_config": "CLI configuration", "project": "Project", "docker_executable": "Docker CLI",
		"docker_daemon": "Docker engine", "docker_buildx": "Docker Buildx", "qemu_executable": "QEMU",
		"qemu_firmware": "ARM64 VM firmware", "selected_target": "Selected device",
		"device_reachable": "Device connection", "wendy_agent": "Wendy Agent", "authentication": "Authentication",
		"diagnostics": "Diagnostic checks",
	}
	if label, ok := labels[id]; ok {
		return label
	}
	return strings.ReplaceAll(id, "_", " ")
}
