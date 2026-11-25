package commands

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/wendylabsinc/wendy/go/internal/cli/grpcclient"
	agentpbv2 "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
)

func TestCountAddons(t *testing.T) {
	for n, want := range map[int]string{1: "1 driver add-on", 2: "2 driver add-ons"} {
		if got := countAddons(n); got != want {
			t.Errorf("countAddons(%d) = %q, want %q", n, got, want)
		}
	}
}

// The verdict decides whether an OS update may proceed, so each way of failing to
// stage has to reach it.
func TestDriverPreflightBlocking(t *testing.T) {
	tests := []struct {
		desc string
		pf   driverPreflight
		want bool
	}{
		{"nothing to report", driverPreflight{}, false},
		{"one add-on could not be staged", driverPreflight{
			unstaged: []unstagedAddon{{"acme-nic", "no rebuild published for 0.20.0"}},
		}, true},
		// The worst case: there may be no add-ons, or a critical one. Both look the
		// same from here, so it has to block.
		{"installed set unreadable", driverPreflight{unreadable: "context deadline exceeded"}, true},
	}
	for _, tt := range tests {
		if got := tt.pf.blocking(); got != tt.want {
			t.Errorf("%s: blocking() = %v, want %v", tt.desc, got, tt.want)
		}
	}
}

// The abort message names what the device would lose, so an operator reading a CI
// log knows which driver to go and rebuild.
func TestDriverPreflightSubject(t *testing.T) {
	got := driverPreflightSubject(driverPreflight{
		unstaged: []unstagedAddon{{"acme-nic", "x"}, {"acme-npu", "y"}},
	})
	if got != "acme-nic, acme-npu" {
		t.Errorf("subject = %q, want both names", got)
	}
	if got := driverPreflightSubject(driverPreflight{unreadable: "timeout"}); !strings.Contains(got, "could not be read") {
		t.Errorf("unreadable subject = %q, want it to say the set could not be read", got)
	}
}

// fakeDriverClient lets the pre-flight run against a driver service that fails
// the way a real one does when a device is slow or half-reachable.
type fakeDriverClient struct {
	agentpbv2.WendyDriverServiceClient
	listErr     error
	list        *agentpbv2.ListDriversResponse
	installFail string
	installs    *[]*fakeInstallStream
}

func (f fakeDriverClient) ListDrivers(context.Context, *agentpbv2.ListDriversRequest, ...grpc.CallOption) (*agentpbv2.ListDriversResponse, error) {
	return f.list, f.listErr
}

// The case that used to be silent: a ListDrivers timeout looked exactly like a
// device with no add-ons, so the update proceeded without a word. It must now
// reach the caller as a blocking verdict.
func TestWarnDriverAddons_ListDriversFailureBlocks(t *testing.T) {
	conn := &grpcclient.AgentConnection{
		DriverService: fakeDriverClient{listErr: status.Error(codes.DeadlineExceeded, "context deadline exceeded")},
	}
	pf := warnDriverAddonsBeforeUpdate(context.Background(), conn, "raspberry-pi-5", "0.20.0", "", 0, true)
	if !pf.blocking() {
		t.Fatal("a ListDrivers failure did not block the update")
	}
	if pf.unreadable == "" {
		t.Error("the failure was not recorded as unreadable")
	}
}

// Unimplemented does not prove the store is empty: the service is deliberately
// absent on an unenrolled connection, while unprovisioning leaves /data add-ons
// in place. An OTA must therefore require an explicit override.
func TestWarnDriverAddons_UnimplementedBlocks(t *testing.T) {
	conn := &grpcclient.AgentConnection{
		DriverService: fakeDriverClient{listErr: status.Error(codes.Unimplemented, "unknown service")},
	}
	pf := warnDriverAddonsBeforeUpdate(context.Background(), conn, "raspberry-pi-5", "0.20.0", "", 0, true)
	if !pf.blocking() || pf.unreadable == "" {
		t.Error("an unavailable driver service did not block the update")
	}
}

func TestWarnDriverAddons_MissingClientBlocks(t *testing.T) {
	pf := warnDriverAddonsBeforeUpdate(context.Background(), &grpcclient.AgentConnection{}, "raspberry-pi-5", "0.20.0", "", 0, true)
	if !pf.blocking() || pf.unreadable == "" {
		t.Error("a connection with no driver client did not block the update")
	}
}

// A device with add-ons and no way to stage them (no manifest resolved, as with a
// local artifact) must block, naming each one.
func TestWarnDriverAddons_NoManifestBlocksPerAddon(t *testing.T) {
	conn := &grpcclient.AgentConnection{DriverService: fakeDriverClient{
		list: &agentpbv2.ListDriversResponse{
			KernelVersion: "6.18.33-v8-16k",
			Installed: []*agentpbv2.InstalledDriver{
				{Name: "acme-nic", KernelVersion: "6.18.33-v8-16k"},
				{Name: "acme-npu", KernelVersion: "6.18.33-v8-16k"},
			},
		},
	}}
	pf := warnDriverAddonsBeforeUpdate(context.Background(), conn, "", "", "", 0, true)
	if len(pf.unstaged) != 2 || !pf.blocking() {
		t.Fatalf("unstaged = %+v, want both add-ons blocking", pf.unstaged)
	}
	// --no-drivers is the explicit opt-out and must never block.
	if pf := warnDriverAddonsBeforeUpdate(context.Background(), conn, "", "", "", 0, false); pf.blocking() {
		t.Error("--no-drivers blocked the update")
	}
}

func stubDriverExtensionsFor(t *testing.T, exts []extensionEntry, err error) {
	t.Helper()
	orig := driverExtensionsForFn
	driverExtensionsForFn = func(string, string, int) ([]extensionEntry, error) { return exts, err }
	t.Cleanup(func() { driverExtensionsForFn = orig })
}

func connWithAddons(names ...string) *grpcclient.AgentConnection {
	return connWith(fakeDriverClient{}, names...)
}

func connWith(c fakeDriverClient, names ...string) *grpcclient.AgentConnection {
	installed := make([]*agentpbv2.InstalledDriver, 0, len(names))
	for _, n := range names {
		installed = append(installed, &agentpbv2.InstalledDriver{Name: n, KernelVersion: "6.18.33-v8-16k"})
	}
	c.list = &agentpbv2.ListDriversResponse{KernelVersion: "6.18.33-v8-16k", Installed: installed}
	return &grpcclient.AgentConnection{DriverService: c}
}

// If the published add-ons cannot be read, nothing can be staged — and the device
// is one reboot away from losing a driver it has. That has to block, not warn.
func TestWarnDriverAddons_ManifestFailureBlocks(t *testing.T) {
	stubDriverExtensionsFor(t, nil, errors.New("fetching manifest: 503 Service Unavailable"))

	pf := warnDriverAddonsBeforeUpdate(context.Background(), connWithAddons("acme-nic"), "raspberry-pi-5", "0.20.0", "", 0, true)
	if !pf.blocking() {
		t.Fatal("a manifest failure did not block the update")
	}
	if len(pf.unstaged) != 1 || !strings.Contains(pf.unstaged[0].reason, "503") {
		t.Errorf("unstaged = %+v, want the add-on named with the underlying cause", pf.unstaged)
	}
	// --no-drivers stays the explicit opt-out.
	if pf := warnDriverAddonsBeforeUpdate(context.Background(), connWithAddons("acme-nic"), "raspberry-pi-5", "0.20.0", "", 0, false); pf.blocking() {
		t.Error("--no-drivers blocked on a manifest failure")
	}
}

// A manifest that resolves but publishes no rebuild for this add-on is the CI
// race, and looks the same to the device: it must block too.
func TestWarnDriverAddons_NoRebuildPublishedBlocks(t *testing.T) {
	stubDriverExtensionsFor(t, []extensionEntry{
		{Name: "someone-else", KernelVersion: "6.19.0", Path: "p"},
	}, nil)

	pf := warnDriverAddonsBeforeUpdate(context.Background(), connWithAddons("acme-nic"), "raspberry-pi-5", "0.20.0", "", 0, true)
	if !pf.blocking() || len(pf.unstaged) != 1 {
		t.Fatalf("unstaged = %+v, want acme-nic blocking", pf.unstaged)
	}
	if !strings.Contains(pf.unstaged[0].reason, "no rebuild published") {
		t.Errorf("reason = %q, want it to say no rebuild was published", pf.unstaged[0].reason)
	}
}

// An add-on the target still publishes for the running kernel is unaffected, so
// the update must not be held up.
func TestWarnDriverAddons_UnaffectedAddonDoesNotBlock(t *testing.T) {
	stubDriverExtensionsFor(t, []extensionEntry{
		{Name: "acme-nic", KernelVersion: "6.18.33-v8-16k", Path: "p"},
	}, nil)

	pf := warnDriverAddonsBeforeUpdate(context.Background(), connWithAddons("acme-nic"), "raspberry-pi-5", "0.20.0", "", 0, true)
	if pf.blocking() {
		t.Errorf("an unaffected add-on blocked the update: %+v", pf.unstaged)
	}
}

// A release that moves kernels must stage the exact matching rebuild and carry
// that kernel through to the agent, where the image itself is cross-checked.
func TestWarnDriverAddons_StagesRebuildForReleaseKernel(t *testing.T) {
	stubDriverExtensionsFor(t, []extensionEntry{
		{Name: "acme-nic", KernelVersion: "6.19.0", Path: "acme-nic.raw"},
	}, nil)
	var installs []*fakeInstallStream
	conn := connWith(fakeDriverClient{installs: &installs}, "acme-nic")

	pf := warnDriverAddonsBeforeUpdate(context.Background(), conn, "raspberry-pi-5", "0.20.0", "", 0, true)
	if pf.blocking() {
		t.Fatalf("verified rebuild blocked: %+v", pf.unstaged)
	}
	if len(installs) != 1 || len(installs[0].specs) != 1 {
		t.Fatalf("installs = %+v, want one rebuild", installs)
	}
	if got := installs[0].specs[0].GetKernelVersion(); got != "6.19.0" {
		t.Errorf("staged kernel = %q, want OTA target 6.19.0", got)
	}
}

func TestWarnDriverAddons_AmbiguousReleaseKernelBlocks(t *testing.T) {
	stubDriverExtensionsFor(t, []extensionEntry{
		{Name: "acme-nic", KernelVersion: "6.19.0", Path: "a.raw"},
		{Name: "acme-nic", KernelVersion: "6.20.0", Path: "b.raw"},
	}, nil)

	pf := warnDriverAddonsBeforeUpdate(context.Background(), connWithAddons("acme-nic"), "raspberry-pi-5", "0.20.0", "", 0, true)
	if !pf.blocking() || len(pf.unstaged) != 1 || !strings.Contains(pf.unstaged[0].reason, "several kernels") {
		t.Fatalf("unstaged = %+v, want ambiguous release to block", pf.unstaged)
	}
}

func TestWarnDriverAddons_UnpinnedDriverNeedsNoKernelProof(t *testing.T) {
	conn := &grpcclient.AgentConnection{DriverService: fakeDriverClient{list: &agentpbv2.ListDriversResponse{
		KernelVersion: "6.18.33-v8-16k",
		Installed:     []*agentpbv2.InstalledDriver{{Name: "acme-firmware", KernelVersion: ""}},
	}}}
	pf := warnDriverAddonsBeforeUpdate(context.Background(), conn, "", "", "", 0, true)
	if pf.blocking() {
		t.Fatalf("unpinned add-on blocked a kernel update: %+v", pf.unstaged)
	}
}

// fakeInstallStream is one InstallDriver call: it records the spec the CLI sent
// and answers with the verdict the test asked for.
type fakeInstallStream struct {
	grpc.ClientStream
	specs []*agentpbv2.DriverSpec
	data  []byte
	fail  string
	done  bool
}

func (f *fakeInstallStream) Send(req *agentpbv2.InstallDriverRequest) error {
	if spec := req.GetSpec(); spec != nil {
		f.specs = append(f.specs, spec)
	}
	if chunk := req.GetChunk(); chunk != nil {
		f.data = append(f.data, chunk.GetData()...)
	}
	return nil
}

func (f *fakeInstallStream) CloseSend() error { return nil }

func (f *fakeInstallStream) Recv() (*agentpbv2.DriverApplyResponse, error) {
	if f.done {
		return nil, io.EOF
	}
	f.done = true
	if f.fail != "" {
		return &agentpbv2.DriverApplyResponse{ResponseType: &agentpbv2.DriverApplyResponse_Failed_{
			Failed: &agentpbv2.DriverApplyResponse_Failed{ErrorMessage: f.fail},
		}}, nil
	}
	return &agentpbv2.DriverApplyResponse{ResponseType: &agentpbv2.DriverApplyResponse_Completed_{
		Completed: &agentpbv2.DriverApplyResponse_Completed{},
	}}, nil
}

func (f fakeDriverClient) InstallDriver(context.Context, ...grpc.CallOption) (agentpbv2.WendyDriverService_InstallDriverClient, error) {
	st := &fakeInstallStream{fail: f.installFail}
	if f.installs != nil {
		*f.installs = append(*f.installs, st)
	}
	return st, nil
}

// driversDirWith writes placeholder images: the agent is faked here, so only the
// filenames matter.
func driversDirWith(t *testing.T, names ...string) string {
	t.Helper()
	dir := t.TempDir()
	for _, n := range names {
		if err := os.WriteFile(filepath.Join(dir, n+".raw"), []byte("image-"+n), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// --drivers-dir stages the whole directory, including an add-on the device has
// not got: that is how a fresh air-gapped device is pre-loaded.
func TestWarnDriverAddons_DriversDirStagesEveryImage(t *testing.T) {
	stubDriverExtensionsFor(t, []extensionEntry{
		{Name: "acme-nic", KernelVersion: "6.19.0", Path: "nic.raw"},
		{Name: "acme-npu", KernelVersion: "6.19.0", Path: "npu.raw"},
	}, nil)
	var installs []*fakeInstallStream
	conn := connWith(fakeDriverClient{installs: &installs}, "acme-nic")
	dir := driversDirWith(t, "acme-nic", "acme-npu")

	pf := warnDriverAddonsBeforeUpdate(context.Background(), conn, "raspberry-pi-5", "0.20.0", dir, 0, true)
	if pf.blocking() {
		t.Fatalf("staging every image still blocked: %+v", pf)
	}
	if len(installs) != 2 {
		t.Fatalf("staged %d images, want both", len(installs))
	}
	for _, st := range installs {
		if len(st.specs) != 1 {
			t.Fatalf("stream sent %d specs, want 1", len(st.specs))
		}
		spec := st.specs[0]
		// Binding the declared version makes the agent reject an image for any
		// kernel other than the one represented by the OTA release.
		if !spec.GetStageOnly() || spec.GetKernelVersion() != "6.19.0" {
			t.Errorf("spec = %+v, want StageOnly for target kernel 6.19.0", spec)
		}
	}
}

// A device with nothing installed is the pre-load case: the directory must still
// be staged rather than the flag silently doing nothing.
func TestWarnDriverAddons_DriversDirPreloadsAnEmptyDevice(t *testing.T) {
	var installs []*fakeInstallStream
	conn := connWith(fakeDriverClient{installs: &installs})
	dir := driversDirWith(t, "acme-nic")

	pf := warnDriverAddonsBeforeUpdate(context.Background(), conn, "raspberry-pi-5", "0.20.0", dir, 0, true)
	if pf.blocking() {
		t.Errorf("pre-loading blocked the update: %+v", pf)
	}
	if len(installs) != 1 {
		t.Fatalf("staged %d images, want 1", len(installs))
	}
}

// An installed add-on the directory has no image for is one the device loses on
// reboot, so it must block and say which.
func TestWarnDriverAddons_DriversDirMissingAnInstalledAddonBlocks(t *testing.T) {
	stubDriverExtensionsFor(t, []extensionEntry{
		{Name: "acme-nic", KernelVersion: "6.19.0", Path: "nic.raw"},
	}, nil)
	conn := connWith(fakeDriverClient{}, "acme-nic", "acme-npu")
	dir := driversDirWith(t, "acme-nic")

	pf := warnDriverAddonsBeforeUpdate(context.Background(), conn, "raspberry-pi-5", "0.20.0", dir, 0, true)
	if !pf.blocking() || len(pf.unstaged) != 1 {
		t.Fatalf("unstaged = %+v, want only acme-npu blocking", pf.unstaged)
	}
	if pf.unstaged[0].name != "acme-npu" || !strings.Contains(pf.unstaged[0].reason, "no acme-npu.raw") {
		t.Errorf("unstaged = %+v, want acme-npu named as missing", pf.unstaged[0])
	}
}

// A staging failure must be reported once, against the add-on whose image was
// there — never a second time as a missing file.
func TestWarnDriverAddons_DriversDirStagingFailureBlocksOnce(t *testing.T) {
	stubDriverExtensionsFor(t, []extensionEntry{
		{Name: "acme-nic", KernelVersion: "6.19.0", Path: "nic.raw"},
	}, nil)
	conn := connWith(fakeDriverClient{installFail: "signature verification failed"}, "acme-nic")
	dir := driversDirWith(t, "acme-nic")

	pf := warnDriverAddonsBeforeUpdate(context.Background(), conn, "raspberry-pi-5", "0.20.0", dir, 0, true)
	if !pf.blocking() || len(pf.unstaged) != 1 {
		t.Fatalf("unstaged = %+v, want acme-nic blocking exactly once", pf.unstaged)
	}
	if !strings.Contains(pf.unstaged[0].reason, "signature verification failed") {
		t.Errorf("reason = %q, want the agent's cause", pf.unstaged[0].reason)
	}
}

// A local or opaque artifact exposes no trustworthy target kernel. The images
// are still staged for an offline workflow, but filename coverage alone must not
// waive the OTA guard: the user has to explicitly accept the unverified match.
func TestWarnDriverAddons_DriversDirUnknownTargetStillBlocks(t *testing.T) {
	var installs []*fakeInstallStream
	conn := connWith(fakeDriverClient{installs: &installs}, "acme-nic")
	dir := driversDirWith(t, "acme-nic")

	pf := warnDriverAddonsBeforeUpdate(context.Background(), conn, "raspberry-pi-5", "", dir, 0, true)
	if !pf.blocking() || len(pf.unstaged) != 1 {
		t.Fatalf("unstaged = %+v, want the unverified acme-nic to block", pf.unstaged)
	}
	if !strings.Contains(pf.unstaged[0].reason, "target kernel") {
		t.Errorf("reason = %q, want the unknown target kernel named", pf.unstaged[0].reason)
	}
	if len(installs) != 1 || len(installs[0].specs) != 1 || installs[0].specs[0].GetKernelVersion() != "" {
		t.Fatalf("offline staging specs = %+v, want one self-declared-kernel install", installs)
	}
}

// Several kernels in the release metadata cannot identify which one this
// device's OTA artifact boots. Staging remains possible, but it is not proof.
func TestWarnDriverAddons_DriversDirAmbiguousTargetStillBlocks(t *testing.T) {
	stubDriverExtensionsFor(t, []extensionEntry{
		{Name: "acme-nic", KernelVersion: "6.19.0", Path: "a.raw"},
		{Name: "acme-nic", KernelVersion: "6.20.0", Path: "b.raw"},
	}, nil)
	conn := connWith(fakeDriverClient{}, "acme-nic")
	dir := driversDirWith(t, "acme-nic")

	pf := warnDriverAddonsBeforeUpdate(context.Background(), conn, "raspberry-pi-5", "0.20.0", dir, 0, true)
	if !pf.blocking() || len(pf.unstaged) != 1 || !strings.Contains(pf.unstaged[0].reason, "several kernels") {
		t.Fatalf("unstaged = %+v, want an ambiguous-target blocker", pf.unstaged)
	}
}

// A --drivers-dir the update cannot honour is an operator error, so it fails
// outright instead of becoming a prompt about the device.
func TestCheckDriversDir(t *testing.T) {
	withImages := driversDirWith(t, "acme-nic")
	conn := connWithAddons("acme-nic")

	if err := checkDriversDir(conn, withImages); err != nil {
		t.Errorf("a directory with images was rejected: %v", err)
	}
	if err := checkDriversDir(conn, t.TempDir()); err == nil {
		t.Error("an empty directory was accepted")
	}
	if err := checkDriversDir(&grpcclient.AgentConnection{}, withImages); err == nil {
		t.Error("a connection with no driver service was accepted")
	}
}

// Piped into CI there is nobody to ask, so the pre-flight has to fail with a
// message naming what would be lost and the way to override it.
func TestConfirmDriverPreflight_NonInteractiveAborts(t *testing.T) {
	orig := isInteractiveTerminalFn
	isInteractiveTerminalFn = func() bool { return false }
	t.Cleanup(func() { isInteractiveTerminalFn = orig })

	err := confirmDriverPreflight(driverPreflight{
		unstaged: []unstagedAddon{{"acme-nic", "no rebuild published for 0.20.0"}},
	})
	if err == nil {
		t.Fatal("a non-interactive run was allowed to proceed")
	}
	if !strings.Contains(err.Error(), "acme-nic") || !strings.Contains(err.Error(), "--no-drivers") {
		t.Errorf("error = %q, want the add-on named and --no-drivers offered", err)
	}
}

// --no-drivers says stage nothing and --drivers-dir says stage these; the pair is
// refused rather than one of them silently winning.
func TestOSUpdate_NoDriversWithDriversDirIsRejected(t *testing.T) {
	cmd := newOSUpdateCmd()
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	cmd.SetArgs([]string{"--no-drivers", "--drivers-dir", t.TempDir()})
	// Asserting the message, not just an error: this command fails for plenty of
	// other reasons off a device, and any of them would pass a bare nil check.
	err := cmd.Execute()
	if err == nil || !strings.Contains(err.Error(), "--no-drivers cannot be combined with --drivers-dir") {
		t.Fatalf("err = %v, want the flag pair refused", err)
	}
}
