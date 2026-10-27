package commands

import (
	"bytes"
	"context"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	agentpbv2 "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// campaignStateAgent answers the campaign calls the way the agent does, for
// one campaign whose state the arm and disarm calls switch. Each command dials
// its own connection, so the handlers run on unrelated goroutines and mu
// orders them.
type campaignStateAgent struct {
	agentpbv2.UnimplementedDataServiceServer
	mu        sync.Mutex
	campaign  *agentpbv2.DataCampaign
	recording bool
}

func (a *campaignStateAgent) withState(state string) *agentpbv2.DataCampaign {
	a.campaign.State = state
	a.campaign.PlanJson = []byte(`{"inference_status": {"state": "` + state + `"}, "name": "c920-yolo-live", "state": "` + state + `"}` + "\n")
	return a.campaign
}

func (a *campaignStateAgent) Campaigns(context.Context, *agentpbv2.DataCampaignsRequest) (*agentpbv2.DataCampaignsResponse, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.campaign == nil {
		return &agentpbv2.DataCampaignsResponse{}, nil
	}
	return &agentpbv2.DataCampaignsResponse{Campaigns: []*agentpbv2.DataCampaign{a.campaign}}, nil
}

func (a *campaignStateAgent) CampaignInspect(_ context.Context, req *agentpbv2.DataCampaignInspectRequest) (*agentpbv2.DataCampaign, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.campaign == nil || req.GetName() != a.campaign.GetName() {
		return nil, status.Errorf(codes.NotFound, "campaign %s is not deployed on this device", req.GetName())
	}
	return a.campaign, nil
}

func (a *campaignStateAgent) CampaignArm(context.Context, *agentpbv2.DataCampaignArmRequest) (*agentpbv2.DataCampaignArmResponse, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	changed := a.campaign.GetState() != "armed"
	return &agentpbv2.DataCampaignArmResponse{Campaign: a.withState("armed"), Changed: changed}, nil
}

func (a *campaignStateAgent) CampaignDisarm(context.Context, *agentpbv2.DataCampaignDisarmRequest) (*agentpbv2.DataCampaignDisarmResponse, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	changed := a.campaign.GetState() != "disarmed"
	return &agentpbv2.DataCampaignDisarmResponse{Campaign: a.withState("disarmed"), Changed: changed,
		Warnings: []string{"campaign c920-yolo-live names fleet lab. It stays disarmed on this device even when the plan is deployed again, until it is armed here"}}, nil
}

func (a *campaignStateAgent) CampaignRemove(_ context.Context, req *agentpbv2.DataCampaignRemoveRequest) (*agentpbv2.DataCampaignRemoveResponse, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.recording && !req.GetForce() {
		return nil, status.Error(codes.FailedPrecondition, "campaign c920-yolo-live is recording episode ep-7; wait for it to be sealed, or remove the campaign with --force to stop and seal that recording first")
	}
	response := &agentpbv2.DataCampaignRemoveResponse{Name: a.campaign.GetName(), Revision: a.campaign.GetRevision()}
	if a.recording {
		response.Sealed = &agentpbv2.DataEpisode{Id: "ep-7", State: "complete"}
	}
	a.campaign = nil
	return response, nil
}

// serveDataOnSocket serves a DataService on a unix socket and points the CLI
// at it through WENDY_AGENT_SOCKET, the on-device route that skips discovery,
// so a test runs the real command against a scripted agent.
func serveDataOnSocket(t *testing.T, server agentpbv2.DataServiceServer) {
	t.Helper()
	// os.MkdirTemp, not t.TempDir: a unix socket path is limited to about a
	// hundred bytes, and t.TempDir's path embeds the test's long name.
	dir, err := os.MkdirTemp("", "wdc")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	socket := filepath.Join(dir, "agent.sock")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	grpcServer := grpc.NewServer()
	agentpbv2.RegisterDataServiceServer(grpcServer, server)
	go func() { _ = grpcServer.Serve(listener) }()
	t.Cleanup(grpcServer.Stop)
	t.Setenv("WENDY_AGENT_SOCKET", socket)
}

func runDataCampaign(t *testing.T, args ...string) (string, error) {
	t.Helper()
	cmd := newDataCampaignCmd()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SilenceErrors, cmd.SilenceUsage = true, true
	cmd.SetArgs(args)
	err := cmd.Execute()
	return out.String(), err
}

func TestDataCampaignDisarmListInspectArm(t *testing.T) {
	agent := &campaignStateAgent{campaign: &agentpbv2.DataCampaign{Name: "c920-yolo-live", State: "armed", Revision: "1a2b3c4d5e6f7a8b9c0d", Fleet: "lab"}}
	serveDataOnSocket(t, agent)

	out, err := runDataCampaign(t, "disarm", "c920-yolo-live")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"Campaign c920-yolo-live: disarmed (revision 1a2b3c4d5e6f)",
		"Its triggers and inference are stopped and its camera pre-roll is released.",
		"Warning: campaign c920-yolo-live names fleet lab.",
		"Arm it again with: wendy data campaign arm c920-yolo-live",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("disarm output lacks %q:\n%s", want, out)
		}
	}

	out, err = runDataCampaign(t, "list")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "c920-yolo-live\tdisarmed\t1a2b3c4d5e6f\tlab") {
		t.Fatalf("list does not show the campaign disarmed:\n%s", out)
	}
	out, err = runDataCampaign(t, "inspect", "c920-yolo-live")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, `"state": "disarmed"`) || !strings.Contains(out, `"inference_status": {"state": "disarmed"}`) {
		t.Fatalf("inspect does not show the campaign and its inference disarmed:\n%s", out)
	}

	if out, err = runDataCampaign(t, "disarm", "c920-yolo-live"); err != nil || !strings.Contains(out, "Campaign c920-yolo-live is already disarmed") {
		t.Fatalf("disarming twice: %v\n%s", err, out)
	}
	out, err = runDataCampaign(t, "arm", "c920-yolo-live")
	if err != nil || !strings.Contains(out, "Campaign c920-yolo-live: armed (revision 1a2b3c4d5e6f)") {
		t.Fatalf("arm: %v\n%s", err, out)
	}
	if out, err = runDataCampaign(t, "list"); err != nil || !strings.Contains(out, "c920-yolo-live\tarmed") {
		t.Fatalf("list after arm: %v\n%s", err, out)
	}
}

func TestDataCampaignRemoveNeedsForceWhileRecording(t *testing.T) {
	agent := &campaignStateAgent{campaign: &agentpbv2.DataCampaign{Name: "c920-yolo-live", State: "armed", Revision: "1a2b3c4d5e6f7a8b9c0d"}, recording: true}
	serveDataOnSocket(t, agent)

	_, err := runDataCampaign(t, "remove", "c920-yolo-live")
	if err == nil || !strings.Contains(err.Error(), "--force") || !strings.Contains(err.Error(), "ep-7") {
		t.Fatalf("remove while recording: %v", err)
	}
	out, err := runDataCampaign(t, "rm", "--force", "c920-yolo-live")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"Stopped and sealed its recording first: episode ep-7 (complete).",
		"Removed campaign c920-yolo-live (revision 1a2b3c4d5e6f).",
		"Its episodes are kept; list them with: wendy data episodes",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("remove output lacks %q:\n%s", want, out)
		}
	}
	if out, err = runDataCampaign(t, "list"); err != nil || strings.Contains(out, "c920-yolo-live") {
		t.Fatalf("list after remove: %v\n%s", err, out)
	}
}

// TestDataCampaignStateCommandsExplainAnOlderAgent: an agent that predates the
// calls answers Unimplemented, which must read as "update the agent" rather
// than as a gRPC error.
func TestDataCampaignStateCommandsExplainAnOlderAgent(t *testing.T) {
	serveDataOnSocket(t, &agentpbv2.UnimplementedDataServiceServer{})
	for _, verb := range []string{"arm", "disarm", "remove"} {
		_, err := runDataCampaign(t, verb, "c920-yolo-live")
		want := "this device's agent does not support `wendy data campaign " + verb + "`; update it with `wendy device update`"
		if err == nil || err.Error() != want {
			t.Fatalf("%s against an older agent: %v, want %q", verb, err, want)
		}
	}
}

func TestDataCampaignStateCommandsJSON(t *testing.T) {
	agent := &campaignStateAgent{campaign: &agentpbv2.DataCampaign{Name: "c920-yolo-live", State: "armed", Revision: "1a2b3c4d5e6f7a8b9c0d"}}
	serveDataOnSocket(t, agent)
	previous := jsonOutput
	t.Cleanup(func() { jsonOutput = previous })
	jsonOutput = true

	out, err := runDataCampaign(t, "disarm", "c920-yolo-live")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(out, `{"campaign":{"name":"c920-yolo-live"`) || !strings.Contains(out, `"state":"disarmed"`) || !strings.Contains(out, `"changed":true`) || strings.Count(out, "\n") != 1 {
		t.Fatalf("disarm --json is not one protobuf JSON line: %q", out)
	}
	out, err = runDataCampaign(t, "remove", "c920-yolo-live")
	if err != nil || !strings.HasPrefix(out, `{"name":"c920-yolo-live","revision":"1a2b3c4d5e6f7a8b9c0d"}`) {
		t.Fatalf("remove --json: %v %q", err, out)
	}
}
