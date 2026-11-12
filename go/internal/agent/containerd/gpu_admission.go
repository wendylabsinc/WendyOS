package containerd

import (
	"context"
	"fmt"

	"github.com/wendylabsinc/wendy/go/internal/agent/hoststats"
	"github.com/wendylabsinc/wendy/go/internal/shared/appconfig"
)

// AppCgroups resolves trusted containerd app labels, never process environment
// claims or a caller-supplied path. Stopped containers own no GPU processes.
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
