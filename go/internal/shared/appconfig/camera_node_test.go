package appconfig

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/santhosh-tekuri/jsonschema/v6"
)

func TestValidate_CameraNodeFormatAndStamp(t *testing.T) {
	cam := func(format, stamp string) Entitlement {
		return Entitlement{Type: EntitlementCamera, Format: format, Stamp: stamp}
	}
	tests := []struct {
		name    string
		in      []Entitlement
		wantErr string
	}{
		{"neither key", []Entitlement{cam("", "")}, ""},
		{"explicit defaults", []Entitlement{cam("pixels", "time")}, ""},
		{"native and frame", []Entitlement{cam("native", "frame")}, ""},
		{"h264", []Entitlement{cam("h264", "")}, ""},
		{"format only", []Entitlement{cam("native", "")}, ""},
		{"stamp only", []Entitlement{cam("", "frame")}, ""},
		{"video alias", []Entitlement{{Type: EntitlementVideo, Format: "native", Stamp: "frame"}}, ""},
		{"shared mode other than direct", []Entitlement{{Type: EntitlementCamera, Mode: "legacy", Format: "native"}}, ""},
		{"direct without node keys", []Entitlement{{Type: EntitlementCamera, Mode: "direct"}}, ""},
		{
			"unknown format",
			[]Entitlement{cam("vp8", "")},
			`entitlement[0]: camera format must be one of "pixels", "native", "h264"; got "vp8"`,
		},
		{
			"format is case sensitive",
			[]Entitlement{cam("Native", "")},
			`entitlement[0]: camera format must be one of "pixels", "native", "h264"; got "Native"`,
		},
		{
			"unknown stamp names its index",
			[]Entitlement{{Type: EntitlementGPU}, cam("", "sequence")},
			`entitlement[1]: camera stamp must be one of "time", "frame"; got "sequence"`,
		},
		{
			"unknown format on the video alias",
			[]Entitlement{{Type: EntitlementVideo, Format: "raw"}},
			`entitlement[0]: video format must be one of "pixels", "native", "h264"; got "raw"`,
		},
		{
			"format with direct",
			[]Entitlement{{Type: EntitlementCamera, Mode: "direct", Format: "native"}},
			`entitlement[0]: camera format cannot be combined with mode "direct": the app gets the real device, so there is no camera node to configure`,
		},
		{
			"stamp with direct names its index",
			[]Entitlement{{Type: EntitlementGPU}, {Type: EntitlementVideo, Mode: "direct", Stamp: "frame"}},
			`entitlement[1]: video stamp cannot be combined with mode "direct": the app gets the real device, so there is no camera node to configure`,
		},
		{
			"direct entry beside plain entries",
			[]Entitlement{{Type: EntitlementCamera, Mode: "direct"}, {Type: EntitlementVideo}, {Type: EntitlementCamera, Allowlist: []string{"/dev/video2"}}},
			"",
		},
		{
			"format on another entry than the direct one",
			[]Entitlement{{Type: EntitlementCamera, Mode: "direct", Allowlist: []string{"/dev/video0"}}, {Type: EntitlementCamera, Allowlist: []string{"/dev/video2"}, Format: "native"}},
			`entitlement[1]: camera format cannot be combined with mode "direct" on entitlement[0]: the app gets the real device, so there is no camera node to configure`,
		},
		{
			"stamp on an entry before the direct one",
			[]Entitlement{{Type: EntitlementVideo, Stamp: "time"}, {Type: EntitlementGPU}, {Type: EntitlementCamera, Mode: "direct"}},
			`entitlement[0]: video stamp cannot be combined with mode "direct" on entitlement[2]: the app gets the real device, so there is no camera node to configure`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assertValidationError(t, validateEntitlements(tt.in, "entitlement"), tt.wantErr)
		})
	}
}

func TestValidate_CameraEntriesClaimingTheSameCameraMustAgree(t *testing.T) {
	entry := func(format, stamp string, allowlist ...string) Entitlement {
		return Entitlement{Type: EntitlementCamera, Format: format, Stamp: stamp, Allowlist: allowlist}
	}
	const mustAgree = "; an app gets one node per camera, so entries that claim the same camera must agree"
	tests := []struct {
		name    string
		in      []Entitlement
		wantErr string
	}{
		{"neither sets a key", []Entitlement{entry("", ""), entry("", "")}, ""},
		{"both set the same values", []Entitlement{entry("native", "frame"), entry("native", "frame")}, ""},
		{"explicit default agrees with an unset key", []Entitlement{entry("pixels", "time"), entry("", "")}, ""},
		{
			"different settings for different cameras",
			[]Entitlement{entry("native", "", "/dev/video0", "Brio 101"), entry("", "frame", "/dev/video2")},
			"",
		},
		{
			"path and name of one camera are different strings",
			[]Entitlement{entry("native", "", "/dev/video0"), entry("", "", "video0")},
			"",
		},
		{
			"shared allowlist entry with the same settings",
			[]Entitlement{entry("native", "", "/dev/video0"), entry("native", "", "/dev/video0", "/dev/video2")},
			"",
		},
		{
			"neither has an allowlist, format differs",
			[]Entitlement{entry("", ""), entry("native", "")},
			`entitlement[0] and entitlement[1] claim the same camera (neither has an allowlist) but differ in format ("pixels" and "native")` + mustAgree,
		},
		{
			"camera and its video alias, stamp differs",
			[]Entitlement{{Type: EntitlementVideo, Stamp: "frame"}, {Type: EntitlementGPU}, entry("", "time")},
			`entitlement[0] and entitlement[2] claim the same camera (neither has an allowlist) but differ in stamp ("frame" and "time")` + mustAgree,
		},
		{
			"both settings differ",
			[]Entitlement{entry("native", "frame"), entry("", "")},
			`entitlement[0] and entitlement[1] claim the same camera (neither has an allowlist) but differ in format ("native" and "pixels") and stamp ("frame" and "time")` + mustAgree,
		},
		{
			"first has no allowlist",
			[]Entitlement{entry("", ""), entry("native", "", "/dev/video0")},
			`entitlement[0] and entitlement[1] claim the same camera (entitlement[0] has no allowlist, so it claims every camera) but differ in format ("pixels" and "native")` + mustAgree,
		},
		{
			"second has no allowlist",
			[]Entitlement{entry("", "frame", "/dev/video0"), {Type: EntitlementGPU}, {Type: EntitlementVideo}},
			`entitlement[0] and entitlement[2] claim the same camera (entitlement[2] has no allowlist, so it claims every camera) but differ in stamp ("frame" and "time")` + mustAgree,
		},
		{
			"allowlists share an entry",
			[]Entitlement{entry("native", "", "/dev/video0", "/dev/video2"), entry("", "", "/dev/video4", "/dev/video2")},
			`entitlement[0] and entitlement[1] claim the same camera (both allowlists contain "/dev/video2") but differ in format ("native" and "pixels")` + mustAgree,
		},
		{
			"third entry overlaps the first",
			[]Entitlement{entry("", "frame", "a"), entry("", "", "b"), entry("", "", "c", "a")},
			`entitlement[0] and entitlement[2] claim the same camera (both allowlists contain "a") but differ in stamp ("frame" and "time")` + mustAgree,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assertValidationError(t, validateEntitlements(tt.in, "entitlement"), tt.wantErr)
		})
	}
}

func TestValidate_CameraRulesApplyToTheEntitlementsAContainerGets(t *testing.T) {
	const noNode = ": the app gets the real device, so there is no camera node to configure"
	const mustAgree = "; an app gets one node per camera, so entries that claim the same camera must agree"
	tests := []struct {
		name     string
		topLevel string
		services map[string]string
		wantErr  string
	}{
		{
			"service agrees with the app",
			`[{"type": "camera", "format": "native"}]`,
			map[string]string{"vision": `[{"type": "video", "format": "native"}]`},
			"",
		},
		{
			"two services differ from each other but not from the app",
			`[{"type": "gpu"}]`,
			map[string]string{"vision": `[{"type": "camera", "format": "native"}]`, "viewer": `[{"type": "camera"}]`},
			"",
		},
		{
			"service sets a format the app's plain entry does not",
			`[{"type": "camera"}]`,
			map[string]string{"vision": `[{"type": "camera", "format": "native"}]`},
			`entitlement[0] and services["vision"].entitlement[0] claim the same camera (neither has an allowlist) but differ in format ("pixels" and "native")` + mustAgree,
		},
		{
			"service allowlist entry differs from the app's entry for every camera",
			`[{"type": "gpu"}, {"type": "camera", "stamp": "frame"}]`,
			map[string]string{"vision": `[{"type": "camera", "allowlist": ["/dev/video0"]}]`},
			`entitlement[1] and services["vision"].entitlement[0] claim the same camera (entitlement[1] has no allowlist, so it claims every camera) but differ in stamp ("frame" and "time")` + mustAgree,
		},
		{
			"app is direct and a service sets a format",
			`[{"type": "camera", "mode": "direct"}]`,
			map[string]string{"vision": `[{"type": "camera", "format": "native"}]`},
			`services["vision"].entitlement[0]: camera format cannot be combined with mode "direct" on entitlement[0]` + noNode,
		},
		{
			"service is direct and the app sets a stamp",
			`[{"type": "video", "stamp": "frame"}]`,
			map[string]string{"vision": `[{"type": "gpu"}, {"type": "camera", "mode": "direct"}]`},
			`entitlement[0]: video stamp cannot be combined with mode "direct" on services["vision"].entitlement[1]` + noNode,
		},
		{
			"conflict inside one service",
			`[]`,
			map[string]string{"vision": `[{"type": "camera"}, {"type": "camera", "format": "native"}]`},
			`services["vision"].entitlement[0] and services["vision"].entitlement[1] claim the same camera (neither has an allowlist) but differ in format ("pixels" and "native")` + mustAgree,
		},
	}
	for _, tt := range tests {
		var services []string
		for name, entitlements := range tt.services {
			services = append(services, `"`+name+`": {"context": "`+name+`", "entitlements": `+entitlements+`}`)
		}
		manifest := []byte(`{"appId": "com.example.app", "entitlements": ` + tt.topLevel + `, "services": {` + strings.Join(services, ", ") + `}}`)
		t.Run(tt.name, func(t *testing.T) {
			cfg, err := LoadFromBytes(manifest)
			if err != nil {
				t.Fatal(err)
			}
			assertValidationError(t, cfg.Validate(), tt.wantErr)
		})
		t.Run(tt.name+" as a compose companion", func(t *testing.T) {
			cfg, err := LoadFromBytes(manifest)
			if err != nil {
				t.Fatal(err)
			}
			assertValidationError(t, cfg.ValidateComposeCompanion(), tt.wantErr)
		})
	}
}

func TestContainerEntitlements(t *testing.T) {
	cfg := &AppConfig{Entitlements: []Entitlement{{Type: EntitlementGPU}, {Type: EntitlementCamera}}}
	svc := &ServiceConfig{Entitlements: []Entitlement{{Type: EntitlementCamera, Format: CameraFormatNative}}}
	want := []Entitlement{{Type: EntitlementGPU}, {Type: EntitlementCamera}, {Type: EntitlementCamera, Format: CameraFormatNative}}
	if got := cfg.ContainerEntitlements(svc); !reflect.DeepEqual(got, want) {
		t.Fatalf("ContainerEntitlements(svc) = %+v, want %+v", got, want)
	}
	if got := cfg.ContainerEntitlements(nil); !reflect.DeepEqual(got, cfg.Entitlements) {
		t.Fatalf("ContainerEntitlements(nil) = %+v, want %+v", got, cfg.Entitlements)
	}
}

func TestValidate_CameraManifestsFromBeforeNodeSettingsStillValidate(t *testing.T) {
	tests := []struct {
		name     string
		manifest string
	}{
		{"camera alone", `{"appId": "com.example.app", "entitlements": [{"type": "camera"}]}`},
		{"video alone", `{"appId": "com.example.app", "entitlements": [{"type": "video"}]}`},
		{"video plus camera", `{"appId": "com.example.app", "entitlements": [{"type": "video"}, {"type": "camera"}]}`},
		{"camera with allowlist", `{"appId": "com.example.app", "entitlements": [{"type": "camera", "allowlist": ["/dev/video0"]}]}`},
		{"camera direct", `{"appId": "com.example.app", "entitlements": [{"type": "camera", "mode": "direct"}]}`},
		{"camera direct plus video", `{"appId": "com.example.app", "entitlements": [{"type": "camera", "mode": "direct"}, {"type": "video"}]}`},
		{"two cameras", `{"appId": "com.example.app", "entitlements": [{"type": "camera"}, {"type": "camera"}]}`},
		{
			"overlapping allowlists",
			`{"appId": "com.example.app", "entitlements": [
				{"type": "camera", "allowlist": ["/dev/video0"]},
				{"type": "video", "allowlist": ["/dev/video0", "/dev/video2"]}
			]}`,
		},
		{
			"legacy mode, allowlist and network camera login",
			`{"appId": "com.example.app", "entitlements": [
				{"type": "camera", "mode": "legacy", "allowlist": ["/dev/video0"], "user": "admin", "password": "secret"},
				{"type": "video", "mode": "detect"}
			]}`,
		},
		{
			"camera on the app and on a service",
			`{"appId": "com.example.app", "entitlements": [{"type": "camera"}],
				"services": {"vision": {"context": "vision", "entitlements": [{"type": "video"}, {"type": "camera", "allowlist": ["/dev/video0"]}]}}}`,
		},
		{
			"direct app with a plain camera service",
			`{"appId": "com.example.app", "entitlements": [{"type": "camera", "mode": "direct"}],
				"services": {"vision": {"context": "vision", "entitlements": [{"type": "camera"}]}}}`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "wendy.json")
			if err := os.WriteFile(path, []byte(tt.manifest), 0o644); err != nil {
				t.Fatal(err)
			}
			cfg, err := LoadFromFile(path)
			if err != nil {
				t.Fatalf("LoadFromFile() rejected a manifest that loaded before format and stamp existed: %v", err)
			}
			if err := cfg.Validate(); err != nil {
				t.Fatalf("Validate() rejected a manifest that validated before format and stamp existed: %v", err)
			}
			for _, warning := range ValidateJSON([]byte(tt.manifest)) {
				if !strings.Contains(warning, "is deprecated") {
					t.Errorf("unexpected warning: %s", warning)
				}
			}
		})
	}
}

func assertValidationError(t *testing.T, err error, want string) {
	t.Helper()
	if want == "" {
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		return
	}
	if err == nil {
		t.Fatalf("no error, want %q", want)
	}
	if err.Error() != want {
		t.Fatalf("error = %q\n      want %q", err.Error(), want)
	}
}

func TestValidateJSON_CameraNodeKeysNoWarnings(t *testing.T) {
	for _, typ := range []string{"camera", "video"} {
		data := []byte(`{"appId": "com.example.app", "entitlements": [{"type": "` + typ + `", "format": "pixels", "stamp": "time"}]}`)
		if warnings := ValidateJSON(data); len(warnings) != typeWarnings(typ) {
			t.Fatalf("%s: ValidateJSON() = %v", typ, warnings)
		}
	}
}

func TestLoadFromBytes_CameraNodeKeys(t *testing.T) {
	cfg, err := LoadFromBytes([]byte(`{"appId": "com.example.app", "entitlements": [{"type": "camera", "format": "native", "stamp": "frame"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	want := Entitlement{Type: EntitlementCamera, Format: CameraFormatNative, Stamp: CameraStampFrame}
	if !reflect.DeepEqual(cfg.Entitlements[0], want) {
		t.Fatalf("entitlement = %+v, want %+v", cfg.Entitlements[0], want)
	}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestCameraNodeSettings(t *testing.T) {
	const path, name = "/dev/video0", "Brio 101"
	tests := []struct {
		name       string
		in         []Entitlement
		wantFormat string
		wantStamp  string
	}{
		{"no entitlements", nil, "pixels", "time"},
		{"no camera entry", []Entitlement{{Type: EntitlementGPU}}, "pixels", "time"},
		{"entry without keys", []Entitlement{{Type: EntitlementCamera}}, "pixels", "time"},
		{
			"entry without allowlist",
			[]Entitlement{{Type: EntitlementCamera, Format: "native", Stamp: "frame"}},
			"native", "frame",
		},
		{"format only", []Entitlement{{Type: EntitlementCamera, Format: "native"}}, "native", "time"},
		{"stamp only", []Entitlement{{Type: EntitlementCamera, Stamp: "frame"}}, "pixels", "frame"},
		{"video alias", []Entitlement{{Type: EntitlementVideo, Format: "native"}}, "native", "time"},
		{
			"allowlist names the device path",
			[]Entitlement{
				{Type: EntitlementCamera, Allowlist: []string{"/dev/video2"}, Stamp: "frame"},
				{Type: EntitlementCamera, Allowlist: []string{"/dev/video4", path}, Format: "native"},
			},
			"native", "time",
		},
		{
			"allowlist names the camera",
			[]Entitlement{
				{Type: EntitlementCamera, Allowlist: []string{"C920"}, Stamp: "frame"},
				{Type: EntitlementCamera, Allowlist: []string{name}, Format: "native"},
			},
			"native", "time",
		},
		{
			"allowlist match wins over an earlier entry without allowlist",
			[]Entitlement{
				{Type: EntitlementCamera, Stamp: "frame"},
				{Type: EntitlementCamera, Allowlist: []string{path}, Format: "native"},
			},
			"native", "time",
		},
		{
			"falls back to the entry without allowlist",
			[]Entitlement{
				{Type: EntitlementCamera, Allowlist: []string{"/dev/video2"}, Format: "native"},
				{Type: EntitlementCamera, Stamp: "frame"},
			},
			"pixels", "frame",
		},
		{
			"camera in no allowlist",
			[]Entitlement{{Type: EntitlementCamera, Allowlist: []string{"/dev/video2"}, Format: "native", Stamp: "frame"}},
			"pixels", "time",
		},
		{
			"direct entry with a matching allowlist is ignored",
			[]Entitlement{{Type: EntitlementCamera, Mode: "direct", Allowlist: []string{path}, Format: "native", Stamp: "frame"}},
			"pixels", "time",
		},
		{
			"direct entry without allowlist is ignored",
			[]Entitlement{
				{Type: EntitlementCamera, Mode: "direct", Format: "native"},
				{Type: EntitlementCamera, Allowlist: []string{name}, Stamp: "frame"},
			},
			"pixels", "frame",
		},
		{
			"other entitlement types are ignored",
			[]Entitlement{{Type: EntitlementNetwork, Allowlist: []string{path}, Format: "native", Stamp: "frame"}},
			"pixels", "time",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			format, stamp, _ := CameraNodeSettings(tt.in, CameraIdentity{Path: path, Name: name})
			if format != tt.wantFormat || stamp != tt.wantStamp {
				t.Fatalf("CameraNodeSettings() = (%q, %q), want (%q, %q)", format, stamp, tt.wantFormat, tt.wantStamp)
			}
		})
	}
}

func TestCameraNodeSettings_UnknownCameraIdentityMatchesNoAllowlist(t *testing.T) {
	in := []Entitlement{{Type: EntitlementCamera, Allowlist: []string{""}, Format: "native", Stamp: "frame"}}
	if format, stamp, _ := CameraNodeSettings(in, CameraIdentity{}); format != "pixels" || stamp != "time" {
		t.Fatalf("CameraNodeSettings() = (%q, %q), want the defaults", format, stamp)
	}
}

func TestCameraNodeSettings_DoesNotDependOnEntryOrder(t *testing.T) {
	const path, name = "/dev/video0", "Brio 101"
	tests := []struct {
		name       string
		in         []Entitlement
		wantFormat string
		wantStamp  string
	}{
		{
			"one camera named by name and by path: the path entry decides",
			[]Entitlement{
				{Type: EntitlementVideo, Allowlist: []string{name}, Format: "native"},
				{Type: EntitlementCamera, Allowlist: []string{path}},
			},
			"pixels", "time",
		},
		{
			"path entry decides over name entry and entry without allowlist",
			[]Entitlement{
				{Type: EntitlementCamera, Stamp: "frame"},
				{Type: EntitlementCamera, Allowlist: []string{name}, Stamp: "frame"},
				{Type: EntitlementCamera, Allowlist: []string{path}, Format: "native"},
			},
			"native", "time",
		},
		{
			"name entry decides over entry without allowlist",
			[]Entitlement{
				{Type: EntitlementCamera, Format: "native"},
				{Type: EntitlementVideo, Allowlist: []string{name}, Stamp: "frame"},
			},
			"pixels", "frame",
		},
		{
			"entry naming both path and name counts as naming the path",
			[]Entitlement{
				{Type: EntitlementCamera, Allowlist: []string{name}, Stamp: "frame"},
				{Type: EntitlementCamera, Allowlist: []string{name, path}, Format: "native"},
			},
			"native", "time",
		},
		{
			"deciding entries that disagree on one setting give its default",
			[]Entitlement{
				{Type: EntitlementCamera, Allowlist: []string{path}, Format: "native", Stamp: "frame"},
				{Type: EntitlementVideo, Allowlist: []string{path}, Stamp: "frame"},
				{Type: EntitlementCamera, Allowlist: []string{path}, Format: "native", Stamp: "frame"},
			},
			"pixels", "frame",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			for _, order := range entitlementOrders(tt.in) {
				format, stamp, _ := CameraNodeSettings(order, CameraIdentity{Path: path, Name: name})
				if format != tt.wantFormat || stamp != tt.wantStamp {
					t.Errorf("CameraNodeSettings(%+v) = (%q, %q), want (%q, %q)", order, format, stamp, tt.wantFormat, tt.wantStamp)
				}
			}
		})
	}
}

func entitlementOrders(entitlements []Entitlement) [][]Entitlement {
	if len(entitlements) <= 1 {
		return [][]Entitlement{slices.Clone(entitlements)}
	}
	var orders [][]Entitlement
	for i, first := range entitlements {
		rest := slices.Delete(slices.Clone(entitlements), i, i+1)
		for _, order := range entitlementOrders(rest) {
			orders = append(orders, append([]Entitlement{first}, order...))
		}
	}
	return orders
}

// The identifiers `wendy device camera list --json` and a campaign file show
// for one camera.
var c920 = CameraIdentity{
	Path:     "/dev/video0",
	StableID: "by-id:usb-046d_HD_Pro_Webcam_C920-video-index0",
	Name:     "HD Pro Webcam C920: HD Pro Webc",
}

func TestCameraNodeSettings_AnAllowlistNamesACameraByEveryIdentifierItIsShownBy(t *testing.T) {
	onPort := CameraIdentity{Path: "/dev/video0", StableID: "by-path:pci-0000:00:14.0-usb-0:2:1.0-video-index0", Name: c920.Name}
	tests := []struct {
		name   string
		camera CameraIdentity
		entry  string
		named  bool
	}{
		{"device path", c920, "/dev/video0", true},
		{"device path as a campaign file writes it", c920, "v4l2:/dev/video0", true},
		{"stable id as the camera list prints it", c920, "by-id:usb-046d_HD_Pro_Webcam_C920-video-index0", true},
		{"stable id without its by-id prefix", c920, "usb-046d_HD_Pro_Webcam_C920-video-index0", true},
		{"name", c920, "HD Pro Webcam C920: HD Pro Webc", true},
		{"port stable id as the camera list prints it", onPort, "by-path:pci-0000:00:14.0-usb-0:2:1.0-video-index0", true},
		{"port stable id without its prefix", onPort, "pci-0000:00:14.0-usb-0:2:1.0-video-index0", false},
		{"another device path", c920, "/dev/video2", false},
		{"another device path as a campaign file writes it", c920, "v4l2:/dev/video2", false},
		{"another camera's stable id", c920, "by-id:usb-046d_HD_Pro_Webcam_C920-video-index1", false},
		{"part of the stable id", c920, "usb-046d_HD_Pro_Webcam_C920", false},
		{"name with the path's prefix", c920, "v4l2:HD Pro Webcam C920: HD Pro Webc", false},
		{"path with the stable id's prefix", c920, "by-id:/dev/video0", false},
		{"prefix alone on a camera without a path", CameraIdentity{Name: c920.Name}, "v4l2:", false},
		{"prefix alone on a camera whose stable id is only the prefix", CameraIdentity{StableID: "by-id:"}, "", false},
		{"empty entry on a camera without a stable id", CameraIdentity{Path: "/dev/video0"}, "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			in := []Entitlement{{Type: EntitlementCamera, Allowlist: []string{"/dev/video9", tt.entry}, Format: "native", Stamp: "frame"}}
			wantFormat, wantStamp := "pixels", "time"
			if tt.named {
				wantFormat, wantStamp = "native", "frame"
			}
			format, stamp, conflicts := CameraNodeSettings(in, tt.camera)
			if format != wantFormat || stamp != wantStamp || len(conflicts) != 0 {
				t.Fatalf("CameraNodeSettings() = (%q, %q, %q), want (%q, %q) and no conflict", format, stamp, conflicts, wantFormat, wantStamp)
			}
		})
	}
}

func TestCameraNodeSettings_TheMostSpecificIdentifierDecidesInAnyEntryOrder(t *testing.T) {
	const bareStableID = "usb-046d_HD_Pro_Webcam_C920-video-index0"
	entry := func(format, stamp string, allowlist ...string) Entitlement {
		return Entitlement{Type: EntitlementCamera, Format: format, Stamp: stamp, Allowlist: allowlist}
	}
	tests := []struct {
		name          string
		in            []Entitlement
		wantFormat    string
		wantStamp     string
		wantConflicts []string
	}{
		{
			"the spellings of one identifier agree",
			[]Entitlement{
				entry("native", "frame", c920.StableID), entry("native", "frame", bareStableID),
				entry("native", "frame", c920.Path), entry("native", "frame", "v4l2:"+c920.Path),
				entry("native", "frame", c920.Name), entry("native", "frame"),
			},
			"native", "frame", nil,
		},
		{
			"stable id decides over device path",
			[]Entitlement{entry("native", "", c920.StableID), entry("h264", "", c920.Path)},
			"native", "time",
			[]string{`format is native (asked by "by-id:usb-046d_HD_Pro_Webcam_C920-video-index0"); passed over: "/dev/video0" asks for h264`},
		},
		{
			"stable id without its prefix decides over the campaign spelling of the path",
			[]Entitlement{entry("", "frame", bareStableID), entry("", "", "v4l2:"+c920.Path)},
			"pixels", "frame",
			[]string{`stamp is frame (asked by "usb-046d_HD_Pro_Webcam_C920-video-index0"); passed over: "v4l2:/dev/video0" asks for time`},
		},
		{
			"device path as a campaign file writes it decides over name",
			[]Entitlement{entry("h264", "", "v4l2:"+c920.Path), entry("native", "frame", c920.Name)},
			"h264", "time",
			[]string{
				`format is h264 (asked by "v4l2:/dev/video0"); passed over: "HD Pro Webcam C920: HD Pro Webc" asks for native`,
				`stamp is time (asked by "v4l2:/dev/video0"); passed over: "HD Pro Webcam C920: HD Pro Webc" asks for frame`,
			},
		},
		{
			"stable id decides over path, name and an entry without an allowlist",
			[]Entitlement{entry("native", "", c920.StableID), entry("h264", "", c920.Path), entry("h264", "", c920.Name), entry("", "")},
			"native", "time",
			[]string{`format is native (asked by "by-id:usb-046d_HD_Pro_Webcam_C920-video-index0"); passed over: "/dev/video0" asks for h264, ` +
				`"HD Pro Webcam C920: HD Pro Webc" asks for h264, an entry without an allowlist asks for pixels`},
		},
		{
			"entry naming both path and stable id counts as naming the stable id",
			[]Entitlement{entry("native", "", c920.Path, bareStableID), entry("h264", "", "v4l2:"+c920.Path)},
			"native", "time",
			[]string{`format is native (asked by "usb-046d_HD_Pro_Webcam_C920-video-index0"); passed over: "v4l2:/dev/video0" asks for h264`},
		},
		{
			"the two spellings of the stable id that disagree give the default",
			[]Entitlement{entry("native", "frame", c920.StableID), entry("h264", "frame", bareStableID), entry("h264", "", c920.Path)},
			"pixels", "frame",
			[]string{
				`format falls back to pixels: the entries deciding for this camera disagree (` +
					`"by-id:usb-046d_HD_Pro_Webcam_C920-video-index0" asks for native, "usb-046d_HD_Pro_Webcam_C920-video-index0" asks for h264)`,
				`stamp is frame (asked by "by-id:usb-046d_HD_Pro_Webcam_C920-video-index0", "usb-046d_HD_Pro_Webcam_C920-video-index0"); ` +
					`passed over: "/dev/video0" asks for time`,
			},
		},
		{
			"the two spellings of the path that disagree give the default",
			[]Entitlement{entry("native", "", c920.Path), entry("native", "frame", "v4l2:"+c920.Path)},
			"native", "time",
			[]string{`stamp falls back to time: the entries deciding for this camera disagree ("/dev/video0" asks for time, "v4l2:/dev/video0" asks for frame)`},
		},
		{
			"entries without an allowlist that disagree give the default",
			[]Entitlement{entry("native", ""), entry("h264", "")},
			"pixels", "time",
			[]string{`format falls back to pixels: the entries deciding for this camera disagree (` +
				`an entry without an allowlist asks for h264, an entry without an allowlist asks for native)`},
		},
		{
			"entries naming other cameras take no part",
			[]Entitlement{entry("native", "", c920.StableID), entry("h264", "frame", "/dev/video2", "by-id:usb-Other-video-index0", "Brio 101")},
			"native", "time", nil,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			for _, order := range entitlementOrders(tt.in) {
				format, stamp, conflicts := CameraNodeSettings(order, c920)
				if format != tt.wantFormat || stamp != tt.wantStamp || !slices.Equal(conflicts, tt.wantConflicts) {
					t.Fatalf("CameraNodeSettings(%+v) = (%q, %q)\nconflicts %q\nwant (%q, %q)\nconflicts %q",
						order, format, stamp, conflicts, tt.wantFormat, tt.wantStamp, tt.wantConflicts)
				}
			}
		})
	}
}

func TestCameraNodeSettings_UnknownValuesResolveToTheDefaults(t *testing.T) {
	tests := []struct {
		name          string
		format, stamp string
		wantFormat    string
		wantStamp     string
	}{
		{"format from a newer command line", "vp8", "frame", "pixels", "frame"},
		{"h264", "h264", "frame", "h264", "frame"},
		{"unknown stamp", "native", "sequence", "native", "time"},
		{"both unknown", "H264", "Frame", "pixels", "time"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			in := []Entitlement{ParseEntitlementAnnotation(EntitlementCamera, "format="+tt.format+",stamp="+tt.stamp)}
			format, stamp, _ := CameraNodeSettings(in, CameraIdentity{Path: "/dev/video0", Name: "Brio 101"})
			if format != tt.wantFormat || stamp != tt.wantStamp {
				t.Fatalf("CameraNodeSettings() = (%q, %q), want (%q, %q)", format, stamp, tt.wantFormat, tt.wantStamp)
			}
		})
	}
}

func TestCameraNodeDefaultsAreTheFirstListedValues(t *testing.T) {
	if CameraFormats[0] != CameraFormatPixels || CameraStamps[0] != CameraStampTime {
		t.Fatalf("defaults are (%q, %q), want (pixels, time)", CameraFormats[0], CameraStamps[0])
	}
}

func cameraSchemaVariants(t *testing.T) map[string]map[string]any {
	t.Helper()
	var schema map[string]any
	if err := json.Unmarshal([]byte(SchemaJSON), &schema); err != nil {
		t.Fatalf("schema is not valid JSON: %v", err)
	}
	variants := make(map[string]map[string]any)
	branches, _ := defOf(t, schema, "entitlement")["oneOf"].([]any)
	for _, raw := range branches {
		branch, _ := raw.(map[string]any)
		props, _ := branch["properties"].(map[string]any)
		typeProp, _ := props["type"].(map[string]any)
		if typ, _ := typeProp["const"].(string); typ == EntitlementCamera || typ == EntitlementVideo {
			variants[typ] = props
		}
	}
	if len(variants) != 2 {
		t.Fatalf("found %d camera/video variants, want 2", len(variants))
	}
	return variants
}

func TestSchema_CameraNodeEnumsMatchValidation(t *testing.T) {
	for typ, props := range cameraSchemaVariants(t) {
		for key, want := range map[string][]string{"format": CameraFormats, "stamp": CameraStamps} {
			prop, _ := props[key].(map[string]any)
			raw, _ := prop["enum"].([]any)
			got := make([]string, len(raw))
			for i, v := range raw {
				got[i], _ = v.(string)
			}
			if !reflect.DeepEqual(got, want) {
				t.Errorf("%s %s enum = %v, want %v", typ, key, got, want)
			}
			if prop["default"] != want[0] {
				t.Errorf("%s %s default = %v, want %q", typ, key, prop["default"], want[0])
			}
		}
	}
}

func TestSchema_ValidatesCameraNodeKeys(t *testing.T) {
	var schemaDoc map[string]any
	if err := json.Unmarshal([]byte(SchemaJSON), &schemaDoc); err != nil {
		t.Fatalf("schema is not valid JSON: %v", err)
	}
	// Only the entitlement definition is compiled: other parts of the schema use
	// lookahead patterns that Go's regexp engine cannot parse.
	compiler := jsonschema.NewCompiler()
	if err := compiler.AddResource("entitlement.schema.json", defOf(t, schemaDoc, "entitlement")); err != nil {
		t.Fatal(err)
	}
	schema, err := compiler.Compile("entitlement.schema.json")
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name        string
		entitlement string
		valid       bool
	}{
		{"documented example", `{"type": "camera", "format": "pixels", "stamp": "time"}`, true},
		{"native and frame", `{"type": "camera", "format": "native", "stamp": "frame"}`, true},
		{"video alias", `{"type": "video", "format": "native", "stamp": "frame"}`, true},
		{"neither key", `{"type": "camera"}`, true},
		{"h264", `{"type": "camera", "format": "h264"}`, true},
		{"h264 on the video alias", `{"type": "video", "format": "h264"}`, true},
		{"unknown format", `{"type": "camera", "format": "vp8"}`, false},
		{"unknown stamp", `{"type": "camera", "stamp": "sequence"}`, false},
		{"unknown format on the video alias", `{"type": "video", "format": "raw"}`, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var entitlement any
			if err := json.Unmarshal([]byte(tt.entitlement), &entitlement); err != nil {
				t.Fatal(err)
			}
			err := schema.Validate(entitlement)
			if tt.valid && err != nil {
				t.Fatalf("schema rejected %s: %v", tt.entitlement, err)
			}
			if !tt.valid && err == nil {
				t.Fatalf("schema accepted %s", tt.entitlement)
			}
		})
	}
}

func TestCameraFormatsServedFor_AskedFormatThenPicturesThenTheCamerasOwnFrames(t *testing.T) {
	for asked, want := range map[string][]string{
		CameraFormatPixels: {CameraFormatPixels, CameraFormatNative},
		CameraFormatNative: {CameraFormatNative, CameraFormatPixels},
		CameraFormatH264:   {CameraFormatH264, CameraFormatPixels, CameraFormatNative},
	} {
		if got := CameraFormatsServedFor(asked); !slices.Equal(got, want) {
			t.Errorf("served for %s = %v, want %v", asked, got, want)
		}
	}
}
