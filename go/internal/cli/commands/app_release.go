package commands

import (
	"context"
	"fmt"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/spf13/cobra"
	"google.golang.org/grpc"

	"github.com/wendylabsinc/wendy/go/internal/cli/cloudrequest"
	"github.com/wendylabsinc/wendy/go/internal/shared/appconfig"
	"github.com/wendylabsinc/wendy/go/internal/shared/certs"
	"github.com/wendylabsinc/wendy/go/internal/shared/config"
	agentpb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
	cloudpb "github.com/wendylabsinc/wendy/go/proto/gen/cloudpb/v2"
)

// Release identity (apps spec section 1-2): a release is a tag naming one
// image digest. Both are always given, so there is never an implicit "latest".
// The contract carries one tag per release (AppRelease.name).

var imageDigestPattern = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)

func newAppCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "app",
		Short: "Manage this project's app in Wendy Cloud",
	}
	cmd.AddCommand(newAppReleaseCmd())
	return cmd
}

func newAppReleaseCmd() *cobra.Command {
	var tag, digest, cloudGRPC string
	cmd := &cobra.Command{
		Use:   "release",
		Short: "Record a release of this project's app in Wendy Cloud",
		Long: "Records a release of the app in wendy.json: a tag naming one image digest.\n" +
			"Both --tag and --digest are required; a release is never implicitly 'latest'.\n" +
			"To record a deploy as it happens, use 'wendy run --record-release <tag>' instead.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := validateReleaseTag(tag); err != nil {
				return usageErrorf("%v", err)
			}
			if !imageDigestPattern.MatchString(digest) {
				return usageErrorf("--digest must be sha256:<64 lowercase hex>, got %q", digest)
			}
			appCfg, err := appconfig.LoadFromFile("wendy.json")
			if err != nil {
				return err
			}
			auth, err := pickAuthEntry(cloudGRPC)
			if err != nil {
				return err
			}
			if len(auth.Certificates) == 0 || auth.Certificates[0].TenantUUID() == "" {
				return fmt.Errorf("wendy app release needs an operator session; run 'wendy auth login'")
			}
			return withCloud(cmd.Context(), auth, func(ctx context.Context, conn grpc.ClientConnInterface) error {
				id, err := createAppRelease(ctx, conn, auth, auth.Certificates[0].TenantUUID(), appCfg.AppID, tag)
				if err != nil {
					return err
				}
				if err := setAppReleaseDigest(ctx, conn, auth, id, tag, digest); err != nil {
					return fmt.Errorf("release %s was created without its digest: %w", id, err)
				}
				cliLogln("Recorded release %s of %s: %s = %s", id, appCfg.AppID, tag, digest)
				return nil
			})
		},
	}
	cmd.Flags().StringVar(&tag, "tag", "", "Release tag, e.g. v1.4.0 (required)")
	cmd.Flags().StringVar(&digest, "digest", "", "Image manifest digest, sha256:<hex> (required)")
	cmd.Flags().StringVar(&cloudGRPC, "cloud-grpc", "", "Cloud gRPC endpoint of the session to use")
	return cmd
}

func validateReleaseTag(tag string) error {
	if strings.TrimSpace(tag) == "" {
		return fmt.Errorf("a release tag is required (--tag); there is no implicit 'latest'")
	}
	return nil
}

func createAppRelease(ctx context.Context, conn grpc.ClientConnInterface, auth *config.AuthConfig, tenant, appID, tag string) (string, error) {
	release := new(cloudpb.AppRelease)
	err := cloudrequest.Invoke(ctx, conn, auth, cloudpb.DeploymentService_CreateAppRelease_FullMethodName,
		&cloudpb.CreateAppReleaseRequest{AppId: appID, OrganizationId: tenant, Name: tag, ReleaseType: cloudpb.ReleaseType_CONTAINER}, release)
	if err != nil {
		return "", fmt.Errorf("creating release %s of %s: %w", tag, appID, err)
	}
	if release.GetId() == "" {
		return "", fmt.Errorf("creating release %s of %s: Cloud returned no release id", tag, appID)
	}
	return release.GetId(), nil
}

// setAppReleaseDigest records the digest. UpdateAppRelease replaces the name
// too, so the tag is sent again.
func setAppReleaseDigest(ctx context.Context, conn grpc.ClientConnInterface, auth *config.AuthConfig, id, tag, digest string) error {
	return cloudrequest.Invoke(ctx, conn, auth, cloudpb.DeploymentService_UpdateAppRelease_FullMethodName,
		&cloudpb.UpdateAppReleaseRequest{Id: id, Name: tag, ImageDigest: digest}, new(cloudpb.AppRelease))
}

// errRecordReleaseUnsupported refuses --record-release before anything is
// built, for a deploy it cannot record.
func errRecordReleaseUnsupported(what string) error {
	return usageErrorf("--record-release cannot record %s; it records one container deployed to one WendyOS device", what)
}

// releaseCallTimeout bounds each release call to Cloud, as app registration
// is bounded.
const releaseCallTimeout = 15 * time.Second

// deployRelease records one `wendy run --record-release <tag>` deploy. The
// device's organization and operator session are found before the build, so
// a device that cannot record is refused up front. The release is created
// only when a path that supports recording is about to send the container, so
// a failed build or an unsupported path leaves nothing behind; the digest is
// stored as soon as the agent reports it, so an attached session ended with
// Ctrl-C still completes the release.
type deployRelease struct {
	appID, tag string

	mu     sync.Mutex
	auth   *config.AuthConfig // operator session for the device's organization
	tenant string             // the device's organization
	id     string
	stored bool
	err    error // why the digest was not stored
}

func newDeployRelease(appID, tag string) (*deployRelease, error) {
	if err := validateReleaseTag(tag); err != nil {
		return nil, usageErrorf("%v", err)
	}
	return &deployRelease{appID: appID, tag: tag}, nil
}

// prepare finds the organization of the device behind provisioning and the
// operator session that deploys to it. It creates nothing.
func (r *deployRelease) prepare(ctx context.Context, provisioning deploymentProvisioningClient, loadConfig func() (*config.Config, error)) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	ctx, cancel := context.WithTimeout(ctx, releaseCallTimeout)
	defer cancel()
	err := registerDeviceApps(ctx, provisioning, []string{r.appID}, loadConfig,
		func(_ context.Context, auth *config.AuthConfig, device *agentpb.ProvisionedResponse, _ []string) error {
			identity, err := certs.ParsePrincipal(device.GetPrincipalUri())
			if err != nil || identity.TenantUUID == "" {
				return fmt.Errorf("--record-release needs a device enrolled with Wendy Cloud v2")
			}
			r.auth, r.tenant = auth, identity.TenantUUID
			return nil
		})
	if err != nil {
		return err
	}
	if r.tenant == "" {
		return fmt.Errorf("--record-release needs a device enrolled with Wendy Cloud")
	}
	return nil
}

// begin creates the release, once, in the organization prepare found and
// returns its id for the container. A nil recorder records nothing.
func (r *deployRelease) begin(ctx context.Context) (*string, error) {
	if r == nil {
		return nil, nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.id == "" {
		ctx, cancel := context.WithTimeout(ctx, releaseCallTimeout)
		defer cancel()
		err := withCloud(ctx, r.auth, func(ctx context.Context, cc grpc.ClientConnInterface) (err error) {
			r.id, err = createAppRelease(ctx, cc, r.auth, r.tenant, r.appID, r.tag)
			return err
		})
		if err != nil {
			return nil, err
		}
	}
	id := r.id
	return &id, nil
}

// store records the digest the agent reported for the release's container.
// It is detached from the deploy's cancellation: the deploy may be ending.
func (r *deployRelease) store(ctx context.Context, digest string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.id == "" || r.stored {
		return
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), releaseCallTimeout)
	defer cancel()
	r.err = withCloud(ctx, r.auth, func(ctx context.Context, cc grpc.ClientConnInterface) error {
		return setAppReleaseDigest(ctx, cc, r.auth, r.id, r.tag, digest)
	})
	if r.stored = r.err == nil; r.stored {
		cliLogln("Recorded release %s: %s = %s", r.id, r.tag, digest)
	}
}

// outcome reports a recording that did not complete; deployErr is the
// deploy's own result.
func (r *deployRelease) outcome(deployErr error) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	switch {
	case r.stored:
		return nil
	case r.id == "" && deployErr == nil:
		return fmt.Errorf("--record-release is not supported for this project or deploy path; nothing was recorded")
	case r.id == "":
		return nil
	case r.err != nil:
		return fmt.Errorf("release %s (%s) was created without its digest: %w", r.id, r.tag, r.err)
	case deployErr != nil:
		return fmt.Errorf("release %s (%s) was created, but the deploy failed before the device reported an image digest; it stays without one", r.id, r.tag)
	default:
		return fmt.Errorf("release %s (%s) was created, but the device reported no image digest (an older agent?); it stays without one", r.id, r.tag)
	}
}

func withCloud(ctx context.Context, auth *config.AuthConfig, call func(context.Context, grpc.ClientConnInterface) error) error {
	conn, err := dialCloudGRPC(auth)
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close() }()
	ctx, err = cloudContext(ctx, auth)
	if err != nil {
		return err
	}
	return call(ctx, conn)
}

type deployReleaseKey struct{}

// withDeployRelease carries the recorder on the deploy's context, so every
// create path can hand it the agent's digest without changing signature.
func withDeployRelease(ctx context.Context, r *deployRelease) context.Context {
	return context.WithValue(ctx, deployReleaseKey{}, r)
}

// noteImageDigest hands a create response's digest to the deploy's recorder,
// if it has one. A stream without a context (test doubles) has none.
func noteImageDigest(ctx context.Context, digest string) {
	if ctx == nil || digest == "" {
		return
	}
	if r, ok := ctx.Value(deployReleaseKey{}).(*deployRelease); ok {
		r.store(ctx, digest)
	}
}
