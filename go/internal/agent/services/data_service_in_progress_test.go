package services

import (
	"context"
	"sort"
	"strings"
	"testing"

	"github.com/wendylabsinc/wendy/go/internal/agent/data"
	agentpbv2 "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
)

// TestStatusAndEpisodesReportTheRecordingInProgress drives the two RPCs the
// CLI reads through one `wendy data record` episode. Before in_progress,
// Episodes said nothing at all about it until it was stopped.
func TestStatusAndEpisodesReportTheRecordingInProgress(t *testing.T) {
	manager, err := data.NewManager(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	service := NewDataService(manager)
	service.adHocDrain = 0
	adapter := &fakeDataAdapter{}
	service.addAdapter(adapter)
	ctx := context.Background()

	started, err := service.Start(ctx, &agentpbv2.DataStartRequest{Name: "manual-test", Sources: []string{"fake:camera", "telemetry"}})
	if err != nil {
		t.Fatal(err)
	}

	status, err := service.Status(ctx, &agentpbv2.DataStatusRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if !status.GetReportsInProgress() {
		t.Fatal("reports_in_progress is false, so a client would fall back to the single active episode")
	}
	if status.GetActive().GetId() != started.GetId() {
		t.Fatalf("active = %v, want the episode older clients still read", status.GetActive())
	}
	if len(status.GetInProgress()) != 1 {
		t.Fatalf("in_progress = %v", status.GetInProgress())
	}
	open := status.GetInProgress()[0]
	if open.GetId() != started.GetId() || open.GetState() != "recording" || open.GetName() != "manual-test" ||
		open.GetTriggerReason() != "manual" || open.GetCampaignName() != "" || open.GetBootId() == "" {
		t.Fatalf("in-progress episode: %v", open)
	}
	// In the manifest's order, which is discovery order, not the request's.
	sources := append([]string(nil), open.GetSources()...)
	sort.Strings(sources)
	if strings.Join(sources, ",") != "fake:camera,telemetry" {
		t.Fatalf("sources = %v", open.GetSources())
	}
	if open.GetDurationNanos() <= 0 || open.GetSizeBytes() <= 0 {
		t.Fatalf("duration %d and size %d must be measured", open.GetDurationNanos(), open.GetSizeBytes())
	}

	episodes, err := service.Episodes(ctx, &agentpbv2.DataEpisodesRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if len(episodes.GetEpisodes()) != 0 {
		t.Fatalf("episodes must stay finalized-only: %v", episodes.GetEpisodes())
	}
	if !episodes.GetReportsInProgress() {
		t.Fatal("Episodes reports_in_progress is false, so a client would ask Status as well")
	}
	if len(episodes.GetInProgress()) != 1 || episodes.GetInProgress()[0].GetId() != started.GetId() {
		t.Fatalf("Episodes in_progress = %v", episodes.GetInProgress())
	}

	if _, err = service.Stop(ctx, &agentpbv2.DataStopRequest{}); err != nil {
		t.Fatal(err)
	}
	if !adapter.stopped {
		t.Fatal("the capture adapter was not stopped")
	}
	status, err = service.Status(ctx, &agentpbv2.DataStatusRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if status.GetActive() != nil || len(status.GetInProgress()) != 0 || !status.GetReportsInProgress() {
		t.Fatalf("after Stop nothing is in progress: %v", status)
	}
	episodes, err = service.Episodes(ctx, &agentpbv2.DataEpisodesRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if len(episodes.GetInProgress()) != 0 || !episodes.GetReportsInProgress() || len(episodes.GetEpisodes()) != 1 || episodes.GetEpisodes()[0].GetState() != "complete" {
		t.Fatalf("after Stop: %v", episodes)
	}
	// A finalized entry carries none of the in-progress fields; they are
	// documented as in-progress only.
	if finalized := episodes.GetEpisodes()[0]; finalized.GetDurationNanos() != 0 || finalized.GetTriggerReason() != "" || len(finalized.GetSources()) != 0 {
		t.Fatalf("finalized entry gained in-progress fields: %v", finalized)
	}
}
