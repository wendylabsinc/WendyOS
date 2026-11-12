package services

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"

	"google.golang.org/protobuf/proto"

	"github.com/wendylabsinc/wendy/go/internal/shared/appconfig"
	agentpb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
	cloudpbv2 "github.com/wendylabsinc/wendy/go/proto/gen/cloudpb/v2"
)

// The start step (apps and deployments spec 9, T1/T2): a deployed app runs
// from its release image with the release's app definition (host access:
// entitlements, ports, volumes, resources) and the deployment's
// configuration (named values, as environment variables), through the same
// create and start path `wendy run` uses. The container gets the run path's
// labels and monitor registration, so it restarts on boot and after a crash.

// ReleaseImagePrefix starts every ReleaseImageName.
const ReleaseImagePrefix = "localhost/wendy-release/"

// ReleaseImageName is the containerd image name of an imported release.
func ReleaseImageName(appID, releaseID string) string {
	return ReleaseImagePrefix + appID + ":" + releaseID
}

// AppConfigLabel is the image config label that carries a release's app
// definition (its wendy.json), set at publish. The operator signs the
// manifest digest, which covers the image config and so this label.
const AppConfigLabel = "sh.wendy/app-config"

// maxAppDefinitionSize bounds AppConfigLabel, as config_json is bounded.
const maxAppDefinitionSize = 64 << 10

// ErrNoAppDefinition is a release image without AppConfigLabel.
var ErrNoAppDefinition = errors.New("this release has no app definition; publish it again with a current CLI")

// ReleaseStart is one start of a deployed app.
type ReleaseStart struct {
	AppID, ReleaseID string
	AppConfig        []byte   // the release's app definition
	Env              []string // the deployment's configuration, KEY=value
	Run              bool     // false: install only, left stopped
}

// configEnv turns a configuration (checked by checkConfig) into environment
// entries, sorted by name.
func configEnv(config string) ([]string, error) {
	dec := json.NewDecoder(strings.NewReader(config))
	dec.UseNumber()
	var values map[string]any
	if err := dec.Decode(&values); err != nil {
		return nil, err
	}
	env := make([]string, 0, len(values))
	for name, v := range values {
		if !appconfig.IsEnvVarName(name) {
			return nil, fmt.Errorf("configuration name %q is not a valid environment variable name", name)
		}
		// The agent's own variables and the linkers' are never set by a
		// deployment: refused, not silently dropped.
		for _, prefix := range appconfig.ReservedEnvPrefixes {
			if strings.HasPrefix(strings.ToUpper(name), prefix) {
				return nil, fmt.Errorf("configuration name %q is reserved", name)
			}
		}
		var text string
		switch v := v.(type) {
		case string:
			text = v
		case json.Number:
			text = v.String()
		case bool:
			text = fmt.Sprint(v)
		default:
			return nil, fmt.Errorf("the configuration may hold only named values (text, numbers, true or false); %q holds more", name)
		}
		env = append(env, name+"="+text)
	}
	slices.Sort(env)
	return env, nil
}

// releaseStart builds the start of statement a: the app definition from its
// release image (refused when missing, naming another app, or declaring
// several services) and the configuration as environment.
func (p *Postbox) releaseStart(ctx context.Context, a *cloudpbv2.DeviceAssignment) (ReleaseStart, error) {
	def, err := p.control.ReleaseAppConfig(ctx, a.GetAppId(), a.GetAppReleaseId(), a.GetManifestDigest())
	if err != nil {
		return ReleaseStart{}, pullFailed("app_definition", err)
	}
	if err := checkAppDefinition(def, a.GetAppId()); err != nil {
		return ReleaseStart{}, err
	}
	env, err := configEnv(a.GetConfigJson())
	if err != nil {
		return ReleaseStart{}, pullFailed("config", err)
	}
	return ReleaseStart{
		AppID: a.GetAppId(), ReleaseID: a.GetAppReleaseId(), AppConfig: def, Env: env,
		Run: a.GetDesiredState() == cloudpbv2.Running_RUNNING,
	}, nil
}

// checkAppDefinition holds a release's app definition to at least what a
// `wendy run` definition passes, and stricter where a release cannot be
// asked: bounded, every field known (strictly decoded, no validation
// warning), its app id the statement's, one service. The create path then
// applies the same entitlement checks as for a run. Nothing in it is trusted
// more for coming from a release.
func checkAppDefinition(def []byte, app string) error {
	refuse := func(reason string, err error) error { return pullFailed(reason, err) }
	if len(def) > maxAppDefinitionSize {
		return refuse("app_definition", fmt.Errorf("this release's app definition is %d bytes, over the %d byte limit", len(def), maxAppDefinitionSize))
	}
	dec := json.NewDecoder(bytes.NewReader(def))
	dec.DisallowUnknownFields()
	var cfg appconfig.AppConfig
	if err := dec.Decode(&cfg); err != nil {
		return refuse("app_definition", fmt.Errorf("this release's app definition is not valid: %w", err))
	}
	if dec.More() {
		return refuse("app_definition", errors.New("this release's app definition is not valid: more than one JSON value"))
	}
	if warnings := appconfig.ValidateJSON(def); len(warnings) > 0 {
		return refuse("app_definition", fmt.Errorf("this release's app definition is not valid: %s", warnings[0]))
	}
	switch {
	case cfg.AppID != app:
		return refuse("app_definition", fmt.Errorf("this release is for app %q, not %q", cfg.AppID, app))
	case len(cfg.Services) > 0:
		return refuse("services", errors.New("apps with several services cannot be deployed yet; deploy each service as its own app"))
	}
	if err := appconfig.ValidateAppID(cfg.AppID); err != nil {
		return refuse("app_definition", err)
	}
	return nil
}

// startRelease starts a's app. If it cannot, the statement it replaced is
// started again, so a failed update leaves the old app running.
func (p *Postbox) startRelease(ctx context.Context, a *cloudpbv2.DeviceAssignment) error {
	s, err := p.releaseStart(ctx, a)
	if err == nil {
		if err = p.control.StartRelease(ctx, s); err != nil {
			err = pullFailed("start", err)
		}
	}
	if err == nil {
		return nil
	}
	p.mu.Lock()
	var previous []byte
	if held := p.state.Assignments[a.GetDeploymentId()]; held != nil {
		previous = held.Previous
	}
	p.mu.Unlock()
	prev := &cloudpbv2.DeviceAssignment{}
	if len(previous) == 0 || proto.Unmarshal(previous, prev) != nil {
		return err
	}
	if ps, perr := p.releaseStart(ctx, prev); perr != nil {
		err = errors.Join(err, fmt.Errorf("restoring the previous release: %w", perr))
	} else if perr := p.control.StartRelease(ctx, ps); perr != nil {
		err = errors.Join(err, fmt.Errorf("restoring the previous release: %w", perr))
	}
	return err
}

// HasRelease asks the containerd client when it can tell.
func (c containerControl) HasRelease(ctx context.Context, app, release, manifestDigest string) bool {
	r, ok := c.svc.containerd.(releaseReader)
	return ok && r.HasRelease(ctx, app, release, manifestDigest)
}

// ReleaseAppConfig reads AppConfigLabel from the release image, only when
// that image is the signed manifest digest.
func (c containerControl) ReleaseAppConfig(ctx context.Context, app, release, manifestDigest string) ([]byte, error) {
	r, ok := c.svc.containerd.(releaseReader)
	if !ok {
		return nil, errors.New("this container runtime cannot read registry releases")
	}
	labels, err := r.ReleaseLabels(ctx, app, release, manifestDigest)
	if err != nil {
		return nil, err
	}
	def, ok := labels[AppConfigLabel]
	if !ok || def == "" {
		return nil, ErrNoAppDefinition
	}
	return []byte(def), nil
}

// releaseReader is the containerd capability the start step reads releases
// through (containerd.Client).
//
// Both answer only for the image named ReleaseImageName(app, release) whose
// manifest digest is manifestDigest, the one the operator signed: an image
// under that name with other content is not the release.
type releaseReader interface {
	HasRelease(ctx context.Context, app, release, manifestDigest string) bool
	ReleaseLabels(ctx context.Context, app, release, manifestDigest string) (map[string]string, error)
}

// StartRelease validates the create while the old app still runs, creates
// the container (replacing the app's previous one), then starts it, or for
// install only records an explicit stop so boot does not start it.
func (c containerControl) StartRelease(ctx context.Context, s ReleaseStart) error {
	release := s.ReleaseID
	req := &agentpb.CreateContainerRequest{
		ImageName: ReleaseImageName(s.AppID, s.ReleaseID), AppName: s.AppID, AppConfig: s.AppConfig, Env: s.Env,
		RestartPolicy: &agentpb.RestartPolicy{Mode: agentpb.RestartPolicyMode_UNLESS_STOPPED}, ReleaseId: &release,
	}
	if _, err := c.svc.ValidateContainer(ctx, req); err != nil {
		return err
	}
	if _, err := c.svc.CreateContainer(ctx, req); err != nil {
		return err
	}
	if !s.Run {
		return c.StopApp(ctx, s.AppID)
	}
	return c.StartApp(ctx, s.AppID)
}
