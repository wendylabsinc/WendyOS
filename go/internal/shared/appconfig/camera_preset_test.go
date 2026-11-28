package appconfig

import (
	"encoding/json"
	"reflect"
	"slices"
	"testing"

	"github.com/santhosh-tekuri/jsonschema/v6"
)

func h264At(preset string, allowlist ...string) Entitlement {
	return Entitlement{Type: EntitlementCamera, Format: CameraFormatH264, Preset: preset, Allowlist: allowlist}
}

func TestValidate_CameraPreset(t *testing.T) {
	const noNode = ": the app gets the real device, so there is no camera node to configure"
	tests := []struct {
		name    string
		in      []Entitlement
		wantErr string
	}{
		{"h264 without a preset", []Entitlement{h264At("")}, ""},
		{"h264 with a preset", []Entitlement{h264At("veryfast")}, ""},
		{"video alias", []Entitlement{{Type: EntitlementVideo, Format: "h264", Preset: "medium"}}, ""},
		{
			"unknown preset",
			[]Entitlement{h264At("slow")},
			`entitlement[0]: camera preset must be one of "ultrafast", "superfast", "veryfast", "faster", "fast", "medium"; got "slow"`,
		},
		{
			"preset is case sensitive, on the video alias",
			[]Entitlement{{Type: EntitlementGPU}, {Type: EntitlementVideo, Format: "h264", Preset: "Medium"}},
			`entitlement[1]: video preset must be one of "ultrafast", "superfast", "veryfast", "faster", "fast", "medium"; got "Medium"`,
		},
		{
			"preset without a format",
			[]Entitlement{{Type: EntitlementCamera, Preset: "veryfast"}},
			`entitlement[0]: camera preset applies only to format "h264": a node of format "pixels" runs no encoder`,
		},
		{
			"preset with native, on the video alias",
			[]Entitlement{{Type: EntitlementGPU}, {Type: EntitlementVideo, Format: "native", Preset: "veryfast"}},
			`entitlement[1]: video preset applies only to format "h264": a node of format "native" runs no encoder`,
		},
		{
			"preset with direct",
			[]Entitlement{{Type: EntitlementCamera, Mode: "direct", Preset: "veryfast"}},
			`entitlement[0]: camera preset cannot be combined with mode "direct"` + noNode,
		},
		{
			"preset on another entry than the direct one",
			[]Entitlement{{Type: EntitlementCamera, Mode: "direct"}, {Type: EntitlementVideo, Preset: "fast"}},
			`entitlement[1]: video preset cannot be combined with mode "direct" on entitlement[0]` + noNode,
		},
		{
			"h264 with a preset beside a direct entry",
			[]Entitlement{h264At("fast"), {Type: EntitlementCamera, Mode: "direct"}},
			`entitlement[0]: camera format cannot be combined with mode "direct" on entitlement[1]` + noNode,
		},
	}
	for _, name := range EncodePresets {
		tests = append(tests, struct {
			name    string
			in      []Entitlement
			wantErr string
		}{"accepts " + name, []Entitlement{h264At(name)}, ""})
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assertValidationError(t, validateEntitlements(tt.in, "entitlement"), tt.wantErr)
		})
	}
}

func TestValidate_CameraEntriesClaimingTheSameCameraMustAgreeOnThePreset(t *testing.T) {
	const mustAgree = "; an app gets one node per camera, so entries that claim the same camera must agree"
	tests := []struct {
		name    string
		in      []Entitlement
		wantErr string
	}{
		{"the same preset", []Entitlement{h264At("veryfast"), h264At("veryfast")}, ""},
		{"neither states one", []Entitlement{h264At(""), h264At("")}, ""},
		{"different presets for different cameras", []Entitlement{h264At("veryfast", "/dev/video0"), h264At("medium", "/dev/video2")}, ""},
		{
			"different presets",
			[]Entitlement{h264At("veryfast"), h264At("fast")},
			`entitlement[0] and entitlement[1] claim the same camera (neither has an allowlist) but differ in preset ("veryfast" and "fast")` + mustAgree,
		},
		{
			"one states a preset and the other none",
			[]Entitlement{h264At("veryfast", "/dev/video0"), h264At("")},
			`entitlement[0] and entitlement[1] claim the same camera (entitlement[1] has no allowlist, so it claims every camera) but differ in preset ("veryfast" and unset)` + mustAgree,
		},
		{
			"one camera spelled differently",
			[]Entitlement{h264At("veryfast", "/dev/video0"), h264At("medium", "v4l2:/dev/video0")},
			`entitlement[0] and entitlement[1] claim the same camera ("/dev/video0" and "v4l2:/dev/video0" name one camera) but differ in preset ("veryfast" and "medium")` + mustAgree,
		},
		{
			"one stable id spelled differently",
			[]Entitlement{h264At("", "usb-046d_C920-video-index0"), h264At("faster", "by-id:usb-046d_C920-video-index0")},
			`entitlement[0] and entitlement[1] claim the same camera ("usb-046d_C920-video-index0" and "by-id:usb-046d_C920-video-index0" name one camera) but differ in preset (unset and "faster")` + mustAgree,
		},
		{
			"stamp and preset differ",
			[]Entitlement{{Type: EntitlementCamera, Format: "h264", Stamp: "frame", Preset: "fast"}, h264At("medium")},
			`entitlement[0] and entitlement[1] claim the same camera (neither has an allowlist) but differ in stamp ("frame" and "time") and preset ("fast" and "medium")` + mustAgree,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assertValidationError(t, validateEntitlements(tt.in, "entitlement"), tt.wantErr)
		})
	}
}

func TestCameraNodeSettings_ThePresetIsSettledLikeFormatAndStamp(t *testing.T) {
	tests := []struct {
		name          string
		in            []Entitlement
		want          EncodePreset
		wantConflicts []string
	}{
		{"no camera entry", []Entitlement{{Type: EntitlementGPU}}, EncodePresetUnset, nil},
		{"none stated", []Entitlement{h264At("")}, EncodePresetUnset, nil},
		{"stated", []Entitlement{h264At("veryfast")}, EncodePresetVeryfast, nil},
		{"stated for this camera", []Entitlement{h264At("medium", c920.Path)}, EncodePresetMedium, nil},
		{"stated for another camera", []Entitlement{h264At("medium", "/dev/video9")}, EncodePresetUnset, nil},
		{"a direct entry configures no node", []Entitlement{{Type: EntitlementCamera, Mode: CameraModeDirect, Format: "h264", Preset: "fast"}}, EncodePresetUnset, nil},
		{"a name this build does not know states none", []Entitlement{h264At("placebo")}, EncodePresetUnset, nil},
		{
			"the most specific entry decides",
			[]Entitlement{h264At("fast", c920.StableID), h264At("medium", c920.Path), h264At("")},
			EncodePresetFast,
			[]string{`preset is fast (asked by allowlist entry "by-id:usb-046d_HD_Pro_Webcam_C920-video-index0"); ` +
				`passed over: allowlist entry "/dev/video0" asks for medium, an entry without an allowlist asks for no preset`},
		},
		{
			"the most specific entry states none",
			[]Entitlement{h264At("", c920.Path), h264At("veryfast")},
			EncodePresetUnset,
			[]string{`preset is no preset (asked by allowlist entry "/dev/video0"); passed over: an entry without an allowlist asks for veryfast`},
		},
		{
			"the deciding entries disagree",
			[]Entitlement{h264At("fast", c920.Path), h264At("veryfast", "v4l2:"+c920.Path)},
			EncodePresetUnset,
			[]string{`preset falls back to no preset: the entries deciding for this camera disagree (allowlist entry "/dev/video0" asks for fast, allowlist entry "v4l2:/dev/video0" asks for veryfast)`},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			for _, order := range entitlementOrders(tt.in) {
				_, _, preset, conflicts := CameraNodeSettings(order, c920)
				if preset != tt.want || !slices.Equal(conflicts, tt.wantConflicts) {
					t.Fatalf("CameraNodeSettings(%+v) preset = %q\nconflicts %q\nwant %q\nconflicts %q", order, preset, conflicts, tt.want, tt.wantConflicts)
				}
			}
		})
	}
}

func TestCameraNodeSettingsByPathOrName_SettlesThePresetToo(t *testing.T) {
	in := []Entitlement{h264At("fast", c920.StableID), h264At("medium", c920.Name)}
	if _, _, preset, conflicts := CameraNodeSettingsByPathOrName(in, c920.Path, c920.Name); preset != EncodePresetMedium || len(conflicts) != 0 {
		t.Fatalf("preset = %q, conflicts %q; want medium from the entry that agent matches, and no conflict", preset, conflicts)
	}
}

func TestLoadFromBytes_CameraPreset(t *testing.T) {
	for _, typ := range []string{EntitlementCamera, EntitlementVideo} {
		data := []byte(`{"appId": "com.example.app", "entitlements": [{"type": "` + typ + `", "format": "h264", "preset": "veryfast"}]}`)
		cfg, err := LoadFromBytes(data)
		if err != nil {
			t.Fatal(err)
		}
		want := Entitlement{Type: typ, Format: CameraFormatH264, Preset: "veryfast"}
		if !reflect.DeepEqual(cfg.Entitlements[0], want) {
			t.Fatalf("entitlement = %+v, want %+v", cfg.Entitlements[0], want)
		}
		if err := cfg.Validate(); err != nil {
			t.Fatal(err)
		}
		if warnings := ValidateJSON(data); len(warnings) != typeWarnings(typ) {
			t.Fatalf("%s: ValidateJSON() = %v", typ, warnings)
		}
	}
}

func TestValidateJSON_PresetIsUnknownOutsideCameraEntries(t *testing.T) {
	data := []byte(`{"appId": "com.example.app", "entitlements": [{"type": "gpu", "preset": "veryfast"}]}`)
	if warnings := ValidateJSON(data); len(warnings) == 0 {
		t.Fatal("ValidateJSON() accepted preset on a gpu entry without a warning")
	}
}

func TestEntitlementAnnotation_CarriesThePreset(t *testing.T) {
	for _, typ := range []string{EntitlementCamera, EntitlementVideo} {
		in := Entitlement{Type: typ, Format: CameraFormatH264, Stamp: CameraStampFrame, Preset: "veryfast", Allowlist: []string{"/dev/video0", "Brio 101"}}
		value := EntitlementAnnotationValue(in)
		if want := "format=h264,stamp=frame,preset=veryfast,allowlist=/dev/video0,Brio 101"; value != want {
			t.Fatalf("%s: annotation = %q, want %q", typ, value, want)
		}
		if got := ParseEntitlementAnnotation(typ, value); !reflect.DeepEqual(got, in) {
			t.Fatalf("%s: parsed %+v, want %+v", typ, got, in)
		}
	}
	if got := ParseEntitlementAnnotation(EntitlementGPU, "preset=veryfast"); got.Preset != "" {
		t.Fatalf("a gpu label gave a preset: %+v", got)
	}
	if value := EntitlementAnnotationValue(Entitlement{Type: EntitlementGPU, Preset: "veryfast"}); value != "" {
		t.Fatalf("a gpu entry's annotation carries %q", value)
	}
}

func TestSchema_CameraPresetEnumMatchesValidation(t *testing.T) {
	for typ, props := range cameraSchemaVariants(t) {
		prop, _ := props["preset"].(map[string]any)
		raw, _ := prop["enum"].([]any)
		got := make([]string, len(raw))
		for i, v := range raw {
			got[i], _ = v.(string)
		}
		if !reflect.DeepEqual(got, EncodePresets) {
			t.Errorf("%s preset enum = %v, want %v", typ, got, EncodePresets)
		}
		if _, defaulted := prop["default"]; defaulted {
			t.Errorf("%s preset has a schema default; an entry without the key states no preset", typ)
		}
	}
}

func TestSchema_ValidatesCameraPreset(t *testing.T) {
	var schemaDoc map[string]any
	if err := json.Unmarshal([]byte(SchemaJSON), &schemaDoc); err != nil {
		t.Fatalf("schema is not valid JSON: %v", err)
	}
	compiler := jsonschema.NewCompiler()
	if err := compiler.AddResource("entitlement.schema.json", defOf(t, schemaDoc, "entitlement")); err != nil {
		t.Fatal(err)
	}
	schema, err := compiler.Compile("entitlement.schema.json")
	if err != nil {
		t.Fatal(err)
	}
	for entitlement, valid := range map[string]bool{
		`{"type": "camera", "format": "h264", "preset": "veryfast"}`: true,
		`{"type": "video", "format": "h264", "preset": "medium"}`:    true,
		`{"type": "camera", "format": "h264", "preset": "slow"}`:     false,
		`{"type": "video", "format": "h264", "preset": "placebo"}`:   false,
		`{"type": "gpu", "preset": "veryfast"}`:                      false,
	} {
		var doc any
		if err := json.Unmarshal([]byte(entitlement), &doc); err != nil {
			t.Fatal(err)
		}
		if err := schema.Validate(doc); (err == nil) != valid {
			t.Errorf("schema.Validate(%s) = %v, want valid %v", entitlement, err, valid)
		}
	}
}
