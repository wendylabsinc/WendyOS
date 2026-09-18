package commands

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strconv"
	"strings"
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
		domain     int
		iface      string
		settle     time.Duration
		window     time.Duration
		vendorKin  string
		skipAgent  bool
		canonical  bool
		expectMode string
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
				vendorKind: vendorKin,
				skipAgent:  skipAgent,
				canonical:  canonical,
				expectMode: expectMode,
				out:        cmd.OutOrStdout(),
			})
		},
	}

	cmd.Flags().IntVar(&domain, "domain", 0, "ROS_DOMAIN_ID / DDS domain to join")
	cmd.Flags().StringVar(&iface, "interface", "", "Network interface to bind discovery to (default: an eligible wired interface)")
	cmd.Flags().DurationVar(&settle, "settle", 8*time.Second, "How long to let DDS discovery run before reading")
	cmd.Flags().DurationVar(&window, "duration", 5*time.Second, "Sampling window for measured values")
	cmd.Flags().StringVar(&vendorKin, "kind", "", "Robot kind to record, such as unitree-g1")
	cmd.Flags().BoolVar(&skipAgent, "no-agent", false, "Skip the agent and report only what the robot publishes")
	cmd.Flags().BoolVar(&canonical, "canonical", false, "With --json, omit wall-clock fields so two passes diff on substance")
	cmd.Flags().StringVar(&expectMode, "expect-mode", "", "Capture mode to ask each camera for, as WIDTHxHEIGHT@FPS; the report then compares it against what arrived")
	return cmd
}

type robotInspectOptions struct {
	domain     int
	iface      string
	settle     time.Duration
	window     time.Duration
	label      string
	vendorKind string
	skipAgent  bool
	canonical  bool
	expectMode string
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
	// The agent answers over a cloud tunnel, so the host half of the report works
	// wherever the device is reachable. Failing to reach it is not fatal: a robot may
	// be inspected from its own LAN with no Wendy agent in the picture at all.
	var host robotprobe.HostFactsSource
	if !opts.skipAgent {
		conn, err := connectToAgent(ctx)
		if err != nil {
			cliLogln("Continuing without the agent: %v", err)
		} else {
			defer conn.Close()
			host = newAgentHostFacts(conn)
			// The device name is whatever the agent calls itself, so a document is
			// always traceable to a unit without the caller having to repeat it.
			if facts, factsErr := host.HostFacts(ctx); factsErr == nil && facts != nil {
				opts.label = facts.Hostname
			}
		}
	}

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
	doc, err := probeRobot(runCtx, reader, host, opts)
	if err != nil {
		return err
	}
	return writeRobotDocument(opts.out, doc, opts)
}

// probeRobot registers a probe for every transport the source can actually serve, so a
// robot that publishes no cameras simply has no camera rows rather than a wall of
// failures.
func probeRobot(ctx context.Context, source robotTopicSource, host robotprobe.HostFactsSource, opts robotInspectOptions) (robotinspect.Document, error) {
	registry := robotinspect.NewRegistry()
	env := robotinspect.NewEnv()
	var want []string

	// What the machine says about itself. This needs only the agent, so it works over
	// a cloud tunnel and answers even on a robot with no ROS 2 graph at all.
	if host != nil {
		env.Offer(robotinspect.RequirementHostStats, host)
		probes := []robotinspect.Probe{
			robotprobe.Compute{}, robotprobe.Storage{}, robotprobe.Network{}, robotprobe.HostBattery{},
		}
		// Hardware enumeration and the clock come off the same connection, but only
		// when the caller supplied a source that can answer them.
		if _, ok := host.(robotprobe.HardwareSource); ok {
			probes = append(probes, robotprobe.Hardware{})
		}
		if clock, ok := host.(robotprobe.ClockSource); ok {
			env.Offer(robotinspect.RequirementTimeSync, clock)
			probes = append(probes, robotprobe.Clock{})
		}
		// Cameras come through the agent, which already abstracts USB, CSI, network
		// and ROS 2 alike. This is the general camera path: it answers on a robot
		// with no ROS installed, and it works wherever the device is reachable
		// rather than only on its own network segment.
		if cameras, ok := host.(robotprobe.CameraSource); ok {
			mode, err := parseCameraMode(opts.expectMode)
			if err != nil {
				return robotinspect.Document{}, err
			}
			env.Offer(robotinspect.RequirementCameraTransport, cameras)
			probes = append(probes, robotprobe.Camera{Window: opts.window, Mode: mode})
		}
		for _, probe := range probes {
			if err := registry.Register(probe); err != nil {
				return robotinspect.Document{}, err
			}
			want = append(want, probe.Provides()...)
		}
	}

	if source == nil {
		return robotinspect.Inspect(ctx, registry, env, robotinspect.Target{
			Device: opts.label, VendorKind: opts.vendorKind, Want: want,
		}), nil
	}
	env.Offer(robotinspect.RequirementDDSDomain, source)

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

	return robotinspect.Inspect(ctx, registry, env, robotinspect.Target{
		Device:     opts.label,
		VendorKind: opts.vendorKind,
		Want:       want,
	}), nil
}

// parseCameraMode reads WIDTHxHEIGHT@FPS, with either half optional: "1280x720",
// "@30" and "1280x720@30" are all valid, and an empty string asks for nothing.
func parseCameraMode(spec string) (robotprobe.CameraMode, error) {
	if strings.TrimSpace(spec) == "" {
		return robotprobe.CameraMode{}, nil
	}
	var mode robotprobe.CameraMode
	resolution, rate, hasRate := strings.Cut(spec, "@")
	if hasRate && rate != "" {
		fps, err := strconv.ParseUint(rate, 10, 32)
		if err != nil {
			return mode, fmt.Errorf("--expect-mode: %q is not a frame rate", rate)
		}
		mode.Framerate = uint32(fps)
	}
	if resolution != "" {
		width, height, ok := strings.Cut(strings.ToLower(resolution), "x")
		if !ok {
			return mode, fmt.Errorf("--expect-mode: %q is not WIDTHxHEIGHT", resolution)
		}
		w, err := strconv.ParseUint(width, 10, 32)
		if err != nil {
			return mode, fmt.Errorf("--expect-mode: %q is not a width", width)
		}
		h, err := strconv.ParseUint(height, 10, 32)
		if err != nil {
			return mode, fmt.Errorf("--expect-mode: %q is not a height", height)
		}
		mode.Width, mode.Height = uint32(w), uint32(h)
	}
	return mode, nil
}

func writeRobotDocument(out io.Writer, doc robotinspect.Document, opts robotInspectOptions) error {
	if jsonOutput {
		// The default carries timestamps, which is right for the record of one pass.
		// --canonical drops them so two passes — the same robot next week, or the
		// next unit off the line — differ only where the robots differ.
		encode := func() ([]byte, error) { return json.MarshalIndent(doc, "", "  ") }
		if opts.canonical {
			encode = doc.CanonicalJSON
		}
		encoded, err := encode()
		if err != nil {
			return err
		}
		_, err = fmt.Fprintln(out, string(encoded))
		return err
	}

	if len(doc.Properties) == 0 {
		// Saying nothing was found is the answer. Printing an empty report would read
		// as "this robot has nothing", which is a different claim.
		_, err := fmt.Fprintf(out,
			"No ROS 2 writers found on domain %d after %s.\nNothing was commanded.\n",
			opts.domain, opts.settle)
		return err
	}

	_, err := fmt.Fprint(out, robotinspect.Render(doc))
	return err
}
