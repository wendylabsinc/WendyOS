package services

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type fakeProc struct {
	dir               string
	defaultConfigPath string
}

func newFakeProc(t *testing.T) *fakeProc {
	t.Helper()
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "self", "ns"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("mnt:[1]", filepath.Join(dir, "self", "ns", "mnt")); err != nil {
		t.Fatal(err)
	}
	return &fakeProc{
		dir:               dir,
		defaultConfigPath: filepath.Join(t.TempDir(), "absent-buildkitd.toml"),
	}
}

func (p *fakeProc) add(t *testing.T, pid, cwd string, argv ...string) {
	t.Helper()
	pidDir := filepath.Join(p.dir, pid)
	if err := os.MkdirAll(pidDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(pidDir, "ns"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("mnt:[1]", filepath.Join(pidDir, "ns", "mnt")); err != nil {
		t.Fatal(err)
	}
	var cmdline []byte
	for _, arg := range argv {
		cmdline = append(cmdline, arg...)
		cmdline = append(cmdline, 0)
	}
	if err := os.WriteFile(filepath.Join(pidDir, "cmdline"), cmdline, 0o644); err != nil {
		t.Fatal(err)
	}
	// A root symlink makes absolute paths resolve as they do through real
	// /proc/<pid>/root, while still allowing tests to use ordinary temp files.
	if err := os.Symlink("/", filepath.Join(pidDir, "root")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(cwd, filepath.Join(pidDir, "cwd")); err != nil {
		t.Fatal(err)
	}
}

func (p *fakeProc) activateUnixSocket(t *testing.T, pid, socketPath, inode string) {
	t.Helper()
	pidDir := filepath.Join(p.dir, pid)
	if err := os.MkdirAll(filepath.Join(pidDir, "net"), 0o755); err != nil {
		t.Fatal(err)
	}
	line := "0000000000000000: 00000002 00000000 00010000 0001 01 " + inode + " " + socketPath + "\n"
	if err := os.WriteFile(filepath.Join(pidDir, "net", "unix"), []byte(line), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(pidDir, "fd"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("socket:["+inode+"]", filepath.Join(pidDir, "fd", "3")); err != nil {
		t.Fatal(err)
	}
}

func mustBuildkitRoot(t *testing.T, proc *fakeProc, address string) buildkitRootLocation {
	t.Helper()
	location, ok := buildkitRoot(proc.dir, address, proc.defaultConfigPath)
	if !ok {
		t.Fatal("BuildKit root inspection unexpectedly failed")
	}
	return location
}

func writeBuildkitConfig(t *testing.T, contents string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "buildkitd.toml")
	if err := os.WriteFile(path, []byte(contents), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestBuildkitRoot_SelectsDaemonForRequestedAddress(t *testing.T) {
	proc := newFakeProc(t)
	proc.add(t, "10", "/", "buildkitd", "--addr", "unix:///run/other.sock", "--root", "/wrong")
	proc.add(t, "20", "/", "buildkitd", "--addr", DefaultBuildkitAddress, "--root", "/data/buildkit/right")

	if got := mustBuildkitRoot(t, proc, DefaultBuildkitAddress).displayPath; got != "/data/buildkit/right" {
		t.Fatalf("got %q, want the root belonging to the requested address", got)
	}
}

func TestBuildkitRoot_MatchesSystemdActivatedDaemonBySocketOwnership(t *testing.T) {
	proc := newFakeProc(t)
	proc.add(t, "42", "/", "buildkitd", "--addr", "fd://", "--root", "/data/buildkit/systemd")
	proc.activateUnixSocket(t, "42", "/run/buildkit/buildkitd.sock", "43355")

	if got := mustBuildkitRoot(t, proc, DefaultBuildkitAddress).displayPath; got != "/data/buildkit/systemd" {
		t.Fatalf("got %q, want root from daemon holding the inherited socket", got)
	}
}

func TestBuildkitRoot_IgnoresFDActivatedDaemonForAnotherSocket(t *testing.T) {
	proc := newFakeProc(t)
	proc.add(t, "10", "/", "buildkitd", "--addr", "fd://", "--root", "/wrong")
	proc.activateUnixSocket(t, "10", "/run/buildkit/other.sock", "11111")
	proc.add(t, "20", "/", "buildkitd", "--addr", DefaultBuildkitAddress, "--root", "/right")

	if got := mustBuildkitRoot(t, proc, DefaultBuildkitAddress).displayPath; got != "/right" {
		t.Fatalf("got %q, want daemon for the requested socket", got)
	}
}

func TestBuildkitRoot_FDAddressWithoutSocketEvidenceIsUnknown(t *testing.T) {
	proc := newFakeProc(t)
	proc.add(t, "42", "/", "buildkitd", "--addr", "fd://", "--root", "/data/buildkit")

	if _, ok := buildkitRoot(proc.dir, DefaultBuildkitAddress, proc.defaultConfigPath); ok {
		t.Fatal("fd:// without readable socket ownership must be unknown")
	}
}

func TestBuildkitRoot_RefusesAmbiguousMatchingDaemons(t *testing.T) {
	proc := newFakeProc(t)
	proc.add(t, "10", "/", "buildkitd", "--addr", DefaultBuildkitAddress, "--root", "/first")
	proc.add(t, "20", "/", "buildkitd", "--addr", DefaultBuildkitAddress, "--root", "/second")

	if _, ok := buildkitRoot(proc.dir, DefaultBuildkitAddress, proc.defaultConfigPath); ok {
		t.Fatal("two daemons claiming the requested address must be ambiguous")
	}
}

func TestBuildkitRoot_RefusesUnknownDaemonAddress(t *testing.T) {
	proc := newFakeProc(t)
	proc.add(t, "10", "/", "buildkitd", "--config", "/removed.toml")
	proc.add(t, "20", "/", "buildkitd", "--addr", DefaultBuildkitAddress, "--root", "/data/buildkit")

	if _, ok := buildkitRoot(proc.dir, DefaultBuildkitAddress, proc.defaultConfigPath); ok {
		t.Fatal("an uninspectable daemon may own the requested address, so the result must be unknown")
	}
}

func TestBuildkitRoot_UsesAddressAndRootFromCustomConfig(t *testing.T) {
	config := writeBuildkitConfig(t, `
root = '/data/buildkit/from-config'
[grpc]
  address = ['unix:///run/buildkit/custom.sock']
`)
	proc := newFakeProc(t)
	proc.add(t, "42", "/", "/usr/local/bin/buildkitd", "--config", config)

	if got := mustBuildkitRoot(t, proc, "unix:///run/buildkit/custom.sock").displayPath; got != "/data/buildkit/from-config" {
		t.Fatalf("got %q, want root from the selected config", got)
	}
}

func TestBuildkitRoot_ReadsConfigEqualsForm(t *testing.T) {
	config := writeBuildkitConfig(t, `
root = "/data/buildkit/equals-config"
[grpc]
  address = ["unix:///run/buildkit/equals.sock"]
`)
	proc := newFakeProc(t)
	proc.add(t, "42", "/", "buildkitd", "--config="+config)

	if got := mustBuildkitRoot(t, proc, "unix:///run/buildkit/equals.sock").displayPath; got != "/data/buildkit/equals-config" {
		t.Fatalf("got %q, want --config= file root", got)
	}
}

func TestBuildkitRoot_RootFlagBeatsCustomConfig(t *testing.T) {
	config := writeBuildkitConfig(t, `root = "/wrong"`)
	proc := newFakeProc(t)
	proc.add(t, "42", "/", "buildkitd", "--config", config, "--root", "/data/buildkit/flag-root")

	if got := mustBuildkitRoot(t, proc, DefaultBuildkitAddress).displayPath; got != "/data/buildkit/flag-root" {
		t.Fatalf("got %q, want --root to override config", got)
	}
}

func TestBuildkitRoot_ResolvesRelativeRootAgainstDaemonCWD(t *testing.T) {
	cwd := t.TempDir()
	proc := newFakeProc(t)
	proc.add(t, "42", cwd, "buildkitd", "--addr", DefaultBuildkitAddress, "--root", "cache/buildkit")

	location := mustBuildkitRoot(t, proc, DefaultBuildkitAddress)
	if want := filepath.Join(cwd, "cache/buildkit"); location.displayPath != want {
		t.Fatalf("got %q, want %q", location.displayPath, want)
	}
	if !strings.Contains(location.statPath, filepath.Join("42", "cwd", "cache", "buildkit")) {
		t.Fatalf("stat path %q does not traverse the daemon cwd", location.statPath)
	}
}

func TestBuildkitRoot_ResolvesRelativeConfigAgainstDaemonCWD(t *testing.T) {
	cwd := t.TempDir()
	if err := os.WriteFile(filepath.Join(cwd, "custom.toml"), []byte(`
root = "state"
[grpc]
  address = ["unix:///run/buildkit/relative.sock"]
`), 0o644); err != nil {
		t.Fatal(err)
	}
	proc := newFakeProc(t)
	proc.add(t, "42", cwd, "buildkitd", "--config", "custom.toml")

	if got := mustBuildkitRoot(t, proc, "unix:///run/buildkit/relative.sock").displayPath; got != filepath.Join(cwd, "state") {
		t.Fatalf("got %q, want root relative to daemon cwd", got)
	}
}

func TestBuildkitRoot_LastScalarFlagWins(t *testing.T) {
	proc := newFakeProc(t)
	proc.add(t, "42", "/", "buildkitd",
		"--addr", "unix:///run/first.sock", "--addr=unix:///run/second.sock",
		"--root", "/first", "--root=/second")

	if got := mustBuildkitRoot(t, proc, "unix:///run/second.sock").displayPath; got != "/second" {
		t.Fatalf("got %q, want the last --root value", got)
	}
}

func TestBuildkitRoot_IgnoresNonDaemons(t *testing.T) {
	proc := newFakeProc(t)
	proc.add(t, "10", "/", "buildctl", "--addr", DefaultBuildkitAddress, "--root", "/wrong")
	proc.add(t, "20", "/", "/bin/sh", "-c", "buildkitd --root /also-wrong")
	proc.add(t, "30", "/", "buildkitd", "--addr", DefaultBuildkitAddress, "--root", "/right")

	if got := mustBuildkitRoot(t, proc, DefaultBuildkitAddress).displayPath; got != "/right" {
		t.Fatalf("got %q, want the actual daemon", got)
	}
}

func TestBuildkitRoot_IgnoresUninspectableRootForOtherExplicitAddress(t *testing.T) {
	proc := newFakeProc(t)
	proc.add(t, "10", "/", "buildkitd", "--addr", "unix:///run/other.sock", "--config", "/removed.toml")
	proc.add(t, "20", "/", "buildkitd", "--addr", DefaultBuildkitAddress, "--root", "/right")

	if got := mustBuildkitRoot(t, proc, DefaultBuildkitAddress).displayPath; got != "/right" {
		t.Fatalf("got %q, want matching daemon despite unrelated daemon's missing root", got)
	}
}

func TestBuildkitRoot_NoDaemonIsUnknown(t *testing.T) {
	proc := newFakeProc(t)
	if _, ok := buildkitRoot(proc.dir, DefaultBuildkitAddress, proc.defaultConfigPath); ok {
		t.Fatal("a socket with no identifiable daemon must not imply the default root")
	}
}

func TestBuildkitRoot_FallsBackToDocumentedDefaultsHermetically(t *testing.T) {
	proc := newFakeProc(t)
	proc.add(t, "42", "/", "buildkitd")

	if got := mustBuildkitRoot(t, proc, DefaultBuildkitAddress).displayPath; got != defaultBuildkitRoot {
		t.Fatalf("got %q, want %q", got, defaultBuildkitRoot)
	}
}

func TestBuildkitRoot_UsesInjectedDefaultConfigHermetically(t *testing.T) {
	proc := newFakeProc(t)
	proc.defaultConfigPath = writeBuildkitConfig(t, `root = "/data/buildkit/default-config"`)
	proc.add(t, "42", "/", "buildkitd")

	if got := mustBuildkitRoot(t, proc, DefaultBuildkitAddress).displayPath; got != "/data/buildkit/default-config" {
		t.Fatalf("got %q, want root from injected default config", got)
	}
}

func TestBuildkitRoot_MissingOrBrokenCustomConfigIsUnknown(t *testing.T) {
	for _, config := range []string{
		filepath.Join(t.TempDir(), "removed.toml"),
		writeBuildkitConfig(t, `root = "unterminated`),
	} {
		proc := newFakeProc(t)
		proc.add(t, "42", "/", "buildkitd", "--config", config)
		if _, ok := buildkitRoot(proc.dir, DefaultBuildkitAddress, proc.defaultConfigPath); ok {
			t.Fatalf("config %q should make inspection unknown", config)
		}
	}
}

func TestBuildkitRoot_BrokenDefaultConfigIsUnknown(t *testing.T) {
	proc := newFakeProc(t)
	proc.defaultConfigPath = writeBuildkitConfig(t, `root = "unterminated`)
	proc.add(t, "42", "/", "buildkitd")

	if _, ok := buildkitRoot(proc.dir, DefaultBuildkitAddress, proc.defaultConfigPath); ok {
		t.Fatal("a malformed default config must not fall through to rootful defaults")
	}
}

func TestRootFromConfig_ParsesRealTOMLAndOnlyTopLevelRoot(t *testing.T) {
	for _, tc := range []struct {
		name     string
		contents string
		want     string
	}{
		{"double quoted", `root = "/data/double" # comment`, "/data/double"},
		{"single quoted", `root = '/data/single'`, "/data/single"},
		{"nested ignored", "[worker.oci]\nroot = \"/not-daemon-root\"\n", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := rootFromConfig(writeBuildkitConfig(t, tc.contents))
			if err != nil {
				t.Fatal(err)
			}
			if got != tc.want {
				t.Fatalf("got %q, want %q", got, tc.want)
			}
		})
	}
}

func TestRootFromConfig_ReportsMissingAndMalformedFiles(t *testing.T) {
	if _, err := rootFromConfig(filepath.Join(t.TempDir(), "absent.toml")); !os.IsNotExist(err) {
		t.Fatalf("got %v, want missing-file error", err)
	}
	if _, err := rootFromConfig(writeBuildkitConfig(t, `root = "unterminated`)); err == nil {
		t.Fatal("malformed TOML must return an error")
	}
}

func TestBuildkitRootSpace_WalksToExistingAncestor(t *testing.T) {
	deep := filepath.Join(t.TempDir(), "does", "not", "exist", "yet")
	total, free := buildkitRootSpace(deep)
	if total == 0 {
		t.Fatal("want the ancestor filesystem size")
	}
	if free > total {
		t.Fatalf("free %d exceeds total %d", free, total)
	}
}

func TestBuildkitRootSpace_DoesNotWalkPastProcessBoundary(t *testing.T) {
	boundary := filepath.Join(t.TempDir(), "vanished-proc-root")
	if total, free := buildkitRootSpaceWithin(filepath.Join(boundary, "data", "buildkit"), boundary); total != 0 || free != 0 {
		t.Fatalf("got (%d, %d), want unknown after the process namespace vanished", total, free)
	}
}

func TestBuildkitRootSpace_EmptyPathIsUnknown(t *testing.T) {
	if total, free := buildkitRootSpace(""); total != 0 || free != 0 {
		t.Fatalf("got (%d, %d), want zeroes", total, free)
	}
}

func (p *fakeProc) isolateSocket(t *testing.T, pid, socketPath string, shared bool) {
	t.Helper()
	pidDir := filepath.Join(p.dir, pid)
	root := t.TempDir()
	for path, target := range map[string]string{"root": root, "ns/mnt": "mnt:[2]"} {
		link := filepath.Join(pidDir, path)
		if err := os.Remove(link); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(target, link); err != nil {
			t.Fatal(err)
		}
	}
	isolated := pathInProcess(pidDir, socketPath)
	if err := os.MkdirAll(filepath.Dir(isolated), 0o755); err != nil {
		t.Fatal(err)
	}
	if shared {
		// A hard link models the same filesystem object visible in two roots.
		if err := os.Link(socketPath, isolated); err != nil {
			t.Fatal(err)
		}
	} else {
		if err := os.WriteFile(isolated, nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

func TestBuildkitRoot_DistinguishesContainerSocketWithSamePath(t *testing.T) {
	for _, inherited := range []bool{false, true} {
		t.Run(map[bool]string{false: "explicit", true: "inherited"}[inherited], func(t *testing.T) {
			proc := newFakeProc(t)
			socketPath := filepath.Join(t.TempDir(), "buildkitd.sock")
			if err := os.WriteFile(socketPath, nil, 0o600); err != nil {
				t.Fatal(err)
			}
			address := "unix://" + socketPath
			proc.add(t, "10", "/", "buildkitd", "--addr", address, "--root", "/host-cache")
			containerAddress := address
			if inherited {
				containerAddress = "fd://"
			}
			proc.add(t, "20", "/", "buildkitd", "--addr", containerAddress, "--root", "/container-cache")
			proc.isolateSocket(t, "20", socketPath, false)
			proc.activateUnixSocket(t, "20", socketPath, "12345")
			if got := mustBuildkitRoot(t, proc, address).displayPath; got != "/host-cache" {
				t.Fatalf("got %q", got)
			}
		})
	}
}

func TestBuildkitRoot_AcceptsSocketSharedAcrossMountNamespaces(t *testing.T) {
	proc := newFakeProc(t)
	socketPath := filepath.Join(t.TempDir(), "buildkitd.sock")
	if err := os.WriteFile(socketPath, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	address := "unix://" + socketPath
	proc.add(t, "10", "/", "buildkitd", "--addr", address, "--root", "/container-cache")
	proc.isolateSocket(t, "10", socketPath, true)
	if got := mustBuildkitRoot(t, proc, address).displayPath; got != "/container-cache" {
		t.Fatalf("got %q", got)
	}
}

func TestBuildkitRoot_MissingContainerSocketRemainsUnknown(t *testing.T) {
	proc := newFakeProc(t)
	socketPath := filepath.Join(t.TempDir(), "buildkitd.sock")
	if err := os.WriteFile(socketPath, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	address := "unix://" + socketPath
	proc.add(t, "10", "/", "buildkitd", "--addr", address, "--root", "/host-cache")
	proc.add(t, "20", "/", "buildkitd", "--addr", address, "--root", "/container-cache")
	proc.isolateSocket(t, "20", socketPath, false)
	if err := os.Remove(pathInProcess(filepath.Join(proc.dir, "20"), socketPath)); err != nil {
		t.Fatal(err)
	}
	if _, ok := buildkitRoot(proc.dir, address, proc.defaultConfigPath); ok {
		t.Fatal("uninspectable container socket must remain unknown")
	}
}

func TestBuildkitRoot_RelativeSocketUsesDaemonWorkingDirectory(t *testing.T) {
	agentDir, daemonDir := t.TempDir(), t.TempDir()
	t.Chdir(agentDir)
	for _, dir := range []string{agentDir, daemonDir} {
		if err := os.WriteFile(filepath.Join(dir, "buildkit.sock"), nil, 0600); err != nil {
			t.Fatal(err)
		}
	}
	proc := newFakeProc(t)
	proc.add(t, "10", daemonDir, "buildkitd", "--addr", "unix://buildkit.sock")
	same, known := daemonSharesBuildkitSocket(filepath.Join(proc.dir, "10"), "buildkit.sock")
	if same || !known {
		t.Fatalf("same=%v known=%v; relative paths name different sockets", same, known)
	}
}
