package appconfig

import (
	"reflect"
	"testing"
)

func TestEncodePresets_AreTheSixNamesFastestFirst(t *testing.T) {
	want := []string{"ultrafast", "superfast", "veryfast", "faster", "fast", "medium"}
	if !reflect.DeepEqual(EncodePresets, want) {
		t.Fatalf("EncodePresets = %q, want %q", EncodePresets, want)
	}
	for i, name := range want {
		preset := ParseEncodePreset(name)
		if preset == EncodePresetUnset || preset.String() != name {
			t.Errorf("ParseEncodePreset(%q).String() = %q", name, preset)
		}
		for _, slower := range want[i+1:] {
			if !preset.FasterThan(ParseEncodePreset(slower)) || ParseEncodePreset(slower).FasterThan(preset) {
				t.Errorf("%s must be faster than %s, and not the other way round", name, slower)
			}
		}
		if preset.FasterThan(preset) {
			t.Errorf("%s is faster than itself", name)
		}
	}
}

func TestEncodePreset_TheDefaultIsUltrafast(t *testing.T) {
	if DefaultEncodePreset.String() != "ultrafast" {
		t.Fatalf("default preset is %q, want ultrafast", DefaultEncodePreset)
	}
	if got := EncodePresetUnset.OrDefault(); got != DefaultEncodePreset {
		t.Errorf("unset.OrDefault() = %q, want the default", got)
	}
	if got := EncodePresetMedium.OrDefault(); got != EncodePresetMedium {
		t.Errorf("medium.OrDefault() = %q, want medium", got)
	}
}

func TestParseEncodePreset_AnythingButTheSixNamesIsUnset(t *testing.T) {
	for _, name := range []string{"", "slow", "slower", "veryslow", "placebo", "Ultrafast", "ultrafast ", "ultrafast ! fakesink", "0"} {
		if got := ParseEncodePreset(name); got != EncodePresetUnset {
			t.Errorf("ParseEncodePreset(%q) = %q, want unset", name, got)
		}
	}
}

// The name reaches a gst-launch-1.0 argument, so no value of the type may
// yield anything but one of the six names.
func TestEncodePreset_EveryValueNamesOneOfTheSixOnceDefaulted(t *testing.T) {
	for value := 0; value < 256; value++ {
		preset := EncodePreset(value)
		name := preset.OrDefault().String()
		if ParseEncodePreset(name) == EncodePresetUnset {
			t.Fatalf("EncodePreset(%d).OrDefault().String() = %q, which is not one of %q", value, name, EncodePresets)
		}
		if preset.String() != "" && ParseEncodePreset(preset.String()) != preset {
			t.Fatalf("EncodePreset(%d).String() = %q does not parse back", value, preset)
		}
	}
}

func TestEncodePreset_UnsetIsNeitherFasterNorSlower(t *testing.T) {
	for _, name := range EncodePresets {
		preset := ParseEncodePreset(name)
		if EncodePresetUnset.FasterThan(preset) || preset.FasterThan(EncodePresetUnset) {
			t.Errorf("unset compares with %s", name)
		}
	}
}

func TestFastestEncodePreset(t *testing.T) {
	tests := []struct {
		name   string
		stated []EncodePreset
		want   EncodePreset
	}{
		{"nobody states one", nil, EncodePresetUnset},
		{"only unset", []EncodePreset{EncodePresetUnset, EncodePresetUnset}, EncodePresetUnset},
		{"one", []EncodePreset{EncodePresetMedium}, EncodePresetMedium},
		{"unset beside one", []EncodePreset{EncodePresetUnset, EncodePresetFast, EncodePresetUnset}, EncodePresetFast},
		{"the fastest of several", []EncodePreset{EncodePresetMedium, EncodePresetVeryfast, EncodePresetFast}, EncodePresetVeryfast},
		{"in any order", []EncodePreset{EncodePresetVeryfast, EncodePresetMedium}, EncodePresetVeryfast},
		{"a value outside the list states nothing", []EncodePreset{EncodePreset(200), EncodePresetFast}, EncodePresetFast},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := FastestEncodePreset(tt.stated...); got != tt.want {
				t.Fatalf("FastestEncodePreset(%v) = %q, want %q", tt.stated, got, tt.want)
			}
		})
	}
}
