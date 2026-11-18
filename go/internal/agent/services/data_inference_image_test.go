package services

import (
	"context"
	"testing"

	"github.com/wendylabsinc/wendy/go/internal/agent/data"
	"github.com/wendylabsinc/wendy/go/internal/agent/inference"
	agentpbv2 "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
)

type workerImageFactory struct{ info inference.WorkerImageInfo }

func (workerImageFactory) Start(context.Context, data.CampaignInference) (inference.Session, error) {
	return nil, nil
}
func (f workerImageFactory) WorkerImage(context.Context) inference.WorkerImageInfo { return f.info }

func TestInferenceWorkerImageIsWhatTheFactoryNeeds(t *testing.T) {
	ask := func(service *DataService) *agentpbv2.DataInferenceWorkerImageResponse {
		t.Helper()
		response, err := service.InferenceWorkerImage(context.Background(), &agentpbv2.DataInferenceWorkerImageRequest{})
		if err != nil {
			t.Fatal(err)
		}
		return response
	}
	info := inference.WorkerImageInfo{Repository: "wendy-inference-worker", DepsHash: "62dfd59926b6a643"}
	missing := ask(&DataService{inference: &campaignInferenceManager{factory: workerImageFactory{info}}})
	if missing.GetRepository() != info.Repository || missing.GetDepsHash() != info.DepsHash || missing.GetPresent() {
		t.Fatalf("an agent without the image answered %v", missing)
	}
	info.Present = true
	if present := ask(&DataService{inference: &campaignInferenceManager{factory: workerImageFactory{info}}}); !present.GetPresent() {
		t.Fatalf("an agent with the image answered %v", present)
	}
	// An agent whose worker never runs in a container asks for no image: one
	// with only the process factory, such as a macOS agent's, or with none.
	for name, service := range map[string]*DataService{
		"process factory": {inference: &campaignInferenceManager{factory: &inference.ManagedFactory{}}},
		"no inference":    {},
	} {
		if response := ask(service); response.GetRepository() != "" || response.GetPresent() {
			t.Fatalf("%s answered %v", name, response)
		}
	}
}
