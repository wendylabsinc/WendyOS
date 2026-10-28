package commands

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	agentpbv2 "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// statusDataClient serves Episodes and Status from fixed responses and counts
// the Status calls. Every other method panics through the nil embedded client.
type statusDataClient struct {
	agentpbv2.DataServiceClient
	episodes    *agentpbv2.DataEpisodesResponse
	status      *agentpbv2.DataStatusResponse
	statusErr   error
	statusCalls int
}

func (c *statusDataClient) Episodes(context.Context, *agentpbv2.DataEpisodesRequest, ...grpc.CallOption) (*agentpbv2.DataEpisodesResponse, error) {
	return c.episodes, nil
}

func (c *statusDataClient) Status(context.Context, *agentpbv2.DataStatusRequest, ...grpc.CallOption) (*agentpbv2.DataStatusResponse, error) {
	c.statusCalls++
	return c.status, c.statusErr
}

var statusTestNow = time.Date(2026, 10, 27, 11, 55, 42, 0, time.UTC)

// recordingEpisode is what a current agent reports for `wendy data record`
// started 2m13s before statusTestNow.
func recordingEpisode() *agentpbv2.DataEpisode {
	return &agentpbv2.DataEpisode{
		Id: "20261027T115329Z-e4418826f638364e", Name: "manual-test", State: "recording",
		StartedUnixNanos: statusTestNow.Add(-133 * time.Second).UnixNano(), SizeBytes: 3145728,
		TriggerReason: "manual", Sources: []string{"v4l2:/dev/video0", "audio:54"}, DurationNanos: (133 * time.Second).Nanoseconds(),
	}
}

func campaignEpisode(state string) *agentpbv2.DataEpisode {
	return &agentpbv2.DataEpisode{
		Id: "20261027T115401Z-0b7c9d1e2f3a4b5c", Name: "c920-yolo-live", State: state,
		StartedUnixNanos: statusTestNow.Add(-101 * time.Second).UnixNano(), SizeBytes: 1048576,
		TriggerReason: "event:person_detected", CampaignName: "c920-yolo-live", Sources: []string{"v4l2:/dev/video0", "applications"},
		DurationNanos: (20 * time.Second).Nanoseconds(),
	}
}

func finalizedEpisode() *agentpbv2.DataEpisode {
	return &agentpbv2.DataEpisode{Id: "20261027T110000Z-aaaaaaaaaaaaaaaa", Name: "earlier", State: "complete", SizeBytes: 7245349}
}

func tableRows(t *testing.T, out string) [][]string {
	t.Helper()
	var rows [][]string
	for _, line := range strings.Split(strings.TrimRight(out, "\n"), "\n") {
		if line == "" {
			break
		}
		rows = append(rows, strings.Fields(line))
	}
	return rows
}

// TestEpisodesListsRecordingsInProgressFirst is the bug report: a recording
// started with `wendy data record` did not appear in `wendy data episodes`
// until it was stopped.
func TestEpisodesListsRecordingsInProgressFirst(t *testing.T) {
	client := &statusDataClient{episodes: &agentpbv2.DataEpisodesResponse{
		Episodes:   []*agentpbv2.DataEpisode{finalizedEpisode()},
		InProgress: []*agentpbv2.DataEpisode{recordingEpisode(), campaignEpisode("draining")},
	}}
	var out, errOut bytes.Buffer
	if err := runDataEpisodes(context.Background(), client, &out, &errOut, false, statusTestNow); err != nil {
		t.Fatal(err)
	}
	if client.statusCalls != 0 {
		t.Fatalf("Status was called %d times although Episodes already reported what is in progress", client.statusCalls)
	}
	rows := tableRows(t, out.String())
	want := [][]string{
		{"EPISODE", "STATE", "SIZE", "NAME", "DURATION", "TRIGGER"},
		{"20261027T115329Z-e4418826f638364e", "recording", "3145728", "manual-test", "2m13s", "manual"},
		{"20261027T115401Z-0b7c9d1e2f3a4b5c", "draining", "1048576", "c920-yolo-live", "20s", "campaign", "c920-yolo-live"},
		{"20261027T110000Z-aaaaaaaaaaaaaaaa", "complete", "7245349", "earlier"},
	}
	if len(rows) != len(want) {
		t.Fatalf("got %d lines, want %d:\n%s", len(rows), len(want), out.String())
	}
	for i := range want {
		if strings.Join(rows[i], " ") != strings.Join(want[i], " ") {
			t.Fatalf("line %d = %q, want %q\n%s", i, rows[i], want[i], out.String())
		}
	}
	// Aligned: every column starts at the same offset on every line.
	lines := strings.Split(out.String(), "\n")
	if strings.Index(lines[0], "STATE") != strings.Index(lines[3], "complete") {
		t.Fatalf("columns are not aligned:\n%s", out.String())
	}
	if errOut.Len() != 0 {
		t.Fatalf("unexpected note: %s", errOut.String())
	}
}

func TestEpisodesWithNothingInProgressKeepsFourColumns(t *testing.T) {
	client := &statusDataClient{
		episodes: &agentpbv2.DataEpisodesResponse{Episodes: []*agentpbv2.DataEpisode{finalizedEpisode()}, ReportsInProgress: true},
		// Never reached: a current agent's Episodes already says nothing is in
		// progress, so a failing Status must not add a note or a round trip.
		statusErr: status.Error(codes.Unavailable, "connection reset"),
	}
	var out bytes.Buffer
	if err := runDataEpisodes(context.Background(), client, &out, &out, false, statusTestNow); err != nil {
		t.Fatal(err)
	}
	if client.statusCalls != 0 {
		t.Fatalf("Status was called %d times although Episodes reports in progress", client.statusCalls)
	}
	if got := tableRows(t, out.String()); len(got) != 2 || strings.Join(got[0], " ") != "EPISODE STATE SIZE NAME" {
		t.Fatalf("unexpected table:\n%s", out.String())
	}
	if strings.Contains(out.String(), "agent") {
		t.Fatalf("a current agent with nothing recording produced a note:\n%s", out.String())
	}
}

// TestEpisodesFromAnOlderAgent covers an agent that predates in_progress, such
// as 2026.10.27-091653: its Episodes response has no in_progress, and its
// Status response carries the one open episode in active with nothing else.
func TestEpisodesFromAnOlderAgent(t *testing.T) {
	started := statusTestNow.Add(-75 * time.Second).UnixNano()
	client := &statusDataClient{
		episodes: &agentpbv2.DataEpisodesResponse{Episodes: []*agentpbv2.DataEpisode{finalizedEpisode()}},
		status: &agentpbv2.DataStatusResponse{Active: &agentpbv2.DataEpisode{
			Id: "20261027T115427Z-1111111111111111", Name: "manual-test", State: "recording", StartedUnixNanos: started,
		}},
	}
	var out bytes.Buffer
	if err := runDataEpisodes(context.Background(), client, &out, &out, false, statusTestNow); err != nil {
		t.Fatal(err)
	}
	rows := tableRows(t, out.String())
	if len(rows) != 3 {
		t.Fatalf("want header, the recording and the finalized episode:\n%s", out.String())
	}
	// No size or trigger is invented, and the duration is marked as estimated
	// from two clocks.
	if got := strings.Join(rows[1], " "); got != "20261027T115427Z-1111111111111111 recording - manual-test ~1m15s -" {
		t.Fatalf("older-agent row = %q\n%s", got, out.String())
	}
	if !strings.Contains(out.String(), olderAgentInProgressNote) {
		t.Fatalf("missing the older-agent note:\n%s", out.String())
	}
}

func TestEpisodesWhenTheAgentHasNoStatus(t *testing.T) {
	client := &statusDataClient{
		episodes:  &agentpbv2.DataEpisodesResponse{Episodes: []*agentpbv2.DataEpisode{finalizedEpisode()}},
		statusErr: status.Error(codes.Unimplemented, "unknown method Status"),
	}
	var out, errOut bytes.Buffer
	if err := runDataEpisodes(context.Background(), client, &out, &errOut, false, statusTestNow); err != nil {
		t.Fatalf("an agent without Status must still list its episodes: %v", err)
	}
	if !strings.Contains(out.String(), "earlier") || !strings.Contains(out.String(), noInProgressReportNote) {
		t.Fatalf("want the finalized episode and the note:\n%s", out.String())
	}
}

// TestEpisodesFallbackSkipsAnEpisodeAlreadyFinalized covers the two calls not
// being atomic: an episode that sealed between Episodes and Status is listed
// once, as finalized.
func TestEpisodesFallbackSkipsAnEpisodeAlreadyFinalized(t *testing.T) {
	sealed := finalizedEpisode()
	client := &statusDataClient{
		episodes: &agentpbv2.DataEpisodesResponse{Episodes: []*agentpbv2.DataEpisode{sealed}},
		status:   &agentpbv2.DataStatusResponse{ReportsInProgress: true, InProgress: []*agentpbv2.DataEpisode{{Id: sealed.GetId(), State: "sealing", TriggerReason: "manual"}}},
	}
	response := client.episodes
	if note := addInProgressFromStatus(context.Background(), client, response); note != "" {
		t.Fatalf("unexpected note %q", note)
	}
	if len(response.GetInProgress()) != 0 {
		t.Fatalf("a finalized episode was also listed in progress: %v", response.GetInProgress())
	}
}

// TestEpisodesJSONOnlyAddsAField pins --json compatibility: with nothing in
// progress, episodes encodes exactly as before beside the reports_in_progress
// flag, and in-progress episodes go in a new in_progress array, never into
// episodes, which scripts download.
func TestEpisodesJSONOnlyAddsAField(t *testing.T) {
	before := &agentpbv2.DataEpisodesResponse{Episodes: []*agentpbv2.DataEpisode{finalizedEpisode()}}
	var legacy bytes.Buffer
	if err := encodeProtoJSON(&legacy, before); err != nil {
		t.Fatal(err)
	}

	idle := &statusDataClient{episodes: &agentpbv2.DataEpisodesResponse{Episodes: []*agentpbv2.DataEpisode{finalizedEpisode()}, ReportsInProgress: true}}
	var out, errOut bytes.Buffer
	if err := runDataEpisodes(context.Background(), idle, &out, &errOut, true, statusTestNow); err != nil {
		t.Fatal(err)
	}
	var legacyFields, idleFields map[string]json.RawMessage
	if err := json.Unmarshal(legacy.Bytes(), &legacyFields); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(out.Bytes(), &idleFields); err != nil {
		t.Fatalf("--json is not one JSON document: %v\n%s", err, out.String())
	}
	if len(idleFields) != 2 || string(idleFields["episodes"]) != string(legacyFields["episodes"]) || string(idleFields["reports_in_progress"]) != "true" {
		t.Fatalf("--json with nothing in progress changed beyond the flag:\n got %s\nwant %s plus reports_in_progress", out.String(), legacy.String())
	}

	busy := &statusDataClient{episodes: &agentpbv2.DataEpisodesResponse{
		Episodes:   []*agentpbv2.DataEpisode{finalizedEpisode()},
		InProgress: []*agentpbv2.DataEpisode{recordingEpisode()},
	}}
	out.Reset()
	if err := runDataEpisodes(context.Background(), busy, &out, &errOut, true, statusTestNow); err != nil {
		t.Fatal(err)
	}
	var decoded struct {
		Episodes   []map[string]any `json:"episodes"`
		InProgress []map[string]any `json:"in_progress"`
	}
	if err := json.Unmarshal(out.Bytes(), &decoded); err != nil {
		t.Fatalf("--json is not one JSON document: %v\n%s", err, out.String())
	}
	if len(decoded.Episodes) != 1 || decoded.Episodes[0]["state"] != "complete" {
		t.Fatalf("episodes must stay finalized-only: %v", decoded.Episodes)
	}
	if len(decoded.InProgress) != 1 {
		t.Fatalf("in_progress = %v", decoded.InProgress)
	}
	got := decoded.InProgress[0]
	for key, want := range map[string]any{
		"state": "recording", "trigger_reason": "manual", "size_bytes": "3145728", "duration_nanos": "133000000000",
	} {
		if got[key] != want {
			t.Fatalf("in_progress[0].%s = %v, want %v (%s)", key, got[key], want, out.String())
		}
	}
	if sources, _ := got["sources"].([]any); len(sources) != 2 {
		t.Fatalf("in_progress[0].sources = %v", got["sources"])
	}
	if errOut.Len() != 0 {
		t.Fatalf("unexpected note: %s", errOut.String())
	}
}

func TestEpisodesJSONFromAnOlderAgentKeepsStdoutJSON(t *testing.T) {
	client := &statusDataClient{
		episodes: &agentpbv2.DataEpisodesResponse{},
		status:   &agentpbv2.DataStatusResponse{Active: &agentpbv2.DataEpisode{Id: "ep", State: "recording"}},
	}
	var out, errOut bytes.Buffer
	if err := runDataEpisodes(context.Background(), client, &out, &errOut, true, statusTestNow); err != nil {
		t.Fatal(err)
	}
	if !json.Valid(out.Bytes()) || !strings.Contains(out.String(), `"in_progress":[{"id":"ep","state":"recording"}]`) {
		t.Fatalf("stdout = %s", out.String())
	}
	if !strings.Contains(errOut.String(), olderAgentInProgressNote) {
		t.Fatalf("the older-agent note belongs on stderr under --json, got %q", errOut.String())
	}
}

func TestStatusShowsEachRecordingInProgress(t *testing.T) {
	client := &statusDataClient{status: &agentpbv2.DataStatusResponse{
		ReportsInProgress: true,
		Active:            recordingEpisode(),
		InProgress:        []*agentpbv2.DataEpisode{campaignEpisode("sealing"), recordingEpisode()},
	}}
	var out bytes.Buffer
	if err := runDataStatus(context.Background(), client, &out, false, statusTestNow); err != nil {
		t.Fatal(err)
	}
	want := `Episode: 20261027T115401Z-0b7c9d1e2f3a4b5c (c920-yolo-live)
State: sealing (capture stopped; writing playable video and checksums)
Started: 2026-10-27T11:54:01Z (device clock)
Duration: 20s
Trigger: event:person_detected (campaign c920-yolo-live)
Sources: v4l2:/dev/video0, applications
Size so far: 1048576 bytes

Episode: 20261027T115329Z-e4418826f638364e (manual-test)
State: recording
Started: 2026-10-27T11:53:29Z (device clock)
Duration: 2m13s
Trigger: manual
Sources: v4l2:/dev/video0, audio:54
Size so far: 3145728 bytes
`
	if out.String() != want {
		t.Fatalf("status output:\n%s\nwant:\n%s", out.String(), want)
	}
}

func TestStatusWithNothingInProgress(t *testing.T) {
	for name, response := range map[string]*agentpbv2.DataStatusResponse{
		"current agent": {ReportsInProgress: true},
		"older agent":   {},
	} {
		t.Run(name, func(t *testing.T) {
			var out bytes.Buffer
			if err := runDataStatus(context.Background(), &statusDataClient{status: response}, &out, false, statusTestNow); err != nil {
				t.Fatal(err)
			}
			if out.String() != "No recording in progress.\n" {
				t.Fatalf("got %q", out.String())
			}
		})
	}
}

func TestStatusFromAnOlderAgent(t *testing.T) {
	client := &statusDataClient{status: &agentpbv2.DataStatusResponse{Active: &agentpbv2.DataEpisode{
		Id: "20261027T115427Z-1111111111111111", Name: "manual-test", State: "draining",
		StartedUnixNanos: statusTestNow.Add(-75 * time.Second).UnixNano(),
	}}}
	var out bytes.Buffer
	if err := runDataStatus(context.Background(), client, &out, false, statusTestNow); err != nil {
		t.Fatal(err)
	}
	got := out.String()
	for _, want := range []string{
		"Episode: 20261027T115427Z-1111111111111111 (manual-test)\n",
		"State: draining (capture stopped; accepting late application records)\n",
		"Duration: ~1m15s\n",
		olderAgentInProgressNote,
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("missing %q in:\n%s", want, got)
		}
	}
	// An older agent sends no trigger, sources or size; printing "manual",
	// "none" and "0 bytes" would state things nobody measured.
	for _, invented := range []string{"Trigger:", "Sources:", "Size so far:"} {
		if strings.Contains(got, invented) {
			t.Fatalf("printed %q for an agent that does not report it:\n%s", invented, got)
		}
	}
}

func TestStatusWhenTheAgentHasNoStatus(t *testing.T) {
	client := &statusDataClient{statusErr: status.Error(codes.Unimplemented, "unknown method Status")}
	err := runDataStatus(context.Background(), client, &bytes.Buffer{}, false, statusTestNow)
	if err == nil || !strings.Contains(err.Error(), "doesn't report recordings in progress") {
		t.Fatalf("err = %v", err)
	}
}

func TestStatusJSONIsTheAgentResponse(t *testing.T) {
	response := &agentpbv2.DataStatusResponse{ReportsInProgress: true, Active: recordingEpisode(), InProgress: []*agentpbv2.DataEpisode{recordingEpisode()}}
	var out, want bytes.Buffer
	if err := runDataStatus(context.Background(), &statusDataClient{status: response}, &out, true, statusTestNow); err != nil {
		t.Fatal(err)
	}
	if err := encodeProtoJSON(&want, response); err != nil {
		t.Fatal(err)
	}
	if out.String() != want.String() {
		t.Fatalf("got %s want %s", out.String(), want.String())
	}
	for _, key := range []string{`"active":`, `"in_progress":[`, `"reports_in_progress":true`, `"duration_nanos":`, `"sources":[`} {
		if !strings.Contains(out.String(), key) {
			t.Fatalf("missing %s in %s", key, out.String())
		}
	}
}

func TestRecordNextStepsNamesStopAndStatus(t *testing.T) {
	var out bytes.Buffer
	printRecordNextSteps(&out, "")
	got := out.String()
	if strings.Count(got, "\n") != 1 || !strings.Contains(got, "wendy data stop") || !strings.Contains(got, "wendy data status") || strings.Contains(got, "--device") {
		t.Fatalf("got %q", got)
	}
}

// TestRecordNextStepsKeepTheDevice: a recording started with --device must be
// stopped on that device. Without the flag, the copied `wendy data stop` would
// go to the saved default device and stop whatever that one is recording.
func TestRecordNextStepsKeepTheDevice(t *testing.T) {
	for device, want := range map[string]string{
		"robot-b.local":  "--device robot-b.local",
		"workshop bench": "--device 'workshop bench'",
	} {
		var out bytes.Buffer
		printRecordNextSteps(&out, device)
		got := out.String()
		if !strings.Contains(got, "wendy data stop "+want) || !strings.Contains(got, "wendy data status "+want) {
			t.Fatalf("device %q: got %q, want both commands to carry %s", device, got, want)
		}
	}
}
