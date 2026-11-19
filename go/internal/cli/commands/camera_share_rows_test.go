package commands

import (
	"context"
	"testing"
)

// --json prints no status column, so cameraShareRows must not call the agent;
// a nil connection would panic if it did.
func TestCameraShareRows_JSONOutputMakesNoCall(t *testing.T) {
	prev := jsonOutput
	jsonOutput = true
	t.Cleanup(func() { jsonOutput = prev })
	if rows := cameraShareRows(context.Background(), nil); rows != nil {
		t.Errorf("rows = %v, want nil", rows)
	}
}
