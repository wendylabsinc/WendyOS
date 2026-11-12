package services

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"

	"go.uber.org/zap"
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

// ReleaseImageRef pins a release image to its signed manifest digest: the
// runtime creates a container from it only when the image under the name has
// that digest (checked on the image the container is made from).
func ReleaseImageRef(appID, releaseID, manifestDigest string) string {
	return ReleaseImageName(appID, releaseID) + "@" + manifestDigest
}

// AppContainerState is the app's container as the runtime holds it.
type AppContainerState struct {
	Image         string   // the image name the container was created from
	Env           []string // its process environment
	Present       bool     // a container exists for the app
	Running       bool     // its task runs
	StoppedByUser bool     // stopped explicitly (a user, or install only)
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
	ManifestDigest   string   // the signed digest the release image must have
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
		if appconfig.IsAgentEnvName(name) {
			return nil, fmt.Errorf("configuration name %q is set by the agent itself", name)
		}
		env = append(env, name+"="+text)
	}
	slices.Sort(env)
	// At least as strict as the runtime, so a statement is refused when it
	// arrives rather than failing at start: no NUL, CR or LF, entries of at
	// most 32 KiB, at most 512 of them.
	if err := appconfig.ValidateEnvEntries(env); err != nil {
		return nil, fmt.Errorf("the configuration cannot be applied: %w", err)
	}
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
		AppID: a.GetAppId(), ReleaseID: a.GetAppReleaseId(), ManifestDigest: a.GetManifestDigest(), AppConfig: def, Env: env,
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
	case len(cfg.Services) > 0 || cfg.ServiceName != "":
		return refuse("services", errors.New("apps with several services cannot be deployed yet; deploy each service as its own app"))
	}
	if err := appconfig.ValidateAppID(cfg.AppID); err != nil {
		return refuse("app_definition", err)
	}
	return nil
}

// startRelease starts a's app. If that fails after this attempt replaced the
// app's container, and what it replaced was the last applied statement's
// app, that app is put back. Nothing is put back when nothing was replaced,
// and a directly run app is never replaced by a restore.
func (p *Postbox) startRelease(ctx context.Context, a *cloudpbv2.DeviceAssignment) error {
	before := p.control.AppState(ctx, a.GetAppId())
	s, err := p.releaseStart(ctx, a)
	if err != nil {
		return err
	}
	replaced, err := p.control.StartRelease(ctx, s)
	if err == nil {
		return nil
	}
	err = pullFailed("start", err)
	prev := p.previous(a)
	if !replaced || prev == nil || !before.Present || before.Image != ReleaseImageName(prev.GetAppId(), prev.GetAppReleaseId()) {
		return err
	}
	// The state the user wanted: running (as the statement asks), unless
	// the user had stopped it, not the instant it was observed in.
	if perr := p.restart(ctx, prev, !before.StoppedByUser); perr != nil {
		return errors.Join(err, fmt.Errorf("putting back the previous release: %w", perr))
	}
	return err
}

// expiredResume settles a resumed statement that expired before its pull
// (re)started: it is not applied. Two crash states are read back from the
// runtime (the held statement and its Previous were on disk before any swap):
//   - its own app already runs (the crash came after its start, before the
//     outcome was saved): reported APPLIED, since that is what runs; it was
//     verified and fresh when it was acted on;
//   - the swap was cut short (the container absent, or a release container
//     stopped without a user's stop): the last applied app is put back in
//     the state it had.
//
// A running app, a stopped one the user stopped, and a directly run
// container are left alone.
func (p *Postbox) expiredResume(ctx context.Context, a *cloudpbv2.DeviceAssignment) (applied bool) {
	st := p.control.AppState(ctx, a.GetAppId())
	if p.runsStatement(st, a) {
		return true
	}
	if st.Present && (st.Running || st.StoppedByUser || !strings.HasPrefix(st.Image, ReleaseImagePrefix)) {
		return false
	}
	if prev := p.previous(a); prev != nil {
		if err := p.restart(ctx, prev, prev.GetDesiredState() == cloudpbv2.Running_RUNNING); err != nil {
			p.logger.Warn("putting back the previous release failed", zap.String("audit", "postbox"),
				zap.String("deployment_id", a.GetDeploymentId()), zap.String("app_id", a.GetAppId()), zap.Error(err))
		}
	}
	return false
}

// runsStatement reports whether the app's container is a's: a's release
// image, running (or left stopped, for install only), and exactly a's
// configuration. Exactly means over every name a configuration could have
// put there, a's and the previous statement's: each of a's values present,
// and no value of the previous configuration that a does not set (a value a
// removed). The rest of the environment (image and agent variables) is not
// the configuration's.
func (p *Postbox) runsStatement(st AppContainerState, a *cloudpbv2.DeviceAssignment) bool {
	env, err := configEnv(a.GetConfigJson())
	if err != nil || !st.Present || st.Image != ReleaseImageName(a.GetAppId(), a.GetAppReleaseId()) ||
		st.Running != (a.GetDesiredState() == cloudpbv2.Running_RUNNING) {
		return false
	}
	for _, kv := range env {
		if !slices.Contains(st.Env, kv) {
			return false
		}
	}
	if prev := p.previous(a); prev != nil {
		old, err := configEnv(prev.GetConfigJson())
		if err != nil {
			return false
		}
		for _, kv := range old {
			if slices.Contains(st.Env, kv) && !slices.Contains(env, kv) {
				return false
			}
		}
	}
	return true
}

// previous is the last applied statement held with a, nil for none.
func (p *Postbox) previous(a *cloudpbv2.DeviceAssignment) *cloudpbv2.DeviceAssignment {
	p.mu.Lock()
	var raw []byte
	if held := p.state.Assignments[a.GetDeploymentId()]; held != nil {
		raw = held.Previous
	}
	p.mu.Unlock()
	prev := &cloudpbv2.DeviceAssignment{}
	if len(raw) == 0 || proto.Unmarshal(raw, prev) != nil {
		return nil
	}
	return prev
}

// restart puts back an earlier, verified statement's app (its definition
// re-read under its own signed digest), running only if it ran. An applied
// removal (ABSENT) is the last applied state: nothing is put back after it.
func (p *Postbox) restart(ctx context.Context, prev *cloudpbv2.DeviceAssignment, run bool) error {
	if prev.GetDesiredState() == cloudpbv2.Running_ABSENT {
		return nil
	}
	s, err := p.releaseStart(ctx, prev)
	if err != nil {
		return err
	}
	s.Run = s.Run && run
	_, err = p.control.StartRelease(ctx, s)
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
	AppState(ctx context.Context, app string) (AppContainerState, error)
}

// StartRelease validates the create while the old app still runs, creates
// the container (replacing the app's previous one), then starts it, or for
// install only records an explicit stop so boot does not start it. replaced
// is true once the create was attempted: from then on the old container may
// be gone.
func (c containerControl) StartRelease(ctx context.Context, s ReleaseStart) (bool, error) {
	release := s.ReleaseID
	req := &agentpb.CreateContainerRequest{
		ImageName: ReleaseImageRef(s.AppID, s.ReleaseID, s.ManifestDigest), AppName: s.AppID, AppConfig: s.AppConfig, Env: s.Env,
		RestartPolicy: &agentpb.RestartPolicy{Mode: agentpb.RestartPolicyMode_UNLESS_STOPPED}, ReleaseId: &release,
	}
	if _, err := c.svc.ValidateContainer(ctx, req); err != nil {
		return false, err
	}
	if _, err := c.svc.CreateContainer(ctx, req); err != nil {
		return true, err
	}
	if !s.Run {
		return true, c.StopApp(ctx, s.AppID)
	}
	return true, c.StartApp(ctx, s.AppID)
}

// AppState asks the containerd client. An unreadable state reads as present,
// running and user-stopped, so nothing is replaced or put back on it.
func (c containerControl) AppState(ctx context.Context, app string) AppContainerState {
	r, ok := c.svc.containerd.(releaseReader)
	if !ok {
		return AppContainerState{}
	}
	st, err := r.AppState(ctx, app)
	if err != nil {
		return AppContainerState{Present: true, Running: true, StoppedByUser: true}
	}
	return st
}
