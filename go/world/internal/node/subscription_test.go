package node

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/world/internal/authority"
	"github.com/wendylabsinc/wendy/go/world/internal/client"
	"github.com/wendylabsinc/wendy/go/world/internal/mission"
	"github.com/wendylabsinc/wendy/go/world/internal/selection"
	"github.com/wendylabsinc/wendy/go/world/internal/sitepub"
	"github.com/wendylabsinc/wendy/go/world/internal/store"
	"github.com/wendylabsinc/wendy/go/world/internal/subscription"
	"github.com/wendylabsinc/wendy/go/world/internal/tasking"
	"github.com/wendylabsinc/wendy/go/world/internal/world"
)

func nodePolicy(kind string) *subscription.Policy {
	return &subscription.Policy{Version: 1, Tenant: "tenant", Site: "office", Selection: selection.Selection{Types: []string{kind}}, RequiredTypes: []string{sitepub.Type}}
}
func subscribedFixture(t *testing.T, root ed25519.PrivateKey, name string, capacity int, policy *subscription.Policy) *fixture {
	t.Helper()
	f := newFixture(t, root, name)
	f.server.Close()
	if err := f.s.Close(); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	f.cfg.ProofPath = filepath.Join(dir, "proofs.json")
	f.cfg.MaxRecords = capacity
	f.wc.MaxEntries = capacity
	f.wc.Frames = append(f.wc.Frames, sitepub.MetadataFrame())
	var err error
	f.s, err = store.OpenSubscribed(filepath.Join(dir, "store.json"), f.wc, policy)
	if err != nil {
		t.Fatal(err)
	}
	f.n, err = New(f.cfg, f.s, f.v)
	if err != nil {
		t.Fatal(err)
	}
	f.start(t)
	return f
}
func subscribedRestart(t *testing.T, f *fixture, policy *subscription.Policy) {
	t.Helper()
	f.server.Close()
	if err := f.s.Close(); err != nil {
		t.Fatal(err)
	}
	var err error
	f.s, err = store.OpenSubscribed(filepath.Join(filepath.Dir(f.cfg.ProofPath), "store.json"), f.wc, policy)
	if err != nil {
		t.Fatal(err)
	}
	f.n, err = New(f.cfg, f.s, f.v)
	if err != nil {
		t.Fatal(err)
	}
	f.start(t)
}
func subscriptionInventory(t *testing.T, f *fixture, creds Credentials, p *subscription.Policy, cursor string) (int, RepairInventoryResponse) {
	t.Helper()
	body, _ := json.Marshal(RepairInventoryRequest{Version: SelectiveRepairVersion, Subscription: p, Cursor: cursor})
	status, raw := call(t, f, creds, "POST", "/v1/repair/inventory", "replicate", body)
	var out RepairInventoryResponse
	if status == 200 {
		if err := json.Unmarshal(raw, &out); err != nil {
			t.Fatal(err)
		}
	}
	return status, out
}

func TestSelectiveThreeNodePartitionRestartAndMetadata(t *testing.T) {
	a := subscribedFixture(t, nil, "source", 256, nil)
	b := subscribedFixture(t, a.root, "gas-reader", 32, nodePolicy("wendy.gas.v0"))
	c := subscribedFixture(t, a.root, "temperature-reader", 192, nodePolicy("wendy.temperature.v0"))
	recordCLI := selectiveCLIRecorder(t)
	epoch := allocate(t, a)
	rows := make([]world.Component, 200)
	for i := range rows {
		row := component(a, epoch, uint64(i+1))
		row.Key.Entity = fmt.Sprintf("sensor-%03d", i)
		if i%10 != 0 {
			row.Key.Type = "wendy.temperature.v0"
		}
		rows[i] = row
		if _, err := a.n.apply(signed(t, a, row), false); err != nil {
			t.Fatal(err)
		}
	}
	siteAdmin(t, a)
	metadata := siteComponent(t, a)
	publish(t, a, metadata)
	var fetchedGas atomic.Int64
	var badFetched atomic.Bool
	var rejectFirstInventory atomic.Bool
	rejectFirstInventory.Store(true)
	original := a.server
	a.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if a.unavailable.Load() {
			http.Error(w, "partition", 503)
			return
		}
		// Exercise the same bounded remote-busy response seen when an earlier
		// timed-out round is still releasing the peer's single repair slot.
		if r.URL.Path == "/v1/repair/inventory" && rejectFirstInventory.Swap(false) {
			http.Error(w, "fixture: repair slot still occupied", http.StatusTooManyRequests)
			return
		}
		if r.URL.Path == "/v1/repair/fetch" {
			body, err := io.ReadAll(r.Body)
			if err != nil {
				t.Error(err)
				return
			}
			r.Body = io.NopCloser(bytes.NewReader(body))
			var req RepairFetchRequest
			json.Unmarshal(body, &req)
			if req.Subscription != nil && req.Subscription.Digest() == b.s.Subscription().Digest() {
				for _, entry := range req.Entries {
					if !b.s.AcceptsKey(entry.Key) {
						badFetched.Store(true)
					}
					fetchedGas.Add(1)
				}
			}
		}
		a.n.Handler().ServeHTTP(w, r)
	}))
	t.Cleanup(a.server.Close)
	original.Close()
	syncTo := func(target *fixture) {
		t.Helper()
		// The production repair budget remains five seconds. Large race-instrumented
		// fixtures can require several bounded rounds; progress must survive them.
		for attempt := 0; attempt < 8; attempt++ {
			err := target.n.Sync(context.Background(), Peer{a.server.URL, a.audience}, target.credentials)
			if err == nil {
				return
			}
			var remote repairHTTPError
			if !errors.Is(err, context.DeadlineExceeded) && !errors.Is(err, errRepairBusy) &&
				!(errors.As(err, &remote) && remote.Status == http.StatusTooManyRequests) {
				t.Fatal(err)
			}
			// Leave the remote handler time to release its slot after cancellation.
			// Keep both the eight-round cap and production repair deadline intact.
			time.Sleep(100 * time.Millisecond)
		}
		t.Fatal("selective repair did not finish within eight bounded rounds")
	}
	syncTo(b)
	syncTo(c)
	for _, pair := range []struct {
		f     *fixture
		count int
	}{{a, 201}, {b, 21}, {c, 181}} {
		records, err := pair.f.s.Records()
		if err != nil || len(records) != pair.count {
			t.Fatalf("%s count %d %v", pair.f.audience, len(records), err)
		}
		for _, record := range records {
			if !pair.f.s.AcceptsKey(record.Key) {
				t.Fatal("irrelevant record persisted")
			}
		}
	}
	if fetchedGas.Load() != 21 || badFetched.Load() {
		t.Fatalf("fetched irrelevant payloads: %d %t", fetchedGas.Load(), badFetched.Load())
	}
	if len(siteList(t, b)) != 1 || len(siteList(t, c)) != 1 {
		t.Fatal("required site metadata missing")
	}
	status, err := selectedClient(t, b, b.credentials).ReplicaStatus(context.Background())
	if err != nil || status.Mode != "partial" || status.PolicySHA256 != b.s.Subscription().Digest() || len(status.Peers) != 1 || status.Peers[0].LastOutcome != "complete" || !status.Peers[0].SourceScopeKnown || status.Peers[0].SourcePartial {
		t.Fatalf("status %+v %v", status, err)
	}
	view := selectedOK(t, b, selection.Request{})
	if view.ReplicaScope == nil || !view.ReplicaScope.Partial || query(t, b).ReplicaScope == nil {
		t.Fatal("partial response unlabeled")
	}
	recordCLI("initial source", a)
	recordCLI("initial gas subset", b)
	recordCLI("initial temperature subset", c)
	// The two subsets retain their own old knowledge during an actual HTTP partition.
	a.unavailable.Store(true)
	if err := b.n.Sync(context.Background(), Peer{a.server.URL, a.audience}, b.credentials); err == nil {
		t.Fatal("partition hidden")
	}
	deleted := rows[0]
	deleted.Version.Epoch = metadata.Version.Epoch
	deleted.Version.Sequence = 300
	deleted.Tombstone = true
	deleted.Payload = nil
	if _, err := a.n.apply(signed(t, a, deleted), false); err != nil {
		t.Fatal(err)
	}
	updated := rows[1]
	updated.Version.Epoch = metadata.Version.Epoch
	updated.Version.Sequence = 301
	updated.Payload = json.RawMessage(`{"temperature":30}`)
	if _, err := a.n.apply(signed(t, a, updated), false); err != nil {
		t.Fatal(err)
	}
	if len(query(t, b).Components) != 21 {
		t.Fatal("partition changed isolated view")
	}
	recordCLI("partition: gas subset retains old knowledge", b)
	a.unavailable.Store(false)
	syncTo(b)
	syncTo(c)
	subscribedRestart(t, b, nodePolicy("wendy.gas.v0"))
	if _, err := b.n.apply(signed(t, a, rows[0]), false); err != nil {
		t.Fatal(err)
	}
	if len(query(t, b).Components) != 20 {
		t.Fatal("restart/replay resurrected selected deletion")
	}
	records, err := b.s.Records()
	if err != nil || len(records) != 21 {
		t.Fatal("high-water lost", err)
	}
	recordCLI("after reconnect and durable restart", b)
	// A full replica can pull from a partial peer, but its receipt records the
	// peer's partial coverage. No source claims global synchronization.
	if err := a.n.Sync(context.Background(), Peer{b.server.URL, b.audience}, a.credentials); err != nil {
		t.Fatal(err)
	}
	sourceStatus, err := selectedClient(t, a, a.credentials).ReplicaStatus(context.Background())
	if err != nil || len(sourceStatus.Peers) != 1 || !sourceStatus.Peers[0].SourcePartial || sourceStatus.Peers[0].SourcePolicySHA256 != b.s.Subscription().Digest() {
		t.Fatalf("partial source hidden: %+v %v", sourceStatus, err)
	}
	recordCLI("full source records partial peer coverage", a)
}

func TestSelectiveCursorAndSignedFilterBinding(t *testing.T) {
	f := subscribedFixture(t, nil, "cursor-source", 128, nil)
	epoch := allocate(t, f)
	for i := 0; i < 70; i++ {
		row := component(f, epoch, uint64(i+1))
		row.Key.Entity = fmt.Sprint(i)
		if _, err := f.n.apply(signed(t, f, row), false); err != nil {
			t.Fatal(err)
		}
	}
	policy := nodePolicy("wendy.gas.v0")
	status, first := subscriptionInventory(t, f, f.credentials, policy, "")
	if status != 200 || first.Cursor == "" || len(first.Entries) != 64 {
		t.Fatalf("page: %d %+v", status, first)
	}
	status, next := subscriptionInventory(t, f, f.credentials, policy, first.Cursor)
	if status != 200 || len(next.Entries) != 6 || next.SubscriptionSHA256 != policy.Digest() {
		t.Fatal("continuation failed")
	}
	changed := nodePolicy("wendy.temperature.v0")
	status, _ = subscriptionInventory(t, f, f.credentials, changed, first.Cursor)
	if status != 400 {
		t.Fatal("cursor accepted changed filter", status)
	}
	status, _ = subscriptionInventory(t, f, issueCredentials(t, f.root, "other"), policy, first.Cursor)
	if status != 400 {
		t.Fatal("cursor accepted another reader", status)
	}
	status, _ = subscriptionInventory(t, f, f.credentials, policy, first.Cursor+"x")
	if status != 400 {
		t.Fatal("tampered cursor accepted")
	}
	original, _ := json.Marshal(RepairInventoryRequest{Version: 2, Subscription: policy})
	modified, _ := json.Marshal(RepairInventoryRequest{Version: 2, Subscription: changed})
	req, _ := http.NewRequest("POST", f.server.URL+"/v1/repair/inventory", bytes.NewReader(modified))
	if err := AuthorizeRequest(req, f.credentials, f.audience, "replicate", original); err != nil {
		t.Fatal(err)
	}
	reply, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	reply.Body.Close()
	if reply.StatusCode != 401 {
		t.Fatal("unsigned filter change accepted", reply.StatusCode)
	}
	// Even an independently authenticated fetch cannot request a key outside its declared filter.
	request := RepairFetchRequest{Version: 2, Subscription: changed, Entries: first.Entries[:1]}
	status, _ = call(t, f, f.credentials, "POST", "/v1/repair/fetch", "replicate", jsonBody(t, request))
	if status != 400 {
		t.Fatal("fetch escaped filter", status)
	}
	subscribedRestart(t, f, nil)
	status, _ = subscriptionInventory(t, f, f.credentials, policy, first.Cursor)
	if status != 400 {
		t.Fatal("restart accepted old cursor")
	}
}

func TestSelectiveImporterAndPublishDoNotTrustPeerSelection(t *testing.T) {
	a := newFixture(t, nil, "source")
	b := subscribedFixture(t, a.root, "target", 16, nodePolicy("wendy.gas.v0"))
	epoch := allocate(t, a)
	good := component(a, epoch, 1)
	bad := good
	bad.Key.Type = "wendy.temperature.v0"
	bad.Key.Entity = "excluded"
	proof := signed(t, a, bad)
	if _, err := b.n.apply(proof, false); !errors.Is(err, subscription.ErrExcluded) {
		t.Fatalf("relay admitted irrelevant record: %v", err)
	}
	r := reviewRepairRecord(t, proof, time.Now().Add(time.Minute))
	if _, err := b.n.applyRepairPage([]RepairRecord{r}); err == nil {
		t.Fatal("unsolicited repair admitted")
	}
	goodProof := signed(t, a, good)
	goodRepair := reviewRepairRecord(t, goodProof, time.Now().Add(time.Minute))
	if _, err := b.n.applyRepairPage([]RepairRecord{goodRepair, r}); err == nil {
		t.Fatal("mixed page admitted")
	}
	records, _ := b.s.Records()
	if len(records) != 0 || len(b.n.proofs) != 0 {
		t.Fatal("mixed page persisted partial world or proofs")
	}
	local := component(b, allocate(t, b), 1)
	local.Key.Type = "wendy.temperature.v0"
	status, _ := call(t, b, b.credentials, "POST", "/v1/components", "publish", jsonBody(t, signed(t, b, local)))
	if status == 200 {
		t.Fatal("local publisher bypassed storage scope")
	}
	// A selected key still needs its original signature and current origin grant.
	goodRepair.Record.Signature = bytes.Clone(goodRepair.Record.Signature)
	goodRepair.Record.Signature[0] ^= 1
	if _, err := b.n.applyRepairPage([]RepairRecord{goodRepair}); err == nil {
		t.Fatal("subscription became authority")
	}
	goodRepair = reviewRepairRecord(t, goodProof, time.Now().Add(time.Minute))
	revokeSelected(t, b, a.credentials.Grant.Grant.ID)
	if _, err := b.n.applyRepairPage([]RepairRecord{goodRepair}); err == nil {
		t.Fatal("revoked origin admitted")
	}
	raw, err := os.ReadFile(b.cfg.ProofPath)
	if err == nil && bytes.Contains(raw, []byte("excluded")) {
		t.Fatal("out-of-scope proof persisted")
	}
}

func TestSelectiveMalformedPeerCannotBroadenOrDowngrade(t *testing.T) {
	for _, mode := range []string{"unselected-inventory", "downgrade", "wrong-digest", "wrong-source-scope", "unsolicited-fetch"} {
		t.Run(mode, func(t *testing.T) {
			a := newFixture(t, nil, "origin")
			b := subscribedFixture(t, a.root, "target", 16, nodePolicy("wendy.gas.v0"))
			good := component(a, 1, 1)
			bad := good
			bad.Key.Type = "wendy.temperature.v0"
			bad.Key.Entity = "outside"
			selected := reviewRepairRecord(t, signed(t, a, good), time.Now().Add(time.Minute))
			excluded := reviewRepairRecord(t, signed(t, a, bad), time.Now().Add(time.Minute))
			fetches := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/v1/repair/inventory" {
					out := RepairInventoryResponse{Version: 2, SubscriptionSHA256: b.s.Subscription().Digest(), Entries: []RepairEntry{selected.Entry}}
					switch mode {
					case "unselected-inventory":
						out.Entries = []RepairEntry{excluded.Entry}
					case "downgrade":
						out.Version = 1
					case "wrong-digest":
						out.SubscriptionSHA256 = "bad"
					case "wrong-source-scope":
						out.SourceSubscription = nodePolicy("wendy.gas.v0")
						out.SourceSubscription.Site = "other"
					}
					writeJSON(w, 200, out)
				} else {
					fetches++
					writeJSON(w, 200, RepairFetchResponse{Version: 2, SubscriptionSHA256: b.s.Subscription().Digest(), Records: []RepairRecord{excluded}, Unavailable: []string{}})
				}
			}))
			defer server.Close()
			if err := b.n.Sync(context.Background(), Peer{server.URL, "origin"}, b.credentials); err == nil {
				t.Fatal("bad peer accepted")
			}
			if mode != "unsolicited-fetch" && fetches != 0 {
				t.Fatal("invalid inventory caused data fetch")
			}
			records, _ := b.s.Records()
			if len(records) != 0 || len(b.n.proofs) != 0 {
				t.Fatal("bad peer wrote data")
			}
		})
	}
}

func TestSelectiveExpiryMetadataAuthorityAndUnsignedAge(t *testing.T) {
	a := newFixture(t, nil, "origin")
	b := subscribedFixture(t, a.root, "target", 16, nodePolicy("wendy.gas.v0"))
	row := component(a, 1, 1)
	proof := signed(t, a, row)
	expired := reviewRepairRecord(t, proof, time.Now().Add(-time.Second))
	if _, err := b.n.applyRepairPage([]RepairRecord{expired}); err != nil {
		t.Fatal(err)
	}
	subscribedRestart(t, b, nodePolicy("wendy.gas.v0"))
	if len(query(t, b).Components) != 0 {
		t.Fatal("expired selected record revived")
	}
	later := reviewRepairRecord(t, proof, time.Now().Add(time.Minute))
	if _, err := b.n.applyRepairPage([]RepairRecord{later}); err != nil {
		t.Fatal(err)
	}
	inventory, err := b.n.localRepairInventory()
	if err != nil || !inventory[row.Key].ExpiresAt.Equal(expired.Entry.ExpiresAt) {
		t.Fatal("repair extended expiry", err)
	}
	metadata := siteComponent(t, a)
	if _, err := b.n.applyRepairPage([]RepairRecord{reviewRepairRecord(t, signed(t, a, metadata), metadata.Timing.SampleTime.Add(metadata.TTL))}); err == nil {
		t.Fatal("mandatory metadata bypassed operator authority")
	}
}

func TestSelectiveReplicaStillAdmitsSignedStopDuringBulkRepair(t *testing.T) {
	f, operator, _, _, _ := taskFixture(t)
	f.server.Close()
	f.s.Close()
	cfg := f.n.cfg
	cfg.ProofPath = filepath.Join(t.TempDir(), "proofs.json")
	var err error
	f.s, err = store.OpenSubscribed(filepath.Join(filepath.Dir(cfg.ProofPath), "store.json"), f.wc, nodePolicy("wendy.gas.v0"))
	if err != nil {
		t.Fatal(err)
	}
	f.n, err = New(cfg, f.s, f.v)
	if err != nil {
		t.Fatal(err)
	}
	f.cfg = cfg
	f.start(t)
	spec := taskSpec("stop-partial-replica")
	if code, raw := call(t, f, operator, "POST", "/v1/tasks", "mission", instructionBody(t, f, operator, "mission", spec)); code != 200 {
		t.Fatalf("mission: %d %s", code, raw)
	}
	body := &reviewBlockedBody{entered: make(chan struct{}), release: make(chan struct{})}
	done := make(chan struct{})
	req := httptest.NewRequest("POST", "/v1/repair/inventory", nil)
	req.Body = body
	go func() { defer close(done); f.n.Handler().ServeHTTP(httptest.NewRecorder(), req) }()
	defer func() { close(body.release); <-done }()
	select {
	case <-body.entered:
	case <-time.After(2 * time.Second):
		t.Fatal("bulk did not start")
	}
	stop := instructionBody(t, f, operator, "stop", StopRequest{TaskID: spec.ID})
	request := httptest.NewRequest("POST", "/v1/tasks/stop", bytes.NewReader(stop))
	if err := AuthorizeRequest(request, operator, f.audience, "stop", stop); err != nil {
		t.Fatal(err)
	}
	response := httptest.NewRecorder()
	stopped := make(chan struct{})
	go func() { f.n.Handler().ServeHTTP(response, request); close(stopped) }()
	select {
	case <-stopped:
	case <-time.After(2 * time.Second):
		t.Fatal("bulk blocked stop")
	}
	var result tasking.Record
	if response.Code != 200 || json.Unmarshal(response.Body.Bytes(), &result) != nil || result.Status.Phase != mission.Canceled {
		t.Fatalf("stop failed: %d %s", response.Code, response.Body.String())
	}
}

func selectiveCrashChild(t *testing.T, dir, phase string) {
	var input repairReviewCrashFixture
	raw, err := os.ReadFile(filepath.Join(dir, "selective-child.json"))
	if err != nil || json.Unmarshal(raw, &input) != nil {
		t.Fatal("child fixture", err)
	}
	wc := world.Config{Tenant: "tenant", Site: "office", ClockDomain: "simulation:live", Now: time.Now, MaxEntries: 8, MaxFutureSkew: time.Second, AllowSynthetic: true, Frames: []world.FrameRef{{ID: "floor", Revision: "1"}}}
	path := filepath.Join(dir, "store.json")
	s, err := store.OpenSubscribed(path, wc, nodePolicy("wendy.gas.v0"))
	if err != nil {
		t.Fatal(err)
	}
	v, err := authority.NewVerifier(authority.Config{RootPublicKey: input.Root, Tenant: "tenant", Site: "office", Domain: "simulation", Audience: "target", Now: time.Now, Replay: authority.NewMemoryReplay(1000)})
	if err != nil {
		t.Fatal(err)
	}
	if err = v.UpdateRevocations(input.Revocations); err != nil {
		t.Fatal(err)
	}
	n, err := New(Config{Tenant: "tenant", Site: "office", ProofPath: filepath.Join(dir, "proofs.json"), MaxRecords: 8}, s, v)
	if err != nil {
		t.Fatal(err)
	}
	if phase == "proof-before-world" {
		if err = os.Rename(path, path+".previous"); err != nil {
			t.Fatal(err)
		}
		if err = os.Mkdir(path, 0700); err != nil {
			t.Fatal(err)
		}
		if result, err := n.applyRepairPage(input.Page); err == nil || result != nil {
			t.Fatal("failed write admitted page")
		}
		if _, err = s.Records(); !errors.Is(err, store.ErrFaulted) {
			t.Fatal("writer failure not latched", err)
		}
	} else {
		if result, err := n.applyRepairPage(input.Page); err != nil || len(result) != 8 {
			t.Fatal("page not committed", err)
		}
	}
	os.Exit(73)
}
func TestSelectiveSignedPageCrashRecovery(t *testing.T) {
	if dir := os.Getenv("WORLD_SELECTIVE_CRASH_DIR"); dir != "" {
		selectiveCrashChild(t, dir, os.Getenv("WORLD_SELECTIVE_CRASH_PHASE"))
		return
	}
	for _, phase := range []string{"proof-before-world", "after-world"} {
		t.Run(phase, func(t *testing.T) {
			a := newFixture(t, nil, "origin")
			b := subscribedFixture(t, a.root, "target", 8, nodePolicy("wendy.gas.v0"))
			oldPage, newPage := []RepairRecord{}, []RepairRecord{}
			for i := 0; i < 8; i++ {
				row := component(a, 1, 1)
				row.Key.Entity = fmt.Sprint(i)
				cap := row.Timing.SampleTime.Add(row.TTL)
				oldPage = append(oldPage, reviewRepairRecord(t, signed(t, a, row), cap))
				row.Version.Sequence = 2
				row.Tombstone = true
				row.Payload = nil
				newPage = append(newPage, reviewRepairRecord(t, signed(t, a, row), cap))
			}
			if _, err := b.n.applyRepairPage(oldPage); err != nil {
				t.Fatal(err)
			}
			now := time.Now()
			rev, err := authority.SignRevocations(authority.Revocations{Version: 1, Tenant: "tenant", Site: "office", Domain: "simulation", Revision: 1, IssuedAtUnixMS: now.Add(-time.Second).UnixMilli(), ExpiresAtUnixMS: now.Add(30 * time.Minute).UnixMilli()}, a.root)
			if err != nil {
				t.Fatal(err)
			}
			dir := filepath.Dir(b.cfg.ProofPath)
			fixture := repairReviewCrashFixture{Root: a.root.Public().(ed25519.PublicKey), Revocations: rev, Page: newPage}
			if err = os.WriteFile(filepath.Join(dir, "selective-child.json"), jsonBody(t, fixture), 0600); err != nil {
				t.Fatal(err)
			}
			b.server.Close()
			b.s.Close()
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			command := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestSelectiveSignedPageCrashRecovery$")
			command.Env = append(os.Environ(), "WORLD_SELECTIVE_CRASH_DIR="+dir, "WORLD_SELECTIVE_CRASH_PHASE="+phase)
			output, err := command.CombinedOutput()
			var exit *exec.ExitError
			if ctx.Err() != nil || !errors.As(err, &exit) || exit.ExitCode() != 73 {
				t.Fatalf("child: %v %s", err, output)
			}
			if phase == "proof-before-world" {
				path := filepath.Join(dir, "store.json")
				if err = os.Remove(path); err != nil {
					t.Fatal(err)
				}
				if err = os.Rename(path+".previous", path); err != nil {
					t.Fatal(err)
				}
			}
			subscribedRestart(t, b, nodePolicy("wendy.gas.v0"))
			records, err := b.s.Records()
			if err != nil || len(records) != 8 {
				t.Fatal("crash lost selected records", err)
			}
			for _, row := range records {
				if row.Tombstone != (phase == "after-world") {
					t.Fatal("partial world commit")
				}
			}
			if _, err = b.n.applyRepairPage(newPage); err != nil {
				t.Fatal("recovery could not complete repair", err)
			}
			if _, err = b.n.applyRepairPage(oldPage); err != nil {
				t.Fatal(err)
			}
			if len(query(t, b).Components) != 0 {
				t.Fatal("stale replay resurrected deletion")
			}
		})
	}
}

// This optional recording path executes the built CLI against the same real
// authenticated HTTP nodes as the acceptance test. Credentials stay in TempDir.
func selectiveCLIRecorder(t *testing.T) func(string, *fixture) {
	t.Helper()
	binary, output := os.Getenv("WORLD_SUBSCRIPTION_DEMO_BINARY"), os.Getenv("WORLD_SUBSCRIPTION_DEMO_TRANSCRIPT")
	if binary == "" && output == "" {
		return func(string, *fixture) {}
	}
	if binary == "" || output == "" {
		t.Fatal("both selective demo environment variables are required")
	}
	type step struct {
		Phase    string          `json:"phase"`
		Command  string          `json:"command"`
		Response json.RawMessage `json:"response"`
	}
	transcript := struct {
		Scenario  string `json:"scenario"`
		Simulated bool   `json:"simulated"`
		Steps     []step `json:"steps"`
	}{Scenario: "Actual world CLI; three authenticated local nodes, 200 signed synthetic sensor records and one site metadata record; HTTP partition and durable gas-node restart", Simulated: true}
	t.Cleanup(func() {
		if t.Failed() {
			return
		}
		raw, err := json.MarshalIndent(transcript, "", "  ")
		if err != nil {
			t.Error(err)
			return
		}
		if err = os.WriteFile(output, append(raw, '\n'), 0600); err != nil {
			t.Error(err)
		}
	})
	invoke := func(phase, display string, args ...string) json.RawMessage {
		t.Helper()
		command := exec.Command(binary, args...)
		var stdout, stderr bytes.Buffer
		command.Stdout = &stdout
		command.Stderr = &stderr
		if err := command.Run(); err != nil {
			t.Fatalf("CLI %s: %v %s", phase, err, stderr.String())
		}
		if !json.Valid(stdout.Bytes()) {
			t.Fatal("CLI returned invalid JSON")
		}
		response := json.RawMessage(bytes.Clone(stdout.Bytes()))
		transcript.Steps = append(transcript.Steps, step{phase, display, response})
		return response
	}
	dir := t.TempDir()
	selectionPath := filepath.Join(dir, "gas.json")
	if err := os.WriteFile(selectionPath, []byte(`{"types":["wendy.gas.v0"]}`), 0600); err != nil {
		t.Fatal(err)
	}
	invoke("create explicit immutable policy", "world replica policy --tenant tenant --site office --selection gas.json --json", "replica", "policy", "--tenant", "tenant", "--site", "office", "--selection", selectionPath, "--json")
	return func(phase string, f *fixture) {
		t.Helper()
		credentials := filepath.Join(dir, "reader.json")
		raw, err := json.Marshal(client.Config{URL: f.server.URL, Audience: f.audience, Grant: f.credentials.Grant, PrivateKey: f.credentials.PrivateKey, AllowInsecureSimulation: true})
		if err != nil {
			t.Fatal(err)
		}
		if err = os.WriteFile(credentials, raw, 0600); err != nil {
			t.Fatal(err)
		}
		receipt := invoke(phase, "world replica status --credentials $READER --json", "replica", "status", "--credentials", credentials, "--json")
		var status subscription.Status
		if err := json.Unmarshal(receipt, &status); err != nil {
			t.Fatal(err)
		}
		records, err := f.s.Records()
		if err != nil || status.RetainedRecords != len(records) || status.PolicySHA256 != f.s.Subscription().Digest() {
			t.Fatalf("CLI status differs from durable state: %+v %v", status, err)
		}
	}
}
