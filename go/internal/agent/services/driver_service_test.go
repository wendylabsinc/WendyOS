package services

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"go.uber.org/zap"

	"github.com/wendylabsinc/wendy/go/internal/shared/sigverify"
	"google.golang.org/grpc"

	agentpbv2 "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
)

// newTestDriverService wires a DriverService against a temp /data store, a fixed
// kernel, an in-memory artifact fetcher, and true as the apply script so the
// verify/place/apply path runs without a device.
// testKernel is what newTestDriverService reports from uname and what the
// install-a/install-b fixtures declare, so an install lands in its bucket.
const testKernel = "6.6.0-test"

func newTestDriverService(t *testing.T, payload []byte) *DriverService {
	t.Helper()
	tmp := t.TempDir()
	return &DriverService{
		logger:            zap.NewNop(),
		verifier:          sigverify.DefaultVerifier,
		enabledDir:        filepath.Join(tmp, "enabled"),
		modulesDir:        filepath.Join(tmp, "modules-load.d"),
		bakedModulesDir:   filepath.Join(tmp, "baked-modules-load.d"),
		privatePayloadDir: filepath.Join(tmp, "private-payloads"),
		activeStateDir:    filepath.Join(tmp, "active-state"),
		applyScript:       testExecutable(t, "true"),
		unameR:            func() string { return "6.6.0-test" },
		// Nothing resident by default, so a test never inherits the host's modules.
		loadedModules: func() []string { return nil },
		httpGet: func(_ context.Context, _ string) (io.ReadCloser, error) {
			return io.NopCloser(bytes.NewReader(payload)), nil
		},
	}
}

func testExecutable(t *testing.T, name string) string {
	t.Helper()
	path, err := exec.LookPath(name)
	if err != nil {
		t.Fatalf("finding %s executable: %v", name, err)
	}
	return path
}

// driverImage returns a real add-on image. finalize reads the extension-release
// out of the .raw, so a placeholder byte string never reaches place/apply.
func driverImage(t *testing.T, variant string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", "install-"+variant+".raw"))
	if err != nil {
		t.Fatalf("reading add-on fixture: %v", err)
	}
	return b
}

func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func TestValidateDriverName(t *testing.T) {
	valid := []string{"wendyos-hello", "hailo_pci", "coral.apex", "a", "Driver-1"}
	for _, n := range valid {
		if err := validateDriverName(n); err != nil {
			t.Errorf("validateDriverName(%q) = %v, want nil", n, err)
		}
	}
	// A leading dash would become the apply script's subject argument.
	invalid := []string{"", ".", "..", "a/b", "a\\b", "../evil", "has space", "sneaky.raw/../x", "-x", "--help", "-"}
	for _, n := range invalid {
		if err := validateDriverName(n); err == nil {
			t.Errorf("validateDriverName(%q) = nil, want error", n)
		}
	}
}

func TestInstallFromURL_HappyPath(t *testing.T) {
	payload := driverImage(t, "a")
	svc := newTestDriverService(t, payload)

	err := svc.InstallFromURL(context.Background(), DriverInstallSpec{
		Name:          "wendyos-hello",
		KernelVersion: "6.6.0-test",
		SHA256:        sha256Hex(payload),
		ArtifactURL:   "https://example/wendyos-hello.raw",
		ModulesLoad:   []string{"wendyos_hello", "helper"},
	})
	if err != nil {
		t.Fatalf("InstallFromURL: %v", err)
	}

	// The .raw is placed under the verified name (== extension-release name).
	got, err := os.ReadFile(svc.rawPath(testKernel, "wendyos-hello"))
	if err != nil {
		t.Fatalf("reading placed .raw: %v", err)
	}
	if !bytes.Equal(got, payload) {
		t.Errorf("placed .raw = %q, want %q", got, payload)
	}
	// modules-load.d config lists the modules, one per line.
	conf, err := os.ReadFile(svc.confPath(testKernel, "wendyos-hello"))
	if err != nil {
		t.Fatalf("reading modules conf: %v", err)
	}
	if want := "wendyos_hello\nhelper\n"; string(conf) != want {
		t.Errorf("modules conf = %q, want %q", conf, want)
	}
}

func TestInstallFromURL_SHA256Mismatch(t *testing.T) {
	payload := []byte("real bytes")
	svc := newTestDriverService(t, payload)

	err := svc.InstallFromURL(context.Background(), DriverInstallSpec{
		Name:          "wendyos-hello",
		KernelVersion: "6.6.0-test",
		SHA256:        sha256Hex([]byte("different bytes")),
		ArtifactURL:   "https://example/x.raw",
	})
	if err == nil {
		t.Fatal("InstallFromURL: got nil error, want sha256 mismatch")
	}
	// Nothing must be placed on a failed verification.
	if _, statErr := os.Stat(svc.rawPath(testKernel, "wendyos-hello")); !os.IsNotExist(statErr) {
		t.Errorf(".raw was placed despite sha256 mismatch")
	}
}

func TestInstallFromURL_KernelMismatch(t *testing.T) {
	payload := []byte("bytes")
	svc := newTestDriverService(t, payload)

	err := svc.InstallFromURL(context.Background(), DriverInstallSpec{
		Name:          "wendyos-hello",
		KernelVersion: "9.9.9-other",
		SHA256:        sha256Hex(payload),
		ArtifactURL:   "https://example/x.raw",
	})
	if err == nil {
		t.Fatal("InstallFromURL: got nil error, want kernel mismatch")
	}
}

func TestInstallFromURL_BadName(t *testing.T) {
	svc := newTestDriverService(t, []byte("bytes"))
	err := svc.InstallFromURL(context.Background(), DriverInstallSpec{
		Name:          "../escape",
		KernelVersion: "6.6.0-test",
		ArtifactURL:   "https://example/x.raw",
	})
	if err == nil {
		t.Fatal("InstallFromURL: got nil error, want invalid-name error")
	}
}

func TestListDrivers_InstalledSet(t *testing.T) {
	svc := newTestDriverService(t, nil)
	if err := os.MkdirAll(svc.enabledDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(svc.modulesDir, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, n := range []string{"alpha", "beta"} {
		if err := os.WriteFile(filepath.Join(svc.enabledDir, n+".raw"), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(svc.modulesDir, "alpha.conf"), []byte("# comment\nalpha_mod\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	resp, err := svc.ListDrivers(context.Background(), &agentpbv2.ListDriversRequest{})
	if err != nil {
		t.Fatalf("ListDrivers: %v", err)
	}
	if resp.GetKernelVersion() != "6.6.0-test" {
		t.Errorf("kernel = %q, want 6.6.0-test", resp.GetKernelVersion())
	}
	got := map[string][]string{}
	for _, d := range resp.GetInstalled() {
		got[d.GetName()] = d.GetModulesLoad()
	}
	if _, ok := got["alpha"]; !ok {
		t.Errorf("alpha not in installed set: %v", got)
	}
	if _, ok := got["beta"]; !ok {
		t.Errorf("beta not in installed set: %v", got)
	}
	if mods := got["alpha"]; len(mods) != 1 || mods[0] != "alpha_mod" {
		t.Errorf("alpha modules = %v, want [alpha_mod] (comments/blank lines skipped)", mods)
	}
}

func TestListDrivers_BakedInModulesFallback(t *testing.T) {
	// Self-describing add-on: the .raw is installed but its module list is baked
	// in (surfaced at the merged path), with no /data override. ListDrivers must
	// still report the baked-in modules.
	svc := newTestDriverService(t, nil)
	for _, d := range []string{filepath.Dir(svc.rawPath(testKernel, "x")), svc.bakedModulesDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(svc.rawPath(testKernel, "wendyos-hello"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(svc.bakedModulesDir, "wendyos-hello.conf"), []byte("wendyos_hello\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	resp, err := svc.ListDrivers(context.Background(), &agentpbv2.ListDriversRequest{})
	if err != nil {
		t.Fatalf("ListDrivers: %v", err)
	}
	var got []string
	for _, d := range resp.GetInstalled() {
		if d.GetName() == "wendyos-hello" {
			got = d.GetModulesLoad()
		}
	}
	if len(got) != 1 || got[0] != "wendyos_hello" {
		t.Errorf("baked-in modules = %v, want [wendyos_hello]", got)
	}
}

func TestListDrivers_DataOverrideWinsOverBaked(t *testing.T) {
	// When both exist, the /data override takes precedence over the baked-in conf
	// (mirrors wendyos-sysext-apply).
	svc := newTestDriverService(t, nil)
	for _, d := range []string{svc.enabledDir, svc.modulesDir, svc.bakedModulesDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(svc.enabledDir, "d.raw"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(svc.modulesDir, "d.conf"), []byte("override_mod\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(svc.bakedModulesDir, "d.conf"), []byte("baked_mod\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	resp, err := svc.ListDrivers(context.Background(), &agentpbv2.ListDriversRequest{})
	if err != nil {
		t.Fatalf("ListDrivers: %v", err)
	}
	var got []string
	for _, d := range resp.GetInstalled() {
		if d.GetName() == "d" {
			got = d.GetModulesLoad()
		}
	}
	if len(got) != 1 || got[0] != "override_mod" {
		t.Errorf("modules = %v, want [override_mod] (/data override wins)", got)
	}
}

// The kernel comes from the image itself, not from anything recorded beside it.
func TestListDrivers_ReportsImageKernel(t *testing.T) {
	svc := newTestDriverService(t, nil)
	if err := os.MkdirAll(svc.enabledDir, 0o755); err != nil {
		t.Fatal(err)
	}
	hello, err := os.ReadFile(filepath.Join("testdata", fixtureName+".raw"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(svc.enabledDir, fixtureName+".raw"), hello, 0o644); err != nil {
		t.Fatal(err)
	}
	// An add-on whose image cannot be parsed must still be listed: hiding it
	// would make a broken install look like no install at all.
	if err := os.WriteFile(filepath.Join(svc.enabledDir, "broken.raw"), []byte("not a squashfs"), 0o644); err != nil {
		t.Fatal(err)
	}

	resp, err := svc.ListDrivers(context.Background(), &agentpbv2.ListDriversRequest{})
	if err != nil {
		t.Fatalf("ListDrivers: %v", err)
	}
	got := map[string]*agentpbv2.InstalledDriver{}
	for _, d := range resp.GetInstalled() {
		got[d.GetName()] = d
	}
	if k := got[fixtureName].GetKernelVersion(); k != fixtureKernel {
		t.Errorf("%s kernel = %q, want %q", fixtureName, k, fixtureKernel)
	}
	if got[fixtureName].GetUnreadable() {
		t.Errorf("%s reported unreadable", fixtureName)
	}
	broken, ok := got["broken"]
	if !ok {
		t.Fatal("broken.raw was not listed")
	}
	// Unreadable, not "pins no kernel": the two must stay distinguishable or a
	// corrupt add-on renders as healthy.
	if !broken.GetUnreadable() || broken.GetKernelVersion() != "" {
		t.Errorf("broken = (kernel %q, unreadable %v), want (\"\", true)",
			broken.GetKernelVersion(), broken.GetUnreadable())
	}
}

func TestValidateArtifactURL(t *testing.T) {
	// http stays allowed for on-prem and usb0 registries, but only once the host
	// is opted in (see TestValidateArtifactURL_ExtraHostsOptIn).
	t.Setenv(driverExtraHostsEnv, "registry.example,10.42.0.1")
	ok := []string{"https://registry.example/wendyos-hello.raw", "http://10.42.0.1:8000/x.raw"}
	for _, u := range ok {
		if err := validateArtifactURL(u); err != nil {
			t.Errorf("validateArtifactURL(%q) = %v, want nil", u, err)
		}
	}
	bad := []string{"file:///etc/passwd", "ftp://h/x", "gopher://h/x", "/no/scheme", "https://"}
	for _, u := range bad {
		if err := validateArtifactURL(u); err == nil {
			t.Errorf("validateArtifactURL(%q) = nil, want error", u)
		}
	}
}

func TestAllModulesLoaded(t *testing.T) {
	loaded := map[string]bool{"wendyos_hello": true, "foo": true}
	// modprobe treats '-' and '_' interchangeably; /proc/modules uses '_'.
	if !allModulesLoaded([]string{"wendyos-hello"}, loaded) {
		t.Error("allModulesLoaded should normalize '-' to '_'")
	}
	if allModulesLoaded([]string{"wendyos_hello", "missing"}, loaded) {
		t.Error("allModulesLoaded should be false when any module is not loaded")
	}
	if allModulesLoaded(nil, loaded) {
		t.Error("allModulesLoaded(nil) should be false (nothing declared to load)")
	}
}

func TestInstallFromURL_ApplyFailureRollsBack(t *testing.T) {
	payload := driverImage(t, "a")
	svc := newTestDriverService(t, payload)
	svc.applyScript = testExecutable(t, "false") // apply exits non-zero

	err := svc.InstallFromURL(context.Background(), DriverInstallSpec{
		Name:          "wendyos-hello",
		KernelVersion: "6.6.0-test",
		SHA256:        sha256Hex(payload),
		ArtifactURL:   "https://example/x.raw",
		ModulesLoad:   []string{"wendyos_hello"},
	})
	if err == nil {
		t.Fatal("InstallFromURL: got nil, want apply failure")
	}
	// Nothing must remain installed or declared after a failed apply.
	if _, statErr := os.Stat(svc.rawPath(testKernel, "wendyos-hello")); !os.IsNotExist(statErr) {
		t.Errorf(".raw was left behind after apply failure")
	}
	if _, statErr := os.Stat(svc.confPath(testKernel, "wendyos-hello")); !os.IsNotExist(statErr) {
		t.Errorf("modules-load conf was left behind after apply failure")
	}
}

// A failed reinstall must restore the working version, not delete it: the
// upgrade renames the new .raw over the old one before apply runs, so a naive
// rollback would destroy a driver that was fine before the operation started.
func TestInstallFromURL_ApplyFailureRestoresPreviousInstall(t *testing.T) {
	oldPayload := driverImage(t, "a")
	svc := newTestDriverService(t, oldPayload)

	if err := svc.InstallFromURL(context.Background(), DriverInstallSpec{
		Name:          "wendyos-hello",
		KernelVersion: "6.6.0-test",
		SHA256:        sha256Hex(oldPayload),
		ArtifactURL:   "https://example/old.raw",
		ModulesLoad:   []string{"wendyos_hello"},
	}); err != nil {
		t.Fatalf("seeding the working install: %v", err)
	}

	// Upgrade attempt whose apply fails (e.g. an unrelated add-on's modprobe
	// returns non-zero, which makes the apply script exit 1).
	newPayload := driverImage(t, "b")
	svc.httpGet = func(_ context.Context, _ string) (io.ReadCloser, error) {
		return io.NopCloser(bytes.NewReader(newPayload)), nil
	}
	svc.applyScript = testExecutable(t, "false")
	if err := svc.InstallFromURL(context.Background(), DriverInstallSpec{
		Name:          "wendyos-hello",
		KernelVersion: "6.6.0-test",
		SHA256:        sha256Hex(newPayload),
		ArtifactURL:   "https://example/new.raw",
		ModulesLoad:   []string{"wendyos_hello"},
	}); err == nil {
		t.Fatal("InstallFromURL: got nil, want apply failure")
	}

	got, err := os.ReadFile(svc.rawPath(testKernel, "wendyos-hello"))
	if err != nil {
		t.Fatalf("previous install was destroyed by the failed upgrade: %v", err)
	}
	if !bytes.Equal(got, oldPayload) {
		t.Errorf("restored .raw = %q, want the previous working payload %q", got, oldPayload)
	}
	if _, err := os.Stat(svc.confPath(testKernel, "wendyos-hello")); err != nil {
		t.Errorf("previous modules-load conf was not restored: %v", err)
	}
	// The backup must not linger where the apply script could see it.
	if entries, _ := os.ReadDir(filepath.Dir(svc.rawPath(testKernel, "x"))); len(entries) != 1 {
		t.Errorf("enabled dir = %d entries, want exactly the restored .raw", len(entries))
	}
}

// A remove whose apply fails must put the driver back: the add-on is still
// merged into /usr at that point, so dropping it from /data would strand a
// merged-but-unlisted driver that the CLI can no longer remove.
func TestRemoveDriver_ApplyFailureRestoresInstall(t *testing.T) {
	payload := driverImage(t, "a")
	svc := newTestDriverService(t, payload)
	if err := svc.InstallFromURL(context.Background(), DriverInstallSpec{
		Name:          "wendyos-hello",
		KernelVersion: "6.6.0-test",
		SHA256:        sha256Hex(payload),
		ArtifactURL:   "https://example/x.raw",
		ModulesLoad:   []string{"wendyos_hello"},
	}); err != nil {
		t.Fatalf("seeding the install: %v", err)
	}

	svc.applyScript = testExecutable(t, "false")
	stream := &fakeRemoveStream{}
	if err := svc.RemoveDriver(&agentpbv2.RemoveDriverRequest{Name: "wendyos-hello"}, stream); err != nil {
		t.Fatalf("RemoveDriver returned a transport error: %v", err)
	}
	if !stream.failed {
		t.Error("RemoveDriver: expected a Failed response when apply fails")
	}

	got, err := os.ReadFile(svc.rawPath(testKernel, "wendyos-hello"))
	if err != nil {
		t.Fatalf("driver was dropped from the store despite the failed unmerge: %v", err)
	}
	if !bytes.Equal(got, payload) {
		t.Errorf("restored .raw = %q, want %q", got, payload)
	}
	if _, err := os.Stat(svc.confPath(testKernel, "wendyos-hello")); err != nil {
		t.Errorf("modules-load conf was not restored: %v", err)
	}
}

// A successful install must not leave rollback backups behind.
func TestInstallFromURL_SuccessLeavesNoBackups(t *testing.T) {
	payload := driverImage(t, "a")
	svc := newTestDriverService(t, payload)
	for i := 0; i < 2; i++ { // install then reinstall over it
		if err := svc.InstallFromURL(context.Background(), DriverInstallSpec{
			Name:          "wendyos-hello",
			KernelVersion: "6.6.0-test",
			SHA256:        sha256Hex(payload),
			ArtifactURL:   "https://example/x.raw",
			ModulesLoad:   []string{"wendyos_hello"},
		}); err != nil {
			t.Fatalf("install %d: %v", i, err)
		}
	}
	entries, _ := os.ReadDir(filepath.Dir(svc.rawPath(testKernel, "x")))
	if len(entries) != 1 || entries[0].Name() != "wendyos-hello.raw" {
		names := make([]string, len(entries))
		for i, e := range entries {
			names[i] = e.Name()
		}
		t.Errorf("enabled dir = %v, want only wendyos-hello.raw", names)
	}
}

func TestValidateArtifactURL_Rejects(t *testing.T) {
	rejected := map[string]string{
		"cloud metadata":   "http://169.254.169.254/latest/meta-data/",
		"internal service": "http://192.168.2.10:8000/x.raw",
		"loopback":         "http://127.0.0.1:8000/x.raw",
		"arbitrary host":   "https://evil.example/x.raw",
		"embedded creds":   "https://user:pass@storage.googleapis.com/x.raw",
		"file scheme":      "file:///etc/shadow",
		"no host":          "https://",
	}
	for what, u := range rejected {
		if err := validateArtifactURL(u); err == nil {
			t.Errorf("validateArtifactURL(%s = %q) = nil, want an error", what, u)
		}
	}
	// The registry the CLI resolves add-ons from must keep working.
	if err := validateArtifactURL("https://storage.googleapis.com/wendyos-images-public/x.raw"); err != nil {
		t.Errorf("registry URL rejected: %v", err)
	}
}

// Bench and on-prem registries are reachable only when explicitly opted in.
func TestValidateArtifactURL_ExtraHostsOptIn(t *testing.T) {
	const benchURL = "http://169.254.198.132:8000/wendyos-hello.raw"
	if err := validateArtifactURL(benchURL); err == nil {
		t.Fatal("a bench host was accepted without the opt-in")
	}
	t.Setenv(driverExtraHostsEnv, "169.254.198.132, registry.internal")
	if err := validateArtifactURL(benchURL); err != nil {
		t.Errorf("opted-in bench host rejected: %v", err)
	}
	if err := validateArtifactURL("https://registry.internal/x.raw"); err != nil {
		t.Errorf("opted-in registry rejected: %v", err)
	}
	if err := validateArtifactURL("https://other.example/x.raw"); err == nil {
		t.Error("the opt-in must not allow unrelated hosts")
	}
}

// The dial guard is what stops a validated name from being rebound to an
// internal address after the fact.
func TestCheckDialAddress(t *testing.T) {
	internal := []string{"10.0.0.5:443", "192.168.1.7:80", "127.0.0.1:8080", "169.254.169.254:80"}
	for _, addr := range internal {
		if err := checkDialAddress("storage.googleapis.com", addr); err == nil {
			t.Errorf("checkDialAddress(%q) = nil, want a refusal", addr)
		}
	}
	if err := checkDialAddress("storage.googleapis.com", "142.250.180.16:443"); err != nil {
		t.Errorf("public address refused: %v", err)
	}
	// An opted-in host is exempt: reaching a LAN address is why it was named.
	t.Setenv(driverExtraHostsEnv, "registry.internal")
	if err := checkDialAddress("registry.internal", "192.168.1.7:80"); err != nil {
		t.Errorf("opted-in host refused its LAN address: %v", err)
	}
}

// fakeRemoveStream captures what RemoveDriver streams back, so a test can tell
// a Failed response apart from a Completed one.
type fakeRemoveStream struct {
	grpc.ServerStreamingServer[agentpbv2.DriverApplyResponse]
	failed         bool
	completed      bool
	rebootRequired bool
}

func (f *fakeRemoveStream) Send(resp *agentpbv2.DriverApplyResponse) error {
	switch resp.GetResponseType().(type) {
	case *agentpbv2.DriverApplyResponse_Failed_:
		f.failed = true
	case *agentpbv2.DriverApplyResponse_Completed_:
		f.completed = true
		f.rebootRequired = resp.GetCompleted().GetRebootRequired()
	}
	return nil
}

func (f *fakeRemoveStream) Context() context.Context { return context.Background() }

// The config-partition seed is unauthenticated storage, so it must refuse to
// install while no signing key is embedded, even though the mTLS RPC may not.
func TestSeedInstall_FailsClosedWithoutSigningKey(t *testing.T) {
	payload := []byte("unsigned-driver")
	svc := newTestDriverService(t, payload)
	svc.requireSignature = true // what NewSeedDriverService sets
	if svc.verifier.Enabled() {
		t.Skip("a signing key is embedded; the fail-closed path is not exercised")
	}

	err := svc.InstallFromURL(context.Background(), DriverInstallSpec{
		Name:          "wendyos-hello",
		KernelVersion: "6.6.0-test",
		SHA256:        sha256Hex(payload),
		ArtifactURL:   "https://example/x.raw",
	})
	if err == nil {
		t.Fatal("seeded install of an unverifiable driver succeeded; want it refused")
	}
	if !strings.Contains(err.Error(), "cannot be authenticated") {
		t.Errorf("error = %v, want an authenticity refusal", err)
	}
	if _, statErr := os.Stat(svc.rawPath(testKernel, "wendyos-hello")); !os.IsNotExist(statErr) {
		t.Error("the refused driver was placed on disk anyway")
	}
}

// The same unsigned add-on is accepted over the mTLS RPC pre-GA (that caller is
// already root-equivalent), so the fail-closed switch must stay opt-in.
func TestRPCInstall_AllowsUnsignedWhileNoKeyEmbedded(t *testing.T) {
	payload := driverImage(t, "a")
	svc := newTestDriverService(t, payload)
	if svc.verifier.Enabled() {
		t.Skip("a signing key is embedded; the pre-GA fail-open path is gone")
	}
	if err := svc.InstallFromURL(context.Background(), DriverInstallSpec{
		Name:          "wendyos-hello",
		KernelVersion: "6.6.0-test",
		SHA256:        sha256Hex(payload),
		ArtifactURL:   "https://example/x.raw",
	}); err != nil {
		t.Fatalf("RPC-path install: %v", err)
	}
}

// A remote install that declares no kernel version cannot be waved through:
// nothing pins the .ko to this kernel at resolve time.
func TestInstallFromURL_RequiresKernelVersion(t *testing.T) {
	payload := []byte("driver")
	svc := newTestDriverService(t, payload)
	err := svc.InstallFromURL(context.Background(), DriverInstallSpec{
		Name:        "wendyos-hello",
		SHA256:      sha256Hex(payload),
		ArtifactURL: "https://example/x.raw",
	})
	if err == nil {
		t.Fatal("remote install without a kernel version succeeded; want it refused")
	}
	if !strings.Contains(err.Error(), "kernel version") {
		t.Errorf("error = %v, want a kernel-version refusal", err)
	}
}

func TestCheckKernel(t *testing.T) {
	svc := newTestDriverService(t, nil)
	cases := []struct {
		what    string
		kver    string
		remote  bool
		wantErr bool
	}{
		{"remote must declare one", "", true, true},
		{"local file may omit it", "", false, false},
		{"matching version passes", "6.6.0-test", true, false},
		{"mismatch refused", "5.10.0-other", true, true},
		{"mismatch refused locally too", "5.10.0-other", false, true},
	}
	for _, tc := range cases {
		if err := svc.checkKernel("d", tc.kver, tc.remote, false); (err != nil) != tc.wantErr {
			t.Errorf("%s: checkKernel(%q, remote=%v) = %v, wantErr=%v", tc.what, tc.kver, tc.remote, err, tc.wantErr)
		}
	}
	// Staging targets a kernel the device is not running, so the running-kernel
	// comparison must not apply - but it still has to say which kernel it is for.
	if err := svc.checkKernel("d", "5.10.0-other", true, true); err != nil {
		t.Errorf("staging another kernel = %v, want nil", err)
	}
	if err := svc.checkKernel("d", "", true, true); err == nil {
		t.Error("staging with no kernel version was accepted")
	}
}

// stageDriverRaw writes payload where finalize expects a staged image and returns
// its digest. It mirrors newStagedFile: the temp file shares the store's
// filesystem, which is what makes place()'s rename work.
func stageDriverRaw(t *testing.T, svc *DriverService, payload []byte) ([]byte, string) {
	t.Helper()
	if err := os.MkdirAll(svc.enabledDir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(filepath.Dir(svc.enabledDir), "staged.raw")
	if err := os.WriteFile(path, payload, 0o644); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(payload)
	return sum[:], path
}

// modprobe cannot displace a module already in the kernel, so an install over a
// resident one leaves the new .ko on disk with the old code still running.
func TestFinalize_RebootRequiredWhenModuleStaysResident(t *testing.T) {
	payload := driverImage(t, "a")
	svc := newTestDriverService(t, payload)
	// /proc/modules spells it with an underscore; the add-on declares a hyphen.
	svc.loadedModules = func() []string { return []string{"wendyos_hello"} }
	digest, staged := stageDriverRaw(t, svc, payload)

	rebootRequired, err := svc.finalize(context.Background(), DriverInstallSpec{
		Name:          "wendyos-hello",
		KernelVersion: "6.6.0-test",
		SHA256:        sha256Hex(payload),
		ModulesLoad:   []string{"wendyos-hello"},
	}, staged, digest)
	if err != nil {
		t.Fatalf("finalize: %v", err)
	}
	if !rebootRequired {
		t.Error("rebootRequired = false, want true: the old module is still resident")
	}
}

// The apply script modprobes the add-on's own modules, so afterwards a freshly
// loaded module looks exactly like one that never went away. Residency has to be
// sampled before the apply or every first install demands a reboot.
func TestFinalize_NoRebootWhenTheApplyLoadsTheModule(t *testing.T) {
	payload := driverImage(t, "a")
	svc := newTestDriverService(t, payload)

	// A stand-in for modprobe: the script records that it ran, and the module
	// only counts as loaded from that point on.
	dir := t.TempDir()
	marker := filepath.Join(dir, "modprobed")
	script := filepath.Join(dir, "apply.sh")
	if err := os.WriteFile(script, []byte("#!/bin/sh\ntouch "+marker+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	svc.applyScript = script
	svc.loadedModules = func() []string {
		if _, err := os.Stat(marker); err != nil {
			return nil
		}
		return []string{"wendyos_hello"}
	}

	digest, staged := stageDriverRaw(t, svc, payload)
	rebootRequired, err := svc.finalize(context.Background(), DriverInstallSpec{
		Name:          "wendyos-hello",
		KernelVersion: "6.6.0-test",
		SHA256:        sha256Hex(payload),
		ModulesLoad:   []string{"wendyos_hello"},
	}, staged, digest)
	if err != nil {
		t.Fatalf("finalize: %v", err)
	}
	if _, statErr := os.Stat(marker); statErr != nil {
		t.Fatal("the apply script did not run; the test proves nothing")
	}
	if rebootRequired {
		t.Error("rebootRequired = true, want false: the apply loaded the module, it is not a leftover")
	}
}

func TestFinalize_NoRebootForAFreshInstall(t *testing.T) {
	payload := driverImage(t, "a")
	svc := newTestDriverService(t, payload)
	digest, staged := stageDriverRaw(t, svc, payload)

	rebootRequired, err := svc.finalize(context.Background(), DriverInstallSpec{
		Name:          "wendyos-hello",
		KernelVersion: "6.6.0-test",
		SHA256:        sha256Hex(payload),
		ModulesLoad:   []string{"wendyos_hello"},
	}, staged, digest)
	if err != nil {
		t.Fatalf("finalize: %v", err)
	}
	if rebootRequired {
		t.Error("rebootRequired = true, want false: nothing was loaded to displace")
	}
}

// Removing unmerges the .ko but never force-unloads the module.
func TestRemoveDriver_RebootRequiredWhileModuleResident(t *testing.T) {
	payload := driverImage(t, "a")
	svc := newTestDriverService(t, payload)
	if err := svc.InstallFromURL(context.Background(), DriverInstallSpec{
		Name:          "wendyos-hello",
		KernelVersion: "6.6.0-test",
		SHA256:        sha256Hex(payload),
		ArtifactURL:   "https://example/x.raw",
		ModulesLoad:   []string{"wendyos_hello"},
	}); err != nil {
		t.Fatalf("seeding the install: %v", err)
	}
	svc.loadedModules = func() []string { return []string{"wendyos_hello"} }

	stream := &fakeRemoveStream{}
	if err := svc.RemoveDriver(&agentpbv2.RemoveDriverRequest{Name: "wendyos-hello"}, stream); err != nil {
		t.Fatalf("RemoveDriver: %v", err)
	}
	if !stream.completed || stream.failed {
		t.Fatalf("remove did not complete: completed=%v failed=%v", stream.completed, stream.failed)
	}
	if !stream.rebootRequired {
		t.Error("rebootRequired = false, want true: the module is still loaded")
	}
}

func TestRemoveDriver_NoRebootWhenNothingIsLoaded(t *testing.T) {
	payload := driverImage(t, "a")
	svc := newTestDriverService(t, payload)
	if err := svc.InstallFromURL(context.Background(), DriverInstallSpec{
		Name:          "wendyos-hello",
		KernelVersion: "6.6.0-test",
		SHA256:        sha256Hex(payload),
		ArtifactURL:   "https://example/x.raw",
		ModulesLoad:   []string{"wendyos_hello"},
	}); err != nil {
		t.Fatalf("seeding the install: %v", err)
	}

	stream := &fakeRemoveStream{}
	if err := svc.RemoveDriver(&agentpbv2.RemoveDriverRequest{Name: "wendyos-hello"}, stream); err != nil {
		t.Fatalf("RemoveDriver: %v", err)
	}
	if !stream.completed {
		t.Fatal("remove did not complete")
	}
	if stream.rebootRequired {
		t.Error("rebootRequired = true, want false: the module was never loaded")
	}
}

// A URL install with no declared digest is unidentifiable: nobody inspected the
// bytes, and with signing not yet enforced nothing else would catch a swap.
func TestFinalize_RefusesARemoteFetchWithNoDigest(t *testing.T) {
	payload := driverImage(t, "a")
	svc := newTestDriverService(t, payload)
	digest, staged := stageDriverRaw(t, svc, payload)

	_, err := svc.finalize(context.Background(), DriverInstallSpec{
		Name:          "wendyos-hello",
		KernelVersion: "6.6.0-test",
		ArtifactURL:   "https://example/x.raw",
	}, staged, digest)
	if err == nil || !strings.Contains(err.Error(), "without a declared sha256") {
		t.Fatalf("finalize = %v, want a refusal for the missing digest", err)
	}
	// A local file stays installable: the operator picked those exact bytes.
	if _, err := svc.finalize(context.Background(), DriverInstallSpec{
		Name:          "wendyos-hello",
		KernelVersion: "6.6.0-test",
	}, staged, digest); err != nil {
		t.Fatalf("local install without a digest: %v", err)
	}
}

// A stat that fails for any reason other than "absent" must not be read as
// "nothing installed": the caller's rollback would then delete a working driver
// instead of restoring it.
func TestSnapshotDriver_UnreadableStoreIsNotMistakenForEmpty(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root bypasses directory permissions")
	}
	svc := newTestDriverService(t, nil)
	if err := os.MkdirAll(svc.enabledDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(svc.enabledDir, "acme.raw"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(svc.enabledDir, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(svc.enabledDir, 0o755) }) //nolint:errcheck // best effort

	if _, err := svc.snapshotDriver(testKernel, "acme"); err == nil {
		t.Error("snapshotDriver = nil, want an error rather than an empty snapshot")
	}
}

// ListDrivers gates an OTA, so an unreadable store must be an RPC failure rather
// than a successful empty inventory. A regular file is a portable way to make
// ReadDir fail on both Linux and macOS without relying on permission semantics.
func TestListDrivers_UnreadableStoreIsNotMistakenForEmpty(t *testing.T) {
	svc := newTestDriverService(t, nil)
	if err := os.WriteFile(svc.enabledDir, []byte("not a directory"), 0o644); err != nil {
		t.Fatal(err)
	}

	resp, err := svc.ListDrivers(context.Background(), &agentpbv2.ListDriversRequest{})
	if err == nil {
		t.Fatalf("ListDrivers = %+v, nil; want a store read error", resp)
	}
	if !strings.Contains(err.Error(), "reading driver store") {
		t.Errorf("ListDrivers error = %v, want driver-store context", err)
	}
}

func TestKernelKeyedPaths(t *testing.T) {
	svc := newTestDriverService(t, nil)
	if got, want := svc.rawPath("6.1-x", "acme"), filepath.Join(svc.enabledDir, "6.1-x", "acme.raw"); got != want {
		t.Errorf("rawPath = %q, want %q", got, want)
	}
	// An add-on pinning no kernel applies to every kernel, so it must not be
	// trapped in one kernel's bucket.
	if got, want := svc.rawPath("", "acme"), filepath.Join(svc.enabledDir, unpinnedKernelDir, "acme.raw"); got != want {
		t.Errorf("unpinned rawPath = %q, want %q", got, want)
	}
	if got, want := svc.confPath("6.1-x", "acme"), filepath.Join(svc.modulesDir, "6.1-x", "acme.conf"); got != want {
		t.Errorf("confPath = %q, want %q", got, want)
	}
}

// The kernel comes out of a stored image for migration, so a crafted add-on must
// not be able to steer a path out of the store.
func TestValidateKernelDir(t *testing.T) {
	for _, ok := range []string{"", "6.18.33-v8-16k", "6.1.0+", "5.10_rt"} {
		if err := validateKernelDir(ok); err != nil {
			t.Errorf("validateKernelDir(%q) = %v, want nil", ok, err)
		}
	}
	for _, bad := range []string{"..", ".", "../../etc", "a/b", "a\\b", "has space", "a\x00b"} {
		if err := validateKernelDir(bad); err == nil {
			t.Errorf("validateKernelDir(%q) = nil, want an error", bad)
		}
	}
}

// Devices updated from an older agent still have a flat store.
func TestMigrateStore(t *testing.T) {
	svc := newTestDriverService(t, nil)
	if err := os.MkdirAll(svc.enabledDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(svc.modulesDir, 0o755); err != nil {
		t.Fatal(err)
	}
	hello, err := os.ReadFile(filepath.Join("testdata", fixtureName+".raw"))
	if err != nil {
		t.Fatal(err)
	}
	nokernel, err := os.ReadFile(filepath.Join("testdata", "nokernel.raw"))
	if err != nil {
		t.Fatal(err)
	}
	write := func(name string, b []byte) {
		if err := os.WriteFile(filepath.Join(svc.enabledDir, name+".raw"), b, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write(fixtureName, hello)
	write("nokernel", nokernel)
	write("broken", []byte("not a squashfs"))
	if err := os.WriteFile(filepath.Join(svc.modulesDir, fixtureName+".conf"), []byte("wendyos_hello\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	svc.MigrateStore()

	if _, err := os.Stat(svc.rawPath(fixtureKernel, fixtureName)); err != nil {
		t.Errorf("%s did not move into its kernel bucket: %v", fixtureName, err)
	}
	if _, err := os.Stat(svc.confPath(fixtureKernel, fixtureName)); err != nil {
		t.Errorf("the /data override did not follow its image: %v", err)
	}
	if _, err := os.Stat(svc.rawPath("", "nokernel")); err != nil {
		t.Errorf("an add-on pinning no kernel did not move to the unpinned bucket: %v", err)
	}
	// Nothing says which bucket it belongs in, and guessing would hide it. Left
	// flat, it stays listed and reported unreadable.
	if _, err := os.Stat(filepath.Join(svc.enabledDir, "broken.raw")); err != nil {
		t.Errorf("an unreadable image should stay put, got %v", err)
	}
	svc.MigrateStore() // idempotent
}

// An add-on left behind by an OTA must still be listed, and a name under several
// kernels must resolve to the copy that can load.
func TestListDrivers_UnionAcrossKernels(t *testing.T) {
	svc := newTestDriverService(t, nil)
	put := func(kernel, name, fixture string) {
		body, err := os.ReadFile(filepath.Join("testdata", fixture))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(filepath.Dir(svc.rawPath(kernel, name)), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(svc.rawPath(kernel, name), body, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	// Same add-on in two buckets: the running kernel's build and an older one.
	put(testKernel, "wendyos-hello", "install-a.raw")
	put(fixtureKernel, "wendyos-hello", "wendyos-hello.raw")
	// And one that exists only under a kernel this device does not run.
	put(fixtureKernel, "nokernel", "nokernel.raw")

	resp, err := svc.ListDrivers(context.Background(), &agentpbv2.ListDriversRequest{})
	if err != nil {
		t.Fatalf("ListDrivers: %v", err)
	}
	got := map[string]string{}
	for _, d := range resp.GetInstalled() {
		if _, dup := got[d.GetName()]; dup {
			t.Errorf("%s listed twice; the buckets should dedupe by name", d.GetName())
		}
		got[d.GetName()] = d.GetKernelVersion()
	}
	if len(got) != 2 {
		t.Fatalf("listed %v, want wendyos-hello and nokernel", got)
	}
	if got["wendyos-hello"] != testKernel {
		t.Errorf("kernel = %q, want the running kernel %q to win the name", got["wendyos-hello"], testKernel)
	}
	if _, ok := got["nokernel"]; !ok {
		t.Error("an add-on under another kernel vanished from the listing")
	}
}

// Staging targets a kernel the device is not running: it must land in that
// kernel's bucket and leave the running system alone.
func TestFinalize_StageOnlyPlacesWithoutApplying(t *testing.T) {
	payload := driverImage(t, "a") // declares 6.6.0-test
	svc := newTestDriverService(t, payload)
	svc.unameR = func() string { return "9.9.9-running" } // a different kernel is live

	dir := t.TempDir()
	marker := filepath.Join(dir, "applied")
	script := filepath.Join(dir, "apply.sh")
	if err := os.WriteFile(script, []byte("#!/bin/sh\ntouch "+marker+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	svc.applyScript = script

	digest, staged := stageDriverRaw(t, svc, payload)
	rebootRequired, err := svc.finalize(context.Background(), DriverInstallSpec{
		Name:          "wendyos-hello",
		KernelVersion: testKernel,
		SHA256:        sha256Hex(payload),
		StageOnly:     true,
	}, staged, digest)
	if err != nil {
		t.Fatalf("finalize: %v", err)
	}
	if rebootRequired {
		t.Error("rebootRequired = true, want false: nothing was applied")
	}
	if _, err := os.Stat(svc.rawPath(testKernel, "wendyos-hello")); err != nil {
		t.Errorf("staged image is not in the target bucket: %v", err)
	}
	if _, err := os.Stat(marker); err == nil {
		t.Error("the apply script ran; staging must not touch the running system")
	}
}

// The manifest and the image must agree, or a rebuild is filed under the wrong
// kernel.
func TestFinalize_StageOnlyRejectsAKernelMismatch(t *testing.T) {
	payload := driverImage(t, "a") // declares 6.6.0-test
	svc := newTestDriverService(t, payload)
	digest, staged := stageDriverRaw(t, svc, payload)

	_, err := svc.finalize(context.Background(), DriverInstallSpec{
		Name:          "wendyos-hello",
		KernelVersion: "7.7.7-elsewhere",
		SHA256:        sha256Hex(payload),
		StageOnly:     true,
	}, staged, digest)
	if err == nil || !strings.Contains(err.Error(), "built for kernel") {
		t.Fatalf("finalize = %v, want a refusal for the kernel mismatch", err)
	}
}

// A rollback returns to the previous kernel, so its bucket must survive a prune.
func TestPruneStore_KeepsThePreviousKernel(t *testing.T) {
	svc := newTestDriverService(t, nil)
	for _, k := range []string{testKernel, unpinnedKernelDir, "1.0-oldest", "2.0-previous", "3.0-target"} {
		if err := os.MkdirAll(filepath.Join(svc.enabledDir, k), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	// Make "2.0-previous" the most recently touched of the non-kept buckets.
	now := time.Now()
	os.Chtimes(filepath.Join(svc.enabledDir, "1.0-oldest"), now.Add(-2*time.Hour), now.Add(-2*time.Hour))     //nolint:errcheck
	os.Chtimes(filepath.Join(svc.enabledDir, "2.0-previous"), now.Add(-1*time.Minute), now.Add(-time.Minute)) //nolint:errcheck

	svc.pruneStore("3.0-target")

	for _, keep := range []string{testKernel, unpinnedKernelDir, "3.0-target", "2.0-previous"} {
		if _, err := os.Stat(filepath.Join(svc.enabledDir, keep)); err != nil {
			t.Errorf("%s was pruned but must be kept: %v", keep, err)
		}
	}
	if _, err := os.Stat(filepath.Join(svc.enabledDir, "1.0-oldest")); !os.IsNotExist(err) {
		t.Error("1.0-oldest should have been pruned")
	}
}

// Through the RPC, so the proto field is proven to reach finalize.
func TestInstallDriver_StageOnlyOverTheRPC(t *testing.T) {
	payload := driverImage(t, "a") // declares 6.6.0-test
	svc := newTestDriverService(t, payload)
	svc.unameR = func() string { return "9.9.9-running" }

	dir := t.TempDir()
	marker := filepath.Join(dir, "applied")
	script := filepath.Join(dir, "apply.sh")
	if err := os.WriteFile(script, []byte("#!/bin/sh\ntouch "+marker+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	svc.applyScript = script

	recv := make(chan *agentpbv2.InstallDriverRequest, 1)
	recv <- &agentpbv2.InstallDriverRequest{
		RequestType: &agentpbv2.InstallDriverRequest_Spec{Spec: &agentpbv2.DriverSpec{
			Name:          "wendyos-hello",
			KernelVersion: testKernel,
			Sha256:        sha256Hex(payload),
			ArtifactUrl:   "https://storage.googleapis.com/wendyos-images-public/x.raw",
			StageOnly:     true,
		}},
	}
	close(recv)
	stream := &fakeBidiStream[agentpbv2.InstallDriverRequest, agentpbv2.DriverApplyResponse]{
		ctx: context.Background(), recv: recv,
	}

	if err := svc.InstallDriver(stream); err != nil {
		t.Fatalf("InstallDriver: %v", err)
	}
	var completed, failed bool
	for _, r := range stream.sent {
		if c := r.GetCompleted(); c != nil {
			completed = true
			if c.GetRebootRequired() {
				t.Error("rebootRequired = true, want false: staging applies nothing")
			}
		}
		if f := r.GetFailed(); f != nil {
			failed = true
			t.Errorf("stage failed: %s", f.GetErrorMessage())
		}
	}
	if !completed || failed {
		t.Fatalf("stream did not complete cleanly (completed=%v failed=%v)", completed, failed)
	}
	if _, err := os.Stat(svc.rawPath(testKernel, "wendyos-hello")); err != nil {
		t.Errorf("staged image is not in the target bucket: %v", err)
	}
	if _, err := os.Stat(marker); err == nil {
		t.Error("the apply script ran; staging must not touch the running system")
	}
}

// Offline pre-load: the operator hands over the bytes, so the image names the
// bucket and no declared kernel is needed.
func TestFinalize_StageOnlyFromStreamedBytesUsesTheImageKernel(t *testing.T) {
	payload := driverImage(t, "a") // declares 6.6.0-test
	svc := newTestDriverService(t, payload)
	svc.unameR = func() string { return "9.9.9-running" }

	dir := t.TempDir()
	marker := filepath.Join(dir, "applied")
	script := filepath.Join(dir, "apply.sh")
	if err := os.WriteFile(script, []byte("#!/bin/sh\ntouch "+marker+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	svc.applyScript = script

	digest, staged := stageDriverRaw(t, svc, payload)
	rebootRequired, err := svc.finalize(context.Background(), DriverInstallSpec{
		Name:      "wendyos-hello",
		SHA256:    sha256Hex(payload),
		StageOnly: true, // no KernelVersion, no ArtifactURL: streamed bytes
	}, staged, digest)
	if err != nil {
		t.Fatalf("finalize: %v", err)
	}
	if rebootRequired {
		t.Error("rebootRequired = true, want false: staging applies nothing")
	}
	if _, err := os.Stat(svc.rawPath(testKernel, "wendyos-hello")); err != nil {
		t.Errorf("staged image is not in the bucket the image names: %v", err)
	}
	if _, err := os.Stat(marker); err == nil {
		t.Error("the apply script ran; staging must not touch the running system")
	}
}

// A URL fetch still has to declare its kernel: the manifest is the only witness
// to what was downloaded.
func TestFinalize_StageOnlyFromURLStillNeedsAKernel(t *testing.T) {
	payload := driverImage(t, "a")
	svc := newTestDriverService(t, payload)
	digest, staged := stageDriverRaw(t, svc, payload)

	_, err := svc.finalize(context.Background(), DriverInstallSpec{
		Name:        "wendyos-hello",
		SHA256:      sha256Hex(payload),
		ArtifactURL: "https://example/x.raw",
		StageOnly:   true,
	}, staged, digest)
	if err == nil || !strings.Contains(err.Error(), "without a kernel version") {
		t.Fatalf("finalize = %v, want a refusal for the missing kernel", err)
	}
}

func TestPrivatePayloadStatusAndOverrides(t *testing.T) {
	svc := newTestDriverService(t, nil)
	svc.loadedModules = func() []string { return []string{"wendyos_hello", "override_mod"} }
	name := "wendyos-hello"
	write := func(path, content string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0644); err != nil {
			t.Fatal(err)
		}
	}
	raw, err := os.ReadFile("testdata/install-private.raw")
	if err != nil {
		t.Fatal(err)
	}
	write(svc.rawPath(testKernel, name), string(raw))
	write(filepath.Join(svc.privatePayloadDir, name, "modules-load.conf"), "wendyos_hello\n")
	check := func(wantLoaded bool, wantModule string) {
		t.Helper()
		resp, err := svc.ListDrivers(context.Background(), &agentpbv2.ListDriversRequest{})
		if err != nil {
			t.Fatal(err)
		}
		if len(resp.Installed) != 1 {
			t.Fatalf("installed = %v", resp.Installed)
		}
		d := resp.Installed[0]
		if d.Loaded != wantLoaded || strings.Join(d.ModulesLoad, ",") != wantModule {
			t.Fatalf("driver = %+v", d)
		}
	}
	check(false, "wendyos_hello") // An inert/colliding package cannot borrow another driver's modules.
	write(filepath.Join(svc.activeStateDir, name, "files"), "/usr/lib/modules/test/updates/wendyos/wendyos-hello/wendyos_hello.ko\n")
	check(true, "wendyos_hello")
	write(svc.confPath(testKernel, name), "override_mod\n")
	check(true, "override_mod")
	write(svc.confPath(testKernel, name), "")
	check(false, "") // An explicit empty override disables autoload, as in the runtime.
}

func TestFinalizeReplacementReceipt(t *testing.T) {
	for _, fresh := range []bool{false, true} {
		t.Run(fmt.Sprint("fresh=", fresh), func(t *testing.T) {
			payload, err := os.ReadFile("testdata/install-private.raw")
			if err != nil {
				t.Fatal(err)
			}
			svc := newTestDriverService(t, payload)
			svc.loadedModules = func() []string { return []string{"wendyos_hello"} }
			state := filepath.Join(svc.activeStateDir, "wendyos-hello")
			if err := os.MkdirAll(state, 0755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(state, "generation"), []byte("old\n"), 0644); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(state, "replaced-modules"), []byte("wendyos-hello\n"), 0644); err != nil {
				t.Fatal(err)
			}
			if fresh {
				script := filepath.Join(t.TempDir(), "apply.sh")
				if err := os.WriteFile(script, []byte("#!/bin/sh\nprintf 'new\\n' > '"+filepath.Join(state, "generation")+"'\n"), 0755); err != nil {
					t.Fatal(err)
				}
				svc.applyScript = script
			}
			digest, staged := stageDriverRaw(t, svc, payload)
			reboot, err := svc.finalize(context.Background(), DriverInstallSpec{Name: "wendyos-hello"}, staged, digest)
			if err != nil {
				t.Fatal(err)
			}
			if reboot == fresh {
				t.Fatalf("reboot=%v with fresh receipt=%v", reboot, fresh)
			}
		})
	}
}

func TestParseMergedSysextNames(t *testing.T) {
	for _, tc := range []struct{ input, want string }{
		{`[{"hierarchy":"/opt","extensions":"none"},{"hierarchy":"/usr","extensions":["intel-be202","another"]}]`, "intel-be202,another"},
		{`[{"hierarchy":"/usr","extensions":"none"}]`, ""},
		{`[{"hierarchy":"/usr","extensions":null}]`, ""},
		{`not JSON`, ""},
	} {
		if got := strings.Join(parseMergedSysextNames([]byte(tc.input)), ","); got != tc.want {
			t.Errorf("parse %q = %q, want %q", tc.input, got, tc.want)
		}
	}
}
