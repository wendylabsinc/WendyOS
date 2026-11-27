package node

import (
	"context"
	"encoding/json"
	"github.com/wendylabsinc/wendy/go/world/internal/selection"
	"github.com/wendylabsinc/wendy/go/world/internal/store"
	"github.com/wendylabsinc/wendy/go/world/internal/world"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func areaRequest() selection.Request {
	return selection.Request{Selection: selection.Selection{Types: []string{"wendy.hazard.v1"}, Region: &selection.Region{Frame: world.FrameRef{ID: "floor", Revision: "1"}, MinXZ: [2]float64{0, 0}, MaxXZ: [2]float64{2, 2}}}, PageSize: 1, MaxBytes: 8192}
}
func TestRegionPagesMovementUnknownAndCursorBinding(t *testing.T) {
	f := newFixture(t, nil, "area")
	epoch := allocate(t, f)
	for i, p := range []string{`{"position_m":[1,0,1],"uncertainty_m":0.2}`, `{"position_m":[2.1,0,1],"uncertainty_m":0.3}`, `{"position_m":[9,0,9],"uncertainty_m":0.1}`, `{"position_m":[null,0,0],"uncertainty_m":0}`} {
		c := component(f, epoch, uint64(i+1))
		c.Key.Type = "wendy.hazard.v1"
		c.Key.Entity = string(rune('a' + i))
		c.Payload = json.RawMessage(p)
		publish(t, f, c)
	}
	req := areaRequest()
	first := selectedOK(t, f, req)
	if first.Complete || len(first.Spatial) != 1 {
		t.Fatal(first)
	}
	req.Cursor = first.NextCursor
	all := append([]selection.Spatial{}, first.Spatial...)
	for {
		page := selectedOK(t, f, req)
		all = append(all, page.Spatial...)
		if page.Complete {
			break
		}
		req.Cursor = page.NextCursor
	}
	if len(all) != 3 {
		t.Fatal(all)
	}
	unknown := 0
	for _, d := range all {
		if d.Class == "unlocated" {
			unknown++
		}
		if d.ContentSHA256 == "" || d.ExpiresAt.IsZero() {
			t.Fatal(d)
		}
	}
	if unknown != 1 {
		t.Fatal(all)
	}
	changed := areaRequest()
	changed.Cursor = first.NextCursor
	changed.Selection.Region.MaxXZ[0] = 3
	status, _, _ := selectedCall(t, f, f.credentials, "/v1/query", changed)
	if status != 400 {
		t.Fatalf("scope cursor %d", status)
	}
	moved := component(f, epoch, 20)
	moved.Key.Type = "wendy.hazard.v1"
	moved.Key.Entity = "a"
	moved.Payload = json.RawMessage(`{"position_m":[9,0,9],"uncertainty_m":0}`)
	publish(t, f, moved)
	stale := areaRequest()
	stale.Cursor = first.NextCursor
	status, _, _ = selectedCall(t, f, f.credentials, "/v1/query", stale)
	if status != 409 {
		t.Fatalf("movement cursor %d", status)
	}
	watch := selection.WatchRequest{Selection: areaRequest().Selection, Revision: first.Revision, WaitMS: 0, PageSize: 8, MaxBytes: 16384}
	status, page, raw := selectedCall(t, f, f.credentials, "/v1/watch/selected", watch)
	if status != 200 || page.TimedOut || len(page.Components) != 2 {
		t.Fatalf("movement watch %d %s", status, raw)
	}
}
func TestRegionThreeNodePartitionRepair(t *testing.T) {
	a := newFixture(t, nil, "area-a")
	b := newFixture(t, a.root, "area-b")
	c := newFixture(t, a.root, "area-c")
	epoch := allocate(t, a)
	row := component(a, epoch, 1)
	row.Key.Type = "wendy.hazard.v1"
	row.Payload = json.RawMessage(`{"position_m":[1,0,1],"uncertainty_m":0.1}`)
	publish(t, a, row)
	sync := func(to, from *fixture) {
		t.Helper()
		if err := to.n.Sync(context.Background(), Peer{from.server.URL, from.audience}, to.credentials); err != nil {
			t.Fatal(err)
		}
	}
	sync(b, a)
	sync(c, b)
	if len(selectedOK(t, c, areaRequest()).Components) != 1 {
		t.Fatal("relay")
	}
	a.unavailable.Store(true)
	if b.n.Sync(context.Background(), Peer{a.server.URL, a.audience}, b.credentials) == nil {
		t.Fatal("partition")
	}
	a.unavailable.Store(false)
	row.Version.Sequence = 2
	row.Payload = json.RawMessage(`{"position_m":[9,0,9],"uncertainty_m":0}`)
	publish(t, a, row)
	if len(selectedOK(t, c, areaRequest()).Components) != 1 {
		t.Fatal("invented remote knowledge")
	}
	sync(b, a)
	sync(c, b)
	if len(selectedOK(t, c, areaRequest()).Components) != 0 {
		t.Fatal("stale area after repair")
	}
}

func TestRegionWithdrawalNeverResurrectsOriginal(t *testing.T) {
	f := newFixture(t, nil, "area-notes")
	annotationOperator(t, f)
	original := annotationRecord(t, f, annotationEpoch(t, f))
	if status, raw := postAnnotation(t, f, original); status != 200 {
		t.Fatal(status, string(raw))
	}
	r := areaRequest()
	r.Selection.Types = []string{"wendy.annotation.note.v1", "wendy.annotation.note.v2"}
	r.Selection.Region.MinXZ = [2]float64{-100, -100}
	r.Selection.Region.MaxXZ = [2]float64{100, 100}
	if len(selectedOK(t, f, r).Components) != 1 {
		t.Fatal("original absent")
	}
	withdrawn := amendRecord(t, f, original, nil, "withdraw", "", 2)
	if status, raw := postAnnotation(t, f, withdrawn); status != 200 {
		t.Fatal(status, string(raw))
	}
	if len(selectedOK(t, f, r).Components) != 0 {
		t.Fatal("region resurrected original")
	}
}

// This opt-in artifact is the exact bytes returned by a real authenticated HTTP
// query. Its fixed fractional phase reproduces millisecond parser truncation.
func TestRegionGoPrecisionFixture(t *testing.T) {
	f := newFixture(t, nil, "area-precision")
	f.server.Close()
	if e := f.s.Close(); e != nil {
		t.Fatal(e)
	}
	observed := time.Now().Truncate(time.Second).Add(999999 * time.Microsecond)
	f.wc.Now = func() time.Time { return observed }
	var e error
	f.s, e = store.Open(filepath.Join(filepath.Dir(f.cfg.ProofPath), "store.json"), f.wc)
	if e != nil {
		t.Fatal(e)
	}
	f.n, e = New(f.cfg, f.s, f.v)
	if e != nil {
		t.Fatal(e)
	}
	f.start(t)
	epoch := allocate(t, f)
	c := component(f, epoch, 1)
	c.Key.Type = "wendy.hazard.v1"
	c.Payload = json.RawMessage(`{"position_m":[1,0,1],"uncertainty_m":0.1}`)
	c.TTL = 5 * time.Second
	c.Timing.SampleTime = observed.Add(-876543 * time.Microsecond)
	publish(t, f, c)
	status, page, raw := selectedCall(t, f, f.credentials, "/v1/query", areaRequest())
	if status != 200 || len(page.Spatial) != 1 {
		t.Fatal(status, string(raw))
	}
	if path := os.Getenv("AREA_GO_PRECISION_FIXTURE"); path != "" {
		if !filepath.IsAbs(path) {
			t.Fatal("absolute fixture path required")
		}
		if e = os.WriteFile(path, raw, 0600); e != nil {
			t.Fatal(e)
		}
	}
}
