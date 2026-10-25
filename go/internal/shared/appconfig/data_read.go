package appconfig

import "fmt"

const (
	// maxDataReadAllowlistEntries bounds a data-read allowlist. Apps declare at
	// most 16 streams each, so an app prefix per producer fits many times over.
	maxDataReadAllowlistEntries = 32
	// maxDataReadAllowlistBytes bounds the entries together. The allowlist
	// travels to the device in a container label, which containerd caps at
	// 4 KiB including the key.
	maxDataReadAllowlistBytes = 2048
)

// validateDataReadAllowlist checks a data-read allowlist. Each entry is a live
// stream id, such as "apps/com.example.detector/markers", or a prefix of stream
// ids that ends at a "/" boundary, such as "apps/com.example.detector". The
// agent matches whole path segments, so that prefix does not admit
// "apps/com.example.detector2". An empty allowlist admits every stream.
// validateAllowlistEntries has already refused ',' and '='.
func validateDataReadAllowlist(allowlist []string) error {
	if len(allowlist) > maxDataReadAllowlistEntries {
		return fmt.Errorf("data-read allowlist has %d entries, at most %d are allowed", len(allowlist), maxDataReadAllowlistEntries)
	}
	total := 0
	for i, entry := range allowlist {
		if entry == "" {
			return fmt.Errorf("data-read allowlist[%d] is empty; name a stream id or a stream id prefix", i)
		}
		for _, c := range []byte(entry) {
			if c <= ' ' || c > '~' {
				return fmt.Errorf("data-read allowlist[%d] must be printable ASCII without spaces, got %q", i, entry)
			}
		}
		total += len(entry)
	}
	if total > maxDataReadAllowlistBytes {
		return fmt.Errorf("data-read allowlist entries add up to %d bytes, at most %d are allowed", total, maxDataReadAllowlistBytes)
	}
	return nil
}
