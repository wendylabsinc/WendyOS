package commands

import (
	"bytes"
	"context"
	"encoding/json"
	"slices"
	"strings"
	"sync"
	"testing"

	agentpbv2 "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// deleteAgent answers Episodes and DeleteEpisode the way the agent does, over
// a list of finalized episodes and recordings in progress. refuse names
// episodes it fails with FAILED_PRECONDITION, as the agent does for one that
// is being uploaded.
type deleteAgent struct {
	agentpbv2.UnimplementedDataServiceServer
	mu         sync.Mutex
	episodes   []*agentpbv2.DataEpisode
	inProgress []*agentpbv2.DataEpisode
	refuse     map[string]string
	deleted    []string
	// noDelete makes the agent one that predates DeleteEpisode.
	noDelete bool
}

func (a *deleteAgent) Episodes(context.Context, *agentpbv2.DataEpisodesRequest) (*agentpbv2.DataEpisodesResponse, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return &agentpbv2.DataEpisodesResponse{Episodes: a.episodes, InProgress: a.inProgress, ReportsInProgress: true}, nil
}

func (a *deleteAgent) DeleteEpisode(_ context.Context, req *agentpbv2.DataDeleteEpisodeRequest) (*agentpbv2.DataDeleteEpisodeResponse, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.noDelete {
		return nil, status.Error(codes.Unimplemented, "unknown method DeleteEpisode")
	}
	if reason, ok := a.refuse[req.GetEpisode()]; ok {
		return nil, status.Error(codes.FailedPrecondition, reason)
	}
	i := slices.IndexFunc(a.episodes, func(e *agentpbv2.DataEpisode) bool { return e.GetId() == req.GetEpisode() })
	if i < 0 {
		return nil, status.Errorf(codes.NotFound, "no finalized episode %s on this device", req.GetEpisode())
	}
	episode := a.episodes[i]
	a.episodes = slices.Delete(a.episodes, i, i+1)
	a.deleted = append(a.deleted, episode.GetId())
	return &agentpbv2.DataDeleteEpisodeResponse{Episode: episode, UploadState: "pending"}, nil
}

func (a *deleteAgent) deletedIDs() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return slices.Clone(a.deleted)
}

func newDeleteAgent() *deleteAgent {
	return &deleteAgent{
		episodes: []*agentpbv2.DataEpisode{
			{Id: "20261106T151319Z-4c95", Name: "hackathon5-human-detection", State: "complete", SizeBytes: 10_047_544},
			{Id: "20261106T145121Z-f124", Name: "hackathon5-human-detection", State: "complete", SizeBytes: 9_293_239},
		},
	}
}

func runDataDeleteCmd(t *testing.T, args ...string) (string, error) {
	t.Helper()
	cmd := newDataDeleteCmd()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SilenceErrors, cmd.SilenceUsage = true, true
	cmd.SetArgs(args)
	err := cmd.Execute()
	return out.String(), err
}

// asInteractiveTerminal makes the command believe it can prompt, and answers
// the confirmation with confirm.
func asInteractiveTerminal(t *testing.T, confirm bool) *[]string {
	t.Helper()
	originalInteractive, originalConfirm, originalJSON := isInteractiveTerminalFn, confirmDeleteEpisodesFn, jsonOutput
	t.Cleanup(func() {
		isInteractiveTerminalFn, confirmDeleteEpisodesFn, jsonOutput = originalInteractive, originalConfirm, originalJSON
	})
	var asked []string
	isInteractiveTerminalFn = func() bool { return true }
	jsonOutput = false
	confirmDeleteEpisodesFn = func(question string) (bool, error) {
		asked = append(asked, question)
		return confirm, nil
	}
	return &asked
}

func TestDataDeleteForceDeletesTheNamedEpisodes(t *testing.T) {
	agent := newDeleteAgent()
	serveDataOnSocket(t, agent)

	out, err := runDataDeleteCmd(t, "--force", "20261106T151319Z-4c95", "20261106T145121Z-f124", "20261106T151319Z-4c95")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"Deleted episode 20261106T151319Z-4c95 (hackathon5-human-detection, 10.0 MB).",
		"Deleted episode 20261106T145121Z-f124 (hackathon5-human-detection, 9.3 MB).",
		"Deleted 2 episodes, 19.3 MB in all.",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("delete output lacks %q:\n%s", want, out)
		}
	}
	if got := agent.deletedIDs(); !slices.Equal(got, []string{"20261106T151319Z-4c95", "20261106T145121Z-f124"}) {
		t.Fatalf("deleted %v; a repeated ID must be deleted once", got)
	}
}

func TestDataDeleteRefusesWithoutConfirmationWhenNotInteractive(t *testing.T) {
	agent := newDeleteAgent()
	serveDataOnSocket(t, agent)
	original := isInteractiveTerminalFn
	isInteractiveTerminalFn = func() bool { return false }
	t.Cleanup(func() { isInteractiveTerminalFn = original })

	_, err := runDataDeleteCmd(t, "20261106T151319Z-4c95")
	if err == nil || !strings.Contains(err.Error(), "without confirmation") {
		t.Fatalf("delete without --force and without a terminal: %v", err)
	}
	if !IsUsageError(err) {
		t.Fatalf("the refusal should be a usage error: %v", err)
	}
	if got := agent.deletedIDs(); len(got) != 0 {
		t.Fatalf("deleted %v without confirmation", got)
	}
}

func TestDataDeleteAsksBeforeDeleting(t *testing.T) {
	agent := newDeleteAgent()
	serveDataOnSocket(t, agent)

	asked := asInteractiveTerminal(t, false)
	out, err := runDataDeleteCmd(t, "20261106T151319Z-4c95")
	if err != nil {
		t.Fatal(err)
	}
	if len(*asked) != 1 || !strings.Contains((*asked)[0], "Delete episode 20261106T151319Z-4c95?") || !strings.Contains((*asked)[0], "cannot be undone") {
		t.Fatalf("confirmation asked %q", *asked)
	}
	if !strings.Contains(out, "20261106T151319Z-4c95  hackathon5-human-detection  10.0 MB") || !strings.Contains(out, "Cancelled; nothing was deleted.") {
		t.Fatalf("declined delete output:\n%s", out)
	}
	if got := agent.deletedIDs(); len(got) != 0 {
		t.Fatalf("deleted %v after the operator declined", got)
	}

	asked = asInteractiveTerminal(t, true)
	if _, err := runDataDeleteCmd(t, "20261106T151319Z-4c95"); err != nil {
		t.Fatal(err)
	}
	if got := agent.deletedIDs(); !slices.Equal(got, []string{"20261106T151319Z-4c95"}) {
		t.Fatalf("deleted %v after the operator confirmed", got)
	}
}

func TestDataDeleteChecksEveryIDBeforeDeletingAny(t *testing.T) {
	agent := newDeleteAgent()
	agent.inProgress = []*agentpbv2.DataEpisode{{Id: "20261106T160000Z-rec", State: "recording"}}
	serveDataOnSocket(t, agent)

	_, err := runDataDeleteCmd(t, "--force", "20261106T151319Z-4c95", "20261106T000000Z-typo")
	if err == nil || !strings.Contains(err.Error(), "no finalized episode 20261106T000000Z-typo") || !strings.Contains(err.Error(), "nothing was deleted") {
		t.Fatalf("delete with a mistyped ID: %v", err)
	}
	_, err = runDataDeleteCmd(t, "--force", "20261106T151319Z-4c95", "20261106T160000Z-rec")
	if err == nil || !strings.Contains(err.Error(), "still recording") || !strings.Contains(err.Error(), "wendy data stop") {
		t.Fatalf("delete of a recording in progress: %v", err)
	}
	if got := agent.deletedIDs(); len(got) != 0 {
		t.Fatalf("deleted %v although one of the IDs could not be deleted", got)
	}
}

func TestDataDeleteAllLeavesRecordingsInProgress(t *testing.T) {
	agent := newDeleteAgent()
	agent.inProgress = []*agentpbv2.DataEpisode{{Id: "20261106T160000Z-rec", State: "recording"}}
	serveDataOnSocket(t, agent)

	if _, err := runDataDeleteCmd(t, "--all", "20261106T151319Z-4c95"); err == nil || !IsUsageError(err) {
		t.Fatalf("--all with IDs: %v", err)
	}
	out, err := runDataDeleteCmd(t, "--all", "--force")
	if err != nil {
		t.Fatal(err)
	}
	if got := agent.deletedIDs(); len(got) != 2 {
		t.Fatalf("--all deleted %v, want both finalized episodes", got)
	}
	if !strings.Contains(out, "1 recording(s) still in progress were not deleted") {
		t.Fatalf("--all does not say the recording was left:\n%s", out)
	}
	out, err = runDataDeleteCmd(t, "--all", "--force")
	if err != nil || !strings.Contains(out, "No finalized episodes on this device; nothing to delete.") {
		t.Fatalf("--all on an empty device: %v\n%s", err, out)
	}
}

func TestDataDeleteReportsEachEpisodeItCouldNotDelete(t *testing.T) {
	agent := newDeleteAgent()
	agent.refuse = map[string]string{"20261106T151319Z-4c95": "episode is being read: episode 20261106T151319Z-4c95 is being downloaded or uploaded right now; delete it when that has finished"}
	serveDataOnSocket(t, agent)

	out, err := runDataDeleteCmd(t, "--all", "--force")
	if err == nil || !strings.Contains(err.Error(), "could not delete 1 of 2 episode(s)") || !strings.Contains(err.Error(), "20261106T151319Z-4c95: episode is being read") {
		t.Fatalf("partial failure: %v", err)
	}
	if !strings.Contains(out, "Deleted episode 20261106T145121Z-f124") {
		t.Fatalf("the episode that could be deleted was not:\n%s", out)
	}
}

func TestDataDeleteJSONPrintsOneResponsePerEpisode(t *testing.T) {
	agent := newDeleteAgent()
	serveDataOnSocket(t, agent)
	original := jsonOutput
	jsonOutput = true
	t.Cleanup(func() { jsonOutput = original })

	if _, err := runDataDeleteCmd(t, "--all"); err == nil || !strings.Contains(err.Error(), "--force") {
		t.Fatalf("--json without --force must refuse rather than prompt: %v", err)
	}
	out, err := runDataDeleteCmd(t, "--all", "--force")
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) != 2 {
		t.Fatalf("want one JSON line per deleted episode, got:\n%s", out)
	}
	for _, line := range lines {
		var response struct {
			Episode struct {
				ID string `json:"id"`
			} `json:"episode"`
			UploadState string `json:"upload_state"`
		}
		if err := json.Unmarshal([]byte(line), &response); err != nil || response.Episode.ID == "" || response.UploadState != "pending" {
			t.Fatalf("line %q is not a DeleteEpisode response: %v", line, err)
		}
	}
}

func TestDataDeleteExplainsAnOlderAgent(t *testing.T) {
	agent := newDeleteAgent()
	agent.noDelete = true
	serveDataOnSocket(t, agent)

	_, err := runDataDeleteCmd(t, "--force", "20261106T151319Z-4c95")
	want := "this device's agent does not support `wendy data delete`; update it with `wendy device update`"
	if err == nil || err.Error() != want {
		t.Fatalf("delete against an older agent: %v, want %q", err, want)
	}
}
