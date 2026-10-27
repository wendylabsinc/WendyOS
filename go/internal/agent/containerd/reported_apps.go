package containerd

import (
	"context"
	"fmt"

	containerd "github.com/containerd/containerd/v2/client"
	"go.uber.org/zap"

	"github.com/wendylabsinc/wendy/go/internal/agent/services"
	"github.com/wendylabsinc/wendy/go/internal/shared/appconfig"
)

// maxSignal is the highest Linux signal number (SIGRTMAX).
const maxSignal = 64

// ReportApps returns one report per Wendy-managed app, for the cloud
// reported-state stream. Apps whose id cloud would refuse are left out, so one
// legacy container cannot refuse the whole report.
func (c *Client) ReportApps(ctx context.Context) ([]services.AppReport, error) {
	ctx = c.withNamespace(ctx)
	ctrs, err := c.client.Containers(ctx, fmt.Sprintf("labels.%q", labelKeyAppVersion))
	if err != nil {
		return nil, fmt.Errorf("listing containers: %w", err)
	}
	byApp := make(map[string]*services.AppReport)
	var order []string
	for _, ctr := range ctrs {
		// Containers just read the records; this runs every poll.
		info, err := ctr.Info(ctx, containerd.WithoutRefreshedMetadata)
		if err != nil {
			c.logger.Warn("Failed to get container info", zap.String("id", ctr.ID()), zap.Error(err))
			continue
		}
		appID := info.Labels[labelKeyAppID]
		if appconfig.ValidateAppID(appID) != nil {
			continue
		}
		running := false
		if task, err := ctr.Task(ctx, nil); err == nil {
			if st, err := task.Status(ctx); err == nil && st.Status == containerd.Running {
				running = true
			}
		}
		var signal uint32
		if code, _, ok := parseExitLabels(info.Labels); ok && info.Labels[labelKeyStoppedByUser] != "true" && code > 128 && code <= 128+maxSignal {
			// The runtime reports a signal death as 128+signal; a higher code
			// is a plain exit status.
			signal = uint32(code - 128)
		}
		digest, release := info.Labels[labelKeyImageDigest], info.Labels[labelKeyReleaseID]
		if !isCanonicalUUID(release) {
			release = "" // cloud refuses the whole report for a non-UUID release id
		}
		r, seen := byApp[appID]
		if !seen {
			byApp[appID] = &services.AppReport{AppID: appID, Running: running, ExitSignal: signal, ManifestDigest: digest, ReleaseID: release}
			order = append(order, appID)
			continue
		}
		// A multi-service app runs if any service runs. Its services have one
		// release but each its own image: a digest is reported only when they
		// all agree.
		r.Running = r.Running || running
		if r.ExitSignal == 0 {
			r.ExitSignal = signal
		}
		if r.ManifestDigest != digest {
			r.ManifestDigest = ""
		}
		if r.ReleaseID != release {
			r.ReleaseID = ""
		}
	}
	reports := make([]services.AppReport, 0, len(order))
	for _, id := range order {
		reports = append(reports, *byApp[id])
	}
	return reports, nil
}

// ImageDigest returns the manifest digest of a locally held image, named as a
// create request names it.
func (c *Client) ImageDigest(ctx context.Context, imageName string) (string, error) {
	image, err := c.client.GetImage(c.withNamespace(ctx), normalizeImageName(imageName))
	if err != nil {
		return "", err
	}
	return image.Target().Digest.String(), nil
}
