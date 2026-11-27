package main

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/world/internal/authority"
	"github.com/wendylabsinc/wendy/go/world/internal/client"
	"github.com/wendylabsinc/wendy/go/world/internal/go2executor"
	"github.com/wendylabsinc/wendy/go/world/internal/tasking"
	"github.com/wendylabsinc/wendy/go/world/internal/world"
)

func testOptions(t *testing.T) options {
	t.Helper()
	repo, e := filepath.Abs("../..")
	if e != nil {
		t.Fatal(e)
	}
	dir := t.TempDir()
	a := go2executor.Alignment{Schema: "wendy.world.go2-alignment.v1", SourceFrame: "odom", TargetFrame: world.FrameRef{ID: "office-floor", Revision: "r1"}, ReferencePoint: "base_footprint", Synthetic: true, PositionUncertaintyM: .1, YawUncertaintyRad: .05}
	p := go2executor.Profile{MotionEnvelope: "isotropic-v1", OdometryMaxEpochAgeS: 120, OdometryIntegrationAllowanceM: .05, ClockErrorMS: 50, MaxAbsRollRad: .05, MaxAbsPitchRad: .05, VerticalObstacles: true, Schema: "wendy.world.go2-profile.v1", FlatFloor: true, HalfLengthM: .425, HalfWidthM: .3, ClearanceM: .05, MaxPositionUncertaintyM: .3, MaxYawUncertaintyRad: .2, MaxForwardMPS: .2, MaxYawRateRPS: .3, ObservationMaxAgeMS: 250, SourceSkewMaxMS: 50, DecisionMaxMS: 250, ExpiryLeadMS: 450, HorizonS: 1, ExtraStopM: .1, ExtraStopRad: .1}
	ab, pb := jsonBytes(a), jsonBytes(p)
	d := strings.Repeat("a", 64)
	b := tasking.Go2Binding{Schema: "wendy.world.go2-execution.v1", PackageSHA256: d, ConversionSHA256: d, LossSHA256: d, ProjectRevision: d, ProjectID: strings.Repeat("b", 32), VMName: "explicit-vm", SourceDigest: go2executor.SupportedOdometrySourceDigest, PolicyBundle: "go2-moe-cts-164k-0.6715-v1", WorldEpoch: 2, RunID: "go2-stage-test", Frame: a.TargetFrame, AlignmentSHA256: digest(ab).SHA256, ProfileSHA256: digest(pb).SHA256}
	o := options{Out: filepath.Join(dir, "stage"), Repo: repo, Binding: filepath.Join(dir, "binding.json"), Alignment: filepath.Join(dir, "alignment.json"), Profile: filepath.Join(dir, "profile.json"), Site: "synthetic-corridor", TaskID: "test-mission", Arch: "arm64", Transport: "host-http", Port: 18885, BridgePort: 18886, Insecure: true, Commission: true, Now: func() time.Time { return time.Unix(1800000000, 0) }}
	for path, raw := range map[string][]byte{o.Binding: jsonBytes(b), o.Alignment: ab, o.Profile: pb} {
		if e := writeNew(path, raw, 0600); e != nil {
			t.Fatal(e)
		}
	}
	return o
}
func fakeBuilder(_, path, arch, target string) error {
	return writeNew(path, []byte("synthetic compiled "+arch+" "+target), 0755)
}
func readJSON(t *testing.T, path string, v any) []byte {
	t.Helper()
	b, e := os.ReadFile(path)
	if e != nil {
		t.Fatal(e)
	}
	if e = authority.DecodeStrict(b, v); e != nil {
		t.Fatal(e)
	}
	return b
}
func TestStageAuthorityAndContextIsolation(t *testing.T) {
	o := testOptions(t)
	builds := []string{}
	m, e := stage(o, func(repo, path, arch, target string) error {
		builds = append(builds, target)
		if _, err := os.Stat(filepath.Join(o.Out, "clients/operator.json")); !errors.Is(err, os.ErrNotExist) {
			t.Fatal("credentials exist before builds")
		}
		return fakeBuilder(repo, path, arch, target)
	})
	if e != nil {
		t.Fatal(e)
	}
	if !reflect.DeepEqual(builds, []string{"./cmd/worldd", "./cmd/world-go2-executor"}) {
		t.Fatal(builds)
	}
	if m.Domain != "simulation" || m.PhysicalAuthority || m.ExpiresAtUnixMS-o.Now().UnixMilli() != (59*time.Minute).Milliseconds() {
		t.Fatal(m)
	}
	var operator, executor client.Config
	readJSON(t, filepath.Join(o.Out, "clients/operator.json"), &operator)
	readJSON(t, filepath.Join(o.Out, "project/executor.json"), &executor)
	if !reflect.DeepEqual(operator.Grant.Grant.Scopes, []string{"mission", "stop", "read"}) || operator.Grant.Grant.Kind != "operator" || !operator.AllowInsecureSimulation {
		t.Fatal("operator scope")
	}
	if !reflect.DeepEqual(executor.Grant.Grant.Scopes, []string{"execute", "read"}) || executor.Grant.Grant.ExecuteAssignee != m.Assignee || executor.Grant.Grant.PublishAudience != m.Audience || executor.Grant.Grant.Domain != "simulation" {
		t.Fatal("executor scope")
	}
	var node map[string]json.RawMessage
	readJSON(t, filepath.Join(o.Out, "project/node.json"), &node)
	var g authority.SignedGrant
	_ = json.Unmarshal(node["peer_grant"], &g)
	if !reflect.DeepEqual(g.Grant.Scopes, []string{"task-status", "replicate", "read"}) || g.Grant.Kind != "device" || g.Grant.TaskStatusAssignee != m.Assignee || g.Grant.TaskStatusAudience != m.Audience {
		t.Fatal("gateway authority")
	}
	var root []byte
	var rev authority.SignedRevocations
	_ = json.Unmarshal(node["root_public_key"], &root)
	_ = json.Unmarshal(node["revocations"], &rev)
	verifier, err := authority.NewVerifier(authority.Config{RootPublicKey: root, Tenant: "lab", Site: o.Site, Domain: "simulation", Audience: m.Audience, Now: o.Now, Replay: authority.NewMemoryReplay(64)})
	if err != nil {
		t.Fatal(err)
	}
	if err = verifier.UpdateRevocations(rev); err != nil {
		t.Fatal(err)
	}
	for _, item := range []struct {
		grant authority.SignedGrant
		scope string
	}{{operator.Grant, "mission"}, {operator.Grant, "stop"}, {executor.Grant, "execute"}, {g, "task-status"}} {
		if _, err = verifier.VerifyGrant(item.grant, item.scope); err != nil {
			t.Fatal("generated grant does not verify", item.scope, err)
		}
	}
	if _, err = verifier.VerifyGrant(executor.Grant, "mission"); err == nil {
		t.Fatal("executor obtained operator authority")
	}
	if _, err = verifier.VerifyGrant(g, "execute"); err == nil {
		t.Fatal("gateway obtained executor authority")
	}
	var tasks struct {
		Path        string             `json:"path"`
		Assignee    string             `json:"assignee"`
		Executor    string             `json:"executor_principal"`
		Catalog     map[string]string  `json:"catalog"`
		Supervision bool               `json:"enable_supervision"`
		Lease       int                `json:"max_lease_ms"`
		Telemetry   int                `json:"max_telemetry_age_ms"`
		MaxTasks    int                `json:"max_tasks"`
		Binding     tasking.Go2Binding `json:"go2_binding"`
	}
	if e = json.Unmarshal(node["tasks"], &tasks); e != nil {
		t.Fatal(e)
	}
	var binding tasking.Go2Binding
	readJSON(t, o.Binding, &binding)
	if tasks.Binding != binding || tasks.Lease != 500 || tasks.Telemetry != 250 || tasks.Supervision || tasks.Executor != executor.Grant.Grant.Principal || !reflect.DeepEqual(tasks.Catalog, map[string]string{tasking.Go2GoToType: "MOVE"}) {
		t.Fatal("gateway task configuration")
	}
	var app struct {
		AppID        string                    `json:"appId"`
		Version      string                    `json:"version"`
		Platform     string                    `json:"platform"`
		Restart      string                    `json:"restartPolicy"`
		Frameworks   map[string]map[string]any `json:"frameworks"`
		Entitlements []map[string]any          `json:"entitlements"`
		Readiness    map[string]any            `json:"readiness"`
	}
	readJSON(t, filepath.Join(o.Out, "project/wendy.json"), &app)
	if app.AppID != m.AppID || app.Platform != "linux/arm64" || app.Frameworks["ros2"]["scope"] != "peripherals" || app.Frameworks["ros2"]["domainId"] != float64(0) || len(app.Entitlements) != 3 {
		t.Fatal("app contract")
	}
	if app.Entitlements[0]["type"] != "network" || app.Entitlements[0]["mode"] != "host" || app.Entitlements[1]["type"] != "http" || app.Entitlements[1]["port"] != float64(o.Port) || app.Entitlements[2]["name"] != m.Volume || app.Entitlements[2]["path"] != "/data" {
		t.Fatal("entitlements")
	}
	files := []string{}
	e = filepath.WalkDir(filepath.Join(o.Out, "project"), func(path string, d os.DirEntry, e error) error {
		if e != nil {
			return e
		}
		if d.IsDir() {
			return nil
		}
		rel, _ := filepath.Rel(o.Out, path)
		files = append(files, rel)
		raw, e := os.ReadFile(path)
		if e != nil {
			return e
		}
		if bytes.Contains(raw, []byte(base64.StdEncoding.EncodeToString(operator.PrivateKey))) || bytes.Contains(raw, []byte(operator.Grant.Grant.Principal)) {
			t.Fatal("operator credential copied into image", rel)
		}
		info, _ := d.Info()
		want := os.FileMode(0600)
		if strings.HasSuffix(rel, "/worldd") || strings.HasSuffix(rel, "/world-go2-executor") {
			want = 0755
		}
		if info.Mode().Perm() != want {
			t.Fatal("file permissions", rel, info.Mode())
		}
		if m.Files[rel] != digest(raw) {
			t.Fatal("manifest hash mismatch", rel)
		}
		return nil
	})
	if e != nil {
		t.Fatal(e)
	}
	expected := []string{}
	for f := range m.Files {
		expected = append(expected, f)
	}
	sort.Strings(files)
	sort.Strings(expected)
	if !reflect.DeepEqual(files, expected) || len(files) != 22+len(cloudSourceFiles()) {
		t.Fatal("unexpected context inventory", files)
	}
	ignore, _ := os.ReadFile(filepath.Join(o.Out, "project/.dockerignore"))
	if !bytes.HasPrefix(ignore, []byte("**\n")) || bytes.Contains(ignore, []byte("clients")) {
		t.Fatal("non-allowlisted Docker context")
	}
	docker, _ := os.ReadFile(filepath.Join(o.Out, "project/Dockerfile"))
	if bytes.Contains(docker, []byte("COPY . ")) || !bytes.Contains(docker, []byte("cmake -S sim/go2_bridge/native")) || !bytes.Contains(docker, []byte("--target world-go2-cloud-live")) {
		t.Fatal("Docker context/build")
	}
	for _, name := range []string{"alignment", "profile", "binding"} {
		original, _ := os.ReadFile(map[string]string{"alignment": o.Alignment, "profile": o.Profile, "binding": o.Binding}[name])
		copy, _ := os.ReadFile(filepath.Join(o.Out, "project", name+".json"))
		if !bytes.Equal(original, copy) {
			t.Fatal("commissioned bytes rewritten", name)
		}
	}
}
func TestStageRefusesUncommissionedInputsBeforeBuild(t *testing.T) {
	cases := map[string]func(*options){"physical transport": func(o *options) { o.Transport = "mesh" }, "no http consent": func(o *options) { o.Insecure = false }, "no commissioning": func(o *options) { o.Commission = false }, "amd64 unpinned": func(o *options) { o.Arch = "amd64" }, "runtime port": func(o *options) { o.Port = 8890 }, "same ports": func(o *options) { o.Port = o.BridgePort }, "task injection": func(o *options) { o.TaskID = "../task" }, "blank site": func(o *options) { o.Site = "" }, "changed profile bytes": func(o *options) {
		f, _ := os.OpenFile(o.Profile, os.O_APPEND|os.O_WRONLY, 0600)
		_, _ = f.Write([]byte(" "))
		_ = f.Close()
	}, "symlink": func(o *options) { _ = os.Symlink(o.Binding, o.Binding+".link"); o.Binding += ".link" }, "fifo": func(o *options) {
		o.Binding += ".fifo"
		if e := syscall.Mkfifo(o.Binding, 0600); e != nil {
			t.Fatal(e)
		}
	}}
	for name, change := range cases {
		t.Run(name, func(t *testing.T) {
			o := testOptions(t)
			change(&o)
			_, e := stage(o, func(string, string, string, string) error { t.Fatal("unexpected build"); return nil })
			if e == nil {
				t.Fatal("accepted")
			}
			if _, e = os.Stat(o.Out); !errors.Is(e, os.ErrNotExist) {
				t.Fatal("created output before validating")
			}
		})
	}
}
func TestStageBuildFailureCleansOnlyNewOutputAndNoIssuance(t *testing.T) {
	o := testOptions(t)
	calls := 0
	_, e := stage(o, func(string, string, string, string) error { calls++; return errors.New("build unavailable") })
	if e == nil || calls != 1 {
		t.Fatal("build failed incorrectly")
	}
	if _, e = os.Stat(o.Out); !errors.Is(e, os.ErrNotExist) {
		t.Fatal("partial context remains")
	}
	if _, e = os.Stat(o.Binding); e != nil {
		t.Fatal("removed input")
	}
	if e = os.Mkdir(o.Out, 0700); e != nil {
		t.Fatal(e)
	}
	sentinel := filepath.Join(o.Out, "operator-owned")
	_ = writeNew(sentinel, []byte("keep"), 0600)
	if _, e = stage(o, fakeBuilder); e == nil {
		t.Fatal("reused output")
	}
	if _, e = os.Stat(sentinel); e != nil {
		t.Fatal("removed existing directory")
	}
}
func TestRepeatedStageGetsDifferentImageAuthorityAndVolume(t *testing.T) {
	o := testOptions(t)
	a, e := stage(o, fakeBuilder)
	if e != nil {
		t.Fatal(e)
	}
	o.Out += "-second"
	b, e := stage(o, fakeBuilder)
	if e != nil {
		t.Fatal(e)
	}
	if a.RunID != b.RunID || a.Volume == b.Volume || a.AppID == b.AppID || a.Audience == b.Audience {
		t.Fatal("unisolated stages")
	}
}
func TestBootContract(t *testing.T) {
	c := exec.Command("python3", "-B", "boot_test.py")
	if out, e := c.CombinedOutput(); e != nil {
		t.Fatalf("boot tests: %v\n%s", e, out)
	}
}
