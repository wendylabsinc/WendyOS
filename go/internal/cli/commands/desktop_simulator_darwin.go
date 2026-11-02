//go:build darwin && !ios

package commands

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/spf13/cobra"
	wendymcp "github.com/wendylabsinc/wendy/go/internal/cli/mcp"
	"github.com/wendylabsinc/wendy/go/internal/cli/vm"
)

// DesktopSimulators reuses the CLI's image, firmware, lifecycle and readiness
// checks. The native app does not maintain a second simulator registry.
func DesktopSimulators() wendymcp.SimulatorBackend {
	b := simulatorBackend()
	start := b.Start
	b.Start = func(ctx context.Context, name string) (*wendymcp.SimulatorInfo, error) {
		store, err := vm.NewStore()
		if err != nil {
			return nil, err
		}
		profile, exists, err := store.ReadRobotProfile(name)
		if err != nil {
			return nil, err
		}
		if exists {
			runtime, err := robotRuntimeForKind(profile.Kind)
			if err != nil {
				return nil, err
			}
			if err := runtime.validateSource(name, profile); err != nil {
				return nil, err
			}
		}
		return start(ctx, name)
	}
	return b
}

// Recovery is derived from the actual CLI error type, never from display text.
func DesktopSimulatorRecovery(err error) int {
	var source *robotSourceMismatchError
	if errors.As(err, &source) {
		return 1
	}
	var agent *robotAgentCapabilityError
	if errors.As(err, &agent) {
		return 2
	}
	return 0
}

func DesktopUpdateSimulatorRuntime(ctx context.Context, name string) error {
	cmd := &cobra.Command{}
	cmd.SetContext(ctx)
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	return runVMRobot(cmd, "update", name)
}

// DesktopSimulatorRequest resolves only an owned local VM's robot endpoint and
// verifies its runtime identity before reading geometry or changing the world.
func DesktopSimulatorRequest(ctx context.Context, name, path string) (*http.Response, error) {
	if err := vm.ValidName(name); err != nil {
		return nil, err
	}
	method := http.MethodGet
	switch path {
	case "/api/scene", "/api/scene/state":
	case "/api/pause", "/api/resume", "/api/reset":
		method = http.MethodPost
	default:
		return nil, fmt.Errorf("unsupported simulator operation")
	}
	_, profile, port, err := robotEndpoint(ctx, name)
	if err != nil {
		return nil, err
	}
	state, err := readRobotStatus(ctx, port)
	if err != nil {
		return nil, err
	}
	if err = state.matches(name, profile); err != nil {
		return nil, err
	}
	path, body := desktopSimulatorControl(profile.Kind, path)
	req, err := http.NewRequestWithContext(ctx, method, robotURL(port)+path, body)
	if err != nil {
		return nil, err
	}
	if method == http.MethodPost {
		req.Header.Set("Content-Type", "application/json")
	}
	return robotHTTPClient.Do(req)
}

func desktopSimulatorControl(profile, path string) (string, io.Reader) {
	if (profile == "so101" || profile == "rosmaster-r2") && (path == "/api/pause" || path == "/api/resume") {
		return "/api/pause", strings.NewReader(fmt.Sprintf(`{"paused":%t}`, path == "/api/pause"))
	}
	return path, nil
}
