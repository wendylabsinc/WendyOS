package episodeexport

// h264Chunk records which indexed read supplied a range of the buffered bytes.
// A read can finish one access unit and begin another, so reads and pictures do
// not have a one-to-one relationship.
type h264Chunk struct {
	end   int
	id    int
	frame cameraIndexLine
}

// h264Assembler assembles indexed reads in one pass over the segment files,
// retaining the current access unit and enough lookahead to recognize a start
// code split across reads. It
// emits complete access units with the timestamp of the read containing their
// first byte. Multiple pictures starting in one read share that recorded time;
// the index cannot supply a finer schedule.
//
// Continuity here means adjacent ranges in the stored segment only. The index
// cannot reveal bytes dropped upstream before the recorder wrote the file.
type h264Assembler struct {
	data         []byte
	chunks       []h264Chunk
	scan         int
	started      bool
	hasVCL       bool
	lastFragment int
	lastSkipped  int
	fragments    int
	emit         func([]byte, int64) error
	skip         func(cameraIndexLine)
}

func (a *h264Assembler) add(frame cameraIndexLine, id int, payload []byte) error {
	a.data = append(a.data, payload...)
	a.chunks = append(a.chunks, h264Chunk{len(a.data), id, frame})
	return a.drain(false)
}

func (a *h264Assembler) drain(final bool) error {
	defer func() {
		// Discard orphaned bytes already scanned, retaining any incomplete
		// prefix/header at scan. Even an all-zero orphan stream stays bounded.
		if !a.started && a.scan > 0 {
			a.skipPrefix(a.scan, false)
			a.consume(a.scan)
		}
	}()
	for a.scan+2 < len(a.data) {
		i := a.scan
		prefix := 0
		if a.data[i] == 0 && a.data[i+1] == 0 {
			if a.data[i+2] == 1 {
				prefix = 3
			} else if a.data[i+2] == 0 && i+3 == len(a.data) {
				return nil // Possible four-byte start code, split before 01.
			} else if i+3 < len(a.data) && a.data[i+2] == 0 && a.data[i+3] == 1 {
				prefix = 4
			}
		}
		if prefix == 0 {
			a.scan++
			continue
		}
		header := i + prefix
		if header >= len(a.data) {
			return nil // The NAL header may arrive in the next read.
		}
		typ := a.data[header] & 0x1f
		vcl := typ == 1 || typ == 5
		if vcl && header+1 >= len(a.data) && !final {
			return nil // first_mb_in_slice may arrive in the next read too.
		}
		if !a.started {
			a.skipPrefix(i, false)
			a.consume(i)
			i, header = 0, prefix
			a.started = true
		}
		// AUD and the leading non-VCL units start the next access unit once
		// a picture has been seen. For ordinary AVC slices, ue(v) == 0 is
		// exactly a leading 1 bit. A subsequent slice with a nonzero
		// first_mb_in_slice belongs to the current picture, even when the
		// transport put its start code at the beginning of another read.
		leading := typ == 6 || typ == 7 || typ == 8 || typ == 9 || (typ >= 14 && typ <= 18)
		firstSlice := vcl && header+1 < len(a.data) && a.data[header+1]&0x80 != 0
		if a.hasVCL && (leading || firstSlice) {
			if err := a.emitPrefix(i); err != nil {
				return err
			}
			a.consume(i)
			header = prefix
			a.hasVCL = false
		}
		a.hasVCL = a.hasVCL || vcl
		a.scan = header + 1
	}
	return nil
}

// flush ends an explicitly discontinuous stored range, or the stream. A bad
// record must not become the anchor for the next readable continuation.
func (a *h264Assembler) flush() error {
	if err := a.drain(true); err != nil {
		return err
	}
	if a.started {
		if err := a.emitPrefix(len(a.data)); err != nil {
			return err
		}
	} else {
		a.skipPrefix(len(a.data), true)
	}
	a.data, a.chunks = nil, nil
	a.scan, a.started, a.hasVCL = 0, false, false
	return nil
}

func (a *h264Assembler) emitPrefix(n int) error {
	first := a.chunks[0]
	for _, chunk := range a.chunks {
		if chunk.id != first.id && chunk.id > a.lastFragment {
			a.fragments++
			a.lastFragment = chunk.id
		}
		if chunk.end >= n {
			break
		}
	}
	return a.emit(a.data[:n], first.frame.CanonicalEpisodeNanos)
}

// Only wholly discarded records are skipped. A read with orphaned leading
// bytes but a usable picture later in it is still usable.
func (a *h264Assembler) skipPrefix(n int, final bool) {
	for _, chunk := range a.chunks {
		if chunk.end > n && !final {
			break
		}
		if chunk.id > a.lastSkipped {
			a.skip(chunk.frame)
			a.lastSkipped = chunk.id
		}
	}
}

func (a *h264Assembler) consume(n int) {
	a.data = a.data[n:]
	a.scan = max(0, a.scan-n)
	kept := a.chunks[:0]
	for _, chunk := range a.chunks {
		if chunk.end > n {
			chunk.end -= n
			kept = append(kept, chunk)
		}
	}
	a.chunks = kept
}
