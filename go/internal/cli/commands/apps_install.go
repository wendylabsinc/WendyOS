package commands

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
)

// defaultAppStoreAPIBase is the Wendy Cloud API that resolves AppStore app ids
// to OCI image references. This is the deployed wendy-cloud-services Cloud Run
// service (the same host as defaultCloudGRPC in auth.go); cloud.wendy.dev is the
// web dashboard, not the API. The api.wendy.dev domain mapping will alias this
// service once its DNS is live. Override with --api or the WENDY_APPSTORE_API
// env var.
const defaultAppStoreAPIBase = "https://wendy-cloud-services-114319063177.us-central1.run.app"

// appImageResolution is the JSON returned by GET /v1/apps/{app_id}/image.
type appImageResolution struct {
	AppID      string `json:"app_id"`
	Source     string `json:"source"`
	Repository string `json:"repository"`
	Tag        string `json:"tag"`
	Reference  string `json:"reference"`
}

// resolveAppStoreAPIBase picks the AppStore API base, preferring the flag, then
// the WENDY_APPSTORE_API env var, then the built-in default. The returned value
// has no trailing slash.
func resolveAppStoreAPIBase(flagVal string) string {
	base := flagVal
	if base == "" {
		base = os.Getenv("WENDY_APPSTORE_API")
	}
	if base == "" {
		base = defaultAppStoreAPIBase
	}
	return strings.TrimRight(base, "/")
}

// resolveAppImage asks the AppStore API for the OCI image reference of an app id.
func resolveAppImage(ctx context.Context, base, appID string) (appImageResolution, error) {
	endpoint := fmt.Sprintf("%s/v1/apps/%s/image", base, url.PathEscape(appID))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return appImageResolution{}, err
	}
	req.Header.Set("Accept", "application/json")

	client := &http.Client{Timeout: 15 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return appImageResolution{}, fmt.Errorf("contacting the AppStore: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotFound {
		return appImageResolution{}, fmt.Errorf("app %q is not in the AppStore", appID)
	}
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return appImageResolution{}, fmt.Errorf("AppStore returned status %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}

	var out appImageResolution
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return appImageResolution{}, fmt.Errorf("parsing AppStore response: %w", err)
	}
	if out.Reference == "" {
		return appImageResolution{}, fmt.Errorf("AppStore returned no image reference for %q", appID)
	}
	return out, nil
}

func newAppsInstallCmd() *cobra.Command {
	var apiBase string
	var noStart bool

	cmd := &cobra.Command{
		Use:   "install <app-id>",
		Short: "Install an app from the Wendy AppStore",
		Long: "Resolves an AppStore app id to a container image (Docker Hub or the Wendy " +
			"registry) and deploys it to the target device. See https://appstore.wendy.dev.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			appID := args[0]

			base := resolveAppStoreAPIBase(apiBase)
			res, err := resolveAppImage(ctx, base, appID)
			if err != nil {
				return err
			}
			cliLogln("Resolved %s to %s", appID, res.Reference)

			target, err := resolveTarget(ctx)
			if err != nil {
				return err
			}
			defer target.Close()

			if target.Agent == nil {
				return fmt.Errorf("selected device does not support installing apps")
			}

			req := &agentpb.CreateContainerRequest{
				ImageName:     res.Reference,
				AppName:       appID,
				RestartPolicy: &agentpb.RestartPolicy{Mode: agentpb.RestartPolicyMode_UNLESS_STOPPED},
			}
			if err := createContainerWithProgress(ctx, target.Agent.ContainerService, req); err != nil {
				return fmt.Errorf("installing %s: %w", appID, err)
			}

			if noStart {
				cliSuccess("Installed %s (not started).", appID)
				return nil
			}

			if _, err := target.Agent.ContainerService.StartContainer(ctx, &agentpb.StartContainerRequest{
				AppName:       appID,
				RestartPolicy: &agentpb.RestartPolicy{Mode: agentpb.RestartPolicyMode_UNLESS_STOPPED},
			}); err != nil {
				return fmt.Errorf("starting %s: %w", appID, err)
			}
			cliSuccess("Installed and started %s.", appID)
			return nil
		},
	}

	cmd.Flags().StringVar(&apiBase, "api", "", "Wendy AppStore API base URL (default: $WENDY_APPSTORE_API or "+defaultAppStoreAPIBase+")")
	cmd.Flags().BoolVar(&noStart, "no-start", false, "Create the app container but do not start it")
	return cmd
}
