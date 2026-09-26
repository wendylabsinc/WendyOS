package commands

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"github.com/wendylabsinc/wendy/go/internal/cli/tui"
	agentpbv2 "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// newDeviceModelCmd is `wendy device model`: run catalog models on a device.
func newDeviceModelCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "model",
		Short: "Run AI models from the catalog on a device's cameras",
		Long: `Run detectors and other catalog models on a device's cameras.

A model runs while something watches it. 'wendy device model run' watches in
the foreground, and Ctrl+C stops the model unless another client is watching
it. A client that disappears without detaching keeps the model running for a
minute, so it can reconnect.`,
	}
	cmd.AddCommand(newDeviceModelCatalogCmd(), newDeviceModelRunCmd(), newDeviceModelListCmd(), newDeviceModelStopCmd())
	return cmd
}

func withModelClient(ctx context.Context, fn func(agentpbv2.WendyModelServiceClient) error) error {
	conn, err := connectToAgent(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()
	return fn(conn.ModelService)
}

// modelServiceErr explains an agent that predates the model service.
func modelServiceErr(err error) error {
	if status.Code(err) == codes.Unimplemented {
		return errors.New("this device's agent does not run models yet; update it with `wendy device update`")
	}
	return err
}

func newDeviceModelCatalogCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "catalog",
		Short: "List the models and cameras this device can use",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return withModelClient(cmd.Context(), func(client agentpbv2.WendyModelServiceClient) error {
				resp, err := client.ListCatalog(cmd.Context(), &agentpbv2.ListModelCatalogRequest{})
				if err != nil {
					return modelServiceErr(err)
				}
				if jsonOutput {
					return encodeProtoJSON(cmd.OutOrStdout(), resp)
				}
				fmt.Fprint(cmd.OutOrStdout(), renderModelCatalog(resp))
				return nil
			})
		},
	}
}

func renderModelCatalog(resp *agentpbv2.ListModelCatalogResponse) string {
	var b strings.Builder
	if len(resp.GetModels()) == 0 {
		b.WriteString("No models in this agent's catalog yet.\n")
	} else {
		rows := make([][]string, 0, len(resp.GetModels()))
		for _, m := range resp.GetModels() {
			engine, firstStart := "—", m.GetUnavailableReason()
			if v := m.GetVariant(); v != nil {
				engine, firstStart = v.GetEngine(), modelFirstStartCost(v)
			}
			rows = append(rows, []string{m.GetId(), engine, fmt.Sprintf("%d classes", len(m.GetLabels())), firstStart})
		}
		b.WriteString(tui.RenderTable([]string{"Model", "Engine", "Detects", "First start"}, rows))
	}
	if len(resp.GetCameras()) == 0 {
		b.WriteString("No cameras a model can watch.\n")
	} else {
		rows := make([][]string, 0, len(resp.GetCameras()))
		for _, c := range resp.GetCameras() {
			rows = append(rows, []string{c.GetSourceId(), c.GetName()})
		}
		b.WriteString(tui.RenderTable([]string{"Camera", "Name"}, rows))
	}
	fmt.Fprintf(&b, "%d of %d model slots in use\n", resp.GetRunning(), resp.GetMaxRunning())
	return b.String()
}

// modelFirstStartCost says what a first start still has to do on this device.
func modelFirstStartCost(v *agentpbv2.CatalogVariant) string {
	var parts []string
	if !v.GetImageCached() {
		parts = append(parts, "downloads its runtime image")
	}
	if n := v.GetDownloadBytes(); n > 0 {
		parts = append(parts, "downloads "+modelBytes(n)+" of model")
	}
	if v.GetNeedsEngineBuild() {
		parts = append(parts, "builds a TensorRT engine (minutes)")
	}
	if len(parts) == 0 {
		return "ready to start"
	}
	return strings.Join(parts, ", ")
}

func modelBytes(n uint64) string {
	switch {
	case n >= 1<<30:
		return fmt.Sprintf("%.1f GB", float64(n)/(1<<30))
	case n >= 1<<20:
		return fmt.Sprintf("%.0f MB", float64(n)/(1<<20))
	default:
		return fmt.Sprintf("%d KB", (n+1023)/1024)
	}
}

func newDeviceModelListCmd() *cobra.Command {
	return &cobra.Command{
		Use:     "list",
		Aliases: []string{"ls"},
		Short:   "List the models running on a device",
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return withModelClient(cmd.Context(), func(client agentpbv2.WendyModelServiceClient) error {
				resp, err := client.ListModels(cmd.Context(), &agentpbv2.ListModelsRequest{})
				if err != nil {
					return modelServiceErr(err)
				}
				if jsonOutput {
					return encodeProtoJSON(cmd.OutOrStdout(), resp)
				}
				if len(resp.GetInstances()) == 0 {
					fmt.Fprintln(cmd.OutOrStdout(), "No models running.")
					return nil
				}
				rows := make([][]string, 0, len(resp.GetInstances()))
				for _, i := range resp.GetInstances() {
					rows = append(rows, []string{i.GetInstanceId(), i.GetModelId(), i.GetCameraSourceId(),
						modelStateText(i), fmt.Sprintf("%d", i.GetWatchers()), shortModelDigest(i.GetFileSha256())})
				}
				fmt.Fprint(cmd.OutOrStdout(), tui.RenderTable([]string{"Instance", "Model", "Camera", "State", "Watchers", "File"}, rows))
				return nil
			})
		},
	}
}

func newDeviceModelStopCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "stop <instance>",
		Short: "Stop a running model, whoever is watching it",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return withModelClient(cmd.Context(), func(client agentpbv2.WendyModelServiceClient) error {
				ctx, cancel := context.WithTimeout(cmd.Context(), time.Minute)
				defer cancel()
				resp, err := client.StopModel(ctx, &agentpbv2.StopModelRequest{InstanceId: args[0]})
				if err != nil {
					return modelServiceErr(err)
				}
				if jsonOutput {
					return encodeProtoJSON(cmd.OutOrStdout(), resp)
				}
				fmt.Fprintf(cmd.OutOrStdout(), "Stopped %s (%s).\n", args[0], modelStateLabel(resp.GetInstance()))
				return nil
			})
		},
	}
}

// modelStateLabel is the state and what it is doing, e.g.
// "preparing: pulling host image".
func modelStateLabel(i *agentpbv2.ModelInstance) string {
	state := strings.ToLower(strings.TrimPrefix(i.GetState().String(), "MODEL_STATE_"))
	if d := i.GetStateDetail(); d != "" {
		return state + ": " + d
	}
	return state
}

// modelStateText adds the frame rate to a ready instance's label.
func modelStateText(i *agentpbv2.ModelInstance) string {
	if i.GetState() == agentpbv2.ModelState_MODEL_STATE_READY && i.GetStats().GetProcessedFps() > 0 {
		return fmt.Sprintf("ready (%.1f fps)", i.GetStats().GetProcessedFps())
	}
	return modelStateLabel(i)
}

func shortModelDigest(sha string) string {
	if len(sha) > 12 {
		return sha[:12] + "…"
	}
	return sha
}

func newDeviceModelRunCmd() *cobra.Command {
	var opts modelRunOptions
	cmd := &cobra.Command{
		Use:   "run <model>",
		Short: "Start a model on a camera and print what it sees until Ctrl+C",
		Example: `  wendy device model run coco-detector --camera v4l2:/dev/video0 --watch person
  wendy device model run coco-detector --camera v4l2:/dev/video0 --json`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if opts.Camera == "" {
				return errors.New("choose a camera with --camera; `wendy device model catalog` lists them")
			}
			opts.Model = args[0]
			return withModelClient(cmd.Context(), func(client agentpbv2.WendyModelServiceClient) error {
				return runModelWatch(cmd.Context(), client, cmd.OutOrStdout(), opts)
			})
		},
	}
	cmd.Flags().StringVar(&opts.Camera, "camera", "", "Camera source to watch, e.g. v4l2:/dev/video0")
	cmd.Flags().StringSliceVar(&opts.Classes, "watch", nil, "Only report these classes (default: all)")
	cmd.Flags().Float32Var(&opts.MinConfidence, "min-confidence", 0, "Only report detections at least this confident (0-1)")
	cmd.Flags().StringVar(&opts.Label, "label", "", "A name for this watch, shown to other clients")
	return cmd
}

type modelRunOptions struct {
	Model, Camera, Label string
	Classes              []string
	MinConfidence        float32
}

const (
	// modelDetachTimeout bounds the StopModel that detaches a run's watch.
	modelDetachTimeout = 10 * time.Second
	// modelWatchConfirmWait is how long Ctrl+C waits for the device to
	// confirm a watch it has not confirmed yet, so the watch can be detached.
	modelWatchConfirmWait = 2 * time.Second
)

// runModelWatch starts the model and prints what it sees until Ctrl+C or the
// model stops. It detaches its watch on the way out, so the device stops the
// model at once unless another client is watching it.
func runModelWatch(ctx context.Context, client agentpbv2.WendyModelServiceClient, out io.Writer, opts modelRunOptions) error {
	started, err := client.StartModel(ctx, &agentpbv2.StartModelRequest{ModelId: opts.Model, CameraSourceId: opts.Camera})
	if err != nil {
		return modelServiceErr(err)
	}
	id := started.GetInstance().GetInstanceId()
	// Ctrl+C must not break the stream: the device reads a broken stream as a
	// client that went away, detaches the watch itself and keeps the model for
	// a minute. The watch is detached first, and the stream closed after.
	streamCtx, closeStream := context.WithCancel(context.WithoutCancel(ctx))
	defer closeStream()
	stream, err := client.WatchModel(streamCtx, &agentpbv2.WatchModelRequest{
		InstanceId: id, Label: opts.Label, Classes: opts.Classes, MinConfidence: opts.MinConfidence})
	if err != nil {
		return modelServiceErr(err)
	}
	var watchID, lastStatusLine string
	var last *agentpbv2.ModelInstance
	detached := false
	detach := func() {
		if watchID == "" || detached {
			return
		}
		detached = true
		detachCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), modelDetachTimeout)
		defer cancel()
		_, _ = client.StopModel(detachCtx, &agentpbv2.StopModelRequest{InstanceId: id, WatchId: watchID})
	}
	defer detach()
	if !jsonOutput {
		cliLogln("Watching %s on %s (instance %s). Press Ctrl+C to stop.", opts.Model, opts.Camera, id)
	}
	received := receiveModelWatch(streamCtx, stream)
	for {
		var r modelWatchReceipt
		select {
		case r = <-received:
		case <-ctx.Done():
			// Ctrl+C. A watch the device has not confirmed yet may be
			// confirmed in a moment; without its id it cannot be detached,
			// and the device's lost-client grace stops the model instead.
			if watchID == "" {
				watchID = awaitWatchConfirmed(received, modelWatchConfirmWait)
			}
			detach()
			return nil
		}
		if r.err != nil {
			if r.err == io.EOF {
				if last.GetState() == agentpbv2.ModelState_MODEL_STATE_FAILED {
					return fmt.Errorf("the model failed: %s", last.GetStateDetail())
				}
				return nil
			}
			return fmt.Errorf("watching %s: %w", id, modelServiceErr(r.err))
		}
		msg := r.msg
		switch {
		case msg.GetStarted() != nil:
			watchID, last = msg.GetStarted().GetWatchId(), msg.GetStarted().GetInstance()
		case msg.GetStatus() != nil:
			last = msg.GetStatus()
		}
		if jsonOutput {
			if err := encodeProtoJSON(out, msg); err != nil {
				return err
			}
			continue
		}
		line := modelWatchLine(msg)
		if msg.GetStarted() != nil || msg.GetStatus() != nil {
			if line == lastStatusLine {
				continue // heartbeats repeat the state
			}
			lastStatusLine = line
		}
		if line != "" {
			fmt.Fprintln(out, line)
		}
	}
}

// modelWatchReceipt is one result of reading a watch stream.
type modelWatchReceipt struct {
	msg *agentpbv2.ModelWatchMessage
	err error
}

// receiveModelWatch reads stream on its own goroutine, so a run can act on
// Ctrl+C while a read blocks. It stops after the stream's first error, or
// once ctx, the stream's context, ends.
func receiveModelWatch(ctx context.Context, stream grpc.ServerStreamingClient[agentpbv2.ModelWatchMessage]) <-chan modelWatchReceipt {
	received := make(chan modelWatchReceipt)
	go func() {
		for {
			msg, err := stream.Recv()
			select {
			case received <- modelWatchReceipt{msg: msg, err: err}:
			case <-ctx.Done():
				return
			}
			if err != nil {
				return
			}
		}
	}()
	return received
}

// awaitWatchConfirmed waits up to wait for the device's WatchStarted, which
// always comes first, and returns the watch's id, or "" if none arrives.
func awaitWatchConfirmed(received <-chan modelWatchReceipt, wait time.Duration) string {
	timer := time.NewTimer(wait)
	defer timer.Stop()
	for {
		select {
		case r := <-received:
			if r.err != nil {
				return ""
			}
			if s := r.msg.GetStarted(); s != nil {
				return s.GetWatchId()
			}
		case <-timer.C:
			return ""
		}
	}
}

// modelWatchLine renders one watch message for a person.
func modelWatchLine(msg *agentpbv2.ModelWatchMessage) string {
	switch {
	case msg.GetStarted() != nil:
		return "state: " + modelStateLabel(msg.GetStarted().GetInstance())
	case msg.GetStatus() != nil:
		return "state: " + modelStateLabel(msg.GetStatus())
	case msg.GetEvent() != nil:
		e := msg.GetEvent()
		at := time.Unix(0, e.GetTimeUnixNanos()).Format("15:04:05")
		return fmt.Sprintf("%s  %-12s %.2f %s (track %d)", at, e.GetClassName(), e.GetConfidence(), e.GetType(), e.GetTrackId())
	case msg.GetGap() != nil:
		return fmt.Sprintf("missed events %d-%d", msg.GetGap().GetFirstMissing(), msg.GetGap().GetLastMissing())
	}
	return ""
}
