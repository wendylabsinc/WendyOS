package evidence

import (
	"errors"
	"fmt"
	"time"
)

// ModalityBudget is a ceiling, not a reservation. Zero fields inherit the
// store-wide bound. Classification follows immutable MIME metadata; it does
// not attest that the stored bytes have the declared media semantics.
type ModalityBudget struct {
	QuotaBytes     int64 `json:"quota_bytes"`
	MaxRetentionMS int64 `json:"max_retention_ms"`
}

var modalities = [...]string{"audio", "data", "image", "video"}

func modality(contentType string) string {
	switch contentType {
	case "image/png", "image/jpeg":
		return "image"
	case "audio/wav":
		return "audio"
	case "video/mp4":
		return "video"
	case "application/json", "application/octet-stream":
		return "data"
	default:
		return ""
	}
}

// NormalizeModalityBudgets detaches caller-owned policy and returns all four
// effective ceilings. It is also used before worldd creates any runtime state.
func NormalizeModalityBudgets(input map[string]ModalityBudget, quota int64, retention time.Duration) (map[string]ModalityBudget, error) {
	if quota == 0 {
		quota = 128 << 20
	}
	if retention == 0 {
		retention = 7 * 24 * time.Hour
	}
	if quota < 1 || quota > 1<<40 || retention < time.Second || retention > 30*24*time.Hour {
		return nil, errors.New("invalid global evidence budget")
	}
	out := make(map[string]ModalityBudget, 4)
	for _, name := range modalities {
		out[name] = ModalityBudget{QuotaBytes: quota, MaxRetentionMS: retention.Milliseconds()}
	}
	for name, b := range input {
		if _, ok := out[name]; !ok {
			return nil, fmt.Errorf("unknown evidence modality %q", name)
		}
		if b.QuotaBytes < 0 || b.QuotaBytes > quota || b.MaxRetentionMS < 0 || b.MaxRetentionMS > retention.Milliseconds() || b.MaxRetentionMS > 0 && b.MaxRetentionMS < 1000 {
			return nil, fmt.Errorf("invalid evidence budget for %s", name)
		}
		if b.QuotaBytes == 0 {
			b.QuotaBytes = quota
		}
		if b.MaxRetentionMS == 0 {
			b.MaxRetentionMS = retention.Milliseconds()
		}
		out[name] = b
	}
	return out, nil
}

// UsageCounters describe accounted manifest bytes, not filesystem blocks or
// verified readable bytes. IncompleteBytes reserves the entire declared size.
// PinnedBytes overlaps ProtectedBytes/RollingBytes; it is not additive.
type UsageCounters struct {
	AllocatedBytes    int64 `json:"allocated_bytes"`
	CommittedBytes    int64 `json:"committed_bytes"`
	CompleteBytes     int64 `json:"complete_bytes"`
	IncompleteBytes   int64 `json:"incomplete_bytes"`
	ProtectedBytes    int64 `json:"protected_bytes"`
	RollingBytes      int64 `json:"rolling_bytes"`
	PinnedBytes       int64 `json:"pinned_bytes"`
	EvictableBytes    int64 `json:"evictable_bytes"`
	Entries           int   `json:"entries"`
	CompleteEntries   int   `json:"complete_entries"`
	IncompleteEntries int   `json:"incomplete_entries"`
}
type ModalityUsage struct {
	Modality string `json:"modality"`
	ModalityBudget
	UsageCounters
}
type Usage struct {
	Version          int             `json:"version"`
	Tenant           string          `json:"tenant"`
	Site             string          `json:"site"`
	ObservedAtUnixMS int64           `json:"observed_at_unix_ms"`
	LimitSemantics   string          `json:"limit_semantics"`
	QuotaBytes       int64           `json:"quota_bytes"`
	MaxBlobBytes     int64           `json:"max_blob_bytes"`
	MaxRetentionMS   int64           `json:"max_retention_ms"`
	MaxEntries       int             `json:"max_entries"`
	Totals           UsageCounters   `json:"totals"`
	Modalities       []ModalityUsage `json:"modalities"`
}

func (c *UsageCounters) add(m manifest, pinned bool) {
	c.AllocatedBytes += m.Size
	c.CommittedBytes += m.Received
	c.Entries++
	if m.Complete {
		c.CompleteBytes += m.Size
		c.CompleteEntries++
	} else {
		c.IncompleteBytes += m.Size
		c.IncompleteEntries++
	}
	if m.RetentionClass == RetentionRolling {
		c.RollingBytes += m.Size
		if m.Complete && !pinned {
			c.EvictableBytes += m.Size
		}
	} else {
		c.ProtectedBytes += m.Size
	}
	if pinned {
		c.PinnedBytes += m.Size
	}
}
func (s *Store) Usage() (Usage, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.ready(); err != nil {
		return Usage{}, err
	}
	if err := s.prune(); err != nil {
		return Usage{}, err
	}
	out := Usage{Version: 1, Tenant: s.cfg.Tenant, Site: s.cfg.Site, ObservedAtUnixMS: s.cfg.Now().UnixMilli(), LimitSemantics: "ceilings", QuotaBytes: s.cfg.QuotaBytes, MaxBlobBytes: s.cfg.MaxBlobBytes, MaxRetentionMS: s.cfg.MaxRetention.Milliseconds(), MaxEntries: s.cfg.MaxEntries, Modalities: make([]ModalityUsage, 0, 4)}
	counts := map[string]UsageCounters{}
	for _, m := range s.entries {
		name := modality(m.ContentType)
		c := counts[name]
		c.add(m, s.pinned[m.SHA256] > 0)
		counts[name] = c
		out.Totals.add(m, s.pinned[m.SHA256] > 0)
	}
	for _, name := range modalities {
		out.Modalities = append(out.Modalities, ModalityUsage{Modality: name, ModalityBudget: s.cfg.ModalityBudgets[name], UsageCounters: counts[name]})
	}
	return out, nil
}

// Validate checks accounting consistency for clients. Counts are not an
// integrity proof and do not grant read permission over any particular blob.
func (u Usage) Validate() error {
	bad := errors.New("invalid evidence usage accounting")
	if u.Version != 1 || u.Tenant == "" || u.Site == "" || u.ObservedAtUnixMS <= 0 || u.LimitSemantics != "ceilings" || u.QuotaBytes < 1 || u.QuotaBytes > 1<<40 || u.MaxBlobBytes < 1 || u.MaxBlobBytes > 64<<20 || u.MaxBlobBytes > u.QuotaBytes || u.MaxRetentionMS < 1000 || u.MaxRetentionMS > int64(30*24*time.Hour/time.Millisecond) || u.MaxEntries < 1 || u.MaxEntries > 100000 || len(u.Modalities) != 4 {
		return bad
	}
	valid := func(c UsageCounters) bool {
		for _, n := range []int64{c.AllocatedBytes, c.CommittedBytes, c.CompleteBytes, c.IncompleteBytes, c.ProtectedBytes, c.RollingBytes, c.PinnedBytes, c.EvictableBytes} {
			if n < 0 || n > u.QuotaBytes {
				return false
			}
		}
		return c.Entries >= 0 && c.Entries <= u.MaxEntries && c.CompleteEntries >= 0 && c.IncompleteEntries >= 0 && c.CompleteEntries+c.IncompleteEntries == c.Entries && c.CompleteBytes+c.IncompleteBytes == c.AllocatedBytes && c.ProtectedBytes+c.RollingBytes == c.AllocatedBytes && c.CommittedBytes >= c.CompleteBytes && c.CommittedBytes <= c.AllocatedBytes && c.PinnedBytes <= c.CompleteBytes && c.EvictableBytes <= c.CompleteBytes && c.EvictableBytes <= c.RollingBytes && c.PinnedBytes+c.EvictableBytes <= c.CompleteBytes && (c.CompleteEntries == 0) == (c.CompleteBytes == 0) && (c.IncompleteEntries == 0) == (c.IncompleteBytes == 0) && c.AllocatedBytes >= int64(c.Entries)
	}
	if !valid(u.Totals) {
		return bad
	}
	var totals UsageCounters
	for i, m := range u.Modalities {
		if m.Modality != modalities[i] || m.QuotaBytes < 1 || m.QuotaBytes > u.QuotaBytes || m.MaxRetentionMS < 1000 || m.MaxRetentionMS > u.MaxRetentionMS || m.AllocatedBytes > m.QuotaBytes || !valid(m.UsageCounters) {
			return bad
		}
		totals.AllocatedBytes += m.AllocatedBytes
		totals.CommittedBytes += m.CommittedBytes
		totals.CompleteBytes += m.CompleteBytes
		totals.IncompleteBytes += m.IncompleteBytes
		totals.ProtectedBytes += m.ProtectedBytes
		totals.RollingBytes += m.RollingBytes
		totals.PinnedBytes += m.PinnedBytes
		totals.EvictableBytes += m.EvictableBytes
		totals.Entries += m.Entries
		totals.CompleteEntries += m.CompleteEntries
		totals.IncompleteEntries += m.IncompleteEntries
	}
	if totals != u.Totals {
		return bad
	}
	return nil
}
