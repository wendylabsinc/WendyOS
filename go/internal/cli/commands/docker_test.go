package commands

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func mustDetectProjectType(t *testing.T, dir string) string {
	t.Helper()
	got, err := detectProjectType(dir)
	if err != nil {
		t.Fatalf("detectProjectType unexpected error: %v", err)
	}
	return got
}

func TestDetectProjectType_Dockerfile(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "Dockerfile"), []byte("FROM alpine"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := mustDetectProjectType(t, dir); got != "docker" {
		t.Errorf("detectProjectType = %q; want %q", got, "docker")
	}
}

func TestDetectProjectType_PackageSwift(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "Package.swift"), []byte("// swift"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := mustDetectProjectType(t, dir); got != "swift" {
		t.Errorf("detectProjectType = %q; want %q", got, "swift")
	}
}

func TestDetectProjectType_RequirementsTxt(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "requirements.txt"), []byte("flask"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := mustDetectProjectType(t, dir); got != "python" {
		t.Errorf("detectProjectType = %q; want %q", got, "python")
	}
}

func TestDetectProjectType_SetupPy(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "setup.py"), []byte("setup()"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := mustDetectProjectType(t, dir); got != "python" {
		t.Errorf("detectProjectType = %q; want %q", got, "python")
	}
}

func TestDetectProjectType_PyprojectToml(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "pyproject.toml"), []byte("[tool.poetry]"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := mustDetectProjectType(t, dir); got != "python" {
		t.Errorf("detectProjectType = %q; want %q", got, "python")
	}
}

func TestDetectProjectType_Unknown(t *testing.T) {
	dir := t.TempDir()
	if got := mustDetectProjectType(t, dir); got != "unknown" {
		t.Errorf("detectProjectType = %q; want %q", got, "unknown")
	}
}

func TestDetectProjectType_DockerfileTakesPrecedence(t *testing.T) {
	dir := t.TempDir()
	// Create both Dockerfile and requirements.txt; Dockerfile should win.
	for _, name := range []string{"Dockerfile", "requirements.txt"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if got := mustDetectProjectType(t, dir); got != "docker" {
		t.Errorf("detectProjectType = %q; want %q (Dockerfile should take precedence)", got, "docker")
	}
}

func TestDetectProjectType_XcodeOnly(t *testing.T) {
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, "MyApp.xcodeproj"), 0o755); err != nil {
		t.Fatal(err)
	}
	if got := mustDetectProjectType(t, dir); got != "xcode" {
		t.Errorf("detectProjectType = %q; want %q", got, "xcode")
	}
}

func TestDetectProjectType_SwiftPMWinsOverXcode(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "Package.swift"), []byte("// swift"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(dir, "MyApp.xcodeproj"), 0o755); err != nil {
		t.Fatal(err)
	}
	if got := mustDetectProjectType(t, dir); got != "swift" {
		t.Errorf("detectProjectType = %q; want %q (Package.swift should take precedence)", got, "swift")
	}
}

func TestDetectProjectType_MultipleXcodeprojs_Error(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"First.xcodeproj", "Second.xcodeproj"} {
		if err := os.Mkdir(filepath.Join(dir, name), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	_, err := detectProjectType(dir)
	if err == nil {
		t.Fatal("detectProjectType expected error for multiple .xcodeproj dirs, got nil")
	}
	if !strings.Contains(err.Error(), "multiple .xcodeproj") {
		t.Errorf("expected 'multiple .xcodeproj' in error, got: %v", err)
	}
}

func TestResolveDetectedBuildOption_PrefersDockerfileOverSwift(t *testing.T) {
	options := []BuildOption{
		{Label: "Dockerfile", Type: "docker", File: "Dockerfile"},
		{Label: "Package.swift (Swift)", Type: "swift", File: "Package.swift"},
	}

	got, err := resolveDetectedBuildOption(options, "")
	if err != nil {
		t.Fatalf("resolveDetectedBuildOption: %v", err)
	}
	if got == nil || got.Type != "docker" || got.File != "Dockerfile" {
		t.Fatalf("got %+v, want Dockerfile docker option", got)
	}
}

func TestResolveDetectedBuildOption_PrefersDockerfileOverPython(t *testing.T) {
	options := []BuildOption{
		{Label: "Dockerfile", Type: "docker", File: "Dockerfile"},
		{Label: "requirements.txt (Python)", Type: "python", File: "requirements.txt"},
	}

	got, err := resolveDetectedBuildOption(options, "")
	if err != nil {
		t.Fatalf("resolveDetectedBuildOption: %v", err)
	}
	if got == nil || got.Type != "docker" || got.File != "Dockerfile" {
		t.Fatalf("got %+v, want Dockerfile docker option", got)
	}
}

func TestPreferredBuildOption_InteractiveMultipleDockerfilesDoesNotAutoPrefer(t *testing.T) {
	options := []BuildOption{
		{Label: "Dockerfile", Type: "docker", File: "Dockerfile"},
		{Label: "Dockerfile.dev", Type: "docker", File: "Dockerfile.dev"},
		{Label: "Package.swift (Swift)", Type: "swift", File: "Package.swift"},
	}

	got := preferredBuildOption(options, true)
	if got != nil {
		t.Fatalf("got %+v, want nil so the picker can choose among Dockerfiles", got)
	}
}

func TestBuildOptionForType_DockerUsesExactDockerfile(t *testing.T) {
	options := []BuildOption{
		{Label: "Dockerfile.dev", Type: "docker", File: "Dockerfile.dev"},
		{Label: "Dockerfile", Type: "docker", File: "Dockerfile"},
		{Label: "Package.swift (Swift)", Type: "swift", File: "Package.swift"},
	}

	got, err := buildOptionForType(options, "docker", false)
	if err != nil {
		t.Fatalf("buildOptionForType: %v", err)
	}
	if got == nil || got.File != "Dockerfile" {
		t.Fatalf("got %+v, want Dockerfile", got)
	}
}

func TestResolveRunProjectType_DefaultPrefersDockerfile(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"Dockerfile", "Package.swift"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	got, err := resolveRunProjectType(dir, "")
	if err != nil {
		t.Fatalf("resolveRunProjectType: %v", err)
	}
	if got != "docker" {
		t.Fatalf("got %q, want docker", got)
	}
}

func TestResolveRunProjectType_SwiftOverride(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"Dockerfile", "Package.swift"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	got, err := resolveRunProjectType(dir, "swift")
	if err != nil {
		t.Fatalf("resolveRunProjectType: %v", err)
	}
	if got != "swift" {
		t.Fatalf("got %q, want swift", got)
	}
}

func TestResolveRunProjectType_PythonOverride(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"Dockerfile", "requirements.txt"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	got, err := resolveRunProjectType(dir, "python")
	if err != nil {
		t.Fatalf("resolveRunProjectType: %v", err)
	}
	if got != "python" {
		t.Fatalf("got %q, want python", got)
	}
}

func TestResolveRunProjectType_InvalidOverride(t *testing.T) {
	dir := t.TempDir()
	_, err := resolveRunProjectType(dir, "ruby")
	if err == nil {
		t.Fatal("expected error for invalid run build type override")
	}
	if !strings.Contains(err.Error(), `invalid value "ruby" for --build-type`) {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestResolveRunProjectType_PropagatesMarkerStatErrors(t *testing.T) {
	dir := t.TempDir()
	notDir := filepath.Join(dir, "not-a-dir")
	if err := os.WriteFile(notDir, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	_, err := resolveRunProjectType(notDir, "docker")
	if err == nil {
		t.Fatal("expected stat error for invalid project path")
	}
	if !strings.Contains(err.Error(), "checking for") {
		t.Fatalf("expected wrapped stat error, got %v", err)
	}
}

func TestGeneratePythonDockerfile_WithRequirements(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "requirements.txt"), []byte("flask"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "app.py"), []byte("print('hi')"), 0o644); err != nil {
		t.Fatal(err)
	}

	path, err := generatePythonDockerfile(dir)
	if err != nil {
		t.Fatalf("generatePythonDockerfile: %v", err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading generated Dockerfile: %v", err)
	}
	content := string(data)

	expectations := []string{
		"FROM python:3.11-slim",
		"WORKDIR /app",
		"COPY requirements.txt .",
		"RUN pip install --no-cache-dir -r requirements.txt",
		"COPY . .",
		`CMD ["python", "app.py"]`,
	}
	for _, exp := range expectations {
		if !strings.Contains(content, exp) {
			t.Errorf("generated Dockerfile missing %q\nGot:\n%s", exp, content)
		}
	}
}

func TestGeneratePythonDockerfile_WithoutRequirements_MainPy(t *testing.T) {
	dir := t.TempDir()
	// Only main.py, no requirements.txt, no app.py.
	if err := os.WriteFile(filepath.Join(dir, "main.py"), []byte("print('hi')"), 0o644); err != nil {
		t.Fatal(err)
	}

	path, err := generatePythonDockerfile(dir)
	if err != nil {
		t.Fatalf("generatePythonDockerfile: %v", err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading generated Dockerfile: %v", err)
	}
	content := string(data)

	if strings.Contains(content, "requirements.txt") {
		t.Error("Dockerfile should not mention requirements.txt when it does not exist")
	}
	if !strings.Contains(content, `CMD ["python", "main.py"]`) {
		t.Errorf("expected CMD with main.py, got:\n%s", content)
	}
}

func TestGeneratePythonDockerfile_FallbackEntrypoint(t *testing.T) {
	dir := t.TempDir()
	// No app.py or main.py; should fall back to app.py as default.

	_, err := generatePythonDockerfile(dir)
	if err != nil {
		t.Fatalf("generatePythonDockerfile: %v", err)
	}

	data, err := os.ReadFile(filepath.Join(dir, "Dockerfile"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `CMD ["python", "app.py"]`) {
		t.Errorf("expected fallback to app.py entrypoint, got:\n%s", string(data))
	}
}

func TestRegistryHost_IPv4(t *testing.T) {
	got := registryHost("192.168.1.5", 5000)
	if got != "192.168.1.5:5000" {
		t.Errorf("registryHost IPv4 = %q, want %q", got, "192.168.1.5:5000")
	}
}

func TestRegistryHost_IPv6Global(t *testing.T) {
	got := registryHost("2001:db8::1", 5000)
	if got != "[2001:db8::1]:5000" {
		t.Errorf("registryHost IPv6 global = %q, want %q", got, "[2001:db8::1]:5000")
	}
}

func TestRegistryHost_IPv6LinkLocalWithZone(t *testing.T) {
	got := registryHost("fe80::2ecf:67ff:feba:6cca%en0", 5000)
	// Zone ID must be stripped — it's host-specific and unusable in containers.
	if got != "[fe80::2ecf:67ff:feba:6cca]:5000" {
		t.Errorf("registryHost IPv6 link-local+zone = %q, want %q", got, "[fe80::2ecf:67ff:feba:6cca]:5000")
	}
}

func TestRegistryHost_IPv6LinkLocalNoZone(t *testing.T) {
	got := registryHost("fe80::1", 5000)
	if got != "[fe80::1]:5000" {
		t.Errorf("registryHost IPv6 link-local no zone = %q, want %q", got, "[fe80::1]:5000")
	}
}

func TestSplitIPv6RegistryAddr_IPv6WithZone(t *testing.T) {
	eff, ip := splitIPv6RegistryAddr("[fe80::1%en0]:5000")
	if eff != "wendy-registry:5000" {
		t.Errorf("effectiveAddr = %q, want %q", eff, "wendy-registry:5000")
	}
	if ip != "fe80::1" {
		t.Errorf("ipv6IP = %q, want %q (zone stripped)", ip, "fe80::1")
	}
}

func TestSplitIPv6RegistryAddr_IPv6NoZone(t *testing.T) {
	eff, ip := splitIPv6RegistryAddr("[2001:db8::1]:5000")
	if eff != "wendy-registry:5000" {
		t.Errorf("effectiveAddr = %q, want %q", eff, "wendy-registry:5000")
	}
	if ip != "2001:db8::1" {
		t.Errorf("ipv6IP = %q, want %q", ip, "2001:db8::1")
	}
}

func TestSplitIPv6RegistryAddr_IPv4Passthrough(t *testing.T) {
	eff, ip := splitIPv6RegistryAddr("192.168.1.5:5000")
	if eff != "192.168.1.5:5000" {
		t.Errorf("effectiveAddr = %q, want unchanged", eff)
	}
	if ip != "" {
		t.Errorf("ipv6IP = %q, want empty for IPv4", ip)
	}
}

func TestSplitIPv6RegistryAddr_HostnamePassthrough(t *testing.T) {
	eff, ip := splitIPv6RegistryAddr("wendy-registry:5000")
	if eff != "wendy-registry:5000" {
		t.Errorf("effectiveAddr = %q, want unchanged", eff)
	}
	if ip != "" {
		t.Errorf("ipv6IP = %q, want empty for hostname", ip)
	}
}

func TestResolveRegistryIP_StripZone(t *testing.T) {
	got := resolveRegistryIP("fe80::1%eth0")
	if got != "fe80::1" {
		t.Errorf("resolveRegistryIP zone = %q, want %q", got, "fe80::1")
	}
}

func TestResolveRegistryIP_IPv4Passthrough(t *testing.T) {
	got := resolveRegistryIP("10.0.0.1")
	if got != "10.0.0.1" {
		t.Errorf("resolveRegistryIP IPv4 = %q, want %q", got, "10.0.0.1")
	}
}

func TestIsLinkLocalIP(t *testing.T) {
	tests := []struct {
		ip   string
		want bool
	}{
		{"169.254.1.1", true},
		{"169.254.189.250", true},
		{"192.168.1.5", false},
		{"10.0.0.1", false},
		{"fe80::1", true},
		{"[fe80::1]", true},
		{"2001:db8::1", false},
		{"not-an-ip", false},
	}
	for _, tt := range tests {
		if got := isLinkLocalIP(tt.ip); got != tt.want {
			t.Errorf("isLinkLocalIP(%q) = %v, want %v", tt.ip, got, tt.want)
		}
	}
}

func TestStartRegistryProxy(t *testing.T) {
	// Start a fake "registry" server.
	fakeRegistry := make(chan string, 1)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		buf := make([]byte, 64)
		n, _ := conn.Read(buf)
		fakeRegistry <- string(buf[:n])
		conn.Write([]byte("OK"))
	}()

	// Start the proxy pointing at the fake registry.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	proxy, err := startRegistryProxy(ctx, ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer proxy.Close()

	// Connect through the proxy.
	conn, err := net.Dial("tcp", "127.0.0.1:"+strconv.Itoa(proxy.Port()))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	conn.Write([]byte("PUSH"))
	got := <-fakeRegistry
	if got != "PUSH" {
		t.Errorf("proxy forwarded %q, want %q", got, "PUSH")
	}
}

func TestFindIPv4ViaNeighborTable_UnknownAddress(t *testing.T) {
	// This test would invoke findIPv4ViaNeighborTable, which may spawn real ndp/arp/ip
	// commands and read the host's neighbor tables, making it environment-dependent.
	// Skip it to avoid flakiness/timeouts in unit test environments.
	t.Skip("disabled: findIPv4ViaNeighborTable depends on host neighbor tables and OS commands")
}
