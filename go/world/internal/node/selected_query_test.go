package node

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/wendylabsinc/wendy/go/world/internal/store"
	"io"
	"net/http"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/world/internal/authority"
	"github.com/wendylabsinc/wendy/go/world/internal/client"
	"github.com/wendylabsinc/wendy/go/world/internal/selection"
	"github.com/wendylabsinc/wendy/go/world/internal/world"
)

func selectedCall(t *testing.T, f *fixture, credentials Credentials, path string, request any) (int, selection.Response, []byte) {
	t.Helper()
	body, err := json.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	status, raw := call(t, f, credentials, "POST", path, "read", body)
	var out selection.Response
	if status == 200 {
		if err := json.Unmarshal(raw, &out); err != nil {
			t.Fatal(err)
		}
	}
	return status, out, raw
}
func selectedOK(t *testing.T, f *fixture, request selection.Request) selection.Response {
	t.Helper()
	status, out, raw := selectedCall(t, f, f.credentials, "/v1/query", request)
	if status != 200 {
		t.Fatalf("selected query: %d %s", status, raw)
	}
	normalized, err := request.Normalize()
	if err != nil {
		t.Fatal(err)
	}
	if err := selection.ValidateResponse(out, normalized); err != nil {
		t.Fatal(err)
	}
	return out
}
func selectedPopulate(t *testing.T, f *fixture, count int) (uint64, []world.Component) {
	t.Helper()
	epoch := allocate(t, f)
	rows := make([]world.Component, count)
	for i := range rows {
		rows[i] = component(f, epoch, uint64(i+1))
		rows[i].Key.Entity = fmt.Sprintf("entity-%03d", i)
		if i%2 == 1 {
			rows[i].Key.Type = "wendy.temperature.v0"
		}
		publish(t, f, rows[i])
	}
	return epoch, rows
}
func TestSelectedQueryPagesFiltersAndUnrelatedUpdates(t *testing.T) {
	f := newFixture(t, nil, "selected")
	epoch, rows := selectedPopulate(t, f, 12)
	s := selection.Selection{Types: []string{"wendy.gas.v0"}, Sources: []world.Source{f.credentials.Grant.Grant.Source}, Frames: []world.FrameRef{{ID: "floor", Revision: "1"}}}
	request := selection.Request{Selection: s, PageSize: 2, MaxBytes: 8192}
	first := selectedOK(t, f, request)
	if first.Complete || len(first.Components) != 2 {
		t.Fatalf("first page: %+v", first)
	}
	unrelated := rows[1]
	unrelated.Version.Sequence = 100
	publish(t, f, unrelated)
	seen := map[string]bool{}
	page := first
	revision := first.Revision
	for {
		if page.Revision != revision {
			t.Fatal("selected revision changed on unrelated write")
		}
		for _, c := range page.Components {
			if seen[c.Key.Entity] {
				t.Fatal("duplicate page entry")
			}
			seen[c.Key.Entity] = true
			if c.Key.Type != "wendy.gas.v0" || c.Provenance.Simulation.RunID != "test" {
				t.Fatal("selection/provenance changed")
			}
		}
		if page.Complete {
			break
		}
		request.Cursor = page.NextCursor
		page = selectedOK(t, f, request)
	}
	if len(seen) != 6 {
		t.Fatalf("missing selected entries: %v", seen)
	}
	update := rows[0]
	update.Version.Sequence = 101
	update.Version.Epoch = epoch
	publish(t, f, update)
	request.Cursor = first.NextCursor
	status, _, raw := selectedCall(t, f, f.credentials, "/v1/query", request)
	if status != 409 {
		t.Fatalf("stale cursor: %d %s", status, raw)
	}
	if len(query(t, f).Components) != 12 {
		t.Fatal("legacy whole-replica query changed")
	}
	request.Cursor = ""
	request.Selection.Entities = []string{"entity-002"}
	if got := selectedOK(t, f, request); len(got.Components) != 1 || got.Components[0].Key.Entity != "entity-002" {
		t.Fatal("AND selection")
	}
}
func TestSelectedQueryByteBoundsAndTombstone(t *testing.T) {
	f := newFixture(t, nil, "bytes")
	_, rows := selectedPopulate(t, f, 8)
	request := selection.Request{PageSize: 8, MaxBytes: 4096}
	first := selectedOK(t, f, request)
	if first.Complete || len(first.Components) == 0 || len(first.Components) >= 8 {
		t.Fatalf("byte-bound page: %d", len(first.Components))
	}
	update := rows[0]
	update.Version.Sequence = 100
	update.Tombstone = true
	update.Payload = nil
	publish(t, f, update)
	request.Cursor = first.NextCursor
	status, _, _ := selectedCall(t, f, f.credentials, "/v1/query", request)
	if status != 409 {
		t.Fatalf("tombstone retained old cursor: %d", status)
	}
	huge := rows[2]
	huge.Version.Sequence = 101
	huge.Payload = json.RawMessage(`{"data":"` + string(bytes.Repeat([]byte("x"), 6000)) + `"}`)
	publish(t, f, huge)
	request.Cursor = ""
	request.Selection.Entities = []string{huge.Key.Entity}
	status, _, raw := selectedCall(t, f, f.credentials, "/v1/query", request)
	if status != 413 {
		t.Fatalf("oversized record: %d %s", status, raw)
	}
}
func TestSelectedCursorBindingExpiryAndRestart(t *testing.T) {
	f := newFixture(t, nil, "cursor")
	selectedPopulate(t, f, 4)
	r := selection.Request{PageSize: 1}
	first := selectedOK(t, f, r)
	for _, test := range []struct {
		name        string
		cursor      string
		selection   selection.Selection
		credentials Credentials
	}{
		{"tamper", first.NextCursor + "x", selection.Selection{}, f.credentials},
		{"purpose", first.Revision, selection.Selection{}, f.credentials},
		{"filter", first.NextCursor, selection.Selection{Entities: []string{"entity-001"}}, f.credentials},
		{"reader", first.NextCursor, selection.Selection{}, issueCredentials(t, f.root, "other-reader")},
	} {
		t.Run(test.name, func(t *testing.T) {
			r := selection.Request{PageSize: 1, Cursor: test.cursor, Selection: test.selection}
			status, _, raw := selectedCall(t, f, test.credentials, "/v1/query", r)
			if status != 400 {
				t.Fatalf("%d %s", status, raw)
			}
		})
	}
	raw, _ := base64.RawURLEncoding.DecodeString(first.NextCursor)
	var c selectedCursor
	if err := json.Unmarshal(raw[:len(raw)-32], &c); err != nil {
		t.Fatal(err)
	}
	c.Expires = time.Now().Add(-time.Second).UnixMilli()
	expired, _ := f.n.encodeSelected(c)
	r.Cursor = expired
	status, _, _ := selectedCall(t, f, f.credentials, "/v1/query", r)
	if status != 410 {
		t.Fatalf("expiry: %d", status)
	}
	f.server.Close()
	var err error
	f.n, err = New(f.cfg, f.s, f.v)
	if err != nil {
		t.Fatal(err)
	}
	f.start(t)
	r.Cursor = first.NextCursor
	status, _, _ = selectedCall(t, f, f.credentials, "/v1/query", r)
	if status != 400 {
		t.Fatalf("restart cursor accepted: %d", status)
	}
	r.Cursor = ""
	if len(selectedOK(t, f, r).Components) != 1 {
		t.Fatal("fresh query after restart failed")
	}
}
func TestSelectedRequestBodyIsSigned(t *testing.T) {
	f := newFixture(t, nil, "binding")
	selectedPopulate(t, f, 2)
	original := []byte(`{"version":1,"selection":{"entities":["entity-000"]}}`)
	modified := []byte(`{"version":1,"selection":{}}`)
	req, err := http.NewRequest("POST", f.server.URL+"/v1/query", bytes.NewReader(original))
	if err != nil {
		t.Fatal(err)
	}
	if err := AuthorizeRequest(req, f.credentials, f.audience, "read", original); err != nil {
		t.Fatal(err)
	}
	req.Body = io.NopCloser(bytes.NewReader(modified))
	req.ContentLength = int64(len(modified))
	response, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != 401 {
		t.Fatalf("unsigned selection change accepted: %d", response.StatusCode)
	}
}
func selectedClient(t *testing.T, f *fixture, creds Credentials) *client.Client {
	t.Helper()
	c, err := client.New(client.Config{URL: f.server.URL, Audience: f.audience, Grant: creds.Grant, PrivateKey: creds.PrivateKey, AllowInsecureSimulation: true})
	if err != nil {
		t.Fatal(err)
	}
	return c
}
func selectedWaitActive(t *testing.T, f *fixture) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if f.n.selectedWatchers.Load() == 1 {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("watch did not start")
}
func TestSelectedWatchChangeTimeoutCancellationAndCapacity(t *testing.T) {
	f := newFixture(t, nil, "watch")
	_, rows := selectedPopulate(t, f, 4)
	c := selectedClient(t, f, f.credentials)
	filter := selection.Selection{Types: []string{"wendy.gas.v0"}}
	first := selectedOK(t, f, selection.Request{Selection: filter})
	// An unrelated update leaves the selected watch waiting until its requested timeout.
	other := rows[1]
	other.Version.Sequence = 100
	publish(t, f, other)
	start := time.Now()
	out, err := c.WatchSelected(context.Background(), selection.WatchRequest{Selection: filter, Revision: first.Revision, WaitMS: 100})
	if err != nil || !out.TimedOut || out.Revision != first.Revision || time.Since(start) < 90*time.Millisecond {
		t.Fatalf("timeout: %+v %v", out, err)
	}
	result := make(chan selection.Response, 1)
	fail := make(chan error, 1)
	go func() {
		out, err := c.WatchSelected(context.Background(), selection.WatchRequest{Selection: filter, Revision: first.Revision, WaitMS: 2000})
		result <- out
		fail <- err
	}()
	selectedWaitActive(t, f)
	changed := rows[0]
	changed.Version.Sequence = 101
	publish(t, f, changed)
	out = <-result
	if err := <-fail; err != nil || out.TimedOut || out.Revision == first.Revision {
		t.Fatalf("change: %+v %v", out, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := c.WatchSelected(ctx, selection.WatchRequest{Selection: filter, Revision: out.Revision, WaitMS: 5000})
		done <- err
	}()
	selectedWaitActive(t, f)
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("cancel: %v", err)
	}
	deadline := time.Now().Add(time.Second)
	for f.n.selectedWatchers.Load() != 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if f.n.selectedWatchers.Load() != 0 {
		t.Fatal("watch leaked capacity")
	}
	f.n.selectedWatchers.Store(maxSelectedWatchers)
	status, _, _ := selectedCall(t, f, f.credentials, "/v1/watch/selected", selection.WatchRequest{})
	f.n.selectedWatchers.Store(0)
	if status != 429 {
		t.Fatalf("capacity: %d", status)
	}
}
func revokeSelected(t *testing.T, f *fixture, id string) {
	t.Helper()
	now := time.Now()
	rev, err := authority.SignRevocations(authority.Revocations{Version: 1, Tenant: "tenant", Site: "office", Domain: "simulation", Revision: 2, IssuedAtUnixMS: now.UnixMilli(), ExpiresAtUnixMS: now.Add(20 * time.Minute).UnixMilli(), GrantIDs: []string{id}}, f.root)
	if err != nil {
		t.Fatal(err)
	}
	f.n.lock()
	defer f.n.mu.Unlock()
	if err = f.v.UpdateRevocations(rev); err != nil {
		t.Fatal(err)
	}
}
func TestSelectedWatchReaderAndOriginRevocation(t *testing.T) {
	for _, readerRevoked := range []bool{true, false} {
		t.Run(fmt.Sprint(readerRevoked), func(t *testing.T) {
			f := newFixture(t, nil, "origin")
			selectedPopulate(t, f, 2)
			reader := issueCredentials(t, f.root, "reader")
			c := selectedClient(t, f, reader)
			first, err := c.QuerySelected(context.Background(), selection.Request{})
			if err != nil {
				t.Fatal(err)
			}
			done := make(chan error, 1)
			result := make(chan selection.Response, 1)
			go func() {
				out, err := c.WatchSelected(context.Background(), selection.WatchRequest{Revision: first.Revision, WaitMS: 2000})
				result <- out
				done <- err
			}()
			selectedWaitActive(t, f)
			id := f.credentials.Grant.Grant.ID
			if readerRevoked {
				id = reader.Grant.Grant.ID
			}
			revokeSelected(t, f, id)
			out := <-result
			err = <-done
			if readerRevoked {
				var status *client.HTTPStatusError
				if !errors.As(err, &status) || status.StatusCode != 401 {
					t.Fatalf("reader revocation: %v", err)
				}
			} else if err != nil || out.TimedOut || len(out.Components) != 0 || out.Revision == first.Revision {
				t.Fatalf("origin revocation: %+v %v", out, err)
			}
		})
	}
}
func TestSelectedExpiryAndFrameInvalidation(t *testing.T) {
	for _, mode := range []string{"expiry", "frame"} {
		t.Run(mode, func(t *testing.T) {
			f := newFixture(t, nil, "liveness")
			epoch := allocate(t, f)
			for i := 0; i < 3; i++ {
				c := component(f, epoch, uint64(i+1))
				c.Key.Entity = fmt.Sprint(i)
				if mode == "expiry" {
					c.TTL = 600 * time.Millisecond
				}
				publish(t, f, c)
			}
			first := selectedOK(t, f, selection.Request{PageSize: 1})
			c := selectedClient(t, f, f.credentials)
			if mode == "frame" {
				if err := f.s.SetFrame(world.FrameRef{ID: "floor", Revision: "2"}); err != nil {
					t.Fatal(err)
				}
			}
			out, err := c.WatchSelected(context.Background(), selection.WatchRequest{Revision: first.Revision, WaitMS: 2000})
			if err != nil || out.TimedOut || out.Revision == first.Revision {
				t.Fatalf("invalidation: %+v %v", out, err)
			}
			_, err = c.QuerySelected(context.Background(), selection.Request{Cursor: first.NextCursor, PageSize: 1})
			if !errors.Is(err, client.ErrSelectedViewChanged) {
				t.Fatalf("stale page: %v", err)
			}
		})
	}
}

func TestSelectedViewAcrossPartitionRepairAndDurableRestart(t *testing.T) {
	origin := newFixture(t, nil, "origin")
	replica := newFixture(t, origin.root, "replica")
	_, rows := selectedPopulate(t, origin, 4)
	syncPeer := func() {
		t.Helper()
		if err := replica.n.Sync(context.Background(), Peer{origin.server.URL, "origin"}, replica.credentials); err != nil {
			t.Fatal(err)
		}
	}
	syncPeer()
	first := selectedOK(t, replica, selection.Request{PageSize: 1})
	// Duplicate delivery cannot change selected membership or an expiry cap.
	syncPeer()
	selectedOK(t, replica, selection.Request{PageSize: 1, Cursor: first.NextCursor})
	origin.unavailable.Store(true)
	if err := replica.n.Sync(context.Background(), Peer{origin.server.URL, "origin"}, replica.credentials); err == nil {
		t.Fatal("partition not injected")
	}
	deleted := rows[0]
	deleted.Version.Sequence = 100
	deleted.Tombstone = true
	deleted.Payload = nil
	if _, err := origin.n.apply(signed(t, origin, deleted), true); err != nil {
		t.Fatal(err)
	}
	selectedOK(t, replica, selection.Request{PageSize: 1, Cursor: first.NextCursor})
	origin.unavailable.Store(false)
	syncPeer()
	status, _, _ := selectedCall(t, replica, replica.credentials, "/v1/query", selection.Request{PageSize: 1, Cursor: first.NextCursor})
	if status != 409 {
		t.Fatalf("repaired view retained cursor: %d", status)
	}
	replica.server.Close()
	if err := replica.s.Close(); err != nil {
		t.Fatal(err)
	}
	var err error
	replica.s, err = store.Open(filepath.Join(filepath.Dir(replica.cfg.ProofPath), "store.json"), replica.wc)
	if err != nil {
		t.Fatal(err)
	}
	replica.n, err = New(replica.cfg, replica.s, replica.v)
	if err != nil {
		t.Fatal(err)
	}
	replica.start(t)
	if _, err := replica.n.apply(signed(t, origin, rows[0]), false); err != nil {
		t.Fatal(err)
	}
	got := selectedOK(t, replica, selection.Request{})
	if len(got.Components) != 3 {
		t.Fatalf("restart/replay resurrected deletion: %+v", got)
	}
	for _, row := range got.Components {
		if row.Key.Entity == rows[0].Key.Entity {
			t.Fatal("deleted record visible")
		}
	}
}

func TestSelectedReadFailsClosedOnMissingProofAndStorage(t *testing.T) {
	for _, kind := range []string{"proof", "store"} {
		t.Run(kind, func(t *testing.T) {
			f := newFixture(t, nil, "failure")
			_, rows := selectedPopulate(t, f, 2)
			if kind == "proof" {
				d, _ := recordDigest(rows[0])
				f.n.lock()
				delete(f.n.proofs, d)
				f.n.mu.Unlock()
			} else {
				if err := f.s.Close(); err != nil {
					t.Fatal(err)
				}
			}
			status, _, raw := selectedCall(t, f, f.credentials, "/v1/query", selection.Request{})
			if status != 503 {
				t.Fatalf("partial success after %s failure: %d %s", kind, status, raw)
			}
		})
	}
}

// Move only the world's simulation clock inside origin verification. Authority
// remains on real time, so simulation expiry cannot change grant validity.
type selectedBoundaryReplay struct {
	authority.ReplayStore
	calls int
	hook  func() error
}

func (r *selectedBoundaryReplay) Checkpoint(revision uint64, digest string, now time.Time) error {
	r.calls++
	if r.calls == 3 && r.hook != nil {
		if err := r.hook(); err != nil {
			return err
		}
	}
	return r.ReplayStore.Checkpoint(revision, digest, now)
}
func TestSelectedReobservesExpiryFramesAndStorageAfterProofVerification(t *testing.T) {
	for _, kind := range []string{"expiry", "frame", "storage"} {
		t.Run(kind, func(t *testing.T) {
			f := newFixture(t, nil, "boundary")
			selectedPopulate(t, f, 1)
			f.server.Close()
			if err := f.s.Close(); err != nil {
				t.Fatal(err)
			}
			var clock atomic.Int64
			clock.Store(time.Now().UnixNano())
			f.wc.Now = func() time.Time { return time.Unix(0, clock.Load()) }
			var err error
			f.s, err = store.Open(filepath.Join(filepath.Dir(f.cfg.ProofPath), "store.json"), f.wc)
			if err != nil {
				t.Fatal(err)
			}
			replay := &selectedBoundaryReplay{ReplayStore: authority.NewMemoryReplay(100)}
			f.v, err = authority.NewVerifier(authority.Config{RootPublicKey: f.root.Public().(ed25519.PublicKey), Tenant: f.cfg.Tenant, Site: f.cfg.Site, Domain: "simulation", Audience: f.audience, Now: time.Now, Replay: replay})
			if err != nil {
				t.Fatal(err)
			}
			repairAuthorityRevocations(t, f, time.Now(), 1)
			f.n, err = New(f.cfg, f.s, f.v)
			if err != nil {
				t.Fatal(err)
			}
			replay.calls = 0
			replay.hook = func() error {
				switch kind {
				case "expiry":
					clock.Add(int64(10 * time.Minute))
					return nil
				case "frame":
					return f.s.SetFrame(world.FrameRef{ID: "floor", Revision: "2"})
				default:
					return f.s.Close()
				}
			}
			f.start(t)
			status, out, raw := selectedCall(t, f, f.credentials, "/v1/query", selection.Request{})
			if kind == "storage" {
				if status != 503 {
					t.Fatalf("storage failed after proof: %d %s", status, raw)
				}
			} else if status != 200 || len(out.Components) != 0 {
				t.Fatalf("stale record returned after %s change: %d %+v %s", kind, status, out, raw)
			}
			if replay.calls < 3 {
				t.Fatal("fault boundary not reached")
			}
		})
	}
}
