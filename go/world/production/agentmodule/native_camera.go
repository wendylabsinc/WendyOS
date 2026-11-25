package agentmodule

import (
	"context"
	"sort"
	"strings"

	"github.com/wendylabsinc/wendy/go/world/production/appservice"
	"github.com/wendylabsinc/wendy/go/world/production/cameraframe"
	"github.com/wendylabsinc/wendy/go/world/production/serviceorigin"
)

// NativeCameraBinding maps an existing app publication scope to one native
// camera. It is host configuration, not a camera name supplied by an app.
// Reading a frame does not publish it or establish its source clock domain.
type NativeCameraBinding struct {
	App           string `json:"app"`
	Service       string `json:"service"`
	ComponentType string `json:"component_type"`
	Stream        string `json:"stream"`
	SourceID      string `json:"source_id"`
}

type NativeCameraProvider func(context.Context, string) (cameraframe.Frame, error)

func validateNativeCameras(policy Policy, cameras []NativeCameraBinding) error {
	if len(cameras) > 16 {
		return ErrDenied
	}
	seen := make(map[string]bool, len(cameras))
	for _, camera := range cameras {
		key := camera.App + "\x00" + camera.Service + "\x00" + camera.ComponentType + "\x00" + camera.Stream
		if seen[key] || !strings.HasPrefix(camera.SourceID, "sensorlink:") || len(camera.SourceID) <= len("sensorlink:") || len(camera.SourceID) > 256 || strings.ContainsAny(camera.SourceID, "\x00\r\n") || camera.ComponentType != "wendy.sensor.image.v1" && camera.ComponentType != "wendy.sensor.capture.v1" {
			return ErrDenied
		}
		seen[key] = true
		allowed := false
		for _, grant := range policy.Grants {
			if grant.App == camera.App && grant.Service == camera.Service {
				for _, scope := range grant.Publish {
					if scope.ComponentType == camera.ComponentType && scope.Stream == camera.Stream {
						allowed = true
					}
				}
			}
		}
		if !allowed {
			return ErrDenied
		}
	}
	return nil
}

// SetNativeCameraProvider attaches the native sensor manager once at startup.
// The provider returns an owned snapshot and must honor context cancellation.
func (m *Manager) SetNativeCameraProvider(provider NativeCameraProvider) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed || provider == nil || m.nativeCamera != nil {
		return ErrDenied
	}
	m.nativeCamera = provider
	return nil
}

func (m *Manager) NativeCameraSourceIDs() []string {
	seen := make(map[string]bool)
	for _, camera := range m.cfg.NativeCameras {
		seen[camera.SourceID] = true
	}
	ids := make([]string, 0, len(seen))
	for id := range seen {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

func (m *Manager) nativeCameraSource(scope serviceorigin.Scope, publication serviceorigin.PublishScope) string {
	if !publishes(scope, publication) {
		return ""
	}
	for _, camera := range m.cfg.NativeCameras {
		if camera.App == scope.App && camera.Service == scope.Service && camera.ComponentType == publication.ComponentType && camera.Stream == publication.Stream {
			return camera.SourceID
		}
	}
	return ""
}

func (m *Manager) readNativeCamera(ctx context.Context, scope serviceorigin.Scope, publication serviceorigin.PublishScope, fence *appservice.Fence) (cameraframe.Frame, error) {
	var frame cameraframe.Frame
	source := m.nativeCameraSource(scope, publication)
	if source == "" {
		return frame, ErrDenied
	}
	err := m.access(ctx, scope, fence, func(b *binding) bool {
		return m.nativeCameraSource(b.scope, publication) == source
	}, func(ctx context.Context) error {
		m.mu.RLock()
		provider := m.nativeCamera
		m.mu.RUnlock()
		if provider == nil {
			return ErrDenied
		}
		var err error
		frame, err = provider(ctx, source)
		if err != nil {
			return err
		}
		if ctx.Err() != nil || frame.Metadata.SourceID != source {
			return ErrDenied
		}
		return nil
	})
	return frame, err
}
