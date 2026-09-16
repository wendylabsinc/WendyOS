package services

import (
	"fmt"
	"os"
	"unsafe"

	"golang.org/x/sys/unix"
)

// The V4L2 OUTPUT writer for the two-plane data path.
//
// UNVERIFIED ON HARDWARE. The ioctl sequence, the buffer strategy and the
// sequence read-back in this file were derived from the kernel's videodev2.h
// and from v4l2loopback 0.15.4's own source, but none of it has been exercised
// against a live node on a device. It is the part of this feature most likely
// to need adjustment, and it deliberately carries the least logic: everything
// that can be reasoned about without a kernel module lives in
// hub_loopback_binding.go and hub_loopback_pump.go, which are covered by tests.
//
// It reuses the V4L2 vocabulary video_service.go already established for the
// capture path (the vidioc* request numbers, v4l2Buf and its offset accessors,
// v4l2ReqBuffers, v4l2Format). Only the OUTPUT buffer type below is new. The
// offset-accessor style is not a stylistic choice copied for its own sake: it
// is how the existing code avoids depending on Go's struct layout matching a C
// struct that contains a union and an alignment-forcing struct timeval.
//
// # Why this is a hand-written writer and not a GStreamer v4l2sink
//
// The existing network-camera pump (ipcam.Loopback) feeds its nodes with a
// GStreamer pipeline ending in v4l2sink, and reusing it here would have been
// less code. It cannot work for this purpose. The binding depends on reading
// back the sequence the kernel assigned to each frame, which VIDIOC_QBUF
// returns in its own argument; a GStreamer sink issues that ioctl internally
// and surfaces nothing. Inferring the sequence instead, by counting frames
// pushed into the pipeline, is exactly the arrival-order assumption that must
// not be made: an intervening decoder reorders (B-frames) and may drop frames,
// so the Nth frame in is not the Nth frame out.
//
// # Why buffers are round-robined and never dequeued
//
// This is not general V4L2 practice, but it is correct for this module.
// v4l2loopback's vidioc_qbuf validates only is_allocated() and the memory type
// for an OUTPUT buffer; it does not reject one still marked queued or done, so
// a buffer can be re-queued directly. Avoiding the dequeue also avoids
// depending on output-queue dequeue semantics, which differ between this module
// and a real output device.
//
// The mmap'd region is the module's own image buffer, so a reader sees the
// bytes we write with no intervening copy. With loopbackOutputBuffers buffers a
// given one is reused only after that many further frames, which bounds the
// window in which a slow reader could see a buffer change under it.

const (
	// v4l2BufTypeVideoOutput is V4L2_BUF_TYPE_VIDEO_OUTPUT, the counterpart of
	// the capture path's v4l2BufTypeVideoCapture.
	v4l2BufTypeVideoOutput = 2

	// v4l2BufFlagTimestampCopy is V4L2_BUF_FLAG_TIMESTAMP_COPY from
	// linux/videodev2.h: the buffer's timestamp came from the writer and was
	// copied through verbatim, rather than being taken from a system clock.
	//
	// Setting it on the queued buffer is not what makes v4l2loopback honour our
	// timestamp (it latches the flag itself for any nonzero writer timestamp),
	// but it makes the intent explicit at the queue site and it is the value a
	// reader will see, so a consumer can tell a writer-supplied stamp from the
	// module's own. The capture path's sibling value, TIMESTAMP_MONOTONIC, is
	// 0x2000; the flags field also carries the MASK 0xe000 those two live in.
	v4l2BufFlagTimestampCopy = 0x00004000

	// loopbackOutputBuffers is how many buffers the node's OUTPUT queue holds.
	// Four is the conventional minimum that still lets a reader lag a few frames
	// before a buffer is reused underneath it.
	loopbackOutputBuffers = 4

	// loopbackNodeWidth and loopbackNodeHeight are the dimensions advertised on
	// the node. They are metadata on a compressed node: a decoder takes the real
	// picture size from the bitstream's own parameter sets. V4L2 requires a
	// complete format, so a conventional size is set and sizeimage, which is
	// what actually constrains a frame, is set from the hub's own per-frame cap.
	loopbackNodeWidth  = 1920
	loopbackNodeHeight = 1080
)

// v4l2BufLength reads the length field of a v4l2_buffer (offset 72), which
// QUERYBUF fills in with the size of the buffer to map. video_service.go reads
// the same offset inline; it is named here because this file reads it too.
func v4l2BufLength(b *v4l2Buf) uint32 { return *(*uint32)(unsafe.Pointer(&b[72])) }

// v4l2BufSetBytesUsed writes the bytesused field (offset 8): how many bytes of
// the buffer this frame actually occupies.
func v4l2BufSetBytesUsed(b *v4l2Buf, n uint32) { *(*uint32)(unsafe.Pointer(&b[8])) = n }

// v4l2BufSetField writes the field field (offset 16).
func v4l2BufSetField(b *v4l2Buf, f uint32) { *(*uint32)(unsafe.Pointer(&b[16])) = f }

// v4l2OutputWriter writes whole access units to one v4l2loopback node and
// reports the sequence the kernel assigned to each.
type v4l2OutputWriter struct {
	file    *os.File
	buffers [][]byte
	// next is the round-robin index of the buffer the next frame will use.
	next int
	// streaming records whether VIDIOC_STREAMON succeeded, so Close only turns
	// off a stream that was actually turned on.
	streaming bool
}

// openLoopbackFrameWriterPlatform opens a v4l2loopback node's OUTPUT queue and
// prepares it to receive H.264 access units.
//
// On a platform or build without the node this simply fails to open the path,
// which is the correct outcome: the data plane is unavailable and the pump
// reports so, while the control plane is untouched.
func openLoopbackFrameWriterPlatform(path string) (loopbackFrameWriter, error) {
	// V4L2 requires read-write access for the OUTPUT queue even though this side
	// only ever writes.
	file, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		return nil, fmt.Errorf("opening %s: %w", path, err)
	}
	w := &v4l2OutputWriter{file: file}
	if err := w.configure(); err != nil {
		_ = w.Close()
		return nil, err
	}
	return w, nil
}

// configure runs S_FMT, REQBUFS, QUERYBUF plus mmap, and STREAMON.
func (w *v4l2OutputWriter) configure() error {
	fd := uintptr(w.file.Fd())
	name := w.file.Name()

	var vfmt v4l2Format
	vfmt.Type = v4l2BufTypeVideoOutput
	vfmt.Width = loopbackNodeWidth
	vfmt.Height = loopbackNodeHeight
	// A compressed fourcc, because the producer hub carries compressed access
	// units and this path deliberately does not decode: a decoder would destroy
	// the frame correspondence the binding depends on (see
	// frameBindableToLoopback). Consumers that accept a compressed fourcc
	// (ffmpeg, GStreamer) can read the node; consumers that insist on raw pixels
	// cannot, and that is a stated limit of the feature rather than a gap.
	vfmt.PixelFormat = v4l2PixFmtH264
	vfmt.Field = v4l2FieldNone
	vfmt.SizeImage = maxFrameBytes
	if _, _, errno := unix.Syscall(unix.SYS_IOCTL, fd, vidiocSFmt, uintptr(unsafe.Pointer(&vfmt))); errno != 0 {
		return fmt.Errorf("VIDIOC_S_FMT on %s: %w", name, errno)
	}

	var req v4l2ReqBuffers
	req.Count = loopbackOutputBuffers
	req.Type = v4l2BufTypeVideoOutput
	req.Memory = v4l2MemoryMmap
	if _, _, errno := unix.Syscall(unix.SYS_IOCTL, fd, vidiocReqbufs, uintptr(unsafe.Pointer(&req))); errno != 0 {
		return fmt.Errorf("VIDIOC_REQBUFS on %s: %w", name, errno)
	}
	if req.Count == 0 {
		return fmt.Errorf("VIDIOC_REQBUFS on %s allocated no buffers", name)
	}

	for i := uint32(0); i < req.Count; i++ {
		var qbuf v4l2Buf
		qbuf.setIndex(i)
		qbuf.setType(v4l2BufTypeVideoOutput)
		qbuf.setMemory(v4l2MemoryMmap)
		if _, _, errno := unix.Syscall(unix.SYS_IOCTL, fd, vidiocQuerybuf, uintptr(unsafe.Pointer(&qbuf))); errno != 0 {
			return fmt.Errorf("VIDIOC_QUERYBUF %d on %s: %w", i, name, errno)
		}
		mem, err := unix.Mmap(int(fd), int64(qbuf.offset()), int(v4l2BufLength(&qbuf)),
			unix.PROT_READ|unix.PROT_WRITE, unix.MAP_SHARED)
		if err != nil {
			return fmt.Errorf("mmap buffer %d on %s: %w", i, name, err)
		}
		w.buffers = append(w.buffers, mem)
	}

	// Buffers are NOT queued here, unlike the capture path. On the output side a
	// queued buffer is a written frame, so queueing at setup would publish
	// loopbackOutputBuffers frames of uninitialised memory, each consuming a
	// kernel sequence that no binding names.
	bufType := int32(v4l2BufTypeVideoOutput)
	if _, _, errno := unix.Syscall(unix.SYS_IOCTL, fd, vidiocStreamon, uintptr(unsafe.Pointer(&bufType))); errno != 0 {
		return fmt.Errorf("VIDIOC_STREAMON on %s: %w", name, errno)
	}
	w.streaming = true
	return nil
}

// WriteFrame copies one access unit into the next buffer, stamps it with the
// frame's canonical CLOCK_BOOTTIME receipt and queues it, returning the
// sequence the kernel assigned.
//
// # The sequence is the kernel's, and only the kernel's
//
// The returned value is read back out of the ioctl's own buffer struct, and
// that is the entire basis of the binding: v4l2loopback overwrites whatever
// sequence a writer supplies with its internal write_position and copies the
// result back before advancing that counter, so this value names THIS frame and
// is authoritative. Nothing here may substitute a locally computed number for
// the SEQUENCE; doing so would reintroduce precisely the arrival-order
// assumption the design exists to avoid.
//
// # The timestamp, by contrast, is ours
//
// The same v0.15.4 QBUF handler that clobbers the sequence passes the timestamp
// through untouched whenever a writer supplies a nonzero one:
//
//	} else {
//	        bufd->buffer.timestamp = buf->timestamp;
//	        bufd->buffer.flags |= V4L2_BUF_FLAG_TIMESTAMP_COPY;
//	        bufd->buffer.flags &= ~V4L2_BUF_FLAG_TIMESTAMP_MONOTONIC;
//	}
//
// (the if branch it belongs to takes the module's own clock only when the
// writer's timestamp is zero and the COPY flag is not already latched). On the
// read side vidioc_dqbuf's CAPTURE branch copies the whole buffer struct out
// and unset_flags clears only QUEUED and DONE, so both the value and the COPY
// flag reach the reader intact.
//
// So this queues the frame's canonical boottime receipt, truncated to whole
// microseconds by struct timeval. The truncation is exact, not approximate:
// 1e9 is divisible by 1000, so sec*1e6 + usec == boottimeNanos/1000 always. A
// consumer derives the expected value from FrameIdentity.boottime_nanos by that
// same division and needs no extra field on the wire to do it.
//
// This is a SECONDARY key and a cross-check, not a replacement for the
// sequence. Microseconds cannot collide at any realistic frame rate (30 frames
// per second is roughly 33,333 microseconds apart), but the sequence remains
// the primary join because it is the number a reader gets on the same struct it
// dequeues, and because it, not the timestamp, is what makes reader-side drops
// visible as a gap.
func (w *v4l2OutputWriter) WriteFrame(data []byte, boottimeNanos int64) (uint32, error) {
	if len(w.buffers) == 0 {
		return 0, fmt.Errorf("loopback node %s has no buffers", w.file.Name())
	}
	idx := w.next
	mem := w.buffers[idx]
	if len(data) > len(mem) {
		// Refuse rather than truncate. Half an access unit written under a valid
		// sequence would give a consumer a resolvable identity for a frame that
		// cannot be decoded, which is worse than the frame simply being absent.
		return 0, fmt.Errorf("frame of %d bytes exceeds loopback buffer size %d", len(data), len(mem))
	}
	copy(mem, data)

	var qbuf v4l2Buf
	qbuf.setIndex(uint32(idx))
	qbuf.setType(v4l2BufTypeVideoOutput)
	qbuf.setMemory(v4l2MemoryMmap)
	v4l2BufSetBytesUsed(&qbuf, uint32(len(data)))
	v4l2BufSetField(&qbuf, v4l2FieldNone)
	qbuf.setTimestampNanos(boottimeNanos)
	qbuf.setFlags(v4l2BufFlagTimestampCopy)
	if _, _, errno := unix.Syscall(unix.SYS_IOCTL, uintptr(w.file.Fd()), vidiocQbuf, uintptr(unsafe.Pointer(&qbuf))); errno != 0 {
		return 0, fmt.Errorf("VIDIOC_QBUF on %s: %w", w.file.Name(), errno)
	}
	w.next = (w.next + 1) % len(w.buffers)
	return qbuf.sequence(), nil
}

// Close turns the stream off, unmaps the buffers and closes the node. The node
// itself is left in place; only this writer's use of it ends, mirroring
// ipcam.Loopback.Shutdown, which also stops pumps without removing nodes.
func (w *v4l2OutputWriter) Close() error {
	var firstErr error
	if w.streaming {
		bufType := int32(v4l2BufTypeVideoOutput)
		if _, _, errno := unix.Syscall(unix.SYS_IOCTL, uintptr(w.file.Fd()), vidiocStreamoff, uintptr(unsafe.Pointer(&bufType))); errno != 0 {
			firstErr = fmt.Errorf("VIDIOC_STREAMOFF on %s: %w", w.file.Name(), errno)
		}
		w.streaming = false
	}
	for _, mem := range w.buffers {
		if err := unix.Munmap(mem); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	w.buffers = nil
	if err := w.file.Close(); err != nil && firstErr == nil {
		firstErr = err
	}
	return firstErr
}
