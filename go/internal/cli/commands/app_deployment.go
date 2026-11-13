package commands

import (
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"slices"
	"strings"

	"github.com/spf13/cobra"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/durationpb"

	"github.com/wendylabsinc/wendy/go/internal/cli/cloudrequest"
	cloudpb "github.com/wendylabsinc/wendy/go/proto/gen/cloudpb/v2"
)

// Changing a deployment (spec 4.3): each change is one new revision, sent as
// expected_revision = the revision shown, and every device's statement is
// signed only from what the operator confirmed.

type changeFlags struct {
	release, configFile, keepTrying, note, cloudGRPC       string
	to                                                     int32
	config, addDevices, removeDevices, addTags, removeTags []string
	start, stop, move, yes                                 bool
}

type changeRun func(ctx context.Context, op *registryOp, appID, name string, c changeFlags, out io.Writer, interactive bool) error

// deploymentSub is a `wendy app deployment <use> <app>/<name>` command.
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
	cmd.Flags().BoolVarP(&c.yes, "yes", "y", false, "Apply without asking for confirmation")
	bind(cmd, &c)
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
			f.BoolVar(&c.start, "start", false, "Run the app")
			f.BoolVar(&c.stop, "stop", false, "Stop the app")
			f.StringArrayVar(&c.addDevices, "add-device", nil, "Add a device by name or id (repeatable)")
			f.StringArrayVar(&c.removeDevices, "remove-device", nil, "Remove a device by name or id (repeatable)")
			f.StringArrayVar(&c.addTags, "add-tag", nil, "Add every device with this tag (repeatable)")
			f.StringArrayVar(&c.removeTags, "remove-tag", nil, "Stop selecting devices by this tag (repeatable)")
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
	if c.yes {
		return nil
	}
	if !interactive {
		return usageErrorf("Not confirmed. Add --yes to apply it without a terminal.")
	}
	ok, err := deployConfirm(question)
	if err != nil {
		return err
	}
	if !ok {
		return errors.New("nothing was changed")
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
	if len(c.config) > 0 || c.configFile != "" {
		text, changed, err := deployConfig(d.GetConfigJson(), deployAnswers{config: c.config, configFile: c.configFile}, false, nil)
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
		review = append(review, "Offline devices keep trying for "+keepTryingWords[c.keepTrying])
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
	if len(c.addDevices)+len(c.removeDevices)+len(c.addTags)+len(c.removeTags) > 0 {
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
				return usageErrorf("%s is not named in %s. If it is in through a tag, use --remove-tag.", n, name)
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
		req.Devices = sel
		next = resolveSelection(assets, sel)
		review = append(review, "Devices: "+deviceDiff(assets, cur, next))
	}
	if len(review) == 0 {
		return usageErrorf("Nothing to change. Add --release, --config, --start, --stop, --add-device, --remove-device, --add-tag, --remove-tag or --keep-trying.")
	}
	content := req.AppReleaseId != nil || req.ConfigJson != nil || req.DesiredState != nil
	get := next
	if !content {
		get = without(next, cur)
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
	return op.change(ctx, d, newRel, get, without(cur, next), moves, true, cloudpb.DeploymentService_UpdateDeployment_FullMethodName, req,
		func(n *cloudpb.Deployment) bool {
			return n.GetAppReleaseId() == orDefault(req.AppReleaseId, d.GetAppReleaseId()) && n.GetConfigJson() == orDefault(req.ConfigJson, d.GetConfigJson()) &&
				n.GetDesiredState() == orDefaultState(req.DesiredState, d.GetDesiredState())
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
	if err := confirmChange(c, interactive, fmt.Sprintf("Roll back %s to revision %d?", name, c.to)); err != nil {
		return err
	}
	req := &cloudpb.RollbackDeploymentRequest{OrganizationId: op.org, DeploymentId: d.GetId(), ExpectedRevision: d.GetRevision(), ToRevision: c.to, Note: c.note, MoveDevices: c.move}
	return op.change(ctx, d, newRel, next, without(cur, next), moves, false, cloudpb.DeploymentService_RollbackDeployment_FullMethodName, req,
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
	d, err := op.findDeployment(ctx, appID, name, true)
	if err != nil {
		return err
	}
	var rows []*cloudpb.PendingDeviceStatement
	for token := ""; ; {
		page, err := cloudpb.NewDeploymentServiceClient(op.conn).ListPendingDeviceStatements(ctx, &cloudpb.ListPendingDeviceStatementsRequest{DeploymentId: d.GetId(), PageToken: token})
		if err != nil {
			return fmt.Errorf("reading what %s still needs: %w", name, err)
		}
		rows = append(rows, page.GetStatements()...)
		if token = page.GetNextPageToken(); token == "" {
			break
		}
	}
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
		say("%s/%s is removed. Devices still to be told:", appID, name)
	} else {
		run := "run it now"
		if d.GetDesiredState() == cloudpb.Running_STOPPED {
			run = "install only"
		}
		say("%s/%s revision %d: release %s, %s, configuration %s. Devices still to be told:", appID, name, revision, releaseName(rels, d.GetAppReleaseId()), run, d.GetConfigJson())
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
			if !c.move {
				if !interactive {
					return usageErrorf("%s moves here from the deployment %q. Add --move to send it.", deviceNames(assets, []string{id}), op.deploymentName(ctx, held.GetDeploymentId()))
				}
				ok, err := deployConfirm(fmt.Sprintf("%s moves here from the deployment %q. Move it?", deviceNames(assets, []string{id}), op.deploymentName(ctx, held.GetDeploymentId())))
				if err != nil {
					return err
				}
				if !ok {
					return errors.New("nothing was sent")
				}
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
		if conf.kd, err = op.current(ctx, d.GetId()); err != nil {
			return err
		}
	}
	return op.signPending(ctx, conf, rows, "")
}

// change records one revision (method with req), checks that cloud recorded
// what was asked (want), and signs the statements: get receives the
// deployment, drop leaves it. A new release is sealed under a new key.
// With mustGet, every device in get must be asked for (an update: the
// changed content reaches each, or each is new).
func (op *registryOp) change(ctx context.Context, d *cloudpb.Deployment, newRel *cloudpb.AppRelease, get, drop []string, moves map[string]string, mustGet bool,
	method string, req proto.Message, want func(*cloudpb.Deployment) bool, say func(string, ...any),
) error {
	conf := confirmed{appID: d.GetAppId(), remove: uris(op.org, drop), moves: map[string]string{}}
	for dev, from := range moves {
		conf.moves[deviceURI(op.org, dev)] = from
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
		} else if conf.kd, err = op.current(ctx, d.GetId()); err != nil {
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
			return fmt.Errorf("%s: %w", d.GetName(), err)
		}
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
		if !move {
			if !interactive {
				return nil, usageErrorf("%s already runs %s in the deployment %q. Add --move to move it, or leave it out.", dn, appID, other)
			}
			ok, err := deployConfirm(fmt.Sprintf("%s already runs %s in the deployment %q. Move it to this one?", dn, appID, other))
			if err != nil {
				return nil, err
			}
			if !ok {
				return nil, usageErrorf("Leave %s out, or move it, then try again.", dn)
			}
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
	var out []string
	for token := ""; ; {
		page, err := cloudpb.NewDeploymentServiceClient(op.conn).ListDeploymentRevisionDevices(ctx, &cloudpb.ListDeploymentRevisionDevicesRequest{DeploymentId: deploymentID, Revision: n, PageToken: token})
		if err != nil {
			return nil, fmt.Errorf("reading the devices of revision %d: %w", n, err)
		}
		out = append(out, page.GetDeviceIds()...)
		if token = page.GetNextPageToken(); token == "" {
			return out, nil
		}
	}
}

func assetID(assets []*cloudpb.Asset, name string) (string, error) {
	i := slices.IndexFunc(assets, func(d *cloudpb.Asset) bool {
		return d.GetId() == name || d.GetName() == name || d.GetDisplayName() == name
	})
	if i < 0 || name == "" {
		return "", usageErrorf("No device %q in this organization.", name)
	}
	return assets[i].GetId(), nil
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
	return slices.Sorted(maps.Keys(set))
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
		out[deviceURI(org, id)] = true
	}
	return out
}

func releaseName(rels []*cloudpb.AppRelease, id string) string {
	if i := slices.IndexFunc(rels, func(r *cloudpb.AppRelease) bool { return r.GetId() == id }); i >= 0 {
		return rels[i].GetName()
	}
	return id
}

func orDefault(p *string, def string) string {
	if p != nil {
		return *p
	}
	return def
}

func orDefaultState(p *cloudpb.Running, def cloudpb.Running) cloudpb.Running {
	if p != nil {
		return *p
	}
	return def
}
