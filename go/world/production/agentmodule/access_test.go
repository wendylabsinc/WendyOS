package agentmodule

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"image"
	"image/jpeg"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	appv1 "github.com/wendylabsinc/wendy/go/world/production/api/world/app/v1"
	"github.com/wendylabsinc/wendy/go/world/production/appservice"
	"github.com/wendylabsinc/wendy/go/world/production/serviceaccess"
	"github.com/wendylabsinc/wendy/go/world/production/serviceorigin"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

func accessSource(service, component, stream string) serviceaccess.Source {
	return serviceaccess.Source{App: "com.example.sensors", Service: service, ComponentType: component, Stream: stream}
}
func accessSelector(s serviceaccess.Source) *appv1.SourceSelector {
	return &appv1.SourceSelector{App: s.App, Service: s.Service, ComponentType: s.ComponentType, Stream: s.Stream, Device: s.Device}
}
func accessConfig(cfg *Config) {
	a := accessSource("telemetry-a", "wendy.sensor.scalar.v1", "gas")
	b := accessSource("telemetry-b", "wendy.sensor.scalar.v1", "gas-b")
	image := accessSource("telemetry-a", "wendy.sensor.image.v1", "front")
	otherImage := accessSource("telemetry-b", image.ComponentType, image.Stream)
	cfg.Policy.Grants = append(cfg.Policy.Grants, Grant{App: "com.example.fusion", Service: "reader", Subscribe: []serviceaccess.Source{a, b, image, otherImage}})
	cfg.EvidenceDirectory = filepath.Join(filepath.Dir(cfg.JournalPath), "evidence")
}
func accessMount(t *testing.T, f *moduleFixture, sources ...serviceaccess.Source) appservice.Mount {
	t.Helper()
	m, err := f.manager.EnsureAccess(context.Background(), "com.example.fusion", "reader", 7, nil, sources)
	if err != nil {
		t.Fatal(err)
	}
	return m
}
func accessQuery(t *testing.T, c appv1.WorldAppServiceClient, sources []serviceaccess.Source, limit uint32, after string) *appv1.QueryReply {
	t.Helper()
	q := &appv1.QueryRequest{Limit: limit, After: after}
	for _, s := range sources {
		q.Sources = append(q.Sources, accessSelector(s))
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	out, err := c.Query(ctx, q)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// Linux uses the real Unix peer credentials and original-process pidfd. The
// fixture commissioning callback deliberately recognizes this test process.
func TestAccessReadOnlyQueriesOriginalBoundedProofs(t *testing.T) {
	f := setupModule(t, accessConfig)
	a := accessSource("telemetry-a", "wendy.sensor.scalar.v1", "gas")
	b := accessSource("telemetry-b", "wendy.sensor.scalar.v1", "gas-b")
	writer := moduleDial(t, f.mount(t, 0, 11).SocketPath)
	other := moduleDial(t, f.mount(t, 1, 12).SocketPath)
	reader := moduleDial(t, accessMount(t, f, a).SocketPath)
	want := map[string][]byte{}
	for i := 0; i < 5; i++ {
		q := moduleRequest("gas")
		q.Entity = fmt.Sprintf("gas-%02d", 4-i)
		r := modulePublish(t, writer, q)
		want[q.Entity] = r.SignedRecordJson
	}
	q := moduleRequest("gas")
	q.Stream = "gas-b"
	modulePublish(t, other, q)
	seen := map[string]bool{}
	cursor := ""
	for page := 0; page < 3; page++ {
		out := accessQuery(t, reader, []serviceaccess.Source{a}, 2, cursor)
		if len(out.SignedRecordJson) == 0 || len(out.SignedRecordJson) > 2 {
			t.Fatal("query did not obey its record bound")
		}
		for _, raw := range out.SignedRecordJson {
			var signed serviceorigin.SignedRecord
			var record serviceorigin.Record
			if json.Unmarshal(raw, &signed) != nil || json.Unmarshal(signed.Record, &record) != nil {
				t.Fatal("invalid original proof")
			}
			id := string(record.Publication.Entity)
			if seen[id] || !bytes.Equal(raw, want[id]) {
				t.Fatal("query duplicated, replaced or exposed an unrequested original proof")
			}
			seen[id] = true
		}
		if page < 2 && (out.Next == "" || out.Next == cursor) || page == 2 && out.Next != "" {
			t.Fatal("pagination did not terminate at the exact admitted set")
		}
		cursor = out.Next
	}
	if len(seen) != len(want) {
		t.Fatal("query omitted admitted records")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	for _, c := range []appv1.WorldAppServiceClient{writer, reader} {
		if out, err := c.Query(ctx, &appv1.QueryRequest{Sources: []*appv1.SourceSelector{accessSelector(b)}, Limit: 1}); err == nil || out != nil {
			t.Fatal("publication permission or undeployed commissioned subscription granted a read")
		}
	}
	if out, err := reader.Publish(ctx, moduleRequest("gas")); status.Code(err) != codes.PermissionDenied || out != nil {
		t.Fatal("empty publication scope could publish", err)
	}
	if out, err := reader.Query(ctx, &appv1.QueryRequest{Sources: []*appv1.SourceSelector{accessSelector(a)}, Limit: 9}); status.Code(err) != codes.InvalidArgument || out != nil {
		t.Fatal("unbounded query accepted", err)
	}
	for _, bad := range []serviceaccess.Source{accessSource("telemetry-c", a.ComponentType, a.Stream), accessSource(a.Service, a.ComponentType, "*")} {
		if _, err := f.manager.EnsureAccess(ctx, "com.example.fusion", "reader", 7, nil, []serviceaccess.Source{bad}); err == nil {
			t.Fatal("uncommissioned or wildcard subscription mounted")
		}
	}
	if _, err := f.manager.EnsureAccess(ctx, "com.example.fusion", "reader", 7, nil, nil); err == nil {
		t.Fatal("service without read or write grants mounted")
	}
}

func TestAccessSubscriptionChangeRevokesSameGenerationSocket(t *testing.T) {
	f := setupModule(t, accessConfig)
	a := accessSource("telemetry-a", "wendy.sensor.scalar.v1", "gas")
	b := accessSource("telemetry-b", "wendy.sensor.scalar.v1", "gas-b")
	modulePublish(t, moduleDial(t, f.mount(t, 0, 1).SocketPath), moduleRequest("gas"))
	q := moduleRequest("gas")
	q.Stream = "gas-b"
	modulePublish(t, moduleDial(t, f.mount(t, 1, 1).SocketPath), q)
	old := accessMount(t, f, a)
	oldClient := moduleDial(t, old.SocketPath)
	if len(accessQuery(t, oldClient, []serviceaccess.Source{a}, 1, "").SignedRecordJson) != 1 {
		t.Fatal("initial reader did not read")
	}
	current := accessMount(t, f, b)
	if current.Directory == old.Directory || accessMount(t, f, b) != current {
		t.Fatal("changed grants reused a socket, or unchanged grants replaced it")
	}
	if err := f.manager.ValidateAccessMount(context.Background(), "com.example.fusion", "reader", 7, old.Directory, nil, []serviceaccess.Source{a}); err == nil {
		t.Fatal("stale read mount remained current")
	}
	client := moduleDial(t, current.SocketPath)
	if len(accessQuery(t, client, []serviceaccess.Source{b}, 1, "").SignedRecordJson) != 1 {
		t.Fatal("replacement reader could not read commissioned data")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if out, err := oldClient.Query(ctx, &appv1.QueryRequest{Sources: []*appv1.SourceSelector{accessSelector(a)}, Limit: 1}); err == nil || out != nil {
		t.Fatal("old live connection survived subscription replacement")
	}
	if out, err := client.Query(ctx, &appv1.QueryRequest{Sources: []*appv1.SourceSelector{accessSelector(a)}, Limit: 1}); err == nil || out != nil {
		t.Fatal("replacement retained removed subscription")
	}
	f.identity.invalidate()
	if out, err := client.Query(ctx, &appv1.QueryRequest{Sources: []*appv1.SourceSelector{accessSelector(b)}, Limit: 1}); err == nil || out != nil {
		t.Fatal("identity invalidation retained read access")
	}
}

func TestAccessEvidenceOwnershipExpiryAndRestart(t *testing.T) {
	f := setupModule(t, accessConfig)
	if err := f.manager.Close(); err != nil {
		t.Fatal(err)
	}
	var sampleNS atomic.Int64
	sampleNS.Store(moduleTestNow.Add(100 * time.Millisecond).UnixNano())
	f.deps.SampleNow = func() time.Time { return time.Unix(0, sampleNS.Load()) }
	f.config.CreateJournal = false
	var err error
	f.manager, err = New(context.Background(), f.config, f.deps)
	if err != nil {
		t.Fatal(err)
	}
	source := accessSource("telemetry-a", "wendy.sensor.image.v1", "front")
	otherSource := accessSource("telemetry-b", source.ComponentType, source.Stream)
	writer := moduleDial(t, f.mount(t, 0, 1).SocketPath)
	oldReader := accessMount(t, f, source, otherSource)
	reader := moduleDial(t, oldReader.SocketPath)
	var encoded bytes.Buffer
	if err := jpeg.Encode(&encoded, image.NewRGBA(image.Rect(0, 0, 2, 2)), nil); err != nil {
		t.Fatal(err)
	}
	data := encoded.Bytes()
	digest := sha256.Sum256(data)
	sha := hex.EncodeToString(digest[:])
	deadline := moduleTestNow.Add(time.Hour).UnixMilli()
	put := func(offset int64, chunk []byte) *appv1.PutEvidenceReply {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		out, err := writer.PutEvidence(ctx, &appv1.PutEvidenceRequest{ComponentType: source.ComponentType, Stream: source.Stream, Sha256: sha, Size: int64(len(data)), Offset: offset, Data: chunk, ContentType: "image/jpeg", RetainUntilUnixMs: deadline})
		if err != nil {
			t.Fatal(err)
		}
		return out
	}
	get := &appv1.GetEvidenceRequest{Source: accessSelector(source), Sha256: sha, Length: int32(len(data))}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if put(0, data[:9]).Complete {
		t.Fatal("partial upload exposed as complete")
	}
	if out, err := reader.GetEvidence(ctx, get); err == nil || out != nil {
		t.Fatal("incomplete media was readable")
	}
	if out := put(9, data[9:]); !out.Complete || out.Received != int64(len(data)) || out.RetainUntilUnixMs != deadline {
		t.Fatal("completed upload lost metadata")
	}
	req := moduleRequest("image")
	req.PayloadJson = []byte(fmt.Sprintf(`{"sha256":%q,"size":%d,"content_type":"image/jpeg","duration_ns":0,"calibration_revision":"camera-cal-1"}`, sha, len(data)))
	proof := modulePublish(t, writer, req)
	if got := accessQuery(t, reader, []serviceaccess.Source{source}, 1, ""); len(got.SignedRecordJson) != 1 || !bytes.Equal(got.SignedRecordJson[0], proof.SignedRecordJson) {
		t.Fatal("media observation proof changed")
	}
	if out, err := writer.GetEvidence(ctx, get); err == nil || out != nil {
		t.Fatal("writer inherited read permission")
	}
	if out, err := reader.PutEvidence(ctx, &appv1.PutEvidenceRequest{ComponentType: source.ComponentType, Stream: source.Stream, Sha256: sha, Size: int64(len(data)), Data: data, ContentType: "image/jpeg", RetainUntilUnixMs: deadline}); err == nil || out != nil {
		t.Fatal("read-only scope uploaded evidence")
	}
	wrongOwner := proto.Clone(get).(*appv1.GetEvidenceRequest)
	wrongOwner.Source = accessSelector(otherSource)
	if out, err := reader.GetEvidence(ctx, wrongOwner); err == nil || out != nil {
		t.Fatal("digest bypassed exact evidence owner")
	}
	sampleNS.Store(moduleTestNow.Add(31 * time.Second).UnixNano())
	if got := accessQuery(t, reader, []serviceaccess.Source{source}, 1, ""); len(got.SignedRecordJson) != 0 {
		t.Fatal("expired observation stayed live")
	}
	if out, err := reader.GetEvidence(ctx, get); err != nil || !bytes.Equal(out.GetData(), data) || out.RetainUntilUnixMs != deadline {
		t.Fatal("observation expiry deleted or renewed retained evidence", err)
	}
	if err := f.manager.Close(); err != nil {
		t.Fatal(err)
	}
	f.manager, err = New(context.Background(), f.config, f.deps)
	if err != nil {
		t.Fatal(err)
	}
	restarted := accessMount(t, f, source, otherSource)
	if restarted.Directory == oldReader.Directory {
		t.Fatal("restart reused old mounted directory")
	}
	current := moduleDial(t, restarted.SocketPath)
	get.Offset, get.Length = 7, 11
	if out, err := current.GetEvidence(ctx, get); err != nil || !bytes.Equal(out.GetData(), data[7:18]) || out.RetainUntilUnixMs != deadline || out.Sha256 != sha {
		t.Fatal("restart changed retained owner bytes, range or deadline", err)
	}
	f.identity.invalidate()
	if out, err := current.GetEvidence(ctx, get); err == nil || out != nil {
		t.Fatal("revoked identity retained evidence access")
	}
}

func TestAccessDeniedDeviceCannotMountReadOnlyService(t *testing.T) {
	f := setupModule(t, accessConfig)
	if err := f.manager.Close(); err != nil {
		t.Fatal(err)
	}
	hash := sha256.Sum256(f.identity.snapshot.ChainDER[0])
	f.config.Policy.DeniedCertificates = []string{hex.EncodeToString(hash[:])}
	f.config.CreateJournal = false
	var err error
	f.manager, err = New(context.Background(), f.config, f.deps)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.manager.EnsureAccess(context.Background(), "com.example.fusion", "reader", 1, nil, []serviceaccess.Source{accessSource("telemetry-a", "wendy.sensor.scalar.v1", "gas")}); err == nil {
		t.Fatal("read-only listener bypassed the denied device certificate")
	}
}
