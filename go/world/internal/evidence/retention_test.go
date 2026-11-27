package evidence

import (
	"bytes"
	"encoding/json"
	"errors"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

type retentionRig struct {
	s   *Store
	cfg Config
	dir string
	now time.Time
}

func retentionSetup(t *testing.T, configure func(*Config)) *retentionRig {
	t.Helper()
	r := &retentionRig{dir: t.TempDir(), now: time.Unix(1700000000, 0)}
	r.cfg = config(&r.now)
	r.cfg.MaxRetention = time.Hour
	if configure != nil {
		configure(&r.cfg)
	}
	var err error
	r.s, err = Open(r.dir, r.cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { r.s.Close() })
	return r
}
func (r *retentionRig) reopen(t *testing.T) {
	t.Helper()
	r.s.Close()
	var err error
	r.s, err = Open(r.dir, r.cfg)
	if err != nil {
		t.Fatal(err)
	}
}
func (r *retentionRig) put(t *testing.T, owner, body, class string) PutResponse {
	t.Helper()
	q := request([]byte(body))
	q.RetentionClass = class
	p, err := r.s.Put(owner, q)
	if err != nil {
		t.Fatal(err)
	}
	return p
}
func available(t *testing.T, s *Store, p PutResponse, want bool) {
	t.Helper()
	_, err := s.Get(GetRequest{SHA256: p.SHA256, Length: 1})
	if want && err != nil || !want && !errors.Is(err, ErrUnavailable) {
		t.Fatalf("availability %t: %v", want, err)
	}
}
func pinFor(r *retentionRig, id string, ps ...PutResponse) PinRequest {
	p := PinRequest{ID: id, RetainUntilUnixMS: r.now.Add(30 * time.Minute).UnixMilli()}
	for _, v := range ps {
		p.SHA256 = append(p.SHA256, v.SHA256)
	}
	return p
}
func references(ps ...PutResponse) []Reference {
	r := []Reference{}
	for _, p := range ps {
		r = append(r, Reference{SHA256: p.SHA256, Size: p.Size, ContentType: p.ContentType})
	}
	return r
}

func TestRollingEvictsOldestAllocationAcrossRestartWithoutReadRefresh(t *testing.T) {
	r := retentionSetup(t, func(c *Config) { c.MaxEntries = 2 })
	a := r.put(t, "sensor", "old", RetentionRolling)
	b := r.put(t, "sensor", "new", RetentionRolling)
	available(t, r.s, a, true)
	q := request([]byte("old"))
	q.Data = nil
	q.RetentionClass = RetentionRolling
	if got, err := r.s.Put("sensor", q); err != nil || got != a {
		t.Fatal("retry changed acknowledgement", got, err)
	}
	r.reopen(t)
	c := r.put(t, "executor", "result", RetentionProtected)
	available(t, r.s, a, false)
	available(t, r.s, b, true)
	available(t, r.s, c, true)
	if r.s.entries[b.SHA256].RetainUntilUnixMS != b.RetainUntilUnixMS {
		t.Fatal("expiry changed")
	}
}

func TestInsufficientVictimsDoesNotEvictUnexpiredData(t *testing.T) {
	r := retentionSetup(t, func(c *Config) { c.QuotaBytes = 10; c.MaxBlobBytes = 10 })
	a := r.put(t, "package", "12345678", RetentionProtected)
	b := r.put(t, "camera", "a", RetentionRolling)
	q := request([]byte("123456789"))
	q.RetentionClass = RetentionRolling
	if _, err := r.s.Put("camera", q); !errors.Is(err, ErrQuota) {
		t.Fatal(err)
	}
	available(t, r.s, a, true)
	available(t, r.s, b, true)
	c := r.put(t, "executor", "bc", RetentionProtected)
	available(t, r.s, b, false)
	available(t, r.s, c, true)
}

func TestLegacyObjectsAndPartialUploadsRemainProtected(t *testing.T) {
	r := retentionSetup(t, func(c *Config) { c.MaxEntries = 2 })
	a := r.put(t, "old-writer", "legacy", RetentionProtected)
	q := request([]byte("partial"))
	q.Data = []byte("par")
	partial, err := r.s.Put("old-writer", q)
	if err != nil {
		t.Fatal(err)
	}
	r.s.Close()
	for _, p := range []PutResponse{a, partial} {
		path := filepath.Join(r.dir, p.SHA256+".json")
		var m manifest
		b, _ := os.ReadFile(path)
		if json.Unmarshal(b, &m) != nil {
			t.Fatal("manifest")
		}
		m.Version = 1
		m.RetentionClass = ""
		m.Allocation = 0
		m.UploadUntilUnixMS = 0
		b, _ = json.Marshal(m)
		if err = os.WriteFile(path, b, 0600); err != nil {
			t.Fatal(err)
		}
	}
	if err = os.Remove(filepath.Join(r.dir, retentionFile)); err != nil {
		t.Fatal(err)
	}
	r.s, err = Open(r.dir, r.cfg)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = r.s.Put("camera", request([]byte("third"))); !errors.Is(err, ErrQuota) {
		t.Fatal(err)
	}
	probe := request([]byte("legacy"))
	probe.Data = nil
	probe.RetentionClass = RetentionRolling
	if p, e := r.s.Put("old-writer", probe); e != nil || p != a {
		t.Fatal("stronger legacy protection not reused", p, e)
	}
	if r.s.entries[a.SHA256].RetentionClass != RetentionProtected {
		t.Fatal("legacy downgraded")
	}
	probe.RetainUntilUnixMS = a.RetainUntilUnixMS - 1
	if _, e := r.s.Put("old-writer", probe); e == nil {
		t.Fatal("changed expiry accepted")
	}
	q.RetentionClass = RetentionRolling
	q.Data = nil
	if _, e := r.s.Put("old-writer", q); e == nil {
		t.Fatal("partial protection changed")
	}
	q.RetentionClass = ""
	q.Offset = 3
	q.Data = []byte("tial")
	if p, e := r.s.Put("old-writer", q); e != nil || !p.Complete {
		t.Fatal(p, e)
	}
	r.reopen(t)
	available(t, r.s, a, true)
}

func TestRollingPartialUploadHasFixedReservationDeadline(t *testing.T) {
	r := retentionSetup(t, func(c *Config) { c.MaxEntries = 1; c.RollingUploadTimeout = 2 * time.Second })
	q := request([]byte("partial"))
	q.RetentionClass = RetentionRolling
	q.Data = nil
	if _, e := r.s.Put("sensor", q); e != nil {
		t.Fatal(e)
	}
	until := r.s.entries[q.SHA256].UploadUntilUnixMS
	r.now = r.now.Add(time.Second)
	if _, e := r.s.Put("sensor", q); e != nil {
		t.Fatal(e)
	}
	if r.s.entries[q.SHA256].UploadUntilUnixMS != until {
		t.Fatal("retry renewed reservation")
	}
	if _, e := r.s.Put("executor", request([]byte("result"))); !errors.Is(e, ErrQuota) {
		t.Fatal(e)
	}
	r.now = r.now.Add(time.Second)
	p := r.put(t, "executor", "result", RetentionProtected)
	available(t, r.s, p, true)
	if _, ok := r.s.entries[q.SHA256]; ok {
		t.Fatal("abandoned rolling reservation survived")
	}
}

func TestBatchPinsPersistAndReleasedIDsCannotReactivate(t *testing.T) {
	r := retentionSetup(t, func(c *Config) { c.MaxEntries = 2 })
	a := r.put(t, "camera", "a", RetentionRolling)
	b := r.put(t, "camera", "b", RetentionRolling)
	p := pinFor(r, "investigation", a, b)
	got, e := r.s.Pin("alice", p)
	if e != nil || got.State != "active" {
		t.Fatal(got, e)
	}
	got.SHA256[0] = "tamper"
	r.reopen(t)
	q := request([]byte("c"))
	q.RetentionClass = RetentionRolling
	if _, e = r.s.Put("camera", q); !errors.Is(e, ErrQuota) {
		t.Fatal(e)
	}
	p.SHA256[0], p.SHA256[1] = p.SHA256[1], p.SHA256[0]
	if got, e = r.s.Pin("alice", p); e != nil || got.State != "active" {
		t.Fatal(got, e)
	}
	if _, e = r.s.Unpin("bob", UnpinRequest{ID: p.ID}); !errors.Is(e, ErrUnavailable) {
		t.Fatal(e)
	}
	if _, e = r.s.Unpin("alice", UnpinRequest{ID: p.ID}); e != nil {
		t.Fatal(e)
	}
	r.reopen(t)
	if got, e = r.s.Pin("alice", p); e != nil || got.State != "released" {
		t.Fatal("released pin revived", got, e)
	}
	c := r.put(t, "camera", "c", RetentionRolling)
	available(t, r.s, a, false)
	available(t, r.s, b, true)
	available(t, r.s, c, true)
	changed := pinFor(r, p.ID, c)
	if _, e = r.s.Pin("alice", changed); e == nil {
		t.Fatal("ID rebound")
	}
	r.now = r.now.Add(31 * time.Minute)
	if _, e = r.s.Pin("alice", p); e == nil {
		t.Fatal("expired signed pin body recreated")
	}
}

func TestPinValidationIsAtomicAndCannotExtendExpiry(t *testing.T) {
	r := retentionSetup(t, nil)
	a := r.put(t, "camera", "a", RetentionRolling)
	b := r.put(t, "camera", "b", RetentionRolling)
	missing := pinFor(r, "missing", a)
	missing.SHA256 = append(missing.SHA256, request([]byte("missing")).SHA256)
	if _, e := r.s.Pin("alice", missing); !errors.Is(e, ErrUnavailable) {
		t.Fatal(e)
	}
	if len(r.s.pins) != 0 || len(r.s.pinned) != 0 {
		t.Fatal("partial pin")
	}
	if e := os.WriteFile(r.s.blob(b.SHA256), []byte("x"), 0600); e != nil {
		t.Fatal(e)
	}
	if _, e := r.s.Pin("alice", pinFor(r, "bad", a, b)); e == nil {
		t.Fatal("corrupt bytes pinned")
	}
	if len(r.s.pins) != 0 {
		t.Fatal("corrupt batch partly pinned")
	}
	p := pinFor(r, "long", a)
	p.RetainUntilUnixMS = a.RetainUntilUnixMS + 1
	if _, e := r.s.Pin("alice", p); e == nil {
		t.Fatal("hard expiry extended")
	}
	p.RetainUntilUnixMS = 0
	if _, e := r.s.Pin("alice", p); e == nil {
		t.Fatal("unbounded/default external pin body accepted")
	}
}

func TestInternalHoldsReplaceOneGroupAndSeparateExternalNamespace(t *testing.T) {
	r := retentionSetup(t, func(c *Config) { c.MaxPins = 3; c.MaxPinsPerOwner = 1; c.MaxEntries = 3 })
	a := r.put(t, "camera", "a", RetentionRolling)
	b := r.put(t, "camera", "b", RetentionRolling)
	if _, e := r.s.Pin("run", pinFor(r, "task", a)); e != nil {
		t.Fatal(e)
	}
	if _, e := r.s.ReplaceHold(HoldTask, "run", "task", references(a), 0); e != nil {
		t.Fatal(e)
	}
	missing := Reference{SHA256: request([]byte("missing")).SHA256, Size: 7, ContentType: "image/png"}
	if _, e := r.s.ReplaceHold(HoldTask, "run", "task", []Reference{missing}, 0); !errors.Is(e, ErrUnavailable) {
		t.Fatal(e)
	}
	if r.s.pinned[a.SHA256] != 2 {
		t.Fatal("failed replacement lost old hold")
	}
	if _, e := r.s.ReplaceHold(HoldTask, "run", "task", append(references(b), references(b)...), 0); e != nil {
		t.Fatal(e)
	}
	if len(r.s.pins) != 2 || r.s.pinned[a.SHA256] != 1 || r.s.pinned[b.SHA256] != 1 {
		t.Fatal("replacement did not preserve distinct groups")
	}
	bad := references(b, b)
	bad[0].Size++
	if _, e := r.s.ReplaceHold(HoldTask, "run", "task", bad, 0); e == nil {
		t.Fatal("conflicting duplicate hidden")
	}
	if _, e := r.s.ReplaceHold(HoldTask, "run", "second", references(a), 0); e != nil {
		t.Fatal("internal owner incorrectly capped", e)
	}
	r.reopen(t)
	ids, e := r.s.HoldIDs(HoldTask, "run")
	if e != nil || !reflect.DeepEqual(ids, []string{"second", "task"}) {
		t.Fatal(ids, e)
	}
	if e = r.s.ReleaseHold(HoldTask, "run", "task"); e != nil {
		t.Fatal(e)
	}
	if e = r.s.ReleaseHold(HoldTask, "run", "task"); e != nil {
		t.Fatal("release not idempotent", e)
	}
	if len(r.s.pins) != 2 || r.s.pinned[a.SHA256] != 2 {
		t.Fatal("internal release altered public pin")
	}
}

func TestPinBoundsIncludeReleasedTombstonesAndSeparateOwners(t *testing.T) {
	r := retentionSetup(t, func(c *Config) { c.MaxPins = 4; c.MaxPinsPerOwner = 1; c.MaxPinReferences = 3 })
	a := r.put(t, "camera", "a", RetentionRolling)
	b := r.put(t, "camera", "b", RetentionRolling)
	p := pinFor(r, "first", a)
	p.RetainUntilUnixMS = r.now.Add(time.Second).UnixMilli()
	if _, e := r.s.Pin("alice", p); e != nil {
		t.Fatal(e)
	}
	if _, e := r.s.Unpin("alice", UnpinRequest{ID: p.ID}); e != nil {
		t.Fatal(e)
	}
	if _, e := r.s.Pin("alice", pinFor(r, "second", b)); !errors.Is(e, ErrQuota) {
		t.Fatal("released ID escaped owner bound", e)
	}
	if _, e := r.s.Pin("bob", pinFor(r, "other", a, b)); e != nil {
		t.Fatal(e)
	}
	if _, e := r.s.ReplaceHold(HoldTask, "run", "task", references(b), 0); !errors.Is(e, ErrQuota) {
		t.Fatal("reference bound escaped", e)
	}
	r.now = r.now.Add(2 * time.Second)
	if _, e := r.s.Pin("alice", pinFor(r, "second", b)); e != nil {
		t.Fatal("expired tombstone retained slot", e)
	}
}

func TestPinCommitFaultDoesNotExposeUnprotectedSuccess(t *testing.T) {
	for _, stage := range []string{"before-write", "after-rename"} {
		t.Run(stage, func(t *testing.T) {
			r := retentionSetup(t, func(c *Config) { c.MaxEntries = 1 })
			a := r.put(t, "camera", "a", RetentionRolling)
			injected := errors.New("pin persistence fault")
			r.s.pinCommitHook = func(at string) error {
				if at == stage {
					return injected
				}
				return nil
			}
			if _, e := r.s.Pin("alice", pinFor(r, "hold", a)); !errors.Is(e, injected) {
				t.Fatal(e)
			}
			if _, e := r.s.Put("camera", request([]byte("b"))); !errors.Is(e, injected) {
				t.Fatal("faulted store kept evicting", e)
			}
			r.reopen(t)
			q := request([]byte("b"))
			q.RetentionClass = RetentionRolling
			_, e := r.s.Put("camera", q)
			if stage == "after-rename" {
				if !errors.Is(e, ErrQuota) {
					t.Fatal("uncertain committed pin lost", e)
				}
				available(t, r.s, a, true)
			} else if e != nil {
				t.Fatal(e)
			}
		})
	}
}

func TestEvictionRemovalFaultLeavesNoResurrectedManifest(t *testing.T) {
	r := retentionSetup(t, func(c *Config) { c.MaxEntries = 1 })
	a := r.put(t, "camera", "a", RetentionRolling)
	r.s.removeHook = func(string) error { return errors.New("crash after durable manifest removal") }
	q := request([]byte("b"))
	q.RetentionClass = RetentionRolling
	if _, e := r.s.Put("camera", q); e == nil {
		t.Fatal("fault hidden")
	}
	if _, e := os.Stat(filepath.Join(r.dir, a.SHA256+".json")); !errors.Is(e, os.ErrNotExist) {
		t.Fatal(e)
	}
	if _, e := os.Stat(r.s.blob(a.SHA256)); e != nil {
		t.Fatal("test did not stop before body removal", e)
	}
	r.reopen(t)
	available(t, r.s, a, false)
	if _, e := os.Stat(r.s.blob(a.SHA256)); !errors.Is(e, os.ErrNotExist) {
		t.Fatal("orphan not cleaned", e)
	}
	if _, e := r.s.Put("camera", q); e != nil {
		t.Fatal(e)
	}
}

func TestRetentionRecoveryRejectsMissingLedgerAndPinnedBytes(t *testing.T) {
	for _, damage := range []string{"ledger", "body", "unknown-field"} {
		t.Run(damage, func(t *testing.T) {
			r := retentionSetup(t, nil)
			a := r.put(t, "camera", "a", RetentionRolling)
			if _, e := r.s.Pin("alice", pinFor(r, "hold", a)); e != nil {
				t.Fatal(e)
			}
			r.s.Close()
			switch damage {
			case "ledger":
				os.Remove(filepath.Join(r.dir, retentionFile))
			case "body":
				os.Remove(r.s.blob(a.SHA256))
			case "unknown-field":
				p := filepath.Join(r.dir, retentionFile)
				b, _ := os.ReadFile(p)
				b = bytes.Replace(b, []byte(`"version":2`), []byte(`"version":2,"unknown":true`), 1)
				os.WriteFile(p, b, 0600)
			}
			if s, e := Open(r.dir, r.cfg); e == nil {
				s.Close()
				t.Fatal("damaged retention state accepted")
			}
		})
	}
}

func TestRestartReducedQuotaEvictsOnlyUnpinnedRolling(t *testing.T) {
	r := retentionSetup(t, func(c *Config) { c.MaxEntries = 2 })
	a := r.put(t, "camera", "a", RetentionRolling)
	b := r.put(t, "camera", "b", RetentionRolling)
	if _, e := r.s.Pin("alice", pinFor(r, "old-but-pinned", a)); e != nil {
		t.Fatal(e)
	}
	r.cfg.MaxEntries = 1
	r.reopen(t)
	available(t, r.s, a, true)
	available(t, r.s, b, false)
}

func TestAllocationOverflowFailsBeforeEviction(t *testing.T) {
	r := retentionSetup(t, nil)
	a := r.put(t, "camera", "a", RetentionRolling)
	r.put(t, "camera", "b", RetentionRolling)
	r.s.Close()
	p := filepath.Join(r.dir, a.SHA256+".json")
	b, _ := os.ReadFile(p)
	var m manifest
	json.Unmarshal(b, &m)
	m.Allocation = math.MaxUint64
	b, _ = json.Marshal(m)
	os.WriteFile(p, b, 0600)
	var e error
	r.s, e = Open(r.dir, r.cfg)
	if e != nil {
		t.Fatal(e)
	}
	q := request([]byte("c"))
	q.RetentionClass = RetentionRolling
	if _, e = r.s.Put("camera", q); e == nil {
		t.Fatal("overflow accepted")
	}
	available(t, r.s, a, true)
}
