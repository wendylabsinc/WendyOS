package commands

import (
	"bytes"
	"math/rand/v2"
	"sync"
	"testing"
	"time"
)

// syncHandleDisk is an in-memory drive behind one shared file pointer, like a
// Windows disk handle opened without FILE_FLAG_OVERLAPPED. seekThenWrite is
// how handleWriterAt wrote before (SetFilePointerEx, then WriteFile at the
// file pointer); positionalWrite is how it writes now (WriteFile with the
// offset in an OVERLAPPED). Both let another writer run before they write,
// as a thread preempted between two syscalls would, so concurrent writers
// interleave on nearly every chunk instead of only by chance.
type syncHandleDisk struct {
	mu      sync.Mutex
	buf     []byte
	pos     int64
	arrived chan struct{}
}

func newSyncHandleDisk(size int64) *syncHandleDisk {
	return &syncHandleDisk{buf: make([]byte, size), arrived: make(chan struct{})}
}

// interleave returns once another writer has called interleave too, or after
// a short wait so the last writers still finish.
func (d *syncHandleDisk) interleave() {
	d.mu.Lock()
	close(d.arrived)
	next := make(chan struct{})
	d.arrived = next
	d.mu.Unlock()
	select {
	case <-next:
	case <-time.After(10 * time.Millisecond):
	}
}

func (d *syncHandleDisk) seekThenWrite(p []byte, off int64) (int, error) {
	d.mu.Lock()
	d.pos = off
	d.mu.Unlock()
	d.interleave()
	d.mu.Lock()
	defer d.mu.Unlock()
	n := copy(d.buf[d.pos:], p)
	d.pos += int64(n)
	return n, nil
}

func (d *syncHandleDisk) positionalWrite(p []byte, off int64) (int, error) {
	d.interleave()
	d.mu.Lock()
	defer d.mu.Unlock()
	n := copy(d.buf[off:], p)
	// A synchronous WriteFile moves the file pointer even when handed an
	// offset; a positional writer must not care.
	d.pos = off + int64(n)
	return n, nil
}

// buildRandomBmapImage returns a 64-block image of random bytes, so a block
// written at another block's offset never matches by chance, and a block map
// with three mapped ranges separated by holes.
func buildRandomBmapImage() ([]byte, *Bmap) {
	const bs = 4096
	img := make([]byte, bs*64)
	rng := rand.New(rand.NewPCG(1, 2))
	for i := range img {
		img[i] = byte(rng.Uint32())
	}
	b := &Bmap{BlockSize: bs, ImageSize: int64(len(img))}
	for _, r := range [][2]int64{{0, 9}, {16, 47}, {56, 63}} {
		b.Ranges = append(b.Ranges, BmapRange{
			First:    r[0],
			Last:     r[1],
			Checksum: blockChecksum(img[r[0]*bs : (r[1]+1)*bs]),
		})
	}
	return img, b
}

// wrongBlocks counts the mapped blocks of disk that differ from img.
func wrongBlocks(disk, img []byte, b *Bmap) int {
	wrong := 0
	for _, r := range b.Ranges {
		for blk := r.First; blk <= r.Last; blk++ {
			s, e := blk*b.BlockSize, (blk+1)*b.BlockSize
			if !bytes.Equal(disk[s:e], img[s:e]) {
				wrong++
			}
		}
	}
	return wrong
}

// TestSeekableBmapWriteNeedsPositionalWrites is the regression test for
// Windows NVMe flashes that reported success over a corrupted drive. On NVMe
// the seekable block-map writer runs several writers at once, so the drive
// writer they share must write at an explicit offset. Seeking a shared file
// pointer and then writing at it misplaces chunks, and the bmap checksums,
// which cover the image rather than the drive, do not catch it.
func TestSeekableBmapWriteNeedsPositionalWrites(t *testing.T) {
	oldChunk, oldWorkers := bmapChunkSize, bmapWriteConcurrency
	bmapChunkSize = 4096
	bmapWriteConcurrency = writersForStorage(StorageNVMe)
	defer func() { bmapChunkSize, bmapWriteConcurrency = oldChunk, oldWorkers }()

	img, b := buildRandomBmapImage()
	comp := encodeSeekable(t, img, int(b.BlockSize))
	mapped := mappedBytes(b) / b.BlockSize

	for _, tc := range []struct {
		name       string
		positional bool
	}{
		{"seek then write (old handleWriterAt)", false},
		{"positional write", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			si, err := openSeekableZstdFromReader(bytes.NewReader(comp))
			if err != nil {
				t.Fatalf("open seekable: %v", err)
			}
			defer si.Close()
			// A drive larger than the image, as real ones are, so a write
			// the shared file pointer misplaces past the image still lands.
			disk := newSyncHandleDisk(2 * b.ImageSize)
			pwrite := disk.seekThenWrite
			if tc.positional {
				pwrite = disk.positionalWrite
			}
			if err := applyBmapSeekable(si, sectorWriterAt{pwrite: pwrite}, b, func(int64) {}); err != nil {
				t.Fatalf("applyBmapSeekable: %v", err)
			}
			wrong := wrongBlocks(disk.buf, img, b)
			t.Logf("%d of %d mapped blocks wrong", wrong, mapped)
			if !tc.positional && wrong == 0 {
				t.Fatalf("seek-then-write with %d writers left the drive intact; the test no longer exercises concurrent writers", bmapWriteConcurrency)
			}
			if tc.positional && wrong != 0 {
				t.Fatalf("%d of %d mapped blocks written at the wrong offset", wrong, mapped)
			}
		})
	}
}

func TestSectorWriterAtPadsToWholeSectors(t *testing.T) {
	var got []byte
	var gotOff int64
	w := sectorWriterAt{pwrite: func(p []byte, off int64) (int, error) {
		got, gotOff = append([]byte(nil), p...), off
		return len(p), nil
	}}
	p := bytes.Repeat([]byte{0xab}, 700)
	n, err := w.WriteAt(p, 8192)
	if err != nil || n != len(p) {
		t.Fatalf("WriteAt = %d, %v; want %d, nil", n, err, len(p))
	}
	if gotOff != 8192 || len(got) != 1024 || !bytes.Equal(got[:700], p) || !isAll(got[700:], 0) {
		t.Fatalf("drive got %d bytes at %d, want p zero-padded to 1024 bytes at 8192", len(got), gotOff)
	}

	short := sectorWriterAt{pwrite: func(p []byte, off int64) (int, error) { return len(p) - 512, nil }}
	if _, err := short.WriteAt(make([]byte, 4096), 0); err == nil {
		t.Fatal("short write: want error, got nil")
	}
}
