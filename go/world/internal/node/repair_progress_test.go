package node

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/world/internal/authority"
	"github.com/wendylabsinc/wendy/go/world/internal/store"
	"github.com/wendylabsinc/wendy/go/world/internal/world"
)

type progressRequest struct {
	Path, Cursor string
	Keys         []string
}

type progressRemote struct {
	mu        sync.Mutex
	handler   http.Handler
	remaining int // -1 permits every fetch; zero interrupts the next fetch.
	fault     string
	requests  []progressRequest
}

func (p *progressRemote) configure(handler http.Handler, remaining int, fault string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if handler != nil {
		p.handler = handler
	}
	p.remaining, p.fault, p.requests = remaining, fault, nil
}

func (p *progressRemote) captured() []progressRequest {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]progressRequest(nil), p.requests...)
}

func (p *progressRemote) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, MaxRepairPageBytes+1))
	if err != nil || len(body) > MaxRepairPageBytes {
		http.Error(w, "bounded fixture request", http.StatusBadRequest)
		return
	}
	r.Body = io.NopCloser(bytes.NewReader(body))
	seen := progressRequest{Path: r.URL.Path}
	if r.URL.Path == "/v1/repair/inventory" {
		var request RepairInventoryRequest
		if json.Unmarshal(body, &request) != nil {
			http.Error(w, "invalid fixture inventory", 400)
			return
		}
		seen.Cursor = request.Cursor
	} else if r.URL.Path == "/v1/repair/fetch" {
		var request RepairFetchRequest
		if json.Unmarshal(body, &request) != nil {
			http.Error(w, "invalid fixture fetch", 400)
			return
		}
		for _, entry := range request.Entries {
			seen.Keys = append(seen.Keys, repairKeyDigest(entry.Key))
		}
	}
	p.mu.Lock()
	p.requests = append(p.requests, seen)
	handler, fault, interrupt := p.handler, p.fault, false
	if r.URL.Path == "/v1/repair/fetch" && p.remaining >= 0 {
		interrupt = p.remaining == 0
		if !interrupt {
			p.remaining--
		}
	}
	p.mu.Unlock()
	if interrupt {
		if fault == "disconnect" {
			connection, _, err := w.(http.Hijacker).Hijack()
			if err == nil {
				_ = connection.Close()
			}
			return
		}
		http.Error(w, "temporary fixture interruption", http.StatusTooManyRequests)
		return
	}
	handler.ServeHTTP(w, r)
}

func progressFixture(t *testing.T, root ed25519.PrivateKey, name string) *fixture {
	t.Helper()
	f := newFixture(t, root, name)
	f.cfg.MaxRecords, f.wc.MaxEntries = 128, 128
	reopenPublication(t, f)
	return f
}

func progressSeed(t *testing.T, f *fixture, count int) []world.Component {
	t.Helper()
	components := make([]world.Component, count)
	for i := range components {
		components[i] = component(f, 1, 1)
		components[i].Key.Entity = fmt.Sprintf("progress-%03d", i)
	}
	progressPublish(t, f, components)
	sort.Slice(components, func(i, j int) bool { return repairKeyDigest(components[i].Key) < repairKeyDigest(components[j].Key) })
	return components
}

func progressPublish(t *testing.T, f *fixture, components []world.Component) {
	t.Helper()
	for offset := 0; offset < len(components); offset += store.MaxRepairRecords {
		page := []RepairRecord{}
		for i := offset; i < len(components) && i < offset+store.MaxRepairRecords; i++ {
			c := components[i]
			page = append(page, reviewRepairRecord(t, signed(t, f, c), c.Timing.SampleTime.Add(c.TTL)))
		}
		if _, err := f.n.applyRepairPage(page); err != nil {
			t.Fatal(err)
		}
	}
}

func progressSync(f *fixture, peer Peer, credentials Credentials) error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return f.n.Sync(ctx, peer, credentials)
}

func TestRepairProgressResumesLaterKeysAcrossInterruptedRounds(t *testing.T) {
	for _, fault := range []string{"429", "disconnect"} {
		t.Run(fault, func(t *testing.T) {
			origin := progressFixture(t, nil, "progress-origin")
			receiver := progressFixture(t, origin.root, "progress-receiver")
			const count = MaxRepairInventoryEntries + 3
			components := progressSeed(t, origin, count)
			remote := &progressRemote{handler: origin.n.Handler()}
			server := httptest.NewServer(remote)
			defer server.Close()
			peer := Peer{URL: server.URL, Audience: origin.audience}
			sawContinuation := false
			for round := 0; round < count/store.MaxRepairRecords+3; round++ {
				remote.configure(nil, 1, fault)
				err := progressSync(receiver, peer, receiver.credentials)
				for _, request := range remote.captured() {
					if request.Path == "/v1/repair/inventory" && request.Cursor != "" {
						sawContinuation = true
					}
				}
				retained := publicationCheckpoint(t, receiver)
				if len(retained.Entries) == count {
					if err != nil {
						t.Fatalf("complete repair returned error: %v", err)
					}
					break
				}
				if err == nil {
					t.Fatal("interrupted transfer claimed completion")
				}
				want := (round + 1) * store.MaxRepairRecords
				if len(retained.Entries) != want {
					t.Fatalf("later keys starved at round%d: retained=%d want=%d", round, len(retained.Entries), want)
				}
				// Repeatedly changing the earliest keys must not send every retry
				// back through that same prefix while later keys remain missing.
				for i := 0; i < store.MaxRepairRecords; i++ {
					components[i].Version.Sequence++
					components[i].Timing.SampleTime = time.Now()
				}
				progressPublish(t, origin, components[:store.MaxRepairRecords])
			}
			if got := len(publicationCheckpoint(t, receiver).Entries); got != count || !sawContinuation {
				t.Fatalf("did not reach the second inventory page: records=%d continuation=%v", got, sawContinuation)
			}
			if len(receiver.n.repairProgress) != 0 {
				t.Fatal("completed cycle retained cached transfer state")
			}
			remote.configure(nil, -1, "")
			if err := progressSync(receiver, peer, receiver.credentials); err != nil {
				t.Fatal(err)
			}
			requests := remote.captured()
			if len(requests) == 0 || requests[0].Path != "/v1/repair/inventory" || requests[0].Cursor != "" {
				t.Fatal("completed cycle reused an old continuation")
			}
			retained := map[world.Key]world.Component{}
			for _, entry := range publicationCheckpoint(t, receiver).Entries {
				retained[entry.Component.Key] = entry.Component
			}
			for _, want := range components {
				if retained[want.Key].Version != want.Version {
					t.Fatal("fresh cycle did not catch up the changed prefix")
				}
			}
		})
	}
}

func TestRepairProgressScopeIsolation(t *testing.T) {
	for _, change := range []string{"url", "audience", "grant", "key", "principal"} {
		t.Run(change, func(t *testing.T) {
			origin := progressFixture(t, nil, "scope-origin")
			receiver := progressFixture(t, origin.root, "scope-receiver")
			progressSeed(t, origin, 17)
			remote := &progressRemote{handler: origin.n.Handler()}
			remote.configure(nil, 1, "429")
			server := httptest.NewServer(remote)
			defer server.Close()
			peer, credentials := Peer{URL: server.URL, Audience: origin.audience}, receiver.credentials
			if err := progressSync(receiver, peer, credentials); err == nil || len(publicationCheckpoint(t, receiver).Entries) != 8 {
				t.Fatal("missing pending original scope")
			}
			remote.configure(nil, -1, "")
			g := credentials.Grant.Grant
			switch change {
			case "url":
				other := httptest.NewServer(remote)
				defer other.Close()
				peer.URL = other.URL
			case "audience":
				other := progressFixture(t, origin.root, "different-audience")
				remote.configure(other.n.Handler(), -1, "")
				peer.Audience = other.audience
			case "grant":
				g.ID += "/new"
			case "key":
				pub, private, err := ed25519.GenerateKey(rand.Reader)
				if err != nil {
					t.Fatal(err)
				}
				g.PublicKey, credentials.PrivateKey = pub, private
			case "principal":
				g.Principal += "/new"
			}
			var err error
			credentials.Grant, err = authority.SignGrant(g, origin.root)
			if err != nil {
				t.Fatal(err)
			}
			if err := progressSync(receiver, peer, credentials); err != nil {
				t.Fatal(err)
			}
			requests := remote.captured()
			if len(requests) == 0 || requests[0].Path != "/v1/repair/inventory" || requests[0].Cursor != "" {
				t.Fatal("different target or signed credential borrowed pending hints")
			}
		})
	}
}

func TestRepairProgressRechecksOriginAuthorityOnResume(t *testing.T) {
	origin := progressFixture(t, nil, "authorized-progress-origin")
	receiver := progressFixture(t, origin.root, "authorized-receiver")
	revoked := issueCredentials(t, origin.root, "revoked-middle-origin")
	page := []RepairRecord{}
	var deniedKey world.Key
	// Hash ordering makes the newly denied source one member of the middle
	// eight-record chunk, with valid neighbors before and after that chunk.
	for _, group := range []struct {
		name        string
		low, high   byte
		count       int
		credentials Credentials
	}{{"prefix", '0', '4', 8, origin.credentials}, {"middle-valid", '4', '8', 7, origin.credentials},
		{"middle-denied", '4', '8', 1, revoked}, {"suffix", 'c', 'g', 8, origin.credentials}} {
		found := 0
		for candidate := 0; found < group.count && candidate < 1000; candidate++ {
			c := component(origin, 1, 1)
			c.Key.Entity, c.Key.Source = fmt.Sprintf("%s-%d", group.name, candidate), group.credentials.Grant.Grant.Source
			order := repairKeyDigest(c.Key)[0]
			if order < group.low || order >= group.high {
				continue
			}
			record, err := authority.SignRecord(group.credentials.Grant, group.credentials.PrivateKey, c, time.Now())
			if err != nil {
				t.Fatal(err)
			}
			page = append(page, reviewRepairRecord(t, record, c.Timing.SampleTime.Add(c.TTL)))
			if group.name == "middle-denied" {
				deniedKey = c.Key
			}
			found++
		}
		if found != group.count {
			t.Fatal("could not construct bounded ordered fixture")
		}
	}
	sort.Slice(page, func(i, j int) bool { return repairKeyDigest(page[i].Entry.Key) < repairKeyDigest(page[j].Entry.Key) })
	for offset := 0; offset < len(page); offset += store.MaxRepairRecords {
		if _, err := origin.n.applyRepairPage(page[offset : offset+store.MaxRepairRecords]); err != nil {
			t.Fatal(err)
		}
	}
	remote := &progressRemote{handler: origin.n.Handler()}
	remote.configure(nil, 1, "429")
	server := httptest.NewServer(remote)
	defer server.Close()
	peer := Peer{URL: server.URL, Audience: origin.audience}
	if err := progressSync(receiver, peer, receiver.credentials); err == nil || len(publicationCheckpoint(t, receiver).Entries) != 8 {
		t.Fatal("missing pending page before revocation")
	}
	repairAuthorityRevocations(t, receiver, time.Now(), 2, revoked.Grant.Grant.Principal)
	// Interrupt again after the denied middle chunk has been retried as eight
	// independent singleton pages. The later call must retain that denial in
	// its completion result while still reaching the authorized suffix.
	remote.configure(nil, 9, "429")
	var status repairHTTPError
	if err := progressSync(receiver, peer, receiver.credentials); !errors.As(err, &status) || status.Status != http.StatusTooManyRequests {
		t.Fatalf("did not reach the interrupted suffix: %v", err)
	}
	if len(publicationCheckpoint(t, receiver).Entries) != 15 {
		t.Fatal("denied chunk partially admitted or starved its authorized neighbors")
	}
	requests := remote.captured()
	remote.configure(nil, -1, "")
	if err := progressSync(receiver, peer, receiver.credentials); !errors.Is(err, authority.ErrDenied) {
		t.Fatalf("completion did not report the current origin denial: %v", err)
	}
	retained := publicationCheckpoint(t, receiver)
	if len(retained.Entries) != 23 {
		t.Fatalf("denied origin starved valid neighbors or suffix: retained=%d want23", len(retained.Entries))
	}
	for _, entry := range retained.Entries {
		if entry.Component.Key == deniedKey {
			t.Fatal("revoked pending record entered the durable replica")
		}
	}
	fetchSizes := []int{}
	requests = append(requests, remote.captured()...)
	for _, request := range requests {
		if request.Path == "/v1/repair/fetch" {
			fetchSizes = append(fetchSizes, len(request.Keys))
		}
	}
	if len(fetchSizes) != 11 || fetchSizes[0] != 8 || fetchSizes[9] != 8 || fetchSizes[10] != 8 {
		t.Fatalf("failed page did not become independent bounded requests: %v", fetchSizes)
	}
	for _, size := range fetchSizes[1:9] {
		if size != 1 {
			t.Fatal("valid members bypassed singleton page admission")
		}
	}
	if len(receiver.n.repairProgress) != 0 {
		t.Fatal("completed traversal with one refused origin retained a cycle")
	}
}

func TestRepairProgressStaleCursorStartsNewCycleOnNextCall(t *testing.T) {
	origin := progressFixture(t, nil, "restarted-origin")
	receiver := progressFixture(t, origin.root, "cursor-receiver")
	progressSeed(t, origin, MaxRepairInventoryEntries+3)
	var mu sync.Mutex
	refused := false
	cursors := []string{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/repair/inventory" {
			body, _ := io.ReadAll(io.LimitReader(r.Body, 4096))
			r.Body = io.NopCloser(bytes.NewReader(body))
			var request RepairInventoryRequest
			if json.Unmarshal(body, &request) != nil {
				http.Error(w, "invalid fixture request", 400)
				return
			}
			mu.Lock()
			cursors = append(cursors, request.Cursor)
			reject := request.Cursor != "" && !refused
			refused = refused || reject
			mu.Unlock()
			if reject {
				http.Error(w, "cursor no longer valid", http.StatusBadRequest)
				return
			}
		}
		origin.n.Handler().ServeHTTP(w, r)
	}))
	defer server.Close()
	peer := Peer{URL: server.URL, Audience: origin.audience}
	if err := progressSync(receiver, peer, receiver.credentials); err == nil {
		t.Fatal("stale cursor was silently retried within the same call")
	}
	if len(publicationCheckpoint(t, receiver).Entries) != MaxRepairInventoryEntries || len(receiver.n.repairProgress) != 0 {
		t.Fatal("stale cursor erased applied records or retained the invalid cycle")
	}
	mu.Lock()
	cursors = nil
	mu.Unlock()
	if err := progressSync(receiver, peer, receiver.credentials); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	restarted := len(cursors) > 0 && cursors[0] == ""
	mu.Unlock()
	if !restarted || len(publicationCheckpoint(t, receiver).Entries) != MaxRepairInventoryEntries+3 {
		t.Fatal("next call did not start a fresh complete inventory cycle")
	}
}

func TestRepairProgressRejectsWholeMalformedInventoryPage(t *testing.T) {
	for _, malformed := range []string{"late-duplicate", "too-many", "cursor-bytes", "response-bytes", "repeated-cursor"} {
		t.Run(malformed, func(t *testing.T) {
			origin := progressFixture(t, nil, "malformed-origin")
			receiver := progressFixture(t, origin.root, "malformed-receiver")
			progressSeed(t, origin, 17)
			status, inventory := reviewInventory(t, origin, origin.credentials, "")
			if status != http.StatusOK || len(inventory.Entries) != 17 {
				t.Fatal("missing signed fixture inventory")
			}
			var mu sync.Mutex
			fetches, inventories := 0, 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/v1/repair/fetch" {
					mu.Lock()
					fetches++
					mu.Unlock()
					origin.n.Handler().ServeHTTP(w, r)
					return
				}
				mu.Lock()
				inventories++
				index := inventories
				mu.Unlock()
				response := RepairInventoryResponse{Version: RepairVersion, Entries: append([]RepairEntry(nil), inventory.Entries[:8]...)}
				switch malformed {
				case "late-duplicate":
					response.Entries = append(response.Entries, response.Entries[0])
				case "too-many":
					for len(response.Entries) <= MaxRepairInventoryEntries {
						response.Entries = append(response.Entries, inventory.Entries[0])
					}
				case "cursor-bytes":
					response.Cursor = strings.Repeat("x", maxRepairCursorBytes+1)
				case "response-bytes":
					response.Cursor = strings.Repeat("x", MaxRepairInventoryBytes)
				case "repeated-cursor":
					response.Cursor = "repeated-token"
					if index > 1 {
						response.Entries = append([]RepairEntry(nil), inventory.Entries[8:16]...)
					}
				}
				_ = json.NewEncoder(w).Encode(response)
			}))
			defer server.Close()
			if err := progressSync(receiver, Peer{URL: server.URL, Audience: origin.audience}, receiver.credentials); err == nil {
				t.Fatal("malformed inventory accepted")
			}
			wantRecords, wantFetches := 0, 0
			if malformed == "repeated-cursor" {
				wantRecords, wantFetches = 8, 1
			}
			mu.Lock()
			gotFetches := fetches
			mu.Unlock()
			if got := len(publicationCheckpoint(t, receiver).Entries); got != wantRecords || gotFetches != wantFetches {
				t.Fatalf("invalid page partially applied: records=%d fetches=%d", got, gotFetches)
			}
			if len(receiver.n.repairProgress) != 0 {
				t.Fatal("malformed page retained continuation metadata")
			}
		})
	}
}

func TestRepairProgressCacheIsBoundedAndExpires(t *testing.T) {
	origin := progressFixture(t, nil, "bounded-origin")
	receiver := progressFixture(t, origin.root, "bounded-receiver")
	progressSeed(t, origin, 17)
	remote := &progressRemote{handler: origin.n.Handler()}
	server := httptest.NewServer(remote)
	defer server.Close()
	peer := Peer{URL: server.URL, Audience: origin.audience}
	var expiredCredentials Credentials
	for i := 0; i < maxRepairPeers+2; i++ {
		remote.configure(nil, 0, "429")
		credentials := receiver.credentials
		grant := credentials.Grant.Grant
		grant.ID = fmt.Sprintf("bounded-grant-%d", i)
		var err error
		credentials.Grant, err = authority.SignGrant(grant, origin.root)
		if err != nil {
			t.Fatal(err)
		}
		if i == 0 {
			expiredCredentials = credentials
		}
		if err := progressSync(receiver, peer, credentials); err == nil {
			t.Fatal("temporary fetch refusal unexpectedly completed")
		}
		if len(receiver.n.repairProgress) > maxRepairPeers {
			t.Fatal("peer/credential continuations exceeded the cache bound")
		}
	}
	if len(receiver.n.repairProgress) == 0 {
		t.Fatal("temporary failures did not retain bounded progress")
	}
	// Simulate elapsed wall time without a two-minute sleep. These hints have
	// no authority; their expiration must discard even a partially used page.
	for _, state := range receiver.n.repairProgress {
		if len(state.Pending) > MaxRepairInventoryEntries || state.Expires.After(time.Now().Add(repairCursorLifetime)) {
			t.Fatal("cached metadata or lifetime exceeded its bound")
		}
		state.Expires = time.Now().Add(-time.Second)
	}
	remote.configure(nil, -1, "")
	if err := progressSync(receiver, peer, expiredCredentials); err != nil {
		t.Fatal(err)
	}
	requests := remote.captured()
	if len(requests) == 0 || requests[0].Path != "/v1/repair/inventory" || requests[0].Cursor != "" || len(receiver.n.repairProgress) != 0 {
		t.Fatal("expired continuation did not begin and finish a fresh cycle")
	}
}

func TestRepairProgressMalformedExactProofDiscardsCycle(t *testing.T) {
	origin := progressFixture(t, nil, "tamper-origin")
	receiver := progressFixture(t, origin.root, "tamper-receiver")
	progressSeed(t, origin, 17)
	var mu sync.Mutex
	fetches := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		captured := httptest.NewRecorder()
		origin.n.Handler().ServeHTTP(captured, r)
		if r.URL.Path == "/v1/repair/fetch" && captured.Code == http.StatusOK {
			mu.Lock()
			fetches++
			tamper := fetches == 2
			mu.Unlock()
			if tamper {
				var response RepairFetchResponse
				if json.Unmarshal(captured.Body.Bytes(), &response) != nil || len(response.Records) != 8 {
					http.Error(w, "invalid tamper fixture", 500)
					return
				}
				// The requested key/version/entry still match. Only the record
				// content now disagrees with its exact promised component digest.
				response.Records[0].Record.Component.Payload = json.RawMessage(`{"tampered":true}`)
				_ = json.NewEncoder(w).Encode(response)
				return
			}
		}
		w.WriteHeader(captured.Code)
		_, _ = w.Write(captured.Body.Bytes())
	}))
	defer server.Close()
	peer := Peer{URL: server.URL, Audience: origin.audience}
	if err := progressSync(receiver, peer, receiver.credentials); err == nil {
		t.Fatal("changed exact proof was accepted")
	}
	if len(publicationCheckpoint(t, receiver).Entries) != 8 || len(receiver.n.repairProgress) != 0 {
		t.Fatal("malformed proof partially admitted its page or retained the invalid cycle")
	}
	if err := progressSync(receiver, peer, receiver.credentials); err != nil {
		t.Fatal(err)
	}
	if len(publicationCheckpoint(t, receiver).Entries) != 17 {
		t.Fatal("fresh complete cycle did not recover after the malformed proof")
	}
}
