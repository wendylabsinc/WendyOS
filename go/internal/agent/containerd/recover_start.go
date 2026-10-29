package containerd

import (
	"context"
	"errors"
	"fmt"
	"time"

	containerdclient "github.com/containerd/containerd/v2/client"
	"github.com/containerd/errdefs"
	"go.uber.org/zap"
)

// RecoverInterruptedTaskStarts retires tasks whose entrypoints were still held
// when the previous agent died. In-memory rollback callbacks cannot survive
// SIGKILL. Clean the task, CNI allocation and anchored namespace independently
// of restart policy; boot reconciliation decides whether to start it again.
func (c *Client) RecoverInterruptedTaskStarts(ctx context.Context) error {
	ctx = c.withNamespace(ctx)
	ctrs, err := c.client.Containers(ctx, fmt.Sprintf("labels.%q", labelKeyAppVersion))
	if err != nil {
		return err
	}
	var failures []error
	for _, ctr := range ctrs {
		if err := c.recoverInterruptedTaskStart(ctx, ctr); err != nil {
			failures = append(failures, fmt.Errorf("%s: %w", ctr.ID(), err))
		}
	}
	return errors.Join(failures...)
}

func (c *Client) recoverInterruptedTaskStart(ctx context.Context, ctr containerdclient.Container) error {
	// A concurrent normal start holds this same lock through task.Start().
	// Inspect status only after acquiring it, so boot recovery cannot delete a
	// task that another operation is actively preparing.
	unlock := c.lockNetworkOperation(ctr.ID())
	defer unlock()
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	labels, err := ctr.Labels(ctx)
	if err != nil {
		return err
	}
	if _, _, err := containerOwnerFromLabels(ctr.ID(), labels); err != nil {
		return err
	}
	task, err := ctr.Task(ctx, nil)
	if errdefs.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return err
	}
	status, err := task.Status(ctx)
	if err != nil || status.Status != containerdclient.Created {
		return err
	}
	if err := c.stopOne(ctx, ctr.ID()); err != nil {
		return err
	}
	c.logger.Warn("Recovered interrupted container startup", zap.String("container_id", ctr.ID()))
	return nil
}
