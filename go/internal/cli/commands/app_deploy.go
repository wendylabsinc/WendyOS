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

	"github.com/wendylabsinc/wendy/go/internal/cli/tui"
	cloudpb "github.com/wendylabsinc/wendy/go/proto/gen/cloudpb/v2"
)

// The deploy steps, in the dashboard's order and wording (spec 4.2, the
// canvas "Deploy an app"): one flag per question; with every flag given
// nothing is asked; without a terminal a missing answer names its flag.

type deployAnswers struct {
	app, release, digest, name, configFile, keepTrying, note, cloudGRPC string
	devices, tags, config                                               []string
	run, installOnly, move, yes, wait                                   bool
}

// Prompts, replaceable in tests.
var (
	deployAsk = func(question, hint, def string) (string, error) {
		return tui.PromptTextWithDefault(question, hint, def, nil)
	}
	deployConfirm = func(question string) (bool, error) { return tui.ConfirmDefaultNo(question) }
	deployPoll    = 3 * time.Second
)

var keepTryingChoices = map[string]time.Duration{
	"7d": 7 * 24 * time.Hour, "30d": 30 * 24 * time.Hour, "60d": 60 * 24 * time.Hour, "1y": 365 * 24 * time.Hour,
}

var keepTryingWords = map[string]string{"7d": "7 days", "30d": "30 days", "60d": "60 days", "1y": "1 year"}

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
			if a.run && a.installOnly {
				return usageErrorf("Choose one of --run and --install-only.")
			}
			if _, ok := keepTryingChoices[a.keepTrying]; !ok {
				return usageErrorf("--keep-trying must be 7d, 30d, 60d or 1y.")
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
	f.StringVar(&a.name, "name", "", "3  Name of the deployment")
	f.BoolVar(&a.move, "move", false, "3  Move chosen devices that run the app in another deployment into this one")
	f.StringArrayVar(&a.config, "config", nil, "4  What configuration? NAME=VALUE (repeatable)")
	f.StringVar(&a.configFile, "config-file", "", "4  What configuration? A JSON object of named values")
	f.BoolVar(&a.run, "run", false, "5  Run it now")
	f.BoolVar(&a.installOnly, "install-only", false, "5  Install only, keep it stopped")
	f.StringVar(&a.keepTrying, "keep-trying", "60d", "5  How long offline devices keep trying: 7d, 30d, 60d or 1y")
	f.StringVar(&a.note, "note", "", "6  Note for the history")
	f.BoolVarP(&a.yes, "yes", "y", false, "6  Deploy without asking for confirmation")
	f.BoolVar(&a.wait, "wait", false, "7  Stay until every device is done")
	f.StringVar(&a.digest, "digest", "", "Refuse unless the release's manifest digest is this sha256:<hex>")
	f.StringVar(&a.cloudGRPC, "cloud-grpc", "", "Cloud gRPC endpoint of the session to use")
	return cmd
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
		if a.app, err = deployAsk("App", "Type its id. A new app? Create it with wendy app create.", ""); err != nil {
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
		if a.release, err = deployAsk("Release", "Newest first.", rels[0].GetName()); err != nil {
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
	if len(a.devices) == 0 && len(a.tags) == 0 {
		if !interactive {
			return missing("devices chosen", "--device <name> or --tag <tag>", "pick them")
		}
		say("3/7  Which devices should get it?")
		say("     Choose tags, single devices, or both.")
		for _, tag := range allTags(assets) {
			say("     tag %s  %s", tag, plural(len(withTag(assets, tag)), "device"))
		}
		for _, d := range assets {
			say("     %s  %s", assetName(d), deviceNote(d, online, states, a.app))
		}
		answer, err := deployAsk("Devices", "Device names and tag:<tag>, separated by commas.", "")
		if err != nil {
			return err
		}
		for _, item := range strings.Split(answer, ",") {
			if item = strings.TrimSpace(item); strings.HasPrefix(item, "tag:") {
				a.tags = append(a.tags, strings.TrimPrefix(item, "tag:"))
			} else if item != "" {
				a.devices = append(a.devices, item)
			}
		}
	}
	plan := deployPlan{appID: a.app, releaseID: rel.GetId(), digest: a.digest, moves: map[string]string{}}
	chosen := map[string]*cloudpb.Asset{}
	for _, name := range a.devices {
		i := slices.IndexFunc(assets, func(d *cloudpb.Asset) bool {
			return d.GetId() == name || d.GetName() == name || d.GetDisplayName() == name
		})
		if i < 0 {
			return usageErrorf("No device %s in this organization.", name)
		}
		chosen[assets[i].GetId()] = assets[i]
		plan.deviceIDs = append(plan.deviceIDs, assets[i].GetId())
	}
	for _, tag := range a.tags {
		tagged := withTag(assets, tag)
		if len(tagged) == 0 {
			return usageErrorf("No device has the tag %s.", tag)
		}
		for _, d := range tagged {
			chosen[d.GetId()] = d
		}
		plan.tags = append(plan.tags, tag)
	}
	for _, id := range slices.Sorted(maps.Keys(chosen)) {
		plan.devices = append(plan.devices, id)
		if other := otherDeployment(states, id, a.app); other != nil {
			name := op.deploymentName(ctx, other.GetDeploymentId())
			if !a.move {
				if !interactive {
					return usageErrorf("%s already runs %s in the deployment %q. Add --move to move it, or leave it out.", assetName(chosen[id]), a.app, name)
				}
				ok, err := deployConfirm(fmt.Sprintf("%s already runs %s in the deployment %q. Move it to this one?", assetName(chosen[id]), a.app, name))
				if err != nil {
					return err
				}
				if !ok {
					return usageErrorf("Leave %s out, or move it, then deploy again.", assetName(chosen[id]))
				}
			}
			plan.moves[id] = other.GetDeploymentId()
		}
	}
	if a.name == "" {
		if !interactive {
			return missing("deployment name given", "--name <name>", "enter one")
		}
		if a.name, err = deployAsk("Name this deployment", "Shown in the deployments list. Use the place or the purpose.", ""); err != nil {
			return err
		}
	}
	plan.name = a.name

	// 4  What configuration?
	config, changed, err := deployConfig(rel.GetDefaultConfigJson(), a, interactive, say)
	if err != nil {
		return err
	}
	plan.config = config

	// 5  Should it run now?
	if !a.run && !a.installOnly {
		if !interactive {
			return missing("run state chosen", "--run or --install-only", "choose")
		}
		say("5/7  Should it run now?")
		answer, err := deployAsk("Run it now, or install only?", "run: each device starts the app as soon as it is installed. install: it stays stopped until someone starts it.", "run")
		if err != nil {
			return err
		}
		a.installOnly = strings.HasPrefix(strings.ToLower(strings.TrimSpace(answer)), "install")
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
	names := make([]string, 0, len(plan.devices))
	for _, id := range plan.devices {
		names = append(names, assetName(chosen[id]))
	}
	say("     Devices: %s", strings.Join(names, ", "))
	if offline > 0 {
		say("     %s offline. Offline devices keep trying for %s.", plural(offline, "device is", "devices are"), keepTryingWords[a.keepTrying])
	} else {
		say("     Offline devices keep trying for %s.", keepTryingWords[a.keepTrying])
	}
	if len(plan.moves) > 0 {
		say("     %s from another deployment of %s.", plural(len(plan.moves), "device moves", "devices move"), a.app)
	}
	if a.note == "" && interactive && !a.yes {
		if a.note, err = deployAsk("Note for the history", "Optional. Shown in this deployment's history and the audit log.", ""); err != nil {
			return err
		}
	}
	plan.note = a.note
	if !a.yes {
		if !interactive {
			return usageErrorf("Not confirmed. Add --yes to deploy without a terminal.")
		}
		ok, err := deployConfirm(fmt.Sprintf("Deploy to %s?", plural(len(plan.devices), "device")))
		if err != nil {
			return err
		}
		if !ok {
			return errors.New("nothing was deployed")
		}
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
	if err := op.progress(ctx, d, rel.GetName(), online, interactive || a.wait, say); err != nil {
		return err
	}
	say("Follow it later with: wendy app deployment show %s/%s", a.app, plan.name)
	return err
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
	if interactive && len(a.config) == 0 && a.configFile == "" && len(base) > 0 {
		say("4/7  What configuration should it use?")
		say("     The release comes with defaults. Change only what this deployment needs.")
		for _, name := range slices.Sorted(maps.Keys(base)) {
			def := string(base[name])
			if s, err := strconv.Unquote(def); err == nil {
				def = s
			}
			answer, err := deployAsk(name, "", def)
			if err != nil {
				return "", 0, err
			}
			if answer != def {
				values[name], _ = json.Marshal(answer)
			}
		}
	}
	changed := 0
	for name, v := range values {
		if string(base[name]) != string(v) {
			changed++
		}
	}
	text, err := json.Marshal(values)
	return string(text), changed, err
}

// progress shows each device's state in the dashboard's words; with follow
// it stays until every device is done, printing changes.
func (op *registryOp) progress(ctx context.Context, d *cloudpb.Deployment, release string, online map[string]bool, follow bool, say func(string, ...any)) error {
	shown := map[string]string{}
	for {
		rows, err := op.appDeviceStates(ctx, d.GetAppId(), d.GetId())
		if err != nil {
			return err
		}
		counts, done := map[string]int{}, true
		for _, r := range rows {
			state, detail := deviceState(r.GetState(), online[r.GetAssetId()])
			counts[state]++
			done = done && (state == "running" || state == "installed" || state == "failed")
			if line := fmt.Sprintf("     %-14s %-12s %s", r.GetAssetName(), state, detail); shown[r.GetAssetId()] != line {
				shown[r.GetAssetId()] = line
				say("%s", strings.TrimRight(line, " "))
			}
		}
		if !follow || done {
			var parts []string
			for _, s := range []string{"running", "installed", "downloading", "waiting", "failed"} {
				if counts[s] > 0 {
					parts = append(parts, fmt.Sprintf("%d %s", counts[s], s))
				}
			}
			say("Deployed %s: %s %s to %s. %s.", d.GetName(), d.GetAppId(), release, plural(len(rows), "device"), strings.Join(parts, ", "))
			return nil
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(deployPoll):
		}
	}
}

// deviceState maps a device's report to the dashboard's per-device states
// (running, installed, downloading, waiting, failed) and the reason with the
// next step. Ruling 11 (sem via the orchestrator, 2026-11-13).
func deviceState(s *cloudpb.AssetApp, isOnline bool) (string, string) {
	switch s.GetOutcome() {
	case cloudpb.ApplyOutcome_APPLY_OUTCOME_APPLIED:
		if s.GetStatementPending() {
			break
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
		var out []*cloudpb.Asset
		for {
			offset, limit, compute := int32(len(out)), int32(500), true
			stream, err := cloudpb.NewAssetServiceClient(op.conn).ListAssets(ctx, &cloudpb.ListAssetsRequest{OrganizationId: op.org, IsComputeDevice: &compute, Offset: &offset, Limit: &limit, OnlineOnly: &onlineOnly})
			if err != nil {
				return nil, fmt.Errorf("reading the devices: %w", err)
			}
			var total int32
			before := len(out)
			for {
				r, err := stream.Recv()
				if err == io.EOF {
					break
				}
				if err != nil {
					return nil, fmt.Errorf("reading the devices: %w", err)
				}
				total = r.GetTotal()
				out = append(out, r.GetAsset())
			}
			if len(out) == before || int32(len(out)) >= total {
				return out, nil
			}
		}
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
	var out []*cloudpb.AppDeviceState
	token := ""
	for {
		resp, err := cloudpb.NewDeploymentServiceClient(op.conn).ListAppDeviceStates(ctx, &cloudpb.ListAppDeviceStatesRequest{OrganizationId: op.org, AppId: appID, DeploymentId: deploymentID, PageToken: token})
		if err != nil {
			return nil, fmt.Errorf("reading the devices of %s: %w", appID, err)
		}
		out = append(out, resp.GetRows()...)
		if token = resp.GetNextPageToken(); token == "" {
			return out, nil
		}
	}
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
	var cloudGRPC string
	show := &cobra.Command{
		Use:   "show <app>/<name>",
		Short: "Show a deployment and each device's state",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			appID, name, ok := strings.Cut(args[0], "/")
			if !ok || appID == "" || name == "" {
				return usageErrorf("Name the deployment as <app>/<name>, e.g. camera-gateway/line-3.")
			}
			auth, err := pickAuthEntry(cloudGRPC)
			if err != nil {
				return err
			}
			return withCloud(cmd.Context(), auth, func(ctx context.Context, conn grpc.ClientConnInterface) error {
				op, err := newRegistryOp(ctx, conn, auth)
				if err != nil {
					return err
				}
				return op.showDeployment(ctx, appID, name, cmd.OutOrStdout())
			})
		},
	}
	show.Flags().StringVar(&cloudGRPC, "cloud-grpc", "", "Cloud gRPC endpoint of the session to use")
	cmd.AddCommand(show)
	return cmd
}

// findDeployment is appID's live deployment called name.
func (op *registryOp) findDeployment(ctx context.Context, appID, name string) (*cloudpb.Deployment, error) {
	for offset := int32(0); ; {
		stream, err := cloudpb.NewDeploymentServiceClient(op.conn).ListDeployments(ctx, &cloudpb.ListDeploymentsRequest{OrganizationId: op.org, AppId: appID, Offset: offset})
		if err != nil {
			return nil, fmt.Errorf("reading the deployments of %s: %w", appID, err)
		}
		var seen, total int32
		for {
			page, err := stream.Recv()
			if err == io.EOF {
				break
			}
			if err != nil {
				return nil, fmt.Errorf("reading the deployments of %s: %w", appID, err)
			}
			total = page.GetTotal()
			for _, d := range page.GetDeployments() {
				seen++
				if d.GetName() == name && d.GetAppId() == appID && d.GetRemovedAt() == nil {
					return d, nil
				}
			}
		}
		if offset += seen; seen == 0 || offset >= total {
			return nil, usageErrorf("%s has no deployment %s.", appID, name)
		}
	}
}

func (op *registryOp) showDeployment(ctx context.Context, appID, name string, out io.Writer) error {
	say := func(format string, args ...any) { _, _ = fmt.Fprintf(out, format+"\n", args...) }
	d, err := op.findDeployment(ctx, appID, name)
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
	return op.progress(ctx, d, release, online, false, say)
}
