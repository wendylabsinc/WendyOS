package cloudenroll

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"testing"

	agentpbv2 "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
)

// statusConn answers every unary call with one recorded IsProvisioned status.
type statusConn struct {
	resp *agentpbv2.IsProvisionedResponse
}

func (c statusConn) Invoke(_ context.Context, _ string, _, reply any, _ ...grpc.CallOption) error {
	proto.Merge(reply.(proto.Message), c.resp)
	return nil
}

func (statusConn) NewStream(context.Context, *grpc.StreamDesc, string, ...grpc.CallOption) (grpc.ClientStream, error) {
	return nil, errors.New("unused")
}

// The agents' answers are pinned by the same contract file (Go and Swift agent
// tests); this proves what the CLI concludes from each of them.
func TestCheckAgentEnrollmentContract(t *testing.T) {
	data, err := os.ReadFile("../../../../Proto/conformance/agent-provisioning-v2/contract.json")
	if err != nil {
		t.Fatal(err)
	}
	var contract struct {
		Steps []struct {
			Name         string          `json:"name"`
			Response     json.RawMessage `json:"response"`
			CLIPreflight string          `json:"cliPreflight"`
		}
	}
	if err := json.Unmarshal(data, &contract); err != nil {
		t.Fatal(err)
	}
	checked := 0
	for _, step := range contract.Steps {
		if step.CLIPreflight == "" {
			continue
		}
		resp := &agentpbv2.IsProvisionedResponse{}
		if err := protojson.Unmarshal(step.Response, resp); err != nil {
			t.Fatalf("%s: %v", step.Name, err)
		}
		got := "ok"
		if err := CheckAgentEnrollment(context.Background(), statusConn{resp}); err != nil {
			got = err.Error()
		}
		if got != step.CLIPreflight {
			t.Fatalf("%s: preflight %q, want %q", step.Name, got, step.CLIPreflight)
		}
		checked++
	}
	if checked == 0 {
		t.Fatal("contract has no preflight steps")
	}
}

// A Mac below macOS 26 says why it cannot enroll; the CLI must stop before Cloud
// mints an EAB and show that reason, escaped, on one line.
func TestCheckAgentEnrollmentShowsUnavailableReason(t *testing.T) {
	supported := false
	resp := &agentpbv2.IsProvisionedResponse{ResponseType: &agentpbv2.IsProvisionedResponse_NotProvisioned{
		NotProvisioned: &agentpbv2.NotProvisionedResponse{
			AcmeEnrollmentSupported:         &supported,
			AcmeEnrollmentUnavailableReason: "direct PKI enrollment needs macOS 26 or later to verify ML-DSA certificate chains; this Mac runs macOS 15.5\n\x1b[2J",
		},
	}}
	err := CheckAgentEnrollment(context.Background(), statusConn{resp})
	want := `this device cannot enroll directly with PKI: "direct PKI enrollment needs macOS 26 or later to verify ML-DSA certificate chains; this Mac runs macOS 15.5\n\x1b[2J" (no Cloud reservation made)`
	if err == nil || err.Error() != want {
		t.Fatalf("preflight error %v, want %s", err, want)
	}
}
