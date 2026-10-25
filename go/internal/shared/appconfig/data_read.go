package appconfig

import (
	"fmt"
	"path"
	"strings"
)

// SystemAPIContainerDirectory is where the agent mounts an app's System API
// socket, which notifications and data-read share.
const SystemAPIContainerDirectory = "/run/wendy/system"

// UnderSystemAPIDirectory reports whether the container path p is
// SystemAPIContainerDirectory or below it, where a persist volume would hide
// the System API socket or be hidden by it.
func UnderSystemAPIDirectory(p string) bool {
	p = path.Clean(p)
	return p == SystemAPIContainerDirectory || strings.HasPrefix(p, SystemAPIContainerDirectory+"/")
}

// dataReadCatalogPrefix starts the id of every stream in the live catalog.
// An allowlist names the streams themselves: the catalog entry of an allowed
// stream comes with it, and an entry for the catalog would admit nothing.
const dataReadCatalogPrefix = "_streams/"

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
// "apps/com.example.detector2". The catalog entry of an admitted stream is
// admitted with it, so an entry never names the catalog. An empty allowlist
// admits every stream. validateAllowlistEntries has already refused ',' and
// '='.
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
		if strings.HasPrefix(entry, dataReadCatalogPrefix) {
			return fmt.Errorf("data-read allowlist[%d] names the stream catalog, %q; name the streams, and their catalog entries come with them", i, entry)
		}
		total += len(entry)
	}
	if total > maxDataReadAllowlistBytes {
		return fmt.Errorf("data-read allowlist entries add up to %d bytes, at most %d are allowed", total, maxDataReadAllowlistBytes)
	}
	return nil
}

// ValidateDataReadAllowlists checks the allowlist of every data-read
// entitlement in entitlements with the rules Validate applies, and nothing
// else: a service's create configuration legitimately carries the app's
// data-read and its own. The agent runs it on every configuration it is sent.
// The allowlist reaches the agent again, after a restart, through a container
// label, and one these rules refuse would not come back the same: [""] would
// come back as no allowlist, which reads every stream.
func ValidateDataReadAllowlists(entitlements []Entitlement) error {
	for i, e := range entitlements {
		if e.Type != EntitlementDataRead {
			continue
		}
		if err := validateAllowlistEntries(e.Allowlist, "entitlement", i); err != nil {
			return err
		}
		if err := validateDataReadAllowlist(e.Allowlist); err != nil {
			return fmt.Errorf("entitlement[%d]: %w", i, err)
		}
	}
	return nil
}
