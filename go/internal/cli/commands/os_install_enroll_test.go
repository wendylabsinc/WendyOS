//go:build darwin || linux || windows

package commands

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/wendylabsinc/wendy/go/internal/cli/tui"
	"github.com/wendylabsinc/wendy/go/internal/shared/config"
	cloudpbv2 "github.com/wendylabsinc/wendy/go/proto/gen/cloudpb/v2"
)

// stubEnrollPrompts replaces the interactive hooks used by the enrollment
// flow and restores them on cleanup. Each stub fails the test if invoked,
// so individual tests override only the hooks they expect to fire.
func stubEnrollPrompts(t *testing.T) {
	t.Helper()
	origSession := promptEnrollmentSession
	origPreEnroll := confirmPreEnroll
	origContinue := confirmContinueUnenrolled
	origEnroll := preEnrollDeviceFn
	origResolveOrg := resolveOrgFn
	promptEnrollmentSession = func([]tui.PickerItem) (string, error) {
		t.Fatal("unexpected session picker")
		return "", nil
	}
	confirmPreEnroll = func() (bool, error) {
		t.Fatal("unexpected pre-enroll confirm")
		return false, nil
	}
	confirmContinueUnenrolled = func() (bool, error) {
		t.Fatal("unexpected continue-unenrolled confirm")
		return false, nil
	}
	preEnrollDeviceFn = func(context.Context, *config.AuthConfig, string, int32, PreEnrollDialer) (*PreProvisionedState, error) {
		t.Fatal("unexpected enrollment call")
		return nil, nil
	}
	origACME := preEnrollACMEFn
	preEnrollACMEFn = func(context.Context, *config.AuthConfig, string) (*acmeEnrollmentBake, error) {
		t.Fatal("unexpected ACME enrollment call")
		return nil, nil
	}
	origName := promptDeviceName
	promptDeviceName = func(string, string, tui.ValidateFunc) (string, error) {
		t.Fatal("unexpected device-name prompt")
		return "", nil
	}
	// Default org resolver returns a stable test org so tests that reach
	// preEnrollDeviceFn don't need a live cloud connection.
	resolveOrgFn = func(_ context.Context, _ *config.AuthConfig, _ bool) (OrgResolution, error) {
		return OrgResolution{ID: 7, Name: "Test Org"}, nil
	}
	t.Cleanup(func() {
		promptEnrollmentSession = origSession
		confirmPreEnroll = origPreEnroll
		confirmContinueUnenrolled = origContinue
		preEnrollDeviceFn = origEnroll
		preEnrollACMEFn = origACME
		promptDeviceName = origName
		resolveOrgFn = origResolveOrg
	})
}

func twoSessionConfig() *config.Config {
	return &config.Config{Auth: []config.AuthConfig{
		{
			CloudDashboard: "https://cloud.wendy.dev",
			CloudGRPC:      "prod.example.com:443",
			Certificates:   []config.CertificateInfo{{OrganizationID: 7}},
		},
		{
			CloudDashboard: "http://localhost:3000",
			CloudGRPC:      "localhost:50051",
			Certificates:   []config.CertificateInfo{{OrganizationID: 1}},
		},
	}}
}

func TestSelectEnrollmentAuthNoSessions(t *testing.T) {
	stubEnrollPrompts(t)
	_, err := selectEnrollmentAuth(&config.Config{}, "", true)
	if err == nil || !strings.Contains(err.Error(), "not logged in") {
		t.Fatalf("expected not-logged-in error, got %v", err)
	}
}

func TestSelectEnrollmentAuthSingleSession(t *testing.T) {
	stubEnrollPrompts(t)
	cfg := &config.Config{Auth: []config.AuthConfig{{
		CloudGRPC:    "prod.example.com:443",
		Certificates: []config.CertificateInfo{{OrganizationID: 7}},
	}}}
	auth, err := selectEnrollmentAuth(cfg, "", false)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if auth == nil || auth.CloudGRPC != "prod.example.com:443" {
		t.Fatalf("got %+v; want the single session", auth)
	}
}

func TestSelectEnrollmentAuthSingleSessionNoCerts(t *testing.T) {
	stubEnrollPrompts(t)
	cfg := &config.Config{Auth: []config.AuthConfig{{CloudGRPC: "prod.example.com:443"}}}
	_, err := selectEnrollmentAuth(cfg, "", true)
	if err == nil || !strings.Contains(err.Error(), "no certificates") {
		t.Fatalf("expected no-certificates error, got %v", err)
	}
}

func TestSelectEnrollmentAuthCloudGRPCFlag(t *testing.T) {
	stubEnrollPrompts(t)
	auth, err := selectEnrollmentAuth(twoSessionConfig(), "localhost:50051", false)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if auth == nil || auth.CloudGRPC != "localhost:50051" {
		t.Fatalf("got %+v; want the localhost session", auth)
	}
}

func TestSelectEnrollmentAuthCloudGRPCFlagNoMatch(t *testing.T) {
	stubEnrollPrompts(t)
	_, err := selectEnrollmentAuth(twoSessionConfig(), "missing.example.com:443", true)
	if err == nil || !strings.Contains(err.Error(), "no auth session for missing.example.com:443") {
		t.Fatalf("expected no-session error, got %v", err)
	}
}

func TestSelectEnrollmentAuthMultipleNonInteractive(t *testing.T) {
	stubEnrollPrompts(t)
	_, err := selectEnrollmentAuth(twoSessionConfig(), "", false)
	if err == nil || !strings.Contains(err.Error(), "--cloud-grpc") {
		t.Fatalf("expected multi-session error mentioning --cloud-grpc, got %v", err)
	}
}

func TestSelectEnrollmentAuthMultipleInteractivePicker(t *testing.T) {
	stubEnrollPrompts(t)
	var gotItems []tui.PickerItem
	promptEnrollmentSession = func(items []tui.PickerItem) (string, error) {
		gotItems = items
		return "1", nil // second session
	}

	auth, err := selectEnrollmentAuth(twoSessionConfig(), "", true)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if auth == nil || auth.CloudGRPC != "localhost:50051" {
		t.Fatalf("got %+v; want the localhost session", auth)
	}
	if len(gotItems) != 3 {
		t.Fatalf("picker should list 2 sessions + skip, got %d items", len(gotItems))
	}
	last := gotItems[len(gotItems)-1]
	if last.Value != skipEnrollmentValue {
		t.Fatalf("last picker item should be the skip option, got %+v", last)
	}
	if !strings.Contains(gotItems[0].Description, "org 7") {
		t.Errorf("session items should show the org id, got %q", gotItems[0].Description)
	}
}

func TestSelectEnrollmentAuthSkipOption(t *testing.T) {
	stubEnrollPrompts(t)
	promptEnrollmentSession = func([]tui.PickerItem) (string, error) {
		return skipEnrollmentValue, nil
	}
	auth, err := selectEnrollmentAuth(twoSessionConfig(), "", true)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if auth != nil {
		t.Fatalf("skip must return nil auth, got %+v", auth)
	}
}

func TestSelectEnrollmentAuthPickerCancelled(t *testing.T) {
	stubEnrollPrompts(t)
	promptEnrollmentSession = func([]tui.PickerItem) (string, error) {
		return "", ErrUserCancelled
	}
	_, err := selectEnrollmentAuth(twoSessionConfig(), "", true)
	if !errors.Is(err, ErrUserCancelled) {
		t.Fatalf("expected ErrUserCancelled, got %v", err)
	}
}

func TestResolvePreEnrollmentSkipMode(t *testing.T) {
	stubEnrollPrompts(t)
	prov, err := resolvePreEnrollment(context.Background(), twoSessionConfig(), preEnrollOptions{mode: preEnrollSkip}, true, "dev")
	if err != nil || prov != nil {
		t.Fatalf("skip mode must be a no-op, got %v / %v", prov, err)
	}
}

func TestResolvePreEnrollmentAutoNonInteractive(t *testing.T) {
	stubEnrollPrompts(t)
	prov, err := resolvePreEnrollment(context.Background(), twoSessionConfig(), preEnrollOptions{mode: preEnrollAuto}, false, "dev")
	if err != nil || prov != nil {
		t.Fatalf("auto mode without a TTY must be a no-op, got %v / %v", prov, err)
	}
}

func TestResolvePreEnrollmentAutoNoSessions(t *testing.T) {
	stubEnrollPrompts(t)
	prov, err := resolvePreEnrollment(context.Background(), &config.Config{}, preEnrollOptions{mode: preEnrollAuto}, true, "dev")
	if err != nil || prov != nil {
		t.Fatalf("auto mode without sessions must be a no-op, got %v / %v", prov, err)
	}
}

func TestResolvePreEnrollmentAutoDeclined(t *testing.T) {
	stubEnrollPrompts(t)
	confirmPreEnroll = func() (bool, error) { return false, nil }
	prov, err := resolvePreEnrollment(context.Background(), twoSessionConfig(), preEnrollOptions{mode: preEnrollAuto}, true, "dev")
	if err != nil || prov != nil {
		t.Fatalf("declining the pre-enroll offer must be a no-op, got %v / %v", prov, err)
	}
}

func TestResolvePreEnrollmentSuccess(t *testing.T) {
	stubEnrollPrompts(t)
	confirmPreEnroll = func() (bool, error) { return true, nil }
	promptEnrollmentSession = func([]tui.PickerItem) (string, error) { return "0", nil }
	preEnrollDeviceFn = func(_ context.Context, auth *config.AuthConfig, name string, _ int32, _ PreEnrollDialer) (*PreProvisionedState, error) {
		if auth.CloudGRPC != "prod.example.com:443" {
			t.Fatalf("enrolled against %s; want the picked session", auth.CloudGRPC)
		}
		if name != "dev" {
			t.Fatalf("device name %q; want dev", name)
		}
		return &PreProvisionedState{Enrolled: true}, nil
	}

	prov, err := resolvePreEnrollment(context.Background(), twoSessionConfig(), preEnrollOptions{mode: preEnrollAuto}, true, "dev")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if prov == nil || !prov.Enrolled {
		t.Fatalf("got %+v; want enrolled provisioning state", prov)
	}
}

func TestResolvePreEnrollmentUserSkips(t *testing.T) {
	stubEnrollPrompts(t)
	confirmPreEnroll = func() (bool, error) { return true, nil }
	promptEnrollmentSession = func([]tui.PickerItem) (string, error) { return skipEnrollmentValue, nil }

	prov, err := resolvePreEnrollment(context.Background(), twoSessionConfig(), preEnrollOptions{mode: preEnrollAuto}, true, "dev")
	if err != nil || prov != nil {
		t.Fatalf("explicit skip must continue without JSON, got %v / %v", prov, err)
	}
}

func TestResolvePreEnrollmentFailureAcknowledged(t *testing.T) {
	stubEnrollPrompts(t)
	confirmPreEnroll = func() (bool, error) { return true, nil }
	promptEnrollmentSession = func([]tui.PickerItem) (string, error) { return "0", nil }
	preEnrollDeviceFn = func(context.Context, *config.AuthConfig, string, int32, PreEnrollDialer) (*PreProvisionedState, error) {
		return nil, errors.New("cloud unreachable")
	}
	confirmContinueUnenrolled = func() (bool, error) { return true, nil }

	prov, err := resolvePreEnrollment(context.Background(), twoSessionConfig(), preEnrollOptions{mode: preEnrollAuto}, true, "dev")
	if err != nil || prov != nil {
		t.Fatalf("acknowledged failure must continue without JSON, got %v / %v", prov, err)
	}
}

func TestResolvePreEnrollmentFailureDeclinedCancelsInstall(t *testing.T) {
	stubEnrollPrompts(t)
	confirmPreEnroll = func() (bool, error) { return true, nil }
	promptEnrollmentSession = func([]tui.PickerItem) (string, error) { return "0", nil }
	preEnrollDeviceFn = func(context.Context, *config.AuthConfig, string, int32, PreEnrollDialer) (*PreProvisionedState, error) {
		return nil, errors.New("cloud unreachable")
	}
	confirmContinueUnenrolled = func() (bool, error) { return false, nil }

	_, err := resolvePreEnrollment(context.Background(), twoSessionConfig(), preEnrollOptions{mode: preEnrollAuto}, true, "dev")
	if !errors.Is(err, ErrUserCancelled) {
		t.Fatalf("declining must cancel the install, got %v", err)
	}
}

func TestResolvePreEnrollmentForcedNonInteractiveFailureIsFatal(t *testing.T) {
	stubEnrollPrompts(t)
	cfg := &config.Config{Auth: []config.AuthConfig{{
		CloudGRPC:    "prod.example.com:443",
		Certificates: []config.CertificateInfo{{OrganizationID: 7}},
	}}}
	preEnrollDeviceFn = func(context.Context, *config.AuthConfig, string, int32, PreEnrollDialer) (*PreProvisionedState, error) {
		return nil, errors.New("cloud unreachable")
	}

	_, err := resolvePreEnrollment(context.Background(), cfg, preEnrollOptions{mode: preEnrollForced}, false, "dev")
	if err == nil || !strings.Contains(err.Error(), "--pre-enroll") {
		t.Fatalf("non-interactive --pre-enroll failure must be fatal, got %v", err)
	}
}

func TestResolvePreEnrollmentForcedNonInteractiveMultiSession(t *testing.T) {
	stubEnrollPrompts(t)
	_, err := resolvePreEnrollment(context.Background(), twoSessionConfig(), preEnrollOptions{mode: preEnrollForced}, false, "dev")
	if err == nil || !strings.Contains(err.Error(), "--cloud-grpc") {
		t.Fatalf("expected fatal multi-session error mentioning --cloud-grpc, got %v", err)
	}
}

func TestResolvePreEnrollmentForcedSkipsConfirm(t *testing.T) {
	stubEnrollPrompts(t)
	// confirmPreEnroll stays at the t.Fatal stub: forced mode must never ask
	// "Pre-enroll this device?".
	promptEnrollmentSession = func([]tui.PickerItem) (string, error) { return "0", nil }
	preEnrollDeviceFn = func(context.Context, *config.AuthConfig, string, int32, PreEnrollDialer) (*PreProvisionedState, error) {
		return &PreProvisionedState{}, nil
	}
	if _, err := resolvePreEnrollment(context.Background(), twoSessionConfig(), preEnrollOptions{mode: preEnrollForced}, true, "dev"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestSelectEnrollmentAuthUsesDefault(t *testing.T) {
	stubEnrollPrompts(t) // picker stub fails the test if invoked
	cfg := twoSessionConfig()
	cfg.DefaultCloudGRPC = "localhost:50051"
	auth, err := selectEnrollmentAuth(cfg, "", true)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if auth == nil || auth.CloudGRPC != "localhost:50051" {
		t.Fatalf("default should be used without the picker, got %+v", auth)
	}
}

func TestMapConfirmCancel(t *testing.T) {
	if _, err := mapConfirmCancel(false, tui.ErrCancelled); !errors.Is(err, ErrUserCancelled) {
		t.Fatalf("tui.ErrCancelled must map to ErrUserCancelled, got %v", err)
	}
	if ok, err := mapConfirmCancel(true, nil); !ok || err != nil {
		t.Fatalf("clean result must pass through, got %v / %v", ok, err)
	}
	otherErr := errors.New("boom")
	if _, err := mapConfirmCancel(false, otherErr); !errors.Is(err, otherErr) {
		t.Fatalf("other errors must pass through, got %v", err)
	}
}

func TestResolvePreEnrollmentConfirmCancelled(t *testing.T) {
	stubEnrollPrompts(t)
	confirmPreEnroll = func() (bool, error) { return false, ErrUserCancelled }

	_, err := resolvePreEnrollment(context.Background(), twoSessionConfig(), preEnrollOptions{mode: preEnrollAuto}, true, "dev")
	if !errors.Is(err, ErrUserCancelled) {
		t.Fatalf("cancel at the pre-enroll confirm must cancel the install, got %v", err)
	}
}

func TestResolvePreEnrollmentAuthErrorInteractiveAsksToContinue(t *testing.T) {
	stubEnrollPrompts(t)
	// Single session without certificates: selection fails, user accepts
	// continuing unenrolled.
	cfg := &config.Config{Auth: []config.AuthConfig{{CloudGRPC: "prod.example.com:443"}}}
	confirmPreEnroll = func() (bool, error) { return true, nil }
	confirmContinueUnenrolled = func() (bool, error) { return true, nil }

	prov, err := resolvePreEnrollment(context.Background(), cfg, preEnrollOptions{mode: preEnrollAuto}, true, "dev")
	if err != nil || prov != nil {
		t.Fatalf("acknowledged auth failure must continue without JSON, got %v / %v", prov, err)
	}
}

func TestACMEPreEnrollmentBakesEABMaterial(t *testing.T) {
	stubEnrollPrompts(t)
	want := &acmeEnrollmentBake{
		DirectoryURL: "https://acme.dev.pki.wendy.sh/" + testOperatorTenant + "/acme/directory",
		DeviceID:     "11111111-1111-4111-8111-111111111111",
		EABKeyID:     "eab-id",
		EABHMACKey:   strings.Repeat("ab", 32),
		CloudHost:    "prod.example.com:443",
	}
	var gotAuth *config.AuthConfig
	var gotName string
	preEnrollACMEFn = func(_ context.Context, auth *config.AuthConfig, name string) (*acmeEnrollmentBake, error) {
		gotAuth, gotName = auth, name
		return want, nil
	}
	name := "dev"
	got, err := resolveACMEPreEnrollment(context.Background(), twoSessionConfig(),
		preEnrollOptions{mode: preEnrollForced, cloudGRPC: "prod.example.com:443"}, false, &name)
	if err != nil {
		t.Fatalf("resolveACMEPreEnrollment: %v", err)
	}
	if got != want {
		t.Fatalf("bake = %#v, want %#v", got, want)
	}
	if gotName != "dev" || name != "dev" || gotAuth == nil || gotAuth.CloudGRPC != "prod.example.com:443" {
		t.Fatalf("mint got auth=%v name=%q (install name %q)", gotAuth, gotName, name)
	}
	// The baked JSON is the on-disk contract the device consumes on first boot:
	// the first four keys must match acmeenroll.Config's tags, plus cloudHost.
	blob, err := json.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	for _, frag := range []string{
		`"directoryURL":"https://acme.dev.pki.wendy.sh/`,
		`"deviceID":"11111111-1111-4111-8111-111111111111"`,
		`"eabKeyID":"eab-id"`,
		`"eabHMACKey":"` + strings.Repeat("ab", 32) + `"`,
		`"cloudHost":"prod.example.com:443"`,
	} {
		if !strings.Contains(string(blob), frag) {
			t.Errorf("acme-enrollment.json missing %s; got %s", frag, blob)
		}
	}
}

// cloudAcceptsDeviceName is EnrollDeviceRequest.name's rule (WDY-3016):
// lowercase, letter-led, at most 63 characters, no trailing hyphen.
func cloudAcceptsDeviceName(name string) bool {
	return regexp.MustCompile(`^[a-z][a-z0-9-]{0,62}$`).MatchString(name) && !strings.HasSuffix(name, "-")
}

// A blank device name ("empty = auto-generate") used to reach Cloud's
// EnrollDevice as "", which Cloud refuses (WDY-3939 #13). Pre-enrolling has to
// settle on a name first, and the device has to be given that same name, so
// its hostname and its Cloud row agree.
func TestProvisioningJSONBlankNameReservesTheNameItBakes(t *testing.T) {
	stubEnrollPrompts(t)
	t.Setenv("WENDY_CONFIG_DIR", t.TempDir())
	t.Setenv("WENDY_SECRET_STORE", "file")
	cloud := &oidcEnrollmentServer{response: &cloudpbv2.EnrollDeviceResponse{CredentialKind: "eab", EabKeyId: "eab-id", EabHmacKey: strings.Repeat("ab", 32)}}
	_, host := enrollmentServers(t, cloud, &acmeProvisioningServer{})
	auth := oidcEnrollmentAuth(t)
	auth.CloudGRPC = host
	if err := config.Save(&config.Config{Auth: []config.AuthConfig{*auth}}); err != nil {
		t.Fatal(err)
	}
	origInteractive := isInteractiveTerminalFn
	t.Cleanup(func() { isInteractiveTerminalFn = origInteractive })
	isInteractiveTerminalFn = func() bool { return true }
	// The default interactive path: Enter at "Pre-enroll this device?".
	confirmPreEnroll = func() (bool, error) { return true, nil }
	preEnrollACMEFn = preEnrollDeviceACME
	promptDeviceName = func(_, _ string, validate tui.ValidateFunc) (string, error) {
		if validate("") == nil {
			t.Error("the pre-enroll name prompt accepts an empty name")
		}
		return "lab-box", nil
	}

	name := ""
	provJSON, err := resolveProvisioningJSON(context.Background(), preEnrollOptions{mode: preEnrollAuto}, &name)
	if err != nil {
		t.Fatalf("resolveProvisioningJSON: %v", err)
	}
	if cloud.req == nil {
		t.Fatal("no EnrollDevice request reached Cloud")
	}
	sent := cloud.req.GetName()
	if sent != "lab-box" || !cloudAcceptsDeviceName(sent) {
		t.Fatalf("EnrollDevice name = %q, want the prompted %q", sent, "lab-box")
	}
	if name != sent {
		t.Fatalf("the install would bake device name %q, but Cloud reserved %q", name, sent)
	}

	// The callers write that same name to the config partition.
	dir := t.TempDir()
	if err := writeConfigFiles(dir, nil, nil, name, provJSON); err != nil {
		t.Fatal(err)
	}
	conf, err := os.ReadFile(filepath.Join(dir, "wendy.conf"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(conf), "[device]\nname = "+sent+"\n") {
		t.Fatalf("wendy.conf does not name the device %q:\n%s", sent, conf)
	}
	if _, err := os.Stat(filepath.Join(dir, "acme-enrollment.json")); err != nil {
		t.Fatalf("the EAB credential was not baked: %v", err)
	}
}

// Without a terminal there is no one to ask, so --pre-enroll needs
// --device-name before any credential is minted.
func TestACMEPreEnrollmentForcedNonInteractiveBlankNameNeedsDeviceName(t *testing.T) {
	stubEnrollPrompts(t) // preEnrollACMEFn and promptDeviceName fail the test if called
	name := ""
	_, err := resolveACMEPreEnrollment(context.Background(), twoSessionConfig(),
		preEnrollOptions{mode: preEnrollForced, cloudGRPC: "prod.example.com:443"}, false, &name)
	if err == nil || !strings.Contains(err.Error(), "--device-name") {
		t.Fatalf("err = %v, want one naming --device-name", err)
	}
}

// Declining pre-enrollment keeps the blank name: the device names itself on
// first boot, as the prompt promised.
func TestACMEPreEnrollmentDeclinedKeepsBlankName(t *testing.T) {
	stubEnrollPrompts(t)
	confirmPreEnroll = func() (bool, error) { return false, nil }
	name := ""
	got, err := resolveACMEPreEnrollment(context.Background(), twoSessionConfig(),
		preEnrollOptions{mode: preEnrollAuto}, true, &name)
	if err != nil || got != nil || name != "" {
		t.Fatalf("bake=%v err=%v name=%q; want nil, nil and a blank name", got, err, name)
	}
}

func TestACMEPreEnrollmentSkips(t *testing.T) {
	stubEnrollPrompts(t)
	name := "dev"
	got, err := resolveACMEPreEnrollment(context.Background(), twoSessionConfig(),
		preEnrollOptions{mode: preEnrollSkip}, true, &name)
	if err != nil || got != nil {
		t.Fatalf("skip: bake=%v err=%v; want nil,nil", got, err)
	}
}
