//go:build darwin || linux || windows

package t234

import (
	"bytes"
	"errors"
	"io"
	"testing"
	"testing/synctest"
)

type imageReaderFunc func([]byte) (int, error)

func (f imageReaderFunc) Read(p []byte) (int, error) { return f(p) }

type imageWriterFunc func([]byte, int64) (int, error)

func (f imageWriterFunc) WriteAt(p []byte, off int64) (int, error) { return f(p, off) }

func TestCopyImageAtContents(t *testing.T) {
	for _, tc := range []struct {
		name string
		size int
	}{
		{"empty", 0},
		{"partial-sector", 13},
		{"whole-sector", sectorSize},
		{"whole-chunk", writeChunk},
		{"reused-buffer-tail", 3*writeChunk + 13},
	} {
		t.Run(tc.name, func(t *testing.T) {
			src := make([]byte, tc.size)
			for i := range src {
				// Different chunks, including an entire zero chunk: zeros must
				// replace old disk content just like any other image bytes.
				src[i] = byte(i / writeChunk)
			}
			const offset = 2 * sectorSize
			padded := (len(src) + sectorSize - 1) / sectorSize * sectorSize
			dst := bytes.Repeat([]byte{0xa5}, offset+padded+sectorSize)
			want := bytes.Clone(dst)
			copy(want[offset:], src)
			clear(want[offset+len(src) : offset+padded])
			var written, reported int64
			writer := imageWriterFunc(func(p []byte, off int64) (int, error) {
				if off != offset+written || off%sectorSize != 0 || len(p)%sectorSize != 0 {
					t.Errorf("unaligned or out-of-order write: off=%d len=%d written=%d", off, len(p), written)
					return 0, errors.New("invalid write")
				}
				n := copy(dst[off:], p)
				written += int64(n)
				return n, nil
			})
			err := copyImageAt(writer, bytes.NewReader(src), offset, func(done int64) {
				if done <= reported || done > int64(len(src)) || done > written {
					t.Errorf("progress=%d, previous=%d, written=%d", done, reported, written)
				}
				reported = done
			})
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(dst, want) {
				t.Fatal("image, sector padding, or surrounding sectors differ")
			}
			if reported != int64(len(src)) || written != int64(padded) {
				t.Fatalf("reported=%d written=%d, want %d/%d", reported, written, len(src), padded)
			}
		})
	}
}

func TestCopyImageAtReadAhead(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var reads, writes int
		var reported int64
		release := make(chan struct{})
		result := make(chan error, 1)
		src := imageReaderFunc(func(p []byte) (int, error) {
			reads++
			if reads > 4 {
				return 0, io.EOF
			}
			for i := range p {
				p[i] = byte(reads)
			}
			return len(p), nil
		})
		dst := imageWriterFunc(func(p []byte, off int64) (int, error) {
			writes++
			if writes == 1 {
				<-release
			}
			for _, b := range p {
				if b != byte(writes) {
					return 0, errors.New("buffer changed before write completed")
				}
			}
			return len(p), nil
		})
		go func() { result <- copyImageAt(dst, src, 0, func(n int64) { reported = n }) }()
		synctest.Wait()
		// The second read must finish while the first write is blocked, but
		// a third read must wait for a buffer. Progress counts completed writes.
		if reads != 2 || writes != 1 || reported != 0 {
			t.Errorf("blocked pipeline: reads=%d writes=%d progress=%d, want 2/1/0", reads, writes, reported)
		}
		close(release)
		if err := <-result; err != nil {
			t.Fatal(err)
		}
		if writes != 4 || reported != 4*writeChunk {
			t.Fatalf("writes=%d progress=%d, want 4/%d", writes, reported, 4*writeChunk)
		}
	})
}

func TestCopyImageAtWriteFailure(t *testing.T) {
	writeErr := errors.New("USB disconnected")
	for _, wantErr := range []error{writeErr, io.ErrShortWrite} {
		t.Run(wantErr.Error(), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				var calls int
				var reported int64
				dst := imageWriterFunc(func(p []byte, off int64) (int, error) {
					calls++
					if calls == 1 {
						return len(p), nil
					}
					if wantErr == io.ErrShortWrite {
						return len(p) - sectorSize, nil
					}
					return 0, writeErr
				})
				err := copyImageAt(dst, bytes.NewReader(make([]byte, 4*writeChunk)), 0, func(n int64) { reported = n })
				if !errors.Is(err, wantErr) {
					t.Fatalf("error=%v, want %v", err, wantErr)
				}
				if calls != 2 || reported != writeChunk {
					t.Fatalf("calls=%d progress=%d, want 2/%d", calls, reported, writeChunk)
				}
			})
		})
	}
}

func TestCopyImageAtReadFailureWaitsForWrite(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		readErr := errors.New("source read failed")
		started := make(chan struct{})
		release := make(chan struct{})
		result := make(chan error, 1)
		var reads, writes int
		src := imageReaderFunc(func(p []byte) (int, error) {
			reads++
			if reads == 1 {
				return len(p), nil
			}
			<-started
			return 0, readErr
		})
		dst := imageWriterFunc(func(p []byte, off int64) (int, error) {
			writes++
			close(started)
			<-release
			return len(p), nil
		})
		go func() { result <- copyImageAt(dst, src, 0, nil) }()
		synctest.Wait()
		select {
		case err := <-result:
			t.Errorf("returned before the in-flight write finished: %v", err)
		default:
		}
		close(release)
		if err := <-result; !errors.Is(err, readErr) {
			t.Fatalf("error=%v, want %v", err, readErr)
		}
		if writes != 1 {
			t.Fatalf("writes=%d, want 1", writes)
		}
	})
}
