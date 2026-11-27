package fusiondemo

import (
	"context"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/world/internal/fusionview"
	"github.com/wendylabsinc/wendy/go/world/internal/world"
)

func TestDeliveredClaimSelectionAndPublisherRestart(t *testing.T) {
	hub := openParticipant(t, "hub", filepath.Join(t.TempDir(), "hub"))
	dir := filepath.Join(t.TempDir(), "atlas")
	atlas := openParticipant(t, "atlas", dir)
	local, media := samples(t, "hub", 1, time.Second)
	scan, _ := samples(t, "atlas", 1, time.Second)
	h := tick(t, hub, Tick{NowNS: int64(time.Second), LocalSamples: local, Media: media})
	a := tick(t, atlas, Tick{NowNS: int64(time.Second), LocalSamples: scan, Received: h.ReplicaRecords, Media: h.Media})
	if a.Claim == nil {
		t.Fatal(a.Result)
	}
	cfg := fusionview.Config{Tenant: Tenant, Site: Site, RunID: "office", ClockDomain: "simulation:office", Frame: config("atlas").Frame, Gateways: map[string]world.Source{"atlas": atlas.claimSource()}, Hold: time.Second, Improvement: .2}
	s, err := fusionview.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	r := fusionview.Report{Gateway: "atlas", Version: a.Version, EvaluatedAt: Anchor.Add(time.Second), State: a.Result.State, Claim: a.Claim}
	v, err := s.Update(Anchor.Add(time.Second), []fusionview.Report{r})
	if err != nil || v.Selected != "atlas" || !reflect.DeepEqual(v.Claim, a.Claim) {
		t.Fatalf("%+v %v", v, err)
	}
	atlas.Close()
	atlas = openParticipant(t, "atlas", dir)
	fresh, err := atlas.Tick(context.Background(), Tick{NowNS: int64(1100 * time.Millisecond)})
	if err != nil {
		t.Fatal(err)
	}
	if fresh.Version.Epoch <= a.Version.Epoch {
		t.Fatal("reopen reused publisher epoch")
	}
	r.Version = fresh.Version
	r.EvaluatedAt = Anchor.Add(1100 * time.Millisecond)
	r.Claim = fresh.Claim
	if _, err = s.Update(r.EvaluatedAt, []fusionview.Report{r}); err != nil {
		t.Fatal(err)
	}
	v, err = s.Update(Anchor.Add(3*time.Second), nil)
	if err != nil || v.Claim != nil {
		t.Fatal("restart extended original lifetime", err)
	}
}
