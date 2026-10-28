package commands

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"github.com/wendylabsinc/wendy/go/internal/cli/grpcclient"
	wendymcp "github.com/wendylabsinc/wendy/go/internal/cli/mcp"
	"github.com/wendylabsinc/wendy/go/internal/cli/vm"
	"github.com/wendylabsinc/wendy/go/internal/shared/version"
	"github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
)

const doctorProbeTimeout = 3 * time.Second

var errDoctorChecksFailed = errors.New("required diagnostic checks failed; follow the report's remediation")

type doctorCheck struct {
	ID          string `json:"id"`
	Status      string `json:"status"`
	Required    bool   `json:"required"`
	Code        string `json:"code"`
	Message     string `json:"message"`
	Remediation string `json:"remediation,omitempty"`
}
type doctorReport struct {
	SchemaVersion int           `json:"schema_version"`
	CLIVersion    string        `json:"cli_version"`
	OS            string        `json:"os"`
	Arch          string        `json:"arch"`
	OK            bool          `json:"ok"`
	Checks        []doctorCheck `json:"checks"`
}
type doctorTargetEvidence struct{ Reachable, AgentResponding, NotProvisioned bool }
type doctorDeps struct {
	homeDir  func() (string, error)
	getwd    func() (string, error)
	getenv   func(string) string
	readJSON func(string, any) error
	lookPath func(string) (string, error)
	run      func(context.Context, string, ...string) error
	firmware func(string, string) error
	validate func(context.Context, wendymcp.ProjectValidationOptions) (*wendymcp.ProjectValidation, error)
	probe    func(context.Context, string) doctorTargetEvidence
}

func defaultDoctorDeps() doctorDeps {
	return doctorDeps{
		homeDir: os.UserHomeDir, getwd: os.Getwd, getenv: os.Getenv, readJSON: doctorReadJSON, lookPath: exec.LookPath,
		run: func(ctx context.Context, name string, args ...string) error {
			cmd := exec.CommandContext(ctx, name, args...)
			cmd.Stdout, cmd.Stderr = io.Discard, io.Discard
			cmd.WaitDelay = 250 * time.Millisecond
			return cmd.Run()
		},
		firmware: func(goos, prefix string) error { _, err := vm.FindFirmware(goos, prefix); return err },
		validate: validateMCPProject, probe: doctorProbeTarget,
	}
}

func newDoctorCmd() *cobra.Command { return newDoctorCmdWithDeps(defaultDoctorDeps()) }
func newDoctorCmdWithDeps(deps doctorDeps) *cobra.Command {
	var reportJSON bool
	var project string
	var timeout time.Duration
	cmd := &cobra.Command{
		Use: "doctor", Short: "Check setup and print a sanitized local diagnostic report",
		Long: "Read-only checks for a project, Docker, QEMU, and the selected target. Does not install tools, start a VM, deploy an app, refresh credentials, or upload the report. Missing optional context is reported as unknown.",
		Args: cobra.NoArgs,
		// These override the root's config migration, telemetry notice, update,
		// skills and completion housekeeping. Doctor must not repair state implicitly.
		PersistentPreRunE:  func(*cobra.Command, []string) error { return nil },
		PersistentPostRunE: func(*cobra.Command, []string) error { return nil },
		RunE: func(cmd *cobra.Command, _ []string) error {
			if timeout <= 0 || timeout > time.Minute {
				return errors.New("diagnostic timeout must be greater than zero and at most one minute")
			}
			ctx, cancel := context.WithTimeout(cmd.Context(), timeout)
			defer cancel()
			selected, _ := cmd.Flags().GetString("device")
			out := collectDoctorReport(ctx, deps, project, selected)
			globalJSON, _ := cmd.Flags().GetBool("json")
			if reportJSON || globalJSON {
				enc := json.NewEncoder(cmd.OutOrStdout())
				enc.SetIndent("", "  ")
				if err := enc.Encode(out); err != nil {
					return errors.New("could not write diagnostic report")
				}
			} else {
				fmt.Fprintln(cmd.OutOrStdout(), "Wendy setup checks. Read-only; nothing is uploaded.")
				for _, check := range out.Checks {
					qualifier := "optional"
					if check.Required {
						qualifier = "required"
					}
					fmt.Fprintf(cmd.OutOrStdout(), "%s [%s, %s]: %s\n", check.ID, check.Status, qualifier, check.Message)
					if check.Remediation != "" {
						fmt.Fprintln(cmd.OutOrStdout(), "  "+check.Remediation)
					}
				}
				fmt.Fprintln(cmd.OutOrStdout(), "Use wendy doctor --report for the sanitized JSON report.")
			}
			if !out.OK {
				return errDoctorChecksFailed
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&reportJSON, "report", false, "Print sanitized JSON to stdout; no upload or report file is created")
	cmd.Flags().StringVar(&project, "project", "", "Project directory to check (default: current directory, when recognized)")
	cmd.Flags().DurationVar(&timeout, "timeout", 20*time.Second, "Maximum duration of all checks, up to one minute")
	return cmd
}

func (r *doctorReport) add(id, status string, required bool, code, message, remediation string) {
	r.Checks = append(r.Checks, doctorCheck{id, status, required, code, message, remediation})
	if required && status == "fail" {
		r.OK = false
	}
}

func collectDoctorReport(ctx context.Context, d doctorDeps, project, selected string) doctorReport {
	v := version.Version
	if !regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9.+_-]{0,63}$`).MatchString(v) {
		v = "unknown"
	}
	out := doctorReport{SchemaVersion: 1, CLIVersion: v, OS: runtime.GOOS, Arch: runtime.GOARCH, OK: true, Checks: []doctorCheck{}}
	configDir := ""
	if custom := d.getenv("WENDY_CONFIG_DIR"); custom != "" {
		if filepath.IsAbs(custom) {
			configDir = filepath.Clean(custom)
		}
	} else if home, err := d.homeDir(); err == nil {
		configDir = filepath.Join(home, ".wendy")
	}
	var cfg struct{}
	if configDir == "" {
		out.add("cli_config", "fail", true, "config_location_invalid", "The CLI configuration location is unavailable.", "Set WENDY_CONFIG_DIR to an absolute directory or check the home directory configuration.")
	} else if err := d.readJSON(filepath.Join(configDir, "config.json"), &cfg); err != nil && !os.IsNotExist(err) {
		out.add("cli_config", "fail", true, "config_unreadable", "The saved CLI configuration is invalid or unreadable.", "Review the local CLI configuration. Doctor does not rewrite it.")
	} else {
		out.add("cli_config", "pass", false, "config_readable", "CLI configuration is readable or has not been created.", "")
	}
	if selected == "" {
		selected = strings.TrimSpace(d.getenv("WENDY_DEVICE"))
	}
	selected = strings.TrimSpace(selected)

	explicitProject := project != ""
	if !explicitProject {
		project, _ = d.getwd()
	}
	recognized := false
	if project != "" {
		for _, marker := range []string{"wendy.json", "compose.yaml", "compose.yml", "docker-compose.yaml", "docker-compose.yml", "Dockerfile", "Containerfile", "Stagefile", "Package.swift", "pyproject.toml"} {
			// The validator does the full bounded read; Stat only decides whether this
			// directory supplies optional project context. Explicit --project is required.
			if _, err := os.Stat(filepath.Join(project, marker)); err == nil {
				recognized = true
				break
			}
		}
	}
	containerProject := false
	if !recognized && !explicitProject {
		out.add("project", "unknown", false, "project_not_observed", "No Wendy or Compose project was recognized in the current directory.", "Run this command from your project, or pass --project.")
	} else {
		probeCtx, cancel := context.WithTimeout(ctx, doctorProbeTimeout)
		type result struct {
			value *wendymcp.ProjectValidation
			err   error
		}
		done := make(chan result, 1)
		go func() {
			value, err := d.validate(probeCtx, wendymcp.ProjectValidationOptions{ProjectPath: project})
			done <- result{value, err}
		}()
		var validation *wendymcp.ProjectValidation
		select {
		case got := <-done:
			if got.err == nil {
				validation = got.value
			}
		case <-probeCtx.Done():
		}
		cancel()
		if validation == nil {
			out.add("project", "fail", true, "project_check_incomplete", "Project validation could not complete.", "Check that the project is readable, then retry. No project details are included in this report.")
		} else {
			containerProject = validation.BuildType != "native-process" && validation.BuildType != "" && validation.BuildType != "unknown"
			if validation.Valid {
				out.add("project", "pass", true, "project_valid", "Project configuration and build inputs passed static validation.", "")
			} else {
				out.add("project", "fail", true, "project_invalid", "Project configuration or build inputs need attention.", "Run wendy json validate in the project and review its local diagnostics.")
			}
		}
	}
	qemuRequired := strings.HasPrefix(selected, "vm:") || selected == "sim" || selected == "simulator"
	dockerRequired := containerProject || selected == "docker" || qemuRequired
	doctorExecutableCheck(&out, d, "docker", dockerRequired, "Install Docker to build container projects.")
	dockerPath, dockerErr := d.lookPath("docker")
	for _, check := range []struct {
		id   string
		args []string
		fix  string
	}{{"docker_daemon", []string{"info"}, "Start Docker, then rerun the check."}, {"docker_buildx", []string{"buildx", "version"}, "Install the Docker Buildx plugin, then rerun the check."}} {
		if dockerErr != nil {
			out.add(check.id, "unknown", dockerRequired, "docker_not_available", "The check needs the Docker executable.", check.fix)
			continue
		}
		probeCtx, cancel := context.WithTimeout(ctx, doctorProbeTimeout)
		err := d.run(probeCtx, dockerPath, check.args...)
		cancel()
		if err != nil {
			out.add(check.id, "fail", dockerRequired, check.id+"_unavailable", "The read-only Docker check did not succeed.", check.fix)
		} else {
			out.add(check.id, "pass", dockerRequired, check.id+"_available", "The read-only Docker check succeeded.", "")
		}
	}
	doctorExecutableCheck(&out, d, "qemu-system-aarch64", qemuRequired, "Install QEMU and ARM64 UEFI firmware before using a WendyOS VM.")
	qemuPath, _ := d.lookPath("qemu-system-aarch64")
	prefix := ""
	if qemuPath != "" {
		prefix = filepath.Dir(filepath.Dir(qemuPath))
	}
	if err := d.firmware(runtime.GOOS, prefix); err != nil {
		out.add("qemu_firmware", "fail", qemuRequired, "firmware_missing", "ARM64 UEFI firmware was not found.", "Install QEMU with its ARM64 UEFI firmware; doctor does not install packages.")
	} else {
		out.add("qemu_firmware", "pass", qemuRequired, "firmware_available", "ARM64 UEFI firmware is available.", "")
	}
	doctorCheckTarget(ctx, &out, d, configDir, selected)
	if ctx.Err() != nil {
		out.add("diagnostics", "fail", true, "diagnostic_timeout", "The diagnostic deadline was reached.", "Retry with a longer --timeout, up to one minute.")
	}
	return out
}

func doctorExecutableCheck(out *doctorReport, d doctorDeps, name string, required bool, fix string) {
	id := "docker_executable"
	if name != "docker" {
		id = "qemu_executable"
	}
	if _, err := d.lookPath(name); err != nil {
		out.add(id, "fail", required, id+"_missing", "The executable is not available.", fix)
	} else {
		out.add(id, "pass", required, id+"_available", "The executable is available.", "")
	}
}

func doctorCheckTarget(ctx context.Context, out *doctorReport, d doctorDeps, configDir, selected string) {
	if selected == "" {
		out.add("selected_target", "unknown", false, "target_not_selected", "No explicit, environment, or saved target was selected.", "Use --device to check one target without scanning or starting it.")
		out.add("authentication", "unknown", false, "auth_not_checked", "Authentication was not checked without a target.", "")
		return
	}
	out.add("selected_target", "pass", true, "target_selected", "A target selector is available; its value is omitted.", "")
	if selected == "docker" {
		out.add("device_reachable", "not_required", false, "local_docker", "The local Docker target does not require a device connection.", "")
		out.add("authentication", "not_required", false, "local_docker", "Wendy Cloud authentication is not required for the local Docker target.", "")
		return
	}
	if strings.HasPrefix(selected, "cloud:") {
		out.add("device_reachable", "unknown", false, "cloud_not_probed", "Cloud targets are not probed by this read-only report.", "Use normal cloud commands to check the selected connection.")
		out.add("authentication", "unknown", false, "auth_not_refreshed", "Cloud credentials were not used or refreshed.", "")
		return
	}
	if selected == "sim" || selected == "simulator" {
		out.add("device_reachable", "unknown", false, "vm_name_required", "An unnamed simulator would require selection or creation.", "Use --device vm:NAME for an existing, running virtual device.")
		out.add("authentication", "unknown", false, "auth_not_checked", "Authentication was not checked.", "")
		return
	}
	var addresses []string
	if strings.HasPrefix(selected, "vm:") {
		name := strings.TrimPrefix(selected, "vm:")
		if configDir == "" || vm.ValidName(name) != nil {
			out.add("device_reachable", "fail", true, "vm_selector_invalid", "The virtual-device selector or configuration location is invalid.", "Use --device vm:NAME with a valid existing VM name.")
			out.add("authentication", "unknown", false, "auth_not_checked", "Authentication was not checked.", "")
			return
		}
		// Avoid vm.Store.Status: it may delete stale state or create locks. Reading
		// state never proves liveness; only the following network probes do that.
		var state vm.State
		err := d.readJSON(filepath.Join(configDir, "vms", name, "state.json"), &state)
		if err != nil || state.AgentPort < 1 || state.AgentPort > 65535-vm.AgentPortSpan+1 {
			out.add("device_reachable", "fail", true, "vm_not_running", "No usable running-state record was found for the selected VM.", "Start the intended VM yourself, then rerun doctor. Doctor never boots it.")
			out.add("authentication", "unknown", false, "auth_not_checked", "Authentication was not checked for the stopped or unavailable VM.", "")
			return
		}
		for offset := range vm.AgentPortSpan {
			addresses = append(addresses, net.JoinHostPort("127.0.0.1", strconv.Itoa(state.AgentPort+offset)))
		}
	} else {
		var err error
		addresses, err = doctorAddresses(selected)
		if err != nil {
			out.add("device_reachable", "fail", true, "target_selector_invalid", "This target selector cannot be checked directly.", "Use a hostname, IP address, host:port, or an existing vm:NAME selector.")
			out.add("authentication", "unknown", false, "auth_not_checked", "Authentication was not checked.", "")
			return
		}
	}
	probeCtx, cancel := context.WithTimeout(ctx, doctorProbeTimeout)
	defer cancel()
	evidence := doctorProbeTargets(probeCtx, d.probe, addresses)
	if !evidence.Reachable {
		out.add("device_reachable", "fail", true, "target_unreachable", "The selected endpoint could not be reached.", "Check target power, connection, and address. Doctor does not change networking.")
	} else {
		out.add("device_reachable", "pass", true, "endpoint_reachable", "The selected endpoint accepted a connection; device identity is not verified.", "")
	}
	if evidence.AgentResponding {
		out.add("wendy_agent", "pass", false, "agent_responding", "A Wendy agent answered a read-only version request.", "")
	} else {
		out.add("wendy_agent", "unknown", false, "agent_unverified", "A Wendy agent response could not be verified; authentication may be required.", "Check the device with your normal authenticated connection.")
	}
	if evidence.AgentResponding && evidence.NotProvisioned {
		out.add("authentication", "not_required", false, "local_unprovisioned", "The responding agent reports that it is not enrolled.", "")
	} else {
		out.add("authentication", "unknown", false, "auth_not_validated", "Authentication validity was not checked. No credentials were refreshed.", "")
	}
}

func doctorAddresses(address string) ([]string, error) {
	if address == "" || strings.ContainsAny(address, " /\\\t\r\n,@?#") {
		return nil, errors.New("invalid target")
	}
	if _, numeric := allDigitDeviceHost(address); numeric {
		return nil, errors.New("invalid target")
	}
	if host, port, err := net.SplitHostPort(address); err == nil {
		n, e := strconv.Atoi(port)
		if host == "" || e != nil || n < 1 || n > 65535 {
			return nil, errors.New("invalid target")
		}
		return []string{address}, nil
	}
	if strings.Contains(address, ":") && net.ParseIP(address) == nil {
		return nil, errors.New("invalid target")
	}
	// Provisioned agents stop listening on plaintext and move to the TLS port.
	// Probe both defaults only when the user has not selected a specific port.
	return []string{net.JoinHostPort(address, "50051"), net.JoinHostPort(address, "50052")}, nil
}

func doctorProbeTargets(ctx context.Context, probe func(context.Context, string) doctorTargetEvidence, addresses []string) doctorTargetEvidence {
	// A silent endpoint must not consume the whole deadline before the other
	// endpoint gets a chance to connect. Each probe obeys the same deadline.
	results := make(chan doctorTargetEvidence, len(addresses))
	for _, address := range addresses {
		go func() { results <- probe(ctx, address) }()
	}
	var out doctorTargetEvidence
	for range addresses {
		evidence := <-results
		out.Reachable = out.Reachable || evidence.Reachable
		if evidence.AgentResponding {
			out.AgentResponding = true
			out.NotProvisioned = out.NotProvisioned || evidence.NotProvisioned
		}
	}
	return out
}

func doctorReadJSON(path string, value any) error {
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() || info.Size() > 1<<20 {
		return errors.New("invalid diagnostic input")
	}
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, (1<<20)+1))
	if err != nil {
		return err
	}
	if len(data) > 1<<20 {
		return errors.New("diagnostic input exceeds limit")
	}
	return json.Unmarshal(data, value)
}

func doctorProbeTarget(ctx context.Context, address string) doctorTargetEvidence {
	var out doctorTargetEvidence
	conn, err := (&net.Dialer{}).DialContext(ctx, "tcp", address)
	if err != nil {
		return out
	}
	conn.Close()
	out.Reachable = true
	agent, err := grpcclient.Connect(ctx, address)
	if err != nil {
		return out
	}
	defer agent.Close()
	if _, err := agent.AgentService.GetAgentVersion(ctx, &agentpb.GetAgentVersionRequest{}); err != nil {
		return out
	}
	out.AgentResponding = true
	response, err := agent.ProvisioningService.IsProvisioned(ctx, &agentpb.IsProvisionedRequest{})
	if err == nil {
		_, out.NotProvisioned = response.GetResponse().(*agentpb.IsProvisionedResponse_NotProvisioned)
	}
	return out
}
