package commands

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/wendylabsinc/wendy/go/internal/cli/cloudrequest"
	cloudpb "github.com/wendylabsinc/wendy/go/proto/gen/cloudpb/v2"
)

// Deleting an app (spec 4.4, the canvas "Delete app", DeleteAppRequest):
// every device the app was sent gets one signed statement, a removal by
// default or a detach the operator chose; a device the operator cannot reach
// can only be detached. The delete is resumable: running it again sends what
// is still uncovered.

// deleteBatch is how many devices one DeleteApp call carries.
const deleteBatch = 200

func newAppDeleteCmd() *cobra.Command {
	var detach []string
	var yes bool
	var cloudGRPC string
	cmd := &cobra.Command{
		Use:   "delete <app>",
		Short: "Delete an app, its releases and its deployments, and remove it from every device",
		Long: "Removes the app from every device that runs it and deletes all its releases. A device\n" +
			"can be detached instead: the app keeps running there as a direct run. A device you\n" +
			"can't reach can only be detached. Run it again to finish an interrupted delete.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			auth, err := pickAuthEntry(cloudGRPC)
			if err != nil {
				return err
			}
			return withCloud(cmd.Context(), auth, func(ctx context.Context, conn grpc.ClientConnInterface) error {
				op, err := newRegistryOp(ctx, conn, auth)
				if err != nil {
					return err
				}
				return runAppDelete(ctx, op, args[0], detach, yes, cmd.OutOrStdout(), isInteractiveTerminal())
			})
		},
	}
	cmd.Flags().StringArrayVar(&detach, "detach", nil, "Detach this device instead of removing the app from it: name or id (repeatable)")
	cmd.Flags().BoolVarP(&yes, "yes", "y", false, "Delete without asking for confirmation")
	cmd.Flags().StringVar(&cloudGRPC, "cloud-grpc", "", "Cloud gRPC endpoint of the session to use")
	return cmd
}

func runAppDelete(ctx context.Context, op *registryOp, appID string, detachFlags []string, yes bool, out io.Writer, interactive bool) error {
	say := func(format string, args ...any) { _, _ = fmt.Fprintf(out, format+"\n", args...) }
	app, err := cloudpb.NewAppServiceClient(op.conn).GetApp(ctx, &cloudpb.GetAppRequest{Id: appID, OrganizationId: op.org})
	if err != nil {
		return fmt.Errorf("reading %s: %w", appID, err)
	}
	var rows []*cloudpb.PendingDeviceStatement
	for token := ""; ; {
		page, err := cloudpb.NewDeploymentServiceClient(op.conn).ListPendingDeviceStatements(ctx, &cloudpb.ListPendingDeviceStatementsRequest{AppId: appID, PageToken: token})
		if err != nil {
			return fmt.Errorf("reading the devices of %s: %w", appID, err)
		}
		rows = append(rows, page.GetStatements()...)
		if token = page.GetNextPageToken(); token == "" {
			break
		}
	}
	name := func(r *cloudpb.PendingDeviceStatement) string {
		if r.GetDeviceDisplayName() != "" {
			return r.GetDeviceDisplayName()
		}
		return strings.TrimPrefix(r.GetDevice(), "spiffe://wendy.sh/tenant/"+op.org+"/device/")
	}
	detach := map[string]bool{} // device URI -> detached
	for _, d := range detachFlags {
		found := false
		for _, r := range rows {
			if d == name(r) || deviceURI(op.org, d) == r.GetDevice() {
				detach[r.GetDevice()], found = true, true
			}
		}
		if !found {
			return usageErrorf("%s does not run %s. Name only devices that run it with --detach.", d, appID)
		}
	}
	if !interactive {
		unreachable := 0
		for _, r := range rows {
			if r.GetKind() == cloudpb.PendingDeviceStatement_KIND_DETACH && !detach[r.GetDevice()] {
				unreachable++
			}
		}
		if unreachable > 0 {
			return onlyDetach(unreachable)
		}
	}

	started := app.GetDeletionStartedAt() != nil
	// A resumed delete shows and asks exactly like a first run: it signs only
	// for devices the operator saw and confirmed.
	if started {
		say("%s is being deleted: %s a removal or a detach. Finishing.", appID, plural(len(rows), "device still needs", "devices still need"))
	}
	{
		if !yes {
			if !interactive {
				return usageErrorf("Not confirmed. Add --yes to delete it without a terminal.")
			}
			ok, err := deployConfirm(fmt.Sprintf("Delete %s? This removes it from every device that runs it and deletes all its releases.", appID))
			if err != nil {
				return err
			}
			if !ok {
				return errors.New("nothing was deleted")
			}
		}
		if len(rows) > 0 {
			rels, err := op.releases(ctx, appID)
			if err != nil {
				return err
			}
			say("%s is still running on %s", appID, plural(len(rows), "device"))
			var order []string
			byDeployment := map[string][]string{}
			for _, r := range rows {
				if byDeployment[r.GetDeploymentId()] == nil {
					order = append(order, r.GetDeploymentId())
				}
				byDeployment[r.GetDeploymentId()] = append(byDeployment[r.GetDeploymentId()], name(r))
			}
			for _, id := range order {
				say("Deployment %q: %s: %s", op.deploymentName(ctx, id), plural(len(byDeployment[id]), "device"), strings.Join(byDeployment[id], ", "))
			}
			var relNames []string
			for _, r := range rels {
				relNames = append(relNames, cmp.Or(r.GetName(), r.GetId()))
			}
			if len(rels) > 0 {
				say("%s: %s", plural(len(rels), "release"), strings.Join(relNames, ", "))
			}
			say("Deleting the app removes it from these devices and deletes its %s. This cannot be undone.", plural(len(rels), "release"))
			say("A device that is offline removes the app when it next comes online.")
		}
	}

	for _, r := range rows {
		if detach[r.GetDevice()] || !interactive {
			continue
		}
		if r.GetKind() == cloudpb.PendingDeviceStatement_KIND_DETACH {
			if err := askUnreachable(name(r), say); err != nil {
				return err
			}
			detach[r.GetDevice()] = true
			continue
		}
		ok, err := deployConfirm(fmt.Sprintf("Detach %s instead? The app keeps running there as a direct run.", name(r)))
		if err != nil {
			return err
		}
		detach[r.GetDevice()] = ok
	}
	for _, r := range rows {
		if detach[r.GetDevice()] {
			say("Detached devices keep the app, its image and its configuration, which may hold secrets. The app keeps running and restarts with the device. It stays until it is removed there or the device is reflashed.")
			break
		}
	}
	if len(rows) > 0 && !yes {
		typed, err := deployAsk("Type the app name to confirm", "", "")
		if err != nil {
			return err
		}
		if typed != appID {
			return errors.New("nothing was deleted")
		}
	}

	if err := op.sendDelete(ctx, appID, rows, detach, started, interactive, name, say); err != nil {
		return fmt.Errorf("%w\nFinish it later with: wendy app delete %s", err, appID)
	}
	say("%s is deleted.", appID)
	for _, r := range rows {
		if detach[r.GetDevice()] {
			say("%s keeps running %s as a direct run.", name(r), appID)
		}
	}
	return nil
}

// onlyDetach is the error for n devices the operator cannot reach, without a
// terminal to ask about them.
func onlyDetach(n int) error {
	return usageErrorf("%s only be detached. Add --detach <device> for each, or run in a terminal.", plural(n, "device can", "devices can"))
}

// askUnreachable asks to detach a device the operator cannot reach, the only
// statement they may sign for it.
func askUnreachable(device string, say func(string, ...any)) error {
	say("You can't reach %s. It can only be detached: the app keeps running there.", device)
	ok, err := deployConfirm(fmt.Sprintf("Detach %s?", device))
	if err != nil {
		return err
	}
	if !ok {
		return errors.New("Nothing was deleted. A device you can't reach can only be detached.")
	}
	return nil
}

// sendDelete sends DeleteApp in batches, each entry signed fresh, until
// cloud deletes the app.
func (op *registryOp) sendDelete(ctx context.Context, appID string, rows []*cloudpb.PendingDeviceStatement, detach map[string]bool, started, interactive bool,
	name func(*cloudpb.PendingDeviceStatement) string, say func(string, ...any),
) error {
	for i := 0; ; {
		end := min(i+deleteBatch, len(rows))
		req := &cloudpb.DeleteAppRequest{Id: appID, OrganizationId: op.org}
		valid := timestamppb.New(time.Now().Add(cloudrequest.MaxValidity)) // removals and detaches: the platform cap
		for _, r := range rows[i:end] {
			method := cloudpb.DeploymentService_UnassignDevice_FullMethodName
			var stmt proto.Message = &cloudpb.UnassignDeviceRequest{OrganizationId: op.org, DeploymentId: r.GetDeploymentId(), Device: r.GetDevice(), Revision: maxRevision, ValidUntil: valid}
			if detach[r.GetDevice()] {
				method = cloudpb.DeploymentService_DetachDevice_FullMethodName
				stmt = &cloudpb.DetachDeviceRequest{OrganizationId: op.org, DeploymentId: r.GetDeploymentId(), Device: r.GetDevice(), AppId: appID, ValidUntil: valid}
			}
			signed, err := cloudrequest.Sign(ctx, op.auth, method, stmt)
			if err != nil {
				return err
			}
			if detach[r.GetDevice()] {
				req.DeviceDetachments = append(req.DeviceDetachments, signed)
			} else {
				req.DeviceRemovals = append(req.DeviceRemovals, signed)
			}
		}
		err := cloudrequest.Invoke(ctx, op.conn, op.auth, cloudpb.AppService_DeleteApp_FullMethodName, req, new(cloudpb.DeleteAppResponse))
		st := status.Convert(err)
		switch {
		case err == nil:
			return nil
		case st.Code() == codes.NotFound && (started || i > 0): // a call already completed it
			return nil
		case st.Code() == codes.FailedPrecondition && strings.Contains(st.Message(), "still need") && end < len(rows):
			i = end
			continue
		case strings.Contains(st.Message(), "can't reach this device"):
			// The named device's row became detach-only since it was read:
			// ask again, then resend the batch signed fresh.
			for _, r := range rows[i:end] {
				id := strings.TrimPrefix(r.GetDevice(), "spiffe://wendy.sh/tenant/"+op.org+"/device/")
				if detach[r.GetDevice()] || !strings.Contains(st.Message(), id) {
					continue
				}
				if !interactive {
					return onlyDetach(1)
				}
				if err := askUnreachable(name(r), say); err != nil {
					return err
				}
				detach[r.GetDevice()] = true
				err = nil
				break
			}
			if err == nil {
				continue
			}
		}
		return err
	}
}
