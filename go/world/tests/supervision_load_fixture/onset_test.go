package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/world/internal/authority"
)

func onsetOptions(t *testing.T, endpoint, variant string) options {
	t.Helper()
	o, _, _ := fixture(t, endpoint)
	o.duration = 24 * time.Second
	o.entities = 200
	o.hz = 2
	o.onsetVariant = variant
	dir := t.TempDir()
	o.manifest = filepath.Join(dir, "manifest.json")
	o.release = filepath.Join(dir, "release.json")
	o.report = filepath.Join(dir, "report.json")
	digests := make([]string, 200)
	for i := range digests {
		digests[i] = strings.Repeat("0", 64)
	}
	writeJSON(t, o.manifest, preloadManifest{Schema: "wendy.world.preload-manifest.v1", RunID: o.runID, Count: 200, Epoch: 7, Digests: digests})
	return o
}
func TestOnsetFixedGateMakesNoHTTPRequests(t *testing.T) {
	var requests atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { requests.Add(1); t.Error("fixed gate made HTTP request") }))
	defer server.Close()
	o := onsetOptions(t, server.URL, "fixed")
	now := time.Now().UnixMilli()
	writeJSON(t, o.release, onsetRelease{"wendy.world.publication-onset-release.v1", o.runID, "fixed", now, now + 1000, false})
	var out bytes.Buffer
	if err := run(context.Background(), o, &out); err != nil {
		t.Fatal(err)
	}
	var report onsetReport
	if json.Unmarshal(out.Bytes(), &report) != nil {
		t.Fatal("missing report")
	}
	if requests.Load() != 0 || report.Attempted != 0 || report.FirstSend != 0 || report.Stage != "finished" || report.Observed == 0 {
		t.Fatal(report)
	}
}
func TestOnsetChangingKeepsEpochAndStartsAfterReleaseWithoutCatchup(t *testing.T) {
	var requests atomic.Int64
	var first atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/components" {
			t.Errorf("unexpected endpoint %s", r.URL.Path)
			return
		}
		body, _ := io.ReadAll(io.LimitReader(r.Body, 65537))
		var proof authority.SignedRecord
		if authority.DecodeStrict(body, &proof) != nil {
			t.Error("invalid proof")
			return
		}
		n := requests.Add(1)
		if n == 1 {
			first.Store(time.Now().UnixMilli())
		}
		if proof.Component.Version.Epoch != 7 || proof.Component.Version.Sequence != uint64(200+n) || proof.Component.TTL != 5*time.Minute {
			t.Error("epoch, sequence or TTL changed")
		}
		time.Sleep(20 * time.Millisecond)
		_, _ = w.Write([]byte(`{"result":"applied"}`))
	}))
	defer server.Close()
	o := onsetOptions(t, server.URL, "changing")
	release := time.Now().Add(80 * time.Millisecond).UnixMilli()
	writeJSON(t, o.release, onsetRelease{"wendy.world.publication-onset-release.v1", o.runID, "changing", release, release + 1000, true})
	var out bytes.Buffer
	if err := run(context.Background(), o, &out); err != nil {
		t.Fatal(err)
	}
	var report onsetReport
	if json.Unmarshal(out.Bytes(), &report) != nil {
		t.Fatal("missing report")
	}
	if first.Load() < release || report.Attempted > 60 || report.Published == 0 || report.FirstACK < report.FirstSend || report.FirstCompletion < report.FirstSend {
		t.Fatal(report)
	}
}
func TestOnsetRefusesWrongRunStaleAndFixedPublish(t *testing.T) {
	for _, kind := range []string{"run", "stale", "fixed_publish", "huge", "manifest"} {
		t.Run(kind, func(t *testing.T) {
			o := onsetOptions(t, "http://127.0.0.1:1", "fixed")
			now := time.Now().UnixMilli()
			release := onsetRelease{"wendy.world.publication-onset-release.v1", o.runID, "fixed", now, now + 1000, false}
			switch kind {
			case "run":
				release.RunID = "another"
			case "stale":
				release.Release -= 3000
			case "fixed_publish":
				release.Publish = true
			case "huge":
				release.Release = 1<<63 - 1
			case "manifest":
				_ = os.WriteFile(o.manifest, []byte(`{}`), 0600)
			}
			writeJSON(t, o.release, release)
			var out bytes.Buffer
			if run(context.Background(), o, &out) == nil {
				t.Fatal("accepted bad release")
			}
		})
	}
}
