package services

import (
	"fmt"
	"os/exec"
	"strings"
	"sync"

	"go.uber.org/zap"
	"google.golang.org/grpc/status"

	"github.com/wendylabsinc/wendy/go/internal/agent/audio"
	"github.com/wendylabsinc/wendy/go/internal/agent/sandbox"
)

// cameraSandboxEnv chooses how camera GStreamer subprocesses are started:
//
//   - unset or "on": without root. A host that cannot do that (see
//     sandbox.Lookup) is logged once and its pipelines run as root, as they did
//     before the sandbox existed.
//   - "require": without root, and a host that cannot do that streams nothing.
//   - "off": as root.
//
// The default falls back rather than refusing because the sandbox has not been
// run on every board and distribution the agent supports, and a host that
// cannot set it up is a host configuration fact, not something a camera or a
// network peer can cause. A pipeline that starts sandboxed and then fails is
// never retried as root: that failure is one a hostile camera could cause.
const cameraSandboxEnv = "WENDY_CAMERA_SANDBOX"

// cameraSandboxGroups are the groups that own the device nodes a capture or
// encode pipeline opens. They are the ones the camera and gpu entitlements give
// a non-root app container (oci.applyCamera): video for V4L2 and media nodes,
// render for DRM render nodes.
var cameraSandboxGroups = []string{"video", "render"}

// sandboxLookup is behind a var so tests can make the host refuse a sandbox.
var sandboxLookup = sandbox.Lookup

// cameraSandbox starts camera GStreamer subprocesses without root. A nil
// cameraSandbox starts them unchanged.
type cameraSandbox struct {
	require bool
	// device is who a pipeline runs as, session who a PipeWire client runs as.
	// Each resolves once: ponytail: a failed lookup stays failed until the agent
	// restarts; retry it if a host turns out to fail transiently.
	device, session func() (*sandbox.Identity, error)
}

func newCameraSandbox(logger *zap.Logger, mode string) *cameraSandbox {
	switch mode {
	case "off":
		logger.Warn("camera sandbox is off: camera pipelines run as root", zap.String("env", cameraSandboxEnv))
		return nil
	case "", "on", "require":
	default:
		logger.Warn("unknown camera sandbox mode; using on", zap.String("env", cameraSandboxEnv), zap.String("value", mode))
	}
	lookup := func(user string) func() (*sandbox.Identity, error) {
		return sync.OnceValues(func() (*sandbox.Identity, error) {
			id, err := sandboxLookup(user, cameraSandboxGroups...)
			switch {
			case err != nil && mode != "require":
				logger.Warn("camera sandbox unavailable: these camera pipelines run as root; set "+cameraSandboxEnv+"=require to refuse instead",
					zap.String("user", user), zap.Error(err))
			case id != nil:
				logger.Info("camera pipelines run without root; if a camera that worked before now fails, set "+cameraSandboxEnv+"=off in /etc/default/wendy-agent",
					zap.Stringer("run_as", id), zap.Strings("groups", cameraSandboxGroups))
			}
			return id, err
		})
	}
	return &cameraSandbox{require: mode == "require", device: lookup("nobody"), session: lookup(audio.SessionUser)}
}

// identity is who the next pipeline runs as; nil means unchanged.
func (c *cameraSandbox) identity(session bool) (*sandbox.Identity, error) {
	if c == nil {
		return nil, nil
	}
	if session {
		return c.session()
	}
	return c.device()
}

// start is cmd.Start without root for the child.
func (c *cameraSandbox) start(cmd *exec.Cmd, session bool) error {
	id, err := c.identity(session)
	if err != nil && c.require {
		// Start reports Err itself, after closing any pipes made for cmd.
		cmd.Err = fmt.Errorf("camera sandbox unavailable and %s=require: %w", cameraSandboxEnv, err)
	}
	if err := id.Start(cmd); err != nil {
		if id != nil {
			return fmt.Errorf("%w (started as %s; %s=off starts camera pipelines as root)", err, id, cameraSandboxEnv)
		}
		return err
	}
	return nil
}

// explain names the sandbox in a sandboxed pipeline's permission failure, the
// failure it causes on a host whose device nodes the sandbox user cannot open.
func (c *cameraSandbox) explain(err error, stderr string, session bool) error {
	// LC_ALL=C pins strerror's wording; GStreamer prints it mid-sentence.
	if err == nil || !strings.Contains(stderr, "ermission denied") {
		return err
	}
	st := status.Convert(err)
	id, _ := c.identity(session)
	if id == nil || len(st.Details()) != 0 {
		return err
	}
	return status.Errorf(st.Code(), "%s The pipeline ran without root as %s; if this camera worked before, set %s=off in /etc/default/wendy-agent and restart the agent.",
		st.Message(), id, cameraSandboxEnv)
}
