package resultrecord

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/wendylabsinc/wendy/go/world/internal/world"
	"github.com/wendylabsinc/wendy/go/world/production/jws"
)

func exactText(s string, limit int) bool {
	return s != "" && len(s) <= limit && utf8.ValidString(s) && strings.TrimSpace(s) == s && !strings.ContainsAny(s, "*?") && strings.IndexFunc(s, unicode.IsControl) < 0
}

func validTime(t time.Time) bool {
	return !t.IsZero() && t.UnixMilli() > 0 && t.Year() <= 9999
}

// Validate checks structure and identifiers, never proof authority. ResultJSON
// must be one bounded JSON object; its arbitrary domain fields are not decoded
// into an inference model or normalized into a different representation.
func (r Result) Validate() error {
	if r.Version != Version || !exactText(r.ResultSchema, 256) || len(r.ResultJSON) == 0 || len(r.ResultJSON) > MaxResultBytes || len(r.Inputs) == 0 || len(r.Inputs) > MaxInputs || r.RetainUntilUnixMS <= 0 || !validTime(time.UnixMilli(r.RetainUntilUnixMS)) {
		return errors.New("invalid derived result version, schema, size, inputs or retention timestamp")
	}
	var body map[string]json.RawMessage
	if err := jws.DecodeStrict(r.ResultJSON, &body); err != nil {
		return fmt.Errorf("result JSON: %w", err)
	}
	proofs, records := map[string]bool{}, map[string]bool{}
	type identity struct {
		key     world.Key
		version world.Version
	}
	identities := map[identity]bool{}
	for _, ref := range r.Inputs {
		id := identity{ref.Key, ref.Version}
		if ref.Validate() != nil || proofs[ref.EnvelopeSHA256] || records[ref.RecordSHA256] || identities[id] {
			return errors.New("invalid, duplicate or conflicting derived input reference")
		}
		proofs[ref.EnvelopeSHA256], records[ref.RecordSHA256], identities[id] = true, true, true
	}
	return nil
}

func Encode(r Result) ([]byte, error) {
	if err := r.Validate(); err != nil {
		return nil, err
	}
	raw, err := json.Marshal(r)
	if err != nil {
		return nil, err
	}
	if len(raw) > MaxPayloadBytes {
		return nil, errors.New("derived envelope exceeds publication payload budget")
	}
	return raw, nil
}

// Decode refuses ambiguous outer JSON and requires every envelope field. The
// original result body is carried as base64 bytes and independently subjected
// to duplicate-key, Unicode, object-shape, nesting and byte-budget checks.
func Decode(raw []byte) (Result, error) {
	var result Result
	if len(raw) > MaxPayloadBytes {
		return result, errors.New("derived envelope exceeds publication payload budget")
	}
	if err := jws.DecodeStrict(raw, &result); err != nil {
		return result, err
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return result, err
	}
	for _, name := range []string{"version", "result_schema", "result_json", "inputs", "retain_until_unix_ms"} {
		value, exists := fields[name]
		if !exists || bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
			return result, fmt.Errorf("required derived field %s missing or null", name)
		}
	}
	body := bytes.TrimSpace(fields["result_json"])
	if len(body) < 2 || body[0] != '"' {
		return result, errors.New("result_json must carry original bytes as base64")
	}
	return result, result.Validate()
}
