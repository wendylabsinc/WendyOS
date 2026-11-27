package livefusion

import (
	"bytes"
	"encoding/json"
	"fmt"
	"reflect"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/world/internal/sensing"
	"github.com/wendylabsinc/wendy/go/world/production/sensorrecord"
	"github.com/wendylabsinc/wendy/go/world/production/serviceaccess"
)

func candidateFixture(t *testing.T, skew time.Duration) (*session, *candidateWindow, []inputProof) {
	t.Helper()
	fixture, _ := setup(t, "complete")
	s := &session{cfg: fixture.cfg, sources: make(map[string]Source, 4)}
	for _, source := range s.cfg.Sources {
		s.sources[source.SensorID] = source
	}
	w, err := newCandidateWindow(s, skew)
	if err != nil {
		t.Fatal(err)
	}
	var current []inputProof
	for _, source := range s.cfg.Sources {
		p, err := s.parse(fixture.proofs[source.SensorID], s.sources)
		if err != nil {
			t.Fatal(err)
		}
		current = append(current, p)
	}
	return s, w, current
}

// These selection-only fixtures deliberately do not resign edited records.
// Matching grants no authority. Actual signed/expired/forged selection through
// Run is covered separately by the ordinary RPC integration tests.
func candidateAt(t *testing.T, s *session, original inputProof, sequence uint64, end time.Duration, padding int) inputProof {
	t.Helper()
	signed, record, err := decodeEnvelope(original.raw)
	if err != nil {
		t.Fatal(err)
	}
	var capture sensorrecord.Capture
	if err = json.Unmarshal(record.Publication.Payload, &capture); err != nil {
		t.Fatal(err)
	}
	var sample sensing.Sample
	if err = json.Unmarshal(capture.SampleJSON, &sample); err != nil {
		t.Fatal(err)
	}
	sample.Sequence = sequence
	sample.ID = fmt.Sprintf("%s/%s/%d", sample.RunID, sample.SensorID, sequence)
	sample.SampleNS = int64(end)
	capture.SampleJSON, err = json.Marshal(sample)
	if err != nil {
		t.Fatal(err)
	}
	if capture.SensorLink != nil {
		capture.SensorLink.CaptureSequence = sequence
		capture.SensorLink.SampleSHA256 = digest(capture.SampleJSON)
	}
	record.Publication, err = sensorrecord.Publication(capture, record.Publication.TTL)
	if err != nil {
		t.Fatal(err)
	}
	record.Sequence = sequence
	signed.Record, err = json.Marshal(record)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(signed)
	if err != nil {
		t.Fatal(err)
	}
	if padding > len(raw) {
		raw = append(raw, bytes.Repeat([]byte{' '}, padding-len(raw))...)
	}
	p, err := s.parse(raw, s.sources)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func candidateSet(t *testing.T, s *session, base []inputProof, sequence uint64, ends map[string]time.Duration) []inputProof {
	t.Helper()
	set := make([]inputProof, len(base))
	for i, p := range base {
		set[i] = candidateAt(t, s, p, sequence, ends[p.record.Publication.Stream], 0)
	}
	return set
}

func candidateState(w *candidateWindow) []string {
	var out []string
	for _, sensor := range w.order {
		source := w.state[sensor]
		out = append(out, fmt.Sprintf("%s:%#v", sensor, source.binding))
		for _, entry := range source.records {
			out = append(out, fmt.Sprintf("%d/%s/%s/%s", entry.sequence, entry.start, entry.end, digest(entry.raw)))
		}
	}
	return out
}

func TestCandidateWindowSelectsFreshestCoherentOriginalTuple(t *testing.T) {
	s, w, base := candidateFixture(t, 350*time.Millisecond)
	first := candidateSet(t, s, base, 1, map[string]time.Duration{"camera-a": 250 * time.Millisecond, "camera-b": 650 * time.Millisecond, "gas-1": 650 * time.Millisecond, "audio-1": 650 * time.Millisecond})
	if selected, err := w.updateAndSelect(first); err != nil || selected != nil {
		t.Fatal("first incoherent tuple selected", err)
	}
	second := candidateSet(t, s, base, 2, map[string]time.Duration{"camera-a": 650 * time.Millisecond, "camera-b": 650 * time.Millisecond, "gas-1": 850 * time.Millisecond, "audio-1": 650 * time.Millisecond})
	if coherent(second, w.skew) {
		t.Fatal("fixture latest-only tuple must exceed skew")
	}
	selected, err := w.updateAndSelect(second)
	if err != nil || len(selected) != 4 || !coherent(selected, w.skew) {
		t.Fatal("coherent recent tuple unavailable", err)
	}
	for i, p := range selected {
		if p.record.Publication.Stream != w.order[i] {
			t.Fatal("unstable source order")
		}
		want := uint64(2)
		originals := second
		if p.record.Publication.Stream == "gas-1" {
			want, originals = 1, first
		}
		if p.record.Sequence != want {
			t.Fatal("wrong retained source version")
		}
		for _, original := range originals {
			if original.record.Publication.Stream == p.record.Publication.Stream && (!bytes.Equal(original.raw, p.raw) || original.reference != p.reference || !reflect.DeepEqual(original.record, p.record)) {
				t.Fatal("selection changed original proof or time")
			}
		}
	}
	third := candidateSet(t, s, base, 3, map[string]time.Duration{"camera-a": time.Second, "camera-b": time.Second, "gas-1": time.Second, "audio-1": time.Second})
	selected, err = w.updateAndSelect(third)
	if err != nil || len(selected) != 4 {
		t.Fatal(err)
	}
	for _, p := range selected {
		if p.record.Sequence != 3 {
			t.Fatal("older coherent tuple preferred to fresher complete tuple")
		}
	}
}

func TestCandidateWindowRequiresCompleteCurrentSet(t *testing.T) {
	_, w, current := candidateFixture(t, 350*time.Millisecond)
	if selected, err := w.updateAndSelect(current); err != nil || len(selected) != 4 {
		t.Fatal(err)
	}
	before := candidateState(w)
	for _, count := range []int{0, 1, 3} {
		selected, err := w.updateAndSelect(current[:count])
		if err != nil || selected != nil || !reflect.DeepEqual(before, candidateState(w)) {
			t.Fatal("cached source concealed incomplete current query", err)
		}
	}
	duplicate := append([]inputProof(nil), current...)
	duplicate[3] = duplicate[0]
	if _, err := w.updateAndSelect(duplicate); err == nil || !reflect.DeepEqual(before, candidateState(w)) {
		t.Fatal("duplicate source changed candidate window")
	}
}

func TestCandidateWindowOwnsBytesAndIsOrderIndependent(t *testing.T) {
	s, a, current := candidateFixture(t, 350*time.Millisecond)
	other := *s
	other.cfg.Sources = append([]Source(nil), s.cfg.Sources...)
	for left, right := 0, 3; left < right; left, right = left+1, right-1 {
		other.cfg.Sources[left], other.cfg.Sources[right] = other.cfg.Sources[right], other.cfg.Sources[left]
	}
	b, err := newCandidateWindow(&other, a.skew)
	if err != nil {
		t.Fatal(err)
	}
	reversed := []inputProof{current[3], current[2], current[1], current[0]}
	selectedA, err := a.updateAndSelect(current)
	if err != nil {
		t.Fatal(err)
	}
	selectedB, err := b.updateAndSelect(reversed)
	if err != nil || !reflect.DeepEqual(selectedA, selectedB) || !reflect.DeepEqual(candidateState(a), candidateState(b)) {
		t.Fatal("query/configuration order changed selection", err)
	}
	before := candidateState(a)
	for range 20 {
		if _, err := a.updateAndSelect(reversed); err != nil || !reflect.DeepEqual(before, candidateState(a)) {
			t.Fatal("duplicate query grew or changed history", err)
		}
	}
	current[0].raw[0] ^= 1
	current[0].record.Publication.Payload[0] ^= 1
	selectedA[0].raw[0] ^= 1
	selectedA[0].signed.Record[0] ^= 1
	if !reflect.DeepEqual(before, candidateState(a)) {
		t.Fatal("caller mutation changed retained proof")
	}
}

func TestCandidateWindowRefusesIdentityAndVersionChangesAtomically(t *testing.T) {
	for _, mode := range []string{"generation", "epoch", "delegation", "scope", "regression", "conflict", "projection"} {
		t.Run(mode, func(t *testing.T) {
			s, w, base := candidateFixture(t, 350*time.Millisecond)
			current := candidateSet(t, s, base, 2, map[string]time.Duration{"camera-a": time.Second, "camera-b": time.Second, "gas-1": time.Second, "audio-1": time.Second})
			if _, err := w.updateAndSelect(current); err != nil {
				t.Fatal(err)
			}
			before := candidateState(w)
			changed := candidateSet(t, s, base, 3, map[string]time.Duration{"camera-a": time.Second, "camera-b": time.Second, "gas-1": time.Second, "audio-1": time.Second})
			p := changed[3]
			switch mode {
			case "generation":
				p.record.Scope.DeploymentGeneration++
			case "epoch":
				p.record.Epoch++
			case "delegation":
				p.signed.Delegation.Claims = append(p.signed.Delegation.Claims, ' ')
				p.record.DelegationSHA256 = digest(p.signed.Delegation.Claims)
			case "scope":
				p.record.Scope.Site = "another-site"
			case "regression":
				p = base[3]
			case "conflict":
				p = current[3]
				p.raw = append(bytes.Clone(p.raw), ' ')
			case "projection":
				p.record.Sequence++
			}
			if mode != "regression" && mode != "conflict" && mode != "projection" {
				p.signed.Record, _ = json.Marshal(p.record)
				p.raw, _ = json.Marshal(p.signed)
			}
			if mode != "projection" {
				parsed, err := s.parse(p.raw, s.sources)
				if err == nil {
					p = parsed
				} else if mode != "scope" {
					t.Fatal("fixture failed before window guard", err)
				}
			}
			changed[3] = p
			if selected, err := w.updateAndSelect(changed); err == nil || selected != nil || !reflect.DeepEqual(before, candidateState(w)) {
				t.Fatal("identity or version change altered history", err)
			}
		})
	}
}

func TestCandidateWindowIncludesFullAudioInterval(t *testing.T) {
	s, w, base := candidateFixture(t, 350*time.Millisecond)
	ends := map[string]time.Duration{"camera-a": 650 * time.Millisecond, "camera-b": 650 * time.Millisecond, "gas-1": 751 * time.Millisecond, "audio-1": 650 * time.Millisecond}
	current := candidateSet(t, s, base, 1, ends)
	if selected, err := w.updateAndSelect(current); err != nil || selected != nil {
		t.Fatal("point timestamps hid full audio span", err)
	}
	other, err := newCandidateWindow(s, 350*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	ends["gas-1"] = 750 * time.Millisecond
	if selected, err := other.updateAndSelect(candidateSet(t, s, base, 1, ends)); err != nil || len(selected) != 4 {
		t.Fatal("exact skew boundary refused", err)
	}
}

func TestCandidateWindowRecordAndByteBounds(t *testing.T) {
	s, w, base := candidateFixture(t, 350*time.Millisecond)
	current := append([]inputProof(nil), base...)
	sequence := uint64(1)
	for i := range current {
		for range 10 {
			sequence++
			otherBytes := 0
			for j, p := range current {
				if i != j {
					otherBytes += len(p.raw)
				}
			}
			current[i] = candidateAt(t, s, base[i], sequence, time.Second, serviceaccess.MaxQueryBytes-otherBytes-1)
			if _, err := w.updateAndSelect(current); err != nil {
				t.Fatal(err)
			}
			count := 0
			for _, source := range w.state {
				if len(source.records) > candidatePerSource {
					t.Fatal("per-source history exceeded bound")
				}
				count += len(source.records)
			}
			if count > 32 || candidateBytes(w.state) > candidateMaxBytes {
				t.Fatal("candidate storage exceeded bounds")
			}
		}
		sequence++
		current[i] = candidateAt(t, s, base[i], sequence, time.Second, 0)
		if _, err := w.updateAndSelect(current); err != nil {
			t.Fatal(err)
		}
	}
	if candidateBytes(w.state) < candidateMaxBytes/2 {
		t.Fatal("fixture did not exercise retained byte pressure")
	}
	before := candidateState(w)
	current[0] = candidateAt(t, s, base[0], sequence+1, time.Second, serviceaccess.MaxQueryBytes)
	if _, err := w.updateAndSelect(current); err == nil || !reflect.DeepEqual(before, candidateState(w)) {
		t.Fatal("oversize query changed retained window")
	}
}
