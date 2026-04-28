// Protocol constants translated from NVIDIA tegrarcm nv3p.h
// (BSD 3-Clause License, Copyright (c) 2011 NVIDIA CORPORATION)
package nv3p

// Protocol version
const Version = uint32(1)

// Commands
const (
	CmdGetPlatformInfo = uint32(0x01)
	CmdGetBCT          = uint32(0x02)
	CmdDLBCT           = uint32(0x04)
	CmdDLBL            = uint32(0x06)
	CmdDLPartition     = uint32(0x08)
	CmdStatus          = uint32(0x0a)
	CmdReset           = uint32(0x0e)
)

// NACK codes
const (
	NACKSuccess = uint32(0x1)
	NACKBadCmd  = uint32(0x2)
	NACKBadData = uint32(0x3)
)

// Packet types
const (
	PacketTypeCmd       = uint32(0x1)
	PacketTypeData      = uint32(0x2)
	PacketTypeEncrypted = uint32(0x3)
	PacketTypeACK       = uint32(0x4)
	PacketTypeNACK      = uint32(0x5)
)

// Packet header/footer sizes in bytes (from nv3p.h NV3P_PACKET_SIZE_* constants)
const (
	sizeBasic     = 3 * 4 // version + type + sequence
	sizeCommand   = 2 * 4 // args_length + command
	sizeData      = 1 * 4 // data_length
	sizeEncrypted = 1 * 4
	sizeFooter    = 1 * 4 // checksum
	sizeACK       = 0 * 4
	sizeNACK      = 1 * 4 // nack_code

	StringMax = 32
)

// Boot device types (NV3P_DEV_TYPE_*)
const (
	DevTypeNAND          = uint32(0x1)
	DevTypeEMMC          = uint32(0x2)
	DevTypeSPI           = uint32(0x3)
	DevTypeIDE           = uint32(0x4)
	DevTypeNANDx16       = uint32(0x5)
	DevTypeSNOR          = uint32(0x6)
	DevTypeMuxOneNAND    = uint32(0x7)
	DevTypeMobileLBANAND = uint32(0x8)
)
