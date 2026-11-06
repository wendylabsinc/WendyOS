package commands

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	agentpb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
)

// validatingContainerClient answers ValidateContainer from refusals, keyed by
// the request's AppName, and records the names it was asked about.
type validatingContainerClient struct {
	agentpb.WendyContainerServiceClient // embedded nil — satisfies interface

	refusals  map[string]error
	validated []string
}

func (f *validatingContainerClient) ValidateContainer(_ context.Context, in *agentpb.CreateContainerRequest, _ ...grpc.CallOption) (*agentpb.ValidateContainerResponse, error) {
	f.validated = append(f.validated, in.GetAppName())
	if err := f.refusals[in.GetAppName()]; err != nil {
		return nil, err
	}
	return &agentpb.ValidateContainerResponse{}, nil
}

func requestNamedAfterService(name string) (*agentpb.CreateContainerRequest, error) {
	return &agentpb.CreateContainerRequest{AppName: "app_" + name}, nil
}

func TestValidateServicesBeforeReplaceChecksEveryService(t *testing.T) {
	client := &validatingContainerClient{}

	err := validateServicesBeforeReplace(context.Background(), client, []string{"db", "api", "web"}, requestNamedAfterService)
	if err != nil {
		t.Fatalf("validateServicesBeforeReplace = %v, want nil", err)
	}
	if want := []string{"app_db", "app_api", "app_web"}; !reflect.DeepEqual(client.validated, want) {
		t.Fatalf("validated %v, want %v", client.validated, want)
	}
}

// Every refusal is reported, not only the first, so one deploy shows the user
// all the services to fix.
func TestValidateServicesBeforeReplaceReportsEveryRefusal(t *testing.T) {
	client := &validatingContainerClient{refusals: map[string]error{
		"app_api": status.Error(codes.FailedPrecondition, `container would fail to create: env key "LD_PRELOAD" is reserved`),
		"app_web": status.Error(codes.FailedPrecondition, "container would fail to create: getting/pulling image"),
	}}

	err := validateServicesBeforeReplace(context.Background(), client, []string{"db", "api", "web"}, requestNamedAfterService)
	if err == nil {
		t.Fatal("validateServicesBeforeReplace = nil, want the refusals")
	}
	for _, want := range []string{"none of the 3 services was replaced", "api", "LD_PRELOAD", "web", "getting/pulling image"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err, want)
		}
	}
	if strings.Contains(err.Error(), "db") {
		t.Errorf("error %q blames db, which the agent accepted", err)
	}
	if len(client.validated) != 3 {
		t.Fatalf("validated %v, want all three services", client.validated)
	}
}

// An agent that predates ValidateContainer keeps the old service-by-service
// deploy rather than failing it.
func TestValidateServicesBeforeReplaceSkipsOlderAgent(t *testing.T) {
	client := &validatingContainerClient{refusals: map[string]error{
		"app_db": status.Error(codes.Unimplemented, "method ValidateContainer not implemented"),
	}}

	err := validateServicesBeforeReplace(context.Background(), client, []string{"db", "api"}, requestNamedAfterService)
	if err != nil {
		t.Fatalf("validateServicesBeforeReplace = %v, want nil for an older agent", err)
	}
	if want := []string{"app_db"}; !reflect.DeepEqual(client.validated, want) {
		t.Fatalf("validated %v, want only the first probe %v", client.validated, want)
	}
}

func TestValidateServicesBeforeReplaceReturnsRequestError(t *testing.T) {
	buildErr := errors.New("marshaling config for service api")
	client := &validatingContainerClient{}

	err := validateServicesBeforeReplace(context.Background(), client, []string{"api"}, func(string) (*agentpb.CreateContainerRequest, error) {
		return nil, buildErr
	})
	if !errors.Is(err, buildErr) {
		t.Fatalf("validateServicesBeforeReplace = %v, want %v", err, buildErr)
	}
	if len(client.validated) != 0 {
		t.Fatal("a request that could not be built was validated")
	}
}
