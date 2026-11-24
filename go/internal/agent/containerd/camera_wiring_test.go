package containerd

import (
	"context"
	"reflect"
	"testing"

	"go.uber.org/zap"

	"github.com/wendylabsinc/wendy/go/internal/shared/appconfig"
)

// TestCameraConsumerNames_FiltersEntitlementAndRunning covers the pure
// filter cameraConsumerNames applies to raw container truth: camera and the
// deprecated video alias (both map to applyCamera, entitlements.go:83) count
// as camera consumers; no entitlement, a stopped task, or labels that fail
// the same appID/serviceName revalidation appSystemAPIOwnersFromLabels
// applies (client.go:204-217) are all excluded.
func TestCameraConsumerNames_FiltersEntitlementAndRunning(t *testing.T) {
	cameraLabels := map[string]string{
		labelKeyAppID:                 "camapp",
		"sh.wendy/entitlement.camera": `{"mode":"detect"}`,
	}
	videoLabels := map[string]string{
		labelKeyAppID:                "videoapp",
		"sh.wendy/entitlement.video": `{"mode":"detect"}`,
	}
	noneLabels := map[string]string{
		labelKeyAppID: "plainapp",
	}
	episodeWriteLabels := map[string]string{
		labelKeyAppID:                        "writerapp",
		"sh.wendy/entitlement.episode-write": `{}`,
	}
	invalidAppIDLabels := map[string]string{
		labelKeyAppID:                 "bad app id!",
		"sh.wendy/entitlement.camera": `{"mode":"detect"}`,
	}

	tests := []struct {
		name  string
		infos []containerCameraInfo
		want  []string
	}{
		{
			name: "camera entitlement, running -> in",
			infos: []containerCameraInfo{
				{name: "camapp", labels: cameraLabels, running: true},
			},
			want: []string{"camapp"},
		},
		{
			name: "deprecated video entitlement, running -> in",
			infos: []containerCameraInfo{
				{name: "videoapp", labels: videoLabels, running: true},
			},
			want: []string{"videoapp"},
		},
		{
			name: "no camera/video entitlement -> out",
			infos: []containerCameraInfo{
				{name: "plainapp", labels: noneLabels, running: true},
			},
			want: nil,
		},
		{
			// episode-write is the write path and grants no camera node.
			name: "episode-write without camera -> out",
			infos: []containerCameraInfo{
				{name: "writerapp", labels: episodeWriteLabels, running: true},
			},
			want: nil,
		},
		{
			name: "camera entitlement but stopped -> out",
			infos: []containerCameraInfo{
				{name: "camapp", labels: cameraLabels, running: false},
			},
			want: nil,
		},
		{
			name: "invalid appID label -> out",
			infos: []containerCameraInfo{
				{name: "brokenapp", labels: invalidAppIDLabels, running: true},
			},
			want: nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := cameraConsumerNames(tt.infos)
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("cameraConsumerNames() = %#v, want %#v", got, tt.want)
			}
		})
	}
}

// TestSyncCameraLoopbacks_NilProviderNoop verifies the guard that keeps a
// Client built without SetCameraLoopbackProvider (the common case for unit
// tests, and any build without the ipcam module) from touching containerd at
// all — it must return immediately rather than panicking on a nil c.client.
func TestSyncCameraLoopbacks_NilProviderNoop(t *testing.T) {
	c := &Client{logger: zap.NewNop()}

	// Must not panic despite c.client being nil: the nil-provider check has
	// to come before any containerd call.
	c.SyncCameraLoopbacks(context.Background())
}

// TestShouldEnsureCameraNodes covers the create-hook gating predicate: only
// the camera entitlement and its deprecated video alias should trigger
// EnsureCameraNodes before ApplyEntitlements runs (see the create-hook
// comment in CreateContainerWithProgress, citing applyCamera's /dev
// bind-mount + major-81 minor-unrestricted rationale).
func TestShouldEnsureCameraNodes(t *testing.T) {
	tests := []struct {
		name   string
		appCfg *appconfig.AppConfig
		want   bool
	}{
		{
			name:   "nil appCfg",
			appCfg: nil,
			want:   false,
		},
		{
			name:   "camera entitlement",
			appCfg: &appconfig.AppConfig{Entitlements: []appconfig.Entitlement{{Type: appconfig.EntitlementCamera}}},
			want:   true,
		},
		{
			name:   "deprecated video entitlement",
			appCfg: &appconfig.AppConfig{Entitlements: []appconfig.Entitlement{{Type: appconfig.EntitlementVideo}}},
			want:   true,
		},
		{
			name:   "unrelated entitlement only",
			appCfg: &appconfig.AppConfig{Entitlements: []appconfig.Entitlement{{Type: appconfig.EntitlementGPU}}},
			want:   false,
		},
		{
			name:   "no entitlements",
			appCfg: &appconfig.AppConfig{},
			want:   false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := shouldEnsureCameraNodes(tt.appCfg)
			if got != tt.want {
				t.Errorf("shouldEnsureCameraNodes(%+v) = %v, want %v", tt.appCfg, got, tt.want)
			}
		})
	}
}
