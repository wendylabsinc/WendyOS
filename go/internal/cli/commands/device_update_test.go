package commands

import "testing"

// ---- deviceUpdateUpload streaming stubs (Iteration 1) ----
//
// These stubs pin the observable behaviour of the refactored deviceUpdateUpload
// function, whose signature changes from (ctx, svc, []byte, string) to
// (ctx, svc, io.Reader, int64, string) so that the CLI can stream a file
// from disk instead of holding the entire binary in memory.

// TestDeviceUpdateUpload_SendsAllBytesFromReader verifies that every byte
// produced by the io.Reader is delivered to the agent service, and that the
// agent receives them in the correct order.
func TestDeviceUpdateUpload_SendsAllBytesFromReader(t *testing.T) {
	t.Skip("TODO: implement")
}

// TestDeviceUpdateUpload_SHA256IsComputedFromReaderContent verifies that the
// hash sent in the commit control message equals the SHA256 of the bytes
// actually read from the io.Reader, computed inline during streaming rather
// than pre-computed from a full in-memory []byte.
func TestDeviceUpdateUpload_SHA256IsComputedFromReaderContent(t *testing.T) {
	t.Skip("TODO: implement")
}

// TestDeviceUpdateUpload_SendsDataInChunksNoLargerThan64KiB verifies that
// no single chunk message sent to the agent carries more than 64 KiB of
// payload, ensuring the stream stays bounded in per-message memory use.
func TestDeviceUpdateUpload_SendsDataInChunksNoLargerThan64KiB(t *testing.T) {
	t.Skip("TODO: implement")
}
