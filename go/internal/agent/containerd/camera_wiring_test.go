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

// TestTwoPlaneConsumerNames_RequiresCamera pins the gate after frame identity
// moved in-band. The camera entitlement is the whole grant: it is the
// device-node grant that the agent-fed v4l2loopback node needs, and identity
// now arrives in the buffer timestamp rather than on a second channel that
// would need authorising separately.
func TestTwoPlaneConsumerNames_RequiresCamera(t *testing.T) {
	cameraLabels := map[string]string{
		labelKeyAppID:                 "modelapp",
		"sh.wendy/entitlement.camera": `{"mode":"detect"}`,
	}
	viaVideoAlias := map[string]string{
		labelKeyAppID:                "aliasapp",
		"sh.wendy/entitlement.video": `{"mode":"detect"}`,
	}
	noCamera := map[string]string{
		labelKeyAppID:                        "writerapp",
		"sh.wendy/entitlement.episode-write": `{}`,
	}
	invalidAppID := map[string]string{
		labelKeyAppID:                 "bad app id!",
		"sh.wendy/entitlement.camera": `{"mode":"detect"}`,
	}

	tests := []struct {
		name  string
		infos []containerCameraInfo
		want  []string
	}{
		{
			name:  "camera, running -> in",
			infos: []containerCameraInfo{{name: "modelapp", labels: cameraLabels, running: true}},
			want:  []string{"modelapp"},
		},
		{
			name:  "deprecated video alias, running -> in",
			infos: []containerCameraInfo{{name: "aliasapp", labels: viaVideoAlias, running: true}},
			want:  []string{"aliasapp"},
		},
		{
			// No device-node grant, so no node: episode-write is the write
			// path and says nothing about reading frames.
			name:  "no camera entitlement -> out",
			infos: []containerCameraInfo{{name: "writerapp", labels: noCamera, running: true}},
			want:  nil,
		},
		{
			name:  "camera but stopped -> out",
			infos: []containerCameraInfo{{name: "modelapp", labels: cameraLabels, running: false}},
			want:  nil,
		},
		{
			name:  "invalid appID label -> out",
			infos: []containerCameraInfo{{name: "brokenapp", labels: invalidAppID, running: true}},
			want:  nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := twoPlaneConsumerNames(tt.infos)
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("twoPlaneConsumerNames() = %#v, want %#v", got, tt.want)
			}
		})
	}
}

// TestTwoPlaneConsumerNames_MatchesCameraConsumers pins the relation the design
// depends on. It used to be containment, because the two-plane path demanded a
// second entitlement; with frame identity in-band the two sets are equal, and
// asserting equality keeps this test honest. Containment alone would now pass
// no matter what the gate did, since identical sets satisfy it trivially.
func TestTwoPlaneConsumerNames_MatchesCameraConsumers(t *testing.T) {
	infos := []containerCameraInfo{
		{name: "cam", labels: map[string]string{
			labelKeyAppID:                 "cam",
			"sh.wendy/entitlement.camera": `{"mode":"detect"}`,
		}, running: true},
		{name: "alias", labels: map[string]string{
			labelKeyAppID:                "alias",
			"sh.wendy/entitlement.video": `{"mode":"detect"}`,
		}, running: true},
		{name: "writer", labels: map[string]string{
			labelKeyAppID:                        "writer",
			"sh.wendy/entitlement.episode-write": `{}`,
		}, running: true},
		{name: "stopped", labels: map[string]string{
			labelKeyAppID:                 "stopped",
			"sh.wendy/entitlement.camera": `{"mode":"detect"}`,
		}, running: false},
	}

	if got, want := twoPlaneConsumerNames(infos), cameraConsumerNames(infos); !reflect.DeepEqual(got, want) {
		t.Errorf("twoPlaneConsumerNames() = %#v, cameraConsumerNames() = %#v; the two-plane "+
			"path is the camera path and the two must not drift apart", got, want)
	}
}
