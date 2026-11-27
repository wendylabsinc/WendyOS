package evidence

// ValidateProtectedOwned checks metadata for bytes already verified by this
// store during one host operation. It does not read content, advance time,
// prune, or extend retention. Only protected completed objects qualify: normal
// store APIs cannot change their owner/content or evict them before expiry.
func (s *Store) ValidateProtectedOwned(owner string, ref Reference, retainUntil int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.ready(); err != nil {
		return err
	}
	m, ok := s.entries[ref.SHA256]
	if !ok || owner == "" || m.Owner != owner || !m.Complete || m.RetentionClass != RetentionProtected ||
		m.Size != ref.Size || m.ContentType != ref.ContentType || retainUntil <= 0 || m.RetainUntilUnixMS != retainUntil {
		return ErrUnavailable
	}
	return nil
}
