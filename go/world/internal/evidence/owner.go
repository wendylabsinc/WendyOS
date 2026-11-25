package evidence

import (
	"errors"
	"fmt"
)

// GetOwned is the owner-scoped counterpart of Get. The owner is supplied by a
// trusted host's authenticated caller binding, never inferred from the digest.
// Existing site-wide Get semantics are unchanged.
func (s *Store) GetOwned(owner string, r GetRequest) (GetResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.ready(); err != nil {
		return GetResponse{}, err
	}
	if owner == "" || len(owner) > 512 || !validDigest(r.SHA256) || r.Offset < 0 || r.Length < 1 || r.Length > MaxChunk {
		return GetResponse{}, errors.New("invalid owner-scoped evidence range")
	}
	if err := s.prune(); err != nil {
		return GetResponse{}, err
	}
	m, ok := s.entries[r.SHA256]
	if !ok || m.Owner != owner || !m.Complete {
		return GetResponse{}, ErrUnavailable
	}
	if r.Offset >= m.Size {
		return GetResponse{}, errors.New("evidence offset outside blob")
	}
	verified, err := s.verified(m)
	if err != nil {
		return GetResponse{}, fmt.Errorf("evidence integrity: %w", err)
	}
	length := min(int64(r.Length), m.Size-r.Offset)
	return GetResponse{SHA256: m.SHA256, Size: m.Size, Offset: r.Offset, Data: append([]byte(nil), verified[r.Offset:r.Offset+length]...), ContentType: m.ContentType, RetainUntilUnixMS: m.RetainUntilUnixMS}, nil
}

// CompleteOwned verifies complete retained bytes and immutable source metadata
// under the store lock. It neither grants access by digest nor extends retention.
func (s *Store) CompleteOwned(owner string, ref Reference) (PutResponse, error) {
	meta, _, err := s.ReadCompleteOwned(owner, ref)
	return meta, err
}

// ReadCompleteOwned reads and verifies one bounded complete blob while holding
// the store lock. Returned bytes are independently owned and have no authority
// beyond this checked owner/reference/retention operation.
func (s *Store) ReadCompleteOwned(owner string, ref Reference) (PutResponse, []byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.ready(); err != nil {
		return PutResponse{}, nil, err
	}
	if owner == "" || len(owner) > 512 || !validDigest(ref.SHA256) || ref.Size < 1 || ref.Size > s.cfg.MaxBlobBytes || !validType(ref.ContentType) {
		return PutResponse{}, nil, errors.New("invalid owner-scoped evidence reference")
	}
	if err := s.prune(); err != nil {
		return PutResponse{}, nil, err
	}
	m, ok := s.entries[ref.SHA256]
	if !ok || m.Owner != owner || !m.Complete || m.Size != ref.Size || m.ContentType != ref.ContentType {
		return PutResponse{}, nil, ErrUnavailable
	}
	data, err := s.verified(m)
	if err != nil {
		return PutResponse{}, nil, fmt.Errorf("evidence integrity: %w", err)
	}
	return m.PutResponse, data, nil
}
