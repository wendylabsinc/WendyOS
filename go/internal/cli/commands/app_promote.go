package commands

import (
	"context"
	"fmt"

	"github.com/spf13/cobra"
	"google.golang.org/grpc"
)

// newAppPromoteCmd is `wendy app promote` (spec 4.5): publish the image the
// last wendy run built, then deploy that release to the device it ran on.
// The device replacing the running container instead of adopting it is the
// agent's gap (T5), named in the last line.
func newAppPromoteCmd() *cobra.Command {
	var p publishArgs
	var a deployAnswers
	var device string
	cmd := &cobra.Command{
		Use:   "promote",
		Short: "Publish what the last wendy run built and deploy it to the device it ran on",
		Long: "Publishes the image the last wendy run of this project built as a new release, then asks the\n" +
			"deploy questions with the app, the release and the device already answered.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := a.checkSteps(); err != nil {
				return err
			}
			interactive := isInteractiveTerminal()
			if device == "" && interactive {
				var err error
				if device, err = deployAsk("Device", "The device you ran it on: its name or id.", ""); err != nil {
					return err
				}
			}
			if device == "" {
				return missing("device chosen", "--device <name|id>", "pick it")
			}
			p.lastRun = true
			rel, err := publishProject(cmd, p)
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			_, _ = fmt.Fprintf(out, "Published %s of %s. Deploying it to %s.\n", rel.name, rel.app, device)
			a.app, a.release, a.digest, a.devices = rel.app, rel.id, rel.digest, []string{device}
			auth, err := pickAppAuth(p.cloudGRPC)
			if err != nil {
				return err
			}
			if err := withAppCloud(cmd.Context(), auth, func(ctx context.Context, conn grpc.ClientConnInterface) error {
				op, err := newRegistryOp(ctx, conn, auth)
				if err != nil {
					return err
				}
				return runDeploy(ctx, op, a, out, interactive)
			}); err != nil {
				return err
			}
			_, _ = fmt.Fprintf(out, "%s replaces the running app with the published release; it does not keep the running container yet.\n", device)
			return nil
		},
	}
	p.bind(cmd)
	a.bindSteps(cmd)
	cmd.Flags().StringVar(&device, "device", "", "The device you ran the app on: its name or id")
	return cmd
}
