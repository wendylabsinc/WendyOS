// nv3p protocol client, translated from NVIDIA tegrarcm nv3p.c
// (BSD 3-Clause License, Copyright (c) 2011 NVIDIA CORPORATION)
//
// Packet format (all little-endian, all uint32):
//
//	CMD:  [version][type][seq][args_len][command][args...][checksum]
//	DATA: [version][type][seq][data_len][data...][checksum]
//	ACK:  [version][type][seq][checksum]
//	NACK: [version][type][seq][code][checksum]
//
// Checksum: ~(sum of all preceding bytes) + 1  (two's complement of byte sum)
// Verify:   header_accum + footer_word == 0  (uint32 wraparound)
package nv3p

import (
	"encoding/binary"
	"fmt"
	"io"
)

// transport is satisfied by rcm.Device after the applet is running.
type transport interface {
	Read([]byte) (int, error)
	Write([]byte) error
}

// Client implements the nv3p protocol over a bulk USB transport.
type Client struct {
	t        transport
	sequence uint32
	recvSeq  uint32

	// buffered reader state (device pads reads with extra bytes)
	buf    [4096]byte
	bufOff int
	bufLen int
}

// NewClient opens an nv3p session on the given transport.
func NewClient(t transport) (*Client, error) {
	return &Client{t: t}, nil
}

// GetPlatformInfo sends NV3P_CMD_GET_PLATFORM_INFO and returns device info.
func (c *Client) GetPlatformInfo() (*PlatformInfo, error) {
	if err := c.sendCmd(CmdGetPlatformInfo, nil); err != nil {
		return nil, err
	}

	var info PlatformInfo
	raw := make([]byte, 6*8+4*4+2*4+4+4*4+4*4) // match nv3p_platform_info_t
	if err := c.recvData(raw); err != nil {
		return nil, err
	}

	r := raw
	for i := range info.UID {
		info.UID[i] = binary.LittleEndian.Uint64(r[:8])
		r = r[8:]
	}
	info.ChipID.ID = binary.LittleEndian.Uint16(r[:2])
	info.ChipID.Major = r[2]
	info.ChipID.Minor = r[3]
	r = r[4:]
	info.SKU = binary.LittleEndian.Uint32(r[:4])
	r = r[4:]
	info.Version = binary.LittleEndian.Uint32(r[:4])
	r = r[4:]
	info.BootDevice = binary.LittleEndian.Uint32(r[:4])
	r = r[4:]
	info.OpMode = binary.LittleEndian.Uint32(r[:4])
	r = r[4:]
	info.DevConfStrap = binary.LittleEndian.Uint32(r[:4])
	r = r[4:]
	info.DevConfFuse = binary.LittleEndian.Uint32(r[:4])
	r = r[4:]
	info.SDRAMConfStrap = binary.LittleEndian.Uint32(r[:4])
	r = r[4:]
	r = r[8:] // reserved[2]
	info.BoardID.BoardNo = binary.LittleEndian.Uint32(r[:4])
	r = r[4:]
	info.BoardID.Fab = binary.LittleEndian.Uint32(r[:4])
	r = r[4:]
	info.BoardID.MemType = binary.LittleEndian.Uint32(r[:4])
	r = r[4:]
	info.BoardID.Freq = binary.LittleEndian.Uint32(r[:4])

	return &info, nil
}

// DownloadBL downloads a bootloader binary to the device, then executes it.
// addr is the SDRAM load address; entry is the execution entry point.
// After this call the device begins executing the payload — no further nv3p
// communication is expected.
func (c *Client) DownloadBL(payload []byte, addr, entry uint32) error {
	args := DLBLArgs{
		Length:  uint64(len(payload)),
		Address: addr,
		Entry:   entry,
	}
	argBytes := make([]byte, 16) // 8+4+4
	binary.LittleEndian.PutUint64(argBytes[0:], args.Length)
	binary.LittleEndian.PutUint32(argBytes[8:], args.Address)
	binary.LittleEndian.PutUint32(argBytes[12:], args.Entry)

	if err := c.sendCmd(CmdDLBL, argBytes); err != nil {
		return err
	}
	return c.sendData(payload)
}

// DlBCT downloads a Boot Configuration Table binary to the device.
// The BCT must be loaded before any partition writes.
func (c *Client) DlBCT(bct []byte) error {
	argBytes := make([]byte, 4)
	binary.LittleEndian.PutUint32(argBytes, uint32(len(bct)))
	if err := c.sendCmd(CmdDLBCT, argBytes); err != nil {
		return err
	}
	return c.sendData(bct)
}

// WritePartition downloads a partition image to the device and writes it to flash.
// id is the partition ID from the partition layout XML.
// partType: use 0x01 for generic partition data.
// TODO: verify DLPartitionArgs layout and CmdDLPartition opcode against T234 hardware.
func (c *Client) WritePartition(id, partType uint32, data []byte) error {
	args := make([]byte, 16)
	binary.LittleEndian.PutUint64(args[0:], uint64(len(data)))
	binary.LittleEndian.PutUint32(args[8:], id)
	binary.LittleEndian.PutUint32(args[12:], partType)
	if err := c.sendCmd(CmdDLPartition, args); err != nil {
		return err
	}
	return c.sendData(data)
}

// Reset sends a reset command to restart the device after flashing.
func (c *Client) Reset() error {
	return c.sendCmd(CmdReset, nil)
}

// sendCmd serialises and sends a CMD packet, then waits for ACK/NACK.
func (c *Client) sendCmd(cmd uint32, args []byte) error {
	argsLen := uint32(len(args))

	// Build packet: basic(12) + command(8) + args + footer(4)
	pktLen := sizeBasic + sizeCommand + int(argsLen) + sizeFooter
	pkt := make([]byte, pktLen)
	p := pkt

	writeHeader(PacketTypeCmd, c.sequence, p)
	p = p[sizeBasic:]

	binary.LittleEndian.PutUint32(p[:4], argsLen)
	binary.LittleEndian.PutUint32(p[4:], cmd)
	p = p[sizeCommand:]

	copy(p, args)
	p = p[argsLen:]

	checksum := cksum(pkt[:pktLen-sizeFooter])
	binary.LittleEndian.PutUint32(p[:4], twosComplement(checksum))

	if err := c.t.Write(pkt); err != nil {
		return fmt.Errorf("nv3p send cmd 0x%02x: %w", cmd, err)
	}
	c.sequence++

	return c.waitACK()
}

// sendData serialises and sends a DATA packet, then waits for ACK/NACK.
func (c *Client) sendData(data []byte) error {
	hdrLen := sizeBasic + sizeData
	pkt := make([]byte, hdrLen+sizeFooter)
	writeHeader(PacketTypeData, c.sequence, pkt)
	binary.LittleEndian.PutUint32(pkt[sizeBasic:], uint32(len(data)))

	// Checksum covers header + data
	sum := cksum(pkt[:hdrLen])
	sum += cksum(data)
	binary.LittleEndian.PutUint32(pkt[hdrLen:], twosComplement(sum))

	// Send header
	if err := c.t.Write(pkt[:hdrLen]); err != nil {
		return fmt.Errorf("nv3p send data header: %w", err)
	}
	// Send data body
	if err := c.t.Write(data); err != nil {
		return fmt.Errorf("nv3p send data body: %w", err)
	}
	// Send checksum footer
	if err := c.t.Write(pkt[hdrLen:]); err != nil {
		return fmt.Errorf("nv3p send data footer: %w", err)
	}
	c.sequence++

	return c.waitACK()
}

// recvData reads a DATA packet from the device and copies the payload into dst.
func (c *Client) recvData(dst []byte) error {
	hdr, accumSum, err := c.recvHeader()
	if err != nil {
		return err
	}
	if hdr.pktType != PacketTypeData {
		return fmt.Errorf("nv3p: expected DATA packet, got type %d", hdr.pktType)
	}
	c.recvSeq = hdr.sequence

	var dataLenBuf [4]byte
	if err := c.readExact(dataLenBuf[:]); err != nil {
		return fmt.Errorf("nv3p recv data length: %w", err)
	}
	dataLen := binary.LittleEndian.Uint32(dataLenBuf[:])
	accumSum += cksum(dataLenBuf[:])

	buf := make([]byte, dataLen)
	if err := c.readExact(buf); err != nil {
		return fmt.Errorf("nv3p recv data body: %w", err)
	}
	accumSum += cksum(buf)

	var footerBuf [4]byte
	if err := c.readExact(footerBuf[:]); err != nil {
		return fmt.Errorf("nv3p recv data footer: %w", err)
	}
	footer := binary.LittleEndian.Uint32(footerBuf[:])
	if accumSum+footer != 0 {
		return fmt.Errorf("nv3p recv data: checksum mismatch")
	}

	copy(dst, buf[:min(len(dst), len(buf))])
	c.sendACK()
	return nil
}

// waitACK reads the ACK or NACK response after sending a command/data packet.
func (c *Client) waitACK() error {
	hdr, accumSum, err := c.recvHeader()
	if err != nil {
		return err
	}

	switch hdr.pktType {
	case PacketTypeACK:
		// nothing extra to read

	case PacketTypeNACK:
		var codeBuf [4]byte
		if err := c.readExact(codeBuf[:]); err != nil {
			return err
		}
		code := binary.LittleEndian.Uint32(codeBuf[:])
		accumSum += cksum(codeBuf[:])
		var footerBuf [4]byte
		if err := c.readExact(footerBuf[:]); err != nil {
			return err
		}
		footer := binary.LittleEndian.Uint32(footerBuf[:])
		if accumSum+footer != 0 {
			return fmt.Errorf("nv3p nack: checksum mismatch")
		}
		return fmt.Errorf("nv3p NACK code 0x%x", code)

	default:
		return fmt.Errorf("nv3p: unexpected packet type %d waiting for ACK", hdr.pktType)
	}

	var footerBuf [4]byte
	if err := c.readExact(footerBuf[:]); err != nil {
		return err
	}
	footer := binary.LittleEndian.Uint32(footerBuf[:])
	if accumSum+footer != 0 {
		return fmt.Errorf("nv3p ack: checksum mismatch")
	}
	if hdr.sequence != c.sequence-1 {
		return fmt.Errorf("nv3p ack: sequence mismatch (got %d, want %d)", hdr.sequence, c.sequence-1)
	}
	return nil
}

// sendACK sends an ACK packet for the last received sequence number.
func (c *Client) sendACK() {
	pkt := make([]byte, sizeBasic+sizeFooter)
	writeHeader(PacketTypeACK, c.recvSeq, pkt)
	sum := cksum(pkt[:sizeBasic])
	binary.LittleEndian.PutUint32(pkt[sizeBasic:], twosComplement(sum))
	_ = c.t.Write(pkt)
}

type header struct {
	version  uint32
	pktType  uint32
	sequence uint32
}

// recvHeader reads the 12-byte basic header and returns it with the running checksum.
func (c *Client) recvHeader() (header, uint32, error) {
	buf := make([]byte, sizeBasic)
	if err := c.readExact(buf); err != nil {
		return header{}, 0, fmt.Errorf("nv3p recv header: %w", err)
	}
	hdr := header{
		version:  binary.LittleEndian.Uint32(buf[0:4]),
		pktType:  binary.LittleEndian.Uint32(buf[4:8]),
		sequence: binary.LittleEndian.Uint32(buf[8:12]),
	}
	if hdr.version != Version {
		return header{}, 0, fmt.Errorf("nv3p: protocol version mismatch (got %d, want %d)", hdr.version, Version)
	}
	return hdr, cksum(buf), nil
}

// readExact fills buf completely, buffering excess bytes for the next call.
// The device sometimes pads responses; this matches the buffering in tegrarcm nv3p_read().
func (c *Client) readExact(buf []byte) error {
	total := 0
	for total < len(buf) {
		if c.bufLen == 0 {
			n, err := c.t.Read(c.buf[:])
			if err != nil && err != io.EOF {
				return err
			}
			c.bufOff = 0
			c.bufLen = n
		}
		avail := c.bufLen
		need := len(buf) - total
		if avail > need {
			avail = need
		}
		copy(buf[total:], c.buf[c.bufOff:c.bufOff+avail])
		c.bufOff += avail
		c.bufLen -= avail
		total += avail
	}
	return nil
}

// writeHeader writes the 12-byte basic header into the start of pkt.
func writeHeader(pktType, sequence uint32, pkt []byte) {
	binary.LittleEndian.PutUint32(pkt[0:], Version)
	binary.LittleEndian.PutUint32(pkt[4:], pktType)
	binary.LittleEndian.PutUint32(pkt[8:], sequence)
}

// cksum is the nv3p checksum: sum of all bytes (uint32 wrapping).
func cksum(data []byte) uint32 {
	var s uint32
	for _, b := range data {
		s += uint32(b)
	}
	return s
}

// twosComplement returns ^sum + 1 so that sum + result == 0 in uint32.
func twosComplement(sum uint32) uint32 {
	return ^sum + 1
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
