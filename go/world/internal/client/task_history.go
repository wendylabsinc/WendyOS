package client

import (
	"context"
	"errors"
	"github.com/wendylabsinc/wendy/go/world/internal/taskhistory"
	"net/url"
	"strconv"
)

func (c *Client) TaskHistory(ctx context.Context, origin, task string, limit int, after string) (taskhistory.List, error) {
	var out taskhistory.List
	if origin == "" || len(origin) > 256 || len(task) > 256 || limit < 1 || limit > 100 || len(after) > 2048 {
		return out, errors.New("invalid history selection")
	}
	q := url.Values{"origin": {origin}, "task_id": {task}, "limit": {strconv.Itoa(limit)}, "after": {after}}
	err := c.readStrict(ctx, "/v1/task-history?"+q.Encode(), &out)
	if err == nil && (out.Origin != origin || out.TaskID != task || !out.Historical || len(out.Events) > limit) {
		return taskhistory.List{}, errors.New("history response scope mismatch")
	}
	for _, v := range out.Events {
		if v.Executable || (task != "" && v.Event.TaskID != task) || v.Receipt.Audience != origin || v.Receipt.RunID != out.RunID || v.Event.Sequence != v.Receipt.Sequence {
			return taskhistory.List{}, errors.New("history event scope mismatch")
		}
	}
	return out, err
}
