package jws

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"reflect"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// DecodeStrict accepts one bounded JSON object, rejects duplicate and unknown
// fields, and refuses invalid Unicode instead of replacing it during decoding.
func DecodeStrict(data []byte, out any) error {
	return decodeStrict(data, out, MaxPayloadBytes)
}
func decodeStrict(data []byte, out any, limit int) error {
	if err := validateJSON(data, limit); err != nil {
		return err
	}
	if out == nil {
		return errors.New("JSON output required")
	}
	if err := exactFieldNames(data, reflect.TypeOf(out)); err != nil {
		return err
	}
	d := json.NewDecoder(bytes.NewReader(data))
	d.DisallowUnknownFields()
	if d.Decode(out) != nil {
		return errors.New("unexpected JSON fields or field types")
	}
	if _, err := d.Token(); !errors.Is(err, io.EOF) {
		return errors.New("trailing JSON")
	}
	return nil
}
func validateJSON(data []byte, limit int) error {
	if len(data) == 0 || len(data) > limit || !utf8.Valid(data) || !validStringEscapes(data) {
		return errors.New("invalid or over-budget JSON")
	}
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 || trimmed[0] != '{' {
		return errors.New("JSON object required")
	}
	d := json.NewDecoder(bytes.NewReader(data))
	d.UseNumber()
	if err := walkJSON(d, 0); err != nil {
		return err
	}
	if _, err := d.Token(); !errors.Is(err, io.EOF) {
		return errors.New("trailing JSON")
	}
	return nil
}
func walkJSON(d *json.Decoder, depth int) error {
	if depth > 32 {
		return errors.New("JSON nesting exceeds bound")
	}
	tok, err := d.Token()
	if err != nil {
		return errors.New("invalid JSON")
	}
	delim, ok := tok.(json.Delim)
	if !ok {
		return nil
	}
	switch delim {
	case '{':
		keys := map[string]bool{}
		for d.More() {
			key, err := d.Token()
			if err != nil {
				return errors.New("invalid JSON key")
			}
			name, ok := key.(string)
			if !ok || keys[name] {
				return errors.New("duplicate JSON key")
			}
			keys[name] = true
			if err = walkJSON(d, depth+1); err != nil {
				return err
			}
		}
		end, err := d.Token()
		if err != nil || end != json.Delim('}') {
			return errors.New("invalid JSON object")
		}
	case '[':
		for d.More() {
			if err := walkJSON(d, depth+1); err != nil {
				return err
			}
		}
		end, err := d.Token()
		if err != nil || end != json.Delim(']') {
			return errors.New("invalid JSON array")
		}
	default:
		return errors.New("unexpected JSON delimiter")
	}
	return nil
}

// encoding/json otherwise replaces unpaired escaped UTF-16 surrogates. Reject
// them before the duplicate-key scan and the typed decode see different text.
func validStringEscapes(b []byte) bool {
	for i := 0; i < len(b); i++ {
		if b[i] != '"' {
			continue
		}
		i++
		for ; i < len(b) && b[i] != '"'; i++ {
			if b[i] < 0x20 {
				return false
			}
			if b[i] != '\\' {
				continue
			}
			i++
			if i >= len(b) {
				return false
			}
			if b[i] != 'u' {
				continue
			}
			if i+4 >= len(b) {
				return false
			}
			v, err := strconv.ParseUint(string(b[i+1:i+5]), 16, 16)
			if err != nil {
				return false
			}
			i += 4
			if v >= 0xdc00 && v <= 0xdfff {
				return false
			}
			if v >= 0xd800 && v <= 0xdbff {
				if i+6 >= len(b) || b[i+1] != '\\' || b[i+2] != 'u' {
					return false
				}
				low, err := strconv.ParseUint(string(b[i+3:i+7]), 16, 16)
				if err != nil || low < 0xdc00 || low > 0xdfff {
					return false
				}
				i += 6
			}
		}
		if i >= len(b) {
			return false
		}
	}
	return true
}

// encoding/json's legacy decoder folds case even with DisallowUnknownFields.
// Cryptographic schemas admit only the exact field spelling in their JSON tags.
func exactFieldNames(data []byte, t reflect.Type) error {
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	// time.Time has a standard RFC3339 string encoding, not public object fields.
	// Do not generalize this exception to arbitrary custom unmarshalers.
	if t == reflect.TypeOf(time.Time{}) {
		trimmed := bytes.TrimSpace(data)
		if len(trimmed) < 2 || trimmed[0] != '"' {
			return errors.New("time must be an RFC3339 string")
		}
		var value time.Time
		if err := json.Unmarshal(data, &value); err != nil {
			return errors.New("invalid RFC3339 time")
		}
		return nil
	}
	switch t.Kind() {
	case reflect.Struct:
		fields := map[string]reflect.Type{}
		for i := 0; i < t.NumField(); i++ {
			f := t.Field(i)
			if f.PkgPath != "" {
				continue
			}
			name := strings.Split(f.Tag.Get("json"), ",")[0]
			if name == "-" {
				continue
			}
			if name == "" {
				name = f.Name
			}
			fields[name] = f.Type
		}
		var obj map[string]json.RawMessage
		if json.Unmarshal(data, &obj) != nil || obj == nil {
			return errors.New("JSON object required for schema")
		}
		for name, value := range obj {
			field, ok := fields[name]
			if !ok {
				return errors.New("unknown or noncanonical JSON field")
			}
			if err := exactFieldNames(value, field); err != nil {
				return err
			}
		}
	case reflect.Array, reflect.Slice:
		if t.Elem().Kind() == reflect.Uint8 {
			return nil
		}
		var values []json.RawMessage
		if json.Unmarshal(data, &values) != nil {
			return errors.New("JSON array required")
		}
		for _, value := range values {
			if err := exactFieldNames(value, t.Elem()); err != nil {
				return err
			}
		}
	case reflect.Map:
		var values map[string]json.RawMessage
		if json.Unmarshal(data, &values) != nil {
			return errors.New("JSON object required")
		}
		for _, value := range values {
			if err := exactFieldNames(value, t.Elem()); err != nil {
				return err
			}
		}
	}
	return nil
}
