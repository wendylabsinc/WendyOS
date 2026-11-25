package commands

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"google.golang.org/grpc"

	"github.com/wendylabsinc/wendy/go/internal/cli/cloudrequest"
	"github.com/wendylabsinc/wendy/go/internal/shared/appconfig"
	"github.com/wendylabsinc/wendy/go/internal/shared/certs"
	cloudpb "github.com/wendylabsinc/wendy/go/proto/gen/cloudpb/v2"
)

// The deploy steps, in the dashboard's order and wording (spec 4.2, the
// canvas "Deploy an app"): one flag per question; with every flag given
// nothing is asked; without a terminal a missing answer names its flag.

type deployAnswers struct {
	app, release, digest, name, configFile, keepTrying, note, cloudGRPC string
	devices, tags, sites, config, remove                                []string
	required                                                            []string // settings the release requires; filled from the release
	run, installOnly, move, yes, wait                                   bool
	waitTimeout                                                         time.Duration
}

// deployPoll is how often progress is read; tests shorten it.
var deployPoll = 3 * time.Second

var keepTryingChoices = map[string]time.Duration{
	"7d": 7 * 24 * time.Hour, "30d": 30 * 24 * time.Hour, "60d": 60 * 24 * time.Hour, "1y": 365 * 24 * time.Hour,
}

// keepTryingFor is how long a statement valid for v is signed for, in words:
// 0 is the stated default, and 1y is signed a day short (cloudrequest.MaxSigned).
func keepTryingFor(v time.Duration) string {
	if v == 0 {
		v = cloudrequest.DefaultValidity
	}
	if v = min(v, cloudrequest.MaxSigned); v%(24*time.Hour) == 0 {
		return plural(int(v/(24*time.Hour)), "day")
	}
	return v.String()
}

func newAppDeployCmd() *cobra.Command {
	var a deployAnswers
	cmd := &cobra.Command{
		Use:   "deploy",
		Short: "Deploy a release of an app to devices through Wendy Cloud",
		Long: "Asks which app, which release, which devices, what configuration and whether it should run now,\n" +
			"then shows a review and sends each device its own operator-signed instruction. Each question has\n" +
			"a flag; with all of them given nothing is asked.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := a.checkSteps(); err != nil {
				return err
			}
			auth, err := pickAuthEntry(a.cloudGRPC)
			if err != nil {
				return err
			}
			return withCloud(cmd.Context(), auth, func(ctx context.Context, conn grpc.ClientConnInterface) error {
				op, err := newRegistryOp(ctx, conn, auth)
				if err != nil {
					return err
				}
				return runDeploy(ctx, op, a, cmd.OutOrStdout(), isInteractiveTerminal())
			})
		},
	}
	f := cmd.Flags()
	f.StringVar(&a.app, "app", "", "1  Which app? The app id")
	f.StringVar(&a.release, "release", "", "2  Which release? Its version (or id)")
	f.StringArrayVar(&a.devices, "device", nil, "3  Which devices? A device name or id (repeatable)")
	f.StringArrayVar(&a.tags, "tag", nil, "3  Which devices? Every device with this tag (repeatable)")
	f.StringArrayVar(&a.sites, "site", nil, "3  Which devices? Every device in this site, by name or id (repeatable)")
	a.bindSteps(cmd)
	f.StringVar(&a.digest, "digest", "", "Refuse unless the release's manifest digest is this sha256:<hex>")
	f.StringVar(&a.cloudGRPC, "cloud-grpc", "", "Cloud gRPC endpoint of the session to use")
	return cmd
}

// bindSteps adds the flags for questions 3 to 7 that deploy shares with
// promote.
func (a *deployAnswers) bindSteps(cmd *cobra.Command) {
	f := cmd.Flags()
	f.StringVar(&a.name, "name", "", "3  Name of the deployment")
	f.BoolVar(&a.move, "move", false, "3  Move chosen devices that run the app in another deployment into this one")
	f.StringArrayVar(&a.config, "config", nil, "4  What configuration? NAME=VALUE (repeatable)")
	f.StringVar(&a.configFile, "config-file", "", "4  What configuration? A JSON object of named values")
	f.StringArrayVar(&a.remove, "remove-config", nil, "4  What configuration? Leave a setting out, by name (repeatable)")
	f.BoolVar(&a.run, "run", false, "5  Run it now")
	f.BoolVar(&a.installOnly, "install-only", false, "5  Install only, keep it stopped")
	f.StringVar(&a.keepTrying, "keep-trying", "", "5  How long offline devices keep trying: 7d, 30d, 60d (default) or 1y")
	f.StringVar(&a.note, "note", "", "6  Note for the history")
	f.BoolVarP(&a.yes, "yes", "y", false, "6  Deploy without asking for confirmation")
	f.BoolVar(&a.wait, "wait", false, "7  Stay until every device is done")
	f.DurationVar(&a.waitTimeout, "wait-timeout", 30*time.Minute, "7  With --wait, stop waiting after this long, e.g. 45m or 2h")
}

func (a *deployAnswers) checkSteps() error {
	if a.run && a.installOnly {
		return usageErrorf("Choose one of --run and --install-only.")
	}
	if _, ok := keepTryingChoices[a.keepTrying]; !ok && a.keepTrying != "" {
		return usageErrorf("--keep-trying must be 7d, 30d, 60d or 1y.")
	}
	if a.waitTimeout <= 0 {
		return usageErrorf("--wait-timeout must be longer than 0, e.g. 30m.")
	}
	return nil
}

// missing is the error for an unanswered question without a terminal.
func missing(what, flag, verb string) error {
	return usageErrorf("No %s. Add %s, or run in a terminal to %s.", what, flag, verb)
}

func runDeploy(ctx context.Context, op *registryOp, a deployAnswers, out io.Writer, interactive bool) error {
	say := func(format string, args ...any) { _, _ = fmt.Fprintf(out, format+"\n", args...) }

	// 1  Which app?
	apps, err := op.apps(ctx)
	if err != nil {
		return err
	}
	if a.app == "" {
		if !interactive {
			return missing("app chosen", "--app <id>", "pick one")
		}
		say("1/7  Which app do you want to deploy?")
		for _, app := range apps {
			rels, err := op.releases(ctx, app.GetId())
			if err != nil {
				return err
			}
			if len(rels) == 0 {
				say("     %s  No release yet. Publish one from the project first.", app.GetId())
				continue
			}
			say("     %s  %s", app.GetId(), plural(len(rels), "release"))
		}
		if a.app, err = askText("App", "Type its id. A new app? Create it with wendy app create.", "", nil); err != nil {
			return err
		}
	}
	ai := slices.IndexFunc(apps, func(x *cloudpb.App) bool { return x.GetId() == a.app })
	if ai < 0 {
		return usageErrorf("No app %s in this organization. Create it with wendy app create.", a.app)
	}
	if apps[ai].GetDeletionStartedAt() != nil {
		return usageErrorf("%s is being deleted. Finish it with: wendy app delete %s", a.app, a.app)
	}

	// 2  Which release?
	rels, err := op.releases(ctx, a.app)
	if err != nil {
		return err
	}
	if len(rels) == 0 {
		return usageErrorf("%s has no release yet. Publish one from the project first: wendy app publish --app %s", a.app, a.app)
	}
	states, err := op.appDeviceStates(ctx, a.app, "")
	if err != nil {
		return err
	}
	if a.release == "" {
		if !interactive {
			return missing("release chosen", "--release <version>", "pick one")
		}
		say("2/7  Which release of %s?", a.app)
		for _, r := range rels {
			kind := "Tagged"
			if strings.Contains(r.GetName(), "@sha256:") {
				kind = "Untagged"
			}
			line := fmt.Sprintf("     %s  Published %s · %s", r.GetName(), r.GetCreatedAt().AsTime().Local().Format("2 Jan"), kind)
			if n := countRows(states, func(s *cloudpb.AppDeviceState) bool { return s.GetState().GetReportedReleaseId() == r.GetId() }); n > 0 {
				line += " · running on " + plural(n, "device")
			}
			say("%s", line)
		}
		if a.release, err = askText("Release", "Newest first.", rels[0].GetName(), nil); err != nil {
			return err
		}
	}
	ri := slices.IndexFunc(rels, func(r *cloudpb.AppRelease) bool { return r.GetName() == a.release || r.GetId() == a.release })
	if ri < 0 {
		return usageErrorf("%s has no release %s. Publish it first with wendy app publish.", a.app, a.release)
	}
	rel := rels[ri]

	// 3  Which devices?
	assets, online, err := op.devices(ctx)
	if err != nil {
		return err
	}
	var sites []*cloudpb.Site
	if len(a.devices) == 0 && len(a.tags) == 0 && len(a.sites) == 0 {
		if !interactive {
			return missing("devices chosen", "--device <name>, --tag <tag> or --site <site>", "pick them")
		}
		say("3/7  Which devices should get it?")
		if sites, err = op.sites(ctx); err != nil {
			say("     Sites could not be loaded; choose tags or devices.")
		}
		say("     Choose sites, tags, single devices, or any mix.")
		for _, s := range sites {
			say("     site %s  %s", s.GetName(), plural(len(inSite(assets, s.GetId())), "device"))
		}
		for _, tag := range allTags(assets) {
			say("     tag %s  %s", tag, plural(len(withTag(assets, tag)), "device"))
		}
		for _, d := range assets {
			say("     %s (%s)  %s", assetName(d), d.GetId(), deviceNote(d, online, states, a.app))
		}
		answer, err := askText("Devices", "Device names, site:<site> and tag:<tag>, separated by commas.", "", nil)
		if err != nil {
			return err
		}
		for _, item := range strings.Split(answer, ",") {
			if item = strings.TrimSpace(item); strings.HasPrefix(item, "tag:") {
				a.tags = append(a.tags, strings.TrimPrefix(item, "tag:"))
			} else if strings.HasPrefix(item, "site:") {
				a.sites = append(a.sites, strings.TrimPrefix(item, "site:"))
			} else if item != "" {
				a.devices = append(a.devices, item)
			}
		}
	}
	plan := deployPlan{appID: a.app, releaseID: rel.GetId(), digest: a.digest, tags: a.tags}
	for _, name := range a.devices {
		id, err := assetID(assets, name)
		if err != nil {
			return err
		}
		plan.deviceIDs = append(plan.deviceIDs, id)
	}
	for _, tag := range a.tags {
		if len(withTag(assets, tag)) == 0 {
			return usageErrorf("No device has the tag %s.", tag)
		}
	}
	if len(a.sites) > 0 && sites == nil {
		if sites, err = op.sites(ctx); err != nil {
			return err
		}
	}
	for _, name := range a.sites {
		s, err := findSite(sites, name)
		if err != nil {
			return err
		}
		if len(inSite(assets, s.GetId())) == 0 {
			return usageErrorf("No device is in the site %s. Add devices to it under Sites in the dashboard first.", s.GetName())
		}
		plan.siteIDs = append(plan.siteIDs, s.GetId())
	}
	plan.devices = resolveSelection(assets, &cloudpb.DeviceSelection{TargetDeviceIds: plan.deviceIDs, TargetTags: plan.tags, TargetSiteIds: plan.siteIDs})
	if plan.moves, err = op.confirmMoves(ctx, a.app, "", plan.devices, assets, a.move, interactive); err != nil {
		return err
	}
	if a.name == "" {
		if !interactive {
			return missing("deployment name given", "--name <name>", "enter one")
		}
		if a.name, err = askText("Name this deployment", "Shown in the deployments list. Use the place or the purpose.", "", nil); err != nil {
			return err
		}
	}
	plan.name = a.name

	// 4  What configuration?
	a.required = releaseRequired(rel)
	config, changed, err := deployConfig(rel.GetDefaultConfigJson(), a, interactive, say)
	if err != nil {
		return err
	}
	if err := checkDeployConfig(config, a.required, func(name string) string {
		b := a
		b.remove = append(slices.Clone(a.remove), name)
		return "Leave it out with: " + deployCommand(b)
	}); err != nil {
		return err
	}
	plan.config = config

	// 5  Should it run now?
	if !a.run && !a.installOnly {
		if !interactive {
			return missing("run state chosen", "--run or --install-only", "choose")
		}
		say("5/7  Should it run now?")
		answer, err := askText("Run it now, or install only?", "run: each device starts the app as soon as it is installed. install: it stays stopped until someone starts it.", "run", nil)
		if err != nil {
			return err
		}
		a.installOnly = strings.HasPrefix(strings.ToLower(strings.TrimSpace(answer)), "install")
	}
	for a.keepTrying == "" || keepTryingChoices[a.keepTrying] == 0 {
		if !interactive {
			a.keepTrying = "60d" // the preselected answer (ruling 4)
			break
		}
		answer, err := askText("If a device is offline, keep trying for", "7d, 30d, 60d or 1y. After that, the device no longer installs this deployment.", "60d", nil)
		if err != nil {
			return err
		}
		a.keepTrying = strings.TrimSpace(answer)
	}
	plan.desired, plan.validity = cloudpb.Running_RUNNING, keepTryingChoices[a.keepTrying]
	runWords := "running"
	if a.installOnly {
		plan.desired, runWords = cloudpb.Running_STOPPED, "installed only"
	}

	// 6  Review
	offline := 0
	for _, id := range plan.devices {
		if !online[id] {
			offline++
		}
	}
	say("6/7  Review")
	say("     %s: %s %s to %s, %s, %s.", plan.name, a.app, rel.GetName(), plural(len(plan.devices), "device"), runWords, plural(changed, "value")+" changed")
	say("     Devices: %s", deviceNames(assets, plan.devices))
	if offline > 0 {
		say("     %s offline. Offline devices keep trying for %s.", plural(offline, "device is", "devices are"), keepTryingFor(plan.validity))
	} else {
		say("     Offline devices keep trying for %s.", keepTryingFor(plan.validity))
	}
	if len(plan.moves) > 0 {
		say("     %s from another deployment of %s.", plural(len(plan.moves), "device moves", "devices move"), a.app)
	}
	if a.note == "" && interactive && !a.yes {
		if a.note, err = askText("Note for the history", "Optional. Shown in this deployment's history and the audit log.", "", nil); err != nil {
			return err
		}
	}
	plan.note = a.note
	if err := confirmOrYes(a.yes, interactive, fmt.Sprintf("Deploy to %s?", plural(len(plan.devices), "device")),
		usageErrorf("Not confirmed. Add --yes to deploy without a terminal."), errors.New("nothing was deployed")); err != nil {
		return err
	}
	d, err := op.deploy(ctx, plan)
	if d == nil {
		return err
	}
	if err != nil {
		say("Some devices did not get their instruction: %v", err)
		say("Send them later with: wendy app deployment resume %s/%s", a.app, plan.name)
	}

	// 7  Progress
	say("7/7  Progress")
	timeout := time.Duration(0) // an offline device is a normal state: only --wait gives up
	if a.wait {
		timeout = a.waitTimeout
	}
	failed, err := op.progress(ctx, d, rel.GetName(), online, interactive || a.wait, timeout, say)
	if err != nil {
		return &deployedError{a.app, plan.name, err}
	}
	if failed > 0 {
		return &deployedError{a.app, plan.name, fmt.Errorf("%s failed. The reason is next to each device above; fix it, then update the deployment", plural(failed, "device"))}
	}
	say("Follow it later with: wendy app deployment show %s/%s", a.app, plan.name)
	return err
}

// deployedError is a deploy error after the deployment app/name was
// created: a caller must not suggest deploying again.
type deployedError struct {
	app, name string
	err       error
}

func (e *deployedError) Error() string { return e.err.Error() }
func (e *deployedError) Unwrap() error { return e.err }

// deployCommand is the wendy app deploy command line for a's answers, so it
// works without a terminal when they were all given as flags.
func deployCommand(a deployAnswers) string {
	args := []string{"wendy", "app", "deploy", "--app", a.app, "--release", a.release}
	add := func(flag string, values ...string) {
		for _, v := range values {
			if v != "" {
				args = append(args, flag, shellQuoteArg(v))
			}
		}
	}
	add("--device", a.devices...)
	add("--tag", a.tags...)
	add("--site", a.sites...)
	add("--name", a.name)
	add("--config", a.config...)
	add("--config-file", a.configFile)
	add("--remove-config", a.remove...)
	add("--keep-trying", a.keepTrying)
	add("--note", a.note)
	add("--digest", a.digest)
	for _, f := range []struct {
		flag string
		on   bool
	}{{"--move", a.move}, {"--run", a.run}, {"--install-only", a.installOnly}, {"--yes", a.yes}, {"--wait", a.wait}} {
		if f.on {
			args = append(args, f.flag)
		}
	}
	if a.wait {
		add("--wait-timeout", a.waitTimeout.String())
	}
	return strings.Join(args, " ")
}

// deployConfig resolves question 4: the release's defaults, overridden by
// --config-file, then --config, then answers. It returns the compact JSON and
// how many values differ from the defaults.
func deployConfig(defaults string, a deployAnswers, interactive bool, say func(string, ...any)) (string, int, error) {
	base := map[string]json.RawMessage{}
	if defaults != "" {
		if err := json.Unmarshal([]byte(defaults), &base); err != nil {
			return "", 0, fmt.Errorf("the release's default configuration is not a JSON object: %w", err)
		}
	}
	values := maps.Clone(base)
	if a.configFile != "" {
		raw, err := os.ReadFile(a.configFile)
		if err != nil {
			return "", 0, err
		}
		var file map[string]json.RawMessage
		if err := json.Unmarshal(raw, &file); err != nil {
			return "", 0, usageErrorf("--config-file must hold one JSON object of named values: %v", err)
		}
		maps.Copy(values, file)
	}
	for _, kv := range a.config {
		name, value, ok := strings.Cut(kv, "=")
		if !ok || name == "" {
			return "", 0, usageErrorf("--config must be NAME=VALUE, got %q.", kv)
		}
		values[name], _ = json.Marshal(value)
	}
	for _, name := range a.remove {
		if _, ok := values[name]; !ok {
			return "", 0, usageErrorf("There is no setting %s to remove. The settings are: %s.", name, strings.Join(slices.Sorted(maps.Keys(values)), ", "))
		}
		delete(values, name)
	}
	if interactive && len(a.config) == 0 && a.configFile == "" && len(base) > 0 {
		say("4/7  What configuration should it use?")
		say("     The release comes with defaults. Change only what this deployment needs.")
		for _, name := range slices.Sorted(maps.Keys(base)) {
			def := string(base[name])
			if s, err := strconv.Unquote(def); err == nil {
				def = s
			}
			hint := ""
			if slices.Contains(a.required, name) {
				hint = "Required."
			}
			answer, err := askText(name, hint, def, nil)
			for err == nil && hint != "" && strings.TrimSpace(answer) == "" {
				answer, err = askText(name, name+" is required. Enter a value to continue.", def, nil)
			}
			if err != nil {
				return "", 0, err
			}
			if answer != def {
				values[name], _ = json.Marshal(answer)
			}
		}
	}
	changed := len(a.remove)
	for name, v := range values {
		if string(base[name]) != string(v) {
			changed++
		}
	}
	text, err := json.Marshal(values)
	return string(text), changed, err
}

// checkDeployConfig refuses a configuration that leaves a required setting
// empty or that a device would refuse, naming the setting, the rule and
// fix(setting): a command that leaves it out.
func checkDeployConfig(config string, required []string, fix func(string) string) error {
	if err := checkRequired(config, required); err != nil {
		return err
	}
	name, err := refusedSetting(config)
	switch {
	case err == nil:
		return nil
	case name == "":
		return usageErrorf("Devices refuse this configuration: %v. Leave settings out with --remove-config NAME.", err)
	}
	return usageErrorf("Devices refuse the setting %s: %v. %s", name, err, fix(name))
}

// removeConfigFix is the advice for a refused setting name when the
// command that refused it takes --remove-config.
func removeConfigFix(name string) string {
	return fmt.Sprintf("Add --remove-config %s to the same command to leave it out, or change it with --config %s=VALUE.", name, name)
}

// refusedSetting runs the device's check of config (appconfig.
// CheckDeploymentConfig). When one setting alone breaks the rule, it returns
// that setting's name with its refusal; a rule about the whole configuration
// (its size, its number of settings) has no name.
func refusedSetting(config string) (string, error) {
	err := appconfig.CheckDeploymentConfig(config)
	if err == nil {
		return "", nil
	}
	var values map[string]json.RawMessage
	if json.Unmarshal([]byte(config), &values) == nil {
		for _, name := range slices.Sorted(maps.Keys(values)) {
			one, _ := json.Marshal(map[string]json.RawMessage{name: values[name]})
			if e := appconfig.CheckDeploymentConfig(string(one)); e != nil {
				return name, e
			}
		}
	}
	return "", err
}

// releaseRequired is the settings rel requires (wendy.json requiredEnv,
// sent at publish).
var releaseRequired = func(rel *cloudpb.AppRelease) []string { return rel.GetRequiredConfigNames() }

// checkRequired refuses a configuration that leaves a required setting
// empty (sem 2026-11-13 11:01): naming it, with the one next step.
func checkRequired(config string, required []string) error {
	name, err := emptyRequired(config, required)
	if err != nil || name == "" {
		return err
	}
	return usageErrorf("%s has no value. Add --config %s=<value>.", name, name)
}

// emptyRequired is the first required setting config leaves empty, or "".
func emptyRequired(config string, required []string) (string, error) {
	var values map[string]json.RawMessage
	if err := json.Unmarshal([]byte(config), &values); err != nil {
		return "", err
	}
	for _, name := range required {
		var v any
		if raw, ok := values[name]; !ok || json.Unmarshal(raw, &v) != nil || v == nil || v == "" {
			return name, nil
		}
	}
	return "", nil
}

// progress shows each device's state in the dashboard's words; with follow
// it stays until every device is done, printing changes, or until timeout
// (above 0) has passed, which is an error naming the devices not done.
// Ctrl-C leaves it: the deployment goes on. It returns how many devices
// failed; whether that is a command failure is the caller's to say.
func (op *registryOp) progress(ctx context.Context, d *cloudpb.Deployment, release string, online map[string]bool, follow bool, timeout time.Duration, say func(string, ...any)) (int, error) {
	shown := map[string]string{}
	deadline := time.Now().Add(timeout)
	left := func(err error) (int, error) {
		if !interruptedByUser(ctx) {
			return 0, err
		}
		say("Stopped following. The deployment continues; follow it with: wendy app deployment show %s/%s", d.GetAppId(), d.GetName())
		return 0, nil
	}
	for {
		rows, err := op.appDeviceStates(ctx, d.GetAppId(), d.GetId())
		if err != nil {
			return left(err)
		}
		counts := map[string]int{}
		var notDone []string
		for _, r := range rows {
			state, detail := deviceState(r.GetState(), online[r.GetAssetId()], op.sent[certs.DeviceSPIFFEURI(op.org, r.GetAssetId())])
			counts[state]++
			if state != "running" && state != "installed" && state != "failed" {
				notDone = append(notDone, r.GetAssetName())
			}
			if line := fmt.Sprintf("     %-14s %-12s %s", r.GetAssetName(), state, detail); shown[r.GetAssetId()] != line {
				shown[r.GetAssetId()] = line
				say("%s", strings.TrimRight(line, " "))
			}
		}
		done := len(notDone) == 0
		late := follow && timeout > 0 && !done && !time.Now().Before(deadline)
		if !follow || done || late {
			var parts []string
			for _, s := range []string{"running", "installed", "downloading", "waiting", "failed"} {
				if counts[s] > 0 {
					parts = append(parts, fmt.Sprintf("%d %s", counts[s], s))
				}
			}
			say("Deployed %s: %s %s to %s. %s.", d.GetName(), d.GetAppId(), release, plural(len(rows), "device"), strings.Join(parts, ", "))
			if late {
				return counts["failed"], fmt.Errorf("stopped waiting after %s; still waiting: %s. The deployment continues: check it with wendy app deployment show %s/%s, or wait longer with --wait-timeout",
					timeout, strings.Join(notDone, ", "), d.GetAppId(), d.GetName())
			}
			return counts["failed"], nil
		}
		wait := deployPoll
		if timeout > 0 {
			wait = min(wait, max(time.Until(deadline), 0))
		}
		select {
		case <-ctx.Done():
			return left(ctx.Err())
		case <-time.After(wait):
		}
	}
}

// deviceState maps a device's report to the dashboard's per-device states
// (running, installed, downloading, waiting, failed) and the reason with the
// next step. Ruling 11 (sem via the orchestrator, 2026-11-13). sent is the
// postbox entry this run sent the device (0: none): a report of an earlier
// entry, or "applied" for another release, is not about the statement sent.
func deviceState(s *cloudpb.AssetApp, isOnline bool, sent int64) (string, string) {
	if s.GetStatementPending() || s.GetPostboxEntryId() < sent {
		return waitingState(isOnline)
	}
	switch s.GetOutcome() {
	case cloudpb.ApplyOutcome_APPLY_OUTCOME_APPLIED:
		if s.GetReportedReleaseId() != s.GetAppReleaseId() {
			if !isOnline {
				return waitingState(false)
			}
			return "waiting", "Still runs an earlier release. It installs this one when it picks up its instruction."
		}
		if s.GetDesiredState() == cloudpb.Running_STOPPED {
			return "installed", ""
		}
		return "running", ""
	case cloudpb.ApplyOutcome_APPLY_OUTCOME_PULLING:
		return "downloading", ""
	case cloudpb.ApplyOutcome_APPLY_OUTCOME_NOT_STARTED:
		return "downloading", "Installed. Starting it."
	case cloudpb.ApplyOutcome_APPLY_OUTCOME_PULL_RETRYING:
		return "waiting", "Can't reach the registry yet. It keeps trying."
	case cloudpb.ApplyOutcome_APPLY_OUTCOME_APPLY_FAILED:
		return "failed", "The app did not start on the device. Check the app's logs on the device, then deploy again."
	case cloudpb.ApplyOutcome_APPLY_OUTCOME_PULL_REFUSED:
		return "failed", "The device could not download the release in time. Deploy again."
	case cloudpb.ApplyOutcome_APPLY_OUTCOME_REJECTED_SIGNATURE, cloudpb.ApplyOutcome_APPLY_OUTCOME_REJECTED_TARGET,
		cloudpb.ApplyOutcome_APPLY_OUTCOME_REJECTED_STALE, cloudpb.ApplyOutcome_APPLY_OUTCOME_REJECTED_DIGEST,
		cloudpb.ApplyOutcome_APPLY_OUTCOME_REJECTED_UNWRAP:
		return "failed", "The device refused this deployment. Check the device's log, then deploy again."
	}
	return waitingState(isOnline)
}

func waitingState(isOnline bool) (string, string) {
	if !isOnline {
		return "waiting", "Offline. It installs the app when it comes back online."
	}
	return "waiting", "Waiting for its instruction."
}

func countRows(rows []*cloudpb.AppDeviceState, match func(*cloudpb.AppDeviceState) bool) int {
	n := 0
	for _, r := range rows {
		if match(r) {
			n++
		}
	}
	return n
}

// plural is "1 device" or "2 devices"; forms gives the singular and plural
// phrases when the noun is not a plain "+s".
func plural(n int, forms ...string) string {
	one, many := forms[0], forms[0]+"s"
	if len(forms) > 1 {
		many = forms[1]
	}
	if n == 1 {
		return "1 " + one
	}
	return strconv.Itoa(n) + " " + many
}

func assetName(d *cloudpb.Asset) string {
	if d.GetDisplayName() != "" {
		return d.GetDisplayName()
	}
	if d.GetName() != "" {
		return d.GetName()
	}
	return d.GetId()
}

func allTags(assets []*cloudpb.Asset) []string {
	set := map[string]bool{}
	for _, d := range assets {
		for _, t := range d.GetTags() {
			set[t] = true
		}
	}
	return slices.Sorted(maps.Keys(set))
}

func withTag(assets []*cloudpb.Asset, tag string) []*cloudpb.Asset {
	var out []*cloudpb.Asset
	for _, d := range assets {
		if slices.Contains(d.GetTags(), tag) {
			out = append(out, d)
		}
	}
	return out
}

func inSite(assets []*cloudpb.Asset, siteID string) []*cloudpb.Asset {
	var out []*cloudpb.Asset
	for _, d := range assets {
		if d.GetSiteId() == siteID {
			out = append(out, d)
		}
	}
	return out
}

// sites lists the organization's sites (SiteService.ListSites).
func (op *registryOp) sites(ctx context.Context) ([]*cloudpb.Site, error) {
	resp, err := cloudpb.NewSiteServiceClient(op.conn).ListSites(ctx, &cloudpb.ListSitesRequest{OrganizationId: op.org})
	if err != nil {
		return nil, fmt.Errorf("reading the sites: %w", err)
	}
	return resp.GetSites(), nil
}

// findSite is the site with this name or id.
func findSite(sites []*cloudpb.Site, nameOrID string) (*cloudpb.Site, error) {
	i := slices.IndexFunc(sites, func(s *cloudpb.Site) bool { return s.GetId() == nameOrID || s.GetName() == nameOrID })
	if i < 0 || nameOrID == "" {
		return nil, usageErrorf("No site %q in this organization. Use a site's name or id as shown under Sites in the dashboard.", nameOrID)
	}
	return sites[i], nil
}

// otherDeployment is the device's row for appID when it holds a deployment
// of it (a directly run app has no deployment).
func otherDeployment(states []*cloudpb.AppDeviceState, device, appID string) *cloudpb.AssetApp {
	for _, r := range states {
		if r.GetAssetId() == device && r.GetState().GetAppId() == appID && r.GetState().GetDeploymentId() != "" && !r.GetState().GetUnassigned() {
			return r.GetState()
		}
	}
	return nil
}

func deviceNote(d *cloudpb.Asset, online map[string]bool, states []*cloudpb.AppDeviceState, appID string) string {
	if otherDeployment(states, d.GetId(), appID) != nil {
		return "In another deployment of " + appID + ". Choosing it moves it to this one."
	}
	if !online[d.GetId()] {
		return "Offline. It installs the app when it comes back online."
	}
	return "Online"
}

func (op *registryOp) apps(ctx context.Context) ([]*cloudpb.App, error) {
	var out []*cloudpb.App
	for {
		offset, limit := int32(len(out)), int32(200)
		resp, err := cloudpb.NewAppServiceClient(op.conn).ListApps(ctx, &cloudpb.ListAppsRequest{OrganizationId: op.org, Offset: &offset, Limit: &limit})
		if err != nil {
			return nil, fmt.Errorf("reading the apps: %w", err)
		}
		out = append(out, resp.GetApps()...)
		if len(resp.GetApps()) == 0 || int32(len(out)) >= resp.GetTotal() {
			return out, nil
		}
	}
}

// devices lists the organization's compute devices and which are online.
func (op *registryOp) devices(ctx context.Context) ([]*cloudpb.Asset, map[string]bool, error) {
	list := func(onlineOnly bool) ([]*cloudpb.Asset, error) {
		out, err := streamAll(func(offset int32) (grpc.ServerStreamingClient[cloudpb.ListAssetsResponse], error) {
			limit, compute := int32(500), true
			return cloudpb.NewAssetServiceClient(op.conn).ListAssets(ctx, &cloudpb.ListAssetsRequest{OrganizationId: op.org, IsComputeDevice: &compute, Offset: &offset, Limit: &limit, OnlineOnly: &onlineOnly})
		}, func(r *cloudpb.ListAssetsResponse) ([]*cloudpb.Asset, int32) {
			return []*cloudpb.Asset{r.GetAsset()}, r.GetTotal()
		})
		if err != nil {
			return nil, fmt.Errorf("reading the devices: %w", err)
		}
		return out, nil
	}
	all, err := list(false)
	if err != nil {
		return nil, nil, err
	}
	up, err := list(true)
	if err != nil {
		return nil, nil, err
	}
	online := map[string]bool{}
	for _, d := range up {
		online[d.GetId()] = true
	}
	return all, online, nil
}

func (op *registryOp) appDeviceStates(ctx context.Context, appID, deploymentID string) ([]*cloudpb.AppDeviceState, error) {
	out, err := pages(func(token string) ([]*cloudpb.AppDeviceState, string, error) {
		resp, err := cloudpb.NewDeploymentServiceClient(op.conn).ListAppDeviceStates(ctx, &cloudpb.ListAppDeviceStatesRequest{OrganizationId: op.org, AppId: appID, DeploymentId: deploymentID, PageToken: token})
		return resp.GetRows(), resp.GetNextPageToken(), err
	})
	if err != nil {
		return nil, fmt.Errorf("reading the devices of %s: %w", appID, err)
	}
	return out, nil
}

func (op *registryOp) deploymentName(ctx context.Context, id string) string {
	d, err := cloudpb.NewDeploymentServiceClient(op.conn).GetDeployment(ctx, &cloudpb.GetDeploymentRequest{Id: id})
	if err != nil || d.GetName() == "" {
		return id
	}
	return d.GetName()
}

// newAppDeploymentCmd is `wendy app deployment`: what a deployment asks for
// and where each device stands.
func newAppDeploymentCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "deployment", Short: "Show and change a deployment of an app"}
	cmd.AddCommand(deploymentSub("show", "Show a deployment and each device's state", nil,
		func(ctx context.Context, op *registryOp, appID, name string, _ changeFlags, out io.Writer, _ bool) error {
			return op.showDeployment(ctx, appID, name, out)
		}))
	cmd.AddCommand(deploymentChangeCmds()...)
	return cmd
}

// findDeployment is appID's live deployment called name (or a removed one,
// with includeRemoved).
func (op *registryOp) findDeployment(ctx context.Context, appID, name string, includeRemoved bool) (*cloudpb.Deployment, error) {
	var found *cloudpb.Deployment
	err := op.eachDeployment(ctx, appID, includeRemoved, func(d *cloudpb.Deployment) bool {
		if d.GetName() == name {
			found = d
		}
		return found != nil
	})
	if err == nil && found == nil {
		err = usageErrorf("%s has no deployment %s.", appID, name)
	}
	return found, err
}

// eachDeployment calls fn with appID's deployments in Cloud's order (newest
// first) until fn returns true.
func (op *registryOp) eachDeployment(ctx context.Context, appID string, includeRemoved bool, fn func(*cloudpb.Deployment) bool) error {
	for offset := int32(0); ; {
		stream, err := cloudpb.NewDeploymentServiceClient(op.conn).ListDeployments(ctx, &cloudpb.ListDeploymentsRequest{OrganizationId: op.org, AppId: appID, Offset: offset, IncludeRemoved: includeRemoved})
		if err != nil {
			return fmt.Errorf("reading the deployments of %s: %w", appID, err)
		}
		var seen, total int32
		for {
			page, err := stream.Recv()
			if err == io.EOF {
				break
			}
			if err != nil {
				return fmt.Errorf("reading the deployments of %s: %w", appID, err)
			}
			total = page.GetTotal()
			for _, d := range page.GetDeployments() {
				seen++
				if d.GetAppId() == appID && (includeRemoved || d.GetRemovedAt() == nil) && fn(d) {
					return nil
				}
			}
		}
		if offset += seen; seen == 0 || offset >= total {
			return nil
		}
	}
}

func (op *registryOp) showDeployment(ctx context.Context, appID, name string, out io.Writer) error {
	say := func(format string, args ...any) { _, _ = fmt.Fprintf(out, format+"\n", args...) }
	d, err := op.findDeployment(ctx, appID, name, false)
	if err != nil {
		return err
	}
	release := d.GetAppReleaseId()
	if rels, err := op.releases(ctx, appID); err == nil {
		if i := slices.IndexFunc(rels, func(r *cloudpb.AppRelease) bool { return r.GetId() == release }); i >= 0 {
			release = rels[i].GetName()
		}
	}
	run := "run it now"
	if d.GetDesiredState() == cloudpb.Running_STOPPED {
		run = "install only"
	}
	say("%s/%s: release %s, %s, revision %d.", appID, name, release, run, d.GetRevision())
	say("Configuration: %s", d.GetConfigJson())
	_, online, err := op.devices(ctx)
	if err != nil {
		return err
	}
	// A failed device is information here, not a command failure.
	_, err = op.progress(ctx, d, release, online, false, 0, say)
	return err
}
