// Command argument types translated from NVIDIA tegrarcm nv3p.h
// (BSD 3-Clause License, Copyright (c) 2011 NVIDIA CORPORATION)
package nv3p

// ChipID mirrors nv3p_chip_id_t.
type ChipID struct {
	ID    uint16
	Major uint8
	Minor uint8
}

// BoardID mirrors nv3p_board_id_t.
type BoardID struct {
	BoardNo uint32
	Fab     uint32
	MemType uint32
	Freq    uint32
}

// PlatformInfo mirrors nv3p_platform_info_t.
// All fields are output from CmdGetPlatformInfo.
type PlatformInfo struct {
	UID            [2]uint64
	ChipID         ChipID
	SKU            uint32
	Version        uint32
	BootDevice     uint32
	OpMode         uint32
	DevConfStrap   uint32
	DevConfFuse    uint32
	SDRAMConfStrap uint32
	Reserved       [2]uint32
	BoardID        BoardID
}

// BCTInfo mirrors nv3p_bct_info_t.
type BCTInfo struct {
	Length uint32
}

// DLBLArgs mirrors nv3p_cmd_dl_bl_t — arguments for CmdDLBL.
type DLBLArgs struct {
	Length  uint64
	Address uint32 // Load address in device SDRAM
	Entry   uint32 // Execution entry point
}

// DLBCTArgs mirrors nv3p_cmd_dl_bct_t.
type DLBCTArgs struct {
	Length uint32
}

// DLPartitionArgs mirrors nv3p_cmd_dl_partition_t — arguments for CmdDLPartition.
// TODO: verify exact field layout against T234 nv3p implementation on hardware.
type DLPartitionArgs struct {
	Length uint64
	PartID uint32
	Type   uint32
}

// Status mirrors nv3p_cmd_status_t.
type Status struct {
	Msg   [StringMax]byte
	Code  uint32
	Flags uint32
}

func (s *Status) Message() string {
	end := 0
	for end < len(s.Msg) && s.Msg[end] != 0 {
		end++
	}
	return string(s.Msg[:end])
}
