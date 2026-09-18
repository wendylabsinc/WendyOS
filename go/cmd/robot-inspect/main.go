// Command robot-inspect runs a read-only robot inspection from inside the robot's own
// network namespace and prints the document.
//
// It exists because DDS discovery is multicast: a participant on a laptop cannot see a
// robot's ROS 2 graph across a routed link or a cloud tunnel, however reachable the
// device itself is. This binary is static and has no cgo, so it cross-compiles for the
// robot's architecture and ships in a scratch container with host networking.
//
// It commands nothing. Only passive probes are ever planned, which is a property of the
// registry rather than a promise made here.
package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/robotprobe"
	"github.com/wendylabsinc/wendy/go/internal/rtps"
	"github.com/wendylabsinc/wendy/go/internal/shared/robotinspect"
	"github.com/wendylabsinc/wendy/go/internal/shared/rosmsg"
)

func main() {
	domain := flag.Int("domain", 0, "ROS_DOMAIN_ID / DDS domain to join")
	iface := flag.String("interface", "", "network interface to bind discovery to")
	settle := flag.Duration("settle", 10*time.Second, "how long to let DDS discovery run before reading")
	window := flag.Duration("duration", 5*time.Second, "sampling window for measured values")
	device := flag.String("device", "", "name to record the inspection against")
	kind := flag.String("kind", "", "robot kind to record, such as unitree-g1")
	asJSON := flag.Bool("json", false, "emit the document as JSON")
	capture := flag.String("capture", "", "instead of inspecting, print one raw payload from this topic as base64, for use as a test fixture")
	canonical := flag.Bool("canonical", false, "with -json, omit wall-clock fields so two passes diff on substance")
	flag.Parse()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if *capture != "" {
		if err := captureOne(ctx, *capture, options{domain: *domain, iface: *iface, settle: *settle}); err != nil {
			fmt.Fprintf(os.Stderr, "robot-inspect: %v\n", err)
			os.Exit(1)
		}
		return
	}

	if err := run(ctx, options{
		domain: *domain, iface: *iface, settle: *settle, window: *window,
		device: *device, kind: *kind, asJSON: *asJSON, canonical: *canonical,
	}); err != nil {
		fmt.Fprintf(os.Stderr, "robot-inspect: %v\n", err)
		os.Exit(1)
	}
}

type options struct {
	domain    int
	iface     string
	settle    time.Duration
	window    time.Duration
	device    string
	kind      string
	asJSON    bool
	canonical bool
}

func run(ctx context.Context, opts options) error {
	participant, err := rtps.NewParticipant(rtps.Config{
		DomainID:  opts.domain,
		Interface: opts.iface,
		Logf: func(format string, args ...any) {
			// Discovery is silent by nature, so progress goes to stderr and leaves
			// stdout carrying only the document.
			fmt.Fprintf(os.Stderr, "rtps: "+format+"\n", args...)
		},
	})
	if err != nil {
		return fmt.Errorf("joining DDS domain %d: %w", opts.domain, err)
	}
	defer participant.Close()

	runCtx, stop := context.WithCancel(ctx)
	defer stop()
	go participant.Run(runCtx)

	select {
	case <-time.After(opts.settle):
	case <-runCtx.Done():
		return runCtx.Err()
	}

	reader := robotprobe.NewDDSReader(robotprobe.NewParticipantLease(participant, runCtx.Done()))
	registry := robotinspect.NewRegistry()
	var want []string

	if topics := reader.TopicsOfType(rosmsg.TypeCameraInfo); len(topics) > 0 {
		probe := robotprobe.CameraInfo{Topics: topics}
		if err := registry.Register(probe); err != nil {
			return err
		}
		want = append(want, probe.Provides()...)
	}
	if topics := reader.TopicsOfType(rosmsg.TypeImage); len(topics) > 0 {
		probe := robotprobe.CameraStream{Topics: topics, Window: opts.window}
		if err := registry.Register(probe); err != nil {
			return err
		}
		want = append(want, probe.Provides()...)
	}

	// The body. Registered unconditionally: the probe reports an absent topic as a
	// finding, which is the honest answer for a robot that is not a Unitree humanoid,
	// and is different from never having looked.
	// The body, its battery and its hands. All three are registered unconditionally:
	// each reports an absent topic as a finding, which is the honest answer on a robot
	// that is not a Unitree humanoid and is different from never having looked.
	for _, probe := range []robotinspect.Probe{
		robotprobe.Joints{}, robotprobe.RobotBattery{}, robotprobe.Hands{},
	} {
		if err := registry.Register(probe); err != nil {
			return err
		}
		want = append(want, probe.Provides()...)
	}

	env := robotinspect.NewEnv().Offer(robotinspect.RequirementDDSDomain, reader)
	doc := robotinspect.Inspect(runCtx, registry, env, robotinspect.Target{
		Device: opts.device, VendorKind: opts.kind, Want: want,
	})

	if opts.asJSON {
		encode := func() ([]byte, error) { return json.MarshalIndent(doc, "", "  ") }
		if opts.canonical {
			encode = doc.CanonicalJSON
		}
		encoded, err := encode()
		if err != nil {
			return err
		}
		fmt.Println(string(encoded))
		return nil
	}
	if len(doc.Properties) == 0 {
		fmt.Printf("No ROS 2 writers found on domain %d after %s.\nNothing was commanded.\n",
			opts.domain, opts.settle)
		return nil
	}
	fmt.Print(robotinspect.Render(doc))
	return nil
}

// captureOne prints a single raw payload so it can be saved as a test fixture.
//
// A decoder tested only against a message the same author reconstructed from a spec
// proves the two agree, not that either matches the robot. Pinning it to bytes the robot
// actually sent is what closes that gap, and the agent's own ROS 2 decoders keep captured
// payloads as testdata for the same reason.
func captureOne(ctx context.Context, topic string, opts options) error {
	participant, err := rtps.NewParticipant(rtps.Config{DomainID: opts.domain, Interface: opts.iface})
	if err != nil {
		return fmt.Errorf("joining DDS domain %d: %w", opts.domain, err)
	}
	defer participant.Close()

	runCtx, stop := context.WithCancel(ctx)
	defer stop()
	go participant.Run(runCtx)

	select {
	case <-time.After(opts.settle):
	case <-runCtx.Done():
		return runCtx.Err()
	}

	reader := robotprobe.NewDDSReader(robotprobe.NewParticipantLease(participant, runCtx.Done()))
	payloads, err := reader.Sample(runCtx, topic, "", 5*time.Second, 1)
	if err != nil {
		return fmt.Errorf("sampling %s: %w", topic, err)
	}
	if len(payloads) == 0 {
		return fmt.Errorf("nothing published %s", topic)
	}
	fmt.Printf("CAPTURE %s %d bytes\n%s\n", topic, len(payloads[0]), base64.StdEncoding.EncodeToString(payloads[0]))
	return nil
}
