package livefusion

import (
	"reflect"
	"sort"
	"time"

	"github.com/wendylabsinc/wendy/go/world/internal/sensing"
	"github.com/wendylabsinc/wendy/go/world/production/serviceaccess"
)

const (
	candidatePerSource = 8
	candidateMaxBytes  = 2 << 20
)

type candidate struct {
	raw        []byte
	start, end time.Time
	sequence   uint64
	hash       string
}

type candidateSource struct {
	binding sourceBinding
	records []candidate // Increasing publication sequence, with exact duplicates omitted.
}

// candidateWindow matches original capture intervals. It supplies no authority
// or freshness decision: the selected originals must still pass every existing
// signature, current-source, media, sample-time and physical-time check.
// It is owned by one Run or Session and is not safe for concurrent use.
type candidateWindow struct {
	parser            *session
	sources           map[string]Source
	order             []string
	skew              time.Duration
	state             map[string]candidateSource
	retired           map[[16]string]struct{}
	maxRetired        int
	available         map[string]bool
	currentRemoteOnly bool
}

func newCandidateWindow(s *session, maxSkew time.Duration) (*candidateWindow, error) {
	if s == nil || len(s.cfg.Sources) < 4 || len(s.cfg.Sources) > serviceaccess.MaxQueryRecords || len(s.sources) != len(s.cfg.Sources) || maxSkew <= 0 || maxSkew > time.Second {
		return nil, ErrConfig
	}
	w := &candidateWindow{sources: make(map[string]Source, len(s.sources)), skew: maxSkew, state: make(map[string]candidateSource, len(s.sources)), retired: make(map[[16]string]struct{})}
	for _, source := range s.cfg.Sources {
		if source.selector().Validate() != nil || s.sources[source.SensorID] != source {
			return nil, ErrConfig
		}
		if _, exists := w.sources[source.SensorID]; exists {
			return nil, ErrConfig
		}
		w.sources[source.SensorID] = source
		w.order = append(w.order, source.SensorID)
	}
	sort.Strings(w.order)
	// Parsing uses a fixed commissioning snapshot, with no client, clock or
	// verifier. It only owns and checks the exact original envelope projection.
	cfg := s.cfg
	cfg.Sources = append([]Source(nil), s.cfg.Sources...)
	w.parser = &session{cfg: cfg}
	return w, nil
}

// updateAndSelect accepts only the complete result of a new session.query.
// Missing sources cannot be supplied from history. A refused update is atomic.
// A nil selection means the current set is incomplete or no retained tuple is
// coherent; neither outcome authorizes use of an earlier returned selection.
func (w *candidateWindow) updateAndSelect(current []inputProof) ([]inputProof, error) {
	if w == nil || len(current) > len(w.order) {
		return nil, ErrSource
	}
	next := make(map[string]candidateSource, len(w.order))
	for id, state := range w.state {
		state.records = append([]candidate(nil), state.records...)
		next[id] = state
	}
	seen := make(map[string]bool, len(w.order))
	currentBytes := 0
	for _, input := range current {
		currentBytes += len(input.raw)
		if currentBytes > serviceaccess.MaxQueryBytes {
			return nil, ErrSource
		}
		p, err := w.parser.parse(input.raw, w.sources)
		if err != nil || !reflect.DeepEqual(p.record, input.record) || !reflect.DeepEqual(p.capture, input.capture) ||
			!reflect.DeepEqual(p.signed, input.signed) || p.reference != input.reference {
			return nil, ErrSource
		}
		sensor := p.record.Publication.Stream
		if seen[sensor] {
			return nil, ErrSource
		}
		seen[sensor] = true
		old := w.state[sensor]
		if len(old.records) != 0 {
			b := old.binding
			if !reflect.DeepEqual(b.scope, p.record.Scope) || b.epoch != p.record.Epoch ||
				b.delegation != p.record.DelegationSHA256 || p.record.Sequence < b.sequence ||
				p.record.Sequence == b.sequence && p.reference.EnvelopeSHA256 != b.envelope {
				return nil, ErrSource
			}
		}
		records := append([]candidate(nil), old.records...)
		if w.currentRemoteOnly && p.record.Scope.Device != w.parser.cfg.Device {
			// Native admission requires an exact current remote register. Local
			// originals may remain valid archived dependencies until expiry.
			records = nil
		}
		if len(records) == 0 || p.record.Sequence > old.binding.sequence {
			sample, err := p.capture.Sample()
			if err != nil {
				return nil, ErrSource
			}
			end := p.record.Publication.Timing.SampleTime
			start := end
			if sample.Audio != nil {
				start = start.Add(-time.Duration(sample.Audio.DurationNS))
			}
			if start.IsZero() || end.IsZero() || start.After(end) {
				return nil, ErrSource
			}
			records = append(records, candidate{raw: p.raw, start: start, end: end, sequence: p.record.Sequence, hash: p.reference.EnvelopeSHA256})
			if len(records) > candidatePerSource {
				clear(records[:len(records)-candidatePerSource])
				records = records[len(records)-candidatePerSource:]
			}
		}
		next[sensor] = candidateSource{binding: sourceBinding{p.record.Scope, p.record.Epoch, p.record.DelegationSHA256, p.record.Sequence, p.reference.EnvelopeSHA256}, records: records}
	}
	for _, id := range w.order {
		if !sensing.LidarSensorID(id) && !seen[id] {
			return nil, nil
		}
	}
	// Required sources are present. Absent optional scans keep their version
	// high-water but cannot contribute a cached observation to this selection.
	// The latest entry of each source fits within MaxQueryBytes. Remove older
	// capture intervals deterministically if the envelope budget is exhausted.
	for candidateBytes(next) > candidateMaxBytes {
		var selected string
		var index int
		var oldest candidate
		for _, sensor := range w.order {
			records := next[sensor].records
			if len(records) < 2 {
				continue
			}
			for i, value := range records[:len(records)-1] {
				if selected == "" || value.start.Before(oldest.start) || value.start.Equal(oldest.start) && value.sequence < oldest.sequence {
					selected, index, oldest = sensor, i, value
				}
			}
		}
		if selected == "" {
			return nil, ErrSource
		}
		source := next[selected]
		copy(source.records[index:], source.records[index+1:])
		source.records[len(source.records)-1] = candidate{}
		source.records = source.records[:len(source.records)-1]
		next[selected] = source
	}
	w.state = next
	w.available = seen
	best := w.selectTuple()
	if best == nil {
		return nil, nil
	}
	selected := make([]inputProof, 0, len(w.order))
	for _, entry := range best {
		p, err := w.parser.parse(entry.raw, w.sources)
		if err != nil {
			return nil, ErrSource
		}
		selected = append(selected, p)
	}
	return selected, nil
}

// retire prevents an exact evaluated tuple from being selected again in this
// Run. Individual records may still join a newer tuple, which must undergo all
// verification again. Retirement changes neither source pins nor high-water
// marks and grants no authority. At most one tuple is retired per attempt.
func (w *candidateWindow) retire(proofs []inputProof) error {
	if w == nil || len(proofs) < 4 || len(proofs) > len(w.order) {
		return ErrSource
	}
	var key [16]string
	seen := make(map[string]bool, len(w.order))
	for _, p := range proofs {
		stream := p.record.Publication.Stream
		if seen[stream] || w.sources[stream].SensorID != stream || digest(p.raw) != p.reference.EnvelopeSHA256 {
			return ErrSource
		}
		seen[stream] = true
		for i, source := range w.order {
			if source == stream {
				key[i] = p.reference.EnvelopeSHA256
			}
		}
	}
	for _, id := range w.order {
		if !sensing.LidarSensorID(id) && !seen[id] {
			return ErrSource
		}
	}
	if _, exists := w.retired[key]; exists {
		return nil
	}
	limit := w.maxRetired
	if limit == 0 {
		limit = 16
	}
	if len(w.retired) >= limit {
		return ErrSource
	}
	w.retired[key] = struct{}{}
	return nil
}

func candidateBytes(state map[string]candidateSource) int {
	total := 0
	for _, source := range state {
		for _, record := range source.records {
			total += len(record.raw)
		}
	}
	return total
}

// Search each observed interval start as the lower edge of a coherent window.
// Within it, enumerate newest sequence vectors until the first non-retired
// tuple. At most len(retired)+1 vectors per window are needed, rather than
// enumerating 8^N combinations when LiDAR sources are configured.
func (w *candidateWindow) selectTuple() []candidate {
	var best []candidate
	var bestStart time.Time
	var bestSequences [16]uint64
	hasLidar := false
	for _, id := range w.order {
		hasLidar = hasLidar || sensing.LidarSensorID(id)
	}
	for _, sensor := range w.order {
		if w.available != nil && !w.available[sensor] {
			continue
		}
		for _, anchor := range w.state[sensor].records {
			options := make([][]candidate, 0, len(w.order))
			slots := []int{}
			selectedLidar := 0
			complete := true
			for slot, id := range w.order {
				optional := sensing.LidarSensorID(id)
				if w.available != nil && !w.available[id] {
					if optional {
						continue
					}
					complete = false
					break
				}
				values := []candidate{}
				for _, c := range w.state[id].records {
					if !c.start.Before(anchor.start) && !c.end.After(anchor.start.Add(w.skew)) {
						values = append(values, c)
					}
				}
				sort.Slice(values, func(a, b int) bool { return values[a].sequence > values[b].sequence })
				if len(values) == 0 {
					if optional {
						continue
					}
					complete = false
					break
				}
				options = append(options, values)
				slots = append(slots, slot)
				if optional {
					selectedLidar++
				}
			}
			if !complete || hasLidar && selectedLidar == 0 {
				continue
			}
			indices := make([]int, len(options))
			for trial := 0; trial <= len(w.retired); trial++ {
				tuple := make([]candidate, len(options))
				var key [16]string
				earliest := time.Time{}
				var sequences [16]uint64
				for i := range tuple {
					tuple[i] = options[i][indices[i]]
					key[slots[i]] = tuple[i].hash
					sequences[slots[i]] = tuple[i].sequence
					if earliest.IsZero() || tuple[i].start.Before(earliest) {
						earliest = tuple[i].start
					}
				}
				if _, retired := w.retired[key]; !retired {
					better := best == nil || earliest.After(bestStart)
					if !better && earliest.Equal(bestStart) {
						for i := range sequences {
							if sequences[i] != bestSequences[i] {
								better = sequences[i] > bestSequences[i]
								break
							}
						}
					}
					if better {
						best, bestStart, bestSequences = tuple, earliest, sequences
					}
					break
				}
				advanced := false
				for i := len(indices) - 1; i >= 0; i-- {
					indices[i]++
					if indices[i] < len(options[i]) {
						advanced = true
						break
					}
					indices[i] = 0
				}
				if !advanced {
					break
				}
			}
		}
	}
	return best
}
