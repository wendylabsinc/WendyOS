package containerd

import (
	"context"
	"fmt"

	containerd "github.com/containerd/containerd/v2/client"

	"github.com/wendylabsinc/wendy/go/internal/agent/hoststats"
	"github.com/wendylabsinc/wendy/go/internal/shared/appconfig"
)

// AppCgroups resolves trusted containerd app labels, never process environment
// claims or a caller-supplied path. Stopped containers own no GPU processes.
// An exited task keeps its PID until the restart monitor deletes it, and that
// PID may already belong to another process, so only live tasks are resolved.
func (c *Client) AppCgroups(ctx context.Context, appID string) ([]string, error) {
	if err := appconfig.ValidateAppID(appID); err != nil {
		return nil, err
	}
	ctx = c.withNamespace(ctx)
	containers, err := c.client.Containers(ctx, fmt.Sprintf("labels.%q==%q", labelKeyAppID, appID))
	if err != nil {
		return nil, err
	}
	var groups []string
	for _, ctr := range containers {
		task, err := ctr.Task(ctx, nil)
		if err != nil {
			continue
		}
		status, err := task.Status(ctx)
		if err != nil {
			return nil, err
		}
		if status.Status != containerd.Running && status.Status != containerd.Paused {
			continue
		}
		group, err := hoststats.ProcessCgroup(task.Pid())
		if err != nil {
			return nil, err
		}
		groups = append(groups, group)
	}
	if len(groups) == 0 {
		return nil, fmt.Errorf("no running app cgroups")
	}
	return groups, nil
}
