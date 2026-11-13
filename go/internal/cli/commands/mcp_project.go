package commands

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"slices"
	"sort"
	"strings"

	"github.com/moby/buildkit/frontend/dockerfile/instructions"
	"github.com/moby/buildkit/frontend/dockerfile/parser"
	wendymcp "github.com/wendylabsinc/wendy/go/internal/cli/mcp"
	"github.com/wendylabsinc/wendy/go/internal/shared/appconfig"
	"github.com/wendylabsinc/wendy/go/internal/stagefile"
	"github.com/wendylabsinc/wendy/go/internal/stagefile/spec"
	"gopkg.in/yaml.v3"
)

func validateMCPProject(ctx context.Context, opts wendymcp.ProjectValidationOptions) (*wendymcp.ProjectValidation, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	result := &wendymcp.ProjectValidation{ProjectPath: opts.ProjectPath, Valid: true, Diagnostics: []wendymcp.ProjectDiagnostic{}, Builds: []wendymcp.ProjectBuild{}}
	v := projectValidator{result: result}
	info, err := os.Stat(opts.ProjectPath)
	if err != nil || !info.IsDir() {
		v.add("error", "project_path", "Project directory does not exist or is unreadable.", "Pass the directory containing the project.")
		v.compatibility(nil, opts)
		return result, nil
	}
	data, readErr := readProjectInput(filepath.Join(opts.ProjectPath, "wendy.json"))
	projectType, detectErr := detectProjectType(opts.ProjectPath)
	if readErr == nil {
		projectType, detectErr = resolveRunProjectType(opts.ProjectPath, opts.BuildType)
	} else if opts.BuildType != "" {
		projectType = opts.BuildType
	}
	if detectErr != nil {
		v.add("error", "build_type", detectErr.Error(), "Choose an available build type or add its project files.")
	}
	var cfg *appconfig.AppConfig
	if readErr != nil {
		if projectType != "compose" || !os.IsNotExist(readErr) {
			v.add("error", "wendy.json", readErr.Error(), "Create a readable wendy.json with an appId and this project's runtime settings.")
		}
	} else {
		cfg, err = appconfig.LoadFromBytes(data)
		if err != nil {
			v.add("error", "wendy.json", err.Error(), "Correct the JSON syntax and field value types.")
		} else {
			if appconfig.ValidateAppID(cfg.AppID) == nil {
				result.AppID = cfg.AppID
			}
			for _, warning := range appconfig.ValidateJSON(data) {
				v.add("warning", projectDiagnosticField(warning), warning, "Correct unknown keys or replace the deprecated setting described in the warning.")
			}
			if projectType == "compose" {
				_, _, err = appconfig.LoadComposeCompanion(opts.ProjectPath)
			} else {
				err = cfg.Validate()
			}
			if err != nil {
				v.add("error", projectDiagnosticField(err.Error()), err.Error(), "Correct the reported field in wendy.json before deploying.")
			}
			for name, service := range cfg.Services {
				if err := appconfig.ValidateServiceName(name); err != nil {
					v.add("error", "services."+name, err.Error(), "Use lowercase service names containing letters, digits, and hyphens.")
				}
				if service == nil {
					v.add("error", "services."+name, "Service configuration must not be null.", "Supply a service configuration object.")
				}
			}
		}
	}
	if cfg != nil && cfg.Run != nil && cfg.Run.Command != "" {
		projectType = "native-process"
	}
	if cfg != nil && len(cfg.Services) > 0 && projectType != "compose" && projectType != "native-process" {
		projectType = "multi-service"
	}
	result.BuildType = projectType
	switch projectType {
	case "compose":
		v.compose(opts.ProjectPath, cfg)
	case "multi-service":
		v.services(opts.ProjectPath, cfg)
	case "native-process":
		result.Builds = append(result.Builds, wendymcp.ProjectBuild{Type: "native-process"})
		v.native(opts.ProjectPath, cfg)
		if cfg.Platform == "" {
			v.add("error", "platform", "The CLI defaults an omitted platform to Linux; run.command requires a native Darwin target.", "Set platform to darwin in wendy.json and select a Mac agent.")
		}
		if opts.BuildType != "" {
			v.add("error", "build_type", "run.command cannot be combined with build_type.", "Omit build_type for a native run.command project.")
		}
	case "", "unknown":
		v.add("error", "build", "No supported build inputs were found.", "Add a Dockerfile, Containerfile, Stagefile, Compose file, Package.swift, Python project marker, or native run.command.")
	default:
		v.build(opts.ProjectPath, "", projectType)
	}
	if cfg != nil {
		for i, file := range cfg.Files {
			if file.Path == "" {
				continue
			}
			if _, err := os.Stat(filepath.Join(opts.ProjectPath, file.Path)); err != nil {
				v.add("error", fmt.Sprintf("files[%d].path", i), err.Error(), "Add the declared source file or remove its files entry.")
			}
		}
		if cfg.Brewfile != "" {
			if _, err := readProjectInput(filepath.Join(opts.ProjectPath, cfg.Brewfile)); err != nil {
				v.add("error", "brewfile", err.Error(), "Add the declared Brewfile or correct its relative path.")
			}
		}
	}
	v.compatibility(cfg, opts)
	sort.SliceStable(result.Diagnostics, func(i, j int) bool {
		a, b := result.Diagnostics[i], result.Diagnostics[j]
		if a.Severity != b.Severity {
			return a.Severity < b.Severity
		}
		if a.Field != b.Field {
			return a.Field < b.Field
		}
		return a.Message < b.Message
	})
	return result, ctx.Err()
}

type projectValidator struct{ result *wendymcp.ProjectValidation }

func (v projectValidator) add(severity, field, message, fix string) {
	for _, existing := range v.result.Diagnostics {
		if existing.Severity == severity && existing.Field == field && existing.Message == message {
			return
		}
	}
	v.result.Diagnostics = append(v.result.Diagnostics, wendymcp.ProjectDiagnostic{Severity: severity, Field: field, Message: message, Fix: fix})
	if severity == "error" {
		v.result.Valid = false
	}
}

func projectDiagnosticField(message string) string {
	for _, field := range []string{"appId", "services", "entitlement", "readiness", "resources", "frameworks", "env", "files", "brewfile", "run", "hooks"} {
		if strings.HasPrefix(message, field) {
			if i := strings.Index(message, ":"); i > 0 && i < 200 {
				return message[:i]
			}
			return field
		}
	}
	return "wendy.json"
}

func readProjectInput(path string) ([]byte, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("%s must be a regular file", path)
	}
	if info.Size() > 2*1024*1024 {
		return nil, fmt.Errorf("%s exceeds the 2 MiB validation limit", path)
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, 2*1024*1024+1))
	if len(data) > 2*1024*1024 {
		return nil, fmt.Errorf("%s exceeds the 2 MiB validation limit", path)
	}
	return data, err
}

func (v projectValidator) build(dir, service, buildType string) {
	options := detectBuildOptions(dir)
	matches := []BuildOption{}
	for _, option := range options {
		if option.Type == buildType {
			matches = append(matches, option)
		}
	}
	if len(matches) == 0 {
		v.add("error", "build"+projectServiceSuffix(service), "No "+buildType+" build input exists in "+dir, "Add the requested build input or correct the service context/build type.")
		return
	}
	chosen := &matches[0]
	if buildType == "docker" {
		var err error
		chosen, err = buildOptionForType(options, "docker", false)
		if err != nil {
			v.add("error", "build"+projectServiceSuffix(service), err.Error(), "Keep a conventional build.stagefile.yaml, Dockerfile, or Containerfile, or select one build file explicitly in the CLI.")
			return
		}
	}
	v.result.Builds = append(v.result.Builds, wendymcp.ProjectBuild{Service: service, Type: chosen.Type, File: filepath.Join(dir, chosen.File)})
	if chosen.Type == "docker" {
		v.buildFile(dir, chosen.File, service)
	}
	// SwiftPM and Python metadata can execute arbitrary package code when
	// loaded by their tooling. Detect their files without evaluating them.
}

func projectServiceSuffix(service string) string {
	if service == "" {
		return ""
	}
	return ".services." + service
}

func (v projectValidator) buildFile(dir, name, service string) {
	confinedPath, err := confinedDockerfilePath(dir, name)
	var data []byte
	if err == nil {
		data, err = readProjectInput(confinedPath)
	}
	if err == nil {
		if stagefile.IsSourceName(filepath.Base(name)) {
			_, err = spec.Parse(data)
		} else {
			var parsed *parser.Result
			parsed, err = parser.Parse(bytes.NewReader(data))
			if err == nil {
				var stages []instructions.Stage
				stages, _, err = instructions.Parse(parsed.AST, nil)
				if err == nil && len(stages) == 0 {
					err = fmt.Errorf("Dockerfile must contain at least one FROM instruction")
				}
			}
		}
	}
	if err != nil {
		v.add("error", "build"+projectServiceSuffix(service), err.Error(), "Correct the selected build file; validation does not execute build steps or resolve images.")
	}
}

func (v projectValidator) native(dir string, cfg *appconfig.AppConfig) {
	entries, err := assembleNativeCommandSyncEntries(dir, cfg)
	if err != nil {
		v.add("error", "run.command", err.Error(), "Add the native executable and correct its files mapping.")
		return
	}
	if path.IsAbs(cfg.Run.Command) {
		return
	}
	command := path.Clean(cfg.Run.Command)
	for _, entry := range entries {
		local := entry.localPath
		info, err := os.Stat(local)
		if err != nil {
			continue
		}
		if info.IsDir() {
			relative, covered := remotePathRelativeToPrefix(command, entry.remotePath)
			if !covered {
				continue
			}
			local = filepath.Join(local, relative)
			info, err = os.Lstat(local)
		} else if path.Clean(entry.remotePath) != command {
			continue
		}
		if err == nil && info.Mode().IsRegular() && info.Mode().Perm()&0111 != 0 {
			return
		}
	}
	v.add("error", "run.command", "The relative native command is not an executable in the synced files.", "Add the command through files, verify its destination, and set an executable permission with chmod +x.")
}

func (v projectValidator) services(dir string, cfg *appconfig.AppConfig) {
	validServices := map[string]*appconfig.ServiceConfig{}
	for name, svc := range cfg.Services {
		if svc != nil {
			validServices[name] = svc
		}
	}
	if _, err := appconfig.ServiceTopoOrder(validServices); err != nil {
		v.add("error", "services.dependsOn", err.Error(), "Remove dependency cycles and reference only declared service names.")
	}
	names := make([]string, 0, len(validServices))
	for name := range validServices {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		svc := validServices[name]
		copyService := *svc
		copyService.DependsOn = nil
		serviceConfig := &appconfig.AppConfig{AppID: cfg.AppID, Services: map[string]*appconfig.ServiceConfig{name: &copyService}}
		if err := serviceConfig.Validate(); err != nil {
			v.add("error", "services."+name, err.Error(), "Correct the service context or runtime settings before deploying.")
			continue
		}
		contextDir := filepath.Join(dir, svc.Context)
		info, err := os.Stat(contextDir)
		if err != nil || !info.IsDir() {
			v.add("error", "services."+name+".context", "Service build context is missing: "+contextDir, "Create the context directory or correct its path relative to wendy.json.")
			continue
		}
		v.build(contextDir, name, "docker")
	}
}

func (v projectValidator) compose(dir string, companion *appconfig.AppConfig) {
	if companion != nil && companion.Platform != "" {
		v.add("warning", "platform", "Compose deployment derives its Linux platform from the device architecture and ignores companion platform.", "Remove the ignored companion platform or deploy a standalone project when an explicit platform is required.")
	}
	for _, name := range []string{"docker-compose.yml", "docker-compose.yaml", "compose.yml", "compose.yaml"} {
		if _, err := os.Stat(filepath.Join(dir, name)); err == nil {
			if _, err := readProjectInput(filepath.Join(dir, name)); err != nil {
				v.add("error", name, err.Error(), "Provide a readable Compose file within the validation size limit.")
				return
			}
			break
		}
	}
	cfg, filename, err := parseComposeFile(dir)
	if err != nil {
		v.add("error", "compose", err.Error(), "Correct the Compose YAML syntax.")
		return
	}
	if len(cfg.Services) == 0 {
		v.add("error", "compose.services", "Compose file declares no services.", "Add at least one service with an image or build context.")
		return
	}
	if _, err := serviceOrder(cfg); err != nil {
		v.add("error", "compose.services.depends_on", err.Error(), "Remove dependency cycles and reference only declared services.")
	}
	for _, warning := range composeCompanionWarnings(companion, cfg) {
		v.add("warning", "wendy.json.services", warning, "Match companion service names to the Compose services.")
	}
	serviceConfigs, warnings, configErr := buildComposeServiceConfigs(strings.ToLower(filepath.Base(dir)), cfg, companion)
	for _, warning := range warnings {
		v.add("warning", "compose.x-wendy", warning, "Correct the Wendy extension as described in the warning.")
	}
	if configErr != nil {
		v.add("error", "compose.x-wendy", configErr.Error(), "Correct x-wendy readiness/hooks configuration.")
	}
	names := make([]string, 0, len(cfg.Services))
	for name := range cfg.Services {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		svc := cfg.Services[name]
		field := "compose.services." + name
		if err := appconfig.ValidateServiceName(name); err != nil {
			v.add("error", field, err.Error(), "Rename the service to a lowercase DNS label.")
		}
		if merged := serviceConfigs[name]; merged != nil {
			if err := merged.Validate(); err != nil {
				v.add("error", field, err.Error(), "Correct the Compose-derived or companion runtime setting.")
			}
		}
		for _, ignored := range unsupportedComposeWarnings(svc) {
			v.add("warning", field+"."+ignored, "Wendy does not apply this Compose setting.", "Use a supported Wendy entitlement or remove the ignored setting.")
		}
		if !svc.DependsOn.IsZero() && svc.DependsOn.Kind != yaml.SequenceNode && svc.DependsOn.Kind != yaml.MappingNode {
			v.add("error", field+".depends_on", "depends_on must be a list or mapping.", "Use a list of declared service names.")
		}
		contextDir, buildFile, _, err := composeBuildContext(svc, dir)
		if err != nil {
			v.add("error", field+".build", err.Error(), "Correct the build context and Dockerfile path.")
			continue
		}
		if contextDir == "" {
			if strings.TrimSpace(svc.Image) == "" {
				v.add("error", field, "Service has neither an image nor a build context.", "Set image or build for this service.")
			}
			v.result.Builds = append(v.result.Builds, wendymcp.ProjectBuild{Service: name, Type: "image", File: filename})
			continue
		}
		info, err := os.Stat(contextDir)
		if err != nil || !info.IsDir() {
			v.add("error", field+".build.context", "Build context directory is missing: "+contextDir, "Correct the context path or create its directory.")
			continue
		}
		v.result.Builds = append(v.result.Builds, wendymcp.ProjectBuild{Service: name, Type: "docker", File: filepath.Join(contextDir, buildFile)})
		v.buildFile(contextDir, buildFile, name)
	}
}

func (v projectValidator) compatibility(cfg *appconfig.AppConfig, opts wendymcp.ProjectValidationOptions) {
	c := wendymcp.ProjectCompatibility{Status: "unknown", Scope: "Declared platform and reported GPU/NPU only; builds, image availability, entitlement authorization, and runtime behavior are not checked.", Checks: []wendymcp.ProjectCompatibilityCheck{}}
	defer func() { v.result.Compatibility = c }()
	add := func(field, status, message string) {
		c.Checks = append(c.Checks, wendymcp.ProjectCompatibilityCheck{Field: field, Status: status, Message: message})
		if status == "incompatible" {
			v.add("error", field, message, "Select a compatible device or change this project requirement.")
		}
	}
	if opts.Device == nil {
		reason := opts.DeviceError
		if reason == "" {
			reason = "no device information available"
		}
		add("device", "unknown", reason)
		return
	}
	if cfg == nil && v.result.BuildType != "compose" {
		add("device", "unknown", "Project configuration could not be loaded.")
		return
	}
	platform := ""
	if cfg != nil {
		platform = cfg.Platform
	}
	if v.result.BuildType == "compose" {
		platform = ""
	}
	resolved := resolveAgentPlatform(platform, opts.Device.GetOs(), opts.Device.GetCpuArchitecture())
	projectOS, projectArch, _ := strings.Cut(resolved, "/")
	projectArch, _, _ = strings.Cut(projectArch, "/")
	deviceOS, deviceArch := normalizePlatformOS(strings.ToLower(opts.Device.GetOs())), opts.Device.GetCpuArchitecture()
	if deviceOS == "" || deviceArch == "" {
		add("platform", "unknown", "Device did not report both operating system and CPU architecture.")
	} else if projectArch != deviceArch || (projectOS != deviceOS && !(deviceOS == "darwin" && projectOS == "linux")) {
		add("platform", "incompatible", fmt.Sprintf("Project resolves to %s; device reports %s/%s.", resolved, deviceOS, deviceArch))
	} else {
		add("platform", "compatible", fmt.Sprintf("Project %s is supported by the %s/%s deployment path.", resolved, deviceOS, deviceArch))
	}
	if v.result.BuildType == "native-process" {
		if projectOS != "darwin" || deviceOS != "darwin" {
			add("platform", "incompatible", "run.command requires both platform darwin and a Mac agent target.")
		}
		if slices.Contains(opts.Device.GetFeatureset(), "native-process") {
			add("run.command", "compatible", "Agent advertises the native-process feature.")
		} else {
			add("run.command", "incompatible", "Agent does not advertise native-process; update Wendy Agent on the target Mac.")
		}
	}
	if v.result.BuildType == "xcode" && projectOS != "darwin" {
		add("platform", "incompatible", "Xcode projects require platform darwin; set platform in wendy.json and select a Mac target.")
	}
	if cfg != nil {
		checkEntitlements := func(prefix string, entitlements []appconfig.Entitlement) {
			for _, e := range entitlements {
				var present *bool
				switch e.Type {
				case appconfig.EntitlementGPU:
					present = opts.Device.HasGpu
					if len(opts.Device.GetGpuCapabilities()) > 0 {
						yes := true
						present = &yes
					}
				case "npu":
					present = opts.Device.HasNpu
				default:
					continue
				}
				field := prefix + "." + e.Type
				if present == nil {
					add(field, "unknown", "Device did not report whether this accelerator is available.")
				} else if !*present {
					add(field, "incompatible", "Project requests "+e.Type+" but the device reports it is unavailable.")
				} else {
					add(field, "compatible", "Device reports this accelerator is available; driver/backend access still requires runtime validation.")
				}
			}
		}
		checkEntitlements("entitlements", cfg.Entitlements)
		names := make([]string, 0, len(cfg.Services))
		for name := range cfg.Services {
			names = append(names, name)
		}
		sort.Strings(names)
		for _, name := range names {
			if svc := cfg.Services[name]; svc != nil {
				checkEntitlements("services."+name+".entitlements", svc.Entitlements)
			}
		}
	}
	c.Status = "compatible"
	for _, check := range c.Checks {
		if check.Status == "incompatible" {
			c.Status = "incompatible"
			break
		}
		if check.Status == "unknown" {
			c.Status = "unknown"
		}
	}
}
