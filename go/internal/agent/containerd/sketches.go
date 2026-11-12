package containerd

import (
	"context"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/wendylabsinc/wendy/go/internal/shared/appconfig"
)

// SketchOwnership ties the microcontroller's one sketch to app lifecycle.
type SketchOwnership interface {
	// AnySketchApp is false on devices where no app has a sketch.
	AnySketchApp() bool
	// SketchStartRefusal says why appID must not start now, or "": an app
	// with a sketch only works while its own sketch is on the chip.
	SketchStartRefusal(appID string) string
	// ForgetApp removes a deleted app's sketch.
	ForgetApp(ctx context.Context, appID string)
}

// SetSketchOwnership wires the sketch owner in. Wire it before starting containers.
func (c *Client) SetSketchOwnership(s SketchOwnership) {
	c.sketches = s
}

// checkSketchBeforeStart refuses to start an app whose sketch is not on the
// chip. It only reads: a start never flashes and never stops another app.
func (c *Client) checkSketchBeforeStart(ctx context.Context, appName, appID string) error {
	if c.sketches == nil || !c.sketches.AnySketchApp() {
		return nil
	}
	nsCtx := c.withNamespace(ctx)
	// The name alone is ambiguous for a service container; the label is not.
	if ctr, err := c.client.LoadContainer(nsCtx, appName); err == nil {
		if labels, err := ctr.Labels(nsCtx); err == nil {
			if id := labels[labelKeyAppID]; id != "" && appconfig.ValidateAppID(id) == nil {
				appID = id
			}
		}
	}
	if why := c.sketches.SketchStartRefusal(appID); why != "" {
		return status.Error(codes.FailedPrecondition, why)
	}
	return nil
}
