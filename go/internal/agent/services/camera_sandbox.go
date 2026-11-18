package services

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
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
//     sandbox.Isolated) is logged once and its pipelines run as root, as they
//     did before the sandbox existed.
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

// Behind vars so tests can make the host refuse a sandbox.
var (
	sandboxIsolated = sandbox.Isolated
	sandboxLookup   = sandbox.Lookup
)

// gstRegistry is the plugin registry sandboxed pipelines load. Root writes it
// and nothing else can: a registry names the plugin files GStreamer loads, so
// one a pipeline could write would let it choose the code of every later one.
// A var so tests can move it.
var gstRegistry = "/run/wendy-sandbox/gstreamer-registry.bin"

// cameraSandbox starts camera GStreamer subprocesses without root. A nil
// cameraSandbox starts them unchanged.
type cameraSandbox struct {
	logger  *zap.Logger
	require bool
	// device is who a pipeline runs as: a uid of its own each time. session is
	// who a PipeWire client runs as: the session's user, the one non-root
	// identity that can reach its socket, so those pipelines share a uid with
	// each other and with everything else that user runs.
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
	once := func(pipelines string, lookup func() (*sandbox.Identity, error)) func() (*sandbox.Identity, error) {
		return sync.OnceValues(func() (*sandbox.Identity, error) {
			id, err := lookup()
			switch {
			case err != nil && mode != "require":
				logger.Warn("camera sandbox unavailable: these camera pipelines run as root; set "+cameraSandboxEnv+"=require to refuse instead",
					zap.String("pipelines", pipelines), zap.Error(err))
			case id != nil:
				logger.Info("camera pipelines run without root; if a camera that worked before now fails, set "+cameraSandboxEnv+"=off in /etc/default/wendy-agent",
					zap.String("pipelines", pipelines), zap.Stringer("run_as", id), zap.Strings("groups", cameraSandboxGroups))
			}
			return id, err
		})
	}
	return &cameraSandbox{
		logger:  logger,
		require: mode == "require",
		device:  once("device and IP camera", func() (*sandbox.Identity, error) { return sandboxIsolated(cameraSandboxGroups...) }),
		session: once("PipeWire", func() (*sandbox.Identity, error) { return sandboxLookup(audio.SessionUser, cameraSandboxGroups...) }),
	}
}

// registryEnv has GStreamer, as root, bring gstRegistry up to date, and
// returns the environment that makes a pipeline load it and never write one.
// Root scans the plugins before each pipeline anyway (listGSTElements), so
// this hands root nothing new to parse.
//
// Without it a sandboxed pipeline still works: it has no cache it can write,
// so it scans every plugin each time it starts.
func (c *cameraSandbox) registryEnv() []string {
	err := func() error {
		inspect, err := resolveGSTBinary("gst-inspect-1.0")
		if err != nil {
			return err
		}
		if err := os.MkdirAll(filepath.Dir(gstRegistry), 0o755); err != nil {
			return err
		}
		ctx, cancel := context.WithTimeout(context.Background(), gstInspectTimeout)
		defer cancel()
		// Any use of the registry updates it; asking after one core element prints nothing.
		refresh := exec.CommandContext(ctx, inspect, "--exists", "fdsink")
		refresh.Env = append(os.Environ(), "GST_REGISTRY="+gstRegistry)
		if err := refresh.Run(); err != nil {
			return err
		}
		// GStreamer writes it readable by its owner alone.
		return os.Chmod(gstRegistry, 0o644)
	}()
	if err != nil {
		c.logger.Debug("no shared GStreamer registry; the pipeline scans its plugins itself", zap.Error(err))
		return nil
	}
	return []string{"GST_REGISTRY=" + gstRegistry, "GST_REGISTRY_UPDATE=no"}
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

// warm does the slow part of a sandboxed start ahead of a caller's deadline:
// the first one after boot has root scan every plugin for the registry.
func (c *cameraSandbox) warm(session bool) {
	if id, _ := c.identity(session); id != nil {
		c.registryEnv()
	}
}

// start is cmd.Start without root for the child.
func (c *cameraSandbox) start(cmd *exec.Cmd, session bool) error {
	id, err := c.identity(session)
	if err != nil && c.require {
		// Start reports Err itself, after closing any pipes made for cmd.
		cmd.Err = fmt.Errorf("camera sandbox unavailable and %s=require: %w", cameraSandboxEnv, err)
	}
	if id != nil {
		// GIO, which rtspsrc loads, otherwise opens the user's dconf database and
		// complains on stderr when it cannot create one.
		cmd.Env = append(append(cmd.Environ(), "GSETTINGS_BACKEND=memory"), c.registryEnv()...)
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
