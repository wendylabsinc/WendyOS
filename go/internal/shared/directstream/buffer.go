// Package directstream implements bounded byte ownership for recoverable
// application streams carried directly by an authenticated BLE neighbour.
// The session actor serializes all calls; buffers do not own carrier lifetimes.
package directstream

import (
	"errors"
	"math"
)

const ChunkSize = 256

var (
	ErrBounds   = errors.New("direct stream offset outside retained or granted range")
	ErrCapacity = errors.New("direct stream buffer capacity exhausted")
)

type chunk struct {
	data [ChunkSize]byte
	next int
}

// Pool is a fixed payload slab shared by streams. Receive grants allocate
// chunks immediately, including grants for bytes which have not arrived yet.
type Pool struct {
	chunks    []chunk
	free      int
	available int
}

func NewPool(bytes int) (*Pool, error) {
	if bytes < ChunkSize || bytes%ChunkSize != 0 {
		return nil, ErrBounds
	}
	p := &Pool{chunks: make([]chunk, bytes/ChunkSize), available: bytes / ChunkSize}
	for i := range p.chunks {
		p.chunks[i].next = i + 1
	}
	p.chunks[len(p.chunks)-1].next = -1
	return p, nil
}

func (p *Pool) Available() int { return p.available * ChunkSize }

// Buffer owns the absolute interval [start,end), with physically backed
// capacity through limit. CopyTo copies into caller-owned storage: queued or
// asynchronous writes must never borrow a slice of the recyclable slab.
type Buffer struct {
	pool              *Pool
	head, tail        int
	start, end, limit uint64
}

func (p *Pool) NewBuffer() *Buffer { return &Buffer{pool: p, head: -1, tail: -1} }
func (b *Buffer) Start() uint64    { return b.start }
func (b *Buffer) End() uint64      { return b.end }
func (b *Buffer) Limit() uint64    { return b.limit }

// Reserve grows the absolute limit to a chunk boundary. Failure is atomic.
// Limits cannot be withdrawn: a smaller local policy simply stops new grants.
func (b *Buffer) Reserve(through uint64) error {
	if through <= b.limit {
		return nil
	}
	if through > math.MaxUint64-(ChunkSize-1) {
		return ErrBounds
	}
	through = (through + ChunkSize - 1) / ChunkSize * ChunkSize
	n := (through - b.limit) / ChunkSize
	if n > uint64(b.pool.available) {
		return ErrCapacity
	}
	for ; b.limit < through; b.limit += ChunkSize {
		i := b.pool.free
		b.pool.free = b.pool.chunks[i].next
		b.pool.available--
		b.pool.chunks[i].next = -1
		if b.tail < 0 {
			b.head = i
		} else {
			b.pool.chunks[b.tail].next = i
		}
		b.tail = i
	}
	return nil
}

func (b *Buffer) at(offset uint64) (int, int) {
	i := b.head
	for n := offset/ChunkSize - b.start/ChunkSize; n > 0; n-- {
		i = b.pool.chunks[i].next
	}
	return i, int(offset % ChunkSize)
}

// Append accepts only a complete validated DATA payload. Partial carrier
// frames must stay in the frame decoder, outside the acknowledged interval.
func (b *Buffer) Append(data []byte) error {
	if uint64(len(data)) > b.limit-b.end {
		return ErrCapacity
	}
	if len(data) == 0 {
		return nil
	}
	i, offset := b.at(b.end)
	for len(data) > 0 {
		n := copy(b.pool.chunks[i].data[offset:], data)
		b.end += uint64(n)
		data = data[n:]
		i, offset = b.pool.chunks[i].next, 0
	}
	return nil
}

func (b *Buffer) CopyTo(dst []byte, offset uint64) (int, error) {
	if offset < b.start || offset > b.end {
		return 0, ErrBounds
	}
	n := min(uint64(len(dst)), b.end-offset)
	dst = dst[:int(n)]
	if n == 0 {
		return 0, nil
	}
	i, within := b.at(offset)
	for len(dst) > 0 {
		copied := copy(dst, b.pool.chunks[i].data[within:])
		dst = dst[copied:]
		i, within = b.pool.chunks[i].next, 0
	}
	return int(n), nil
}

// Release advances ownership after remote ACK (TX) or local consumption (RX).
// Partially consumed chunks stay charged, including their unused prefix.
func (b *Buffer) Release(through uint64) error {
	if through < b.start || through > b.end {
		return ErrBounds
	}
	for n := through/ChunkSize - b.start/ChunkSize; n > 0; n-- {
		i := b.head
		b.head = b.pool.chunks[i].next
		clear(b.pool.chunks[i].data[:])
		b.pool.chunks[i].next = b.pool.free
		b.pool.free = i
		b.pool.available++
	}
	if b.head < 0 {
		b.tail = -1
	}
	b.start = through
	return nil
}

// Close releases stored bytes AND promised receive capacity. The session must
// first fence the attachment and resolve/reset the stream before calling it.
func (b *Buffer) Close() {
	for b.head >= 0 {
		i := b.head
		b.head = b.pool.chunks[i].next
		clear(b.pool.chunks[i].data[:])
		b.pool.chunks[i].next = b.pool.free
		b.pool.free = i
		b.pool.available++
	}
	b.tail = -1
	b.start, b.end, b.limit = 0, 0, 0
}
