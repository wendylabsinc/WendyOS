package containerd

import (
	"fmt"

	specs "github.com/opencontainers/runtime-spec/specs-go"
	"go.uber.org/zap"

	"github.com/wendylabsinc/wendy/go/internal/agent/appcgroup"
	"github.com/wendylabsinc/wendy/go/internal/agent/logfields"
	localoci "github.com/wendylabsinc/wendy/go/internal/agent/oci"
)

// SetAppMemoryBudget wires the function that configures the shared app memory
// budget. Called once from main.go before any container starts.
func (c *Client) SetAppMemoryBudget(ensure func() appcgroup.Status) {
	c.appMemoryBudget = ensure
}

// EnsureAppMemoryBudget applies the budget and logs its state when that
// differs from the last log. It returns false when no budget is wired.
func (c *Client) EnsureAppMemoryBudget() (appcgroup.Status, bool) {
	if c.appMemoryBudget == nil {
		return appcgroup.Status{}, false
	}
	// Held across the ensure call so concurrent starts log in apply order.
	c.appMemoryBudgetMu.Lock()
	defer c.appMemoryBudgetMu.Unlock()
	st := c.appMemoryBudget()
	errText := ""
	if st.Err != nil {
		errText = st.Err.Error()
	}
	// Err is a fresh value on every call, so compare its text, not the Status.
	key := fmt.Sprintf("%t|%d|%d|%q|%q", st.Limited, st.Max, st.Reserve, st.Note, errText)
	if key == c.appMemoryBudgetLogged {
		return st, true
	}
	c.appMemoryBudgetLogged = key
	msg := "apps run without a shared memory budget"
	var fields []zap.Field
	if st.Limited {
		msg = "app memory budget applied"
		fields = append(fields, zap.Uint64("max_bytes", st.Max), zap.Uint64("reserve_bytes", st.Reserve))
	}
	if st.Note != "" {
		fields = append(fields, zap.String("note", st.Note))
	}
	if st.Err != nil {
		fields = append(fields, zap.Error(st.Err))
	}
	if st.Limited && st.Note == "" && st.Err == nil {
		c.logger.Info(msg, fields...)
	} else {
		c.logger.Warn(msg, fields...)
	}
	return st, true
}

// applyAppResourcePolicy refreshes the budget and the spec before a start.
// Budget problems are only logged: an app always starts, with or without it.
func (c *Client) applyAppResourcePolicy(spec *specs.Spec, appID, serviceName string) bool {
	unified := appcgroup.Unified()
	if st, ok := c.EnsureAppMemoryBudget(); ok {
		unified = st.Unified
	}
	changed, err := refreshAppResourcePolicy(spec, appID, serviceName, unified)
	if err != nil {
		c.logger.Warn("could not apply the app resource policy", zap.String(logfields.AppID, appID), zap.Error(err))
		return false
	}
	return changed
}

// refreshAppResourcePolicy moves a saved spec to the current placement and
// score. It changes the next task only; a running task keeps its cgroup.
func refreshAppResourcePolicy(spec *specs.Spec, appID, serviceName string, unified bool) (bool, error) {
	if spec == nil || spec.Linux == nil || spec.Process == nil {
		return false, fmt.Errorf("app spec lacks Linux or process configuration")
	}
	changed := false
	if path := appcgroup.ContainerPath(appID, serviceName, unified); spec.Linux.CgroupsPath != path {
		spec.Linux.CgroupsPath = path
		changed = true
	}
	if spec.Process.OOMScoreAdj == nil || *spec.Process.OOMScoreAdj != 0 {
		score := 0
		spec.Process.OOMScoreAdj = &score
		changed = true
	}
	return changed, nil
}

// placeROS2Helper moves an agent-started ROS 2 helper into the app budget,
// replacing any path already set. Without cgroup v2 the spec is left alone.
func placeROS2Helper(spec *localoci.Spec, name string, unified bool) {
	path, ok := appcgroup.HelperPath(name, unified)
	if !ok {
		return
	}
	if spec.Linux == nil {
		spec.Linux = &localoci.Linux{}
	}
	spec.Linux.CgroupsPath = path
}

// ros2HelperUnified fails open: a budget problem never stops a helper.
func (c *Client) ros2HelperUnified() bool {
	if st, ok := c.EnsureAppMemoryBudget(); ok {
		return st.Unified
	}
	return appcgroup.Unified()
}
