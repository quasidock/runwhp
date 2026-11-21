// Package ext4 makes empty ext4 file systems for runwhp's writable disks, the
// way mke2fs would, and reads back whether one was left clean.
//
// A new file system is mostly holes. Inode tables, bitmaps of untouched
// groups and the journal are never written: the disk is a sparse file whose
// holes read as zeros, and the group descriptors tell the kernel those
// groups are uninitialized and their inode tables already zeroed. Formatting
// 64 GiB writes a few hundred KiB.
//
// The layout follows mke2fs with 4 KiB blocks: flex_bg groups of 16,
// sparse_super backups, 256-byte inodes at one per 16 KiB, group descriptor
// checksums (gdt_csum), extents, directory indexes and, if asked, an internal
// journal of up to 64 MiB. There is no resize inode, no 64bit feature, and so
// no file system larger than 16 TiB.
package ext4

import (
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"time"
)

var (
	le = binary.LittleEndian
	be = binary.BigEndian
)

// Sizes, in bytes, that Format accepts.
const (
	MinSize = 64 << 20
	MaxSize = (1<<32 - 1) * blockSize
)

const (
	blockSize      = 4096
	blocksPerGroup = 8 * blockSize // one block bitmap's worth
	inodeSize      = 256
	inodeRatio     = 16 << 10
	inodesPerBlock = blockSize / inodeSize
	descSize       = 32
	logFlex        = 4
	flexSize       = 1 << logFlex
	extraIsize     = 32

	sbOffset = 1024 // the primary superblock's byte offset

	rootIno      = 2
	journalIno   = 8
	firstIno     = 11 // also lost+found
	lostFoundIno = 11

	// Group descriptor flags.
	bgInodeUninit = 0x1
	bgBlockUninit = 0x2
	bgItableZero  = 0x4

	// Superblock features.
	compatHasJournal = 0x4
	compatExtAttr    = 0x8
	compatDirIndex   = 0x20

	incompatFiletype = 0x2
	incompatRecover  = 0x4
	incompatExtents  = 0x40
	incompatFlexBg   = 0x200

	roCompatSparseSuper = 0x1
	roCompatLargeFile   = 0x2
	roCompatHugeFile    = 0x8
	roCompatGdtCsum     = 0x10
	roCompatDirNlink    = 0x20
	roCompatExtraIsize  = 0x40

	extentsFlag  = 0x80000
	extentMagic  = 0xf30a
	jbd2Magic    = 0xc03b3998
	jbd2SBv2     = 4
	ext4Magic    = 0xef53
	stateValid   = 1
	stateErrors  = 2
	maxExtentLen = 32768
)

// Options control Format.
type Options struct {
	// Journal adds an internal journal, so the file system survives the VM
	// stopping at any moment. A disk thrown away after the run needs none.
	Journal bool
	// UUID and HashSeed are random when zero.
	UUID, HashSeed [16]byte
	// Label is the volume name, at most 16 bytes.
	Label string
	// Now stamps the superblock and the first inodes; zero means time.Now.
	Now time.Time
}

// geometry is where everything goes in a file system of a given size.
type geometry struct {
	blocks  uint64 // in the file system, which may be smaller than the disk
	groups  uint32
	ipg     uint32 // inodes per group
	itb     uint32 // inode table blocks per group
	gdt     uint32 // group descriptor blocks
	journal uint32 // journal blocks, 0 for none
}

func newGeometry(size int64, journal bool) (geometry, error) {
	if size < MinSize || size > MaxSize {
		return geometry{}, fmt.Errorf("ext4: a file system must be between %d MiB and %d TiB, not %d bytes", MinSize>>20, (MaxSize+1)>>40, size)
	}
	g := geometry{blocks: uint64(size) / blockSize}
	for {
		g.groups = uint32((g.blocks + blocksPerGroup - 1) / blocksPerGroup)
		inodes := g.blocks * blockSize / inodeRatio
		ipg := (inodes + uint64(g.groups) - 1) / uint64(g.groups)
		ipg = (ipg + inodesPerBlock - 1) / inodesPerBlock * inodesPerBlock
		g.ipg = uint32(min(max(ipg, inodesPerBlock), blocksPerGroup))
		g.itb = g.ipg / inodesPerBlock
		g.gdt = (g.groups*descSize + blockSize - 1) / blockSize
		// As mke2fs does, drop a last group too small to be worth its
		// metadata.
		rem := g.blocks % blocksPerGroup
		overhead := uint64(3 + g.itb + g.gdt)
		if rem != 0 && rem < overhead+50 && g.groups > 1 {
			g.blocks -= rem
			continue
		}
		break
	}
	if journal {
		switch {
		case g.blocks < 32768:
			g.journal = 1024
		case g.blocks < 256<<10:
			g.journal = 4096
		case g.blocks < 512<<10:
			g.journal = 8192
		default:
			g.journal = 16384
		}
	}
	for l := uint32(0); l < g.groups; l += flexSize {
		end := g.metaEnd(l)
		if l == 0 {
			end = g.journalStart() + uint64(g.journal)
		}
		if end > g.firstBlock(l)+g.groupBlocks(l) {
			return geometry{}, fmt.Errorf("ext4: metadata of groups %d-%d does not fit in group %d", l, l+g.flexCount(l)-1, l)
		}
	}
	return g, nil
}

func (g *geometry) firstBlock(i uint32) uint64 { return uint64(i) * blocksPerGroup }

func (g *geometry) groupBlocks(i uint32) uint64 {
	return min(blocksPerGroup, g.blocks-g.firstBlock(i))
}

// hasSuper reports whether group i holds a copy of the superblock and the
// group descriptors: groups 0 and 1, and powers of 3, 5 and 7.
func hasSuper(i uint32) bool {
	if i <= 1 {
		return true
	}
	for _, b := range []uint32{3, 5, 7} {
		n := b
		for n < i {
			n *= b
		}
		if n == i {
			return true
		}
	}
	return false
}

func (g *geometry) superBlocks(i uint32) uint64 {
	if hasSuper(i) {
		return 1 + uint64(g.gdt)
	}
	return 0
}

func (g *geometry) flexCount(l uint32) uint32 { return min(flexSize, g.groups-l) }

// metaBase is where the leader l of a flex group starts packing the bitmaps
// and inode tables of its groups: block bitmaps, then inode bitmaps, then
// inode tables.
func (g *geometry) metaBase(l uint32) uint64 { return g.firstBlock(l) + g.superBlocks(l) }

func (g *geometry) blockBitmap(i uint32) uint64 {
	l := i &^ (flexSize - 1)
	return g.metaBase(l) + uint64(i-l)
}

func (g *geometry) inodeBitmap(i uint32) uint64 {
	l := i &^ (flexSize - 1)
	return g.metaBase(l) + uint64(g.flexCount(l)) + uint64(i-l)
}

func (g *geometry) inodeTable(i uint32) uint64 {
	l := i &^ (flexSize - 1)
	return g.metaBase(l) + 2*uint64(g.flexCount(l)) + uint64(i-l)*uint64(g.itb)
}

func (g *geometry) metaEnd(l uint32) uint64 {
	n := uint64(g.flexCount(l))
	return g.metaBase(l) + 2*n + n*uint64(g.itb)
}

// Group 0 holds, after its flex group's metadata, the root directory's
// block, lost+found's, and the journal.
func (g *geometry) rootBlock() uint64      { return g.metaEnd(0) }
func (g *geometry) lostFoundBlock() uint64 { return g.metaEnd(0) + 1 }
func (g *geometry) journalStart() uint64   { return g.metaEnd(0) + 2 }

// used returns how many blocks of group i are in use when the file system is
// new. They are always a run from the group's first block.
func (g *geometry) used(i uint32) uint64 {
	n := g.superBlocks(i)
	if i%flexSize == 0 {
		n = g.metaEnd(i) - g.firstBlock(i)
	}
	if i == 0 {
		n = g.journalStart() + uint64(g.journal)
	}
	return n
}

func (g *geometry) flags(i uint32) uint16 {
	switch {
	case i == 0:
		return bgItableZero
	case i%flexSize == 0 || i == g.groups-1:
		// A leader's bitmap must say where the other groups' metadata is,
		// which the kernel would not work out for itself; mke2fs never
		// leaves the last group's bitmap uninitialized either.
		return bgInodeUninit | bgItableZero
	}
	return bgInodeUninit | bgBlockUninit | bgItableZero
}

// Format writes an empty ext4 file system of size bytes to w, which must
// read as zeros wherever Format does not write, as a new sparse file does.
func Format(w io.WriterAt, size int64, o Options) error {
	g, err := newGeometry(size, o.Journal)
	if err != nil {
		return err
	}
	if len(o.Label) > 16 {
		return errors.New("ext4: a label is at most 16 bytes")
	}
	if o.UUID == [16]byte{} {
		rand.Read(o.UUID[:])
		o.UUID[6] = o.UUID[6]&0x0f | 0x40 // version 4
		o.UUID[8] = o.UUID[8]&0x3f | 0x80 // RFC 4122 variant
	}
	if o.HashSeed == [16]byte{} {
		rand.Read(o.HashSeed[:])
	}
	if o.Now.IsZero() {
		o.Now = time.Now()
	}
	now := uint32(o.Now.Unix())

	put := func(block uint64, data []byte) error {
		_, err := w.WriteAt(data, int64(block)*blockSize)
		return err
	}

	// Inodes, in the first block of group 0's inode table, which holds
	// inodes 1 to 16.
	itable := make([]byte, blockSize)
	dir := func(ino uint32, mode uint16, links uint16, block uint64) {
		writeInode(itable[(ino-1)*inodeSize:], mode, links, blockSize, now, block, 1)
	}
	dir(rootIno, 0o40755, 3, g.rootBlock())
	dir(lostFoundIno, 0o40700, 2, g.lostFoundBlock())
	var jblocks [60]byte
	if o.Journal {
		in := itable[(journalIno-1)*inodeSize:]
		writeInode(in, 0o100600, 1, uint64(g.journal)*blockSize, now, g.journalStart(), g.journal)
		copy(jblocks[:], in[0x28:0x28+60])
	}
	if err := put(g.inodeTable(0), itable); err != nil {
		return err
	}

	// Directories.
	root := make([]byte, blockSize)
	n := putDirent(root, rootIno, 12, ".")
	n += putDirent(root[n:], rootIno, 12, "..")
	putDirent(root[n:], lostFoundIno, blockSize-n, "lost+found")
	if err := put(g.rootBlock(), root); err != nil {
		return err
	}
	lf := make([]byte, blockSize)
	n = putDirent(lf, lostFoundIno, 12, ".")
	putDirent(lf[n:], rootIno, blockSize-n, "..")
	if err := put(g.lostFoundBlock(), lf); err != nil {
		return err
	}

	// The journal's superblock. The rest of the journal stays a hole: with
	// s_start 0 the journal is empty, and nothing reads past it.
	if o.Journal {
		j := make([]byte, blockSize)
		be.PutUint32(j[0x00:], jbd2Magic)
		be.PutUint32(j[0x04:], jbd2SBv2)
		be.PutUint32(j[0x0c:], blockSize)
		be.PutUint32(j[0x10:], g.journal)
		be.PutUint32(j[0x14:], 1) // s_first
		be.PutUint32(j[0x18:], 1) // s_sequence
		copy(j[0x30:], o.UUID[:])
		be.PutUint32(j[0x40:], 1) // s_nr_users
		copy(j[0x100:], o.UUID[:])
		if err := put(g.journalStart(), j); err != nil {
			return err
		}
	}

	// Bitmaps of the groups whose descriptors do not say "uninitialized".
	for i := uint32(0); i < g.groups; i++ {
		if g.flags(i)&bgBlockUninit != 0 {
			continue
		}
		b := make([]byte, blockSize)
		setBits(b, 0, g.used(i))
		setBits(b, g.groupBlocks(i), blocksPerGroup) // past the end of the file system
		if err := put(g.blockBitmap(i), b); err != nil {
			return err
		}
	}
	ib := make([]byte, blockSize)
	setBits(ib, 0, firstIno)
	setBits(ib, uint64(g.ipg), blocksPerGroup)
	if err := put(g.inodeBitmap(0), ib); err != nil {
		return err
	}

	// Group descriptors, and the superblock with every backup.
	gdt := make([]byte, int(g.gdt)*blockSize)
	var freeBlocks uint64
	for i := uint32(0); i < g.groups; i++ {
		d := gdt[i*descSize:]
		free := g.groupBlocks(i) - g.used(i)
		freeBlocks += free
		freeInodes, unused, dirs := g.ipg, g.ipg, uint16(0)
		if i == 0 {
			freeInodes, unused, dirs = g.ipg-firstIno, g.ipg-firstIno, 2
		}
		le.PutUint32(d[0x00:], uint32(g.blockBitmap(i)))
		le.PutUint32(d[0x04:], uint32(g.inodeBitmap(i)))
		le.PutUint32(d[0x08:], uint32(g.inodeTable(i)))
		le.PutUint16(d[0x0c:], uint16(free))
		le.PutUint16(d[0x0e:], uint16(freeInodes))
		le.PutUint16(d[0x10:], dirs)
		le.PutUint16(d[0x12:], g.flags(i))
		le.PutUint16(d[0x1c:], uint16(unused))
		le.PutUint16(d[0x1e:], descChecksum(o.UUID, i, d[:descSize]))
	}
	sb := make([]byte, 1024)
	le.PutUint32(sb[0x00:], g.ipg*g.groups)
	le.PutUint32(sb[0x04:], uint32(g.blocks))
	le.PutUint32(sb[0x0c:], uint32(freeBlocks))
	le.PutUint32(sb[0x10:], g.ipg*g.groups-firstIno)
	le.PutUint32(sb[0x18:], 2) // s_log_block_size: 1024 << 2
	le.PutUint32(sb[0x1c:], 2) // s_log_cluster_size
	le.PutUint32(sb[0x20:], blocksPerGroup)
	le.PutUint32(sb[0x24:], blocksPerGroup)
	le.PutUint32(sb[0x28:], g.ipg)
	le.PutUint32(sb[0x30:], now)    // s_wtime
	le.PutUint16(sb[0x36:], 0xffff) // s_max_mnt_count: never ask for a check
	le.PutUint16(sb[0x38:], ext4Magic)
	le.PutUint16(sb[0x3a:], stateValid)
	le.PutUint16(sb[0x3c:], 1)   // s_errors: continue
	le.PutUint32(sb[0x40:], now) // s_lastcheck
	le.PutUint32(sb[0x4c:], 1)   // s_rev_level: dynamic
	le.PutUint32(sb[0x54:], firstIno)
	le.PutUint16(sb[0x58:], inodeSize)
	compat := uint32(compatExtAttr | compatDirIndex)
	if o.Journal {
		compat |= compatHasJournal
		le.PutUint32(sb[0xe0:], journalIno)
		sb[0xfd] = 1 // s_jnl_backup_type: s_jnl_blocks holds the inode's blocks
		copy(sb[0x10c:], jblocks[:])
		le.PutUint32(sb[0x10c+16*4:], g.journal*blockSize) // i_size
	}
	le.PutUint32(sb[0x5c:], compat)
	le.PutUint32(sb[0x60:], incompatFiletype|incompatExtents|incompatFlexBg)
	le.PutUint32(sb[0x64:], roCompatSparseSuper|roCompatLargeFile|roCompatHugeFile|roCompatGdtCsum|roCompatDirNlink|roCompatExtraIsize)
	copy(sb[0x68:], o.UUID[:])
	copy(sb[0x78:], o.Label)
	copy(sb[0xec:], o.HashSeed[:])
	sb[0xfc] = 1                  // s_def_hash_version: half MD4
	le.PutUint32(sb[0x100:], 0xc) // s_default_mount_opts: user_xattr, acl
	le.PutUint32(sb[0x108:], now) // s_mkfs_time
	le.PutUint16(sb[0x15c:], extraIsize)
	le.PutUint16(sb[0x15e:], extraIsize)
	le.PutUint32(sb[0x160:], 1) // s_flags: signed directory hash, as on x86
	sb[0x174] = logFlex

	for i := uint32(0); i < g.groups; i++ {
		if !hasSuper(i) {
			continue
		}
		le.PutUint16(sb[0x5a:], uint16(i)) // s_block_group_nr
		off := int64(g.firstBlock(i)) * blockSize
		if i == 0 {
			off = sbOffset
		}
		if _, err := w.WriteAt(sb, off); err != nil {
			return err
		}
		if err := put(g.firstBlock(i)+1, gdt); err != nil {
			return err
		}
	}
	return nil
}

// writeInode fills in an inode whose data is one extent of n blocks at
// start.
func writeInode(in []byte, mode, links uint16, size uint64, now uint32, start uint64, n uint32) {
	le.PutUint16(in[0x00:], mode)
	le.PutUint32(in[0x04:], uint32(size))
	le.PutUint32(in[0x08:], now) // atime
	le.PutUint32(in[0x0c:], now) // ctime
	le.PutUint32(in[0x10:], now) // mtime
	le.PutUint16(in[0x1a:], links)
	le.PutUint32(in[0x1c:], n*(blockSize/512)) // i_blocks, in sectors
	le.PutUint32(in[0x20:], extentsFlag)
	eh := in[0x28:]
	le.PutUint16(eh[0:], extentMagic)
	le.PutUint16(eh[2:], 1) // entries
	le.PutUint16(eh[4:], 4) // max
	le.PutUint16(eh[6:], 0) // depth
	ex := eh[12:]
	le.PutUint32(ex[0:], 0) // first logical block
	le.PutUint16(ex[4:], uint16(n))
	le.PutUint16(ex[6:], uint16(start>>32))
	le.PutUint32(ex[8:], uint32(start))
	le.PutUint32(in[0x6c:], uint32(size>>32))
	le.PutUint16(in[0x80:], extraIsize)
	le.PutUint32(in[0x90:], now) // crtime
}

// putDirent writes a directory entry for a directory and returns its length.
func putDirent(b []byte, ino uint32, recLen int, name string) int {
	le.PutUint32(b[0:], ino)
	le.PutUint16(b[4:], uint16(recLen))
	b[6] = byte(len(name))
	b[7] = 2 // directory
	copy(b[8:], name)
	return recLen
}

// setBits sets bits [from, to) of a bitmap.
func setBits(b []byte, from, to uint64) {
	for i := from; i < to; i++ {
		b[i/8] |= 1 << (i % 8)
	}
}

func descChecksum(uuid [16]byte, group uint32, desc []byte) uint16 {
	var g [4]byte
	le.PutUint32(g[:], group)
	crc := crc16(0xffff, uuid[:])
	crc = crc16(crc, g[:])
	return crc16(crc, desc[:0x1e])
}

// crc16Table is Linux's lib/crc16.c table: CRC-16 with the polynomial
// x^16 + x^15 + x^2 + 1, bit-reversed (0xA001).
var crc16Table = func() (t [256]uint16) {
	for i := range t {
		c := uint16(i)
		for range 8 {
			if c&1 != 0 {
				c = c>>1 ^ 0xa001
			} else {
				c >>= 1
			}
		}
		t[i] = c
	}
	return t
}()

func crc16(crc uint16, data []byte) uint16 {
	for _, b := range data {
		crc = crc>>8 ^ crc16Table[byte(crc)^b]
	}
	return crc
}

// State is what a file system's superblock and journal say about how it was
// left.
type State struct {
	// Clean is true when the file system was unmounted or frozen cleanly:
	// nothing waits in the journal and no error was recorded.
	Clean bool
	// NeedsRecovery means the journal holds transactions not yet written to
	// their place; the kernel replays them on the next mount.
	NeedsRecovery bool
	// Errors means the kernel found the file system damaged.
	Errors     bool
	MountCount int
	// Journal is false for a file system without one, and JournalStart is
	// then 0. Otherwise JournalStart is the journal's s_start, 0 when it is
	// empty.
	Journal      bool
	JournalStart uint32
}

// ReadState reads the state of the ext4 file system in r.
func ReadState(r io.ReaderAt) (State, error) {
	sb := make([]byte, 1024)
	if _, err := r.ReadAt(sb, sbOffset); err != nil {
		return State{}, err
	}
	if le.Uint16(sb[0x38:]) != ext4Magic {
		return State{}, errors.New("ext4: no ext4 superblock")
	}
	s := State{
		NeedsRecovery: le.Uint32(sb[0x60:])&incompatRecover != 0,
		Errors:        le.Uint16(sb[0x3a:])&stateErrors != 0,
		MountCount:    int(le.Uint16(sb[0x34:])),
		Journal:       le.Uint32(sb[0x5c:])&compatHasJournal != 0,
	}
	if s.Journal {
		// The journal's first block, from the backup of its inode's extent
		// in s_jnl_blocks.
		ex := sb[0x10c+12:]
		start := uint64(le.Uint16(ex[6:]))<<32 | uint64(le.Uint32(ex[8:]))
		j := make([]byte, 64)
		if _, err := r.ReadAt(j, int64(start)*blockSize); err != nil {
			return State{}, err
		}
		if be.Uint32(j) != jbd2Magic {
			return State{}, errors.New("ext4: no journal superblock where the superblock says")
		}
		s.JournalStart = be.Uint32(j[0x1c:])
	}
	s.Clean = le.Uint16(sb[0x3a:])&stateValid != 0 && !s.NeedsRecovery && !s.Errors && s.JournalStart == 0
	return s, nil
}
