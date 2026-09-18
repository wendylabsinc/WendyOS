package commands

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"time"

	"github.com/spf13/cobra"

	"github.com/wendylabsinc/wendy/go/internal/robotprobe"
	"github.com/wendylabsinc/wendy/go/internal/rtps"
	"github.com/wendylabsinc/wendy/go/internal/shared/robotinspect"
	"github.com/wendylabsinc/wendy/go/internal/shared/rosmsg"
)

// newDeviceRobotCmd groups the robot-level commands. Hidden for now: the visible CLI
// surface is being reworked, and inspection reads a robot over DDS from this host rather
// than through the agent, so it is a prototype rather than the shipped shape.
func newDeviceRobotCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:    "robot",
		Short:  "Inspect what a robot is, without commanding it",
		Hidden: true,
	}
	cmd.AddCommand(newDeviceRobotInspectCmd())
	return cmd
}

func newDeviceRobotInspectCmd() *cobra.Command {
	var (
		domain    int
		iface     string
		settle    time.Duration
		window    time.Duration
		label     string
		vendorKin string
	)

	cmd := &cobra.Command{
		Use:   "inspect",
		Short: "Read a robot's declared and measured properties, commanding nothing",
		Long: "Join the robot's ROS 2 graph read-only and report what it declares about " +
			"itself beside what it was observed doing, flagging where the two differ.\n\n" +
			"Only probes that cannot actuate are ever run, so this is safe on a robot " +
			"unboxed ten minutes ago and safe to repeat.",
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runRobotInspect(cmd.Context(), robotInspectOptions{
				domain:     domain,
				iface:      iface,
				settle:     settle,
				window:     window,
				label:      label,
				vendorKind: vendorKin,
				out:        cmd.OutOrStdout(),
			})
		},
	}

	cmd.Flags().IntVar(&domain, "domain", 0, "ROS_DOMAIN_ID / DDS domain to join")
	cmd.Flags().StringVar(&iface, "interface", "", "Network interface to bind discovery to (default: an eligible wired interface)")
	cmd.Flags().DurationVar(&settle, "settle", 8*time.Second, "How long to let DDS discovery run before reading")
	cmd.Flags().DurationVar(&window, "duration", 5*time.Second, "Sampling window for measured values")
	cmd.Flags().StringVar(&label, "device", "", "Name to record the inspection against")
	cmd.Flags().StringVar(&vendorKin, "kind", "", "Robot kind to record, such as unitree-g1")
	return cmd
}

type robotInspectOptions struct {
	domain     int
	iface      string
	settle     time.Duration
	window     time.Duration
	label      string
	vendorKind string
	out        io.Writer
}

// robotTopicSource is what an inspection needs from a transport: the topics it can see,
// and the ability to sample them. Taking it as an interface keeps the probe selection and
// the output path testable without a DDS domain.
type robotTopicSource interface {
	robotprobe.TopicReader
	TopicsOfType(typeName string) []string
}

// runRobotInspect joins the graph, runs the probes whose transports are present, and
// prints the document. It never writes to the robot: the registry only admits passive
// probes, so there is no path from here to an actuator.
func runRobotInspect(ctx context.Context, opts robotInspectOptions) error {
	participant, err := rtps.NewParticipant(rtps.Config{
		DomainID:  opts.domain,
		Interface: opts.iface,
	})
	if err != nil {
		return fmt.Errorf("joining DDS domain %d: %w", opts.domain, err)
	}
	defer participant.Close()

	runCtx, stop := context.WithCancel(ctx)
	defer stop()
	go participant.Run(runCtx)

	// Discovery is announcement-driven, so the graph appears over a second or two
	// rather than on request.
	select {
	case <-time.After(opts.settle):
	case <-runCtx.Done():
		return runCtx.Err()
	}

	reader := robotprobe.NewDDSReader(robotprobe.NewParticipantLease(participant, runCtx.Done()))
	doc, err := probeRobot(runCtx, reader, opts)
	if err != nil {
		return err
	}
	return writeRobotDocument(opts.out, doc, opts)
}

// probeRobot registers a probe for every transport the source can actually serve, so a
// robot that publishes no cameras simply has no camera rows rather than a wall of
// failures.
func probeRobot(ctx context.Context, source robotTopicSource, opts robotInspectOptions) (robotinspect.Document, error) {
	registry := robotinspect.NewRegistry()
	var want []string

	// What the cameras claim about themselves.
	if topics := source.TopicsOfType(rosmsg.TypeCameraInfo); len(topics) > 0 {
		camera := robotprobe.CameraInfo{Topics: topics}
		if err := registry.Register(camera); err != nil {
			return robotinspect.Document{}, err
		}
		want = append(want, camera.Provides()...)
	}

	// What they actually deliver. Both probes answer camera.<stream>.resolution.width,
	// and the inspection folds them onto one property — which is where a calibration
	// taken at one resolution and a stream running at another becomes a finding
	// instead of two unrelated rows.
	if topics := source.TopicsOfType(rosmsg.TypeImage); len(topics) > 0 {
		stream := robotprobe.CameraStream{Topics: topics, Window: opts.window}
		if err := registry.Register(stream); err != nil {
			return robotinspect.Document{}, err
		}
		want = append(want, stream.Provides()...)
	}

	env := robotinspect.NewEnv().Offer(robotinspect.RequirementDDSDomain, source)
	return robotinspect.Inspect(ctx, registry, env, robotinspect.Target{
		Device:     opts.label,
		VendorKind: opts.vendorKind,
		Want:       want,
	}), nil
}

func writeRobotDocument(out io.Writer, doc robotinspect.Document, opts robotInspectOptions) error {
	if jsonOutput {
		encoded, err := json.MarshalIndent(doc, "", "  ")
		if err != nil {
			return err
		}
		_, err = fmt.Fprintln(out, string(encoded))
		return err
	}

	if len(doc.Properties) == 0 {
		// Saying the graph was empty is the answer. Printing an empty report would
		// read as "this robot has nothing", which is a different claim.
		_, err := fmt.Fprintf(out,
			"No ROS 2 writers found on domain %d after %s.\nNothing was commanded.\n",
			opts.domain, opts.settle)
		return err
	}

	_, err := fmt.Fprint(out, robotinspect.Render(doc))
	return err
}
