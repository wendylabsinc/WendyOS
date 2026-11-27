package peoplereader

import (
	"context"
	"encoding/json"
	"reflect"
	"sort"
	"testing"
)

func TestSharedReaderPreservesPersonReceiptJSON(t *testing.T) {
	f, api := readerFixture(t)
	s, err := New(f.cfg, api)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	r, err := s.Step(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	var object map[string]json.RawMessage
	json.Unmarshal(raw, &object)
	keys := []string{}
	for key := range object {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	want := []string{"camera_inputs_independently_verified", "clock", "origin_attested_inference", "reader_generation", "run_id", "sample_time", "status", "tick", "tracks", "version"}
	if !reflect.DeepEqual(keys, want) {
		t.Fatalf("person receipt keys changed: %v", keys)
	}
	var tracks []map[string]json.RawMessage
	json.Unmarshal(object["tracks"], &tracks)
	keys = nil
	for key := range tracks[0] {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	want = []string{"envelope_json", "envelope_sha256", "payload", "record_sha256", "version"}
	if !reflect.DeepEqual(keys, want) {
		t.Fatalf("person track keys changed: %v", keys)
	}
}
