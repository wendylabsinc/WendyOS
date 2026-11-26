package containerd

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"strings"

	containerd "github.com/containerd/containerd/v2/client"
	"github.com/containerd/containerd/v2/core/containers"
	"github.com/containerd/errdefs"
	"github.com/containerd/typeurl/v2"
	specs "github.com/opencontainers/runtime-spec/specs-go"
	"go.uber.org/zap"
	"google.golang.org/protobuf/proto"

	localoci "github.com/wendylabsinc/wendy/go/internal/agent/oci"
	"github.com/wendylabsinc/wendy/go/internal/agent/secretmount"
	"github.com/wendylabsinc/wendy/go/internal/agent/services"
	cloudpbv2 "github.com/wendylabsinc/wendy/go/proto/gen/cloudpb/v2"
)

const secretExtension = "sh.wendy.sealed-app-secrets"
const secretContentLabel = "sh.wendy/secret-content"
const secretMountAnnotation = "sh.wendy/secret-mount"
const secretDestination = "/run/secrets"

// SetAppSecretOpener is installed before the agent starts serving requests.
func (c *Client) SetAppSecretOpener(opener services.AppSecretOpener) { c.appSecretOpener = opener }
func (c *Client) SupportsAppSecrets() bool {
	return secretmount.Supported() && c.appSecretOpener != nil
}

func secretCreateOptions(ctx context.Context, app string, spec *localoci.Spec, labels map[string]string) (containerd.NewContainerOpts, error) {
	a := services.ReleaseSecretsFromContext(ctx)
	if len(a.GetSecrets()) == 0 {
		return func(context.Context, *containerd.Client, *containers.Container) error { return nil }, nil
	}
	if a.GetAppId() != app {
		return nil, errors.New("secret assignment belongs to another app")
	}
	for _, m := range spec.Mounts {
		if conflictsWithSecrets(m.Destination) {
			return nil, errors.New("app mount conflicts with /run/secrets")
		}
	}
	labels[secretContentLabel] = services.SecretContentID(a)
	// Container extensions have no label-size limit. Only ciphertext and the
	// device wraps persist; decryption happens again for every task start.
	envelope := &cloudpbv2.DeviceAssignment{OrganizationId: a.GetOrganizationId(), Device: a.GetDevice(), AppId: app, ConfigJson: a.GetConfigJson(), Secrets: a.GetSecrets()}
	return containerd.WithContainerExtension(secretExtension, envelope), nil
}

func conflictsWithSecrets(destination string) bool {
	d := path.Clean(destination)
	return d == secretDestination || strings.HasPrefix(d, secretDestination+"/")
}

func copyContainerExtensions(extensions map[string]typeurl.Any) containerd.NewContainerOpts {
	return func(_ context.Context, _ *containerd.Client, c *containers.Container) error {
		c.Extensions = extensions
		return nil
	}
}

func hostID(id uint32, mappings []specs.LinuxIDMapping) (uint32, error) {
	if len(mappings) == 0 {
		return id, nil
	}
	for _, m := range mappings {
		if uint64(id) >= uint64(m.ContainerID) && uint64(id) < uint64(m.ContainerID)+uint64(m.Size) {
			h := uint64(m.HostID) + uint64(id) - uint64(m.ContainerID)
			if h <= 1<<32-1 {
				return uint32(h), nil
			}
		}
	}
	return 0, errors.New("app secret owner has no host ID mapping")
}

func (c *Client) prepareAppSecrets(ctx context.Context, ctr containerd.Container) (string, error) {
	info, err := ctr.Info(ctx)
	if err != nil {
		return "", err
	}
	ext := info.Extensions[secretExtension]
	if ext == nil {
		if info.Labels[secretContentLabel] != "" {
			return "", errors.New("missing encrypted app secrets")
		}
		return "", nil
	}
	if !c.SupportsAppSecrets() {
		return "", errors.New("app secret mounts unavailable")
	}
	a := &cloudpbv2.DeviceAssignment{}
	if err = proto.Unmarshal(ext.GetValue(), a); err != nil {
		return "", errors.New("invalid encrypted app secrets")
	}
	if a.GetAppId() != info.Labels[labelKeyAppID] || services.SecretContentID(a) != info.Labels[secretContentLabel] {
		return "", errors.New("app secret binding mismatch")
	}
	spec, err := ctr.Spec(ctx)
	if err != nil {
		return "", err
	}
	if spec.Process == nil {
		return "", errors.New("missing container process")
	}
	previous := spec.Annotations[secretMountAnnotation]
	mounts := make([]specs.Mount, 0, len(spec.Mounts)+1)
	for _, m := range spec.Mounts {
		if m.Destination == secretDestination && previous != "" && m.Source == previous {
			continue
		}
		if conflictsWithSecrets(m.Destination) {
			return "", errors.New("app mount conflicts with /run/secrets")
		}
		mounts = append(mounts, m)
	}
	if err = secretmount.Cleanup(previous); err != nil {
		return "", fmt.Errorf("cleaning previous app secrets: %w", err)
	}
	uid, gid := spec.Process.User.UID, spec.Process.User.GID
	if spec.Linux != nil {
		if uid, err = hostID(uid, spec.Linux.UIDMappings); err != nil {
			return "", err
		}
		if gid, err = hostID(gid, spec.Linux.GIDMappings); err != nil {
			return "", err
		}
	}
	values, err := c.appSecretOpener(a)
	defer services.ClearAppSecrets(values)
	if err != nil {
		return "", errors.New("cannot decrypt app secrets")
	}
	dir, err := secretmount.Prepare(values, uid, gid)
	if err != nil {
		return "", err
	}
	spec.Mounts = append(mounts, specs.Mount{Destination: secretDestination, Type: "bind", Source: dir, Options: []string{"bind", "ro", "nodev", "nosuid", "noexec"}})
	if spec.Annotations == nil {
		spec.Annotations = map[string]string{}
	}
	spec.Annotations[secretMountAnnotation] = dir
	// Persist paths and ciphertext only. The next start replaces the path.
	if err = ctr.Update(ctx, func(_ context.Context, _ *containerd.Client, c *containers.Container) error {
		encoded, e := typeurl.MarshalAny(spec)
		if e != nil {
			return e
		}
		c.Spec = encoded
		return nil
	}); err != nil {
		_ = secretmount.Cleanup(dir)
		return "", err
	}
	return dir, nil
}

func (c *Client) cleanupAppSecretMount(dir string) {
	if err := secretmount.Cleanup(dir); err != nil {
		c.logger.Warn("Could not release app secret mount", zap.Error(err))
	}
}

func (c *Client) cleanupContainerSecrets(ctx context.Context, ctr containerd.Container) error {
	labels, err := ctr.Labels(ctx)
	if err != nil {
		return err
	}
	if labels[secretContentLabel] == "" {
		return nil
	}
	spec, err := ctr.Spec(ctx)
	if err != nil {
		return err
	}
	return secretmount.Cleanup(spec.Annotations[secretMountAnnotation])
}

// UpdateAppSecretWraps refreshes only the encrypted restart material after a
// verified equal-revision rewrap. It leaves an already running mount intact.
func (c *Client) UpdateAppSecretWraps(ctx context.Context, a *cloudpbv2.DeviceAssignment) error {
	unlock := c.lockNetworkOperation(a.GetAppId())
	defer unlock()
	ctx = c.withNamespace(ctx)
	ctr, err := c.client.LoadContainer(ctx, a.GetAppId())
	if errdefs.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return err
	}
	info, err := ctr.Info(ctx)
	if err != nil {
		return err
	}
	if info.Labels[secretContentLabel] != services.SecretContentID(a) {
		return nil
	} // failed revision, not installed
	return ctr.Update(ctx, func(_ context.Context, _ *containerd.Client, info *containers.Container) error {
		envelope := &cloudpbv2.DeviceAssignment{OrganizationId: a.GetOrganizationId(), Device: a.GetDevice(), AppId: a.GetAppId(), ConfigJson: a.GetConfigJson(), Secrets: a.GetSecrets()}
		encoded, err := typeurl.MarshalAny(envelope)
		if err != nil {
			return err
		}
		if info.Extensions == nil {
			info.Extensions = map[string]typeurl.Any{}
		}
		info.Extensions[secretExtension] = encoded
		return nil
	})
}

// recoverAppSecretMounts runs before this client is exposed to start requests.
// Unknown task state preserves its mount. Failed inventory preserves everything.
func (c *Client) recoverAppSecretMounts(ctx context.Context) error {
	if !secretmount.Supported() {
		return nil
	}
	entries, err := os.ReadDir(secretmount.Root)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	ctx = c.withNamespace(ctx)
	ctrs, err := c.client.Containers(ctx)
	if err != nil {
		return err
	}
	retained := map[string]bool{}
	for _, ctr := range ctrs {
		spec, err := ctr.Spec(ctx)
		if err != nil {
			return err
		}
		dir := spec.Annotations[secretMountAnnotation]
		if dir == "" {
			continue
		}
		task, err := ctr.Task(ctx, nil)
		if errdefs.IsNotFound(err) {
			continue
		}
		retained[dir] = true
		if err != nil {
			continue
		}
		status, err := task.Status(ctx)
		if err == nil && status.Status == containerd.Stopped {
			delete(retained, dir)
		}
	}
	var failures []error
	for _, entry := range entries {
		if !entry.IsDir() || !strings.HasPrefix(entry.Name(), "task-") {
			continue
		}
		dir := filepath.Join(secretmount.Root, entry.Name())
		if !retained[dir] {
			if err := secretmount.Cleanup(dir); err != nil {
				failures = append(failures, err)
			}
		}
	}
	return errors.Join(failures...)
}
