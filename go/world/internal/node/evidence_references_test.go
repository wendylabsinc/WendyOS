package node

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/world/internal/authority"
	"github.com/wendylabsinc/wendy/go/world/internal/evidence"
	"github.com/wendylabsinc/wendy/go/world/internal/geography"
	"github.com/wendylabsinc/wendy/go/world/internal/sensing"
	"github.com/wendylabsinc/wendy/go/world/internal/sitepub"
	"github.com/wendylabsinc/wendy/go/world/internal/tasking"
	"github.com/wendylabsinc/wendy/go/world/internal/worldpackage"
)

func putReference(t *testing.T, store *evidence.Store, data []byte, typ string) evidence.Reference {
	t.Helper()
	sum := sha256.Sum256(data)
	ref := evidence.Reference{SHA256: hex.EncodeToString(sum[:]), Size: int64(len(data)), ContentType: typ}
	until := time.Now().Add(time.Hour).UnixMilli()
	for offset := 0; offset < len(data); {
		end := min(offset+evidence.MaxChunk, len(data))
		_, err := store.Put("capture", evidence.PutRequest{SHA256: ref.SHA256, Size: ref.Size, ContentType: typ, Offset: int64(offset), Data: data[offset:end], RetainUntilUnixMS: until, RetentionClass: "rolling"})
		if err != nil {
			t.Fatal(err)
		}
		offset = end
	}
	return ref
}

func TestInspectionHoldReplacesCandidateAndSurvivesChurnRestart(t *testing.T) {
	path := t.TempDir()
	cfg := evidence.Config{Tenant: "tenant", Site: "office", MaxEntries: 2}
	blobs, err := evidence.Open(path, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { blobs.Close() })
	a := putReference(t, blobs, []byte("first captured pixels"), "image/png")
	inspection := tasking.Inspection{TaskID: "inspection", RunID: "run", Samples: []sensing.Sample{{Evidence: &sensing.EvidenceRef{SHA256: a.SHA256, Size: a.Size, ContentType: a.ContentType}}}}
	if err := InspectionEvidence(blobs)(inspection); err != nil {
		t.Fatal(err)
	}
	var b evidence.Reference
	for j := 0; j < 8; j++ {
		b = putReference(t, blobs, []byte(fmt.Sprintf("new pixels %d", j)), "image/png")
	}
	if _, err := blobs.Get(evidence.GetRequest{SHA256: a.SHA256, Length: 1}); err != nil {
		t.Fatal("held result evicted", err)
	}
	inspection.Samples[0].Evidence = &sensing.EvidenceRef{SHA256: b.SHA256, Size: b.Size, ContentType: b.ContentType}
	if err := InspectionEvidence(blobs)(inspection); err != nil {
		t.Fatal(err)
	}
	ids, err := blobs.HoldIDs(evidence.HoldTask, "run")
	if err != nil || len(ids) != 1 || ids[0] != "inspection" {
		t.Fatal("candidate holds accumulated", ids, err)
	}
	putReference(t, blobs, []byte("another frame"), "image/png")
	if _, err := blobs.Get(evidence.GetRequest{SHA256: a.SHA256, Length: 1}); !errors.Is(err, evidence.ErrUnavailable) {
		t.Fatal("superseded candidate stayed pinned", err)
	}
	if err := blobs.Close(); err != nil {
		t.Fatal(err)
	}
	blobs, err = evidence.Open(path, cfg)
	if err != nil {
		t.Fatal(err)
	}
	for j := 0; j < 8; j++ {
		putReference(t, blobs, []byte(fmt.Sprintf("post restart %d", j)), "image/png")
	}
	if _, err := blobs.Get(evidence.GetRequest{SHA256: b.SHA256, Length: 1}); err != nil {
		t.Fatal("hold lost at restart", err)
	}
}

func TestTaskEvidenceCleanupRequiresKnownImmutableTerminalFailure(t *testing.T) {
	f, op, _, _, cfg := taskFixture(t)
	f.n.cfg.Tasks.Close()
	cfg.RunID = "run"
	tasks, err := tasking.Open(filepath.Join(t.TempDir(), "tasks"), cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer tasks.Close()
	f.n.cfg.Tasks = tasks
	blobs, err := evidence.Open(t.TempDir(), evidence.Config{Tenant: "tenant", Site: "office"})
	if err != nil {
		t.Fatal(err)
	}
	defer blobs.Close()
	f.n.cfg.Evidence = blobs
	ref := putReference(t, blobs, []byte("candidate evidence"), "image/png")
	if _, err := blobs.ReplaceHold(evidence.HoldTask, "run", "task", []evidence.Reference{ref}, 0); err != nil {
		t.Fatal(err)
	}
	if err := f.n.ReconcileTaskEvidence(); err != nil {
		t.Fatal(err)
	}
	ids, _ := blobs.HoldIDs(evidence.HoldTask, "run")
	if len(ids) != 1 {
		t.Fatal("unknown task treated as permission to delete")
	}
	if code, b := call(t, f, op, "POST", "/v1/tasks", "mission", instructionBody(t, f, op, "mission", taskSpec("task"))); code != 200 {
		t.Fatal(code, string(b))
	}
	if err := f.n.ReconcileTaskEvidence(); err != nil {
		t.Fatal(err)
	}
	ids, _ = blobs.HoldIDs(evidence.HoldTask, "run")
	if len(ids) != 1 {
		t.Fatal("accepted task candidate released")
	}
	if code, b := call(t, f, op, "POST", "/v1/tasks/stop", "stop", instructionBody(t, f, op, "stop", StopRequest{TaskID: "task"})); code != 200 {
		t.Fatal(code, string(b))
	}
	if err := f.n.ReconcileTaskEvidence(); err != nil {
		t.Fatal(err)
	}
	ids, _ = blobs.HoldIDs(evidence.HoldTask, "run")
	if len(ids) != 0 {
		t.Fatal("terminal failure candidate leaked", ids)
	}
}

func TestPackagePublicationPinsAllRollingAssetsBeforeReference(t *testing.T) {
	f := newFixture(t, nil, "origin")
	siteAdmin(t, f)
	g := f.credentials.Grant.Grant
	g.Scopes = append(g.Scopes, "evidence-read", "evidence-write")
	var err error
	f.credentials.Grant, err = authority.SignGrant(g, f.root)
	if err != nil {
		t.Fatal(err)
	}
	bundle, err := worldpackage.Load("../../examples/live-office/world.json")
	if err != nil {
		t.Fatal(err)
	}
	var root map[string]any
	if err = json.Unmarshal(bundle.Root, &root); err != nil {
		t.Fatal(err)
	}
	root["site_id"] = "office"
	rootBytes, _ := json.Marshal(root)
	manifest, err := worldpackage.Inspect(rootBytes)
	if err != nil {
		t.Fatal(err)
	}
	unique := map[string]bool{manifest.SHA256: true}
	for _, file := range manifest.Files {
		unique[file.SHA256] = true
	}
	path := t.TempDir()
	evidenceNow := time.Now()
	cfg := evidence.Config{Tenant: "tenant", Site: "office", MaxEntries: len(unique) + 1, Now: func() time.Time { return evidenceNow }}
	blobs, err := evidence.Open(path, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { blobs.Close() })
	f.n.cfg.Evidence = blobs
	refs := []evidence.Reference{putReference(t, blobs, rootBytes, "application/octet-stream")}
	seen := map[string]bool{}
	for _, file := range manifest.Files {
		if !seen[file.SHA256] {
			refs = append(refs, putReference(t, blobs, bundle.Files[file.Path], "application/octet-stream"))
			seen[file.SHA256] = true
		}
	}
	component := siteComponent(t, f)
	var placement geography.SiteConfig
	if err = json.Unmarshal(component.Payload, &placement); err != nil {
		t.Fatal(err)
	}
	placement.PackageSHA256 = manifest.SHA256
	raw, _ := json.Marshal(placement)
	component, err = sitepub.Build(raw, sitepub.Config{Tenant: f.cfg.Tenant, Site: f.cfg.Site, RunID: "live", Source: f.credentials.Grant.Grant.Source, Epoch: component.Version.Epoch, Sequence: 1, Now: time.Now(), TTL: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	if code, b := call(t, f, f.credentials, "POST", "/v1/packages/publish", "publish", jsonBody(t, signed(t, f, component))); code != 200 {
		t.Fatal(code, string(b))
	}
	if len(siteList(t, f)) != 1 {
		t.Fatal("package reference missing")
	}
	// Record verification precedes local epoch admission. Even a correctly signed
	// request that fails that later check must not shorten an existing hold.
	stale := component
	stale.Version.Epoch++
	stale.TTL = 500 * time.Millisecond
	if code, body := call(t, f, f.credentials, "POST", "/v1/packages/publish", "publish", jsonBody(t, signed(t, f, stale))); code != 400 {
		t.Fatalf("unallocated package epoch: %d %s", code, body)
	}
	ids, err := blobs.HoldIDs(evidence.HoldPackage, g.Principal)
	if err != nil || len(ids) != 2 {
		t.Fatal("distinct instructions did not retain separate holds", ids, err)
	}
	evidenceNow = evidenceNow.Add(2 * time.Second)
	for j := 0; j < 3*cfg.MaxEntries; j++ {
		putReference(t, blobs, []byte(fmt.Sprintf("capture churn %d", j)), "image/png")
	}
	for _, ref := range refs {
		if _, err := blobs.Get(evidence.GetRequest{SHA256: ref.SHA256, Length: 1}); err != nil {
			t.Fatal("published package evicted", err)
		}
	}
	if err := blobs.Close(); err != nil {
		t.Fatal(err)
	}
	blobs, err = evidence.Open(path, cfg)
	if err != nil {
		t.Fatal(err)
	}
	f.n.cfg.Evidence = blobs
	for j := 0; j < 3*cfg.MaxEntries; j++ {
		putReference(t, blobs, []byte(fmt.Sprintf("restart churn %d", j)), "image/png")
	}
	for _, ref := range refs {
		if _, err := blobs.Get(evidence.GetRequest{SHA256: ref.SHA256, Length: 1}); err != nil {
			t.Fatal("package hold lost at restart", err)
		}
	}
	// Invalid entity identity cannot create another signed reference.
	component.Version.Sequence++
	component.Key.Entity += "invalid"
	if code, _ := call(t, f, f.credentials, "POST", "/v1/packages/publish", "publish", jsonBody(t, signed(t, f, component))); code == 200 {
		t.Fatal("invalid package publication accepted")
	}
	if len(siteList(t, f)) != 1 {
		t.Fatal("invalid package reference appeared")
	}
}

func TestPackagePublicationRejectsMissingAndMismatchedMembers(t *testing.T) {
	for _, missing := range []string{"root", "model", "asset", "metadata"} {
		t.Run(missing, func(t *testing.T) {
			f := newFixture(t, nil, "origin")
			siteAdmin(t, f)
			g := f.credentials.Grant.Grant
			g.Scopes = append(g.Scopes, "evidence-read", "evidence-write")
			var err error
			f.credentials.Grant, err = authority.SignGrant(g, f.root)
			if err != nil {
				t.Fatal(err)
			}
			blobs, err := evidence.Open(t.TempDir(), evidence.Config{Tenant: "tenant", Site: "office"})
			if err != nil {
				t.Fatal(err)
			}
			defer blobs.Close()
			f.n.cfg.Evidence = blobs
			bundle, err := worldpackage.Load("../../examples/live-office/world.json")
			if err != nil {
				t.Fatal(err)
			}
			var root map[string]any
			if err = json.Unmarshal(bundle.Root, &root); err != nil {
				t.Fatal(err)
			}
			asset := []byte("package auxiliary asset")
			digest := sha256.Sum256(asset)
			assetSize := len(asset)
			if missing == "metadata" {
				assetSize++
			}
			root["site_id"] = "office"
			root["assets"] = []worldpackage.File{{Path: "extra.bin", SHA256: hex.EncodeToString(digest[:]), Size: int64(assetSize)}}
			rootBytes, _ := json.Marshal(root)
			manifest, err := worldpackage.Inspect(rootBytes)
			if err != nil {
				t.Fatal(err)
			}
			if missing != "root" {
				putReference(t, blobs, rootBytes, "application/octet-stream")
			}
			if missing != "model" {
				putReference(t, blobs, bundle.Files[manifest.Files[0].Path], "application/octet-stream")
			}
			if missing != "asset" {
				putReference(t, blobs, asset, "application/octet-stream")
			}
			component := siteComponent(t, f)
			var placement geography.SiteConfig
			if err = json.Unmarshal(component.Payload, &placement); err != nil {
				t.Fatal(err)
			}
			placement.PackageSHA256 = manifest.SHA256
			raw, _ := json.Marshal(placement)
			component, err = sitepub.Build(raw, sitepub.Config{Tenant: f.cfg.Tenant, Site: f.cfg.Site, RunID: "live", Source: f.credentials.Grant.Grant.Source, Epoch: component.Version.Epoch, Sequence: 1, Now: time.Now(), TTL: time.Minute})
			if err != nil {
				t.Fatal(err)
			}
			if code, body := call(t, f, f.credentials, "POST", "/v1/packages/publish", "publish", jsonBody(t, signed(t, f, component))); code != 409 {
				t.Fatalf("incomplete package: %d %s", code, body)
			}
			if len(siteList(t, f)) != 0 {
				t.Fatal("incomplete package published")
			}
			ids, err := blobs.HoldIDs(evidence.HoldPackage, g.Principal)
			if err != nil || len(ids) != 0 {
				t.Fatal("failed package acquired partial hold", ids, err)
			}
		})
	}
}
