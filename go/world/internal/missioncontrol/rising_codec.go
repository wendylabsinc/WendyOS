package missioncontrol

import (
	"bytes"
	"compress/flate"
	"encoding/json"
	"errors"
	"io"
)

const RisingTemporalCodec = "deflate-json-v1"
const MaxRisingTemporalExpanded = 40 << 10
const MaxRisingTemporalCompressed = 12 << 10

type risingPlain RisingSelection
type risingWire struct {
	risingPlain
	TemporalCodec string `json:"temporal_codec"`
}

func (p RisingSelection) MarshalJSON() ([]byte, error) {
	if len(p.TemporalJSON) == 0 || len(p.TemporalJSON) > MaxRisingTemporalExpanded {
		return nil, errors.New("temporal originals exceed expansion bound")
	}
	var compressed bytes.Buffer
	w, _ := flate.NewWriter(&compressed, flate.BestCompression)
	if _, err := w.Write(p.TemporalJSON); err != nil {
		return nil, err
	}
	if err := w.Close(); err != nil {
		return nil, err
	}
	if compressed.Len() > MaxRisingTemporalCompressed {
		return nil, errors.New("temporal originals exceed compressed bound")
	}
	plain := risingPlain(p)
	plain.TemporalJSON = compressed.Bytes()
	return json.Marshal(risingWire{plain, RisingTemporalCodec})
}
func (p *RisingSelection) UnmarshalJSON(data []byte) error {
	var wire risingWire
	d := json.NewDecoder(bytes.NewReader(data))
	d.DisallowUnknownFields()
	if err := d.Decode(&wire); err != nil {
		return err
	}
	if d.Decode(new(any)) != io.EOF {
		return errors.New("trailing rising context")
	}
	if wire.TemporalCodec != RisingTemporalCodec || len(wire.TemporalJSON) == 0 || len(wire.TemporalJSON) > MaxRisingTemporalCompressed {
		return errors.New("invalid temporal codec or compressed bound")
	}
	input := bytes.NewReader(wire.TemporalJSON)
	r := flate.NewReader(input)
	expanded, err := io.ReadAll(io.LimitReader(r, MaxRisingTemporalExpanded+1))
	closeErr := r.Close()
	if err != nil || closeErr != nil || input.Len() != 0 || len(expanded) == 0 || len(expanded) > MaxRisingTemporalExpanded {
		return errors.New("invalid temporal expansion or trailing bytes")
	}
	value := RisingSelection(wire.risingPlain)
	value.TemporalJSON = expanded
	*p = value
	return nil
}
