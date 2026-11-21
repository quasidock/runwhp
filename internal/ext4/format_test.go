package ext4

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// image is a sparse in-memory disk: what was never written reads as zeros.
type image struct {
	mu      sync.Mutex
	blocks  map[int64][]byte
	written int64
}

func newImage() *image { return &image{blocks: map[int64][]byte{}} }

func (m *image) WriteAt(p []byte, off int64) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.written += int64(len(p))
	for n := 0; n < len(p); {
		b, o := (off+int64(n))/blockSize, int((off+int64(n))%blockSize)
		blk := m.blocks[b]
		if blk == nil {
			blk = make([]byte, blockSize)
			m.blocks[b] = blk
		}
		n += copy(blk[o:], p[n:])
	}
	return len(p), nil
}

func (m *image) ReadAt(p []byte, off int64) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for n := 0; n < len(p); {
		b, o := (off+int64(n))/blockSize, int((off+int64(n))%blockSize)
		c := min(len(p)-n, blockSize-o)
		if blk := m.blocks[b]; blk != nil {
			copy(p[n:n+c], blk[o:])
		} else {
			clear(p[n : n+c])
		}
		n += c
	}
	return len(p), nil
}

func (m *image) block(b uint64) []byte {
	p := make([]byte, blockSize)
	m.ReadAt(p, int64(b)*blockSize)
	return p
}

var fixed = Options{
	UUID:     [16]byte{1, 2, 3, 4, 5, 6, 0x47, 8, 0x89, 10, 11, 12, 13, 14, 15, 16},
	HashSeed: [16]byte{9, 9, 9},
	Now:      time.Unix(1_800_000_000, 0),
}

func TestCRC16(t *testing.T) {
	if got := crc16(0xffff, []byte("123456789")); got != 0x4b37 {
		t.Errorf("crc16(123456789) = %#04x, want 0x4b37", got)
	}
}

// sizes covers one partial group, the smallest sizes, a last group that is
// dropped, a flex group of one group, and large file systems.
var sizes = []int64{
	MinSize,
	100 << 20,
	128 << 20,
	2<<30 + 4096,
	17 * blocksPerGroup * blockSize,
	17*blocksPerGroup*blockSize + 100*blockSize,
	64 << 30,
	1 << 40,
}

func TestFormat(t *testing.T) {
	for _, size := range sizes {
		for _, journal := range []bool{false, true} {
			t.Run(fmt.Sprintf("%d/journal=%v", size, journal), func(t *testing.T) {
				o := fixed
				o.Journal = journal
				img := newImage()
				if err := Format(img, size, o); err != nil {
					t.Fatal(err)
				}
				check(t, img, size, journal)
			})
		}
	}
}

func TestFormatWritesLittle(t *testing.T) {
	img := newImage()
	if err := Format(img, 64<<30, Options{Journal: true}); err != nil {
		t.Fatal(err)
	}
	if img.written > 1<<20 {
		t.Errorf("formatting 64 GiB wrote %d KiB", img.written>>10)
	}
	t.Logf("formatting 64 GiB wrote %d KiB", img.written>>10)
}

func TestFormatDeterministic(t *testing.T) {
	a, b := newImage(), newImage()
	o := fixed
	o.Journal = true
	Format(a, 1<<30, o)
	Format(b, 1<<30, o)
	if len(a.blocks) != len(b.blocks) {
		t.Fatalf("%d blocks written, then %d", len(a.blocks), len(b.blocks))
	}
	for n, blk := range a.blocks {
		if !bytes.Equal(blk, b.blocks[n]) {
			t.Fatalf("block %d differs between two runs with the same options", n)
		}
	}
	c := newImage()
	Format(c, 1<<30, Options{})
	if bytes.Equal(a.block(0), c.block(0)) {
		t.Error("random UUIDs gave the same superblock")
	}
}

func TestFormatRefusesSizes(t *testing.T) {
	for _, size := range []int64{MinSize - 1, MaxSize + 1} {
		if err := Format(newImage(), size, Options{}); err == nil {
			t.Errorf("formatted %d bytes", size)
		}
	}
	if err := Format(newImage(), MinSize, Options{Label: strings.Repeat("x", 17)}); err == nil {
		t.Error("took a 17-byte label")
	}
}

func TestReadState(t *testing.T) {
	img := newImage()
	o := fixed
	o.Journal = true
	Format(img, 1<<30, o)
	s, err := ReadState(img)
	if err != nil || !s.Clean || !s.Journal || s.NeedsRecovery || s.MountCount != 0 {
		t.Fatalf("new file system: %+v, %v", s, err)
	}
	// What a mount with work in the journal leaves behind.
	sb := img.block(0)
	le.PutUint32(sb[sbOffset+0x60:], le.Uint32(sb[sbOffset+0x60:])|incompatRecover)
	le.PutUint16(sb[sbOffset+0x34:], 3)
	img.WriteAt(sb, 0)
	var g geometry
	g, _ = newGeometry(1<<30, true)
	j := img.block(g.journalStart())
	be.PutUint32(j[0x1c:], 5)
	img.WriteAt(j, int64(g.journalStart())*blockSize)
	s, err = ReadState(img)
	if err != nil || s.Clean || !s.NeedsRecovery || s.MountCount != 3 || s.JournalStart != 5 {
		t.Errorf("after an unclean stop: %+v, %v", s, err)
	}
	if _, err := ReadState(bytes.NewReader(make([]byte, 4096))); err == nil {
		t.Error("ReadState accepted zeros")
	}
	img = newImage()
	Format(img, MinSize, fixed)
	if s, err := ReadState(img); err != nil || !s.Clean || s.Journal {
		t.Errorf("without a journal: %+v, %v", s, err)
	}
}

// check reads the file system back as the kernel and e2fsck would and
// checks everything they check that Format decides.
func check(t *testing.T, img *image, size int64, journal bool) {
	t.Helper()
	sb := make([]byte, 1024)
	img.ReadAt(sb, sbOffset)
	u32 := func(off int) uint32 { return le.Uint32(sb[off:]) }
	u16 := func(off int) uint16 { return le.Uint16(sb[off:]) }
	if u16(0x38) != ext4Magic || u16(0x3a) != stateValid || u32(0x4c) != 1 {
		t.Fatal("bad magic, state or revision")
	}
	blocks := uint64(u32(0x04))
	if blocks > uint64(size)/blockSize || blocks+blocksPerGroup < uint64(size)/blockSize {
		t.Fatalf("%d blocks in a %d-byte disk", blocks, size)
	}
	if u32(0x14) != 0 || u32(0x18) != 2 || u32(0x1c) != 2 || u32(0x20) != blocksPerGroup || u32(0x24) != blocksPerGroup {
		t.Fatal("bad block geometry")
	}
	groups := uint32((blocks + blocksPerGroup - 1) / blocksPerGroup)
	ipg := u32(0x28)
	if ipg%inodesPerBlock != 0 || ipg > blocksPerGroup || u32(0x00) != ipg*groups {
		t.Fatalf("inodes: %d per group, %d in all, %d groups", ipg, u32(0x00), groups)
	}
	if u16(0x58) != inodeSize || u32(0x54) != firstIno || u16(0x15c) != extraIsize || u16(0x15e) != extraIsize || u16(0x36) != 0xffff || sb[0x174] != logFlex {
		t.Fatal("bad inode size, first inode, extra isize, max mount count or flex size")
	}
	if journal != (u32(0x5c)&compatHasJournal != 0) || u32(0x60)&incompatRecover != 0 {
		t.Fatalf("journal feature %v, compat %#x, incompat %#x", journal, u32(0x5c), u32(0x60))
	}
	itb := uint64(ipg / inodesPerBlock)
	gdtBlocks := uint64((groups*descSize + blockSize - 1) / blockSize)
	var uuid [16]byte
	copy(uuid[:], sb[0x68:])

	groupBlocks := func(i uint32) uint64 { return min(blocksPerGroup, blocks-uint64(i)*blocksPerGroup) }
	gdt := make([]byte, gdtBlocks*blockSize)
	img.ReadAt(gdt, blockSize)

	// Every block the metadata claims, which must be claimed once: a bit
	// per block, and the ranges by name to say what collided.
	claimed := make([]byte, (blocks+blocksPerGroup+7)/8)
	type claimRange struct {
		start, n uint64
		what     string
	}
	var ranges []claimRange
	isClaimed := func(b uint64) bool { return claimed[b/8]&(1<<(b%8)) != 0 }
	owner := func(b uint64) string {
		for _, r := range ranges {
			if b >= r.start && b < r.start+r.n {
				return r.what
			}
		}
		return "nothing"
	}
	claim := func(start, n uint64, what string) {
		t.Helper()
		if start+n > blocks {
			t.Fatalf("%s: blocks %d-%d past the end (%d)", what, start, start+n-1, blocks)
		}
		for b := start; b < start+n; b++ {
			if isClaimed(b) {
				t.Fatalf("block %d belongs to %s and %s", b, owner(b), what)
			}
			claimed[b/8] |= 1 << (b % 8)
		}
		ranges = append(ranges, claimRange{start, n, what})
	}
	type desc struct {
		bb, ib, it              uint64
		free, freeIno, dirs, fl uint16
		unused                  uint16
	}
	ds := make([]desc, groups)
	for i := range groups {
		d := gdt[i*descSize:]
		ds[i] = desc{
			bb: uint64(le.Uint32(d[0:])), ib: uint64(le.Uint32(d[4:])), it: uint64(le.Uint32(d[8:])),
			free: le.Uint16(d[0xc:]), freeIno: le.Uint16(d[0xe:]), dirs: le.Uint16(d[0x10:]), fl: le.Uint16(d[0x12:]),
			unused: le.Uint16(d[0x1c:]),
		}
		if got, want := le.Uint16(d[0x1e:]), descChecksum(uuid, i, d[:descSize]); got != want {
			t.Fatalf("group %d: checksum %#x, want %#x", i, got, want)
		}
		if hasSuper(i) {
			claim(uint64(i)*blocksPerGroup, 1+gdtBlocks, fmt.Sprintf("group %d's superblock", i))
		}
		claim(ds[i].bb, 1, fmt.Sprintf("group %d's block bitmap", i))
		claim(ds[i].ib, 1, fmt.Sprintf("group %d's inode bitmap", i))
		claim(ds[i].it, itb, fmt.Sprintf("group %d's inode table", i))

		fl := ds[i].fl
		switch {
		case fl&bgItableZero == 0:
			t.Errorf("group %d: inode table not marked zeroed", i)
		case fl&bgBlockUninit != 0 && fl&bgInodeUninit == 0:
			t.Errorf("group %d: block bitmap uninitialized but inode bitmap not", i)
		case fl&bgBlockUninit != 0 && (i == 0 || i == groups-1 || i%flexSize == 0):
			t.Errorf("group %d: block bitmap uninitialized in group 0, a flex leader or the last group", i)
		case i == 0 && fl&bgInodeUninit != 0:
			t.Error("group 0: inode bitmap uninitialized")
		}
	}

	// Backups agree with the primary.
	for i := uint32(1); i < groups; i++ {
		if !hasSuper(i) {
			continue
		}
		b := img.block(uint64(i) * blocksPerGroup)
		if le.Uint16(b[0x5a:]) != uint16(i) {
			t.Errorf("backup in group %d says it is in group %d", i, le.Uint16(b[0x5a:]))
		}
		le.PutUint16(b[0x5a:], 0)
		if !bytes.Equal(b[:1024], sb) {
			t.Errorf("backup superblock in group %d differs", i)
		}
		bg := make([]byte, len(gdt))
		img.ReadAt(bg, int64(uint64(i)*blocksPerGroup+1)*blockSize)
		if !bytes.Equal(bg, gdt) {
			t.Errorf("backup descriptors in group %d differ", i)
		}
	}

	// Inodes.
	inode := func(ino uint32) []byte {
		g, idx := (ino-1)/ipg, (ino-1)%ipg
		b := make([]byte, inodeSize)
		img.ReadAt(b, int64(ds[g].it)*blockSize+int64(idx)*inodeSize)
		return b
	}
	extent := func(name string, in []byte, wantLen uint16) uint64 {
		t.Helper()
		if le.Uint32(in[0x20:])&extentsFlag == 0 || le.Uint16(in[0x80:]) != extraIsize {
			t.Fatalf("%s: no extents flag or bad extra isize", name)
		}
		eh := in[0x28:]
		if le.Uint16(eh[0:]) != extentMagic || le.Uint16(eh[2:]) != 1 || le.Uint16(eh[4:]) != 4 || le.Uint16(eh[6:]) != 0 {
			t.Fatalf("%s: bad extent header", name)
		}
		ex := eh[12:]
		if le.Uint32(ex[0:]) != 0 || le.Uint16(ex[4:]) != wantLen || wantLen > maxExtentLen {
			t.Fatalf("%s: extent of %d blocks at logical %d, want %d at 0", name, le.Uint16(ex[4:]), le.Uint32(ex[0:]), wantLen)
		}
		start := uint64(le.Uint16(ex[6:]))<<32 | uint64(le.Uint32(ex[8:]))
		claim(start, uint64(wantLen), name)
		size := uint64(le.Uint32(in[0x04:])) | uint64(le.Uint32(in[0x6c:]))<<32
		if size != uint64(wantLen)*blockSize || le.Uint32(in[0x1c:]) != uint32(wantLen)*8 {
			t.Fatalf("%s: size %d, i_blocks %d", name, size, le.Uint32(in[0x1c:]))
		}
		return start
	}
	root, lf := inode(rootIno), inode(lostFoundIno)
	if le.Uint16(root[0:]) != 0o40755 || le.Uint16(root[0x1a:]) != 3 || le.Uint16(lf[0:]) != 0o40700 || le.Uint16(lf[0x1a:]) != 2 {
		t.Fatal("bad mode or link count on / or lost+found")
	}
	rootBlock := extent("root", root, 1)
	lfBlock := extent("lost+found", lf, 1)
	for ino := uint32(1); ino < firstIno; ino++ {
		if ino == rootIno || (ino == journalIno && journal) {
			continue
		}
		if !bytes.Equal(inode(ino), make([]byte, inodeSize)) {
			t.Errorf("reserved inode %d is not zero", ino)
		}
	}
	type dirent struct {
		ino  uint32
		name string
	}
	readDir := func(b uint64) (out []dirent) {
		blk := img.block(b)
		for off := 0; off < blockSize; {
			rec := int(le.Uint16(blk[off+4:]))
			if rec < 12 || rec%4 != 0 || off+rec > blockSize || int(blk[off+6])+8 > rec || blk[off+7] != 2 {
				t.Fatalf("block %d: bad directory entry at %d", b, off)
			}
			out = append(out, dirent{le.Uint32(blk[off:]), string(blk[off+8 : off+8+int(blk[off+6])])})
			off += rec
		}
		return out
	}
	if got := fmt.Sprint(readDir(rootBlock)); got != "[{2 .} {2 ..} {11 lost+found}]" {
		t.Errorf("root directory: %s", got)
	}
	if got := fmt.Sprint(readDir(lfBlock)); got != "[{11 .} {2 ..}]" {
		t.Errorf("lost+found: %s", got)
	}

	if journal {
		jin := inode(journalIno)
		if le.Uint16(jin[0:]) != 0o100600 || le.Uint16(jin[0x1a:]) != 1 || u32(0xe0) != journalIno || sb[0xfd] != 1 {
			t.Fatal("bad journal inode or superblock fields")
		}
		jlen := le.Uint16(jin[0x28+12+4:])
		start := extent("journal", jin, jlen)
		if !bytes.Equal(sb[0x10c:0x10c+60], jin[0x28:0x28+60]) || u32(0x10c+15*4) != 0 || u32(0x10c+16*4) != uint32(jlen)*blockSize {
			t.Error("s_jnl_blocks does not back up the journal inode")
		}
		j := img.block(start)
		if be.Uint32(j[0:]) != jbd2Magic || be.Uint32(j[4:]) != jbd2SBv2 || be.Uint32(j[0xc:]) != blockSize ||
			be.Uint32(j[0x10:]) != uint32(jlen) || be.Uint32(j[0x14:]) != 1 || be.Uint32(j[0x18:]) != 1 ||
			be.Uint32(j[0x1c:]) != 0 || be.Uint32(j[0x40:]) != 1 || !bytes.Equal(j[0x30:0x40], uuid[:]) {
			t.Error("bad journal superblock")
		}
	} else if !bytes.Equal(inode(journalIno), make([]byte, inodeSize)) {
		t.Error("inode 8 used without a journal")
	}

	// Block bitmaps: as written, or as the kernel builds them for an
	// uninitialized group. Either way they must mark exactly the claimed
	// blocks, and the descriptors must count what is left.
	var free uint64
	for i := range groups {
		first, n := uint64(i)*blocksPerGroup, groupBlocks(i)
		bm := img.block(ds[i].bb)
		if ds[i].fl&bgBlockUninit != 0 {
			bm = make([]byte, blockSize)
			if hasSuper(i) {
				setBits(bm, 0, 1+gdtBlocks)
			}
			for _, r := range [][2]uint64{{ds[i].bb, 1}, {ds[i].ib, 1}, {ds[i].it, itb}} {
				for b := r[0]; b < r[0]+r[1]; b++ {
					if b >= first && b < first+n {
						setBits(bm, b-first, b-first+1)
					}
				}
			}
			setBits(bm, n, blocksPerGroup)
		}
		// Past the end of the file system everything is marked; before it,
		// exactly the claimed blocks. first is a multiple of 8.
		want := make([]byte, blockSize)
		copy(want, claimed[first/8:min(uint64(len(claimed)), first/8+blockSize)])
		clear(want[(n+7)/8:])
		setBits(want, n, blocksPerGroup)
		if !bytes.Equal(bm, want) {
			for b := uint64(0); b < blocksPerGroup; b++ {
				if bm[b/8]&(1<<(b%8)) != want[b/8]&(1<<(b%8)) {
					t.Fatalf("group %d: block %d (%s) marked %v", i, first+b, owner(first+b), bm[b/8]&(1<<(b%8)) != 0)
				}
			}
		}
		zeros := uint64(0)
		for b := uint64(0); b < n; b++ {
			if bm[b/8]&(1<<(b%8)) == 0 {
				zeros++
			}
		}
		if uint64(ds[i].free) != zeros {
			t.Fatalf("group %d: descriptor counts %d free blocks, bitmap %d", i, ds[i].free, zeros)
		}
		free += zeros

		wantFreeIno, wantDirs := ipg, uint16(0)
		if i == 0 {
			wantFreeIno, wantDirs = ipg-firstIno, 2
			ib := img.block(ds[0].ib)
			for b := uint64(0); b < blocksPerGroup; b++ {
				if set := ib[b/8]&(1<<(b%8)) != 0; set != (b < firstIno || b >= uint64(ipg)) {
					t.Fatalf("group 0: inode bitmap bit %d is %v", b, set)
				}
			}
		}
		if uint32(ds[i].freeIno) != wantFreeIno || uint32(ds[i].unused) != wantFreeIno || ds[i].dirs != wantDirs {
			t.Fatalf("group %d: %d free inodes, %d unused, %d directories", i, ds[i].freeIno, ds[i].unused, ds[i].dirs)
		}
	}
	if uint64(u32(0x0c)) != free || u32(0x10) != ipg*groups-firstIno {
		t.Errorf("superblock counts %d free blocks and %d free inodes; groups %d and %d", u32(0x0c), u32(0x10), free, ipg*groups-firstIno)
	}
}

// TestE2fsck checks formatted file systems with e2fsck, where it is
// installed: on the Linux side, as in the race-detector container.
func TestE2fsck(t *testing.T) {
	e2fsck, err := exec.LookPath("e2fsck")
	if err != nil {
		t.Skip("e2fsck not installed")
	}
	dir := t.TempDir()
	for _, size := range sizes {
		for _, journal := range []bool{false, true} {
			path := filepath.Join(dir, fmt.Sprintf("%d-%v.ext4", size, journal))
			f, err := os.Create(path)
			if err != nil {
				t.Fatal(err)
			}
			if err := f.Truncate(size); err != nil {
				t.Fatal(err)
			}
			if err := Format(f, size, Options{Journal: journal, Label: "runwhp"}); err != nil {
				t.Fatal(err)
			}
			f.Close()
			out, err := exec.Command(e2fsck, "-fn", path).CombinedOutput()
			if err != nil || bytes.Contains(out, []byte("?")) {
				t.Errorf("e2fsck -fn on %d bytes, journal %v: %v\n%s", size, journal, err, out)
			}
			lines := strings.Split(strings.TrimSpace(string(out)), "\n")
			t.Logf("%d bytes, journal %v: %s", size, journal, lines[len(lines)-1])
			os.Remove(path)
		}
	}
}
