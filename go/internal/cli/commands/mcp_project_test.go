package commands

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	wendymcp "github.com/wendylabsinc/wendy/go/internal/cli/mcp"
	"github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
)

func mcpProjectFixture(t *testing.T, files map[string]string) string {
	t.Helper()
	root := t.TempDir()
	for name, data := range files {
		path := filepath.Join(root, name)
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(data), 0600); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

func TestMCPProjectValidOfflineDoesNotRunHooksOrBuilders(t *testing.T) {
	root := mcpProjectFixture(t, map[string]string{"wendy.json": `{"appId":"robot","hooks":{"postStart":{"cli":"touch SHOULD_NOT_EXIST"}}}`, "Dockerfile": "FROM scratch\nRUN touch /not-executed\n"})
	result, err := validateMCPProject(context.Background(), wendymcp.ProjectValidationOptions{ProjectPath: root})
	if err != nil || !result.Valid || result.BuildType != "docker" || result.Compatibility.Status != "unknown" || len(result.Builds) != 1 {
		t.Fatalf("validation: %+v %v", result, err)
	}
	entries, err := os.ReadDir(root)
	if err != nil || len(entries) != 2 {
		t.Fatalf("validation wrote project files: %v %v", entries, err)
	}
}

func TestMCPProjectInvalidConfigurationHasActionableFindings(t *testing.T) {
	tests := []struct{ name, config, want string }{
		{"malformed", `{"appId":`, "JSON"},
		{"missing-app", `{}`, "appId"},
		{"unknown-entitlement", `{"appId":"robot","entitlements":[{"type":"not-real"}]}`, "unknown type"},
		{"null-service", `{"appId":"robot","services":{"api":null}}`, "null"},
		{"unsafe-service", `{"appId":"robot","services":{"Api_BAD":{"context":"."}}}`, "service"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			root := mcpProjectFixture(t, map[string]string{"wendy.json": tc.config, "Dockerfile": "FROM scratch\n"})
			result, err := validateMCPProject(context.Background(), wendymcp.ProjectValidationOptions{ProjectPath: root})
			if err != nil || result.Valid {
				t.Fatalf("accepted invalid config: %+v %v", result, err)
			}
			joined := ""
			for _, d := range result.Diagnostics {
				joined += d.Message
				if d.Field == "" || d.Fix == "" {
					t.Fatalf("nonactionable finding: %+v", d)
				}
			}
			if !strings.Contains(strings.ToLower(joined), strings.ToLower(tc.want)) {
				t.Fatalf("missing %q in %+v", tc.want, result.Diagnostics)
			}
		})
	}
}

func TestMCPProjectUnknownKeysWarnWithoutRejectingValidConfig(t *testing.T) {
	root := mcpProjectFixture(t, map[string]string{"wendy.json": `{"appId":"robot","entitlments":[]}`, "Dockerfile": "FROM scratch\n"})
	result, err := validateMCPProject(context.Background(), wendymcp.ProjectValidationOptions{ProjectPath: root})
	if err != nil || !result.Valid || len(result.Diagnostics) != 1 || result.Diagnostics[0].Severity != "warning" {
		t.Fatalf("unknown key: %+v %v", result, err)
	}
}

func TestMCPProjectMultiServiceContextsAndTopology(t *testing.T) {
	for _, tc := range []struct {
		name, config string
		valid        bool
	}{
		{"valid", `{"appId":"robot","services":{"api":{"context":"api"},"worker":{"context":"worker","dependsOn":["api"]}}}`, true},
		{"cycle", `{"appId":"robot","services":{"api":{"context":"api","dependsOn":["worker"]},"worker":{"context":"worker","dependsOn":["api"]}}}`, false},
		{"missing", `{"appId":"robot","services":{"api":{"context":"missing"}}}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := mcpProjectFixture(t, map[string]string{"wendy.json": tc.config, "api/Dockerfile": "FROM scratch\n", "worker/Containerfile": "FROM scratch\n"})
			result, err := validateMCPProject(context.Background(), wendymcp.ProjectValidationOptions{ProjectPath: root})
			if err != nil || result.Valid != tc.valid || result.BuildType != "multi-service" {
				t.Fatalf("multi-service: %+v %v", result, err)
			}
			if tc.valid && len(result.Builds) != 2 {
				t.Fatalf("missing builds: %+v", result)
			}
		})
	}
}

func TestMCPProjectComposeCompanionDoesNotRequireContext(t *testing.T) {
	root := mcpProjectFixture(t, map[string]string{"wendy.json": `{"appId":"robot","services":{"api":{"entitlements":[{"type":"http","port":8080}]}}}`, "compose.yaml": "services:\n  api:\n    image: nginx:latest\n    healthcheck:\n      test: ['CMD', 'true']\n"})
	result, err := validateMCPProject(context.Background(), wendymcp.ProjectValidationOptions{ProjectPath: root})
	if err != nil || !result.Valid || result.BuildType != "compose" || len(result.Builds) != 1 {
		t.Fatalf("companion: %+v %v", result, err)
	}
	found := false
	for _, finding := range result.Diagnostics {
		found = found || finding.Field == "compose.services.api.healthcheck"
	}
	if !found {
		t.Fatalf("ignored Compose healthcheck not reported: %+v", result)
	}
}

func TestMCPProjectComposeWithoutCompanionAndMissingBuildInput(t *testing.T) {
	root := mcpProjectFixture(t, map[string]string{"compose.yaml": "services:\n  api:\n    image: nginx:latest\n"})
	result, err := validateMCPProject(context.Background(), wendymcp.ProjectValidationOptions{ProjectPath: root})
	if err != nil || !result.Valid {
		t.Fatalf("standalone Compose: %+v %v", result, err)
	}
	if err := os.WriteFile(filepath.Join(root, "compose.yaml"), []byte("services:\n  api:\n    build: .\n"), 0600); err != nil {
		t.Fatal(err)
	}
	result, err = validateMCPProject(context.Background(), wendymcp.ProjectValidationOptions{ProjectPath: root})
	if err != nil || result.Valid {
		t.Fatalf("missing Dockerfile accepted: %+v %v", result, err)
	}
}

func TestMCPProjectNativeProcessAndTargetCompatibility(t *testing.T) {
	root := mcpProjectFixture(t, map[string]string{"wendy.json": `{"appId":"native","platform":"darwin","run":{"command":"/usr/bin/true"}}`})
	result, err := validateMCPProject(context.Background(), wendymcp.ProjectValidationOptions{ProjectPath: root, Device: &agentpb.GetAgentVersionResponse{Os: "darwin", CpuArchitecture: "arm64", Featureset: []string{"native-process"}}})
	if err != nil || !result.Valid || result.BuildType != "native-process" || result.Compatibility.Status != "compatible" {
		t.Fatalf("native project: %+v %v", result, err)
	}
	result, err = validateMCPProject(context.Background(), wendymcp.ProjectValidationOptions{ProjectPath: root, Device: &agentpb.GetAgentVersionResponse{Os: "linux", CpuArchitecture: "arm64"}})
	if err != nil || result.Valid || result.Compatibility.Status != "incompatible" {
		t.Fatalf("native/Linux mismatch lost: %+v %v", result, err)
	}
}

func TestMCPProjectNativeProcessRequiresExplicitDarwinPlatform(t *testing.T) {
	root := mcpProjectFixture(t, map[string]string{"wendy.json": `{"appId":"native","run":{"command":"/usr/bin/true"}}`})
	result, err := validateMCPProject(context.Background(), wendymcp.ProjectValidationOptions{ProjectPath: root, Device: &agentpb.GetAgentVersionResponse{Os: "darwin", CpuArchitecture: "arm64", Featureset: []string{"native-process"}}})
	if err != nil || result.Valid || result.Compatibility.Status != "incompatible" {
		t.Fatalf("native CLI platform rejection missed: %+v %v", result, err)
	}
}

func TestMCPProjectOptionalHardwareEvidenceAndArchitecture(t *testing.T) {
	root := mcpProjectFixture(t, map[string]string{"wendy.json": `{"appId":"robot","platform":"linux/arm64","entitlements":[{"type":"gpu"}]}`, "Dockerfile": "FROM scratch\n"})
	no := false
	for _, tc := range []struct {
		name   string
		device *agentpb.GetAgentVersionResponse
		want   string
		valid  bool
	}{
		{"unknown-hardware", &agentpb.GetAgentVersionResponse{Os: "linux", CpuArchitecture: "arm64"}, "unknown", true},
		{"no-gpu", &agentpb.GetAgentVersionResponse{Os: "linux", CpuArchitecture: "arm64", HasGpu: &no}, "incompatible", false},
		{"wrong-arch", &agentpb.GetAgentVersionResponse{Os: "linux", CpuArchitecture: "amd64"}, "incompatible", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			result, err := validateMCPProject(context.Background(), wendymcp.ProjectValidationOptions{ProjectPath: root, Device: tc.device})
			if err != nil || result.Valid != tc.valid || result.Compatibility.Status != tc.want {
				t.Fatalf("compatibility: %+v %v", result, err)
			}
		})
	}
}

func TestMCPProjectInvalidStagefileAndAmbiguousBuildVariants(t *testing.T) {
	for _, files := range []map[string]string{
		{"wendy.json": `{"appId":"robot"}`, "build.stagefile.yaml": "not-valid: [\n"},
		{"wendy.json": `{"appId":"robot"}`, "Dockerfile.a": "FROM scratch\n", "Dockerfile.b": "FROM scratch\n"},
		{"wendy.json": `{"appId":"robot"}`, "Dockerfile": "FROM scratch\nBOGUS instruction\n"},
		{"wendy.json": `{"appId":"robot"}`, "Dockerfile": "ARG FOO=bar\n"},
	} {
		root := mcpProjectFixture(t, files)
		result, err := validateMCPProject(context.Background(), wendymcp.ProjectValidationOptions{ProjectPath: root})
		if err != nil || result.Valid {
			t.Fatalf("invalid build accepted: %+v %v", result, err)
		}
	}
}

func TestMCPProjectNativeExecutableMappingsAndFeatureGate(t *testing.T) {
	for _, executable := range []bool{false, true} {
		root := mcpProjectFixture(t, map[string]string{"wendy.json": `{"appId":"native","platform":"darwin","run":{"command":"bin/app"},"files":[{"path":"source.sh","to":"bin/app"}]}`, "source.sh": "#!/bin/sh\nexit 0\n", "bin/app": "unmapped placeholder"})
		if executable {
			if err := os.Chmod(filepath.Join(root, "source.sh"), 0700); err != nil {
				t.Fatal(err)
			}
		}
		result, err := validateMCPProject(context.Background(), wendymcp.ProjectValidationOptions{ProjectPath: root})
		if err != nil || result.Valid != executable {
			t.Fatalf("executable=%v validation: %+v %v", executable, result, err)
		}
		if executable {
			result, err = validateMCPProject(context.Background(), wendymcp.ProjectValidationOptions{ProjectPath: root, Device: &agentpb.GetAgentVersionResponse{Os: "darwin", CpuArchitecture: "arm64"}})
			if err != nil || result.Valid || result.Compatibility.Status != "incompatible" {
				t.Fatalf("missing native feature passed: %+v %v", result, err)
			}
		}
	}
}

func TestMCPProjectComposeIgnoresCompanionPlatformLikeCLI(t *testing.T) {
	root := mcpProjectFixture(t, map[string]string{"wendy.json": `{"appId":"robot","platform":"darwin"}`, "compose.yaml": "services:\n  api:\n    image: nginx\n"})
	result, err := validateMCPProject(context.Background(), wendymcp.ProjectValidationOptions{ProjectPath: root, Device: &agentpb.GetAgentVersionResponse{Os: "linux", CpuArchitecture: "arm64"}})
	if err != nil || !result.Valid || result.Compatibility.Status != "compatible" {
		t.Fatalf("Compose platform mismatch: %+v %v", result, err)
	}
}

func TestMCPProjectRejectsLiteralMissingFilesAndEscapingBuildFile(t *testing.T) {
	root := mcpProjectFixture(t, map[string]string{"wendy.json": `{"appId":"robot","files":[{"path":"*.py"}]}`, "Dockerfile": "FROM scratch\n", "main.py": "print('hi')"})
	result, err := validateMCPProject(context.Background(), wendymcp.ProjectValidationOptions{ProjectPath: root})
	if err != nil || result.Valid {
		t.Fatalf("unsupported wildcard passed: %+v %v", result, err)
	}
	outside := mcpProjectFixture(t, map[string]string{"Dockerfile": "FROM scratch\n"})
	root = mcpProjectFixture(t, map[string]string{"wendy.json": `{"appId":"robot"}`})
	if err := os.Symlink(filepath.Join(outside, "Dockerfile"), filepath.Join(root, "Dockerfile")); err != nil {
		t.Fatal(err)
	}
	result, err = validateMCPProject(context.Background(), wendymcp.ProjectValidationOptions{ProjectPath: root})
	if err != nil || result.Valid {
		t.Fatalf("escaping Dockerfile passed: %+v %v", result, err)
	}
}
