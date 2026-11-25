package simbackend

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/world/internal/authority"
	"github.com/wendylabsinc/wendy/go/world/internal/client"
	"github.com/wendylabsinc/wendy/go/world/internal/world"
	"github.com/wendylabsinc/wendy/go/world/production/readservice"
)

func fixture(t *testing.T) (client.Config, readservice.Dataset) {
	t.Helper()
	pub, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return client.Config{URL: "http://127.0.0.1:18881", Audience: "sim-node", AllowInsecureSimulation: true, PrivateKey: key, Grant: authority.SignedGrant{Grant: authority.Grant{Version: 1, ID: "lab-test-reader", Tenant: "lab", Site: "office", Domain: "simulation", Principal: "test-reader", Kind: "operator", PublicKey: pub, Scopes: []string{"read", "evidence-read"}, NotBeforeUnixMS: time.Now().Add(-time.Hour).UnixMilli(), NotAfterUnixMS: time.Now().Add(time.Hour).UnixMilli()}}}, readservice.Dataset{ID: "virtual-office", Domain: "simulation", SourceTenant: "lab", SourceSite: "office", Frame: readservice.FrameRef{ID: "floor", Revision: "1"}, RunID: "run"}
}
func TestNumericLoopbackAndReadOnlyCredential(t *testing.T) {
	cfg, dataset := fixture(t)
	for _, url := range []string{"http://localhost:18881", "http://example.invalid:18881", "http://192.168.1.1:18881", "https://127.0.0.1:18881", "http://127.0.0.1", "http://u:p@127.0.0.1:18881", "http://127.0.0.1:18881/?", "http://127.0.0.1:18881/#x", "http://127.0.0.1:18881/x", "http://127.0.0.1:18881/%2f"} {
		c := cfg
		c.URL = url
		if _, err := New(c, dataset); err == nil {
			t.Fatalf("accepted %q", url)
		}
	}
	for _, url := range []string{"http://127.0.0.1:18881", "http://[::1]:18881/"} {
		c := cfg
		c.URL = url
		if _, err := New(c, dataset); err != nil {
			t.Fatal(err)
		}
	}
	for _, change := range []func(*client.Config){func(c *client.Config) { c.Grant.Grant.Scopes = []string{"read", "publish"} }, func(c *client.Config) { c.Grant.Grant.Scopes = []string{"read", "annotation-write"} }, func(c *client.Config) { c.Grant.Grant.Scopes = []string{"evidence-read"} }, func(c *client.Config) { c.Grant.Grant.PublishAudience = "node" }, func(c *client.Config) { c.Grant.Grant.ExecuteAssignee = "robot" }, func(c *client.Config) { c.Grant.Grant.TaskStatusAssignee = "robot" }, func(c *client.Config) { c.Grant.Grant.TaskStatusAudience = "node" }, func(c *client.Config) { c.Grant.Grant.Kind = "app" }, func(c *client.Config) { c.Grant.Grant.Domain = "physical" }, func(c *client.Config) { c.Grant.Grant.Tenant = "other" }, func(c *client.Config) { c.AllowInsecureSimulation = false }, func(c *client.Config) { c.PrivateKey = []byte("wrong") }} {
		c := cfg
		change(&c)
		if _, err := New(c, dataset); err == nil {
			t.Fatal("accepted overbroad or mismatched credential")
		}
	}
}
func TestQueryOnlyReadsAndPreservesComponent(t *testing.T) {
	cfg, dataset := fixture(t)
	var calls atomic.Int64
	component := world.Component{Key: world.Key{Tenant: "lab", Site: "office", Entity: "gas", Type: "gas", Source: world.Source{Device: "gateway", App: "sensor", Service: "main", Stream: "gas"}}, Version: world.Version{Epoch: 1, Sequence: 1}, Frame: world.FrameRef{ID: "floor", Revision: "1"}, Timing: world.Timing{SampleTime: time.Now().UTC(), ClockDomain: "simulation:run", ErrorKnown: true}, TTL: time.Minute, Provenance: world.Provenance{Origin: world.Simulated, AppVersion: "1", Simulation: &world.SimulationContext{RunID: "run", ScenarioVersion: "1", ClockDomain: "simulation:run"}}, Payload: json.RawMessage(`{"value":12,"unit":"ppm"}`)}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Method != "GET" || r.URL.Path != "/v1/components" || r.URL.RawQuery != "" {
			t.Errorf("unexpected API: %s %s", r.Method, r.URL)
		}
		raw, err := base64.StdEncoding.DecodeString(r.Header.Get("X-World-Authorization"))
		if err != nil {
			t.Error(err)
		}
		var signed authority.SignedRequest
		if json.Unmarshal(raw, &signed) != nil || signed.Operation != "read" || signed.Audience != cfg.Audience {
			t.Error("wrong request scope")
		}
		if strings.Contains(string(raw), base64.StdEncoding.EncodeToString(cfg.PrivateKey)) {
			t.Error("private key sent to backend")
		}
		_ = json.NewEncoder(w).Encode(client.Snapshot{Components: []world.Component{component}, Cursor: strings.Repeat("a", 64)})
	}))
	defer server.Close()
	cfg.URL = server.URL
	reader, err := New(cfg, dataset)
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := reader.Query(context.Background(), readservice.Query{Version: 1, DatasetID: dataset.ID, Limit: 1})
	if err != nil {
		t.Fatal(err)
	}
	want, _ := json.Marshal(component)
	if snapshot.Dataset != dataset || len(snapshot.Components) != 1 || string(snapshot.Components[0]) != string(want) {
		t.Fatalf("changed data: %+v", snapshot)
	}
	for _, q := range []readservice.Query{{Version: 2, DatasetID: dataset.ID, Limit: 1}, {Version: 1, DatasetID: "other", Limit: 1}, {Version: 1, DatasetID: dataset.ID, Limit: 257}} {
		if _, err := reader.Query(context.Background(), q); err == nil {
			t.Fatal("accepted invalid query")
		}
	}
	if calls.Load() != 1 {
		t.Fatal("invalid query reached upstream")
	}
}
func TestBackendRedirectNeverForwardsCredential(t *testing.T) {
	var leaked atomic.Bool
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { leaked.Store(true) }))
	defer target.Close()
	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL, http.StatusTemporaryRedirect)
	}))
	defer source.Close()
	cfg, d := fixture(t)
	cfg.URL = source.URL
	r, err := New(cfg, d)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = r.Query(context.Background(), readservice.Query{Version: 1, DatasetID: d.ID, Limit: 1}); err == nil {
		t.Fatal("redirect accepted")
	}
	if leaked.Load() {
		t.Fatal("redirect target contacted")
	}
}
func TestBackendCancellationAndBound(t *testing.T) {
	for _, oversize := range []bool{false, true} {
		t.Run(map[bool]string{false: "cancel", true: "size"}[oversize], func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if oversize {
					_, _ = io.WriteString(w, strings.Repeat("x", (1<<20)+1))
					return
				}
				<-r.Context().Done()
			}))
			defer server.Close()
			cfg, d := fixture(t)
			cfg.URL = server.URL
			reader, err := New(cfg, d)
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
			defer cancel()
			if _, err = reader.Query(ctx, readservice.Query{Version: 1, DatasetID: d.ID, Limit: 1}); err == nil {
				t.Fatal("unbounded response accepted")
			}
		})
	}
}

func TestQuerySelectsCommissionedFrameBeforeLimit(t *testing.T) {
	cfg, dataset := fixture(t)
	metadata := world.Component{Key: world.Key{Tenant: dataset.SourceTenant, Site: dataset.SourceSite, Entity: "site-package", Type: "wendy.site.georeference.v1"}, Frame: world.FrameRef{ID: "site-metadata", Revision: "v1"}, Payload: json.RawMessage(`{"metadata":true}`)}
	oldRevision := metadata
	oldRevision.Frame = world.FrameRef{ID: dataset.Frame.ID, Revision: "old-revision"}
	selected := metadata
	selected.Key.Entity = "gas"
	selected.Key.Type = "wendy.sensor.v1"
	selected.Frame = world.FrameRef{ID: dataset.Frame.ID, Revision: dataset.Frame.Revision}
	selected.Payload = json.RawMessage(`{"selected":true}`)
	cursor := strings.Repeat("b", 64)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(client.Snapshot{Cursor: cursor, Components: []world.Component{metadata, oldRevision, selected}})
	}))
	defer server.Close()
	cfg.URL = server.URL
	reader, err := New(cfg, dataset)
	if err != nil {
		t.Fatal(err)
	}
	got, err := reader.Query(context.Background(), readservice.Query{Version: 1, DatasetID: dataset.ID, Limit: 1})
	if err != nil {
		t.Fatal(err)
	}
	want, _ := json.Marshal(selected)
	if len(got.Components) != 1 || string(got.Components[0]) != string(want) || got.Cursor != cursor || got.Dataset != dataset {
		t.Fatal("other frames leaked, consumed limit, or changed source cursor")
	}
}
