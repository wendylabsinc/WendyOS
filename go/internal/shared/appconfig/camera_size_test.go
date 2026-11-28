package appconfig

import (
	"encoding/json"
	"reflect"
	"slices"
	"testing"

	"github.com/santhosh-tekuri/jsonschema/v6"
)

func camAt(size string, allowlist ...string) Entitlement {
	return Entitlement{Type: EntitlementCamera, Size: size, Allowlist: allowlist}
}

func TestParseCameraSize(t *testing.T) {
	for value, want := range map[string]CameraSize{
		"1920x1080": {1920, 1080},
		"640x480":   {640, 480},
		"1x1":       {1, 1},
	} {
		if got, ok := ParseCameraSize(value); !ok || got != want {
			t.Errorf("ParseCameraSize(%q) = %v, %v; want %v", value, got, ok, want)
		}
		if want.String() != value {
			t.Errorf("%v.String() = %q, want %q", want, want.String(), value)
		}
	}
	for _, value := range []string{"", "1920", "1920x", "x1080", "0x1080", "1920x0", "-1920x1080", "+1920x1080", "1920X1080",
		"1920 x 1080", "1920x1080x3", "01920x1080", "1920.0x1080", "wide", "100000x1080"} {
		if got, ok := ParseCameraSize(value); ok || got != (CameraSize{}) {
			t.Errorf("ParseCameraSize(%q) = %v, %v; want it refused", value, got, ok)
		}
	}
	if got := (CameraSize{}).String(); got != "" {
		t.Errorf("the zero size reads %q, want nothing", got)
	}
}

func TestValidate_CameraSize(t *testing.T) {
	const noNode = ": the app gets the real device, so there is no camera node to configure"
	tests := []struct {
		name    string
		in      []Entitlement
		wantErr string
	}{
		{"no size", []Entitlement{camAt("")}, ""},
		{"a size", []Entitlement{camAt("1920x1080")}, ""},
		{"with every other node key", []Entitlement{{Type: EntitlementCamera, Format: "h264", Stamp: "frame", Preset: "fast", Size: "1920x1080"}}, ""},
		{"video alias", []Entitlement{{Type: EntitlementVideo, Size: "640x480"}}, ""},
		{
			"not a size",
			[]Entitlement{camAt("1080p")},
			`entitlement[0]: camera size must be WxH in pixels, for example "1920x1080"; got "1080p"`,
		},
		{
			"a zero height, on the video alias",
			[]Entitlement{{Type: EntitlementGPU}, {Type: EntitlementVideo, Size: "1920x0"}},
			`entitlement[1]: video size must be WxH in pixels, for example "1920x1080"; got "1920x0"`,
		},
		{
			"size with direct",
			[]Entitlement{{Type: EntitlementCamera, Mode: "direct", Size: "1920x1080"}},
			`entitlement[0]: camera size cannot be combined with mode "direct"` + noNode,
		},
		{
			"size on another entry than the direct one",
			[]Entitlement{{Type: EntitlementCamera, Mode: "direct"}, {Type: EntitlementVideo, Size: "1920x1080"}},
			`entitlement[1]: video size cannot be combined with mode "direct" on entitlement[0]` + noNode,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assertValidationError(t, validateEntitlements(tt.in, "entitlement"), tt.wantErr)
		})
	}
}

func TestValidate_CameraEntriesClaimingTheSameCameraMustAgreeOnTheSize(t *testing.T) {
	const mustAgree = "; an app gets one node per camera, so entries that claim the same camera must agree"
	tests := []struct {
		name    string
		in      []Entitlement
		wantErr string
	}{
		{"the same size", []Entitlement{camAt("1920x1080"), camAt("1920x1080")}, ""},
		{"neither states one", []Entitlement{camAt(""), camAt("")}, ""},
		{"different sizes for different cameras", []Entitlement{camAt("1920x1080", "/dev/video0"), camAt("640x480", "/dev/video2")}, ""},
		{
			"different sizes",
			[]Entitlement{camAt("1920x1080"), camAt("640x480")},
			`entitlement[0] and entitlement[1] claim the same camera (neither has an allowlist) but differ in size ("1920x1080" and "640x480")` + mustAgree,
		},
		{
			"one states a size and the other none",
			[]Entitlement{camAt("1920x1080", "/dev/video0"), camAt("")},
			`entitlement[0] and entitlement[1] claim the same camera (entitlement[1] has no allowlist, so it claims every camera) but differ in size ("1920x1080" and unset)` + mustAgree,
		},
		{
			"one camera spelled differently",
			[]Entitlement{camAt("1920x1080", "/dev/video0"), camAt("1280x720", "v4l2:/dev/video0")},
			`entitlement[0] and entitlement[1] claim the same camera ("/dev/video0" and "v4l2:/dev/video0" name one camera) but differ in size ("1920x1080" and "1280x720")` + mustAgree,
		},
		{
			"format and size differ",
			[]Entitlement{{Type: EntitlementCamera, Format: "native", Size: "1920x1080"}, camAt("640x480")},
			`entitlement[0] and entitlement[1] claim the same camera (neither has an allowlist) but differ in format ("native" and "pixels") and size ("1920x1080" and "640x480")` + mustAgree,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assertValidationError(t, validateEntitlements(tt.in, "entitlement"), tt.wantErr)
		})
	}
}

func TestCameraNodeSize_IsSettledLikeFormatAndStamp(t *testing.T) {
	full, small := CameraSize{1920, 1080}, CameraSize{640, 480}
	tests := []struct {
		name          string
		in            []Entitlement
		want          CameraSize
		wantConflicts []string
	}{
		{"no camera entry", []Entitlement{{Type: EntitlementGPU}}, CameraSize{}, nil},
		{"none stated", []Entitlement{camAt("")}, CameraSize{}, nil},
		{"stated", []Entitlement{camAt("1920x1080")}, full, nil},
		{"stated for this camera", []Entitlement{camAt("640x480", c920.Path)}, small, nil},
		{"stated for another camera", []Entitlement{camAt("640x480", "/dev/video9")}, CameraSize{}, nil},
		{"a direct entry configures no node", []Entitlement{{Type: EntitlementCamera, Mode: CameraModeDirect, Size: "1920x1080"}}, CameraSize{}, nil},
		{"a value this build cannot read states none", []Entitlement{camAt("1080p")}, CameraSize{}, nil},
		{
			"the most specific entry decides",
			[]Entitlement{camAt("1920x1080", c920.StableID), camAt("640x480", c920.Path), camAt("")},
			full,
			[]string{`size is 1920x1080 (asked by allowlist entry "by-id:usb-046d_HD_Pro_Webcam_C920-video-index0"); ` +
				`passed over: allowlist entry "/dev/video0" asks for 640x480, an entry without an allowlist asks for no size`},
		},
		{
			"the most specific entry states none",
			[]Entitlement{camAt("", c920.Path), camAt("1920x1080")},
			CameraSize{},
			[]string{`size is no size (asked by allowlist entry "/dev/video0"); passed over: an entry without an allowlist asks for 1920x1080`},
		},
		{
			"the deciding entries disagree",
			[]Entitlement{camAt("1920x1080", c920.Path), camAt("640x480", "v4l2:"+c920.Path)},
			CameraSize{},
			[]string{`size falls back to no size: the entries deciding for this camera disagree (allowlist entry "/dev/video0" asks for 1920x1080, allowlist entry "v4l2:/dev/video0" asks for 640x480)`},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			for _, order := range entitlementOrders(tt.in) {
				_, _, _, conflicts := CameraNodeSettings(order, c920)
				if size := CameraNodeSize(order, c920); size != tt.want || !slices.Equal(conflicts, tt.wantConflicts) {
					t.Fatalf("CameraNodeSize(%+v) = %v\nconflicts %q\nwant %v\nconflicts %q", order, size, conflicts, tt.want, tt.wantConflicts)
				}
			}
		})
	}
}

func TestCameraNodeSettings_TheSizesSentenceComesLast(t *testing.T) {
	in := []Entitlement{
		{Type: EntitlementCamera, Format: "native", Size: "1920x1080", Allowlist: []string{c920.Path}},
		{Type: EntitlementCamera, Format: "pixels", Size: "640x480"},
	}
	_, _, _, conflicts := CameraNodeSettings(in, c920)
	want := []string{
		`format is native (asked by allowlist entry "/dev/video0"); passed over: an entry without an allowlist asks for pixels`,
		`size is 1920x1080 (asked by allowlist entry "/dev/video0"); passed over: an entry without an allowlist asks for 640x480`,
	}
	if !slices.Equal(conflicts, want) {
		t.Fatalf("conflicts = %q\nwant        %q", conflicts, want)
	}
}

func TestLoadFromBytes_CameraSize(t *testing.T) {
	for _, typ := range []string{EntitlementCamera, EntitlementVideo} {
		data := []byte(`{"appId": "com.example.app", "entitlements": [{"type": "` + typ + `", "format": "native", "size": "1920x1080"}]}`)
		cfg, err := LoadFromBytes(data)
		if err != nil {
			t.Fatal(err)
		}
		want := Entitlement{Type: typ, Format: CameraFormatNative, Size: "1920x1080"}
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

func TestValidateJSON_SizeIsUnknownOutsideCameraEntries(t *testing.T) {
	data := []byte(`{"appId": "com.example.app", "entitlements": [{"type": "gpu", "size": "1920x1080"}]}`)
	if warnings := ValidateJSON(data); len(warnings) == 0 {
		t.Fatal("ValidateJSON() accepted size on a gpu entry without a warning")
	}
}

func TestEntitlementAnnotation_CarriesTheSize(t *testing.T) {
	for _, typ := range []string{EntitlementCamera, EntitlementVideo} {
		in := Entitlement{Type: typ, Format: CameraFormatH264, Stamp: CameraStampFrame, Preset: "veryfast", Size: "1920x1080", Allowlist: []string{"/dev/video0", "Brio 101"}}
		value := EntitlementAnnotationValue(in)
		if want := "format=h264,stamp=frame,preset=veryfast,size=1920x1080,allowlist=/dev/video0,Brio 101"; value != want {
			t.Fatalf("%s: annotation = %q, want %q", typ, value, want)
		}
		if got := ParseEntitlementAnnotation(typ, value); !reflect.DeepEqual(got, in) {
			t.Fatalf("%s: parsed %+v, want %+v", typ, got, in)
		}
	}
	if got := ParseEntitlementAnnotation(EntitlementGPU, "size=1920x1080"); got.Size != "" {
		t.Fatalf("a gpu label gave a size: %+v", got)
	}
	if value := EntitlementAnnotationValue(Entitlement{Type: EntitlementGPU, Size: "1920x1080"}); value != "" {
		t.Fatalf("a gpu entry's annotation carries %q", value)
	}
	if value := EntitlementAnnotationValue(camAt("")); value != "" {
		t.Fatalf("an entry without a size carries %q", value)
	}
}

func TestSchema_CameraSizePatternMatchesValidation(t *testing.T) {
	for typ, props := range cameraSchemaVariants(t) {
		prop, _ := props["size"].(map[string]any)
		if prop["pattern"] != cameraSizePattern.String() {
			t.Errorf("%s size pattern = %v, want %q", typ, prop["pattern"], cameraSizePattern.String())
		}
		if _, defaulted := prop["default"]; defaulted {
			t.Errorf("%s size has a schema default; an entry without the key states no size", typ)
		}
	}
}

func TestSchema_ValidatesCameraSize(t *testing.T) {
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
		`{"type": "camera", "size": "1920x1080"}`:                  true,
		`{"type": "video", "format": "native", "size": "640x480"}`: true,
		`{"type": "camera", "size": "1080p"}`:                      false,
		`{"type": "camera", "size": "1920x0"}`:                     false,
		`{"type": "video", "size": "1920X1080"}`:                   false,
		`{"type": "camera", "size": 1080}`:                         false,
		`{"type": "gpu", "size": "1920x1080"}`:                     false,
	} {
		var doc any
		if err := json.Unmarshal([]byte(entitlement), &doc); err != nil {
			t.Fatal(err)
		}
		if err := schema.Validate(doc); (err == nil) != valid {
			t.Errorf("schema.Validate(%s) = %v, want valid %v", entitlement, err, valid)
		}
		var parsed Entitlement
		if json.Unmarshal([]byte(entitlement), &parsed) == nil && parsed.Type != EntitlementGPU {
			if err := validateEntitlements([]Entitlement{parsed}, "entitlement"); (err == nil) != valid {
				t.Errorf("validateEntitlements(%s) = %v, want valid %v like the schema", entitlement, err, valid)
			}
		}
	}
}
