package appconfig

import (
	"encoding/json"
	"reflect"
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
		{"format only", []Entitlement{cam("native", "")}, ""},
		{"stamp only", []Entitlement{cam("", "frame")}, ""},
		{"video alias", []Entitlement{{Type: EntitlementVideo, Format: "native", Stamp: "frame"}}, ""},
		{"shared mode other than direct", []Entitlement{{Type: EntitlementCamera, Mode: "legacy", Format: "native"}}, ""},
		{"direct without node keys", []Entitlement{{Type: EntitlementCamera, Mode: "direct"}}, ""},
		{
			"unknown format",
			[]Entitlement{cam("h264", "")},
			`entitlement[0]: camera format must be one of "pixels", "native"; got "h264"`,
		},
		{
			"format is case sensitive",
			[]Entitlement{cam("Native", "")},
			`entitlement[0]: camera format must be one of "pixels", "native"; got "Native"`,
		},
		{
			"unknown stamp names its index",
			[]Entitlement{{Type: EntitlementGPU}, cam("", "sequence")},
			`entitlement[1]: camera stamp must be one of "time", "frame"; got "sequence"`,
		},
		{
			"unknown format on the video alias",
			[]Entitlement{{Type: EntitlementVideo, Format: "raw"}},
			`entitlement[0]: video format must be one of "pixels", "native"; got "raw"`,
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

func TestValidate_CameraClaimErrorNamesTheServicePrefix(t *testing.T) {
	cfg := &AppConfig{
		AppID: "com.example.app",
		Services: map[string]*ServiceConfig{
			"vision": {
				Context:      "vision",
				Entitlements: []Entitlement{{Type: EntitlementCamera}, {Type: EntitlementCamera, Format: "native"}},
			},
		},
	}
	err := cfg.Validate()
	if err == nil || !strings.Contains(err.Error(), `services["vision"].entitlement[0] and services["vision"].entitlement[1] claim the same camera`) {
		t.Fatalf("Validate() = %v, want an error naming both service entries", err)
	}
}

func TestValidate_CameraManifestsFromBeforeNodeSettingsStillValidate(t *testing.T) {
	tests := []struct {
		name string
		in   []Entitlement
	}{
		{"camera alone", []Entitlement{{Type: EntitlementCamera}}},
		{"video alone", []Entitlement{{Type: EntitlementVideo}}},
		{"video plus camera", []Entitlement{{Type: EntitlementVideo}, {Type: EntitlementCamera}}},
		{"camera with allowlist", []Entitlement{{Type: EntitlementCamera, Allowlist: []string{"/dev/video0"}}}},
		{"camera direct", []Entitlement{{Type: EntitlementCamera, Mode: "direct"}}},
		{"camera direct plus video", []Entitlement{{Type: EntitlementCamera, Mode: "direct"}, {Type: EntitlementVideo}}},
		{"two cameras", []Entitlement{{Type: EntitlementCamera}, {Type: EntitlementCamera}}},
		{
			"overlapping allowlists",
			[]Entitlement{{Type: EntitlementCamera, Allowlist: []string{"/dev/video0"}}, {Type: EntitlementVideo, Allowlist: []string{"/dev/video0", "/dev/video2"}}},
		},
		{
			"legacy mode, allowlist and network camera login",
			[]Entitlement{{Type: EntitlementCamera, Mode: "legacy", Allowlist: []string{"/dev/video0"}, User: "admin", Password: "secret"}, {Type: EntitlementVideo, Mode: "detect"}},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := &AppConfig{AppID: "com.example.app", Entitlements: tt.in}
			if err := cfg.Validate(); err != nil {
				t.Fatalf("Validate() rejected a manifest that validated before format and stamp existed: %v", err)
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
			format, stamp := CameraNodeSettings(tt.in, path, name)
			if format != tt.wantFormat || stamp != tt.wantStamp {
				t.Fatalf("CameraNodeSettings() = (%q, %q), want (%q, %q)", format, stamp, tt.wantFormat, tt.wantStamp)
			}
		})
	}
}

func TestCameraNodeSettings_UnknownCameraIdentityMatchesNoAllowlist(t *testing.T) {
	in := []Entitlement{{Type: EntitlementCamera, Allowlist: []string{""}, Format: "native", Stamp: "frame"}}
	if format, stamp := CameraNodeSettings(in, "", ""); format != "pixels" || stamp != "time" {
		t.Fatalf("CameraNodeSettings() = (%q, %q), want the defaults", format, stamp)
	}
}

func TestCameraNodeSettings_UnknownValuesResolveToTheDefaults(t *testing.T) {
	tests := []struct {
		name          string
		format, stamp string
		wantFormat    string
		wantStamp     string
	}{
		{"format from a newer command line", "h264", "frame", "pixels", "frame"},
		{"unknown stamp", "native", "sequence", "native", "time"},
		{"both unknown", "H264", "Frame", "pixels", "time"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			in := []Entitlement{ParseEntitlementAnnotation(EntitlementCamera, "format="+tt.format+",stamp="+tt.stamp)}
			format, stamp := CameraNodeSettings(in, "/dev/video0", "Brio 101")
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
		{"unknown format", `{"type": "camera", "format": "h264"}`, false},
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
