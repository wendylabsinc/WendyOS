package services

import (
	"context"
	"encoding/json"
	"os"
	"testing"

	"github.com/wendylabsinc/wendy/go/internal/agent/acmeenroll"
	agentpb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
	agentpbv2 "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
	"go.uber.org/zap"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
)

// contractStep is one row of Proto/conformance/agent-provisioning-v2, which the
// Swift macOS agent's tests replay too.
type contractStep struct {
	Name     string          `json:"name"`
	Method   string          `json:"method"`
	Request  json.RawMessage `json:"request"`
	Code     codes.Code      `json:"code"`
	Message  *string         `json:"message"`
	Response json.RawMessage `json:"response"`
}

func TestProvisioningV2Contract(t *testing.T) {
	data, err := os.ReadFile("../../../../Proto/conformance/agent-provisioning-v2/contract.json")
	if err != nil {
		t.Fatal(err)
	}
	var contract struct{ Steps []contractStep }
	if err := json.Unmarshal(data, &contract); err != nil {
		t.Fatal(err)
	}
	stubACMEEnroll(t, func(context.Context, acmeenroll.Config, string, []byte) (string, string, error) {
		return "leaf", "chain", nil
	})
	v1 := NewProvisioningService(zap.NewNop(), t.TempDir())
	v2 := NewProvisioningServiceV2(v1)
	ctx := context.Background()

	for _, step := range contract.Steps {
		var req, want proto.Message
		var call func() (proto.Message, error)
		switch step.Method {
		case "IsProvisioned":
			r := &agentpbv2.IsProvisionedRequest{}
			req, want = r, &agentpbv2.IsProvisionedResponse{}
			call = func() (proto.Message, error) { return v2.IsProvisioned(ctx, r) }
		case "StartACMEProvisioning":
			r := &agentpbv2.StartACMEProvisioningRequest{}
			req, want = r, &agentpbv2.StartACMEProvisioningResponse{}
			call = func() (proto.Message, error) { return v2.StartACMEProvisioning(ctx, r) }
		case "Unprovision":
			r := &agentpbv2.UnprovisionRequest{}
			req, want = r, &agentpbv2.UnprovisionResponse{}
			call = func() (proto.Message, error) { return v2.Unprovision(ctx, r) }
		case "V1Unprovision":
			r := &agentpb.UnprovisionRequest{}
			req, want = r, &agentpb.UnprovisionResponse{}
			call = func() (proto.Message, error) { return v1.Unprovision(ctx, r) }
		case "RevokeACMECertificate", "CheckACMERevocation":
			r := &agentpbv2.RevokeACMECertificateRequest{}
			req, want = r, &agentpbv2.RevokeACMECertificateResponse{}
			call = func() (proto.Message, error) { return v2.RevokeACMECertificate(ctx, r) }
			if step.Method == "CheckACMERevocation" {
				call = func() (proto.Message, error) { return v2.CheckACMERevocation(ctx, r) }
			}
		default:
			t.Fatalf("%s: unknown method %q", step.Name, step.Method)
		}
		if err := protojson.Unmarshal(step.Request, req); err != nil {
			t.Fatalf("%s: request: %v", step.Name, err)
		}

		got, err := call()
		st := status.Convert(err)
		if st.Code() != step.Code || (step.Message != nil && st.Message() != *step.Message) {
			t.Fatalf("%s: got %v %q, want %v %v", step.Name, st.Code(), st.Message(), step.Code, step.Message)
		}
		if step.Response == nil {
			continue
		}
		if err := protojson.Unmarshal(step.Response, want); err != nil {
			t.Fatalf("%s: response: %v", step.Name, err)
		}
		if !proto.Equal(got, want) {
			t.Fatalf("%s: got %v, want %v", step.Name, got, want)
		}
	}
}
