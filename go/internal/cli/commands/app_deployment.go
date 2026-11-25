package commands

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/durationpb"

	"github.com/wendylabsinc/wendy/go/internal/cli/cloudrequest"
	"github.com/wendylabsinc/wendy/go/internal/shared/certs"
	cloudpb "github.com/wendylabsinc/wendy/go/proto/gen/cloudpb/v2"
)

// Changing a deployment (spec 4.3): each change is one new revision, sent as
// expected_revision = the revision shown, and every device's statement is
// signed only from what the operator confirmed.

type changeFlags struct {
	release, configFile, keepTrying, note, cloudGRPC string
	to                                               int32
	config, removeConfig, addDevices, removeDevices  []string
	addTags, removeTags                              []string
	addSites, removeSites                            []string
	start, stop, move, yes                           bool
}

type changeRun func(ctx context.Context, op *registryOp, appID, name string, c changeFlags, out io.Writer, interactive bool) error

// deploymentSub is a `wendy app deployment <use> <app>/<name>` command; one
// without bind changes nothing and has no --yes.
func deploymentSub(use, short string, bind func(*cobra.Command, *changeFlags), run changeRun) *cobra.Command {
	var c changeFlags
	cmd := &cobra.Command{
		Use:   use + " <app>/<name>",
		Short: short,
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			appID, name, ok := strings.Cut(args[0], "/")
			if !ok || appID == "" || name == "" {
				return usageErrorf("Name the deployment as <app>/<name>, e.g. camera-gateway/line-3.")
			}
			auth, err := pickAuthEntry(c.cloudGRPC)
			if err != nil {
				return err
			}
			return withCloud(cmd.Context(), auth, func(ctx context.Context, conn grpc.ClientConnInterface) error {
				op, err := newRegistryOp(ctx, conn, auth)
				if err != nil {
					return err
				}
				return run(ctx, op, appID, name, c, cmd.OutOrStdout(), isInteractiveTerminal())
			})
		},
	}
	cmd.Flags().StringVar(&c.cloudGRPC, "cloud-grpc", "", "Cloud gRPC endpoint of the session to use")
	if bind != nil {
		cmd.Flags().BoolVarP(&c.yes, "yes", "y", false, "Apply without asking for confirmation")
		bind(cmd, &c)
	}
	return cmd
}

func deploymentChangeCmds() []*cobra.Command {
	note := func(cmd *cobra.Command, c *changeFlags) {
		cmd.Flags().StringVar(&c.note, "note", "", "Note for the history")
	}
	move := func(cmd *cobra.Command, c *changeFlags) {
		cmd.Flags().BoolVar(&c.move, "move", false, "Move devices that run the app in another deployment into this one")
	}
	return []*cobra.Command{
		deploymentSub("update", "Change a deployment's release, configuration, run state or devices", func(cmd *cobra.Command, c *changeFlags) {
			f := cmd.Flags()
			f.StringVar(&c.release, "release", "", "New release: its version (or id)")
			f.StringArrayVar(&c.config, "config", nil, "Set a value: NAME=VALUE (repeatable)")
			f.StringVar(&c.configFile, "config-file", "", "Set values from a JSON object of named values")
			f.StringArrayVar(&c.removeConfig, "remove-config", nil, "Remove a setting by name (repeatable)")
			f.BoolVar(&c.start, "start", false, "Run the app")
			f.BoolVar(&c.stop, "stop", false, "Stop the app")
			f.StringArrayVar(&c.addDevices, "add-device", nil, "Add a device by name or id (repeatable)")
			f.StringArrayVar(&c.removeDevices, "remove-device", nil, "Remove a device by name or id (repeatable)")
			f.StringArrayVar(&c.addTags, "add-tag", nil, "Add every device with this tag (repeatable)")
			f.StringArrayVar(&c.removeTags, "remove-tag", nil, "Stop selecting devices by this tag (repeatable)")
			f.StringArrayVar(&c.addSites, "add-site", nil, "Add every device in this site, by name or id (repeatable)")
			f.StringArrayVar(&c.removeSites, "remove-site", nil, "Stop selecting devices by this site (repeatable)")
			f.StringVar(&c.keepTrying, "keep-trying", "", "How long offline devices keep trying: 7d, 30d, 60d or 1y")
			note(cmd, c)
			move(cmd, c)
		}, runUpdate),
		deploymentSub("rollback", "Restore an earlier revision as a new revision", func(cmd *cobra.Command, c *changeFlags) {
			cmd.Flags().Int32Var(&c.to, "to", 0, "The revision to restore (required)")
			note(cmd, c)
			move(cmd, c)
		}, runRollback),
		deploymentSub("remove", "Remove a deployment from all its devices", note, runRemove),
		deploymentSub("resume", "Send the instructions devices are still waiting for", move, runResume),
	}
}

// confirmChange shows the review and asks, or needs --yes without a terminal.
func confirmChange(c changeFlags, interactive bool, question string) error {
	return confirmOrYes(c.yes, interactive, question, usageErrorf("Not confirmed. Add --yes to apply it without a terminal."), errors.New("nothing was changed"))
}

// confirmOrYes asks question unless yes. Without a terminal it is
// notConfirmed; a "no" answer is no.
func confirmOrYes(yes, interactive bool, question string, notConfirmed, no error) error {
	if yes {
		return nil
	}
	if !interactive {
		return notConfirmed
	}
	ok, err := askConfirm(question)
	if err != nil {
		return err
	}
	if !ok {
		return no
	}
	return nil
}

func runUpdate(ctx context.Context, op *registryOp, appID, name string, c changeFlags, out io.Writer, interactive bool) error {
	say := func(format string, args ...any) { _, _ = fmt.Fprintf(out, format+"\n", args...) }
	if c.start && c.stop {
		return usageErrorf("Choose one of --start and --stop.")
	}
	d, err := op.findDeployment(ctx, appID, name, false)
	if err != nil {
		return err
	}
	req := &cloudpb.UpdateDeploymentRequest{OrganizationId: op.org, DeploymentId: d.GetId(), ExpectedRevision: d.GetRevision(), Note: c.note, MoveDevices: c.move}
	var review []string
	rels, err := op.releases(ctx, appID)
	if err != nil {
		return err
	}
	var newRel *cloudpb.AppRelease
	if c.release != "" {
		i := slices.IndexFunc(rels, func(r *cloudpb.AppRelease) bool { return r.GetName() == c.release || r.GetId() == c.release })
		if i < 0 {
			return usageErrorf("%s has no release %s. Publish it first with wendy app publish.", appID, c.release)
		}
		if rels[i].GetId() != d.GetAppReleaseId() {
			newRel, req.AppReleaseId = rels[i], proto.String(rels[i].GetId())
			review = append(review, fmt.Sprintf("Release: %s → %s", releaseName(rels, d.GetAppReleaseId()), newRel.GetName()))
		}
	}
	if len(c.config) > 0 || c.configFile != "" || len(c.removeConfig) > 0 {
		text, changed, err := deployConfig(d.GetConfigJson(), deployAnswers{config: c.config, configFile: c.configFile, remove: c.removeConfig}, false, nil)
		if err != nil {
			return err
		}
		if changed > 0 {
			req.ConfigJson = proto.String(text)
			review = append(review, "Configuration: "+plural(changed, "value")+" changed")
		}
	}
	if c.start && d.GetDesiredState() != cloudpb.Running_RUNNING {
		req.DesiredState = cloudpb.Running_RUNNING.Enum()
		review = append(review, "Run state: start it")
	}
	if c.stop && d.GetDesiredState() != cloudpb.Running_STOPPED {
		req.DesiredState = cloudpb.Running_STOPPED.Enum()
		review = append(review, "Run state: stop it")
	}
	if c.keepTrying != "" {
		v, ok := keepTryingChoices[c.keepTrying]
		if !ok {
			return usageErrorf("--keep-trying must be 7d, 30d, 60d or 1y.")
		}
		req.Validity = durationpb.New(v)
	}
	cur, err := op.revisionDevices(ctx, d.GetId(), d.GetRevision())
	if err != nil {
		return err
	}
	assets, _, err := op.devices(ctx)
	if err != nil {
		return err
	}
	next := cur
	if len(c.addDevices)+len(c.removeDevices)+len(c.addTags)+len(c.removeTags)+len(c.addSites)+len(c.removeSites) > 0 {
		sel, err := op.currentSelection(ctx, d)
		if err != nil {
			return err
		}
		for _, n := range c.addDevices {
			id, err := assetID(assets, n)
			if err != nil {
				return err
			}
			if slices.Contains(sel.TargetDeviceIds, id) {
				return usageErrorf("%s is already in %s.", n, name)
			}
			sel.TargetDeviceIds = append(sel.TargetDeviceIds, id)
		}
		for _, n := range c.removeDevices {
			id, err := assetID(assets, n)
			if err != nil {
				return err
			}
			i := slices.Index(sel.TargetDeviceIds, id)
			if i < 0 {
				return usageErrorf("%s is not named in %s. If it is in through a tag or site, use --remove-tag or --remove-site.", n, name)
			}
			sel.TargetDeviceIds = slices.Delete(sel.TargetDeviceIds, i, i+1)
		}
		for _, t := range c.addTags {
			if len(withTag(assets, t)) == 0 {
				return usageErrorf("No device has the tag %s.", t)
			}
			sel.TargetTags = append(sel.TargetTags, t)
		}
		for _, t := range c.removeTags {
			i := slices.Index(sel.TargetTags, t)
			if i < 0 {
				return usageErrorf("%s does not select devices by the tag %s.", name, t)
			}
			sel.TargetTags = slices.Delete(sel.TargetTags, i, i+1)
		}
		if len(c.addSites)+len(c.removeSites) > 0 {
			sites, err := op.sites(ctx)
			if err != nil {
				return err
			}
			for _, n := range c.addSites {
				s, err := findSite(sites, n)
				if err != nil {
					return err
				}
				if len(inSite(assets, s.GetId())) == 0 {
					return usageErrorf("No device is in the site %s. Add devices to it under Sites in the dashboard first.", s.GetName())
				}
				sel.TargetSiteIds = append(sel.TargetSiteIds, s.GetId())
			}
			for _, n := range c.removeSites {
				s, err := findSite(sites, n)
				if err != nil {
					return err
				}
				i := slices.Index(sel.TargetSiteIds, s.GetId())
				if i < 0 {
					return usageErrorf("%s does not select devices by the site %s. To drop a single device, use --remove-device.", name, s.GetName())
				}
				sel.TargetSiteIds = slices.Delete(sel.TargetSiteIds, i, i+1)
			}
		}
		req.Devices = sel
		next = resolveSelection(assets, sel)
		review = append(review, "Devices: "+deviceDiff(assets, cur, next))
	}
	release := newRel
	if release == nil {
		if i := slices.IndexFunc(rels, func(r *cloudpb.AppRelease) bool { return r.GetId() == d.GetAppReleaseId() }); i >= 0 {
			release = rels[i]
		}
	}
	if err := checkRequired(orDefault(req.ConfigJson, d.GetConfigJson()), releaseRequired(release)); err != nil {
		return err
	}
	if err := checkDeployConfig(orDefault(req.ConfigJson, d.GetConfigJson()), nil, removeConfigFix); err != nil {
		return err
	}
	if len(review) == 0 && req.Validity == nil {
		return usageErrorf("Nothing to change. Add --release, --config, --remove-config, --start, --stop, --add-device, --remove-device, --add-tag, --remove-tag, --add-site, --remove-site or --keep-trying.")
	}
	content := req.AppReleaseId != nil || req.ConfigJson != nil || req.DesiredState != nil
	get := next
	if !content {
		get = without(next, cur)
	}
	// Statements are signed with the stored time unless --keep-trying changes it.
	validity := d.GetValidity().AsDuration()
	if req.Validity != nil {
		validity = req.Validity.AsDuration()
	}
	switch {
	case req.Validity != nil && !content:
		review = append(review, "Offline devices keep trying for "+keepTryingFor(validity)+" on instructions sent from now on. Devices that have theirs keep the time they got.")
	case len(get) > 0:
		review = append(review, "Offline devices keep trying for "+keepTryingFor(validity))
	}
	moves, err := op.confirmMoves(ctx, appID, d.GetId(), without(next, cur), assets, c.move, interactive)
	if err != nil {
		return err
	}
	say("Change %s/%s, revision %d → %d:", appID, name, d.GetRevision(), d.GetRevision()+1)
	for _, line := range review {
		say("     %s", line)
	}
	if err := confirmChange(c, interactive, fmt.Sprintf("Apply this change to %s?", name)); err != nil {
		return err
	}
	return op.change(ctx, d, newRel, get, without(cur, next), moves, true, validity, cloudpb.DeploymentService_UpdateDeployment_FullMethodName, req,
		func(n *cloudpb.Deployment) bool {
			return n.GetAppReleaseId() == orDefault(req.AppReleaseId, d.GetAppReleaseId()) && n.GetConfigJson() == orDefault(req.ConfigJson, d.GetConfigJson()) &&
				n.GetDesiredState() == orDefault(req.DesiredState, d.GetDesiredState())
		}, say)
}

func runRollback(ctx context.Context, op *registryOp, appID, name string, c changeFlags, out io.Writer, interactive bool) error {
	say := func(format string, args ...any) { _, _ = fmt.Fprintf(out, format+"\n", args...) }
	d, err := op.findDeployment(ctx, appID, name, false)
	if err != nil {
		return err
	}
	if c.to < 1 || c.to >= d.GetRevision() {
		return usageErrorf("Choose an earlier revision of %s with --to <1-%d>.", name, d.GetRevision()-1)
	}
	target, err := op.revision(ctx, d.GetId(), c.to)
	if err != nil {
		return err
	}
	if target.GetAppReleaseId() == "" {
		return usageErrorf("The release of revision %d was deleted, so it cannot be restored.", c.to)
	}
	rels, err := op.releases(ctx, appID)
	if err != nil {
		return err
	}
	var newRel *cloudpb.AppRelease
	if target.GetAppReleaseId() != d.GetAppReleaseId() {
		i := slices.IndexFunc(rels, func(r *cloudpb.AppRelease) bool { return r.GetId() == target.GetAppReleaseId() })
		if i < 0 {
			return usageErrorf("The release of revision %d was deleted, so it cannot be restored.", c.to)
		}
		newRel = rels[i]
	}
	// The same rule as deploy and update: the release rolled back to must get
	// every setting it requires.
	if i := slices.IndexFunc(rels, func(r *cloudpb.AppRelease) bool { return r.GetId() == target.GetAppReleaseId() }); i >= 0 {
		name, err := emptyRequired(target.GetConfigJson(), releaseRequired(rels[i]))
		if err != nil {
			return err
		}
		if name != "" {
			return usageErrorf("Revision %d leaves %s empty, which release %s requires. Choose another revision, or use wendy app deployment update --release %s --config %s=<value>.",
				c.to, name, rels[i].GetName(), rels[i].GetName(), name)
		}
	}
	// And the device's own check, as deploy and update run it.
	if setting, err := refusedSetting(target.GetConfigJson()); err != nil {
		return rollbackRefusal(appID, name, c.to, d, target, rels, setting, err)
	}
	cur, err := op.revisionDevices(ctx, d.GetId(), d.GetRevision())
	if err != nil {
		return err
	}
	next, err := op.revisionDevices(ctx, d.GetId(), c.to)
	if err != nil {
		return err
	}
	assets, _, err := op.devices(ctx)
	if err != nil {
		return err
	}
	moves, err := op.confirmMoves(ctx, appID, d.GetId(), without(next, cur), assets, c.move, interactive)
	if err != nil {
		return err
	}
	run := "run it now"
	if target.GetDesiredState() == cloudpb.Running_STOPPED {
		run = "install only"
	}
	say("Roll back %s/%s to revision %d, as revision %d:", appID, name, c.to, d.GetRevision()+1)
	say("     Release: %s", releaseName(rels, target.GetAppReleaseId()))
	say("     Configuration: %s", target.GetConfigJson())
	say("     Run state: %s", run)
	say("     Devices: %s", deviceDiff(assets, cur, next))
	say("     Offline devices keep trying for %s", keepTryingFor(target.GetValidity().AsDuration()))
	if err := confirmChange(c, interactive, fmt.Sprintf("Roll back %s to revision %d?", name, c.to)); err != nil {
		return err
	}
	req := &cloudpb.RollbackDeploymentRequest{OrganizationId: op.org, DeploymentId: d.GetId(), ExpectedRevision: d.GetRevision(), ToRevision: c.to, Note: c.note, MoveDevices: c.move}
	return op.change(ctx, d, newRel, next, without(cur, next), moves, false, target.GetValidity().AsDuration(), cloudpb.DeploymentService_RollbackDeployment_FullMethodName, req,
		func(n *cloudpb.Deployment) bool {
			return n.GetAppReleaseId() == target.GetAppReleaseId() && n.GetConfigJson() == target.GetConfigJson() && n.GetDesiredState() == target.GetDesiredState()
		}, say)
}

func runRemove(ctx context.Context, op *registryOp, appID, name string, c changeFlags, out io.Writer, interactive bool) error {
	say := func(format string, args ...any) { _, _ = fmt.Fprintf(out, format+"\n", args...) }
	d, err := op.findDeployment(ctx, appID, name, false)
	if err != nil {
		return err
	}
	cur, err := op.revisionDevices(ctx, d.GetId(), d.GetRevision())
	if err != nil {
		return err
	}
	assets, _, err := op.devices(ctx)
	if err != nil {
		return err
	}
	say("Remove %s/%s: %s is removed from %s: %s.", appID, name, appID, plural(len(cur), "device"), deviceNames(assets, cur))
	say("     A device that is offline removes the app when it next comes online.")
	if err := confirmChange(c, interactive, fmt.Sprintf("Remove %s from %s?", name, plural(len(cur), "device"))); err != nil {
		return err
	}
	resp := new(cloudpb.DeploymentChangeResponse)
	if err := cloudrequest.Invoke(ctx, op.conn, op.auth, cloudpb.DeploymentService_RemoveDeployment_FullMethodName, &cloudpb.RemoveDeploymentRequest{
		OrganizationId: op.org, DeploymentId: d.GetId(), ExpectedRevision: d.GetRevision(), Note: c.note,
	}, resp); err != nil {
		return fmt.Errorf("removing %s: %w", name, err)
	}
	if n := resp.GetDeployment(); n.GetId() != d.GetId() || n.GetRemovedAt() == nil {
		return fmt.Errorf("Cloud did not record %s as removed; nothing was sent to devices", name)
	}
	remove := uris(op.org, cur)
	return op.signPending(ctx, confirmed{d: resp.GetDeployment(), appID: appID, revision: maxRevision, remove: remove, must: remove}, resp.GetToSign(), resp.GetNextPageToken())
}

func runResume(ctx context.Context, op *registryOp, appID, name string, c changeFlags, out io.Writer, interactive bool) error {
	say := func(format string, args ...any) { _, _ = fmt.Fprintf(out, format+"\n", args...) }
	d, rows, err := op.resumeTarget(ctx, appID, name, say)
	if err != nil {
		return err
	}
	name = d.GetName()
	if len(rows) == 0 {
		say("Every device of %s has its instruction.", name)
		return nil
	}
	assets, _, err := op.devices(ctx)
	if err != nil {
		return err
	}
	rels, err := op.releases(ctx, appID)
	if err != nil {
		return err
	}
	revision := d.GetRevision()
	if d.GetRemovedAt() != nil {
		revision = maxRevision
		// Named by its id: a live deployment may carry the name now.
		say("%s/%s (deployment %s) is the removed deployment of that name. Devices still to be told:", appID, name, d.GetId())
	} else {
		run := "run it now"
		if d.GetDesiredState() == cloudpb.Running_STOPPED {
			run = "install only"
		}
		say("%s/%s revision %d: release %s, %s, configuration %s, offline devices keep trying for %s. Devices still to be told:", appID, name, revision,
			releaseName(rels, d.GetAppReleaseId()), run, d.GetConfigJson(), keepTryingFor(d.GetValidity().AsDuration()))
	}
	var gets []string
	remove, moves := map[string]bool{}, map[string]string{}
	states, err := op.appDeviceStates(ctx, appID, "")
	if err != nil {
		return err
	}
	for _, r := range rows {
		id := strings.TrimPrefix(r.GetDevice(), "spiffe://wendy.sh/tenant/"+op.org+"/device/")
		switch {
		case r.GetKind() == cloudpb.PendingDeviceStatement_KIND_UNASSIGN:
			remove[r.GetDevice()] = true
			say("     %s: remove the app", deviceNames(assets, []string{id}))
		case r.GetReplacesDeploymentId() != "":
			held := otherDeployment(states, id, appID)
			if held == nil || held.GetDeploymentId() != r.GetReplacesDeploymentId() {
				return fmt.Errorf("Cloud asked to move %s from a deployment it is not in; nothing was sent", deviceNames(assets, []string{id}))
			}
			dn, other := deviceNames(assets, []string{id}), op.deploymentName(ctx, held.GetDeploymentId())
			if err := confirmOrYes(c.move, interactive, fmt.Sprintf("%s moves here from the deployment %q. Move it?", dn, other),
				usageErrorf("%s moves here from the deployment %q. Add --move to send it.", dn, other), errors.New("nothing was sent")); err != nil {
				return err
			}
			moves[r.GetDevice()] = r.GetReplacesDeploymentId()
			gets = append(gets, id)
			say("     %s: move it here", deviceNames(assets, []string{id}))
		default:
			gets = append(gets, id)
			say("     %s: get revision %d", deviceNames(assets, []string{id}), revision)
		}
	}
	if err := confirmChange(c, interactive, fmt.Sprintf("Send %s?", plural(len(rows), "instruction"))); err != nil {
		return err
	}
	conf := confirmed{d: d, appID: appID, revision: revision, remove: remove, moves: moves}
	if len(gets) > 0 {
		if conf.assign, err = op.deviceKeys(ctx, gets); err != nil {
			return err
		}
		if conf.kd, err = op.current(ctx, d.GetId(), true); err != nil {
			return err
		}
		conf.kd.validity = d.GetValidity().AsDuration()
	}
	return op.signPending(ctx, conf, rows, "")
}

// resumeTarget is the deployment resume works on and what it still needs:
// the one whose id is given, else the one named so. When a removed
// deployment and a newer one share the name, the newest with devices still
// to be told is taken, and any other with some is named by its id.
func (op *registryOp) resumeTarget(ctx context.Context, appID, name string, say func(string, ...any)) (*cloudpb.Deployment, []*cloudpb.PendingDeviceStatement, error) {
	var named []*cloudpb.Deployment
	var byID *cloudpb.Deployment
	if err := op.eachDeployment(ctx, appID, true, func(d *cloudpb.Deployment) bool {
		if d.GetId() == name {
			byID = d
			return true
		}
		if d.GetName() == name {
			named = append(named, d)
		}
		return false
	}); err != nil {
		return nil, nil, err
	}
	if byID != nil {
		named = []*cloudpb.Deployment{byID}
	}
	if len(named) == 0 {
		return nil, nil, usageErrorf("%s has no deployment %s.", appID, name)
	}
	pending := func(d *cloudpb.Deployment) ([]*cloudpb.PendingDeviceStatement, error) {
		rows, err := pages(func(token string) ([]*cloudpb.PendingDeviceStatement, string, error) {
			page, err := cloudpb.NewDeploymentServiceClient(op.conn).ListPendingDeviceStatements(ctx, &cloudpb.ListPendingDeviceStatementsRequest{DeploymentId: d.GetId(), PageToken: token})
			return page.GetStatements(), page.GetNextPageToken(), err
		})
		if err != nil {
			return nil, fmt.Errorf("reading what %s still needs: %w", d.GetName(), err)
		}
		return rows, nil
	}
	target, rows := named[0], []*cloudpb.PendingDeviceStatement(nil)
	picked := false
	for _, d := range named {
		r, err := pending(d)
		if err != nil {
			return nil, nil, err
		}
		switch {
		case len(r) == 0:
		case !picked:
			target, rows, picked = d, r, true
		default:
			say("Deployment %s of the same name (removed) also has devices to be told: wendy app deployment resume %s/%s", d.GetId(), appID, d.GetId())
		}
	}
	return target, rows, nil
}

// change records one revision (method with req), checks that cloud recorded
// what was asked (want), and signs the statements: get receives the
// deployment, drop leaves it. A new release is sealed under a new key.
// With mustGet, every device in get must be asked for (an update: the
// changed content reaches each, or each is new).
// validity is the keep-trying time shown in the review, 0 for the stated default.
func (op *registryOp) change(ctx context.Context, d *cloudpb.Deployment, newRel *cloudpb.AppRelease, get, drop []string, moves map[string]string, mustGet bool,
	validity time.Duration, method string, req proto.Message, want func(*cloudpb.Deployment) bool, say func(string, ...any),
) error {
	conf := confirmed{appID: d.GetAppId(), remove: uris(op.org, drop), moves: map[string]string{}}
	for dev, from := range moves {
		conf.moves[certs.DeviceSPIFFEURI(op.org, dev)] = from
	}
	var m *cloudpb.ArtifactManifest
	var contentKey []byte
	var err error
	if len(get) > 0 {
		if conf.assign, err = op.deviceKeys(ctx, get); err != nil {
			return err
		}
		if newRel != nil {
			if m, err = op.artifact(ctx, newRel.GetId(), ""); err != nil {
				return err
			}
			if contentKey, err = op.contentKey(ctx, newRel.GetId(), m); err != nil {
				return err
			}
			defer clear(contentKey)
		} else if conf.kd, err = op.current(ctx, d.GetId(), true); err != nil {
			return err
		}
	}
	resp := new(cloudpb.DeploymentChangeResponse)
	if err := cloudrequest.Invoke(ctx, op.conn, op.auth, method, req, resp); err != nil {
		return fmt.Errorf("changing %s: %w", d.GetName(), err)
	}
	conf.d = resp.GetDeployment()
	if conf.d.GetId() != d.GetId() || conf.d.GetRevision() != d.GetRevision()+1 || !want(conf.d) {
		return fmt.Errorf("Cloud recorded a change of %s that differs from the one asked for; nothing was sent to devices", d.GetName())
	}
	conf.revision = conf.d.GetRevision()
	if m != nil {
		if conf.kd, err = op.newDeploymentKey(ctx, conf.d, m, contentKey); err != nil {
			say("Revision %d is recorded, but its key was not stored, so no device got it.", conf.revision)
			say("Send it later with: wendy app deployment resume %s/%s", d.GetAppId(), d.GetName())
			return fmt.Errorf("%s: %w", d.GetName(), err)
		}
	}
	if conf.kd != nil {
		conf.kd.validity = validity
	}
	conf.must = maps.Clone(conf.remove)
	if mustGet {
		maps.Copy(conf.must, uris(op.org, get))
	}
	if err := op.signPending(ctx, conf, resp.GetToSign(), resp.GetNextPageToken()); err != nil {
		say("Some devices did not get their instruction: %v", err)
		say("Send them later with: wendy app deployment resume %s/%s", d.GetAppId(), d.GetName())
		return err
	}
	say("%s is now at revision %d.", d.GetName(), conf.revision)
	return nil
}

// confirmMoves finds the added devices that hold another deployment of the
// app and gets the operator's consent to move each.
func (op *registryOp) confirmMoves(ctx context.Context, appID, deploymentID string, added []string, assets []*cloudpb.Asset, move, interactive bool) (map[string]string, error) {
	moves := map[string]string{}
	if len(added) == 0 {
		return moves, nil
	}
	states, err := op.appDeviceStates(ctx, appID, "")
	if err != nil {
		return nil, err
	}
	for _, id := range added {
		held := otherDeployment(states, id, appID)
		if held == nil || held.GetDeploymentId() == deploymentID {
			continue
		}
		dn, other := deviceNames(assets, []string{id}), op.deploymentName(ctx, held.GetDeploymentId())
		if err := confirmOrYes(move, interactive, fmt.Sprintf("%s already runs %s in the deployment %q. Move it to this one?", dn, appID, other),
			usageErrorf("%s already runs %s in the deployment %q. Add --move to move it, or leave it out.", dn, appID, other),
			usageErrorf("Leave %s out, or move it, then try again.", dn)); err != nil {
			return nil, err
		}
		moves[id] = held.GetDeploymentId()
	}
	return moves, nil
}

func (op *registryOp) revision(ctx context.Context, deploymentID string, n int32) (*cloudpb.DeploymentRevision, error) {
	for token := ""; ; {
		page, err := cloudpb.NewDeploymentServiceClient(op.conn).ListDeploymentRevisions(ctx, &cloudpb.ListDeploymentRevisionsRequest{DeploymentId: deploymentID, PageSize: 50, PageToken: token})
		if err != nil {
			return nil, fmt.Errorf("reading the history: %w", err)
		}
		for _, r := range page.GetRevisions() {
			if r.GetRevision() == n {
				return r, nil
			}
		}
		if token = page.GetNextPageToken(); token == "" {
			return nil, usageErrorf("There is no revision %d.", n)
		}
	}
}

func (op *registryOp) currentSelection(ctx context.Context, d *cloudpb.Deployment) (*cloudpb.DeviceSelection, error) {
	r, err := op.revision(ctx, d.GetId(), d.GetRevision())
	if err != nil {
		return nil, err
	}
	return proto.Clone(r.GetSelection()).(*cloudpb.DeviceSelection), nil
}

func (op *registryOp) revisionDevices(ctx context.Context, deploymentID string, n int32) ([]string, error) {
	out, err := pages(func(token string) ([]string, string, error) {
		page, err := cloudpb.NewDeploymentServiceClient(op.conn).ListDeploymentRevisionDevices(ctx, &cloudpb.ListDeploymentRevisionDevicesRequest{DeploymentId: deploymentID, Revision: n, PageToken: token})
		return page.GetDeviceIds(), page.GetNextPageToken(), err
	})
	if err != nil {
		return nil, fmt.Errorf("reading the devices of revision %d: %w", n, err)
	}
	return out, nil
}

// assetID resolves a device by its id, else by its name (unique in the
// organization) or its display name, a free label: only when exactly one
// device carries the string as either, so one device's name that is
// another's display name is refused, as a shared display name is.
func assetID(assets []*cloudpb.Asset, name string) (string, error) {
	if name == "" {
		return "", usageErrorf("No device %q in this organization.", name)
	}
	if i := slices.IndexFunc(assets, func(d *cloudpb.Asset) bool { return d.GetId() == name }); i >= 0 {
		return assets[i].GetId(), nil
	}
	var same []string
	id := ""
	for _, d := range assets {
		if d.GetName() == name || d.GetDisplayName() == name {
			id = d.GetId()
			same = append(same, fmt.Sprintf("%s (%s)", d.GetName(), d.GetId()))
		}
	}
	switch len(same) {
	case 0:
		return "", usageErrorf("No device %q in this organization.", name)
	case 1:
		return id, nil
	}
	return "", usageErrorf("%d devices are named or shown as %q: %s. Use the device id.", len(same), name, strings.Join(same, ", "))
}

func resolveSelection(assets []*cloudpb.Asset, sel *cloudpb.DeviceSelection) []string {
	set := map[string]bool{}
	for _, id := range sel.GetTargetDeviceIds() {
		set[id] = true
	}
	for _, t := range sel.GetTargetTags() {
		for _, d := range withTag(assets, t) {
			set[d.GetId()] = true
		}
	}
	for _, s := range sel.GetTargetSiteIds() {
		for _, d := range inSite(assets, s) {
			set[d.GetId()] = true
		}
	}
	return slices.Sorted(maps.Keys(set))
}

// pages collects every item of a page-token listing.
func pages[T any](list func(token string) ([]T, string, error)) ([]T, error) {
	var out []T
	for token := ""; ; {
		items, next, err := list(token)
		if err != nil {
			return nil, err
		}
		out = append(out, items...)
		if token = next; token == "" {
			return out, nil
		}
	}
}

// streamAll collects every item of an offset-paged streaming listing: open
// starts the stream at an offset, items reads one message's items and the
// total. It stops at the total or when a stream adds nothing.
func streamAll[R, T any](open func(offset int32) (grpc.ServerStreamingClient[R], error), items func(*R) ([]T, int32)) ([]T, error) {
	var out []T
	for {
		stream, err := open(int32(len(out)))
		if err != nil {
			return nil, err
		}
		before, total := len(out), int32(0)
		for {
			msg, err := stream.Recv()
			if err == io.EOF {
				break
			}
			if err != nil {
				return nil, err
			}
			var got []T
			got, total = items(msg)
			out = append(out, got...)
		}
		if len(out) == before || int32(len(out)) >= total {
			return out, nil
		}
	}
}

// without is a minus b.
func without(a, b []string) []string {
	var out []string
	for _, x := range a {
		if !slices.Contains(b, x) {
			out = append(out, x)
		}
	}
	return out
}

func deviceNames(assets []*cloudpb.Asset, ids []string) string {
	names := make([]string, 0, len(ids))
	for _, id := range ids {
		name := id
		if i := slices.IndexFunc(assets, func(d *cloudpb.Asset) bool { return d.GetId() == id }); i >= 0 {
			name = assetName(assets[i])
		}
		names = append(names, name)
	}
	return strings.Join(names, ", ")
}

func deviceDiff(assets []*cloudpb.Asset, cur, next []string) string {
	var parts []string
	if added := without(next, cur); len(added) > 0 {
		parts = append(parts, "add "+deviceNames(assets, added))
	}
	if removed := without(cur, next); len(removed) > 0 {
		parts = append(parts, "remove "+deviceNames(assets, removed))
	}
	if len(parts) == 0 {
		return "unchanged, " + plural(len(next), "device")
	}
	return strings.Join(parts, "; ") + " (" + plural(len(next), "device") + " in all)"
}

func uris(org string, ids []string) map[string]bool {
	out := map[string]bool{}
	for _, id := range ids {
		out[certs.DeviceSPIFFEURI(org, id)] = true
	}
	return out
}

func releaseName(rels []*cloudpb.AppRelease, id string) string {
	if i := slices.IndexFunc(rels, func(r *cloudpb.AppRelease) bool { return r.GetId() == id }); i >= 0 {
		return rels[i].GetName()
	}
	return id
}

func orDefault[T any](p *T, def T) T {
	if p != nil {
		return *p
	}
	return def
}

// rollbackRefusal is the error for rolling deployment d back to revision
// to, whose configuration devices refuse (setting is "" for a rule about the
// whole configuration). The command it gives moves to that revision's
// release with the current configuration, which update checks again.
func rollbackRefusal(appID, name string, to int32, d *cloudpb.Deployment, target *cloudpb.DeploymentRevision, rels []*cloudpb.AppRelease, setting string, refused error) error {
	what := fmt.Sprintf("Revision %d's configuration", to)
	if setting != "" {
		what = fmt.Sprintf("Revision %d sets %s", to, setting)
	}
	release := releaseName(rels, target.GetAppReleaseId())
	fix := []string{"wendy app deployment update", shellQuoteArg(appID + "/" + name)}
	if target.GetAppReleaseId() != d.GetAppReleaseId() {
		fix = append(fix, "--release", shellQuoteArg(release))
	}
	var current map[string]json.RawMessage
	_ = json.Unmarshal([]byte(d.GetConfigJson()), &current)
	if _, has := current[setting]; has && setting != "" {
		fix = append(fix, "--remove-config", shellQuoteArg(setting))
	}
	if len(fix) == 2 {
		return usageErrorf("%s, which devices refuse: %v. Choose another revision; the current one already runs release %s.", what, refused, release)
	}
	return usageErrorf("%s, which devices refuse: %v. Choose another revision, or keep the current configuration with: %s", what, refused, strings.Join(fix, " "))
}
