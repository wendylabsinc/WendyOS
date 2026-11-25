package worldpackage

import (
	"bytes"
	"encoding/json"
	"errors"
	"math"
	"strconv"
	"unicode/utf8"
)

// validateGeneric checks only the versioned package envelope. Model and asset
// bytes remain opaque here; publication and installation never compile them.
func validateGeneric(raw []byte, fields map[string]json.RawMessage) error {
	if !utf8.Valid(raw) || !scalarEscapes(raw) {
		return errors.New("generic package requires Unicode scalar strings")
	}
	if e := shape(fields,
		[]string{"schema_version", "site_id", "package_revision", "frame_revision", "provenance", "geographic_anchor", "local_frame", "mujoco"},
		[]string{"assets", "description", "fidelity", "asset_license", "scenario"}); e != nil {
		return e
	}
	anchor, e := object(fields, "geographic_anchor", "latitude_deg", "longitude_deg", "altitude_m", "altitude_datum", "placement")
	if e != nil {
		return e
	}
	for _, coordinate := range []struct {
		key      string
		min, max float64
	}{{"latitude_deg", -90, 90}, {"longitude_deg", -180, 180}, {"altitude_m", -math.MaxFloat64, math.MaxFloat64}} {
		var value float64
		if e = json.Unmarshal(anchor[coordinate.key], &value); e != nil || math.IsNaN(value) || math.IsInf(value, 0) || value < coordinate.min || value > coordinate.max {
			return errors.New("invalid generic package geographic coordinate")
		}
	}
	if !literal(anchor["altitude_datum"], "WGS84-ellipsoid") || !literal(anchor["placement"], "illustrative") {
		return errors.New("generic package anchor must be illustrative WGS84 ellipsoid placement")
	}
	frame, e := object(fields, "local_frame", "units", "handedness", "up", "axes")
	if e != nil {
		return e
	}
	if !literal(frame["units"], "metres") || !literal(frame["handedness"], "right") || !literal(frame["up"], "Y") || !stringTriple(frame["axes"], [3]string{"east", "up", "south"}) {
		return errors.New("unsupported generic package local frame")
	}
	model, e := object(fields, "mujoco", "path", "sha256", "to_local_position")
	if e != nil {
		return e
	}
	if !stringTriple(model["to_local_position"], [3]string{"x", "z", "-y"}) {
		return errors.New("unsupported generic package MuJoCo coordinate transform")
	}
	return nil
}

func shape(fields map[string]json.RawMessage, requiredKeys, optionalKeys []string) error {
	allowed := make(map[string]bool, len(requiredKeys)+len(optionalKeys))
	for _, key := range requiredKeys {
		if _, e := required(fields, key); e != nil {
			return e
		}
		allowed[key] = true
	}
	for _, key := range optionalKeys {
		allowed[key] = true
	}
	for key := range fields {
		if !allowed[key] {
			return errors.New("unknown generic package field")
		}
	}
	return nil
}

func object(fields map[string]json.RawMessage, key string, keys ...string) (map[string]json.RawMessage, error) {
	raw, e := required(fields, key)
	if e != nil {
		return nil, e
	}
	var out map[string]json.RawMessage
	if e = json.Unmarshal(raw, &out); e != nil {
		return nil, e
	}
	if e = shape(out, keys, nil); e != nil {
		return nil, e
	}
	return out, nil
}

func literal(raw []byte, want string) bool {
	var got string
	return json.Unmarshal(raw, &got) == nil && got == want
}

func stringTriple(raw []byte, want [3]string) bool {
	var got []string
	if json.Unmarshal(raw, &got) != nil || len(got) != len(want) {
		return false
	}
	for i, value := range got {
		if value != want[i] {
			return false
		}
	}
	return true
}

// encoding/json replaces unpaired UTF-16 escape surrogates. Version 1 rejects
// them before that replacement so Go and Python bind the same string values.
// The caller has already checked JSON syntax and duplicate keys.
func scalarEscapes(raw []byte) bool {
	quoted := false
	for i := 0; i < len(raw); i++ {
		switch raw[i] {
		case '"':
			quoted = !quoted
		case '\\':
			if !quoted {
				continue
			}
			i++
			if i >= len(raw) {
				return false
			}
			if raw[i] != 'u' {
				continue
			}
			if i+4 >= len(raw) {
				return false
			}
			n, e := strconv.ParseUint(string(raw[i+1:i+5]), 16, 16)
			if e != nil {
				return false
			}
			i += 4
			if n >= 0xdc00 && n <= 0xdfff {
				return false
			}
			if n < 0xd800 || n > 0xdbff {
				continue
			}
			if i+6 >= len(raw) || !bytes.Equal(raw[i+1:i+3], []byte(`\u`)) {
				return false
			}
			low, e := strconv.ParseUint(string(raw[i+3:i+7]), 16, 16)
			if e != nil || low < 0xdc00 || low > 0xdfff {
				return false
			}
			i += 6
		}
	}
	return true
}
