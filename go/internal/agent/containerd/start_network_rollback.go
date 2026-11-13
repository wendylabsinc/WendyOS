package containerd

import (
	"context"
	"fmt"
	"time"
)

// rollbackPreparedNetwork retires a successful fresh CNI ADD when preparation
// fails before the entrypoint starts. Retained ordinary bridge sandboxes use
// their own destruction path. The caller holds the container lifecycle lock.
// Kernel-rule failure leaves allocation, proof and cache intact for retry.
// A failed CNI release can have partial effects; keep the cache and re-prove
// allocator ownership on retry instead of assuming its lease still exists.
func (c *Client) rollbackPreparedNetwork(ctx context.Context, containerID string, labels map[string]string, ip string, releaseCNI func(context.Context) error) error {
	appID, _, err := containerOwnerFromLabels(containerID, labels)
	if err != nil {
		c.releaseMeshIngress(containerID)
		return err
	}
	// The ADD result is known, but the pinned allocator must still prove this
	// exact container owns it before any cleanup can release the address.
	if err := c.retireMeshNetwork(containerID, labels, ip); err != nil {
		return err
	}
	if err := rollbackCNIAdd(ctx, 10*time.Second, releaseCNI); err != nil {
		return fmt.Errorf("releasing failed-start CNI allocation: %w", err)
	}
	c.mu.Lock()
	c.forgetRetiredMeshIP(labels)
	c.mu.Unlock()
	if needsGatewayDNS(labels[labelKeyIsolation], parseEntitlementsFromAnnotations(labels)) {
		c.releaseMeshDNS(containerID, appID)
	}
	return nil
}
