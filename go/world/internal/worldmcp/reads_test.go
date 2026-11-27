package worldmcp

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/wendylabsinc/wendy/go/world/internal/authority"
	"github.com/wendylabsinc/wendy/go/world/internal/client"
	"github.com/wendylabsinc/wendy/go/world/internal/mission"
	"github.com/wendylabsinc/wendy/go/world/internal/presence"
	"github.com/wendylabsinc/wendy/go/world/internal/proposalreplica"
	"github.com/wendylabsinc/wendy/go/world/internal/proposals"
	"github.com/wendylabsinc/wendy/go/world/internal/sensing"
	"github.com/wendylabsinc/wendy/go/world/internal/sitepub"
	"github.com/wendylabsinc/wendy/go/world/internal/taskhistory"
	"github.com/wendylabsinc/wendy/go/world/internal/tasking"
	"github.com/wendylabsinc/wendy/go/world/internal/world"
	"github.com/wendylabsinc/wendy/go/world/internal/zonepub"
)

func readFixture(t *testing.T) (client.Config, *authority.Verifier) {
	t.Helper()
	root, rk, _ := ed25519.GenerateKey(rand.Reader)
	pub, key, _ := ed25519.GenerateKey(rand.Reader)
	now := time.Now()
	grant, err := authority.SignGrant(authority.Grant{Version: 1, ID: "reader", Tenant: "tenant", Site: "site", Domain: "simulation", Principal: "reader", Kind: "agent", Source: world.Source{Device: "device", App: "chat", Service: "world", Stream: "reads"}, PublicKey: pub, Scopes: []string{"read"}, NotBeforeUnixMS: now.Add(-time.Minute).UnixMilli(), NotAfterUnixMS: now.Add(time.Hour).UnixMilli()}, rk)
	if err != nil {
		t.Fatal(err)
	}
	v, err := authority.NewVerifier(authority.Config{RootPublicKey: root, Tenant: "tenant", Site: "site", Domain: "simulation", Audience: "gateway", Now: time.Now, Replay: authority.NewMemoryReplay(100)})
	if err != nil {
		t.Fatal(err)
	}
	rev, err := authority.SignRevocations(authority.Revocations{Version: 1, Tenant: "tenant", Site: "site", Domain: "simulation", Revision: 1, IssuedAtUnixMS: now.Add(-time.Second).UnixMilli(), ExpiresAtUnixMS: now.Add(30 * time.Minute).UnixMilli()}, rk)
	if err != nil {
		t.Fatal(err)
	}
	if err = v.UpdateRevocations(rev); err != nil {
		t.Fatal(err)
	}
	return client.Config{Audience: "gateway", Grant: grant, PrivateKey: key, AllowInsecureSimulation: true}, v
}
func readSession(t *testing.T, cfg client.Config) *mcp.ClientSession {
	t.Helper()
	srv, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	st, ct := mcp.NewInMemoryTransports()
	ss, err := srv.Connect(ctx, st, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ss.Close() })
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "reads-test", Version: "1"}, nil).Connect(ctx, ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cs.Close() })
	return cs
}
func publicationFixtures(t *testing.T) (world.Component, world.Component) {
	t.Helper()
	now := time.Now()
	cfg := sitepub.Config{Tenant: "tenant", Site: "site", RunID: "run", Source: world.Source{Device: "source", App: "site", Service: "catalog", Stream: "metadata"}, Epoch: 1, Sequence: 1, Now: now, TTL: time.Minute}
	raw, err := os.ReadFile("../../examples/geography/office.json")
	if err != nil {
		t.Fatal(err)
	}
	raw = bytes.ReplaceAll(raw, []byte("synthetic-office"), []byte("site"))
	site, err := sitepub.Build(raw, cfg)
	if err != nil {
		t.Fatal(err)
	}
	z := zonepub.Catalog{SchemaVersion: zonepub.Type, Name: "Room", Revision: "v1", Site: "site", RunID: "run", Frame: world.FrameRef{ID: "floor", Revision: "1"}, ValidFrom: now.Add(-time.Minute), ValidUntil: now.Add(time.Hour), Zones: []presence.Zone{{ID: "room", Label: "Room", MinXZ: [2]float64{0, 0}, MaxXZ: [2]float64{2, 2}, DwellSeconds: 2}}}
	raw, _ = json.Marshal(z)
	zone, err := zonepub.Build(raw, cfg)
	if err != nil {
		t.Fatal(err)
	}
	return site, zone
}
func proposalFixture() proposals.Record {
	return proposals.Record{Sequence: 1, Proposal: proposals.Request{Version: 1, ID: "proposal", Tenant: "tenant", Site: "site", RunID: "run", Assignee: "robot", Type: "inspect", Parameters: json.RawMessage(`{"point":[1,2,3]}`), Summary: "Inspect", NotAfterUnixMS: time.Now().Add(time.Minute).UnixMilli()}, Principal: "proposer", Kind: "agent", Source: world.Source{Device: "source", App: "chat", Service: "world", Stream: "proposals"}, Class: mission.Move, AcceptedAtUnixMS: time.Now().UnixMilli(), BodySHA256: strings.Repeat("a", 64), Body: []byte("stored-body-must-not-escape"), Proof: authority.SignedRequest{Signature: []byte("stored-signature-must-not-escape")}}
}
func historyFixture() taskhistory.List {
	return taskhistory.List{Origin: "origin&fixed=yes", RunID: "run", TaskID: "task", Events: []taskhistory.View{{Receipt: authority.SignedTaskAudit{Audience: "origin&fixed=yes", RunID: "run", Sequence: 1, Event: json.RawMessage(`{"stored_event":"must-not-escape"}`)}, Event: tasking.Audit{Sequence: 1, UnixMS: 1234, TaskID: "task", Principal: "operator", Event: "stopped", Status: mission.Status{Version: 2, Phase: mission.Canceled}, Proof: json.RawMessage(`{"authority_secret":"do-not-return"}`)}, HistoricalProvenanceVerified: true}}, Historical: true}
}
func TestAdditionalReadsUseSignedReadOnlyHTTPAndSanitizeProofs(t *testing.T) {
	cfg, verifier := readFixture(t)
	site, zone := publicationFixtures(t)
	proposal := proposalFixture()
	var calls atomic.Int32
	var deny atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		raw, err := base64.StdEncoding.DecodeString(r.Header.Get("X-World-Authorization"))
		var signed authority.SignedRequest
		if err != nil || authority.DecodeStrict(raw, &signed) != nil {
			t.Error("unsigned request")
			http.Error(w, "denied", 401)
			return
		}
		id, err := verifier.Verify(signed, "read", body)
		if err != nil || id.Kind != "agent" {
			t.Errorf("read authority: %v", err)
			http.Error(w, "denied", 401)
			return
		}
		calls.Add(1)
		if deny.Load() {
			http.Error(w, "sensitive denial details", 403)
			return
		}
		if r.Method != http.MethodGet && !(r.Method == http.MethodPost && r.URL.Path == "/v1/annotations/views") {
			t.Error("unexpected mutation", r.Method, r.URL)
			http.Error(w, "bad", 400)
			return
		}
		var reply any
		switch r.URL.Path {
		case "/v1/sites":
			reply = struct {
				Publications []authority.SignedRecord `json:"publications"`
			}{[]authority.SignedRecord{{Component: site, Signature: []byte("signature")}}}
		case "/v1/zones":
			reply = zonepub.Reply{Records: []authority.SignedRecord{{Component: zone, Signature: []byte("signature")}}}
		case "/v1/annotations/views":
			var q struct {
				Limit    int    `json:"limit"`
				Revision string `json:"revision"`
				Offset   int    `json:"offset"`
			}
			if authority.DecodeStrict(body, &q) != nil || q.Limit != 8 {
				t.Error("wrong annotation page")
			}
			now := time.Now()
			reply = annotationPage{Views: []annotationRow{{OriginalSHA256: strings.Repeat("a", 64), HeadSHA256: strings.Repeat("a", 64), OriginalKey: world.Key{Tenant: "tenant", Site: "site"}, Frame: world.FrameRef{ID: "floor", Revision: "1"}, State: "withdrawn", Text: "withdrawn-text-must-not-return", CurrentVerification: true, ExpiresAtUnixMS: now.Add(30 * time.Minute).UnixMilli(), VerificationUntilUnixMS: now.Add(time.Hour).UnixMilli()}}, Revision: strings.Repeat("b", 64), NextOffset: 1, Complete: true, ObservedAt: now, Scope: "local replica"}
		case "/v1/proposals":
			reply = proposals.Page{Proposals: []proposals.Summary{{ID: "proposal", Summary: "Inspect", NotAfterUnixMS: time.Now().Add(time.Minute).UnixMilli()}}}
		case "/v1/proposals/proposal":
			reply = proposals.View{Record: proposal}
		case "/v1/proposal-inbox":
			if r.URL.Query().Get("origin") != "origin&fixed=yes" {
				t.Error("origin lost or interpreted as query", r.URL)
			}
			v := proposalreplica.View{Origin: "origin&fixed=yes", Record: proposal, HistoricalProvenanceVerified: true}
			if r.URL.Query().Get("id") != "" {
				reply = v
			} else {
				reply = proposalreplica.List{Origin: v.Origin, Proposals: []proposalreplica.View{v}}
			}
		case "/v1/task-history":
			reply = historyFixture()
		case "/v1/tasks/status":
			reply = tasking.Record{Spec: tasking.Spec{ID: "task", Site: "site", Assignee: "robot"}, Status: mission.Status{Version: 2, Phase: mission.Succeeded}, Proof: json.RawMessage(`{"authority_secret":"do-not-return"}`), Result: &tasking.Inspection{TaskID: "task", RunID: "run", CapturedUnixMS: 1234, Frame: world.FrameRef{ID: "floor", Revision: "1"}, Sensors: map[string]bool{}, Samples: []sensing.Sample{}, Trigger: &tasking.TriggerRef{SHA256: strings.Repeat("c", 64), Component: world.Component{Payload: json.RawMessage(`{"trigger_proof":"do-not-return"}`)}}}}
		default:
			t.Error("unexpected endpoint", r.URL)
			http.NotFound(w, r)
			return
		}
		json.NewEncoder(w).Encode(reply)
	}))
	defer server.Close()
	cfg.URL = server.URL
	cs := readSession(t, cfg)
	for _, test := range []struct {
		name string
		args map[string]any
	}{
		{"world_sites", map[string]any{}}, {"world_zones", map[string]any{}}, {"world_annotations", map[string]any{}}, {"world_proposals", map[string]any{}}, {"world_proposal", map[string]any{"id": "proposal"}}, {"world_proposals", map[string]any{"origin": "origin&fixed=yes"}}, {"world_proposal", map[string]any{"id": "proposal", "origin": "origin&fixed=yes"}}, {"world_task_history", map[string]any{"origin": "origin&fixed=yes", "task_id": "task"}}, {"world_task_result", map[string]any{"task_id": "task"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			result := reviewCall(t, cs, test.name, test.args)
			value := reviewResult(t, result)
			if value.Unavailable {
				t.Fatalf("read failed: %+v", value)
			}
			encoded, _ := json.Marshal(result)
			for _, secret := range []string{`"proof":`, `"origin_proof":`, `"signature":`, `"grant":`, `"receipt":`, `"authority_secret":`, `"trigger_proof":`, "withdrawn-text-must-not-return", "stored-body-must-not-escape"} {
				if bytes.Contains(encoded, []byte(secret)) {
					t.Fatalf("leaked %s", secret)
				}
			}
		})
	}
	if calls.Load() != 9 {
		t.Fatal("unexpected requests", calls.Load())
	}
	deny.Store(true)
	got := reviewCall(t, cs, "world_sites", map[string]any{})
	encoded, _ := json.Marshal(got)
	if !got.IsError || bytes.Contains(encoded, []byte("sensitive denial")) {
		t.Fatal("denial leaked or reported success")
	}
}

func TestReadPagesRejectMismatchedScopeAndUnsafeChronology(t *testing.T) {
	cfg := reviewConfig(t)
	for _, test := range []struct {
		name  string
		args  map[string]any
		reply any
	}{
		{"world_task_history", map[string]any{"origin": "wrong", "task_id": "task"}, historyFixture()},
		{"world_task_history", map[string]any{"origin": "origin&fixed=yes", "task_id": "other"}, historyFixture()},
		{"world_proposal", map[string]any{"id": "wrong"}, proposals.View{Record: proposalFixture()}},
		{"world_proposal", map[string]any{"id": "proposal", "origin": "other"}, proposalreplica.View{Origin: "wrong", Record: proposalFixture()}},
		{"world_proposal", map[string]any{"id": "proposal", "origin": "origin"}, proposalreplica.View{Origin: "origin", Record: proposalFixture(), Executable: true}},
		{"world_task_result", map[string]any{"task_id": "task"}, tasking.Record{Spec: tasking.Spec{ID: "other", Site: "site"}}},
		{"world_task_result", map[string]any{"task_id": "task"}, tasking.Record{Spec: tasking.Spec{ID: "task", Site: "other"}}},
	} {
		t.Run(test.name, func(t *testing.T) {
			cs := reviewSession(t, cfg, reviewBackend(func(_ context.Context, op, method, path string, _ []byte, out any) error {
				return reviewReply(out, test.reply)
			}))
			r := reviewResult(t, reviewCall(t, cs, test.name, test.args))
			if !r.Unavailable || r.Code != "invalid_backend_response" {
				t.Fatalf("accepted scope change: %+v", r)
			}
		})
	}
	history := historyFixture()
	history.Events[0].Executable = true
	cs := reviewSession(t, cfg, reviewBackend(func(_ context.Context, _, _, _ string, _ []byte, out any) error { return reviewReply(out, history) }))
	if r := reviewResult(t, reviewCall(t, cs, "world_task_history", map[string]any{"origin": history.Origin, "task_id": "task"})); !r.Unavailable {
		t.Fatal("accepted executable history")
	}
}

func TestAnnotationPaginationAndExpiryCannotResurrectText(t *testing.T) {
	now := time.Now()
	cfg := reviewConfig(t)
	var calls atomic.Int32
	row := annotationRow{OriginalSHA256: strings.Repeat("a", 64), HeadSHA256: strings.Repeat("a", 64), OriginalKey: world.Key{Tenant: "tenant", Site: "site"}, Frame: world.FrameRef{ID: "floor", Revision: "1"}, State: "original", Text: "current annotation", CurrentVerification: true, ExpiresAtUnixMS: now.Add(time.Hour).UnixMilli(), VerificationUntilUnixMS: now.Add(time.Hour).UnixMilli()}
	page := annotationPage{Views: []annotationRow{row}, Revision: strings.Repeat("b", 64), NextOffset: 1, Complete: false, ObservedAt: now, Scope: "local replica"}
	cs := reviewSession(t, cfg, reviewBackend(func(_ context.Context, op, method, path string, body []byte, out any) error {
		calls.Add(1)
		if op != "read" || method != "POST" || path != "/v1/annotations/views" {
			t.Fatal("annotation read changed route or authority")
		}
		return reviewReply(out, page)
	}))
	r := reviewResult(t, reviewCall(t, cs, "world_annotations", map[string]any{"limit": 1}))
	if r.Unavailable || !r.Truncated {
		t.Fatalf("page lost incompleteness: %+v", r)
	}
	raw, _ := json.Marshal(r.Data)
	if !bytes.Contains(raw, []byte(row.Text)) {
		t.Fatal("current original text disappeared")
	}
	for _, args := range []map[string]any{{"offset": 1}, {"revision": "not-a-digest", "offset": 1}, {"limit": 9}, {"offset": -1}} {
		got := reviewResult(t, reviewCall(t, cs, "world_annotations", args))
		if !got.Unavailable {
			t.Fatal("invalid traversal accepted", args)
		}
	}
	if calls.Load() != 1 {
		t.Fatal("invalid traversal reached gateway")
	}
	page.Views[0].ExpiresAtUnixMS = now.Add(-time.Second).UnixMilli()
	page.Complete = true
	got := reviewResult(t, reviewCall(t, cs, "world_annotations", map[string]any{}))
	raw, _ = json.Marshal(got.Data)
	if bytes.Contains(raw, []byte(row.Text)) || bytes.Contains(raw, []byte(`"current_verification":true`)) {
		t.Fatal("expired text was treated as live")
	}
	page.NextOffset = 9
	if got := reviewResult(t, reviewCall(t, cs, "world_annotations", map[string]any{})); !got.Unavailable {
		t.Fatal("accepted skipped annotation offset")
	}
	page.NextOffset = 1
	page.Views[0].OriginalKey.Site = "other"
	if got := reviewResult(t, reviewCall(t, cs, "world_annotations", map[string]any{})); !got.Unavailable {
		t.Fatal("accepted annotation from another site")
	}
}

func TestReadTruncationNeverSkipsHiddenHistoryOrReturnsProofs(t *testing.T) {
	cfg := reviewConfig(t)
	site, _ := publicationFixtures(t)
	sites := struct {
		Publications []authority.SignedRecord `json:"publications"`
	}{Publications: []authority.SignedRecord{}}
	for i := 0; i < 100; i++ {
		sites.Publications = append(sites.Publications, authority.SignedRecord{Component: site})
	}
	cs := reviewSession(t, cfg, reviewBackend(func(_ context.Context, _, _, _ string, _ []byte, out any) error { return reviewReply(out, sites) }))
	value := reviewCall(t, cs, "world_sites", map[string]any{"limit": 100})
	encoded, _ := json.Marshal(value)
	r := reviewResult(t, value)
	if r.Unavailable || !r.Truncated || len(encoded) > MaxResultBytes {
		t.Fatalf("site output bound lost: bytes=%d %+v", len(encoded), r)
	}
	var data publicationsData
	raw, _ := json.Marshal(r.Data)
	json.Unmarshal(raw, &data)
	if len(data.Components) == 0 || len(data.Components) >= 100 {
		t.Fatal("expected useful bounded prefix")
	}
	page := historyFixture()
	page.Events = []taskhistory.View{}
	page.Next = "opaque-next-page"
	for i := 0; i < 10; i++ {
		v := historyFixture().Events[0]
		v.Receipt.Sequence = uint64(i + 1)
		v.Event.Sequence = uint64(i + 1)
		v.Event.Status.Reason = strings.Repeat("detailed retained history ", 600)
		page.Events = append(page.Events, v)
	}
	cs = reviewSession(t, cfg, reviewBackend(func(_ context.Context, _, _, _ string, _ []byte, out any) error { return reviewReply(out, page) }))
	value = reviewCall(t, cs, "world_task_history", map[string]any{"origin": page.Origin, "task_id": "task", "limit": 10})
	encoded, _ = json.Marshal(value)
	r = reviewResult(t, value)
	if r.Unavailable || !r.Truncated || len(encoded) > MaxResultBytes || bytes.Contains(encoded, []byte("opaque-next-page")) || bytes.Contains(encoded, []byte("authority_secret")) {
		t.Fatal("truncated history skipped records or leaked proof", len(encoded), r)
	}
}

func TestHistoryCannotReorderOrDuplicateRetainedEvents(t *testing.T) {
	for _, sequences := range [][]uint64{{1, 1}, {2, 1}, {0}} {
		page := historyFixture()
		page.Events = nil
		for _, sequence := range sequences {
			v := historyFixture().Events[0]
			v.Event.Sequence = sequence
			v.Receipt.Sequence = sequence
			page.Events = append(page.Events, v)
		}
		cs := reviewSession(t, reviewConfig(t), reviewBackend(func(_ context.Context, _, _, _ string, _ []byte, out any) error { return reviewReply(out, page) }))
		if got := reviewResult(t, reviewCall(t, cs, "world_task_history", map[string]any{"origin": page.Origin, "task_id": "task"})); !got.Unavailable {
			t.Fatal("accepted invalid event order", sequences)
		}
	}
}
func TestProposalPageCursorCannotSkipOrRepeatRows(t *testing.T) {
	for _, page := range []proposals.Page{
		{Proposals: []proposals.Summary{{ID: "a"}}, Next: "z"},
		{Proposals: []proposals.Summary{{ID: "b"}, {ID: "a"}}},
		{Proposals: []proposals.Summary{{ID: "a"}, {ID: "a"}}},
		{Proposals: []proposals.Summary{}, Next: "a"},
	} {
		cs := reviewSession(t, reviewConfig(t), reviewBackend(func(_ context.Context, _, _, _ string, _ []byte, out any) error { return reviewReply(out, page) }))
		if got := reviewResult(t, reviewCall(t, cs, "world_proposals", map[string]any{})); !got.Unavailable {
			t.Fatal("accepted misleading proposal page", page)
		}
	}
}
