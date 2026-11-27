package agentmodule

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/world/internal/world"
	appv1 "github.com/wendylabsinc/wendy/go/world/production/api/world/app/v1"
	"github.com/wendylabsinc/wendy/go/world/production/appservice"
	"github.com/wendylabsinc/wendy/go/world/production/internal/testidentity"
	"github.com/wendylabsinc/wendy/go/world/production/serviceorigin"
	"github.com/wendylabsinc/wendy/go/world/production/trustedtime"
	bolt "go.etcd.io/bbolt"
	"google.golang.org/grpc"
)

var moduleTestNow = time.Date(2026, 11, 11, 12, 0, 0, 0, time.UTC)

type moduleClock struct {
	mu     sync.Mutex
	window trustedtime.Interval
	err    error
}

func (c *moduleClock) Now() (trustedtime.Interval, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.window, c.err
}
func moduleTestClock() *moduleClock {
	return &moduleClock{window: trustedtime.Interval{Earliest: moduleTestNow, Latest: moduleTestNow.Add(time.Millisecond), BootID: "agent-test-boot", Monotonic: time.Second, Domain: "physical", Authenticated: true, Source: "agent-test-authenticated-anchor"}}
}

// Current deliberately does not take mu: verification may call it while the
// provisioning commit fence holds mu. Snapshot and invalidation serialize with
// that fence; this matches the host Identity contract.
type moduleIdentity struct {
	mu          sync.Mutex
	token       atomic.Uint64
	snapshot    IdentitySnapshot
	invalidated chan struct{}
}

func newModuleIdentity(f *testidentity.Fixture) *moduleIdentity {
	invalidated := make(chan struct{})
	i := &moduleIdentity{invalidated: invalidated, snapshot: IdentitySnapshot{Token: 1, ChainDER: f.ChainDER, Key: f.Key, Invalidated: invalidated}}
	i.token.Store(1)
	return i
}
func (i *moduleIdentity) Snapshot(ctx context.Context) (IdentitySnapshot, error) {
	i.mu.Lock()
	defer i.mu.Unlock()
	if ctx.Err() != nil || i.token.Load() == 0 {
		return IdentitySnapshot{}, ErrDenied
	}
	return i.snapshot, nil
}
func (i *moduleIdentity) Current(token uint64) bool { return token != 0 && token == i.token.Load() }
func (i *moduleIdentity) WithCurrent(token uint64, fn func() error) error {
	i.mu.Lock()
	defer i.mu.Unlock()
	if !i.Current(token) {
		return ErrDenied
	}
	return fn()
}
func (i *moduleIdentity) invalidate() {
	i.mu.Lock()
	defer i.mu.Unlock()
	if i.token.Swap(0) != 0 {
		close(i.invalidated)
	}
}

func (i *moduleIdentity) rotate() {
	i.mu.Lock()
	defer i.mu.Unlock()
	close(i.invalidated)
	i.invalidated = make(chan struct{})
	i.snapshot.Token++
	i.snapshot.Invalidated = i.invalidated
	i.token.Store(i.snapshot.Token)
}

type moduleFixture struct {
	manager      *Manager
	config       Config
	deps         Dependencies
	identity     *moduleIdentity
	clock        *moduleClock
	policyOK     atomic.Bool
	peerMu       sync.Mutex
	peerServices map[int32]string
}

func basePolicy() Policy {
	return Policy{Version: 1, Tenant: testidentity.Tenant, Site: "office", Device: "spiffe://wendy.sh/tenant/" + testidentity.Tenant + "/device/22222222-2222-4222-8222-222222222222", ExecutionDomain: "physical", SampleClockDomain: "utc", Frames: []world.FrameRef{{ID: "floor", Revision: "survey-1"}}, NotBefore: moduleTestNow.Add(-time.Minute), NotAfter: moduleTestNow.Add(15 * time.Minute), Grants: []Grant{{App: "com.example.sensors", Service: "telemetry-a", Publish: []serviceorigin.PublishScope{{ComponentType: "wendy.sensor.scalar.v1", Stream: "gas"}, {ComponentType: "wendy.sensor.image.v1", Stream: "front"}, {ComponentType: "wendy.sensor.audio.v1", Stream: "ambient"}}}, {App: "com.example.sensors", Service: "telemetry-b", Publish: []serviceorigin.PublishScope{{ComponentType: "wendy.sensor.scalar.v1", Stream: "gas-b"}}}}}
}

func setupModule(t *testing.T, changes ...func(*Config)) *moduleFixture {
	t.Helper()
	return setupModuleWithDependencies(t, nil, changes...)
}

func setupModuleWithDependencies(t *testing.T, changeDependencies func(*Dependencies), changes ...func(*Config)) *moduleFixture {
	t.Helper()
	if runtime.GOOS != "linux" {
		t.Skip("positive host integration requires Linux 6.5+ SO_PEERPIDFD; no injected identity fallback")
	}
	policy := basePolicy()
	cert := testidentity.NewWith(t, moduleTestNow, policy.Device)
	parent, err := filepath.EvalSymlinks(os.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	dir, err := os.MkdirTemp(parent, "ww-agent-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	f := &moduleFixture{identity: newModuleIdentity(cert), clock: moduleTestClock(), peerServices: map[int32]string{int32(os.Getpid()): "*test-services*"}}
	f.policyOK.Store(true)
	f.config = Config{Policy: policy, Roots: [][]byte{cert.RootDER}, Directory: filepath.Join(dir, "sockets"), JournalPath: filepath.Join(dir, "journal", "world.db"), CreateJournal: true, UID: os.Geteuid(), GID: os.Getegid(), Limits: appservice.Limits{AuthorityCheckInterval: 10 * time.Millisecond}}
	for _, change := range changes {
		change(&f.config)
	}
	f.deps = Dependencies{Identity: f.identity, Clock: f.clock, SampleNow: func() time.Time { return moduleTestNow.Add(100 * time.Millisecond) }, PolicyCurrent: func(context.Context) error {
		if !f.policyOK.Load() {
			return ErrDenied
		}
		return nil
	}, PeerCheck: func(_ context.Context, peer appservice.Peer, scope serviceorigin.Scope) error {
		f.peerMu.Lock()
		defer f.peerMu.Unlock()
		allowed := f.peerServices[peer.PID]
		if allowed != scope.Service && allowed != "*test-services*" {
			return ErrDenied
		}
		return nil
	}}
	if changeDependencies != nil {
		changeDependencies(&f.deps)
	}
	f.manager, err = New(context.Background(), f.config, f.deps)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = f.manager.Close() })
	return f
}

func (f *moduleFixture) mount(t *testing.T, index int, generation uint64) appservice.Mount {
	t.Helper()
	g := f.config.Policy.Grants[index]
	m, err := f.manager.Ensure(context.Background(), g.App, g.Service, generation, g.Publish)
	if err != nil {
		t.Fatal(err)
	}
	return m
}
func moduleDial(t *testing.T, path string) appv1.WorldAppServiceClient {
	t.Helper()
	conn, err := grpc.NewClient("passthrough:///agent-world", grpc.WithTransportCredentials(appservice.ClientCredentials()), grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", path)
	}))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return appv1.NewWorldAppServiceClient(conn)
}

func moduleRequest(kind string) *appv1.PublishRequest {
	request := &appv1.PublishRequest{Entity: kind + "-sample", Frame: &appv1.FrameRef{Id: "floor", Revision: "survey-1"}, Timing: &appv1.Timing{SampleUnixNs: moduleTestNow.UnixNano(), ClockDomain: "utc", ErrorKnown: true}, TtlNs: uint64(30 * time.Second), Provenance: &appv1.Provenance{Origin: appv1.Origin_ORIGIN_MEASURED, AppVersion: "sensor-app-v1"}}
	switch kind {
	case "gas":
		request.ComponentType = "wendy.sensor.scalar.v1"
		request.Stream = "gas"
		request.PayloadJson = []byte("{ \"value\" : 12.5, \"unit\":\"ppm\", \"sensor_type\":\"gas\", \"calibration_revision\":\"gas-cal-1\", \"uncertainty\":0.5 }\n")
	case "image":
		request.ComponentType = "wendy.sensor.image.v1"
		request.Stream = "front"
		request.PayloadJson = []byte(`{"sha256":"` + strings.Repeat("a", 64) + `","content_type":"image/jpeg","duration_ns":0,"calibration_revision":"camera-cal-1"}`)
	case "audio":
		request.ComponentType = "wendy.sensor.audio.v1"
		request.Stream = "ambient"
		request.PayloadJson = []byte(`{"sha256":"` + strings.Repeat("b", 64) + `","content_type":"audio/wav","duration_ns":1000000000,"calibration_revision":"microphone-cal-1"}`)
	}
	return request
}
func modulePublish(t *testing.T, client appv1.WorldAppServiceClient, request *appv1.PublishRequest) *appv1.PublishReply {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	reply, err := client.Publish(ctx, request)
	if err != nil || reply == nil || !reply.Accepted {
		t.Fatal("publication not durably accepted", err)
	}
	return reply
}
func decodeReply(t *testing.T, reply *appv1.PublishReply) (serviceorigin.SignedRecord, serviceorigin.Record) {
	t.Helper()
	var signed serviceorigin.SignedRecord
	var record serviceorigin.Record
	if json.Unmarshal(reply.SignedRecordJson, &signed) != nil || json.Unmarshal(signed.Record, &record) != nil {
		t.Fatal("invalid signed reply")
	}
	return signed, record
}

func TestLinuxHostPersistsGasImageAudioOriginalProofs(t *testing.T) {
	f := setupModule(t, func(c *Config) { c.EvidenceDirectory = filepath.Join(filepath.Dir(c.JournalPath), "media") })
	mount := f.mount(t, 0, 37)
	client := moduleDial(t, mount.SocketPath)
	wanted := map[string][]byte{}
	for _, kind := range []string{"gas", "image", "audio"} {
		req := moduleRequest(kind)
		if kind != "gas" {
			body := []byte("stored test " + kind + " bytes")
			h := sha256.Sum256(body)
			var media mediaPayload
			if json.Unmarshal(req.PayloadJson, &media) != nil {
				t.Fatal("media fixture")
			}
			media.SHA256 = hex.EncodeToString(h[:])
			media.Size = int64(len(body))
			req.PayloadJson, _ = json.Marshal(media)
			if _, err := client.Publish(context.Background(), req); err == nil {
				t.Fatal("media accepted before complete upload")
			}
			put, err := client.PutEvidence(context.Background(), &appv1.PutEvidenceRequest{ComponentType: req.ComponentType, Stream: req.Stream, Sha256: media.SHA256, Size: media.Size, Data: body, ContentType: media.ContentType})
			if err != nil || !put.Complete {
				t.Fatal("media upload", err)
			}
		}
		reply := modulePublish(t, client, req)
		signed, record := decodeReply(t, reply)
		if record.Scope.Service != "telemetry-a" || record.Scope.App != f.config.Policy.Grants[0].App || record.Scope.Device != f.config.Policy.Device || record.Scope.Site != f.config.Policy.Site || record.Scope.DeploymentGeneration != 37 || record.Publication.Timing.SampleTime.UnixNano() != req.Timing.SampleUnixNs || !bytes.Equal(record.Publication.Payload, req.PayloadJson) {
			t.Fatal("caller data or commissioned source changed")
		}
		wanted[string(signed.Record)] = append([]byte(nil), reply.SignedRecordJson...)
	}
	snapshot, err := f.manager.Snapshot(context.Background())
	if err != nil || len(snapshot) != 3 {
		t.Fatal("accepted records missing from actual replica", len(snapshot), err)
	}
	for _, proof := range snapshot {
		raw, _ := json.Marshal(proof)
		if !bytes.Equal(raw, wanted[string(proof.Record)]) {
			t.Fatal("snapshot reconstructed or changed original proof")
		}
	}
	if err = f.manager.Close(); err != nil {
		t.Fatal(err)
	}
	db, err := bolt.Open(f.config.JournalPath, 0600, &bolt.Options{ReadOnly: true, Timeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	count := 0
	err = db.View(func(tx *bolt.Tx) error {
		return tx.Bucket([]byte("proofs")).ForEach(func(_, value []byte) error {
			var stored struct {
				Envelope []byte `json:"envelope"`
			}
			if json.Unmarshal(value, &stored) != nil {
				return errors.New("bad persisted envelope")
			}
			var proof serviceorigin.SignedRecord
			if json.Unmarshal(stored.Envelope, &proof) != nil || !bytes.Equal(stored.Envelope, wanted[string(proof.Record)]) {
				return errors.New("persisted original proof differs from acknowledgement")
			}
			count++
			return nil
		})
	})
	if err != nil || count != 3 {
		t.Fatal("durable journal proof mismatch", count, err)
	}
}

func TestLinuxHostRefusesScopeAndSchemaSpoofing(t *testing.T) {
	f := setupModule(t)
	mount := f.mount(t, 0, 1)
	client := moduleDial(t, mount.SocketPath)
	for name, change := range map[string]func(*appv1.PublishRequest){"foreign-stream": func(r *appv1.PublishRequest) { r.Stream = "gas-b" }, "unknown-schema": func(r *appv1.PublishRequest) { r.ComponentType = "wendy.secret.v1" }, "wrong-frame-revision": func(r *appv1.PublishRequest) { r.Frame.Revision = "invented" }, "wrong-clock": func(r *appv1.PublishRequest) { r.Timing.ClockDomain = "other-clock" }, "unknown-clock-error": func(r *appv1.PublishRequest) { r.Timing.ErrorKnown = false }, "source-in-payload": func(r *appv1.PublishRequest) {
		r.PayloadJson = []byte(`{"source":"another-service","value":1,"unit":"ppm","sensor_type":"gas","calibration_revision":"c","uncertainty":0}`)
	}, "wrong-modality": func(r *appv1.PublishRequest) { r.PayloadJson = moduleRequest("image").PayloadJson }, "synthetic-as-measured": func(r *appv1.PublishRequest) {
		r.Provenance.Origin = appv1.Origin_ORIGIN_SIMULATED
		r.Provenance.Simulation = &appv1.SimulationContext{RunId: "r", ScenarioVersion: "v", ClockDomain: "simulation:r"}
	}} {
		t.Run(name, func(t *testing.T) {
			r := moduleRequest("gas")
			change(r)
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			if reply, err := client.Publish(ctx, r); err == nil || reply != nil {
				t.Fatal("spoofed publication accepted")
			}
		})
	}
	if snapshot, err := f.manager.Snapshot(context.Background()); err != nil || len(snapshot) != 0 {
		t.Fatal("refused record persisted", len(snapshot), err)
	}
	g := f.config.Policy.Grants[0]
	if _, err := f.manager.Ensure(context.Background(), g.App, "telemetry-b", 1, g.Publish); err == nil {
		t.Fatal("another service inherited grants")
	}
}

func TestLinuxHostInvalidationClosesActiveStream(t *testing.T) {
	for _, kind := range []string{"policy", "identity"} {
		t.Run(kind, func(t *testing.T) {
			f := setupModule(t)
			client := moduleDial(t, f.mount(t, 0, 1).SocketPath)
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			stream, err := client.PublishStream(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if err = stream.Send(moduleRequest("gas")); err != nil {
				t.Fatal(err)
			}
			if reply, err := stream.Recv(); err != nil || !reply.Accepted {
				t.Fatal(err)
			}
			if kind == "policy" {
				f.policyOK.Store(false)
			} else {
				f.identity.invalidate()
			}
			started := time.Now()
			if _, err = stream.Recv(); err == nil || time.Since(started) > time.Second {
				t.Fatal("active stream retained invalid authority", err)
			}
			if snapshots, err := f.manager.Snapshot(context.Background()); err == nil && len(snapshots) != 0 {
				t.Fatal("revoked records still returned as authorized")
			}
		})
	}
}

func TestLinuxHostReplacementStaleReleaseAndRestartEpoch(t *testing.T) {
	f := setupModule(t)
	grant := f.config.Policy.Grants[0]
	old := f.mount(t, 0, 37)
	if same := f.mount(t, 0, 37); same != old {
		t.Fatal("unchanged scope replaced")
	}
	oldClient := moduleDial(t, old.SocketPath)
	_, first := decodeReply(t, modulePublish(t, oldClient, moduleRequest("gas")))
	replacement := f.mount(t, 0, 19) // Deployment tokens are identities, not ordered counters.
	if replacement.Directory == old.Directory {
		t.Fatal("replacement inherited old mount")
	}
	f.manager.Release(grant.App, grant.Service, 37)
	if err := f.manager.ValidateMount(context.Background(), grant.App, grant.Service, 19, replacement.Directory, grant.Publish); err != nil {
		t.Fatal("stale release revoked replacement", err)
	}
	if err := f.manager.ValidateMount(context.Background(), grant.App, grant.Service, 37, old.Directory, grant.Publish); err == nil {
		t.Fatal("old mount remained authorized")
	}
	current := moduleDial(t, replacement.SocketPath)
	_, second := decodeReply(t, modulePublish(t, current, moduleRequest("gas")))
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if _, err := oldClient.Publish(ctx, moduleRequest("gas")); err == nil {
		t.Fatal("old mounted connection retained rights")
	}
	if second.Epoch <= first.Epoch {
		t.Fatal("replacement epoch did not advance")
	}
	if err := f.manager.Close(); err != nil {
		t.Fatal(err)
	}
	f.config.CreateJournal = false
	var err error
	f.manager, err = New(context.Background(), f.config, f.deps)
	if err != nil {
		t.Fatal(err)
	}
	restarted := f.mount(t, 0, 19)
	if restarted.Directory == replacement.Directory {
		t.Fatal("agent restart reused stale mount")
	}
	_, third := decodeReply(t, modulePublish(t, moduleDial(t, restarted.SocketPath), moduleRequest("gas")))
	if third.Epoch <= second.Epoch || third.Sequence != 1 {
		t.Fatal("durable restart epoch not reserved", second.Epoch, third.Epoch, third.Sequence)
	}
}

func TestLinuxHostKernelPeerDeniesSameAppOtherService(t *testing.T) {
	f := setupModule(t)
	a := f.mount(t, 0, 1)
	b := f.mount(t, 1, 1)
	run := func(path, stream string) string {
		t.Helper()
		exe, err := os.Executable()
		if err != nil {
			t.Fatal(err)
		}
		cmd := exec.Command(exe, "-test.run=^TestAgentModuleChildClient$")
		cmd.Env = append(os.Environ(), "WORLD_AGENT_TEST_SOCKET="+path, "WORLD_AGENT_TEST_STREAM="+stream)
		stdin, err := cmd.StdinPipe()
		if err != nil {
			t.Fatal(err)
		}
		var output bytes.Buffer
		cmd.Stdout = &output
		cmd.Stderr = &output
		if err = cmd.Start(); err != nil {
			t.Fatal(err)
		}
		f.peerMu.Lock()
		f.peerServices[int32(cmd.Process.Pid)] = "telemetry-a"
		f.peerMu.Unlock()
		_ = stdin.Close()
		if err = cmd.Wait(); err != nil {
			t.Fatalf("helper failed %v %s", err, output.String())
		}
		f.peerMu.Lock()
		delete(f.peerServices, int32(cmd.Process.Pid))
		f.peerMu.Unlock()
		return strings.TrimSpace(output.String())
	}
	if got := run(a.SocketPath, "gas"); got != "accepted" {
		t.Fatal("commissioned service peer denied", got)
	}
	if got := run(b.SocketPath, "gas-b"); got != "denied" {
		t.Fatal("same-app other-service peer accepted", got)
	}
	snapshot, err := f.manager.Snapshot(context.Background())
	if err != nil || len(snapshot) != 1 {
		t.Fatal("peer refusal did not protect sink", len(snapshot), err)
	}
}

func TestAgentModuleChildClient(t *testing.T) {
	path := os.Getenv("WORLD_AGENT_TEST_SOCKET")
	if path == "" {
		return
	}
	_, _ = io.ReadAll(os.Stdin)
	client := moduleDial(t, path)
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	request := moduleRequest("gas")
	request.Stream = os.Getenv("WORLD_AGENT_TEST_STREAM")
	reply, err := client.Publish(ctx, request)
	if err == nil && reply.Accepted {
		_, _ = os.Stdout.WriteString("accepted\n")
	} else {
		_, _ = os.Stdout.WriteString("denied\n")
	}
	os.Exit(0)
}

func TestHostSchemaValidationPreservesPayloadAndRejectsExtensions(t *testing.T) {
	policy := basePolicy()
	m := &Manager{cfg: Config{Policy: policy}, deps: Dependencies{Clock: moduleTestClock(), PolicyCurrent: func(context.Context) error { return nil }}}
	base := moduleRequest("gas")
	publication, err := appv1.DecodePublish(base)
	if err != nil {
		t.Fatal(err)
	}
	verified := serviceorigin.VerifiedRecord{Record: serviceorigin.Record{Scope: policy.scope(policy.Grants[0], 1), Publication: publication}}
	if err = m.admitSchema(context.Background(), verified); err != nil {
		t.Fatal("valid gas schema rejected", err)
	}
	changed := verified
	changed.Record.Publication.Payload = []byte(`{"value":1,"unit":"ppm","sensor_type":"gas","calibration_revision":"x","uncertainty":0,"appearance":"forbidden-extension"}`)
	if err = m.admitSchema(context.Background(), changed); err == nil {
		t.Fatal("schema extension not refused")
	}
	if !reflect.DeepEqual(publication.Payload, verified.Record.Publication.Payload) {
		t.Fatal("schema validation changed original bytes")
	}
}

func TestLinuxHostSameScopeRotationUpdatesMountedDirectory(t *testing.T) {
	f := setupModule(t)
	old := f.mount(t, 0, 37)
	oldClient := moduleDial(t, old.SocketPath)
	_, before := decodeReply(t, modulePublish(t, oldClient, moduleRequest("gas")))
	f.identity.rotate()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	ticker := time.NewTicker(time.Millisecond)
	defer ticker.Stop()
	for {
		if _, err := os.Stat(old.SocketPath); errors.Is(err, os.ErrNotExist) {
			break
		}
		select {
		case <-ticker.C:
		case <-ctx.Done():
			t.Fatal("old identity did not close its mounted socket")
		}
	}
	replacement := f.mount(t, 0, 37)
	if replacement.Directory == old.Directory {
		t.Fatal("identity rotation reused old mount")
	}
	grant := f.config.Policy.Grants[0]
	if err := f.manager.ValidateMount(ctx, grant.App, grant.Service, 37, replacement.Directory, grant.Publish); err != nil {
		t.Fatal("Ensure returned an unregistered replacement mount", err)
	}
	if err := f.manager.ValidateMount(ctx, grant.App, grant.Service, 37, old.Directory, grant.Publish); err == nil {
		t.Fatal("old identity mount remained valid")
	}
	_, after := decodeReply(t, modulePublish(t, moduleDial(t, replacement.SocketPath), moduleRequest("gas")))
	if after.Epoch <= before.Epoch {
		t.Fatal("rotation did not reserve a new publisher epoch")
	}
}

func TestLinuxHostSnapshotConfigFailureCannotBeRestoredInPlace(t *testing.T) {
	f := setupModule(t)
	mount := f.mount(t, 0, 1)
	modulePublish(t, moduleDial(t, mount.SocketPath), moduleRequest("gas"))
	f.policyOK.Store(false)
	if snapshot, err := f.manager.Snapshot(context.Background()); err == nil && len(snapshot) != 0 {
		t.Fatal("snapshot exposed records after observed configuration failure")
	}
	f.policyOK.Store(true)
	if snapshot, err := f.manager.Snapshot(context.Background()); err == nil && len(snapshot) != 0 {
		t.Fatal("restored configuration revived current records without process restart")
	}
	grant := f.config.Policy.Grants[0]
	if _, err := f.manager.Ensure(context.Background(), grant.App, grant.Service, 2, grant.Publish); err == nil {
		t.Fatal("restored configuration revived socket authority without process restart")
	}
}

func TestLinuxHostOwnsCommissionedPolicySlices(t *testing.T) {
	for _, field := range []string{"grant", "publish", "frame", "denied-key"} {
		t.Run(field, func(t *testing.T) {
			f := setupModule(t, func(cfg *Config) { cfg.Policy.DeniedKeys = []string{strings.Repeat("d", 64)} })
			commissioned := clonePolicy(t, f.config.Policy)
			switch field {
			case "grant":
				f.config.Policy.Grants[0].App = "com.replaced.by.caller"
			case "publish":
				f.config.Policy.Grants[0].Publish[0].Stream = "caller-replaced-stream"
			case "frame":
				f.config.Policy.Frames[0].Revision = "caller-replaced-revision"
			case "denied-key":
				leaf, err := x509.ParseCertificate(f.identity.snapshot.ChainDER[0])
				if err != nil {
					t.Fatal(err)
				}
				digest := sha256.Sum256(leaf.RawSubjectPublicKeyInfo)
				f.config.Policy.DeniedKeys[0] = hex.EncodeToString(digest[:])
			}
			grant := commissioned.Grants[0]
			mount, err := f.manager.Ensure(context.Background(), grant.App, grant.Service, 37, grant.Publish)
			if err != nil {
				t.Fatal("caller mutation changed live commissioned authority", err)
			}
			modulePublish(t, moduleDial(t, mount.SocketPath), moduleRequest("gas"))
			if snapshot, err := f.manager.Snapshot(context.Background()); err != nil || len(snapshot) != 1 {
				t.Fatal("caller slice mutation changed schema/frame/deny policy", err)
			}
		})
	}
}
