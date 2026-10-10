package commands

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"github.com/wendylabsinc/wendy/go/internal/cli/grpcclient"
	"github.com/wendylabsinc/wendy/go/internal/cli/robotcalclient"
	"github.com/wendylabsinc/wendy/go/internal/cli/robotwizard"
	"github.com/wendylabsinc/wendy/go/internal/cli/tui"
	"github.com/wendylabsinc/wendy/go/internal/shared/robotcal"
)

// calibrateOptions are the flags the whole calibrate subtree shares.
type calibrateOptions struct {
	// profileKind selects which robot this is. It is a flag only until Part 1's
	// `wendy device robot configure` records the profile on the device itself;
	// the unit record already carries the kind, so a second run against the
	// same unit does not need it.
	profileKind string
	// unit names which robot on this device. An SO-101 rig is a leader and a
	// follower on one Jetson, with different measured travel on every joint, so
	// records are keyed per unit rather than per device.
	unit string
	// stableID binds the unit to the device behind it, in the platform's
	// existing stable-id scheme (by-id:… survives a port move, by-path:… is
	// topology). Not a raw serial: a servo bus on /dev/ttyACM1 renumbers across
	// boots exactly as /dev/videoN does.
	stableID string
}

const defaultRobotUnit = "default"

func newDeviceRobotCalibrateCmd() *cobra.Command {
	opts := &calibrateOptions{}

	cmd := &cobra.Command{
		Use:   "calibrate [id]",
		Short: "Run a calibration this robot needs, and record whether it passed",
		Long: "Run one of the calibration procedures the robot's profile says it needs.\n\n" +
			"With no id the wizard lists what is missing and asks which to run. " +
			"The procedure loop is the platform's — preconditions, then a statement of what " +
			"will physically happen, then samples, then a solve judged against the profile's " +
			"budget. Running a procedure qualifies nothing; fitting inside the budget does.\n\n" +
			"Steps that need a person have no non-interactive mode. `calibrate status` and " +
			"`calibrate clear` stay scriptable.",
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			id := ""
			if len(args) == 1 {
				id = args[0]
			}
			return runRobotCalibrate(cmd, opts, id)
		},
	}

	cmd.PersistentFlags().StringVar(&opts.profileKind, "profile", "",
		"Robot profile to run against; `calibrate profiles` lists them "+
			"(default: what this unit was last calibrated as)")
	cmd.PersistentFlags().StringVar(&opts.unit, "unit", defaultRobotUnit,
		"Which robot on this device, when it carries more than one (e.g. leader, follower)")
	cmd.PersistentFlags().StringVar(&opts.stableID, "stable-id", "",
		"Stable id of the device backing this unit (by-id:… or by-path:…), recorded with its calibrations")

	cmd.AddCommand(newRobotCalibrateStatusCmd(opts), newRobotCalibrateClearCmd(opts), newRobotProfilesCmd())
	return cmd
}

func newRobotCalibrateStatusCmd(opts *calibrateOptions) *cobra.Command {
	return &cobra.Command{
		Use:   "status",
		Short: "Show which calibrations are qualified, stale or never run",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runRobotCalibrateStatus(cmd, opts)
		},
	}
}

func newRobotCalibrateClearCmd(opts *calibrateOptions) *cobra.Command {
	return &cobra.Command{
		Use:   "clear <id>",
		Short: "Invalidate a calibration, after a repair or a part swap",
		Long: "Delete a stored calibration.\n\n" +
			"This matters more than it looks: after a hand is replaced, the old calibration is " +
			"worse than none, because everything downstream still trusts it.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runRobotCalibrateClear(cmd, opts, args[0])
		},
	}
}

func newRobotProfilesCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "profiles",
		Short: "List the robot profiles WendyOS ships",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			out := cmd.OutOrStdout()
			type entry struct {
				Kind        string   `json:"kind"`
				DisplayName string   `json:"display_name,omitempty"`
				Joints      int      `json:"joints"`
				Unit        string   `json:"unit"`
				Requires    []string `json:"requires_calibration,omitempty"`
			}
			var entries []entry
			for _, kind := range robotcal.ProfileKinds() {
				p, err := robotcal.LoadProfile(kind)
				if err != nil {
					return err
				}
				e := entry{Kind: p.Kind, DisplayName: p.DisplayName, Joints: len(p.Joints.Order), Unit: p.Joints.Unit}
				for _, proc := range p.RequiresCalibration {
					e.Requires = append(e.Requires, proc.ID)
				}
				entries = append(entries, e)
			}
			if jsonOutput {
				return json.NewEncoder(out).Encode(entries)
			}
			headers := []string{"Kind", "Name", "Joints", "Unit", "Requires calibration"}
			rows := make([][]string, 0, len(entries))
			for _, e := range entries {
				rows = append(rows, []string{e.Kind, e.DisplayName, fmt.Sprint(e.Joints), e.Unit, strings.Join(e.Requires, ", ")})
			}
			fmt.Fprint(out, tui.RenderTable(headers, rows))
			return nil
		},
	}
}

// robotSession is a resolved calibration context: the device connection, the
// store behind it, and the profile this unit is.
type robotSession struct {
	conn    *grpcclient.AgentConnection
	store   robotcal.Store
	profile *robotcal.Profile
	unit    robotcal.UnitRecord
}

func (s *robotSession) Close() {
	if s.conn != nil {
		s.conn.Close()
	}
}

// openRobotSession dials the device and resolves the store and profile.
//
// The dial goes through connectToAgent, which is what gives this command its
// cloud twin (`wendy cloud device robot calibrate`) and its on-device socket
// path for free. Dialling gRPC directly here would silently lose both.
func openRobotSession(ctx context.Context, opts *calibrateOptions) (*robotSession, error) {
	if err := robotcal.ValidUnit(opts.unit); err != nil {
		return nil, err
	}
	conn, err := connectToAgent(ctx)
	if err != nil {
		return nil, err
	}
	store, err := resolveCalibrationStore(ctx, conn)
	if err != nil {
		conn.Close()
		return nil, err
	}
	unit, err := store.Load(ctx, opts.unit)
	if err != nil {
		conn.Close()
		return nil, err
	}
	kind := opts.profileKind
	if kind == "" {
		kind = unit.ProfileKind
	}
	if kind == "" {
		conn.Close()
		return nil, fmt.Errorf("no robot profile for unit %q: pass --profile with one of %s "+
			"(`wendy device robot calibrate profiles` describes them). "+
			"Recording the profile on the device itself is `wendy device robot configure`, which is WDY-3132 Part 1",
			opts.unit, strings.Join(robotcal.ProfileKinds(), ", "))
	}
	profile, err := robotcal.LoadProfile(kind)
	if err != nil {
		conn.Close()
		return nil, err
	}
	return &robotSession{conn: conn, store: store, profile: profile, unit: unit}, nil
}

// resolveCalibrationStore returns the store for the connected device.
//
// Either way the record lands on the robot, which is the only property that
// matters here: a calibration is a fact about the machine, and one written into
// this laptop's home directory would be lost to the next person who connected,
// silently.
//
// On-device — the CLI running inside an admin-entitled container on the robot,
// with WENDY_AGENT_SOCKET set — the store is a file it can open directly, and
// that path is left exactly as it was. There is no reason to make a local write
// take a network hop, and it keeps the wizard working on a device whose agent
// predates RobotService.
//
// Otherwise the store is reached over the agent's RobotService, through the
// connection connectToAgent already resolved — so it inherits the cloud tunnel
// and the device pin for free, and `wendy cloud device robot calibrate` works
// without a second code path. The agent serves that RPC from the same
// robotcal.FileStore the on-device branch opens, so the two branches cannot
// disagree about what a record means.
func resolveCalibrationStore(ctx context.Context, conn *grpcclient.AgentConnection) (robotcal.Store, error) {
	if os.Getenv("WENDY_AGENT_SOCKET") != "" {
		return robotcal.NewFileStore(robotcal.DefaultRoot), nil
	}
	if conn == nil || conn.Conn == nil {
		return nil, errors.New("no connection to the device, so there is nowhere to put a calibration: " +
			"a calibration is a fact about the robot and does not belong on this laptop")
	}
	return robotcalclient.Open(ctx, conn.Conn, conn.Host)
}

func calibrateDeps(cmd *cobra.Command, s *robotSession, opts *calibrateOptions) robotwizard.Deps {
	return robotwizard.Deps{
		Profile:         s.profile,
		Store:           s.store,
		Unit:            opts.unit,
		Prompt:          robotwizard.NewTerminalPrompter(cmd.InOrStdin(), cmd.OutOrStdout()),
		OpenJointSource: openJointSource(s.conn.Conn),
		Interactive:     !jsonOutput && isInteractiveTerminal(),
		// WDY-3128 has not landed: appconfig enumerates no `motion` entitlement
		// and oci/entitlements.go has nothing to apply for one. A CLI-side gate
		// with no agent-side enforcement is a gate any other gRPC client walks
		// past, so powered-motion procedures refuse instead of pretending.
		MotionEntitlementAvailable: false,
	}
}

func runRobotCalibrate(cmd *cobra.Command, opts *calibrateOptions, id string) error {
	ctx := cmd.Context()
	session, err := openRobotSession(ctx, opts)
	if err != nil {
		return err
	}
	defer session.Close()

	if opts.stableID != "" {
		if err := session.store.SetStableID(ctx, opts.unit, opts.stableID); err != nil {
			return err
		}
	}

	deps := calibrateDeps(cmd, session, opts)
	statuses, _, err := robotwizard.Plan(ctx, deps)
	if err != nil {
		return err
	}

	if id == "" {
		if !deps.Interactive {
			return fmt.Errorf("choosing a calibration needs an interactive terminal; " +
				"name one (`wendy device robot calibrate <id>`) or run `calibrate status` to see them")
		}
		printCalibrationTable(cmd.OutOrStdout(), session.profile, statuses)
		id, err = pickCalibration(deps.Prompt, statuses)
		if err != nil {
			return err
		}
	}

	rec, err := robotwizard.Run(ctx, deps, id)
	if err != nil {
		return err
	}
	if !rec.Qualified {
		// Refuse rather than warn: a calibration that did not qualify must not
		// look like a completed one, and the exit code is what a script reads.
		return fmt.Errorf("%q did not qualify (%s); it is recorded so the attempt is visible, "+
			"but nothing downstream should trust it", rec.ID, rec.Verdict())
	}
	return nil
}

// pickCalibration offers the procedures that can actually be run, defaulting to
// the first one that has never produced a qualified result.
func pickCalibration(prompt robotwizard.Prompter, statuses []robotcal.Status) (string, error) {
	var runnable []robotcal.Status
	for _, st := range statuses {
		if st.Runnable {
			runnable = append(runnable, st)
		}
	}
	if len(runnable) == 0 {
		return "", errors.New("none of the calibrations this robot needs can be run by this build; " +
			"`wendy device robot calibrate status` says why for each")
	}
	options := make([]string, 0, len(runnable))
	for _, st := range runnable {
		options = append(options, fmt.Sprintf("%s  (%s, %s) — %s", st.ID, st.Method, st.Class, st.Verdict))
	}
	idx, err := prompt.Choose("Which calibration?", options)
	if err != nil {
		return "", err
	}
	return runnable[idx].ID, nil
}

func runRobotCalibrateStatus(cmd *cobra.Command, opts *calibrateOptions) error {
	ctx := cmd.Context()
	session, err := openRobotSession(ctx, opts)
	if err != nil {
		return err
	}
	defer session.Close()

	statuses, _, err := robotwizard.Plan(ctx, calibrateDeps(cmd, session, opts))
	if err != nil {
		return err
	}
	if jsonOutput {
		return json.NewEncoder(cmd.OutOrStdout()).Encode(statuses)
	}
	printCalibrationTable(cmd.OutOrStdout(), session.profile, statuses)
	return nil
}

func printCalibrationTable(out io.Writer, profile *robotcal.Profile, statuses []robotcal.Status) {
	if len(statuses) == 0 {
		fmt.Fprintf(out, "Profile %q declares no calibrations.\n", profile.Kind)
		return
	}
	fmt.Fprintf(out, "\n%s needs %d calibration(s):\n\n", profile.Kind, len(statuses))
	headers := []string{"", "ID", "Method", "Class", "Verdict", "Residual", "Budget", "Measured"}
	rows := make([][]string, 0, len(statuses))
	for _, st := range statuses {
		mark, residual, measured := "✗", "-", "-"
		if st.Record != nil {
			if st.Record.Qualified && st.Verdict == robotcal.VerdictQualified {
				mark = "✓"
			}
			if st.Record.Residual != nil {
				residual = st.Record.Residual.String()
			} else {
				// Rule 3, visible at the point it matters: a skipped step is
				// not measured, and must never read as a zero.
				residual = "not measured"
			}
			measured = st.Record.MeasuredAt.Format("2006-01-02 15:04")
		}
		budget := "-"
		if st.Record != nil {
			budget = st.Record.Budget.String()
		}
		rows = append(rows, []string{mark, st.ID, st.Method, st.Class, string(st.Verdict), residual, budget, measured})
	}
	fmt.Fprint(out, tui.RenderTable(headers, rows))
	for _, st := range statuses {
		if st.Reason != "" {
			fmt.Fprintf(out, "\n  %s: %s\n", st.ID, st.Reason)
		}
	}
}

func runRobotCalibrateClear(cmd *cobra.Command, opts *calibrateOptions, id string) error {
	ctx := cmd.Context()
	session, err := openRobotSession(ctx, opts)
	if err != nil {
		return err
	}
	defer session.Close()

	if _, err := session.profile.Procedure(id); err != nil {
		return err
	}
	if err := session.store.ClearRecord(ctx, opts.unit, id); err != nil {
		return err
	}
	fmt.Fprintf(cmd.OutOrStdout(), "Cleared %q on unit %q. It now reads as never run, which is the "+
		"honest state after a repair.\n", id, opts.unit)
	return nil
}
