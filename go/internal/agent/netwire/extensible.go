package netwire

import (
	"bytes"
	"errors"
	"sync"
	"unicode/utf8"

	"github.com/fxamacker/cbor/v2"
)

var extensibleArtifactDecoder cbor.DecMode
var extensibleArtifactOnce sync.Once

// UnmarshalExtensibleArtifact is an explicit opt-in for versioned signed
// schemas that permit unknown optional metadata. It validates every skipped
// value too, before decoding known fields with case-sensitive names. Schema
// code still checks required fields, known types/values, versions and requires.
// The caller must retain the original signed bytes rather than re-encode the
// resulting local view. UnmarshalSigned remains strict for other protocols.
func UnmarshalExtensibleArtifact(data []byte, value any, maxBytes int) error {
	extensibleArtifactOnce.Do(func() {
		options := decoder.DecOptions()
		options.FieldNameMatching = cbor.FieldNameMatchingCaseSensitive
		mode, err := options.DecMode()
		if err != nil {
			panic(err)
		}
		extensibleArtifactDecoder = mode
	})
	if maxBytes <= 0 || len(data) == 0 || len(data) > maxBytes {
		return errors.New("extensible artifact exceeds byte bound")
	}
	walker := artifactWalker{data: data}
	if err := walker.value(0); err != nil {
		return err
	}
	if walker.at != len(data) {
		return errors.New("trailing artifact CBOR")
	}
	return extensibleArtifactDecoder.Unmarshal(data, value)
}

type artifactWalker struct {
	data []byte
	at   int
}

func (w *artifactWalker) argument() (byte, uint64, error) {
	if w.at == len(w.data) {
		return 0, 0, errors.New("truncated artifact CBOR")
	}
	first := w.data[w.at]
	w.at++
	major, small := first>>5, first&31
	if small < 24 {
		return major, uint64(small), nil
	}
	if small > 27 {
		return 0, 0, errors.New("indefinite or reserved artifact CBOR")
	}
	count := 1 << (small - 24)
	if count > len(w.data)-w.at {
		return 0, 0, errors.New("truncated artifact CBOR argument")
	}
	var value uint64
	for _, b := range w.data[w.at : w.at+count] {
		value = value<<8 | uint64(b)
	}
	w.at += count
	if value < 24 || (count > 1 && value < uint64(1)<<(8*(count/2))) {
		return 0, 0, errors.New("nonminimal artifact CBOR argument")
	}
	return major, value, nil
}

func (w *artifactWalker) value(depth int) error {
	if depth > 16 {
		return errors.New("artifact CBOR nesting exceeds bound")
	}
	major, count, err := w.argument()
	if err != nil {
		return err
	}
	switch major {
	case 0, 1:
		return nil
	case 2, 3:
		if count > uint64(len(w.data)-w.at) {
			return errors.New("truncated artifact CBOR string")
		}
		end := w.at + int(count)
		if major == 3 && !utf8.Valid(w.data[w.at:end]) {
			return errors.New("invalid artifact UTF-8")
		}
		w.at = end
	case 4:
		if count > 1024 {
			return errors.New("artifact CBOR array exceeds bound")
		}
		for range count {
			if err := w.value(depth + 1); err != nil {
				return err
			}
		}
	case 5:
		if count > 64 {
			return errors.New("artifact CBOR map exceeds bound")
		}
		var keys [64][]byte
		for i := uint64(0); i < count; i++ {
			keyMajor, size, err := w.argument()
			if err != nil {
				return err
			}
			if keyMajor != 3 || size > uint64(len(w.data)-w.at) {
				return errors.New("artifact CBOR map key must be text")
			}
			key := w.data[w.at : w.at+int(size)]
			if !utf8.Valid(key) {
				return errors.New("invalid artifact map-key UTF-8")
			}
			for _, old := range keys[:i] {
				if bytes.Equal(old, key) {
					return errors.New("duplicate artifact map key")
				}
			}
			keys[i] = key
			w.at += int(size)
			if err := w.value(depth + 1); err != nil {
				return err
			}
		}
	case 7:
		if count != 20 && count != 21 && count != 22 {
			return errors.New("unsupported artifact CBOR primitive")
		}
	default:
		return errors.New("unsupported artifact CBOR type")
	}
	return nil
}
