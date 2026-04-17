package commands

import (
	"context"
	"fmt"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/wendylabsinc/wendy/internal/cli/tui"
	agentpb "github.com/wendylabsinc/wendy/proto/gen/agentpb"
)

func TestUnpackProgressTitleForPullingPhase(t *testing.T) {
	progress := &agentpb.CreateContainerProgress{
		Phase: agentpb.CreateContainerProgress_UNPACKING,
	}

	if got := unpackProgressTitle(progress); got != "Pulling image on device..." {
		t.Fatalf("title = %q; want pull title", got)
	}
}

func TestUnpackProgressTitleAndPercentForLayerUpdates(t *testing.T) {
	progress := &agentpb.CreateContainerProgress{
		Phase:          agentpb.CreateContainerProgress_APPLYING_LAYER,
		LayerIndex:     1,
		TotalLayers:    4,
		ReusedSnapshot: true,
	}

	if got := unpackProgressTitle(progress); got != "Unpacking image on device... (2/4 layers, reused snapshot)" {
		t.Fatalf("title = %q; want layer detail title", got)
	}

	if got := unpackProgressPercent(progress); got != 0.5 {
		t.Fatalf("percent = %v; want 0.5", got)
	}
}

func TestProgressModelUserCancelled(t *testing.T) {
	model, _ := tui.NewProgress("Unpacking...").Update(tea.KeyMsg{Type: tea.KeyCtrlC})
	if !progressModelUserCancelled(model) {
		t.Fatal("expected direct ctrl+c cancellation to be treated as user cancellation")
	}
}

func TestProgressModelUserCancelledIgnoresWrappedContextCanceled(t *testing.T) {
	model, _ := tui.NewProgress("Unpacking...").Update(tui.ProgressDoneMsg{
		Err: fmt.Errorf("creating container: %w", context.Canceled),
	})
	if progressModelUserCancelled(model) {
		t.Fatal("expected wrapped context cancellation to not be treated as direct user cancellation")
	}
}
