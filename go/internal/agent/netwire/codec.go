// Package netwire defines the breaking Wendy Net binary encoding. There is no
// JSON fallback. Framing and message-specific resource limits belong to callers.
package netwire

import "github.com/fxamacker/cbor/v2"

// RawMessage contains a complete encoded CBOR value, for inspecting a message's
// kind before interpreting a known payload, or embedding the exact encoded
// metadata map covered by an origin signature.
type RawMessage = cbor.RawMessage

var encoder cbor.EncMode
var decoder, strictDecoder cbor.DecMode

func init() {
	enc := cbor.CanonicalEncOptions()
	enc.IndefLength = cbor.IndefLengthForbidden
	enc.TagsMd = cbor.TagsForbidden
	// UUIDs and IP prefixes keep their established textual spelling. Actual
	// binary fields are byte strings; encoding.BinaryMarshaler isn't a reason
	// to change those structured identity types into an undocumented format.
	enc.BinaryMarshaler = cbor.BinaryMarshalerNone
	enc.TextMarshaler = cbor.TextMarshalerTextString
	enc.NilContainers = cbor.NilContainerAsEmpty
	var err error
	encoder, err = enc.EncMode()
	if err != nil {
		panic(err)
	}
	dec := cbor.DecOptions{
		DupMapKey:         cbor.DupMapKeyEnforcedAPF,
		FieldNameMatching: cbor.FieldNameMatchingCaseSensitive,
		IndefLength:       cbor.IndefLengthForbidden,
		TagsMd:            cbor.TagsForbidden,
		MaxNestedLevels:   16,
		MaxArrayElements:  1024,
		MaxMapPairs:       64,
		BinaryUnmarshaler: cbor.BinaryUnmarshalerNone,
		TextUnmarshaler:   cbor.TextUnmarshalerTextString,
	}
	decoder, err = dec.DecMode()
	if err != nil {
		panic(err)
	}
	dec.ExtraReturnErrors = cbor.ExtraDecErrorUnknownField
	strictDecoder, err = dec.DecMode()
	if err != nil {
		panic(err)
	}
}

func Marshal(value any) ([]byte, error)      { return encoder.Marshal(value) }
func Unmarshal(data []byte, value any) error { return decoder.Unmarshal(data, value) }

// UnmarshalSigned rejects undefined fields in an origin-signed schema. Transport
// messages use Unmarshal so additive fields don't break a working carrier.
func UnmarshalSigned(data []byte, value any) error { return strictDecoder.Unmarshal(data, value) }
