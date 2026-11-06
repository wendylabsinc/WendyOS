package containerd

import (
	"context"
	"time"

	containerd "github.com/containerd/containerd/v2/client"
	"go.uber.org/zap"
)

// Bound background containerd RPCs and CNI checks independently of the live
// task watcher, which must survive successful recovery. This is a maintenance
// budget, not the deadline for an application connection.
const meshIngressRecoveryTimeout = 30 * time.Second

func (c *Client) retryRunningMeshIngress(ctr containerd.Container, retries int) {
	select {
	case <-c.meshIngressStop:
		return
	default:
	}
	ctx, cancel := context.WithTimeout(c.withNamespace(context.Background()), meshIngressRecoveryTimeout)
	defer cancel()
	go func() {
		select {
		case <-c.meshIngressStop:
			cancel()
		case <-ctx.Done():
		}
	}()
	fresh, err := ctr.Labels(ctx)
	if err != nil {
		if ctx.Err() != context.Canceled {
			c.logger.Warn("wendy net ingress: could not refresh inherited task labels",
				zap.String("container_id", ctr.ID()), zap.Error(err))
		}
		return
	}
	if ctx.Err() == nil {
		c.rehydrateRunningMeshIngressAttempt(ctx, ctr, fresh, retries)
	}
}
