package commands

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"github.com/wendylabsinc/wendy/go/internal/cli/tui"
	agentpbv2 "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
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
the foreground; after it exits, the device stops the model a minute later
unless another client is watching.`,
	}
	cmd.AddCommand(newDeviceModelCatalogCmd(), newDeviceModelListCmd(), newDeviceModelStopCmd())
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
