package proposals

import "sort"

// Export returns the original contiguous journal sequence, including expired
// suggestions. It does not turn that history into current authority.
func (s *Store) Export(after uint64, limit int) ([]Record, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.ready(); err != nil {
		return nil, false, err
	}
	if limit < 1 || limit > 16 || after > uint64(len(s.records)) {
		return nil, false, ErrInvalid
	}
	records := make([]Record, 0, len(s.records))
	for _, r := range s.records {
		if r.Sequence > after {
			records = append(records, r)
		}
	}
	sort.Slice(records, func(i, j int) bool { return records[i].Sequence < records[j].Sequence })
	more := len(records) > limit
	if more {
		records = records[:limit]
	}
	out := make([]Record, len(records))
	for i, r := range records {
		out[i] = copyRecord(r)
	}
	return out, more, nil
}

// ValidateHistory checks immutable record bindings against an explicitly
// configured origin. Cryptographic verification belongs to the importing node.
func ValidateHistory(r Record, cfg Config) error {
	if r.Sequence == 0 {
		return ErrInvalid
	}
	s := Store{cfg: cfg}
	return s.validateStored(r)
}
