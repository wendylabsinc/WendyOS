package appconfig

// EncodePreset is how fast the software H.264 encoder works: a faster preset
// costs less CPU per frame and more bits for the same picture. The zero value
// states none.
type EncodePreset uint8

// The presets, fastest first: a lower value is a faster preset.
const (
	EncodePresetUnset EncodePreset = iota
	EncodePresetUltrafast
	EncodePresetSuperfast
	EncodePresetVeryfast
	EncodePresetFaster
	EncodePresetFast
	EncodePresetMedium
)

// DefaultEncodePreset is what a camera's encoder runs at when no consumer of
// its encoded video states a preset.
const DefaultEncodePreset = EncodePresetUltrafast

var encodePresetNames = [...]string{
	EncodePresetUltrafast: "ultrafast",
	EncodePresetSuperfast: "superfast",
	EncodePresetVeryfast:  "veryfast",
	EncodePresetFaster:    "faster",
	EncodePresetFast:      "fast",
	EncodePresetMedium:    "medium",
}

// EncodePresets lists the accepted values, fastest first. wendy.schema.json
// repeats them as an enum.
var EncodePresets = encodePresetNames[EncodePresetUltrafast:]

func (p EncodePreset) known() bool {
	return p != EncodePresetUnset && int(p) < len(encodePresetNames)
}

// ParseEncodePreset is the preset of that name, and unset for any other text.
func ParseEncodePreset(name string) EncodePreset {
	for preset := EncodePresetUltrafast; preset.known(); preset++ {
		if encodePresetNames[preset] == name {
			return preset
		}
	}
	return EncodePresetUnset
}

func (p EncodePreset) String() string {
	if !p.known() {
		return ""
	}
	return encodePresetNames[p]
}

func (p EncodePreset) OrDefault() EncodePreset {
	if !p.known() {
		return DefaultEncodePreset
	}
	return p
}

// FasterThan compares two stated presets; a value that states none is neither
// faster nor slower than anything.
func (p EncodePreset) FasterThan(other EncodePreset) bool {
	return p.known() && other.known() && p < other
}

// FastestEncodePreset is the fastest preset stated, and unset when none is.
func FastestEncodePreset(stated ...EncodePreset) EncodePreset {
	fastest := EncodePresetUnset
	for _, preset := range stated {
		if preset.known() && (fastest == EncodePresetUnset || preset.FasterThan(fastest)) {
			fastest = preset
		}
	}
	return fastest
}
