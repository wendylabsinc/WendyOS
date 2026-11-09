package mcp

import (
	"encoding"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"math"
	"reflect"
	"strconv"
	"strings"
	"unicode/utf8"

	mcpgo "github.com/mark3labs/mcp-go/mcp"
)

const maxProxiedJSONDepth = 1000

var (
	jsonRawMessageType    = reflect.TypeFor[json.RawMessage]()
	toolResultContentType = reflect.TypeFor[mcpgo.ToolResultContent]()
	mcpMetaType           = reflect.TypeFor[mcpgo.Meta]()
	mcpMetaPointerType    = reflect.PointerTo(mcpMetaType)
	textMarshalerType     = reflect.TypeFor[encoding.TextMarshaler]()
	jsonMarshalerType     = reflect.TypeFor[json.Marshaler]()
)

// proxiedResultExceedsMaxBytes measures the result's JSON wire size without
// materializing a second full copy of untrusted content. It stops as soon as
// maxBytes is exceeded. Values that cannot be measured safely fail closed and
// are truncated.
func proxiedResultExceedsMaxBytes(result *mcpgo.CallToolResult, maxBytes int) bool {
	s := boundedJSONSizer{maxBytes: maxBytes}
	if err := s.callToolResult(result); err != nil {
		return true
	}
	return s.exceeded
}

type boundedJSONSizer struct {
	maxBytes int
	bytes    int
	exceeded bool
}

func (s *boundedJSONSizer) add(n int) {
	if s.exceeded || n <= 0 {
		return
	}
	if n > s.maxBytes-s.bytes {
		s.exceeded = true
		return
	}
	s.bytes += n
}

func (s *boundedJSONSizer) callToolResult(result *mcpgo.CallToolResult) error {
	if result == nil {
		s.add(len("null"))
		return nil
	}

	// CallToolResult has a custom MarshalJSON implementation. Spell out its
	// wire shape so measuring it does not invoke that implementation, which
	// allocates a complete serialized copy before returning.
	s.add(1) // {
	fields := 0
	if result.Meta != nil {
		s.fieldPrefix("_meta", &fields)
		if err := s.meta(result.Meta, 1); err != nil {
			return err
		}
	}
	if result.ResultType != "" {
		s.fieldPrefix("resultType", &fields)
		s.string(string(result.ResultType))
	}
	s.fieldPrefix("content", &fields)
	s.add(1) // [
	for i, content := range result.Content {
		if i > 0 {
			s.add(1)
		}
		if err := s.value(reflect.ValueOf(content), 1); err != nil {
			return err
		}
		if s.exceeded {
			return nil
		}
	}
	s.add(1) // ]
	if len(result.RawStructuredContent) > 0 {
		s.fieldPrefix("structuredContent", &fields)
		if err := s.rawJSON(result.RawStructuredContent); err != nil {
			return err
		}
	} else if result.StructuredContent != nil {
		s.fieldPrefix("structuredContent", &fields)
		if err := s.value(reflect.ValueOf(result.StructuredContent), 1); err != nil {
			return err
		}
	}
	if result.IsError {
		s.fieldPrefix("isError", &fields)
		s.add(len("true"))
	}
	if len(result.InputRequests) > 0 {
		s.fieldPrefix("inputRequests", &fields)
		if err := s.value(reflect.ValueOf(result.InputRequests), 1); err != nil {
			return err
		}
	}
	if result.RequestState != "" {
		s.fieldPrefix("requestState", &fields)
		s.string(result.RequestState)
	}
	s.add(1) // }
	return nil
}

// Raw JSON is compacted and HTML-escaped by encoding/json. Count that wire
// representation without allocating another copy of an app's response.
func (s *boundedJSONSizer) rawJSON(raw json.RawMessage) error {
	if !json.Valid(raw) {
		return fmt.Errorf("invalid raw JSON")
	}
	inString, escaped := false, false
	for i := 0; i < len(raw) && !s.exceeded; i++ {
		c := raw[i]
		if !inString {
			if c == ' ' || c == '\t' || c == '\r' || c == '\n' {
				continue
			}
			if c == '"' {
				inString = true
			}
			s.add(1)
			continue
		}
		if escaped {
			escaped = false
			s.add(1)
			continue
		}
		switch c {
		case '\\':
			escaped = true
			s.add(1)
		case '"':
			inString = false
			s.add(1)
		case '<', '>', '&':
			s.add(6)
		default:
			if c == 0xe2 && i+2 < len(raw) && raw[i+1] == 0x80 && (raw[i+2] == 0xa8 || raw[i+2] == 0xa9) {
				s.add(6)
				i += 2
			} else {
				s.add(1)
			}
		}
	}
	return nil
}

func (s *boundedJSONSizer) fieldPrefix(name string, fields *int) {
	if *fields > 0 {
		s.add(1) // ,
	}
	*fields = *fields + 1
	s.string(name)
	s.add(1) // :
}

func (s *boundedJSONSizer) meta(meta *mcpgo.Meta, depth int) error {
	if depth > maxProxiedJSONDepth {
		return fmt.Errorf("JSON nesting exceeds %d", maxProxiedJSONDepth)
	}
	s.add(1) // {
	fields := 0
	if meta.ProgressToken != nil {
		if _, overridden := meta.AdditionalFields["progressToken"]; !overridden {
			s.fieldPrefix("progressToken", &fields)
			if err := s.value(reflect.ValueOf(meta.ProgressToken), depth+1); err != nil {
				return err
			}
		}
	}
	for key, value := range meta.AdditionalFields {
		s.fieldPrefix(key, &fields)
		if err := s.value(reflect.ValueOf(value), depth+1); err != nil {
			return err
		}
		if s.exceeded {
			return nil
		}
	}
	s.add(1) // }
	return nil
}

func (s *boundedJSONSizer) value(v reflect.Value, depth int) error {
	if s.exceeded {
		return nil
	}
	if depth > maxProxiedJSONDepth {
		return fmt.Errorf("JSON nesting exceeds %d", maxProxiedJSONDepth)
	}
	if !v.IsValid() {
		s.add(len("null"))
		return nil
	}
	for v.Kind() == reflect.Interface || v.Kind() == reflect.Pointer {
		if v.IsNil() {
			s.add(len("null"))
			return nil
		}
		if v.Type() == mcpMetaPointerType {
			return s.meta(v.Interface().(*mcpgo.Meta), depth+1)
		}
		if hasCustomJSONEncoding(v.Type()) {
			return fmt.Errorf("unsupported custom JSON marshaler %s", v.Type())
		}
		v = v.Elem()
	}

	if v.Type() == mcpMetaType {
		meta := v.Interface().(mcpgo.Meta)
		return s.meta(&meta, depth+1)
	}
	if v.Type() == jsonRawMessageType {
		return s.rawJSON(v.Interface().(json.RawMessage))
	}
	if v.Type() == toolResultContentType {
		content := v.Interface().(mcpgo.ToolResultContent)
		type plainToolResultContent mcpgo.ToolResultContent
		if len(content.RawStructuredContent) > 0 {
			content.StructuredContent = content.RawStructuredContent
		}
		return s.structure(reflect.ValueOf(plainToolResultContent(content)), depth+1)
	}
	if hasCustomJSONEncoding(v.Type()) {
		return fmt.Errorf("unsupported custom JSON marshaler %s", v.Type())
	}

	switch v.Kind() {
	case reflect.Bool:
		if v.Bool() {
			s.add(len("true"))
		} else {
			s.add(len("false"))
		}
	case reflect.String:
		s.string(v.String())
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		var b [32]byte
		s.add(len(strconv.AppendInt(b[:0], v.Int(), 10)))
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr:
		var b [32]byte
		s.add(len(strconv.AppendUint(b[:0], v.Uint(), 10)))
	case reflect.Float32, reflect.Float64:
		return s.float(v)
	case reflect.Slice:
		if v.IsNil() {
			s.add(len("null"))
			return nil
		}
		if v.Type().Elem().Kind() == reflect.Uint8 {
			s.add(2 + base64.StdEncoding.EncodedLen(v.Len()))
			return nil
		}
		return s.sequence(v, depth+1)
	case reflect.Array:
		return s.sequence(v, depth+1)
	case reflect.Map:
		return s.object(v, depth+1)
	case reflect.Struct:
		return s.structure(v, depth+1)
	default:
		return fmt.Errorf("unsupported JSON value %s", v.Kind())
	}
	return nil
}

func (s *boundedJSONSizer) float(v reflect.Value) error {
	f := v.Float()
	if math.IsInf(f, 0) || math.IsNaN(f) {
		return fmt.Errorf("unsupported JSON float %v", f)
	}
	bits := v.Type().Bits()
	format := byte('f')
	abs := math.Abs(f)
	if abs != 0 && (bits == 64 && (abs < 1e-6 || abs >= 1e21) ||
		bits == 32 && (float32(abs) < 1e-6 || float32(abs) >= 1e21)) {
		format = 'e'
	}
	var storage [32]byte
	b := strconv.AppendFloat(storage[:0], f, format, -1, bits)
	if format == 'e' {
		// encoding/json removes a leading zero from negative exponents.
		n := len(b)
		if n >= 4 && b[n-4] == 'e' && b[n-3] == '-' && b[n-2] == '0' {
			b[n-2] = b[n-1]
			b = b[:n-1]
		}
	}
	s.add(len(b))
	return nil
}

func (s *boundedJSONSizer) sequence(v reflect.Value, depth int) error {
	s.add(1) // [
	for i := 0; i < v.Len(); i++ {
		if i > 0 {
			s.add(1)
		}
		if err := s.value(v.Index(i), depth); err != nil {
			return err
		}
		if s.exceeded {
			return nil
		}
	}
	s.add(1) // ]
	return nil
}

func (s *boundedJSONSizer) object(v reflect.Value, depth int) error {
	if v.IsNil() {
		s.add(len("null"))
		return nil
	}
	if v.Type().Key().Kind() != reflect.String {
		return fmt.Errorf("unsupported JSON map key %s", v.Type().Key())
	}
	if hasCustomJSONEncoding(v.Type().Key()) {
		return fmt.Errorf("unsupported custom JSON map key %s", v.Type().Key())
	}
	s.add(1) // {
	fields := 0
	iter := v.MapRange()
	for iter.Next() {
		s.fieldPrefix(iter.Key().String(), &fields)
		if err := s.value(iter.Value(), depth); err != nil {
			return err
		}
		if s.exceeded {
			return nil
		}
	}
	s.add(1) // }
	return nil
}

func (s *boundedJSONSizer) structure(v reflect.Value, depth int) error {
	s.add(1) // {
	fields := 0
	if err := s.structFields(v, depth, &fields); err != nil {
		return err
	}
	s.add(1) // }
	return nil
}

func (s *boundedJSONSizer) structFields(v reflect.Value, depth int, fields *int) error {
	t := v.Type()
	for i := 0; i < v.NumField(); i++ {
		fieldType := t.Field(i)
		if fieldType.PkgPath != "" { // unexported
			continue
		}
		field := v.Field(i)
		tag := fieldType.Tag.Get("json")
		name, options, _ := strings.Cut(tag, ",")
		if name == "-" {
			continue
		}
		if hasJSONOption(options, "string") {
			return fmt.Errorf("unsupported quoted JSON field %s.%s", t, fieldType.Name)
		}
		if fieldType.Anonymous && name == "" {
			embedded := field
			if embedded.Kind() == reflect.Pointer {
				if embedded.IsNil() {
					continue
				}
				embedded = embedded.Elem()
			}
			if embedded.Kind() == reflect.Struct {
				if err := s.structFields(embedded, depth+1, fields); err != nil {
					return err
				}
				continue
			}
		}
		if name == "" {
			name = fieldType.Name
		}
		if hasJSONOption(options, "omitempty") && isEmptyJSONValue(field) {
			continue
		}
		s.fieldPrefix(name, fields)
		if err := s.value(field, depth+1); err != nil {
			return err
		}
		if s.exceeded {
			return nil
		}
	}
	return nil
}

func hasJSONOption(options, want string) bool {
	for options != "" {
		var option string
		option, options, _ = strings.Cut(options, ",")
		if option == want {
			return true
		}
	}
	return false
}

func hasCustomJSONEncoding(t reflect.Type) bool {
	return t.Implements(jsonMarshalerType) || t.Implements(textMarshalerType)
}

func isEmptyJSONValue(v reflect.Value) bool {
	switch v.Kind() {
	case reflect.Array:
		return v.Len() == 0
	case reflect.Map, reflect.Slice, reflect.String:
		return v.Len() == 0
	case reflect.Bool,
		reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr,
		reflect.Float32, reflect.Float64,
		reflect.Interface, reflect.Pointer:
		return v.IsZero()
	}
	return false
}

func (s *boundedJSONSizer) string(value string) {
	s.add(2) // quotes
	for i := 0; i < len(value) && !s.exceeded; {
		c := value[i]
		if c < utf8.RuneSelf {
			switch {
			case c == '\\' || c == '"' || c == '\b' || c == '\f' || c == '\n' || c == '\r' || c == '\t':
				s.add(2)
			case c < 0x20 || c == '<' || c == '>' || c == '&':
				s.add(6)
			default:
				s.add(1)
			}
			i++
			continue
		}
		r, size := utf8.DecodeRuneInString(value[i:])
		if r == utf8.RuneError && size == 1 || r == '\u2028' || r == '\u2029' {
			s.add(6)
		} else {
			s.add(size)
		}
		i += size
	}
}
