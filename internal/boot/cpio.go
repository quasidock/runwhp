package boot

import (
	"bytes"
	"fmt"
)

// File modes for initramfs entries.
const (
	ModeDir  = 0o040000
	ModeFile = 0o100000
)

// File is one initramfs entry. Directories must come before their contents.
type File struct {
	Name string // relative path without a leading slash, such as "modules/virtio_blk.ko"
	Mode uint32 // type and permission bits, such as ModeFile|0o755
	Data []byte
}

// CPIO builds an uncompressed newc archive, padded to 512 bytes, suitable as
// a Linux initramfs.
func CPIO(files []File) []byte {
	var buf bytes.Buffer
	ino := 0
	add := func(name string, mode uint32, data []byte) {
		ino++
		nlink := 1
		if mode&0o170000 == ModeDir {
			nlink = 2
		}
		fmt.Fprintf(&buf, "070701%08x%08x%08x%08x%08x%08x%08x%08x%08x%08x%08x%08x%08x",
			ino, mode, 0, 0, nlink, 0, len(data), 0, 0, 0, 0, len(name)+1, 0)
		buf.WriteString(name)
		buf.WriteByte(0)
		for buf.Len()%4 != 0 {
			buf.WriteByte(0)
		}
		buf.Write(data)
		for buf.Len()%4 != 0 {
			buf.WriteByte(0)
		}
	}
	for _, f := range files {
		add(f.Name, f.Mode, f.Data)
	}
	ino = -1
	add("TRAILER!!!", 0, nil)
	for buf.Len()%512 != 0 {
		buf.WriteByte(0)
	}
	return buf.Bytes()
}
