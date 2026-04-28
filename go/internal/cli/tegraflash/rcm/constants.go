// Protocol constants translated from NVIDIA tegrarcm rcm.h and usb.h
// (BSD 3-Clause License, Copyright (c) 2011-2016 NVIDIA CORPORATION)
package rcm

// USB identifiers
const (
	VendorNVIDIA = 0x0955

	// USB PIDs follow the NVIDIA pattern: 0x70XX where XX is the lower
	// byte of the chip ID. T234 chip ID byte = 0x23 → PID = 0x7023.
	// TODO: verify 0x7023 against real hardware; tegrarcm_v2 binary
	// (proprietary, in L4T BSP) handles T234 and would confirm this.
	ProductOrin = 0x7023
)

// RCM version encoding: major<<16 | minor
const (
	Version1  = uint32(1)<<16 | uint32(0)
	Version35 = uint32(0x35)<<16 | uint32(1)
	Version40 = uint32(0x40)<<16 | uint32(1)

	// T234 (Orin) — version TBD; use rcm40 format as base.
	// TODO: verify by capturing USB traffic from tegrarcm_v2 on Linux.
	VersionT234 = Version40
)

// RCM opcodes
const (
	CmdNone           = 0x0
	CmdSync           = 0x1
	CmdDLMiniloader   = 0x4
	CmdQueryBRVersion = 0x5
	CmdQueryRCMVersion= 0x6
	CmdQueryBDVersion = 0x7
)

// Security operating modes
const (
	OpModePreProduction = 0x1
	OpModeDevel         = 0x3
	OpModeODMSecure     = 0x4
	OpModeODMOpen       = 0x5
	OpModeODMSecurePKC  = 0x6
)

// Message geometry (from rcm.h struct layouts)
const (
	MinMsgLength    = 1024
	AESBlockSize    = 16
	RSAModulusSize  = 256
	RSASigSize      = RSAModulusSize

	// rcm40_msg_t field offsets
	msgOffLenInsecure = 0
	msgOffModulus     = 4
	msgOffObjectSig   = 4 + RSAModulusSize                   // 0x104
	msgOffReserved    = msgOffObjectSig + AESBlockSize + RSASigSize // 0x214
	msgOffECID        = msgOffReserved + 16                  // 0x224
	msgOffOpcode      = msgOffECID + 16                      // 0x234
	msgOffLenSecure   = msgOffOpcode + 4                     // 0x238
	msgOffPayloadLen  = msgOffLenSecure + 4                  // 0x23c
	msgOffRCMVersion  = msgOffPayloadLen + 4                 // 0x240
	msgOffArgs        = msgOffRCMVersion + 4                 // 0x244
	msgOffPadding     = msgOffArgs + 48                      // 0x274
	msgHeaderSize     = msgOffPadding + 16                   // 0x284 = 644 bytes
)
