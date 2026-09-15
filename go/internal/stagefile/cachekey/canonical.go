package cachekey

import (
	"encoding/binary"
	"hash"
	"sort"
)

// enc writes a self-delimiting, order-explicit byte stream into a hash.
//
// Three properties make the stream unambiguous, so that no two distinct
// values can produce the same bytes:
//
//   - Every string is length-prefixed, so a boundary between adjacent
//     strings can never be misread ("ab"+"c" and "a"+"bc" differ).
//   - Every slice is count-prefixed, so a slice cannot be confused with its
//     flattened elements.
//   - Field order is fixed by the call order in cachekey.go, and the arms
//     that could otherwise collide are separated by an explicit tag() —
//     "stagefile-key" at the start of a key, "inputs" before the dependency
//     list, and the node kind before the payload, which is what keeps two
//     different kinds from encoding alike. Payload fields themselves are
//     untagged; their position within a known kind identifies them.
//
// This is deliberately more verbose than gob or JSON: both of those can
// change their output when a struct's fields are reordered or a field is
// added, which would silently invalidate every cached node in the fleet
// during an ordinary refactor. Here, any change to the encoding is a
// visible edit to this file or to the call order in cachekey.go.
type enc struct{ h hash.Hash }

func (e enc) tag(name string) {
	e.str(name)
}

func (e enc) str(s string) {
	var n [8]byte
	binary.BigEndian.PutUint64(n[:], uint64(len(s)))
	e.h.Write(n[:])
	e.h.Write([]byte(s))
}

func (e enc) int(i int) {
	var n [8]byte
	binary.BigEndian.PutUint64(n[:], uint64(i))
	e.h.Write(n[:])
}

func (e enc) bool(b bool) {
	if b {
		e.int(1)
		return
	}
	e.int(0)
}

// strs encodes a slice in its given order. Order is significant for package
// lists: apt resolves differently depending on order in rare cases, and
// pretending otherwise would key two genuinely different rootfs alike.
func (e enc) strs(ss []string) {
	e.int(len(ss))
	for _, s := range ss {
		e.str(s)
	}
}

// kv encodes a map in sorted key order.
//
// Sorting is what makes a map hashable at all — Go's iteration order is
// randomized per run, so encoding in range order would give the same map a
// different key on every process. It is also correct rather than merely
// convenient: the only consumers of these maps (ENV, ARG, cmake -D flags)
// are themselves emitted sorted by key, so two maps that differ only in
// declaration order really do produce identical output.
func (e enc) kv(m map[string]string) {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	e.int(len(keys))
	for _, k := range keys {
		e.str(k)
		e.str(m[k])
	}
}
