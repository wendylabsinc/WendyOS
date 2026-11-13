package commands

import (
	"bytes"
	"context"
	"io"
	"os"
	"os/exec"
	"strings"
	"testing"

	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/empty"
	"github.com/google/go-containerregistry/pkg/v1/layout"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/random"
	"github.com/google/go-containerregistry/pkg/v1/types"
	"github.com/spf13/cobra"
	"google.golang.org/grpc"

	"github.com/wendylabsinc/wendy/go/internal/cli/tui"
	"github.com/wendylabsinc/wendy/go/internal/shared/config"
	"github.com/wendylabsinc/wendy/go/internal/shared/registrycrypto"
	cloudpb "github.com/wendylabsinc/wendy/go/proto/gen/cloudpb/v2"
)

// appFixture runs app commands in an empty folder, without a terminal,
// against the fake cloud; any question asked fails the test.
func appFixture(t *testing.T) (*registryOp, *fakeRegistry) {
	t.Helper()
	op, fake := registryFixture(t)
	t.Chdir(t.TempDir())
	origTerm, origText, origConfirm, origAuth, origCloud := isInteractiveTerminalFn, askText, askConfirm, pickAppAuth, withAppCloud
	t.Cleanup(func() {
		isInteractiveTerminalFn, askText, askConfirm, pickAppAuth, withAppCloud = origTerm, origText, origConfirm, origAuth, origCloud
	})
	t.Cleanup(tui.SetPromptOutput(io.Discard))
	isInteractiveTerminalFn = func() bool { return false }
	askText = func(prompt, _, _ string, _ tui.ValidateFunc) (string, error) {
		t.Errorf("asked %q", prompt)
		return "", nil
	}
	askConfirm = func(q string) (bool, error) {
		t.Errorf("asked %q", q)
		return false, nil
	}
	pickAppAuth = func(string) (*config.AuthConfig, error) { return op.auth, nil }
	withAppCloud = func(ctx context.Context, _ *config.AuthConfig, call func(context.Context, grpc.ClientConnInterface) error) error {
		return call(ctx, op.conn)
	}
	return op, fake
}

func runApp(cmd *cobra.Command, args ...string) (string, error) {
	var out bytes.Buffer
	cmd.SetArgs(args)
	cmd.SetOut(&out)
	cmd.SetErr(io.Discard)
	err := cmd.Execute()
	return out.String(), err
}

// With every flag given nothing is asked, even in a terminal: the app, its
// notification choice and each binding at APP scope are created, and the
// next step is the exact publish command.
func TestAppCreateWithAllFlagsAsksNothing(t *testing.T) {
	_, fake := appFixture(t)
	isInteractiveTerminalFn = func() bool { return true }
	group := "https://auth.example/realms/acme#ops"
	out, err := runApp(newAppCreateCmd(), "--name", "Cam", "--app", "cam.app", "--details", "Counts cars", "--notifications",
		"--access", "ana@example.com=developer", "--access", group+"=App viewer", "--yes")
	if err != nil {
		t.Fatal(err)
	}
	ups := fake.signed(cloudpb.AppService_UpsertApp_FullMethodName)
	if len(ups) != 1 || ups[0].resource != "org/"+testOperatorTenant+"/app/cam.app" {
		t.Fatalf("UpsertApp %+v", ups)
	}
	if r := ups[0].payload.(*cloudpb.UpsertAppRequest); r.GetId() != "cam.app" || r.GetName() != "Cam" || r.GetDetails() != "Counts cars" {
		t.Fatalf("UpsertApp %v", r)
	}
	upd := fake.signed(cloudpb.AppService_UpdateApp_FullMethodName)
	if len(upd) != 1 || !upd[0].payload.(*cloudpb.UpdateAppRequest).GetCanSendNotifications() {
		t.Fatalf("UpdateApp %+v", upd)
	}
	binds := fake.signed(cloudpb.AccessService_CreateBinding_FullMethodName)
	want := []struct {
		kind     cloudpb.BindingPrincipalType
		who, rid string
	}{
		{cloudpb.BindingPrincipalType_BINDING_PRINCIPAL_TYPE_USER, "u-ana", "r-dev"},
		{cloudpb.BindingPrincipalType_BINDING_PRINCIPAL_TYPE_GROUP, group, "r-view"},
	}
	if len(binds) != len(want) {
		t.Fatalf("%d bindings", len(binds))
	}
	for i, w := range want {
		b := binds[i].payload.(*cloudpb.CreateBindingRequest)
		if binds[i].resource != "org/"+testOperatorTenant+"/access-binding" || b.GetOrganizationId() != testOperatorTenant ||
			b.GetPrincipalType() != w.kind || b.GetPrincipalId() != w.who || b.GetRoleId() != w.rid ||
			b.GetScopeType() != cloudpb.BindingScopeType_BINDING_SCOPE_TYPE_APP || b.GetScopeRef() != "cam.app" || b.GetEffect() != cloudpb.BindingEffect_BINDING_EFFECT_ALLOW {
			t.Fatalf("binding %d: %v", i, b)
		}
	}
	for _, s := range []string{"Nothing is created until you confirm.", "Cam is created. It has no release yet.", "wendy app publish --app cam.app --tag 1.0.0"} {
		if !strings.Contains(out, s) {
			t.Fatalf("output lacks %q:\n%s", s, out)
		}
	}
}

// Without a terminal a missing answer names its flag, and nothing reaches Cloud.
func TestAppCreateNamesTheMissingFlagWithoutTerminal(t *testing.T) {
	_, fake := appFixture(t)
	for _, tc := range []struct {
		args []string
		want string
	}{
		{[]string{"--name", "Cam", "--yes"}, "No app id given. Add --app <id>, or run in a terminal to answer."},
		{[]string{"--app", "cam.app", "--yes"}, "No name given. Add --name <name>, or run in a terminal to answer."},
		{[]string{"--name", "Cam", "--app", "cam.app"}, "Add --yes"},
		{[]string{"--name", "Cam", "--app", "Cam_App", "--yes"}, "Use lowercase letters, digits, dots and hyphens."},
		{[]string{"--name", "Cam", "--app", "cam.app", "--access", "ops=viewer", "--yes"}, "name a person by email"},
	} {
		if _, err := runApp(newAppCreateCmd(), tc.args...); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Fatalf("%v: %v", tc.args, err)
		}
	}
	if len(fake.calls) != 0 {
		t.Fatalf("calls %+v", fake.calls)
	}
}

// A taken id and a person who is not a member are refused before anything is
// created.
func TestAppCreateRefusesTakenIDAndUnknownPeople(t *testing.T) {
	_, fake := appFixture(t)
	fake.apps["cam.app"] = &cloudpb.App{Id: "cam.app"}
	if _, err := runApp(newAppCreateCmd(), "--name", "Cam", "--app", "cam.app", "--yes"); err == nil || !strings.Contains(err.Error(), "The app id cam.app is taken") {
		t.Fatalf("taken id: %v", err)
	}
	if _, err := runApp(newAppCreateCmd(), "--name", "Cam", "--app", "cam2", "--access", "bo@example.com=viewer", "--yes"); err == nil || !strings.Contains(err.Error(), "bo@example.com is not a member") {
		t.Fatalf("unknown person: %v", err)
	}
	if len(fake.calls) != 0 {
		t.Fatalf("calls %+v", fake.calls)
	}
}

// UpdateApp is sent only when notifications are chosen; a role that may not
// turn them on is told so and the app is still created.
func TestAppCreateNotifications(t *testing.T) {
	_, fake := appFixture(t)
	if _, err := runApp(newAppCreateCmd(), "--name", "A", "--app", "a", "--no-notifications", "--yes"); err != nil {
		t.Fatal(err)
	}
	if n := len(fake.signed(cloudpb.AppService_UpdateApp_FullMethodName)); n != 0 {
		t.Fatalf("%d UpdateApp without notifications", n)
	}
	fake.noNotify = true
	out, err := runApp(newAppCreateCmd(), "--name", "B", "--app", "b", "--notifications", "--yes")
	if err != nil || !strings.Contains(out, "Your organization role doesn't allow notifications. Ask an admin.") || !strings.Contains(out, "B is created.") {
		t.Fatalf("%v\n%s", err, out)
	}
}

// writePublishProject writes wendy.json and an OCI layout of one small image into
// the current folder, and returns the layout's path.
func writePublishProject(t *testing.T, wendyJSON string) string {
	t.Helper()
	if err := os.WriteFile("wendy.json", []byte(wendyJSON), 0o600); err != nil {
		t.Fatal(err)
	}
	layer, err := random.Layer(1000, types.OCILayer)
	if err != nil {
		t.Fatal(err)
	}
	img, err := mutate.AppendLayers(empty.Image, layer)
	if err != nil {
		t.Fatal(err)
	}
	p, err := layout.Write(t.TempDir(), empty.Index)
	if err != nil {
		t.Fatal(err)
	}
	if err := p.AppendImage(img); err != nil {
		t.Fatal(err)
	}
	return string(p)
}

// published is the one release's create request, manifest and image config,
// decrypted with this machine's wrap.
func published(t *testing.T, op *registryOp, fake *fakeRegistry) (*cloudpb.CreateAppReleaseRequest, *cloudpb.ArtifactManifest, *v1.ConfigFile) {
	t.Helper()
	creates := fake.signed(cloudpb.DeploymentService_CreateAppRelease_FullMethodName)
	if len(creates) != 1 || len(fake.releases) != 1 {
		t.Fatalf("%d releases created, %d published", len(creates), len(fake.releases))
	}
	var id string
	var m *cloudpb.ArtifactManifest
	for id, m = range fake.releases {
	}
	manifest, err := v1.ParseManifest(bytes.NewReader(m.GetOciManifest()))
	if err != nil {
		t.Fatal(err)
	}
	key, err := op.contentKey(context.Background(), id, m)
	if err != nil {
		t.Fatal(err)
	}
	for _, b := range m.GetBlobs() {
		if b.GetPlaintextDigest() != manifest.Config.Digest.String() {
			continue
		}
		var plain bytes.Buffer
		want := registrycrypto.Blob{PlaintextDigest: b.PlaintextDigest, CiphertextDigest: b.CiphertextDigest, PlaintextSize: b.PlaintextSize, CiphertextSize: b.CiphertextSize}
		if err := registrycrypto.DecryptBlob(&plain, bytes.NewReader(fake.blobs[b.CiphertextDigest]), key, want, int(m.GetChunkSize())); err != nil {
			t.Fatal(err)
		}
		cf, err := v1.ParseConfigFile(&plain)
		if err != nil {
			t.Fatal(err)
		}
		return creates[0].payload.(*cloudpb.CreateAppReleaseRequest), m, cf
	}
	t.Fatal("no config blob")
	return nil, nil, nil
}

// Publish labels the image with wendy.json (compact, known fields only) and
// sends its env as the release's default configuration.
func TestPublishWritesAppDefinitionAndDefaults(t *testing.T) {
	op, fake := appFixture(t)
	fake.apps["sh.wendy.example"] = &cloudpb.App{Id: "sh.wendy.example"}
	dir := writePublishProject(t, `{"$schema": "x", "appId": "sh.wendy.example", "version": "1.0", "env": {"LOG_LEVEL": "info"}}`)
	if _, err := runApp(newAppPublishCmd(), "--tag", "1.0.0", "--oci-layout", dir); err != nil {
		t.Fatal(err)
	}
	req, _, cf := published(t, op, fake)
	if req.GetName() != "1.0.0" || req.GetDefaultConfigJson() != `{"LOG_LEVEL":"info"}` {
		t.Fatalf("release %v", req)
	}
	if got := cf.Config.Labels[appConfigLabel]; got != `{"appId":"sh.wendy.example","version":"1.0","env":{"LOG_LEVEL":"info"}}` {
		t.Fatalf("label %q", got)
	}
}

// --untagged names the release <branch>@<manifest digest>; no env is `{}`.
func TestPublishUntaggedNamesBranchAtDigest(t *testing.T) {
	op, fake := appFixture(t)
	fake.apps["sh.wendy.example"] = &cloudpb.App{Id: "sh.wendy.example"}
	if out, err := exec.Command("git", "init", "-q", "-b", "topic", ".").CombinedOutput(); err != nil {
		t.Fatalf("git init: %v %s", err, out)
	}
	dir := writePublishProject(t, `{"appId": "sh.wendy.example"}`)
	if _, err := runApp(newAppPublishCmd(), "--untagged", "--oci-layout", dir); err != nil {
		t.Fatal(err)
	}
	req, m, _ := published(t, op, fake)
	if req.GetName() != "topic@"+m.GetManifestDigest() || req.GetDefaultConfigJson() != "{}" {
		t.Fatalf("release %v", req)
	}
}

// What the device refuses, and flag mistakes, are refused before anything
// reaches Cloud.
func TestPublishRefusesBeforeUploading(t *testing.T) {
	_, fake := appFixture(t)
	big := `{"appId": "sh.wendy.example", "env": {"A": "` + strings.Repeat("a", 30000) + `", "B": "` + strings.Repeat("b", 30000) + `", "C": "` + strings.Repeat("c", 30000) + `"}}`
	for _, tc := range []struct {
		json string
		args []string
		want string
	}{
		{`{"appId": "sh.wendy.example", "services": {"a": {"context": "."}}}`, []string{"--tag", "1"}, "apps with several services cannot be deployed yet; deploy each service as its own app"},
		{`{"appId": "sh.wendy.example", "colour": "red"}`, []string{"--tag", "1"}, "ignored: colour"},
		{big, []string{"--tag", "1"}, "a device accepts"},
		{`{"appId": "sh.wendy.example"}`, []string{"--tag", "1", "--app", "other"}, "--app must match appId in wendy.json (sh.wendy.example)."},
		{`{"appId": "sh.wendy.example"}`, []string{"--tag", "1", "--untagged"}, "not both"},
		{`{"appId": "sh.wendy.example"}`, nil, "No tag given. Add --tag <version>"},
	} {
		dir := writePublishProject(t, tc.json)
		if _, err := runApp(newAppPublishCmd(), append(tc.args, "--oci-layout", dir)...); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Fatalf("%s %v: %v", tc.json, tc.args, err)
		}
	}
	if len(fake.calls) != 0 {
		t.Fatalf("calls %+v", fake.calls)
	}
}

// An app Cloud does not have: without a terminal, the next step; in one, the
// create questions (prefilled from wendy.json), then the publish.
func TestPublishCreatesAnUnknownApp(t *testing.T) {
	op, fake := appFixture(t)
	dir := writePublishProject(t, `{"appId": "sh.wendy.example"}`)
	_, err := runApp(newAppPublishCmd(), "--tag", "1", "--oci-layout", dir)
	if err == nil || err.Error() != "sh.wendy.example doesn't exist yet. Run wendy app create --app sh.wendy.example first, or run this in a terminal." {
		t.Fatalf("unknown app: %v", err)
	}
	isInteractiveTerminalFn = func() bool { return true }
	askText = func(_, _, def string, _ tui.ValidateFunc) (string, error) { return def, nil }
	askConfirm = func(q string) (bool, error) { return q == "Create app?", nil }
	if _, err := runApp(newAppPublishCmd(), "--tag", "1", "--oci-layout", dir); err != nil {
		t.Fatal(err)
	}
	if a := fake.apps["sh.wendy.example"]; a == nil || a.GetName() != "sh.wendy.example" {
		t.Fatalf("app %v", a)
	}
	published(t, op, fake)
}
