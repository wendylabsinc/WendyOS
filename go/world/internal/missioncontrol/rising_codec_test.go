package missioncontrol

import (
	"bytes"
	"compress/flate"
	"encoding/base64"
	"encoding/json"
	"testing"
)

func TestRisingCodecExactBoundedOriginals(t *testing.T) {
	original := []byte(`{"original":"bytes, order and number spelling 1.0000 retained"}`)
	p := RisingSelection{TemporalJSON: original}
	raw, e := json.Marshal(p)
	if e != nil {
		t.Fatal(e)
	}
	var decoded RisingSelection
	if e = json.Unmarshal(raw, &decoded); e != nil || !bytes.Equal(decoded.TemporalJSON, original) {
		t.Fatalf("roundtrip %v", e)
	}
	var wire map[string]any
	_ = json.Unmarshal(raw, &wire)
	compressed, _ := base64.StdEncoding.DecodeString(wire["temporal_json"].(string))
	for _, name := range []string{"unknown", "trailing", "truncated", "expanded", "compressed"} {
		t.Run(name, func(t *testing.T) {
			var bad map[string]any
			_ = json.Unmarshal(raw, &bad)
			switch name {
			case "unknown":
				bad["temporal_codec"] = "other"
			case "trailing":
				bad["temporal_json"] = base64.StdEncoding.EncodeToString(append(append([]byte{}, compressed...), 0))
			case "truncated":
				bad["temporal_json"] = base64.StdEncoding.EncodeToString(compressed[:len(compressed)-1])
			case "compressed":
				bad["temporal_json"] = base64.StdEncoding.EncodeToString(make([]byte, MaxRisingTemporalCompressed+1))
			case "expanded":
				var b bytes.Buffer
				w, _ := flate.NewWriter(&b, flate.BestCompression)
				_, _ = w.Write(bytes.Repeat([]byte("x"), MaxRisingTemporalExpanded+1))
				_ = w.Close()
				bad["temporal_json"] = base64.StdEncoding.EncodeToString(b.Bytes())
			}
			b, _ := json.Marshal(bad)
			if json.Unmarshal(b, &decoded) == nil {
				t.Fatal("accepted invalid codec")
			}
		})
	}
}
