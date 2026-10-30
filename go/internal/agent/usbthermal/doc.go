// Package usbthermal drives USB thermal cameras that have no kernel driver.
//
// The first one is the TOPDON TC001 Max (USB 3474:4962, "TOPDON TECHNOLOGY
// Co., Ltd.", an InfiRay core: vendor 0x3474 is Yantai IRay/Raytron). Unlike
// the original TC001 it is not a UVC camera. It enumerates as one vendor-class
// function (class 0xEF, interface association 0xFF/0xF0) with two interfaces
// and no video descriptors at all, so no /dev/videoN ever appears and V4L2,
// GStreamer's v4l2src and the agent's raw tap cannot see it:
//
//	interface 0  0xFF/0xF0/0x00  bulk IN 0x82, bulk OUT 0x05  (commands go over ep0)
//	interface 1  0xFF/0xF0/0x01  alt 0: no endpoints (idle)
//	                             alt 1: bulk IN 0x81, bulk OUT 0x04 (the stream)
//
// So the agent drives it itself, from userspace, through usbfs
// (/dev/bus/usb/BBB/DDD) with plain ioctls. The agent builds with
// CGO_ENABLED=0, which rules out libusb; the handful of ioctls needed are in
// usbdevfs_linux.go. Only one process can own the interfaces, and that has to
// be the agent, which then shares the frames with every subscriber.
//
// # Protocol
//
// The command protocol is the one the community documented for the same
// vendor's Thermal Master P3 (protocol.go has the details): 18-byte commands
// sent with vendor control requests to interface 0, a busy bit to poll, a
// response to read. Starting the stream is start_stream, alternate setting 1
// on interface 1, vendor request 0xEE, a pause, one throwaway read, and
// start_stream again; stopping is alternate setting 0. The model register
// reads "TC001 Max".
//
// # Stream and frame layout
//
// Measured on exxon-01 (2026-10-30), USB 2.0 high speed: about 28 MB/s of
// 512-byte packets, the thermal core and the visible camera multiplexed into
// one stream. A 12-byte UVC-style payload header starts a composite frame
// every 1,777,664 bytes; each composite frame holds a visible picture and two
// whole thermal blocks, at offsets that change from frame to frame (Parser has
// the evidence). One thermal block is 386 rows of 512 bytes:
//
//	rows   0..191  picture, 256x192, YUYV with every chroma byte 0x80 (grey)
//	rows 192..383  temperatures, 256x192 little-endian uint16, 1/64 K
//	row      384   metadata: zeros, then counters (word 64 is a frame counter)
//	row      385   end-of-block row: 0xFF 0x00 x 256
//
// Four consecutive blocks decoded to a room at 24.2-28.2 degC (median 24.5),
// their temperatures correlating 0.995 with the picture's grey level; the
// frame counter advanced 21901, 21902, 21903, 21904.
//
// A block that straddles two composite frames arrives with the next frame's
// 12-byte header inserted inside it, about one frame in fifteen. The parser
// removes the header and delivers the block whole; before it did, the header's
// clock words reached apps as 300-750 degC pixels. As a second line of defence
// the camera withholds from raw subscribers any frame whose temperature rows
// cannot be a measurement (RawFrame.CheckTemperatures).
//
// The first 384 rows are exactly the 256x384 stacked layout of the original
// TC001's UVC mode (picture on top, 1/64 K temperatures below), so the raw tap
// serves them unchanged and existing TC001 consumers decode them as they are.
// The picture half is also offered on its own as a Frame, for an encoder.
//
// # Credits
//
// The protocol was worked out by others first. This package is written from
// scratch against measurements of the TC001 Max, using their documentation as
// the map:
//
//   - jvdillon/p3-ir-camera (Apache-2.0): the P3 protocol write-up, commands,
//     start sequence and the 1/64 K scale, with research by @aeternium.
//   - xaionaro-go/thermalmaster (CC0): reverse-engineering notes and the AC020
//     device-type table, which puts "TC001 Max" in the same family as the P3
//     and P1 -- the hint that this protocol would work at all.
//   - ferrolho/thermal-master-viewer (Apache-2.0): a WebUSB P1/P3 driver.
//   - leswright1977/PyThermalCamera: the original TC001's 256x384 layout.
package usbthermal
