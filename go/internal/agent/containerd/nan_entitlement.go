package containerd

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	containerd "github.com/containerd/containerd/v2/client"
	"github.com/containerd/errdefs"
	"github.com/wendylabsinc/wendy/go/internal/agent/networkmanager"
	localoci "github.com/wendylabsinc/wendy/go/internal/agent/oci"
	"github.com/wendylabsinc/wendy/go/internal/shared/appconfig"
	"go.uber.org/zap"
)

// nanSocketStat is replaceable in tests so the /proc/<pid>/root comparison can
// be exercised without a container runtime or a privileged mount namespace.
var nanSocketStat = os.Stat

// staleNANSocketForPID compares the socket currently served by wpa_supplicant
// with the file bind-mounted into a running container. A file bind mount keeps
// the old inode when supplicant replaces its socket pathname.
func staleNANSocketForPID(pid uint32, hostSocket os.FileInfo) (bool, error) {
	containerPath := fmt.Sprintf("/proc/%d/root%s", pid, localoci.NANControlSocketContainerPath)
	containerSocket, err := nanSocketStat(containerPath)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil // The task is exiting; a later tick will recheck it.
	}
	if err != nil {
		return false, err
	}
	return containerSocket.Mode()&os.ModeSocket == 0 || !os.SameFile(hostSocket, containerSocket), nil
}

// StaleNANSocketContainers returns only running, NAN-entitled tasks whose
// mounted control socket still points at a replaced supplicant inode. A missing
// host socket during supplicant restart is transient, so it never triggers a
// container restart. Confirmed stopped/missing tasks are also reconciled under
// their app lifecycle lock, covering tasks inherited after an agent restart.
func (c *Client) StaleNANSocketContainers(ctx context.Context) ([]string, error) {
	hostSocket, err := nanSocketStat(localoci.NANControlSocketHostPath)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if hostSocket.Mode()&os.ModeSocket == 0 {
		return nil, fmt.Errorf("NAN control path %s is not a socket", localoci.NANControlSocketHostPath)
	}
	ctx = c.withNamespace(ctx)
	ctrs, err := c.client.Containers(ctx, fmt.Sprintf("labels.%q", appconfig.EntitlementAnnotationKeyPrefix+appconfig.EntitlementNAN))
	if err != nil {
		return nil, err
	}
	var stale []string
	idleApps := make(map[string]bool)
	for _, ctr := range ctrs {
		labels, err := ctr.Labels(ctx)
		if err != nil || !hasNANEntitlement(parseEntitlementsFromAnnotations(labels)) ||
			appconfig.ValidateAppID(labels[labelKeyAppID]) != nil {
			continue
		}
		task, err := ctr.Task(ctx, nil)
		if err != nil {
			if errdefs.IsNotFound(err) {
				idleApps[labels[labelKeyAppID]] = true
			}
			continue
		}
		status, err := task.Status(ctx)
		if err == nil && status.Status == containerd.Stopped {
			idleApps[labels[labelKeyAppID]] = true
		}
		if err != nil || status.Status != containerd.Running || task.Pid() == 0 || labels[labelKeyStoppedByUser] == "true" {
			continue
		}
		isStale, err := staleNANSocketForPID(task.Pid(), hostSocket)
		if err != nil {
			c.logger.Warn("Could not compare NAN socket mount", zap.String("container_id", ctr.ID()), zap.Error(err))
			continue
		}
		if isStale {
			stale = append(stale, ctr.ID())
		}
	}
	// Agent-only restarts inherit tasks without their original output watcher.
	// Recheck all sibling statuses under the lifecycle lock; this initial list
	// is merely a candidate and may already be stale after an automatic restart.
	for appID := range idleApps {
		if err := c.releaseNANIfIdle(ctx, appID); err != nil {
			c.logger.Warn("Could not reconcile stopped app NAN interface", zap.String("app_id", appID), zap.Error(err))
		}
	}
	return stale, nil
}

const nanHelperPath = "/usr/sbin/wendyos-nan"

var nanClientRootPath = "/var/lib/wendy/nan-clients"

// App NDIs have their own names so an app never tears down the mesh provider's
// wnanndi0 or another app's data paths. Linux IFNAMSIZ limits names to 15 bytes.
func nanAppNDI(appID string) string {
	sum := sha256.Sum256([]byte(appID))
	return fmt.Sprintf("wa%x", sum[:6])
}

func nanAppClientDir(appID string) string {
	return filepath.Join(nanClientRootPath, nanAppNDI(appID))
}

// nanCreateLease owns only resources created while replacing/creating one
// container. If a later OCI, snapshot, or containerd step fails, release the
// app NDI when no NAN-entitled task still runs. A successful NewContainer owns
// the NDI until StopContainer or DeleteContainer.
type nanCreateLease struct {
	client               *Client
	ctx                  context.Context
	appID                string
	attempted, committed bool
}

func (l *nanCreateLease) finish(createErr *error) {
	if !l.attempted || l.committed || *createErr == nil {
		return
	}
	if err := l.client.releaseNANIfIdleLocked(l.ctx, l.appID); err != nil {
		*createErr = errors.Join(*createErr, fmt.Errorf("cleaning up failed NAN container create: %w", err))
	}
}

var runNANHelper = func(ctx context.Context, args ...string) error {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, nanHelperPath, args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s %v: %w: %s", nanHelperPath, args, err, out)
	}
	return nil
}

var setNANLinkUp = func(ctx context.Context, ndi string) error {
	cmd := exec.CommandContext(ctx, "ip", "link", "set", "dev", ndi, "up")
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("bring up NAN data interface %s: %w: %s", ndi, err, out)
	}
	return nil
}

// prepareNANEntitlement provisions radio infrastructure, not app sessions.
// The app alone owns its publish/subscribe handles and NDPs. `start` and
// `ndi-create` are idempotent; an already-running mesh or app session survives.
func prepareNANEntitlement(ctx context.Context, appID string) (string, error) {
	ndi := nanAppNDI(appID)
	if err := ensureAppNDIUnmanaged(ctx); err != nil {
		return "", err
	}
	if err := runNANHelper(ctx, "start"); err != nil {
		return "", fmt.Errorf("nan entitlement requires host NAN support: %w", err)
	}
	if err := runNANHelper(ctx, "ndi-create", ndi); err != nil {
		return "", fmt.Errorf("prepare app NAN data interface: %w", err)
	}
	if err := setNANLinkUp(ctx, ndi); err != nil {
		return "", err
	}
	path := localoci.NANControlSocketHostPath
	fi, err := os.Lstat(path)
	if err != nil {
		return "", fmt.Errorf("nan entitlement requires %s: %w", path, err)
	}
	if fi.Mode()&os.ModeSocket == 0 {
		return "", fmt.Errorf("nan entitlement: %s is not a Unix socket", path)
	}
	// The Pi5 supplicant creates nan0 as root:root 0770. Grant exactly this
	// socket to the reserved group that OCI adds only to nan-entitled apps.
	// The global and wlan0 sockets remain root-only and are never mounted.
	if err := os.Chown(path, 0, int(localoci.NANControlGroupGID)); err != nil {
		return "", fmt.Errorf("grant NAN socket group: %w", err)
	}
	if err := os.Chmod(path, 0o770); err != nil {
		return "", fmt.Errorf("grant NAN socket group access: %w", err)
	}
	clientDir := nanAppClientDir(appID)
	if err := os.MkdirAll(clientDir, 0o770); err != nil {
		return "", fmt.Errorf("prepare NAN client socket directory: %w", err)
	}
	if err := os.Chown(clientDir, 0, int(localoci.NANControlGroupGID)); err != nil {
		return "", fmt.Errorf("grant NAN client directory group: %w", err)
	}
	if err := os.Chmod(clientDir, 0o770); err != nil {
		return "", fmt.Errorf("grant NAN client directory access: %w", err)
	}
	return ndi, nil
}

// NetworkManager must not run DHCP on an NDI or install a default route from
// it. Write the rules before creating the interface, just as the mesh provider
// does for wnanndi0. They cover the reserved wa<12 hex> app NDI namespace.
func ensureAppNDIUnmanaged(ctx context.Context) error {
	rules := []nanUnmanagedRule{
		{"/run/NetworkManager/conf.d/05-wendy-nan-app.conf", "# Wendy app NAN data interfaces\n[device-wendy-nan-app]\nmatch-device=interface-name:wa*\nmanaged=0\n"},
		{"/run/systemd/network/05-wendy-nan-app.network", "# Wendy app NAN data interfaces\n[Match]\nName=wa*\n[Link]\nUnmanaged=yes\n"},
	}
	return appNDIRules.ensure(ctx, rules, networkmanager.Reload)
}

// releaseNANEntitlement removes only the app's NDI. nan0 is intentionally
// retained: another app or the localmesh provider may still be using it.
func releaseNANEntitlement(ctx context.Context, appID string) error {
	if err := runNANHelper(ctx, "ndi-remove", nanAppNDI(appID)); err != nil {
		return err
	}
	return os.RemoveAll(nanAppClientDir(appID))
}

func hasNANEntitlement(entitlements []appconfig.Entitlement) bool {
	for _, ent := range entitlements {
		if ent.Type == appconfig.EntitlementNAN {
			return true
		}
	}
	return false
}

// releaseNANIfIdle is safe for one-service stops in a multi-service app and
// for delete-without-stop. The radio-wide nan0 is never stopped here.
// lockNANOperation uses a separate app-scoped key: service containers share
// one NDI. Acquire outside c.mu and after any container network-operation lock.
func (c *Client) lockNANOperation(appID string) func() {
	return c.lockNetworkOperation(":nan:" + appID)
}

func (c *Client) releaseNANIfIdle(ctx context.Context, appID string) error {
	unlock := c.lockNANOperation(appID)
	defer unlock()
	return c.releaseNANIfIdleLocked(ctx, appID)
}

// Caller holds the app NAN lock through truth check and interface removal.
func (c *Client) releaseNANIfIdleLocked(ctx context.Context, appID string) error {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 20*time.Second)
	defer cancel()
	ctrs, err := c.containersForApp(ctx, appID)
	if err != nil {
		return err
	}
	for _, ctr := range ctrs {
		labels, err := ctr.Labels(ctx)
		if err != nil {
			return err
		}
		if !hasNANEntitlement(parseEntitlementsFromAnnotations(labels)) {
			continue
		}
		task, err := ctr.Task(ctx, nil)
		if err != nil {
			if errdefs.IsNotFound(err) {
				continue // Stopped container has no live NDPs.
			}
			return err // Unknown task state: preserve the app NDI.
		}
		status, err := task.Status(ctx)
		if err != nil || status.Status != containerd.Stopped {
			return nil // Running, paused, created or unknown: preserve the app NDI.
		}
	}
	return releaseNANEntitlement(context.WithoutCancel(ctx), appID)
}

// A task exit may arrive after a replacement starts. The same app lock covers
// replacement preparation/start, so neither the generation check nor the
// all-siblings idle check can race deletion of a freshly prepared NDI.
// Run asynchronously: a graceful stop may be waiting for this task's IO drain.
func (c *Client) releaseNANAfterTaskExit(ctx context.Context, containerName, appID string, generation uint64) {
	if appID == "" || generation == 0 {
		return
	}
	unlock := c.lockNANOperation(appID)
	defer unlock()
	c.meshIngressMu.Lock()
	current := c.meshIngressRuns[containerName]
	c.meshIngressMu.Unlock()
	if current != 0 && current != generation {
		return
	}
	if err := c.releaseNANIfIdleLocked(ctx, appID); err != nil {
		c.logger.Warn("Could not remove exited app NAN data interface", zap.String("app_id", appID), zap.Error(err))
	}
}

func (l *nanCreateLease) finishStart() {
	if !l.attempted || l.committed {
		return
	}
	if err := l.client.releaseNANIfIdleLocked(l.ctx, l.appID); err != nil {
		l.client.logger.Warn("Could not clean up failed NAN start", zap.String("app_id", l.appID), zap.Error(err))
	}
}
