package commands

import (
	"context"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"

	"github.com/spf13/cobra"
	"google.golang.org/grpc"

	"github.com/wendylabsinc/wendy/go/internal/shared/appconfig"
	cloudpb "github.com/wendylabsinc/wendy/go/proto/gen/cloudpb/v2"
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
				if device, err = askText("Device", "The device you ran it on: its name or id.", "", nil); err != nil {
					return err
				}
			}
			if device == "" {
				return missing("device chosen", "--device <name|id>", "pick it")
			}
			raw, err := os.ReadFile("wendy.json")
			if err != nil {
				return fmt.Errorf("reading wendy.json: %w", err)
			}
			appCfg, _, err := appDefinition(raw, p.appID)
			if err != nil {
				return err
			}
			auth, err := pickAppAuth(p.cloudGRPC)
			if err != nil {
				return err
			}
			var platform string
			if err := withAppCloud(cmd.Context(), auth, func(ctx context.Context, conn grpc.ClientConnInterface) error {
				op, err := newRegistryOp(ctx, conn, auth)
				if err != nil {
					return err
				}
				platform, err = promoteCheck(ctx, op, appCfg, a, device, interactive)
				return err
			}); err != nil {
				return err
			}
			if !cmd.Flags().Changed("platform") {
				p.platform = platform
			}
			p.lastRun = true
			rel, err := publishProject(cmd, p)
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			_, _ = fmt.Fprintf(out, "Published %s of %s. Deploying it to %s.\n", rel.name, rel.app, device)
			a.app, a.release, a.digest, a.devices = rel.app, rel.id, rel.digest, []string{device}
			if err := withAppCloud(cmd.Context(), auth, func(ctx context.Context, conn grpc.ClientConnInterface) error {
				op, err := newRegistryOp(ctx, conn, auth)
				if err != nil {
					return err
				}
				return runDeploy(ctx, op, a, out, interactive)
			}); err != nil {
				if d, ok := errors.AsType[*deployedError](err); ok {
					return fmt.Errorf("release %s of %s is published and deployment %s/%s was made, but: %w\nSee it with: wendy app deployment show %s/%s", rel.name, rel.app, d.app, d.name, err, d.app, d.name)
				}
				a.release = rel.name
				return fmt.Errorf("release %s of %s is published, but nothing was deployed: %w\nDeploy it with: %s", rel.name, rel.app, err, deployCommand(a))
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

// promoteCheck refuses, before anything is published, what deploying to
// device would refuse: an unknown device and, without a terminal, a missing
// answer or a configuration with an empty required setting or one a device
// refuses. In a terminal the deploy questions ask for what is missing. It
// returns the platform wendy run builds for device.
func promoteCheck(ctx context.Context, op *registryOp, cfg *appconfig.AppConfig, a deployAnswers, device string, interactive bool) (string, error) {
	assets, _, err := op.devices(ctx)
	if err != nil {
		return "", err
	}
	id, err := assetID(assets, device)
	if err != nil {
		return "", err
	}
	i := slices.IndexFunc(assets, func(d *cloudpb.Asset) bool { return d.GetId() == id })
	platform := resolveAgentPlatform(cfg.Platform, "", deviceArch(assets[i].GetArchitecture()))
	if interactive {
		return platform, nil
	}
	switch {
	case a.name == "":
		return "", missing("deployment name given", "--name <name>", "enter one")
	case !a.run && !a.installOnly:
		return "", missing("run state chosen", "--run or --install-only", "choose")
	case !a.yes:
		return "", usageErrorf("Not confirmed. Add --yes to deploy without a terminal.")
	}
	if _, err := op.confirmMoves(ctx, cfg.AppID, "", []string{id}, assets, a.move, false); err != nil {
		return "", err
	}
	config, _, err := deployConfig(releaseDefaults(cfg), a, false, nil)
	if err != nil {
		return "", err
	}
	return platform, checkDeployConfig(config, cfg.RequiredEnv, removeConfigFix)
}

// deviceArch is the Go architecture name of a device's CPU as Cloud lists
// it; none is arm64, as wendy run assumes when the agent reports none.
func deviceArch(arch string) string {
	switch arch = strings.ToLower(strings.TrimSpace(arch)); arch {
	case "", "arm64", "aarch64":
		return "arm64"
	case "amd64", "x86_64":
		return "amd64"
	}
	return arch
}
