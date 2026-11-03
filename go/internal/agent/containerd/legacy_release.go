package containerd

import (
	"context"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"

	containerd "github.com/containerd/containerd/v2/client"
	"github.com/containerd/containerd/v2/core/remotes/docker"
	digest "github.com/opencontainers/go-digest"
	"github.com/wendylabsinc/wendy/go/internal/agent/services"
	"github.com/wendylabsinc/wendy/go/internal/shared/appconfig"
	agentpb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
	cloudpb "github.com/wendylabsinc/wendy/go/proto/gen/cloudpb"
)

// SetCloudEnrollment supplies the saved protocol binding. Numeric release IDs
// are accepted only for an enrolled v1 device; certificate SAN preference does
// not select the protocol. The callback is read again for every operation.
func (c *Client) SetCloudEnrollment(get func() (services.CloudEnrollment, error)) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.cloudEnrollment = get
}
func (c *Client) legacyEnrollment() bool {
	c.mu.Lock()
	get := c.cloudEnrollment
	c.mu.Unlock()
	if get == nil {
		return false
	}
	e, err := get()
	return err == nil && e.Mode == services.CloudLegacy
}
func (c *Client) validReleaseID(id string) bool {
	if !c.legacyEnrollment() {
		return isCanonicalUUID(id)
	}
	n, err := strconv.ParseInt(id, 10, 32)
	return err == nil && n > 0 && strconv.FormatInt(n, 10) == id
}

// InstallLegacyRelease accepts only a container explicitly assigned by the
// authenticated v1 deployment service. Pull by digest and scope the temporary
// credentials to its registry host, never to a redirect or arbitrary endpoint.
func (c *Client) InstallLegacyRelease(ctx context.Context, appID string, releaseID int32, manifest string, credentials *cloudpb.GetPullCredentialsResponse) error {
	if !c.legacyEnrollment() {
		return fmt.Errorf("legacy deployment requires v1 enrollment")
	}
	if err := appconfig.ValidateAppID(appID); err != nil {
		return err
	}
	if releaseID <= 0 {
		return fmt.Errorf("invalid legacy release ID")
	}
	if credentials.GetReleaseType() != cloudpb.ReleaseType_CONTAINER {
		return fmt.Errorf("legacy release is not a container")
	}
	parsed, err := digest.Parse(manifest)
	if err != nil || parsed.Algorithm() != digest.SHA256 || parsed.Validate() != nil {
		return fmt.Errorf("legacy release requires a SHA-256 manifest digest")
	}
	registry := credentials.GetRegistryUrl()
	if !strings.Contains(registry, "://") {
		registry = "https://" + registry
	}
	endpoint, err := url.Parse(registry)
	if err != nil || endpoint.Host == "" || endpoint.User != nil || (endpoint.Scheme != "https" && endpoint.Scheme != "http") || endpoint.RawQuery != "" || endpoint.Fragment != "" {
		return fmt.Errorf("invalid legacy registry endpoint")
	}
	if endpoint.Scheme == "http" && credentials.GetAccessToken() != "" {
		return fmt.Errorf("refusing registry credentials over plaintext HTTP")
	}
	if expiry := credentials.GetExpiresAt(); expiry != nil && (!expiry.IsValid() || !expiry.AsTime().After(time.Now())) {
		return fmt.Errorf("legacy pull credentials expired")
	}
	path := strings.TrimPrefix(strings.TrimPrefix(credentials.GetFullArtifactPath(), "https://"), "http://")
	if !strings.HasPrefix(path, endpoint.Host+"/") || strings.ContainsAny(path, "@?#\r\n") {
		return fmt.Errorf("legacy artifact does not belong to the credential registry")
	}
	reference := path + "@" + manifest
	resolver := docker.NewResolver(docker.ResolverOptions{PlainHTTP: endpoint.Scheme == "http", Credentials: func(host string) (string, string, error) {
		if host != endpoint.Host {
			return "", "", fmt.Errorf("refusing credentials for another registry")
		}
		return credentials.GetUsername(), credentials.GetAccessToken(), nil
	}})
	if _, err := c.client.Pull(c.withNamespace(ctx), reference, containerd.WithResolver(resolver)); err != nil {
		return fmt.Errorf("pulling assigned legacy release: %w", err)
	}
	id := strconv.FormatInt(int64(releaseID), 10)
	return c.CreateContainer(ctx, &agentpb.CreateContainerRequest{AppName: appID, ImageName: reference, ReleaseId: &id}, &appconfig.AppConfig{AppID: appID})
}
