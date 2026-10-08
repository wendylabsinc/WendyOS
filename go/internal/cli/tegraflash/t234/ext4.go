package t234

// Minimal read-only ext4 access, just enough to read the device's final
// status and logs out of the flashpkg LUN image (a small mke2fs-defaults
// ext4 filesystem written by the flashing initrd). Supports extent-mapped
// and block-mapped files and classic (non-hashed lookup) directories; no
// journal replay is attempted, which is fine for a filesystem the device
// unmounts before re-exporting.

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"path"
	"strings"
)

// ErrExt4FileNotFound lets optional recovery files be distinguished from a
// corrupt or unsupported filesystem.
var ErrExt4FileNotFound = errors.New("no such file or directory")

const (
	ext4SuperOffset  = 1024
	ext4Magic        = 0xEF53
	ext4RootIno      = 2
	extentsFlag      = 0x80000  // EXT4_EXTENTS_FL
	incompat64Bit    = 0x80     // INCOMPAT_64BIT
	extentMagic      = 0xF30A   //
	ext4NameLen      = 255      //
	maxReadFileBytes = 16 << 20 // sanity cap: flashpkg files are tiny
	ftypeDir         = 2        //
	ftypeRegular     = 1        //
	modeDirMask      = 0x4000   //
	modeRegMask      = 0x8000   //
	modeFmtMask      = 0xF000   //
	maxWalkEntries   = 100000   // runaway-directory guard
)

type ext4FS struct {
	r              io.ReaderAt
	blockSize      int64
	inodeSize      int64
	inodesPerG     int64
	groupDescSize  int64
	firstDataBlock int64
	descBase       int64 // byte offset of the group descriptor table
}

type ext4DirEntry struct {
	Name  string
	Inode int64
	Type  byte
}

func openExt4(r io.ReaderAt) (*ext4FS, error) {
	sb := make([]byte, 1024)
	if _, err := r.ReadAt(sb, ext4SuperOffset); err != nil {
		return nil, fmt.Errorf("reading ext4 superblock: %w", err)
	}
	if binary.LittleEndian.Uint16(sb[56:58]) != ext4Magic {
		return nil, fmt.Errorf("not an ext4 filesystem (bad magic)")
	}
	logBlockSize := binary.LittleEndian.Uint32(sb[24:28])
	fs := &ext4FS{
		r:              r,
		blockSize:      int64(1024) << logBlockSize,
		inodesPerG:     int64(binary.LittleEndian.Uint32(sb[40:44])),
		inodeSize:      int64(binary.LittleEndian.Uint16(sb[88:90])),
		firstDataBlock: int64(binary.LittleEndian.Uint32(sb[20:24])),
	}
	featureIncompat := binary.LittleEndian.Uint32(sb[96:100])
	fs.groupDescSize = 32
	if featureIncompat&incompat64Bit != 0 {
		if s := binary.LittleEndian.Uint16(sb[254:256]); s >= 32 {
			fs.groupDescSize = int64(s)
		} else {
			fs.groupDescSize = 64
		}
	}
	if fs.inodeSize == 0 {
		fs.inodeSize = 128
	}
	// Group descriptors start in the block after the superblock.
	fs.descBase = (fs.firstDataBlock + 1) * fs.blockSize
	return fs, nil
}

// inodeOffset locates inode ino's on-disk byte offset via its group's
// descriptor (inode table pointer: lo at +8, hi at +40 for 64-bit layouts).
func (fs *ext4FS) inodeOffset(ino int64) (int64, error) {
	if ino < 1 {
		return 0, fmt.Errorf("invalid inode %d", ino)
	}
	group := (ino - 1) / fs.inodesPerG
	index := (ino - 1) % fs.inodesPerG
	desc := make([]byte, fs.groupDescSize)
	if _, err := fs.r.ReadAt(desc, fs.descBase+group*fs.groupDescSize); err != nil {
		return 0, fmt.Errorf("reading group descriptor %d: %w", group, err)
	}
	table := int64(binary.LittleEndian.Uint32(desc[8:12]))
	if fs.groupDescSize >= 64 {
		table |= int64(binary.LittleEndian.Uint32(desc[40:44])) << 32
	}
	return table*fs.blockSize + index*fs.inodeSize, nil
}

type ext4Inode struct {
	mode  uint16
	size  int64
	flags uint32
	block [60]byte
}

func (fs *ext4FS) readInode(ino int64) (*ext4Inode, error) {
	off, err := fs.inodeOffset(ino)
	if err != nil {
		return nil, err
	}
	raw := make([]byte, 128)
	if _, err := fs.r.ReadAt(raw, off); err != nil {
		return nil, fmt.Errorf("reading inode %d: %w", ino, err)
	}
	n := &ext4Inode{
		mode:  binary.LittleEndian.Uint16(raw[0:2]),
		size:  int64(binary.LittleEndian.Uint32(raw[4:8])) | int64(binary.LittleEndian.Uint32(raw[108:112]))<<32,
		flags: binary.LittleEndian.Uint32(raw[32:36]),
	}
	copy(n.block[:], raw[40:100])
	return n, nil
}

// fileBlocks returns the ordered list of fs block numbers backing an inode
// (0 for holes). Handles extent trees and legacy direct/indirect maps far
// enough for small files (direct + single indirect).
func (fs *ext4FS) fileBlocks(n *ext4Inode) ([]int64, error) {
	nblocks := (n.size + fs.blockSize - 1) / fs.blockSize
	if n.size > maxReadFileBytes {
		return nil, fmt.Errorf("file too large (%d bytes)", n.size)
	}
	blocks := make([]int64, nblocks)
	if n.flags&extentsFlag != 0 {
		if err := fs.walkExtents(n.block[:], blocks, 0); err != nil {
			return nil, err
		}
		return blocks, nil
	}
	// Legacy block map: 12 direct + 1 single-indirect covers > 4 MiB at 4k.
	for i := int64(0); i < nblocks && i < 12; i++ {
		blocks[i] = int64(binary.LittleEndian.Uint32(n.block[4*i : 4*i+4]))
	}
	if nblocks > 12 {
		ind := int64(binary.LittleEndian.Uint32(n.block[48:52]))
		perBlock := fs.blockSize / 4
		if nblocks > 12+perBlock {
			return nil, fmt.Errorf("file needs double-indirect blocks (unsupported)")
		}
		buf := make([]byte, fs.blockSize)
		if _, err := fs.r.ReadAt(buf, ind*fs.blockSize); err != nil {
			return nil, fmt.Errorf("reading indirect block: %w", err)
		}
		for i := int64(12); i < nblocks; i++ {
			blocks[i] = int64(binary.LittleEndian.Uint32(buf[4*(i-12) : 4*(i-12)+4]))
		}
	}
	return blocks, nil
}

// walkExtents fills blocks from an extent node (inline in the inode or in an
// index-pointed block), recursing through index nodes.
func (fs *ext4FS) walkExtents(node []byte, blocks []int64, depthGuard int) error {
	if depthGuard > 8 {
		return fmt.Errorf("extent tree too deep")
	}
	if binary.LittleEndian.Uint16(node[0:2]) != extentMagic {
		return fmt.Errorf("bad extent node magic")
	}
	entries := int(binary.LittleEndian.Uint16(node[2:4]))
	depth := binary.LittleEndian.Uint16(node[6:8])
	for i := 0; i < entries; i++ {
		e := node[12+12*i : 24+12*i]
		if depth == 0 {
			logical := int64(binary.LittleEndian.Uint32(e[0:4]))
			length := int64(binary.LittleEndian.Uint16(e[4:6]))
			if length > 32768 {
				length -= 32768 // unwritten extent: still mapped
			}
			physical := int64(binary.LittleEndian.Uint16(e[6:8]))<<32 |
				int64(binary.LittleEndian.Uint32(e[8:12]))
			for j := int64(0); j < length; j++ {
				if idx := logical + j; idx >= 0 && idx < int64(len(blocks)) {
					blocks[idx] = physical + j
				}
			}
			continue
		}
		child := int64(binary.LittleEndian.Uint32(e[4:8])) |
			int64(binary.LittleEndian.Uint16(e[8:10]))<<32
		buf := make([]byte, fs.blockSize)
		if _, err := fs.r.ReadAt(buf, child*fs.blockSize); err != nil {
			return fmt.Errorf("reading extent index block: %w", err)
		}
		if err := fs.walkExtents(buf, blocks, depthGuard+1); err != nil {
			return err
		}
	}
	return nil
}

// readInodeData reads an inode's full contents.
func (fs *ext4FS) readInodeData(n *ext4Inode) ([]byte, error) {
	blocks, err := fs.fileBlocks(n)
	if err != nil {
		return nil, err
	}
	out := make([]byte, n.size)
	for i, b := range blocks {
		if b == 0 {
			continue // hole
		}
		start := int64(i) * fs.blockSize
		end := start + fs.blockSize
		if end > n.size {
			end = n.size
		}
		if _, err := fs.r.ReadAt(out[start:end], b*fs.blockSize); err != nil {
			return nil, fmt.Errorf("reading data block %d: %w", b, err)
		}
	}
	return out, nil
}

// listDir parses an inode's directory entries (classic linear format; ext4
// dir_index directories keep the linear entries too, so this covers both).
func (fs *ext4FS) listDir(n *ext4Inode) ([]ext4DirEntry, error) {
	if n.mode&modeFmtMask != modeDirMask {
		return nil, fmt.Errorf("not a directory")
	}
	data, err := fs.readInodeData(n)
	if err != nil {
		return nil, err
	}
	var out []ext4DirEntry
	for base := int64(0); base+fs.blockSize <= int64(len(data)); base += fs.blockSize {
		off := base
		for off < base+fs.blockSize && len(out) < maxWalkEntries {
			rec := data[off:]
			if len(rec) < 8 {
				break
			}
			ino := int64(binary.LittleEndian.Uint32(rec[0:4]))
			recLen := int64(binary.LittleEndian.Uint16(rec[4:6]))
			nameLen := int(rec[6])
			ftype := rec[7]
			if recLen < 8 || off+recLen > base+fs.blockSize {
				break
			}
			if ino != 0 && nameLen > 0 && nameLen <= ext4NameLen && 8+nameLen <= int(recLen) {
				out = append(out, ext4DirEntry{
					Name:  string(rec[8 : 8+nameLen]),
					Inode: ino,
					Type:  ftype,
				})
			}
			off += recLen
		}
	}
	return out, nil
}

// lookup resolves a /-separated path to an inode number.
func (fs *ext4FS) lookup(p string) (int64, error) {
	ino := int64(ext4RootIno)
	for _, comp := range strings.Split(path.Clean("/"+p), "/") {
		if comp == "" || comp == "." {
			continue
		}
		n, err := fs.readInode(ino)
		if err != nil {
			return 0, err
		}
		entries, err := fs.listDir(n)
		if err != nil {
			return 0, fmt.Errorf("listing directory for %q: %w", comp, err)
		}
		found := int64(0)
		for _, e := range entries {
			if e.Name == comp {
				found = e.Inode
				break
			}
		}
		if found == 0 {
			return 0, fmt.Errorf("%s: %w", p, ErrExt4FileNotFound)
		}
		ino = found
	}
	return ino, nil
}

// Ext4ReadFile reads a regular file out of an ext4 image.
func Ext4ReadFile(r io.ReaderAt, filePath string) ([]byte, error) {
	fs, err := openExt4(r)
	if err != nil {
		return nil, err
	}
	ino, err := fs.lookup(filePath)
	if err != nil {
		return nil, err
	}
	n, err := fs.readInode(ino)
	if err != nil {
		return nil, err
	}
	if n.mode&modeFmtMask != modeRegMask {
		return nil, fmt.Errorf("%s: not a regular file", filePath)
	}
	return fs.readInodeData(n)
}

// Ext4ListDir lists the entry names of a directory in an ext4 image,
// excluding "." and "..".
func Ext4ListDir(r io.ReaderAt, dirPath string) ([]string, error) {
	fs, err := openExt4(r)
	if err != nil {
		return nil, err
	}
	ino, err := fs.lookup(dirPath)
	if err != nil {
		return nil, err
	}
	n, err := fs.readInode(ino)
	if err != nil {
		return nil, err
	}
	entries, err := fs.listDir(n)
	if err != nil {
		return nil, err
	}
	var names []string
	for _, e := range entries {
		if e.Name != "." && e.Name != ".." {
			names = append(names, e.Name)
		}
	}
	return names, nil
}
