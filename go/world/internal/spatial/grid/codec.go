package grid

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"math"
	"math/bits"
)

// Encoded layout, little endian:
//
//	"WOG1"
//	9 × float64   CellSize, Hit, Miss, Min, Max, Occupied, Free, MaxRange, FieldDistance
//	content       see appendContent
//
// The encoding is canonical: equal grids encode to equal bytes, and Decode
// rejects anything Encode would not have produced.
const magic = "WOG1"

func (c Config) fields() [9]float64 {
	return [9]float64{c.CellSize, c.Hit, c.Miss, c.Min, c.Max, c.Occupied, c.Free, c.MaxRange, c.FieldDistance}
}

// appendContent appends the cells: a uvarint count of chunks holding a known
// cell, then for each, ordered by chunk row then chunk column, its column
// and row as varints and its 1024 raw cells row-major as (uvarint run length,
// varint value) pairs with no two adjacent runs of equal value.
func (g *Grid) appendContent(buf []byte) []byte {
	keys := g.sortedKeys()
	live := keys[:0]
	for _, k := range keys {
		for _, v := range g.chunks[k] {
			if v != 0 {
				live = append(live, k)
				break
			}
		}
	}
	buf = binary.AppendUvarint(buf, uint64(len(live)))
	for _, k := range live {
		buf = binary.AppendVarint(buf, int64(k.x))
		buf = binary.AppendVarint(buf, int64(k.z))
		ch := g.chunks[k]
		for i := 0; i < chunkCells; {
			j := i + 1
			for j < chunkCells && ch[j] == ch[i] {
				j++
			}
			buf = binary.AppendUvarint(buf, uint64(j-i))
			buf = binary.AppendVarint(buf, int64(ch[i]))
			i = j
		}
	}
	return buf
}

// Encode serialises the grid, configuration included.
func (g *Grid) Encode() []byte {
	buf := append(make([]byte, 0, 4+9*8+64*len(g.chunks)), magic...)
	for _, v := range g.cfg.fields() {
		buf = binary.LittleEndian.AppendUint64(buf, math.Float64bits(v))
	}
	return g.appendContent(buf)
}

// Digest is the hex SHA-256 of the cell size and the cells. Grids with equal
// cells have equal digests whatever order their evidence arrived in and
// whatever their update parameters are.
func (g *Grid) Digest() string {
	buf := binary.LittleEndian.AppendUint64(nil, math.Float64bits(g.cfg.CellSize))
	sum := sha256.Sum256(g.appendContent(buf))
	return hex.EncodeToString(sum[:])
}

// Equal reports whether both grids have the same cell size and cells.
func (g *Grid) Equal(other *Grid) bool {
	d, err := g.Diff(other)
	return err == nil && len(d) == 0
}

// Decode rebuilds a grid from Encode's output.
func Decode(data []byte) (*Grid, error) {
	bad := func(what string) (*Grid, error) { return nil, fmt.Errorf("%w: %s", ErrFormat, what) }
	if len(data) < 4+9*8 || string(data[:4]) != magic {
		return bad("header")
	}
	var f [9]float64
	for i := range f {
		f[i] = math.Float64frombits(binary.LittleEndian.Uint64(data[4+8*i:]))
	}
	cfg := Config{CellSize: f[0], Hit: f[1], Miss: f[2], Min: f[3], Max: f[4], Occupied: f[5], Free: f[6], MaxRange: f[7], FieldDistance: f[8]}
	// New fills zero fields with defaults; Encode never writes one except
	// MaxRange, so a config that changes is not canonical.
	g, err := New(cfg)
	if err != nil || g.cfg != cfg {
		return bad("config")
	}
	data = data[4+9*8:]
	// Go's varint readers accept padded encodings; a canonical one is as
	// short as AppendUvarint would make it.
	uvar := func() (uint64, bool) {
		v, n := binary.Uvarint(data)
		if n <= 0 || n != (bits.Len64(v|1)+6)/7 {
			return 0, false
		}
		data = data[n:]
		return v, true
	}
	svar := func() (int64, bool) {
		u, ok := uvar()
		v := int64(u >> 1)
		if u&1 != 0 {
			v = ^v
		}
		return v, ok
	}
	count, ok := uvar()
	if !ok || count > uint64(len(data)) {
		return bad("chunk count")
	}
	const maxChunk = maxIndex >> chunkBits
	var prev chunkKey
	for n := uint64(0); n < count; n++ {
		x, okx := svar()
		z, okz := svar()
		if !okx || !okz || x < -maxChunk-1 || x > maxChunk || z < -maxChunk-1 || z > maxChunk {
			return bad("chunk index")
		}
		key := chunkKey{int32(x), int32(z)}
		if n > 0 && (key.z < prev.z || (key.z == prev.z && key.x <= prev.x)) {
			return bad("chunk order")
		}
		prev = key
		ch := new(chunk)
		known := false
		for i, last := 0, int64(math.MinInt64); i < chunkCells; {
			run, okr := uvar()
			v, okv := svar()
			if !okr || !okv || run == 0 || run > uint64(chunkCells-i) || v == last || v < int64(g.lo) || v > int64(g.hi) {
				return bad("cell run")
			}
			for j := 0; j < int(run); j++ {
				ch[i+j] = int16(v)
			}
			known = known || v != 0
			i, last = i+int(run), v
		}
		if !known {
			return bad("empty chunk")
		}
		g.chunks[key] = ch
	}
	if len(data) != 0 {
		return bad("trailing bytes")
	}
	return g, nil
}
