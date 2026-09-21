package commands

import (
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"time"

	"github.com/spf13/cobra"
	"github.com/wendylabsinc/wendy/go/internal/cli/armcalibrator"
)

type armCalibrationOptions struct {
	model, id, channel, gripper, output string
	demo, automatic, local              bool
}

var (
	armCalibrationRunLocal  = runArmCalibrationLocal
	armCalibrationRunRemote = runDeviceShell
)

func newCalibrateCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:     "calibrate",
		Short:   "Calibrate robot joints and grippers with a guided wizard",
		GroupID: "manage",
		Args:    cobra.NoArgs,
	}
	cmd.AddCommand(newCalibrateArmsCmd(), &cobra.Command{
		Use:   "profiles",
		Short: "List supported arm models without contacting hardware",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			args := []string{"profiles"}
			if jsonOutput {
				args = append(args, "--json")
			}
			return armCalibrationRunLocal(cmd, armcalibrator.Command(args))
		},
	})
	return cmd
}

func newCalibrateArmsCmd() *cobra.Command {
	var options armCalibrationOptions
	cmd := &cobra.Command{
		Use:   "arms <dof>",
		Short: "Walk through calibration for an arm with the given number of joints",
		Long: "Calibrate a YAM arm using a guided reference-pose, joint-range and gripper\n" +
			"wizard. DOF excludes the gripper; current YAM profiles have six arm joints.\n\n" +
			"The wizard runs on the selected Wendy device and needs Python 3.10+ and\n" +
			"Linux SocketCAN. Use --demo for an offline walkthrough or --local to run\n" +
			"on this Linux host. Model, arm ID, channel and gripper can be selected in\n" +
			"the wizard. Hardware calibration requires an interactive operator.\n\n" +
			"Calibration reads motor registers and saves software offsets. It does not\n" +
			"enable motors or overwrite factory zeros. Support the arm during the procedure.",
		Example: "  wendy calibrate arms 6 --device my-thor\n" +
			"  wendy calibrate arms 6 --device my-thor --model big_yam --channel can3 --id right-arm\n" +
			"  wendy calibrate arms 6 --demo\n" +
			"  wendy calibrate arms 6 --demo --auto --json",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			dof, err := strconv.Atoi(args[0])
			if err != nil || dof != 6 {
				return fmt.Errorf("no YAM profile supports %q DOF; use 'wendy calibrate arms 6' (gripper excluded)", args[0])
			}
			if err := validateArmCalibrationOptions(options); err != nil {
				return err
			}
			if (options.demo || options.local) && deviceFlag != "" {
				return fmt.Errorf("--device cannot be combined with --demo or --local")
			}
			if !options.automatic && !isInteractiveTerminal() {
				return fmt.Errorf("arm calibration needs an interactive terminal; preview offline with --demo --auto")
			}
			argv := []string{"arms", strconv.Itoa(dof)}
			for _, pair := range [][2]string{
				{"--model", options.model}, {"--id", options.id}, {"--channel", options.channel},
				{"--gripper", options.gripper}, {"--output", options.output},
			} {
				if pair[1] != "" {
					argv = append(argv, pair[0], pair[1])
				}
			}
			if options.demo {
				argv = append(argv, "--demo")
			}
			if options.automatic {
				argv = append(argv, "--auto")
			}
			if jsonOutput {
				argv = append(argv, "--json")
			}
			command := armcalibrator.Command(argv)
			if options.demo || options.local {
				return armCalibrationRunLocal(cmd, command)
			}
			return armCalibrationRunRemote(cmd, command)
		},
	}
	cmd.Flags().StringVar(&options.model, "model", "", "Arm model (big_yam, yam, yam_pro, yam_ultra, yam_ultra_2)")
	cmd.Flags().StringVar(&options.id, "id", "", "Stable name for this physical arm")
	cmd.Flags().StringVar(&options.channel, "channel", "", "CAN interface on the selected device")
	cmd.Flags().StringVar(&options.gripper, "gripper", "", "Gripper type (linear_4310, linear_3507, none)")
	cmd.Flags().StringVar(&options.output, "output", "", "Calibration output directory on the machine running the wizard")
	cmd.Flags().BoolVar(&options.demo, "demo", false, "Preview with synthetic measurements; no hardware connection")
	cmd.Flags().BoolVar(&options.automatic, "auto", false, "Run unattended (requires --demo)")
	cmd.Flags().BoolVar(&options.local, "local", false, "Use SocketCAN on this Linux host instead of a Wendy device")
	return cmd
}

func validateArmCalibrationOptions(options armCalibrationOptions) error {
	if options.automatic && !options.demo {
		return fmt.Errorf("--auto requires --demo; hardware calibration must be operator-guided")
	}
	if options.model != "" {
		switch options.model {
		case "big_yam", "yam", "yam_pro", "yam_ultra", "yam_ultra_2":
		default:
			return fmt.Errorf("unsupported YAM model %q", options.model)
		}
	}
	if options.gripper != "" && options.gripper != "none" && options.gripper != "linear_4310" && options.gripper != "linear_3507" {
		return fmt.Errorf("unsupported gripper %q", options.gripper)
	}
	if options.id != "" && !regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,63}$`).MatchString(options.id) {
		return fmt.Errorf("arm ID must contain 1–64 letters, digits, dots, underscores or hyphens")
	}
	if options.channel != "" && !regexp.MustCompile(`^[A-Za-z0-9_.-]{1,15}$`).MatchString(options.channel) {
		return fmt.Errorf("invalid CAN interface name %q", options.channel)
	}
	return nil
}

func runArmCalibrationLocal(cmd *cobra.Command, argv []string) error {
	python, err := armcalibrator.FindPython(cmd.Context())
	if err != nil {
		return err
	}
	process := exec.CommandContext(cmd.Context(), python, argv[1:]...)
	// Let the Python supervisor stop its reader and remove the temporary bundle
	// before a cancelled CLI context escalates to killing the process.
	process.Cancel = func() error {
		if err := process.Process.Signal(os.Interrupt); err != nil {
			return process.Process.Kill()
		}
		return nil
	}
	process.WaitDelay = 6 * time.Second
	process.Stdin = cmd.InOrStdin()
	process.Stdout = cmd.OutOrStdout()
	process.Stderr = cmd.ErrOrStderr()
	if err := process.Run(); err != nil {
		return fmt.Errorf("arm calibration stopped: %w", err)
	}
	return nil
}
