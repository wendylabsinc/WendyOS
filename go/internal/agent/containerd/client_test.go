package containerd

import (
	"testing"

	agentpb "github.com/wendylabsinc/wendy/proto/gen/agentpb"
)

func TestCreateContainerProgressMappingUsesApplyPhase(t *testing.T) {
	progress := UnpackProgress{
		Phase:       "layer",
		LayerIndex:  2,
		TotalLayers: 5,
		LayerSize:   1234,
		Reused:      true,
	}

	got := toCreateContainerProgress(progress)

	if got.GetPhase() != agentpb.CreateContainerProgress_APPLYING_LAYER {
		t.Fatalf("phase = %v; want APPLYING_LAYER", got.GetPhase())
	}
	if got.GetLayerIndex() != 2 {
		t.Fatalf("layer index = %d; want 2", got.GetLayerIndex())
	}
	if got.GetTotalLayers() != 5 {
		t.Fatalf("total layers = %d; want 5", got.GetTotalLayers())
	}
	if got.GetLayerSize() != 1234 {
		t.Fatalf("layer size = %d; want 1234", got.GetLayerSize())
	}
	if !got.GetReusedSnapshot() {
		t.Fatal("expected reused snapshot to be true")
	}
}

func TestCreateContainerProgressMappingUsesUnpackingPhaseForStart(t *testing.T) {
	progress := UnpackProgress{
		Phase:       "start",
		TotalLayers: 3,
	}

	got := toCreateContainerProgress(progress)

	if got.GetPhase() != agentpb.CreateContainerProgress_UNPACKING {
		t.Fatalf("phase = %v; want UNPACKING", got.GetPhase())
	}
	if got.GetTotalLayers() != 3 {
		t.Fatalf("total layers = %d; want 3", got.GetTotalLayers())
	}
	if got.GetLayerIndex() != 0 {
		t.Fatalf("layer index = %d; want 0", got.GetLayerIndex())
	}
}
